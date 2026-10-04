package trae

// pool.go — the account pool: round-robin selection plus per-account health.
//
// Cooldown policy lives in cooldownFor, a pure function, so the policy can be
// tested without a clock or a network.  The pool never makes a network call.

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"client2api/internal/core"
)

// Account states rendered by the panel.
const (
	stateReady     = "ready"
	stateCooling   = "cooling"
	stateExhausted = "exhausted"
	stateInvalid   = "invalid"
	stateUnknown   = "unknown"
)

// Cooldown policy.  Durations follow docs/upstream/trae.md §4.3.
const (
	planLimitCooldown   = 12 * time.Hour   // 1005: plan entitlement gone
	quotaCooldown       = 6 * time.Hour    // 4008: account quota exhausted
	authCooldown        = 60 * time.Second // 1001: auth failure, try another account
	softRateCooldown    = 60 * time.Second // 429 / 4011
	retryLaterCooldown  = 5 * time.Minute  // 9074: checkin contention
	notFoundCooldown    = 60 * time.Second // 404: do not hammer
	serverCooldown      = 30 * time.Second // 5xx
	clientCooldown      = 5 * time.Minute  // other 4xx (e.g. 4001 param error)
	sessionDeadCooldown = time.Hour        // 401 with no usable refresh token
)

// cooldownFor maps a classified failure to a cooldown and a display state.
func cooldownFor(kind ErrKind) (time.Duration, string) {
	switch kind {
	case ErrPlanLimit:
		return planLimitCooldown, stateExhausted
	case ErrQuota:
		return quotaCooldown, stateExhausted
	case ErrSessionDead:
		return sessionDeadCooldown, stateInvalid
	case ErrAuth:
		return authCooldown, stateCooling
	case ErrSoftRate:
		return softRateCooldown, stateCooling
	case ErrRetryLater:
		return retryLaterCooldown, stateCooling
	case ErrNotFound:
		return notFoundCooldown, stateCooling
	case ErrServer:
		return serverCooldown, stateCooling
	case ErrClient:
		return clientCooldown, stateCooling
	default:
		return 0, stateReady
	}
}

// retryableKind reports whether trying a *different* account could plausibly
// help.  This is the account-failover decision; it is not the same as the
// business-code failover set (see FailoverCode).
func retryableKind(kind ErrKind) bool {
	switch kind {
	case ErrPlanLimit, ErrParam, ErrNotFound, ErrClient:
		// A plan limit is account-wide and switching cannot help; a malformed
		// request (4001/4023) or a missing route will fail everywhere too.
		return false
	default:
		return true
	}
}

// poolEntry is one account plus its health.
type poolEntry struct {
	auth  *Auth
	state string
	until time.Time
	note  string
	fails int

	// modelCool holds per-(account, model) parks.  A model park never replaces
	// an account park, it only removes this account from the running for one
	// model; see pick.go.  The map is swept on the pick path and on every
	// render, so it holds live parks and nothing else.
	modelCool map[string]modelCooldown
	// lastUsed and usedSeq are the recency terms behind minPickGap: they record
	// which account this pool handed out most recently, so two requests
	// arriving together do not both land on it.
	lastUsed time.Time
	usedSeq  uint64
	// current is this account's smooth-weighted-round-robin accumulator, the
	// thing that turns a weight into a share of a long rotation.  See
	// pickWeightedLocked in pick.go.
	current float64
}

// Pool is a concurrency-safe set of accounts.
type Pool struct {
	mu      sync.Mutex
	entries []*poolEntry
	cursor  int
	now     func() time.Time

	// dir and logf are set by AttachState; a pool that never got one is
	// memory-only and never touches the disk.  pending holds the health read
	// back from disk (poolstate.go) that no entry has claimed yet.
	dir     string
	logf    func(string, ...any)
	pending map[string]persistedPoolAccount
	// dirty records that a health transition has happened since the last
	// successful write.  MarkSuccess runs on every successful request, and
	// rewriting the file to say "still ready" would be pure cost.
	dirty bool
	// wrote records that a file exists, so that falling back to "no account
	// has anything to remember" still writes the emptiness out instead of
	// leaving a stale document behind.
	wrote bool

	// pickSeq is the monotonic counter behind usedSeq.
	pickSeq uint64
}

// log reports a problem to the module's logger, or to nobody when none was
// installed.  A pool built by a test literal has no logger and must work.
func (p *Pool) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

