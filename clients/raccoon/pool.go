package raccoon

import (
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// Where an account came from. A config-sourced account cannot be removed
// through the panel (the operator has to drop it from the config).
const (
	originConfig = "config"
	originStored = "stored"
	originEnv    = "env"
	originImport = "import"
)

// Account states reported to the panel.
const (
	stateReady     = "ready"
	stateCooling   = "cooling"
	stateExhausted = "exhausted"
	stateInvalid   = "invalid"
	stateUnknown   = "unknown"
)

// Failure kinds the pool understands.
type errKind string

const (
	kindNone      errKind = ""
	kindNetwork   errKind = "network"
	kindTransient errKind = "transient"
	kindAuth      errKind = "auth"
	kindQuota     errKind = "quota"
	kindClient    errKind = "client"
)

// account is one stored credential. The credential fields are inlined so the
// on-disk schema is exactly the vendor's credential schema plus our
// bookkeeping.
type account struct {
	credential

	ID            string `json:"id,omitempty"`
	Label         string `json:"label,omitempty"`
	Disabled      bool   `json:"disabled,omitempty"`
	Note          string `json:"note,omitempty"`
	Origin        string `json:"origin,omitempty"`
	CreatedAt     int64  `json:"created_at,omitempty"`
	LastUsed      int64  `json:"last_used,omitempty"`
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
	LastError     string `json:"last_error,omitempty"`
}

// id is the pool's stable key. It survives a token refresh (which rewrites
// AccessToken), which is why the vendor user id is preferred over the token.
func (a account) id() string {
	if v := strings.TrimSpace(a.ID); v != "" {
		return v
	}
	if v := strings.TrimSpace(a.UserID); v != "" {
		return "uid:" + v
	}
	tok := strings.TrimSpace(a.AccessToken)
	if len(tok) > 16 {
		tok = tok[:16]
	}
	if tok != "" {
		return "tok:" + tok
	}
	return "tok:"
}

func (a account) label() string {
	if v := strings.TrimSpace(a.Label); v != "" {
		return v
	}
	if v := strings.TrimSpace(a.Nickname); v != "" {
		return v
	}
	if v := strings.TrimSpace(a.Phone); v != "" {
		return maskPhone(v)
	}
	if v := strings.TrimSpace(a.UserID); v != "" {
		return v
	}
	return "raccoon account"
}

func (a account) cred() credential { return a.credential }

// entry is the in-memory view: the credential plus live health.
type entry struct {
	acct  account
	state string
	until time.Time
	note  string
	fails int
}

// healthRecord is one line of state.json. It deliberately carries no
// credential material.
type healthRecord struct {
	State         string `json:"state,omitempty"`
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastUsed      int64  `json:"last_used,omitempty"`
	Disabled      bool   `json:"disabled,omitempty"`
}

type cooldownConfig struct {
	def   time.Duration
	short time.Duration
	quota time.Duration
}

// cooldownFor maps a failure kind to a park duration and the state label
// that goes with it.
//
// kindClient is deliberately absent.  A client error is a 400 the vendor
// rejected, a 404 for a model it does not serve, or a local decode failure:
// the REQUEST was wrong, not the credential.  Parking the account would hide
// the bug and answer "no healthy account" for every OTHER model on that
// account until the park lapsed.  retryable() below already draws half of this
// distinction ("a bad request will be bad everywhere, so do not rotate"); not
// rotating is only half the insight, because the account that sent it is still
// healthy and must stay selectable.
func cooldownFor(kind errKind, c cooldownConfig) (time.Duration, string) {
	switch kind {
	case kindQuota:
		return c.quota, stateExhausted
	case kindNetwork, kindTransient:
		return c.short, stateCooling
	case kindAuth:
		return c.def, stateCooling
	default:
		return 0, stateUnknown
	}
}

func retryable(kind errKind) bool {
	switch kind {
	case kindNetwork, kindTransient, kindAuth, kindQuota:
		return true
	}
	return false
}

func unixMillis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func millisTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

type pool struct {
	mu         sync.Mutex
	entries    []*entry
	storePath  string
	statePath  string
	flushEvery time.Duration
	lastFlush  time.Time
	loaded     bool
	logf       func(string, ...any)
	cd         cooldownConfig
	now        func() time.Time
}

func newPool(storePath, statePath string, flushEvery time.Duration, cd cooldownConfig, logf func(string, ...any)) *pool {
	if flushEvery <= 0 {
		flushEvery = defaultStoreFlush
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &pool{
		storePath:  storePath,
		statePath:  statePath,
		flushEvery: flushEvery,
		logf:       logf,
		cd:         cd,
		now:        time.Now,
	}
}

// load merges the persisted store and health with the configured roster.
//
// The store is read only on the FIRST load. Afterwards the configured set is
// the roster, so an account the config stopped naming is dropped rather than
// silently resurrected from disk.
func (p *pool) load(configured []account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loadLocked(configured)
}

func (p *pool) loadLocked(configured []account) {
	if !p.loaded {
		stored := p.readStoreLocked()
		health := p.readHealthLocked()
		for i := range stored {
			e := &entry{acct: stored[i]}
			if h, ok := health[e.acct.id()]; ok {
				e.acct.LastUsed = h.LastUsed
				e.note = h.LastError
				if h.State != "" {
					e.state = h.State
				}
				if h.Disabled {
					e.acct.Disabled = true
				}
			}
			e.until = millisTime(e.acct.CooldownUntil)
			p.entries = append(p.entries, e)
		}
		p.loaded = true
	}
	for _, a := range configured {
		p.putLocked(a)
	}
	p.saveLocked()
}

func (p *pool) reload(configured []account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loaded = false
	p.entries = nil
	p.loadLocked(configured)
}

func (p *pool) readStoreLocked() []account {
	if p.storePath == "" {
		return nil
	}
	var stored []account
	if err := core.ReadJSON(p.storePath, &stored); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.logf("raccoon: cannot read %s: %v", p.storePath, err)
		}
		return nil
	}
	return stored
}

