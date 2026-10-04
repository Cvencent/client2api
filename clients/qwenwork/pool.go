package qwenwork

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// pool.go owns the credentials and their health.
//
// The selection policy is the reference's: least-recently-used first, so a
// request only lands on an account that has been idle longest.
//
// Two files live in Deps.DataDir.  accounts.json is the credential store: the
// account records themselves, each carrying the cooldown that was in force when
// it was last written, which is what makes a 24 h quota cooldown survive a
// restart.  state.json is the health mirror: the same verdicts in a shape that
// never carries a credential, refreshed behind a small debounce so a failure
// does not become a disk write.
//
// The credential store is the authority for a cooldown.  A fresh authorisation
// rewrites the account record and so revives it; the health mirror is only ever
// read back for the LRU timestamp and the cooling/exhausted label, never to
// park an account the store says is live.

// Account states, rendered verbatim into core.AccountStatus.State.
const (
	stateReady     = "ready"
	stateCooling   = "cooling"
	stateExhausted = "exhausted"
	stateInvalid   = "invalid"
	stateUnknown   = "unknown"
)

// account is one credential.  The JSON tags are the on-disk schema of
// accounts.json; the field names deliberately differ from the wire (COSY wants
// `security_oauth_token`, we store access_token) so the two never get confused.
type account struct {
	ID            string `json:"id"`
	UID           string `json:"uid"`
	Nickname      string `json:"nickname"`
	Email         string `json:"email"`
	AccessToken   string `json:"access_token"`
	RefreshToken  string `json:"refresh_token"`
	ExpiresAt     int64  `json:"expires_at"` // unix seconds, 0 = unknown
	Disabled      bool   `json:"disabled"`
	Note          string `json:"note"`
	CreatedAt     int64  `json:"created_at"`
	LastUsed      int64  `json:"last_used"`
	CooldownUntil int64  `json:"cooldown_until"` // unix milliseconds
	LastError     string `json:"last_error"`
}

// id returns the stable key for an account.  The device flow mints an id, but a
// hand-written config entry may only carry a uid or a token, so fall back.
func (a account) id() string {
	if a.ID != "" {
		return a.ID
	}
	if a.UID != "" {
		return "uid:" + a.UID
	}
	if len(a.AccessToken) > 16 {
		return "tok:" + a.AccessToken[:16]
	}
	return "tok:" + a.AccessToken
}

// label is the human handle used in Status output: never the token.
func (a account) label() string {
	switch {
	case a.Nickname != "" && a.UID != "":
		return a.Nickname + " (" + a.UID + ")"
	case a.Nickname != "":
		return a.Nickname
	case a.UID != "":
		return a.UID
	case a.Email != "":
		return a.Email
	default:
		return "account"
	}
}

// usable mirrors the reference's isUsable: not disabled, not cooling, and not
// within 60 s of expiry (an account that expires mid-request is no use).
func (a account) usable(now time.Time) bool {
	if a.Disabled || a.AccessToken == "" {
		return false
	}
	if a.CooldownUntil > now.UnixMilli() {
		return false
	}
	if a.ExpiresAt > 0 && a.ExpiresAt*1000 <= now.UnixMilli()+60_000 {
		return false
	}
	return true
}

// needsRefresh reports whether the access token should be renewed before use.
func (a account) needsRefresh(now time.Time, margin time.Duration) bool {
	if a.RefreshToken == "" || a.Disabled || a.ExpiresAt <= 0 {
		return false
	}
	return a.ExpiresAt*1000-now.UnixMilli() < margin.Milliseconds()
}

// expired reports whether the access token is already past its expiry.
func (a account) expired(now time.Time) bool {
	return a.ExpiresAt > 0 && a.ExpiresAt <= now.Unix()
}

// entry is an account plus its live health.
type entry struct {
	acct  account
	state string
	until time.Time
	note  string
	fails int
}

