package workbuddy

// poolstate.go — the pool's health survives a restart.
//
// Why this exists: cooldownFor parks an account for 12h when the upstream
// refuses to serve it, 6h when its credits are gone and 30m when it tripped the
// edge WAF.  A process that forgets those on restart replays, immediately and
// proudly, the exact request the vendor just refused — which is how a soft rate
// limit becomes a ban.  What is written here is only the vendor's verdict and
// the deadline that came with it: never a credential, never a token, nothing the
// desktop app does not already own.
//
// Unlike the trae module, `invalid` IS persisted here, and the difference is
// deliberate.  MarkInvalid records `until = now + invalidCooldown` and usable()
// asks `now.After(e.until)`, so this module's `invalid` is a cooling with a
// different label: it cannot outlive an hour even if the file is stale.  A
// verdict that never expires would be wrong to persist — every start re-reads
// the credential from its source, so an old judgement is not evidence about a
// new credential — but one with a deadline the vendor (or our own policy) already
// set is exactly what a restart must not shorten.

import (
	"path/filepath"
	"strings"
	"time"

	"client2api/internal/core"
)

const (
	// poolStateVersion is bumped when the stored shape changes meaning.  A file
	// written by a NEWER version is ignored rather than guessed at: this is a
	// cache of health, and misreading it is worse than having none.
	poolStateVersion = 1
	// poolStateFile is the runtime half; accounts.json holds the credentials.
	// It must NOT live in the credential directory: the loader globs
	// <accounts_dir>/*.json (auth.go), so a state file written there is
	// enumerated and parsed as an account.  This module writes it into the data
	// directory's cache/ instead — see Client.poolStateDir.
	poolStateFile = "pool.json"
)