// NewPool builds a pool from a credential list.
func NewPool(accounts []*Auth) *Pool {
	p := &Pool{now: time.Now}
	p.Replace(accounts)
	return p
}

// Replace merges a fresh credential list, preserving the health of accounts
// that are still present (keyed by Auth.ID).
func (p *Pool) Replace(accounts []*Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := make(map[string]*poolEntry, len(p.entries))
	for _, e := range p.entries {
		prev[e.auth.ID()] = e
	}
	entries := make([]*poolEntry, 0, len(accounts))
	for _, a := range accounts {
		if a == nil {
			continue
		}
		if old, ok := prev[a.ID()]; ok {
			old.auth = a
			entries = append(entries, old)
			continue
		}
		fresh := &poolEntry{auth: a, state: stateReady}
		// A record read back from disk describes a credential that did not
		// exist a moment ago, so this is where it gets handed over.
		p.applyStagedLocked(fresh)
		entries = append(entries, fresh)
	}
	p.entries = entries
	if p.cursor >= len(p.entries) {
		p.cursor = 0
	}
	// A merge can retire a record (an account that is gone, whose cooldown was
	// the only thing in the file) or hand a staged one out, so it always
	// recomputes the document; saveLocked decides whether that is a change.
	p.dirty = true
	p.saveLocked()
}

// Len returns the number of accounts.
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// Accounts returns the account list.
func (p *Pool) Accounts() []*Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Auth, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, e.auth)
	}
	return out
}

// Ready reports whether at least one account could serve a request now.
func (p *Pool) Ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, e := range p.entries {
		if e.usable(now) {
			return true
		}
	}
	return false
}

// effectiveState resolves a timed cooldown that has already elapsed.  Both
// "cooling" and "exhausted" are temporary: a plan limit parks an account for
// 12h and a quota for 6h, and it becomes selectable again afterwards.
func (e *poolEntry) effectiveState(now time.Time) string {
	if (e.state == stateCooling || e.state == stateExhausted) && !now.Before(e.until) {
		return stateReady
	}
	return e.state
}

// usable reports whether the entry may be selected.
func (e *poolEntry) usable(now time.Time) bool {
	switch e.effectiveState(now) {
	case stateInvalid, stateExhausted:
		return false
	default:
		return true
	}
}

// Pick returns the next usable account in round-robin order, skipping ids in
// skip.  It also reports whether one was found.
//
// This is the model-agnostic path and it stays deterministic on purpose: the
// long-standing tests and every caller that knows nothing about models see the
// rotation they always saw.  The scored path is PickForModel in pick.go.
func (p *Pool) Pick(skip map[string]bool) (*Auth, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pickLocked(skip, p.now())
}

// pickLocked walks the rotation.  The caller holds p.mu.
func (p *Pool) pickLocked(skip map[string]bool, now time.Time) (*Auth, bool) {
	n := len(p.entries)
	for i := 0; i < n; i++ {
		idx := (p.cursor + i) % n
		e := p.entries[idx]
		if skip[e.auth.ID()] {
			continue
		}
		if !e.usable(now) {
			continue
		}
		// The pick path is the one place every account is walked, so it is
		// where lapsed per-model parks are swept.
		e.pruneModelCool(now)
		p.cursor = (idx + 1) % n
		p.markPickedLocked(e, now)
		return e.auth, true
	}
	return nil, false
}

// findLocked returns the entry for an account, or nil.
func (p *Pool) findLocked(a *Auth) *poolEntry {
	if a == nil {
		return nil
	}
	id := a.ID()
	for _, e := range p.entries {
		if e.auth.ID() == id {
			return e
		}
	}
	return nil
}

// MarkSuccess clears an account's cooldown.
func (p *Pool) MarkSuccess(a *Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.findLocked(a); e != nil {
		// This runs on every successful request, so only a real transition is
		// worth marking: an account that is already ready stays unwritten.
		if e.state != stateReady || !e.until.IsZero() || e.note != "" || e.fails != 0 {
			e.state = stateReady
			e.until = time.Time{}
			e.note = ""
			e.fails = 0
			p.dirty = true
		}
	}
	p.saveLocked()
}