// healthRecord is one account's line in state.json.  It is deliberately not an
// account: nothing that carries a credential can be written to the health file
// by accident.
type healthRecord struct {
	State         string `json:"state"`
	CooldownUntil int64  `json:"cooldown_until"` // unix milliseconds, 0 = none
	LastError     string `json:"last_error"`
	LastUsed      int64  `json:"last_used"`
	Disabled      bool   `json:"disabled"`
}

// unixMillis renders an instant for the files, where 0 means "no cooldown"
// rather than the zero time's absurd negative millisecond count.
func unixMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// millisTime is the inverse of unixMillis.
func millisTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

// pool holds every account and the round-robin cursor used to break LRU ties.
type pool struct {
	mu         sync.Mutex
	entries    []*entry
	storePath  string
	statePath  string
	flushEvery time.Duration
	lastFlush  time.Time
	loaded     bool
	logf       func(format string, args ...any)

	// cd is the cooldown policy's tuning, injected rather than read from a
	// global so a test can drive the policy directly.
	cd cooldownConfig

	// now is injectable so the cooldown policy can be tested without sleeping.
	now func() time.Time
}

// newPool builds an empty pool bound to two files inside Deps.DataDir.
func newPool(storePath, statePath string, flushEvery time.Duration, cd cooldownConfig, logf func(string, ...any)) *pool {
	if flushEvery <= 0 {
		flushEvery = defaultStoreFlush
	}
	return &pool{
		storePath:  storePath,
		statePath:  statePath,
		flushEvery: flushEvery,
		cd:         cd,
		logf:       logf,
		now:        time.Now,
	}
}

func (p *pool) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

// ---------------------------------------------------------------------------
// loading and persisting
// ---------------------------------------------------------------------------

// load reconciles the pool with the configured credentials.
//
// The credential store and the health mirror are read on the first load only:
// after that the configured set is the roster, so an account the config stopped
// naming is dropped rather than resurrected from a stale file.  reload is the
// one caller that wants the files re-read.
func (p *pool) load(configured []account) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var stored []account
	var health map[string]healthRecord
	if !p.loaded {
		if p.storePath != "" {
			if err := core.ReadJSON(p.storePath, &stored); err != nil && !os.IsNotExist(err) {
				p.log("qwenwork: reading %s: %v", filepath.Base(p.storePath), err)
			}
		}
		if p.statePath != "" {
			if err := core.ReadJSON(p.statePath, &health); err != nil && !os.IsNotExist(err) {
				p.log("qwenwork: reading %s: %v", filepath.Base(p.statePath), err)
			}
		}
	}

	merged := make([]account, 0, len(stored)+len(configured))
	seen := map[string]int{}
	add := func(a account) {
		key := a.id()
		if i, ok := seen[key]; ok {
			merged[i] = mergeAccount(merged[i], a)
			return
		}
		seen[key] = len(merged)
		merged = append(merged, a)
	}
	for _, a := range stored {
		add(a)
	}
	for _, a := range configured {
		add(a)
	}

	now := p.now()
	p.entries = p.entries[:0]
	for _, a := range merged {
		e := &entry{acct: a, state: stateReady}
		if h, ok := health[a.id()]; ok {
			// The health mirror contributes the LRU timestamp and, when the
			// store agrees that the account is parked, the reason it is parked.
			if h.LastUsed > e.acct.LastUsed {
				e.acct.LastUsed = h.LastUsed
			}
			e.note = h.LastError
			if h.State == stateExhausted {
				e.state = stateExhausted
			}
		}
		// The account record's own cooldown is the authority.
		e.until = millisTime(e.acct.CooldownUntil)
		switch {
		case e.acct.Disabled:
			e.state = stateInvalid
			e.note = firstNonEmpty(e.note, a.Note, "disabled")
		case e.until.After(now):
			if e.state != stateExhausted {
				e.state = stateCooling
			}
		}
		p.entries = append(p.entries, e)
	}
	p.loaded = true
	if len(p.entries) > 0 {
		p.log("qwenwork: %d account(s) loaded (%s)", len(p.entries), p.summaryLocked())
	}
	p.saveLocked()
}

