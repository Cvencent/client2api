package codearts

import (
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"client2api/internal/core"
)

// pool.go holds the credentials this module may sign with, and the small
// amount of health state each one carries.
//
// The pool is the single source of truth for two questions:
//
//	who may serve a request right now  (availableLocked)
//	why may nobody serve one            (snapshot, surfaced by Status)
//
// Nothing in here ever puts a secret into a state file or a status line; the
// persisted state is keyed by account id and holds no credential material.
// The credentials themselves live in accounts.json, written by saveLocked.

// Account states, as the panel displays them.
const (
	stateReady     = "ready"
	stateCooling   = "cooling"
	stateExhausted = "exhausted"
	stateInvalid   = "invalid"
	stateUnknown   = "unknown"
)

// entry is one pooled credential plus its runtime state.
type entry struct {
	acct account

	state     string
	until     int64 // unix millis a cooldown ends at; 0 when not cooling
	lastUsed  int64 // unix millis of the last successful use
	fails     int
	note      string
	origin    string // "config" or "stored"
	removable bool
}

// account returns a copy of the credential, so a caller cannot mutate the
// pool's copy by accident.
func (e *entry) account() account {
	if e == nil {
		return account{}
	}
	return e.acct
}

// id is the account's stable identifier.
func (e *entry) id() string {
	if e == nil {
		return ""
	}
	return e.acct.accountID()
}

// label is what the panel shows for this account.
func (e *entry) label() string {
	if e == nil {
		return ""
	}
	return e.acct.label()
}

// poolState is what is persisted to state.json.  It deliberately carries no
// credential material — only health, keyed by account id.
type poolState struct {
	Version int                    `json:"version"`
	Entries map[string]pooledState `json:"entries"`
}

// pooledState is one account's persisted health.
type pooledState struct {
	State    string `json:"state,omitempty"`
	Until    int64  `json:"until,omitempty"`
	LastUsed int64  `json:"last_used,omitempty"`
	Fails    int    `json:"fails,omitempty"`
	Note     string `json:"note,omitempty"`
}

// pool is the credential pool.
type pool struct {
	mu      sync.Mutex
	entries []*entry
	loaded  bool

	// storePath is where credentials are persisted; statePath where health is.
	// Both are empty when the module has no data directory, in which case
	// nothing is ever written.
	storePath string
	statePath string

	// configured is the roster from the config, used on the first load only.
	configured []account

	// flushEvery debounces state writes; stateDirty records that one is owed.
	flushEvery time.Duration
	stateDirty bool
	lastFlush  time.Time

	// onSave is called (outside the lock) after a credential store write, so a
	// caller can observe persistence failures in tests.
	onSave func(error)
}

// newPool builds an empty pool.
func newPool() *pool {
	return &pool{flushEvery: defaultStoreFlush}
}

// ---------------------------------------------------------------------------
// loading and saving
// ---------------------------------------------------------------------------

// load reads the persisted credential store, merging the configured roster in
// on the first call only.
//
// A store that cannot be read is not fatal: the configured roster still
// applies, which is what keeps a corrupt accounts.json from wedging the
// module.  The same is true of the state file — health is an optimisation.
func (p *pool) load(configured []account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.loaded {
		return
	}
	p.loaded = true
	p.configured = configured

	var store accountsStore
	if p.storePath != "" {
		if err := core.ReadJSON(p.storePath, &store); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				// Logged by the caller through onSave; a read failure is not
				// distinguishable from "no file yet" for the caller's purpose.
				p.reportLocked(err)
			}
		} else {
			for _, a := range store.Accounts {
				if a.accountID() == "" {
					continue
				}
				p.upsertLocked(&entry{acct: a, state: stateUnknown, origin: originStored, removable: true})
			}
		}
	}
	for _, a := range configured {
		if a.accountID() == "" {
			continue
		}
		p.upsertLocked(&entry{acct: a, state: stateUnknown, origin: originConfig})
	}

	var state poolState
	if p.statePath != "" {
		if err := core.ReadJSON(p.statePath, &state); err == nil {
			for id, st := range state.Entries {
				e := p.findLocked(id)
				if e == nil {
					continue
				}
				e.state = st.State
				e.until = st.Until
				e.lastUsed = st.LastUsed
				e.fails = st.Fails
				e.note = st.Note
			}
		}
	}
}