func (p *pool) readHealthLocked() map[string]healthRecord {
	out := map[string]healthRecord{}
	if p.statePath == "" {
		return out
	}
	if err := core.ReadJSON(p.statePath, &out); err != nil && !errors.Is(err, os.ErrNotExist) {
		p.logf("raccoon: cannot read %s: %v", p.statePath, err)
	}
	return out
}

// mergeAccount folds b into a: every non-empty field of b wins, and a
// disabled flag is sticky (re-adding a credential can never quietly
// re-enable a parked account).
func mergeAccount(a, b account) account {
	if b.ID != "" {
		a.ID = b.ID
	}
	if b.Label != "" {
		a.Label = b.Label
	}
	if b.AccessToken != "" {
		a.AccessToken = b.AccessToken
	}
	if b.RefreshToken != "" {
		a.RefreshToken = b.RefreshToken
	}
	if b.ExpiresAt != "" {
		a.ExpiresAt = b.ExpiresAt
	}
	if b.OfficeIdentity != "" {
		a.OfficeIdentity = b.OfficeIdentity
	}
	if b.UserID != "" {
		a.UserID = b.UserID
	}
	if b.Nickname != "" {
		a.Nickname = b.Nickname
	}
	if b.Phone != "" {
		a.Phone = b.Phone
	}
	if b.DeviceID != "" {
		a.DeviceID = b.DeviceID
	}
	if b.Origin != "" {
		a.Origin = b.Origin
	}
	if b.Note != "" {
		a.Note = b.Note
	}
	if b.CreatedAt != 0 {
		a.CreatedAt = b.CreatedAt
	}
	a.Disabled = a.Disabled || b.Disabled
	return a
}

func (p *pool) saveLocked() {
	if p.storePath == "" {
		return
	}
	out := make([]account, 0, len(p.entries))
	for _, e := range p.entries {
		a := e.acct
		a.CooldownUntil = unixMillis(e.until)
		a.LastError = e.note
		out = append(out, a)
	}
	if err := core.WriteJSONAtomic(p.storePath, out); err != nil {
		p.logf("raccoon: cannot write %s: %v", p.storePath, err)
	}
}

func (p *pool) flushStateLocked(force bool) {
	now := p.now()
	if !force && !p.lastFlush.IsZero() && now.Sub(p.lastFlush) < p.flushEvery {
		return
	}
	p.lastFlush = now
	p.saveLocked()
	if p.statePath == "" {
		return
	}
	rec := make(map[string]healthRecord, len(p.entries))
	for _, e := range p.entries {
		rec[e.acct.id()] = healthRecord{
			State:         e.state,
			CooldownUntil: unixMillis(e.until),
			LastError:     e.note,
			LastUsed:      e.acct.LastUsed,
			Disabled:      e.acct.Disabled,
		}
	}
	if err := core.WriteJSONAtomic(p.statePath, rec); err != nil {
		p.logf("raccoon: cannot write %s: %v", p.statePath, err)
	}
}