// reload re-reads the credential store and the health mirror, then merges the
// config over them.  It exists for the device flow: the completed authorisation
// writes a credential straight to accounts.json, and the pool has to notice a
// file that changed underneath it.
func (p *pool) reload(configured []account) {
	p.mu.Lock()
	p.loaded = false
	p.mu.Unlock()
	p.load(configured)
}

// mergeAccount overlays b onto a: any field b actually carries wins, so an
// explicit config entry can correct a stale stored record.
func mergeAccount(a, b account) account {
	if b.UID != "" {
		a.UID = b.UID
	}
	if b.Nickname != "" {
		a.Nickname = b.Nickname
	}
	if b.Email != "" {
		a.Email = b.Email
	}
	if b.AccessToken != "" {
		a.AccessToken = b.AccessToken
	}
	if b.RefreshToken != "" {
		a.RefreshToken = b.RefreshToken
	}
	if b.ExpiresAt > 0 {
		a.ExpiresAt = b.ExpiresAt
	}
	if b.ID != "" {
		a.ID = b.ID
	}
	if b.Note != "" {
		a.Note = b.Note
	}
	a.Disabled = a.Disabled || b.Disabled
	return a
}

// save writes accounts.json.  Called only when credentials change (login,
// refresh), so the write rate is bounded by those events.
func (p *pool) save() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saveLocked()
}

func (p *pool) saveLocked() {
	if p.storePath == "" {
		return
	}
	accts := make([]account, 0, len(p.entries))
	for _, e := range p.entries {
		// The credential store carries the cooldown, so a restart knows about
		// the park without reading the health mirror.
		e.acct.CooldownUntil = unixMillis(e.until)
		e.acct.LastError = e.note
		accts = append(accts, e.acct)
	}
	if err := core.WriteJSONAtomic(p.storePath, accts); err != nil {
		p.log("qwenwork: writing %s: %v", filepath.Base(p.storePath), err)
	}
}

// flushState mirrors health to state.json, at most once per flushEvery unless
// force is set.  A failure here is logged and forgotten: health is a cache.
func (p *pool) flushState(force bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.flushStateLocked(force)
}

func (p *pool) flushStateLocked(force bool) {
	now := p.now()
	if !force && now.Sub(p.lastFlush) < p.flushEvery {
		return
	}
	p.lastFlush = now
	// The credential store is refreshed in the same breath: a cooldown that
	// only ever reached memory would be lost on the next restart.
	p.saveLocked()
	if p.statePath == "" {
		return
	}
	health := make(map[string]healthRecord, len(p.entries))
	for _, e := range p.entries {
		health[e.acct.id()] = healthRecord{
			State:         e.state,
			CooldownUntil: unixMillis(e.until),
			LastError:     e.note,
			LastUsed:      e.acct.LastUsed,
			Disabled:      e.acct.Disabled,
		}
	}
	if err := core.WriteJSONAtomic(p.statePath, health); err != nil {
		p.log("qwenwork: writing %s: %v", filepath.Base(p.statePath), err)
	}
}

// ---------------------------------------------------------------------------
// inspection
// ---------------------------------------------------------------------------

func (p *pool) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// usable returns the entries that could serve a request right now, oldest
// lastUsed first.  The sort is stable so equal timestamps keep config order.
func (p *pool) usable() []*entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.usableLocked()
}

func (p *pool) usableLocked() []*entry {
	now := p.now()
	out := make([]*entry, 0, len(p.entries))
	for _, e := range p.entries {
		if p.availableLocked(e, now) {
			out = append(out, e)
		}
	}
	sortEntriesByLastUsed(out)
	return out
}