// reportLocked forwards a persistence problem to the observer, if any.
func (p *pool) reportLocked(err error) {
	if err != nil && p.onSave != nil {
		p.onSave(err)
	}
}

// upsertLocked adds an entry, or overlays the credential fields of one that is
// already there.  The stored copy wins on conflicts except for a credential
// whose stored form is incomplete.
func (p *pool) upsertLocked(e *entry) {
	if existing := p.findLocked(e.id()); existing != nil {
		existing.acct = mergeAccount(e.acct, existing.acct)
		if existing.origin == "" {
			existing.origin = e.origin
		}
		if e.origin == originConfig {
			existing.origin = originConfig
			existing.removable = false
		}
		return
	}
	if e.state == "" {
		e.state = stateUnknown
	}
	p.entries = append(p.entries, e)
}

// findLocked returns the entry with this id, or nil.
func (p *pool) findLocked(id string) *entry {
	if id == "" {
		return nil
	}
	for _, e := range p.entries {
		if e.id() == id {
			return e
		}
	}
	return nil
}

// saveLocked writes the credential store.  The caller must hold the lock.
func (p *pool) saveLocked() error {
	if p.storePath == "" {
		return nil
	}
	store := accountsStore{Version: accountsVersion}
	for _, e := range p.entries {
		a := e.acct
		// A config-sourced account is not persisted: the config is its home,
		// and writing it here would duplicate the secret and let the two
		// copies drift.
		if e.origin == originConfig {
			continue
		}
		store.Accounts = append(store.Accounts, a)
	}
	return core.WriteJSONAtomic(p.storePath, store)
}

// saveStateLocked writes the health state file.  The caller must hold the lock.
func (p *pool) saveStateLocked() {
	if p.statePath == "" {
		return
	}
	state := poolState{Version: 1, Entries: make(map[string]pooledState, len(p.entries))}
	for _, e := range p.entries {
		if e.state == "" || e.state == stateUnknown {
			continue
		}
		state.Entries[e.id()] = pooledState{
			State:    e.state,
			Until:    e.until,
			LastUsed: e.lastUsed,
			Fails:    e.fails,
			Note:     e.note,
		}
	}
	if err := core.WriteJSONAtomic(p.statePath, state); err != nil {
		p.reportLocked(err)
	}
}

// persist writes both files, honouring the debounce for the health state.
// It is safe to call on a pool with no data directory, where it does nothing.
func (p *pool) persist(force bool) {
	p.mu.Lock()
	if p.storePath == "" && p.statePath == "" {
		p.mu.Unlock()
		return
	}
	if err := p.saveLocked(); err != nil {
		p.mu.Unlock()
		if p.onSave != nil {
			p.onSave(err)
		}
		return
	}
	if !force && p.flushEvery > 0 && time.Since(p.lastFlush) < p.flushEvery {
		p.stateDirty = true
		p.mu.Unlock()
		return
	}
	p.stateDirty = false
	p.lastFlush = time.Now()
	p.saveStateLocked()
	p.mu.Unlock()
}

// ---------------------------------------------------------------------------
// picking
// ---------------------------------------------------------------------------

// len is the number of pooled credentials.
func (p *pool) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// availableLocked reports whether an entry may serve a request now, and
// publishes the state label that matches the answer.  It is the single source
// of truth for both, so Status can never disagree with the picker.
func (p *pool) availableLocked(e *entry, now time.Time) bool {
	if e == nil {
		return false
	}
	if e.acct.Disabled {
		e.state = stateInvalid
		return false
	}
	if e.state == stateInvalid {
		return false
	}
	nowMS := now.UnixMilli()
	if e.until > nowMS {
		e.state = stateCooling
		return false
	}
	if e.until != 0 && e.until <= nowMS {
		// The cooldown expired: the account is a candidate again.
		e.until = 0
		if e.state == stateCooling {
			e.state = stateReady
		}
	}
	switch e.state {
	case stateInvalid:
		return false
	case stateCooling, stateExhausted:
		// A state without a matching cooldown is stale bookkeeping from an
		// older run; treat it as usable rather than parking it forever.
		e.state = stateReady
	}
	if !e.acct.usable() {
		e.state = stateInvalid
		return false
	}
	e.state = stateReady
	return true
}