func (p *pool) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

func (p *pool) all() []*entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*entry, len(p.entries))
	copy(out, p.entries)
	return out
}

func (p *pool) find(id string) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.findLocked(id)
}

func (p *pool) findLocked(id string) *entry {
	for _, e := range p.entries {
		if e.acct.id() == id {
			return e
		}
	}
	return nil
}

// usableLocked is the single source of truth for "could this account serve a
// request right now". It also publishes the matching state label, so
// ready(), pick() and snapshot() can never disagree about an account.
func (p *pool) usableLocked(e *entry, now time.Time) bool {
	if e.acct.Disabled {
		e.state = stateInvalid
		if e.note == "" {
			e.note = "disabled by the operator"
		}
		return false
	}
	if strings.TrimSpace(e.acct.AccessToken) == "" {
		e.state = stateInvalid
		e.note = "no access token"
		return false
	}
	if !e.until.IsZero() && e.until.After(now) {
		if e.state == "" || e.state == stateReady {
			e.state = stateCooling
		}
		return false
	}
	if !e.until.IsZero() && !e.until.After(now) {
		e.until = time.Time{}
		e.fails = 0
		e.state = stateReady
		e.note = ""
	}
	if e.state != stateReady {
		e.state = stateReady
		e.note = ""
	}
	return true
}

// usable returns the accounts that may serve a request now, in LRU order.
func (p *pool) usable() []*entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.usableLockedList()
}

func (p *pool) usableLockedList() []*entry {
	now := p.now()
	out := make([]*entry, 0, len(p.entries))
	for _, e := range p.entries {
		if p.usableLocked(e, now) {
			out = append(out, e)
		}
	}
	sortEntriesByLastUsed(out)
	return out
}

func (p *pool) ready() bool {
	return len(p.usable()) > 0
}

// pick returns the least-recently-used usable account that is not in skip.
func (p *pool) pick(skip map[string]bool) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range core.LowestPriorityTier("raccoon", p.usableLockedList(), func(e *entry) string { return e.acct.id() }) {
		if skip != nil && skip[e.acct.id()] {
			continue
		}
		return e
	}
	return nil
}

// refreshDue lists the accounts whose access token is inside the renewal
// window (or already past it). It filters ONLY on refreshability, never on
// the enabled flag: disabling an account affects automatic selection, not
// whether its credential has to stay fresh.
func (p *pool) refreshDue(margin time.Duration) []*entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var out []*entry
	for _, e := range p.entries {
		if !e.acct.cred().refreshable() {
			continue
		}
		if e.acct.cred().expired(now) || e.acct.cred().needsRefresh(now, margin) {
			out = append(out, e)
		}
	}
	return out
}

func (p *pool) snapshot() []core.AccountStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]core.AccountStatus, 0, len(p.entries))
	for _, e := range p.entries {
		p.usableLocked(e, now)
		extra := map[string]any{
			"failures":    e.fails,
			"refreshable": e.acct.cred().refreshable(),
			"origin":      e.acct.Origin,
		}
		if e.acct.UserID != "" {
			extra["user_id"] = e.acct.UserID
		}
		if e.acct.Phone != "" {
			extra["phone"] = maskPhone(e.acct.Phone)
		}
		if e.acct.OfficeIdentity != "" {
			extra["office_identity"] = e.acct.OfficeIdentity
		}
		st := core.AccountStatus{
			ID:       e.acct.id(),
			Label:    panelLabel(e.acct),
			Enabled:  !e.acct.Disabled,
			State:    e.state,
			Note:     e.note,
			Identity: e.acct.cred().identity(),
			Extra:    extra,
		}
		if ms, ok := e.acct.cred().expiresAtMs(); ok {
			st.ExpiresAt = time.UnixMilli(ms).UTC().Format(time.RFC3339)
			extra["expires_in"] = int64(time.Until(time.UnixMilli(ms)).Seconds())
		}
		if !e.until.IsZero() && e.until.After(now) {
			st.Extra["cooldown_until"] = e.until.UTC().Format(time.RFC3339)
			st.Note = withRemaining(st.Note, e.until.Sub(now))
		}
		out = append(out, st)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (p *pool) summary() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	counts := map[string]int{}
	now := p.now()
	for _, e := range p.entries {
		p.usableLocked(e, now)
		counts[e.state]++
	}
	if len(p.entries) == 0 {
		return "no accounts"
	}
	order := []string{stateReady, stateCooling, stateExhausted, stateInvalid, stateUnknown}
	parts := make([]string, 0, len(order))
	for _, s := range order {
		if n := counts[s]; n > 0 {
			parts = append(parts, itoa(n)+" "+s)
		}
	}
	return strings.Join(parts, ", ")
}