// availableLocked is the single source of truth for "could this account serve a
// request right now": it publishes the matching state label and reports whether
// the entry is usable, so ready(), pick() and snapshot() can never disagree.
func (p *pool) availableLocked(e *entry, now time.Time) bool {
	switch {
	case e.acct.Disabled:
		e.state = stateInvalid
	case e.acct.AccessToken == "":
		e.state = stateInvalid
	case e.until.After(now):
		// Keep the label the failure earned: a quota exhaustion stays
		// "exhausted" for the whole day, anything else is "cooling".
		if e.state != stateExhausted {
			e.state = stateCooling
		}
	case e.acct.ExpiresAt > 0 && e.acct.ExpiresAt <= now.Unix():
		e.state = stateInvalid
		e.note = firstNonEmpty(e.note, "access token expired")
	default:
		if e.acct.usable(now) {
			e.state = stateReady
			return true
		}
		// Unusable for a reason this switch does not name: an expiry inside the
		// 60 s safety margin.  It is not broken, only about to be.
		e.state = stateCooling
	}
	return false
}

// ready reports whether any account could plausibly serve a request now.
func (p *pool) ready() bool {
	return len(p.usable()) > 0
}

// pick returns the least-recently-used usable entry that is not in skip, or nil.
func (p *pool) pick(skip map[string]bool) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range core.LowestPriorityTier("qwenwork", p.usableLocked(), func(e *entry) string { return e.acct.id() }) {
		if skip != nil && skip[e.acct.id()] {
			continue
		}
		return e
	}
	return nil
}

// snapshot renders every account for core.Status.  It is deliberately cheap:
// no network, no disk.
func (p *pool) snapshot() []core.AccountStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]core.AccountStatus, 0, len(p.entries))
	for _, e := range p.entries {
		p.availableLocked(e, now)
		st := core.AccountStatus{
			ID:      e.acct.id(),
			Label:   e.acct.label(),
			Enabled: !e.acct.Disabled,
			State:   e.state,
			Note:    e.note,
			Extra: map[string]any{
				"failures": e.fails,
			},
		}
		if e.acct.UID != "" {
			st.Extra["uid"] = e.acct.UID
		}
		if e.acct.ExpiresAt > 0 {
			expiry := time.Unix(e.acct.ExpiresAt, 0)
			st.ExpiresAt = expiry.UTC().Format(time.RFC3339)
			st.Extra["expires_in"] = int64(expiry.Sub(now).Seconds())
		}
		if e.until.After(now) {
			st.Note = withRemaining(st.Note, e.until.Sub(now))
			st.Extra["cooldown_until"] = e.until.UTC().Format(time.RFC3339)
		}
		out = append(out, st)
	}
	return out
}

// summary is a one-line, secret-free description of the pool.
func (p *pool) summary() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.summaryLocked()
}

func (p *pool) summaryLocked() string {
	if len(p.entries) == 0 {
		return "no accounts"
	}
	now := p.now()
	counts := map[string]int{}
	var order []string
	for _, e := range p.entries {
		p.availableLocked(e, now)
		st := e.state
		if _, ok := counts[st]; !ok {
			order = append(order, st)
		}
		counts[st]++
	}
	parts := make([]string, 0, len(order))
	for _, st := range order {
		parts = append(parts, itoa(counts[st])+" "+st)
	}
	return strings.Join(parts, ", ")
}

// ---------------------------------------------------------------------------
// mutation
// ---------------------------------------------------------------------------

// free clears an entry's cooldown, in memory and on its account record, so the
// two can never disagree about whether the account is parked.
func (e *entry) free() {
	e.until = time.Time{}
	e.acct.CooldownUntil = 0
	e.state = stateReady
}

// park records a cooldown that lasts until the given instant.
func (e *entry) park(until time.Time, state string) {
	e.until = until
	e.acct.CooldownUntil = unixMillis(until)
	e.state = state
}

// markUsed records a successful call, which is what makes the LRU rotate.
func (p *pool) markUsed(e *entry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e.acct.LastUsed = p.now().Unix()
	e.fails = 0
	e.note = ""
	e.free()
	p.flushStateLocked(false)
}

// markFailure applies the cooldown policy for kind and records the message.
func (p *pool) markFailure(e *entry, kind errKind, msg string) {
	p.markFailureWith(e, kind, msg, 0)
}