// pick returns the least-recently-used usable entry, skipping any id in skip.
//
// It returns nil when nothing can serve, which every caller must treat as
// "no credential" rather than an error to retry.
func (p *pool) pick(skip map[string]bool) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var best *entry
	for _, e := range p.entries {
		if skip[e.id()] {
			continue
		}
		if !p.availableLocked(e, now) {
			continue
		}
		if best == nil || e.lastUsed < best.lastUsed {
			best = e
		}
	}
	return best
}

// ready reports whether at least one credential can serve right now.
func (p *pool) ready() bool {
	return p.pick(nil) != nil
}

// all returns every entry, for the panel.
func (p *pool) all() []*entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*entry, len(p.entries))
	copy(out, p.entries)
	return out
}

// ---------------------------------------------------------------------------
// state transitions
// ---------------------------------------------------------------------------

// markUsed records a successful use.
func (p *pool) markUsed(e *entry) {
	if e == nil {
		return
	}
	p.mu.Lock()
	e.lastUsed = time.Now().UnixMilli()
	e.state = stateReady
	e.until = 0
	e.fails = 0
	p.mu.Unlock()
	p.persist(false)
}

// markFailure records a failed use, applying the cooldown the failure kind
// deserves.
func (p *pool) markFailure(e *entry, kind errKind, note string) {
	if e == nil {
		return
	}
	cooldown, state := cooldownFor(kind)
	p.mu.Lock()
	e.fails++
	e.note = core.Redact(cleanErrorText(note))
	if cooldown > 0 {
		until := time.Now().Add(cooldown).UnixMilli()
		// A failure never shortens an existing cooldown.
		if until > e.until {
			e.until = until
		}
		e.state = state
	} else if e.state == "" {
		e.state = stateUnknown
	}
	p.mu.Unlock()
	p.persist(false)
}

// markDead parks an account until an operator intervenes.
func (p *pool) markDead(e *entry, note string) {
	if e == nil {
		return
	}
	p.mu.Lock()
	e.state = stateInvalid
	e.until = 0
	e.fails++
	e.note = core.Redact(cleanErrorText(note))
	p.mu.Unlock()
	p.persist(true)
}

// cooldownFor maps a failure kind onto a cooldown and a state label.
//
// The mapping is deliberately conservative: a client error (a bad request this
// module built) must NOT cool the account down, because the next request will
// fail the same way and cooling it down only hides the bug.
func cooldownFor(kind errKind) (time.Duration, string) {
	switch kind {
	case kindQuota:
		return defaultQuotaCooldown, stateExhausted
	case kindAuth:
		return defaultCooldown, stateCooling
	case kindQueue, kindTransient, kindNetwork:
		return defaultShortCooldown, stateCooling
	default:
		return 0, stateUnknown
	}
}

// updateCredential replaces an entry's credential after a successful refresh.
func (p *pool) updateCredential(e *entry, a account) {
	if e == nil {
		return
	}
	p.mu.Lock()
	a.UpdatedAt = time.Now().Unix()
	// The refreshed credential wins; the old one only fills gaps.  Merging the
	// other way round would keep the expired access key and silently discard
	// every refresh, which is exactly the bug this call exists to fix.
	e.acct = mergeAccount(e.acct, a)
	e.state = stateReady
	e.until = 0
	e.fails = 0
	e.note = ""
	p.mu.Unlock()
	p.persist(true)
}

// put adds or replaces a credential, marking it removable unless it came from
// the config.
func (p *pool) put(a account, enabled bool) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	a.Disabled = !enabled
	e := p.findLocked(a.accountID())
	if e == nil {
		e = &entry{origin: originStored, removable: true}
		p.entries = append(p.entries, e)
	}
	e.acct = mergeAccount(a, e.acct)
	e.acct.Disabled = !enabled
	e.state = stateUnknown
	e.until = 0
	e.fails = 0
	e.note = ""
	if e.origin == "" {
		e.origin = originStored
		e.removable = true
	}
	// The write happens here rather than through persist() because the caller
	// expects a new credential to be durable by the time AddAccount returns.
	if err := p.saveLocked(); err != nil {
		p.reportLocked(err)
	}
	return e
}