func (e *entry) free() {
	e.until = time.Time{}
	e.state = stateReady
	e.note = ""
	e.fails = 0
}

func (e *entry) park(until time.Time, state string) {
	e.until = until
	e.state = state
}

func (p *pool) markUsed(e *entry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e.acct.LastUsed = p.now().UnixMilli()
	e.free()
	p.flushStateLocked(false)
}

func (p *pool) markFailure(e *entry, kind errKind, msg string) {
	p.markFailureWith(e, kind, msg, 0)
}

// markFailureWith parks an account after a failure. retryAfter can only
// LENGTHEN the cooldown. A non-retryable kind gets no cooldown at all: the
// fault was not the credential's, so the account must stay in rotation.
func (p *pool) markFailureWith(e *entry, kind errKind, msg string, retryAfter time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e.fails++
	e.note = msg
	d, state := cooldownFor(kind, p.cd)
	if retryAfter > d {
		d = retryAfter
		if state == "" || state == stateUnknown {
			state = stateCooling
		}
	}
	if d > 0 {
		e.park(p.now().Add(d), state)
	} else {
		e.state = state
	}
	p.flushStateLocked(true)
}

func (p *pool) markDead(e *entry, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e.acct.Disabled = true
	e.state = stateInvalid
	e.note = msg
	p.flushStateLocked(true)
}

func (p *pool) revive(e *entry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e.acct.Disabled = false
	e.free()
	p.flushStateLocked(true)
}

// put inserts or merges by id and persists.
func (p *pool) put(a account) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.putLocked(a)
	p.flushStateLocked(true)
	return e
}

func (p *pool) putLocked(a account) *entry {
	if a.CreatedAt == 0 {
		a.CreatedAt = p.now().Unix()
	}
	id := a.id()
	for _, e := range p.entries {
		if e.acct.id() == id {
			e.acct = mergeAccount(e.acct, a)
			return e
		}
	}
	e := &entry{acct: a, state: stateReady}
	p.entries = append(p.entries, e)
	return e
}

func (p *pool) remove(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.entries {
		if e.acct.id() == id {
			p.entries = append(p.entries[:i], p.entries[i+1:]...)
			p.flushStateLocked(true)
			return true
		}
	}
	return false
}

// setEnabled is a hard park on disable: the state becomes `invalid` with an
// operator-facing note, and enabling restores the account to rotation.
func (p *pool) setEnabled(id string, enabled bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(id)
	if e == nil {
		return errors.New("raccoon: unknown account " + id)
	}
	e.acct.Disabled = !enabled
	if enabled {
		e.free()
	} else {
		e.state = stateInvalid
		e.note = "disabled by the operator"
	}
	p.flushStateLocked(true)
	return nil
}

// applyRefresh writes the result of a successful token renewal back onto the
// stored credential. The new expiry MUST be written too: otherwise the panel
// keeps showing "expired" while chat works fine.
func (p *pool) applyRefresh(id string, tok refreshResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.findLocked(id)
	if e == nil {
		return
	}
	e.acct.AccessToken = tok.AccessToken
	if strings.TrimSpace(tok.RefreshToken) != "" {
		e.acct.RefreshToken = tok.RefreshToken
	}
	if tok.ExpiresAt != "" {
		e.acct.ExpiresAt = tok.ExpiresAt
	}
	if tok.Nickname != "" {
		e.acct.Nickname = tok.Nickname
	}
	if tok.OfficeIdentity != "" {
		e.acct.OfficeIdentity = tok.OfficeIdentity
	}
	if tok.UserID != "" && e.acct.UserID == "" {
		e.acct.UserID = tok.UserID
	}
	e.free()
	p.flushStateLocked(true)
}

func sortEntriesByLastUsed(list []*entry) {
	// Insertion sort: stable, and these lists are tiny.
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].acct.LastUsed < list[j-1].acct.LastUsed; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

func panelLabel(a account) string {
	return firstNonEmpty(a.Note, a.label())
}

func withRemaining(note string, left time.Duration) string {
	if left <= 0 {
		return note
	}
	suffix := "cooling for " + left.Round(time.Second).String()
	if strings.TrimSpace(note) == "" {
		return suffix
	}
	return note + " (" + suffix + ")"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

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