// markFailureWith is markFailure plus the upstream's own Retry-After, which may
// only ever lengthen the cooldown, never shorten it.
func (p *pool) markFailureWith(e *entry, kind errKind, msg string, retryAfter time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	d, label := p.cooldownForLocked(kind)
	if retryAfter > d {
		d = retryAfter
	}
	e.fails++
	e.note = cleanErrorText(msg)
	if d > 0 {
		e.park(now.Add(d), label)
	} else {
		e.free()
		e.state = stateUnknown
	}
	p.log("qwenwork: account %s -> %s for %s (%s)", e.acct.id(), e.state, d, e.note)
	p.flushStateLocked(false)
}

// markDead disables an account permanently (until a refresh revives it).  The
// reference does this when a refresh attempt itself fails.
func (p *pool) markDead(e *entry, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e.acct.Disabled = true
	e.state = stateInvalid
	e.note = cleanErrorText(msg)
	p.log("qwenwork: account %s disabled: %s", e.acct.id(), e.note)
	p.flushStateLocked(true)
}

// revive clears a cooldown and the streak that led to it after a successful
// refresh.  It is also the operator's revive, which is why the disable flag and
// the failure count go too: positive evidence and an explicit override both
// mean "stop treating this account as damaged".
func (p *pool) revive(e *entry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e.acct.Disabled = false
	e.free()
	e.note = ""
	e.fails = 0
	p.flushStateLocked(true)
}

// put inserts or updates an account by id (the device flow and the refresh path
// both use it) and persists the store.
func (p *pool) put(a account) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := a.id()
	for _, e := range p.entries {
		if e.acct.id() == key {
			e.acct = mergeAccount(e.acct, a)
			e.free()
			e.note = ""
			p.saveLocked()
			return e
		}
	}
	if a.CreatedAt == 0 {
		a.CreatedAt = p.now().Unix()
	}
	e := &entry{acct: a, state: stateReady}
	p.entries = append(p.entries, e)
	p.saveLocked()
	return e
}

// cooldownForLocked maps a failure kind onto a cooldown duration and the state
// label to publish.  Pure apart from reading the pool's configured durations,
// which is what makes it directly testable.
func (p *pool) cooldownForLocked(kind errKind) (time.Duration, string) {
	return cooldownFor(kind, p.cd)
}

// cooldownConfig carries the tunable durations so cooldownFor stays pure.
type cooldownConfig struct {
	def   time.Duration
	short time.Duration
	quota time.Duration
}

// cooldownFor is the whole policy in one place: a quota exhaustion parks an
// account for a day, a transient failure for 15 s, an auth failure for the
// default minute, and a non-retryable failure earns no cooldown at all.
//
// kindClient is deliberately absent.  A bad request is the REQUEST's fault,
// not the credential's: parking the account would hide the bug and answer
// "no healthy account" for every OTHER model on that account until the park
// lapsed.  retryable() below already says as much ("a bad request will be bad
// on every account, so it is not retryable"); not rotating is only half the
// insight, because the account that sent it is still healthy.
func cooldownFor(kind errKind, c cooldownConfig) (time.Duration, string) {
	switch kind {
	case kindQuota:
		return c.quota, stateExhausted
	case kindTransient:
		return c.short, stateCooling
	case kindNetwork:
		return c.short, stateCooling
	case kindAuth:
		return c.def, stateCooling
	default:
		return 0, stateUnknown
	}
}

// retryable reports whether another account is worth trying after this kind of
// failure.  A bad request will be bad on every account, so it is not retryable.
func retryable(kind errKind) bool {
	switch kind {
	case kindQuota, kindTransient, kindNetwork, kindAuth:
		return true
	default:
		return false
	}
}

func sortEntriesByLastUsed(list []*entry) {
	// insertion sort: the lists are tiny (one entry per account) and stable
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j-1].acct.LastUsed > list[j].acct.LastUsed; j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
}

func withRemaining(note string, left time.Duration) string {
	if left < 0 {
		left = 0
	}
	suffix := " (" + itoa(int(left.Round(time.Second).Seconds())) + "s left)"
	if note == "" {
		return strings.TrimSpace("cooling" + suffix)
	}
	return note + suffix
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// itoa is a tiny helper so the status strings do not drag in strconv's error
// handling at every call site.
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