// persistedPoolAccount is the secret-free half of a poolEntry.
type persistedPoolAccount struct {
	ID            string    `json:"id"`
	State         string    `json:"state,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	Note          string    `json:"note,omitempty"`
	Fails         int       `json:"fails,omitempty"`

	// Fault is the breaker and degrade state.  Both are verdicts about the
	// account rather than about one request, so forgetting them on a restart
	// would send the next request straight back to an account the upstream has
	// already refused several times.  A pointer so that an account with neither
	// penalty leaves the key out entirely; an expired deadline inside the
	// snapshot is ignored on the way back in.
	Fault *core.FaultSnapshot `json:"fault,omitempty"`

	// ModelCost is the measured per-1k cost ledger.  It is the only record of
	// which models turned out to be free on which account, and rebuilding it
	// costs real credit: every forgotten entry is a paid request that has to
	// happen again before the picker learns anything.
	ModelCost map[string]modelCostEntry `json:"model_cost,omitempty"`

	// Credits is the last balance this account reported.  CreditsKnown
	// distinguishes "reported zero" from "never reported", which matters
	// because only a known positive balance lifts a credit park.  The expiry
	// detail is the earliest batch, kept so that earliest-expiry routing is
	// still correct in the minutes before the first refresh lands.
	Credits         int64     `json:"credits,omitempty"`
	CreditsTotal    int64     `json:"credits_total,omitempty"`
	CreditsKnown    bool      `json:"credits_known,omitempty"`
	CreditsExpiring int64     `json:"credits_expiring,omitempty"`
	EarliestExpiry  time.Time `json:"earliest_expiry,omitempty"`
	EarliestRemain  int64     `json:"earliest_remaining,omitempty"`
}

// persistedPool is the on-disk document.
type persistedPool struct {
	Version  int                    `json:"version"`
	Accounts []persistedPoolAccount `json:"accounts,omitempty"`
}

// AttachState binds the pool to a directory and stages the health recorded
// before the last shutdown.  Staged records are handed out by the next Replace,
// which is where the credentials they describe are actually built; a pool
// without a directory is memory-only and never touches the disk.
func (p *Pool) AttachState(dir string, logf func(string, ...any)) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dir = strings.TrimSpace(dir)
	p.logf = logf
	p.stageLocked()
	// Binding starts a new comparison.  A transition that happened before there
	// was a directory diverges from nothing -- there was no file to differ from
	// -- and saveLocked() would have silently skipped the write while leaving
	// the flag set, so the *next* no-op MarkSuccess would rewrite the file to
	// announce a change that had already been forgotten.
	p.dirty = false
}

// stageLocked reads the stored health into p.pending.  Callers hold p.mu.
func (p *Pool) stageLocked() {
	p.pending = nil
	if p.dir == "" {
		return
	}
	var st persistedPool
	if err := core.ReadJSON(filepath.Join(p.dir, poolStateFile), &st); err != nil {
		// Absent or unreadable.  This file is an optimisation: losing it costs
		// a cooldown, never a credential, so there is nothing to report.
		return
	}
	if st.Version > poolStateVersion {
		p.log("workbuddy: %s was written by a newer build (version %d); ignoring it", poolStateFile, st.Version)
		return
	}
	for _, rec := range st.Accounts {
		if rec.ID == "" {
			continue
		}
		if p.pending == nil {
			p.pending = make(map[string]persistedPoolAccount, len(st.Accounts))
		}
		p.pending[rec.ID] = rec
	}
}

// applyStagedLocked hands a freshly built entry the record stored for it, once.
// An entry kept across a Replace already carries its health in memory, and a
// record for an account that is gone is simply dropped with p.pending.  Callers
// hold p.mu.
func (p *Pool) applyStagedLocked(fresh *poolEntry) {
	if fresh == nil || fresh.auth == nil || len(p.pending) == 0 {
		return
	}
	id := fresh.auth.ID()
	rec, ok := p.pending[id]
	if !ok {
		return
	}
	delete(p.pending, id)
	// Faults, the cost ledger and the balance come back before the health guard
	// below, because they are independent of it: an entry kept across a Replace
	// may already carry a cooldown this process decided, but it cannot have an
	// opinion about a breaker it has never seen, and the cost ledger is
	// knowledge about the upstream that costs credit to re-acquire.
	if rec.Fault != nil {
		fresh.faults.Restore(*rec.Fault, p.now())
	}
	if len(rec.ModelCost) > 0 {
		fresh.modelCost = make(map[string]modelCostEntry, len(rec.ModelCost))
		for model, mc := range rec.ModelCost {
			// A stored observation is subject to the same TTL as a live one:
			// a promotion that ended while we were down must not keep steering
			// the picker.
			if mc.LastSeen.IsZero() || p.now().Sub(mc.LastSeen) > modelCostTTL {
				continue
			}
			fresh.modelCost[model] = mc
		}
	}
	if rec.CreditsKnown {
		fresh.credits = rec.Credits
		fresh.creditsTotal = rec.CreditsTotal
		fresh.creditsKnown = true
		fresh.creditsExpiring = rec.CreditsExpiring
		fresh.creditsEarliestRemain = rec.EarliestRemain
		if !rec.EarliestExpiry.IsZero() && rec.EarliestExpiry.After(p.now()) && rec.EarliestRemain > 0 {
			fresh.creditsEarliestExpiry = rec.EarliestExpiry
		}
	}
	if fresh.state != stateReady || !fresh.until.IsZero() {
		// Something already decided this entry's health.  A staged record must
		// never overrule a decision made by this process.
		return
	}
	if rec.CooldownUntil.IsZero() || !p.now().Before(rec.CooldownUntil) {
		// The deadline passed while we were not running.  Restoring the state
		// without a live cooldown would park an account that is owed a retry,
		// so only the counter comes back.
		fresh.fails = rec.Fails
		return
	}
	switch rec.State {
	case stateCooling, stateExhausted, stateFault, stateInvalid:
		fresh.state = rec.State
		fresh.until = rec.CooldownUntil
		fresh.note = rec.Note
	default:
		// Anything else the file might say carries no deadline we can honour.
		if rec.CooldownUntil.After(p.now()) {
			fresh.state = stateCooling
			fresh.until = rec.CooldownUntil
			fresh.note = rec.Note
		}
	}
	fresh.fails = rec.Fails
}

// hasStorableHealth reports whether an entry is worth remembering.  A ready
// account with no failure counter is where every account starts, so recording it
// would only add noise — and it would make this file exist before it has
// anything to say.
//
// The breaker, the cost ledger and the last known balance all count as worth
// remembering even on an otherwise ready account: the first two are verdicts
// that took several requests to earn, and the third is what keeps
// earliest-expiry routing honest across a restart.
func hasStorableHealth(e *poolEntry) bool {
	if e.state != stateReady || !e.until.IsZero() || e.note != "" || e.fails != 0 {
		return true
	}
	if !e.faults.BreakerUntil().IsZero() || !e.faults.DegradeUntil().IsZero() {
		return true
	}
	return len(e.modelCost) > 0 || e.creditsKnown
}

// saveLocked writes the secret-free runtime health.  Callers hold p.mu, and the
// write happens under it on purpose: two concurrent failures that snapshot and
// then write outside the lock could interleave and leave the older snapshot on
// disk, silently forgetting the newer cooldown.
//
// The write is skipped unless a transition actually happened.  MarkSuccess runs
// on every successful request, and rewriting a file per request to record
// "still ready" would be pure cost.
func (p *Pool) saveLocked() {
	if p == nil || p.dir == "" || !p.dirty {
		return
	}
	st := persistedPool{Version: poolStateVersion}
	for _, e := range p.entries {
		if e.auth == nil || !hasStorableHealth(e) {
			continue
		}
		rec := persistedPoolAccount{
			ID:              e.auth.ID(),
			State:           e.state,
			CooldownUntil:   e.until,
			Note:            e.note,
			Fails:           e.fails,
			Credits:         e.credits,
			CreditsTotal:    e.creditsTotal,
			CreditsKnown:    e.creditsKnown,
			CreditsExpiring: e.creditsExpiring,
			EarliestExpiry:  e.creditsEarliestExpiry,
			EarliestRemain:  e.creditsEarliestRemain,
		}
		// Raw deadlines, not Blocked: an expired breaker is still worth one more
		// restart to remember, because the retry counter inside it is what makes
		// the next breaker longer.
		if !e.faults.BreakerUntil().IsZero() || !e.faults.DegradeUntil().IsZero() {
			snap := e.faults.Snapshot()
			rec.Fault = &snap
		}
		if len(e.modelCost) > 0 {
			rec.ModelCost = make(map[string]modelCostEntry, len(e.modelCost))
			for model, mc := range e.modelCost {
				rec.ModelCost[model] = mc
			}
		}
		st.Accounts = append(st.Accounts, rec)
	}
	if len(st.Accounts) == 0 && !p.wrote {
		// Nothing to remember, and no file to correct.  Writing here would
		// create the directory and an empty document on every single start,
		// which is how this file first leaked into the credential directory.
		return
	}
	if err := core.WriteJSONAtomic(filepath.Join(p.dir, poolStateFile), st); err != nil {
		// A failed write costs a cooldown, not a request: the account stays
		// parked in memory and only the restart is worse off.  The flag stays
		// set so the next transition tries again.
		p.log("workbuddy: cannot persist account health to %s: %v", poolStateFile, err)
		return
	}
	p.wrote = true
	p.dirty = false
}
