package trae

// poolstate.go — the pool's health survives a restart.
//
// Why this exists: cooldownFor parks an account for 12h on a plan limit and for
// 6h on an exhausted quota.  A process that forgets those on restart replays,
// immediately and proudly, the exact request the vendor just refused — which is
// how a soft rate limit becomes a ban.  What is written here is only the
// vendor's verdict and the deadline it came with: never a credential, never a
// token, nothing the desktop app already owns.
//
// `invalid` is deliberately NOT persisted.  It means "the credential this
// process last saw cannot be refreshed", and every start re-reads the
// credential from its source (storage.json, or this module's account store).
// The old verdict is therefore not evidence about the new credential, whereas a
// cooldown is a deadline the vendor granted and restarting our process does not
// shorten it.

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
	// poolStateFile sits beside accounts.json inside the module's own
	// directory.  It is the runtime half; accounts.json holds the credentials.
	poolStateFile = "pool.json"
)

// persistedPoolAccount is the secret-free half of a poolEntry.
type persistedPoolAccount struct {
	ID            string    `json:"id"`
	State         string    `json:"state,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
	Note          string    `json:"note,omitempty"`
	Fails         int       `json:"fails,omitempty"`
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
		p.log("trae: %s was written by a newer build (version %d); ignoring it", poolStateFile, st.Version)
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
	if len(p.pending) == 0 {
		return
	}
	rec, ok := p.pending[fresh.auth.ID()]
	if !ok {
		return
	}
	delete(p.pending, fresh.auth.ID())
	if fresh.state != stateReady || !fresh.until.IsZero() {
		// Something already decided this entry's health.  A staged record must
		// never overrule a decision made by this process.
		return
	}
	// Only a timed cooldown is restored as state.  ready/invalid/anything else
	// the file might say carries no deadline, so only the counter comes back —
	// the account is free to be tried again.
	switch rec.State {
	case stateCooling, stateExhausted:
		fresh.state = rec.State
		fresh.until = rec.CooldownUntil
		fresh.note = rec.Note
	}
	fresh.fails = rec.Fails
}

// hasStorableHealth reports whether an entry is worth remembering.  A ready
// account with no failure counter is where every account starts, so recording it
// would only add noise — and it would make this file exist before it has
// anything to say.
func hasStorableHealth(e *poolEntry) bool {
	return e.state != stateReady || !e.until.IsZero() || e.note != "" || e.fails != 0
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
	if p.dir == "" || !p.dirty {
		return
	}
	st := persistedPool{Version: poolStateVersion}
	for _, e := range p.entries {
		if e.state == stateInvalid || !hasStorableHealth(e) {
			// stateInvalid is not a durable verdict (see the file header).
			// Dropping the record is also what keeps a stale cooldown from
			// being restored over it: the account is left with nothing on disk,
			// so the next start lets the freshly read credential decide.
			continue
		}
		st.Accounts = append(st.Accounts, persistedPoolAccount{
			ID:            e.auth.ID(),
			State:         e.state,
			CooldownUntil: e.until,
			Note:          e.note,
			Fails:         e.fails,
		})
	}
	if len(st.Accounts) == 0 && !p.wrote {
		// Nothing to remember, and no file to correct.  Writing here would
		// create the directory and an empty document on every single start.
		return
	}
	if err := core.WriteJSONAtomic(filepath.Join(p.dir, poolStateFile), st); err != nil {
		// A failed write costs a cooldown, not a request: the account stays
		// parked in memory and only the restart is worse off.  The flag stays
		// set so the next transition tries again.
		p.log("trae: cannot persist account health to %s: %v", poolStateFile, err)
		return
	}
	p.wrote = true
	p.dirty = false
}