// remove drops a credential.  It refuses config-sourced accounts, whose home
// is the config file rather than the store.
func (p *pool) remove(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.entries {
		if e.id() != id {
			continue
		}
		if !e.removable {
			return errors.New("codearts: this account comes from the config, so remove it there")
		}
		p.entries = append(p.entries[:i], p.entries[i+1:]...)
		if err := p.saveLocked(); err != nil {
			p.reportLocked(err)
			return err
		}
		return nil
	}
	return errors.New("codearts: no such account")
}

// setEnabled turns one credential on or off.
func (p *pool) setEnabled(id string, enabled bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(id)
	if e == nil {
		return errors.New("codearts: no such account")
	}
	e.acct.Disabled = !enabled
	if enabled {
		e.state = stateUnknown
		e.until = 0
		e.note = ""
	} else {
		e.state = stateInvalid
	}
	if err := p.saveLocked(); err != nil {
		p.reportLocked(err)
		return err
	}
	return nil
}

// revive clears runtime penalty state, leaving the credential alone
// (core.Reviver).
func (p *pool) revive(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(id)
	if e == nil {
		return errors.New("codearts: no such account")
	}
	e.state = stateUnknown
	e.until = 0
	e.fails = 0
	e.note = ""
	p.stateDirty = true
	p.saveStateLocked()
	return nil
}

// snapshot renders the pool as the panel's per-account view.  It never
// includes a secret — only the identity, the expiry and the reason.
func (p *pool) snapshot() []core.AccountStatus {
	p.mu.Lock()
	entries := make([]*entry, len(p.entries))
	copy(entries, p.entries)
	now := time.Now()
	for _, e := range entries {
		p.availableLocked(e, now)
	}
	p.mu.Unlock()

	// A stable order keeps the panel from reshuffling on every refresh.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].id() < entries[j].id() })

	out := make([]core.AccountStatus, 0, len(entries))
	for _, e := range entries {
		st := core.AccountStatus{
			ID:        e.id(),
			Label:     e.label(),
			Enabled:   !e.acct.Disabled,
			State:     e.state,
			ExpiresAt: e.acct.expiresAtString(),
			Note:      e.note,
			Identity:  e.acct.identity(),
		}
		extra := map[string]any{}
		if e.acct.UserName != "" {
			extra["user_name"] = e.acct.UserName
		}
		if e.acct.UserID != "" {
			extra["user_id"] = e.acct.UserID
		}
		if e.acct.DomainID != "" {
			extra["domain_id"] = e.acct.DomainID
		}
		if e.origin != "" {
			extra["origin"] = e.origin
		}
		extra["removable"] = e.removable
		extra["refreshable"] = e.acct.refreshable()
		if e.acct.ExpiresAt > 0 {
			extra["expires_in_seconds"] = int64(time.Until(time.Unix(e.acct.ExpiresAt, 0)).Seconds())
		}
		if e.lastUsed > 0 {
			extra["last_used"] = time.UnixMilli(e.lastUsed).UTC().Format(time.RFC3339)
		}
		st.Extra = extra
		out = append(out, st)
	}
	return out
}

// summary is a one-line description of the pool for Status.Detail.
func (p *pool) summary() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	total, usable := len(p.entries), 0
	states := map[string]int{}
	for _, e := range p.entries {
		if p.availableLocked(e, now) {
			usable++
		}
		states[e.state]++
	}
	if total == 0 {
		return "no credential: add an account or sign in"
	}
	parts := make([]string, 0, len(states))
	for _, s := range []string{stateReady, stateCooling, stateExhausted, stateInvalid, stateUnknown} {
		if n := states[s]; n > 0 {
			parts = append(parts, s+"="+itoa(n))
		}
	}
	sort.Strings(parts)
	return itoa(usable) + "/" + itoa(total) + " usable (" + joinComma(parts) + ")"
}

// itoa is a small dependency-free integer formatter.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// joinComma joins with commas, avoiding an import for one call site.
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