// Revive clears every runtime penalty an operator asked to lift.
//
// It is deliberately unconditional where MarkSuccess is conditional: an
// override is a transition even when the account looks ready, and a model park
// or a failure count is invisible to the state check.  The staged on-disk
// record is dropped too, so the next start cannot re-read a park the operator
// already lifted.
func (p *Pool) Revive(a *Auth) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(a)
	if e == nil || e.auth == nil {
		return false
	}
	e.state = stateReady
	e.until = time.Time{}
	e.note = ""
	e.fails = 0
	e.modelCool = nil
	e.current = 0
	delete(p.pending, e.auth.ID())
	p.dirty = true
	p.saveLocked()
	return true
}

// MarkFailure classifies err, applies the matching cooldown, and returns the
// kind and cooldown for the caller to log.
func (p *Pool) MarkFailure(a *Auth, err error) (ErrKind, time.Duration) {
	kind := ErrClient
	var e *Error
	if errors.As(err, &e) {
		kind = e.Kind
	}
	cd, state := cooldownFor(kind)
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry := p.findLocked(a); entry != nil {
		entry.fails++
		entry.note = kind.String()
		if state != stateReady {
			entry.state = state
			entry.until = p.now().Add(cd)
			// Parking the whole credential subsumes any per-model park, and
			// keeping them would leave a model park outliving the account park
			// it is contained in.
			entry.modelCool = nil
			// A parked account misses the rotation rounds its accumulator is
			// measured over, so the accumulator is reset rather than left
			// carrying a deficit or a surplus from before the park.
			entry.current = 0
		}
		p.dirty = true
	}
	p.saveLocked()
	return kind, cd
}

// MarkInvalid parks an account permanently until it is re-discovered.
func (p *Pool) MarkInvalid(a *Auth, note string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.findLocked(a); e != nil {
		e.state = stateInvalid
		e.until = time.Time{}
		e.note = note
		e.modelCool = nil
		e.current = 0
		// invalid is not persisted, but the record this account may still have
		// on disk has to be dropped, so the file still changes.
		p.dirty = true
	}
	p.saveLocked()
}

// Snapshot renders the pool for Status().
func (p *Pool) Snapshot() []core.AccountStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]core.AccountStatus, 0, len(p.entries))
	for _, e := range p.entries {
		state := e.effectiveState(now)
		extra := map[string]any{}
		if e.auth.Product != "" {
			extra["product"] = e.auth.Product
		}
		if e.auth.Source != "" {
			extra["source"] = e.auth.Source
		}
		if e.fails > 0 {
			extra["failures"] = e.fails
		}
		if e.auth.IdeCredits > 0 {
			extra["ide_credits"] = e.auth.IdeCredits
		}
		if e.auth.WorkCredits > 0 {
			extra["work_credits"] = e.auth.WorkCredits
		}
		if e.auth.BillingMode != "" {
			extra["billing_mode"] = e.auth.BillingMode
		}
		// A per-model park is the only reason an account that looks ready can
		// still refuse one model, so it has to be visible: state=ready plus a
		// failure for one model is otherwise inexplicable from Status() alone.
		e.pruneModelCool(now)
		if len(e.modelCool) > 0 {
			parks := make([]string, 0, len(e.modelCool))
			for m, mc := range e.modelCool {
				if mc.Until.IsZero() {
					parks = append(parks, m)
					continue
				}
				parks = append(parks, fmt.Sprintf("%s until %s", m, mc.Until.UTC().Format(time.RFC3339)))
			}
			sort.Strings(parks)
			extra["model_cooldowns"] = parks
		}
		as := core.AccountStatus{
			ID:      e.auth.ID(),
			Label:   e.auth.Label(),
			Enabled: state == stateReady || state == stateCooling,
			State:   state,
			Note:    e.note,
			Extra:   extra,
		}
		if exp := e.auth.Expiry(); !exp.IsZero() {
			as.ExpiresAt = exp.UTC().Format(time.RFC3339)
		}
		out = append(out, as)
	}
	return out
}

// Summary renders one human line for Status().Detail.
func (p *Pool) Summary() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	counts := map[string]int{}
	for _, e := range p.entries {
		state := e.effectiveState(now)
		counts[state]++
	}
	if len(p.entries) == 0 {
		return "no accounts"
	}
	parts := make([]string, 0, 4)
	for _, s := range []string{stateReady, stateCooling, stateExhausted, stateInvalid} {
		if n := counts[s]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	return joinComma(parts)
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
