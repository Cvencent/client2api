package cline

import (
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// Account states.  They are the labels the panel and Status render.
const (
	stateReady     = "ready"
	stateCooling   = "cooling"
	stateExhausted = "exhausted"
	stateExpired   = "expired"
	stateInvalid   = "invalid"
	stateUnknown   = "unknown"
)

// cooldownConfig is the three park durations the pool applies.
type cooldownConfig struct {
	def   time.Duration
	short time.Duration
	quota time.Duration
}

// storeFile is the on-disk shape of accounts.json.
type storeFile struct {
	Accounts []account `json:"accounts"`
}

// stateFileData is the on-disk shape of state.json.
type stateFileData struct {
	Accounts map[string]healthRecord `json:"accounts"`
}

// pool owns the credential roster and every verdict about it.  It is the single
// source of truth for "which account may serve this request", so Status, pick
// and the retry loop can never disagree.
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

// newPool builds an empty pool bound to the module's two data files.
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

// --- loading and persistence ----------------------------------------------

// load merges the stored roster, the health mirror and the configured set.  The
// store is read on the first load only; reload is the one path that re-reads it.
func (p *pool) load(configured []account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.loaded {
		stored := p.readStoreLocked()
		health := p.readStateLocked()
		p.entries = nil
		for _, a := range stored {
			p.putLocked(a)
		}
		for _, e := range p.entries {
			if h, ok := health[e.acct.id()]; ok {
				p.applyHealthLocked(e, h)
			}
		}
		p.loaded = true
	}
	// Config wins by id: it overlays the stored record field by field, but it
	// never erases a credential the store holds and the config does not name.
	for _, a := range configured {
		p.overlayLocked(a)
	}
}

// reload re-reads the store and the health file, then overlays the config.
func (p *pool) reload(configured []account) {
	p.mu.Lock()
	p.loaded = false
	p.mu.Unlock()
	p.load(configured)
}

// readStoreLocked reads accounts.json.  A missing or unreadable file is not an
// error: it just means there is nothing stored yet.
func (p *pool) readStoreLocked() []account {
	if p.storePath == "" {
		return nil
	}
	var sf storeFile
	if err := core.ReadJSON(p.storePath, &sf); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.logf("cline: could not read credential store: %v", err)
		}
		return nil
	}
	return sf.Accounts
}

// readStateLocked reads the credential-free health mirror.
func (p *pool) readStateLocked() map[string]healthRecord {
	if p.statePath == "" {
		return nil
	}
	var sd stateFileData
	if err := core.ReadJSON(p.statePath, &sd); err != nil {
		return nil
	}
	return sd.Accounts
}

// applyHealthLocked restores a verdict recorded before the restart.
func (p *pool) applyHealthLocked(e *entry, h healthRecord) {
	if h.State != "" {
		e.state = h.State
	}
	if h.CooldownUntil > 0 {
		e.until = millisTime(h.CooldownUntil)
	}
	if h.LastError != "" {
		e.note = h.LastError
	}
	if h.LastUsed > 0 {
		e.acct.LastUsed = h.LastUsed
	}
	if h.Disabled {
		e.acct.Disabled = true
	}
}

// putLocked inserts or updates an entry by account id, preserving the runtime
// verdict when the record already existed.
func (p *pool) putLocked(a account) *entry {
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

// overlayLocked applies a configured account onto the roster.  An unknown id is
// appended; a known one is merged field by field.
func (p *pool) overlayLocked(a account) *entry {
	return p.putLocked(a)
}

// mergeAccount overlays the non-empty fields of next onto prev.
func mergeAccount(prev, next account) account {
	out := prev
	if next.ID != "" {
		out.ID = next.ID
	}
	if next.Label != "" {
		out.Label = next.Label
	}
	if next.AccessToken != "" {
		out.AccessToken = next.AccessToken
	}
	if next.RefreshToken != "" {
		out.RefreshToken = next.RefreshToken
	}
	if next.AccountID != "" {
		out.AccountID = next.AccountID
	}
	if next.Email != "" {
		out.Email = next.Email
	}
	if next.Nickname != "" {
		out.Nickname = next.Nickname
	}
	if next.ExpiresAt != 0 {
		out.ExpiresAt = next.ExpiresAt
	}
	if next.Note != "" {
		out.Note = next.Note
	}
	out.Disabled = next.Disabled
	return out
}

// save persists the roster.  A write failure is logged, never fatal: a
// credential that cannot be written is still usable in memory.
func (p *pool) save() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saveLocked()
}

func (p *pool) saveLocked() {
	if p.storePath == "" {
		return
	}
	sf := storeFile{Accounts: make([]account, 0, len(p.entries))}
	for _, e := range p.entries {
		sf.Accounts = append(sf.Accounts, e.acct)
	}
	if err := core.WriteJSONAtomic(p.storePath, sf); err != nil {
		p.logf("cline: could not write credential store: %v", err)
	}
}

// flushState mirrors the verdicts to state.json behind a debounce.  force
// bypasses the debounce; it is used when a cooldown changes so a restart does
// not lose a 24 h quota park.
func (p *pool) flushState(force bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if !force && now.Sub(p.lastFlush) < p.flushEvery {
		return
	}
	p.lastFlush = now
	if p.statePath == "" {
		return
	}
	sd := stateFileData{Accounts: make(map[string]healthRecord, len(p.entries))}
	for _, e := range p.entries {
		sd.Accounts[e.acct.id()] = healthRecord{
			State:         e.state,
			CooldownUntil: unixMillis(e.until),
			LastError:     e.note,
			LastUsed:      e.acct.LastUsed,
			Disabled:      e.acct.Disabled,
		}
	}
	if err := core.WriteJSONAtomic(p.statePath, sd); err != nil {
		p.logf("cline: could not write state file: %v", err)
	}
}

// --- inspection ------------------------------------------------------------

func (p *pool) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// ready reports whether at least one account can serve a request now.
func (p *pool) ready() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for _, e := range p.entries {
		if p.availableLocked(e, now) {
			return true
		}
	}
	return false
}

// availableLocked is the single source of truth for a verdict.  State and Ready
// are computed from it, so two accounts in the same condition can never be
// reported differently.
func (p *pool) availableLocked(e *entry, now time.Time) bool {
	if e == nil {
		return false
	}
	if e.acct.Disabled {
		return false
	}
	if strings.TrimSpace(e.acct.AccessToken) == "" {
		return false
	}
	if e.until.After(now) {
		return false
	}
	return !e.acct.expired(now)
}

// usable returns every selectable entry, least-recently-used first.
func (p *pool) usable() []*entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var out []*entry
	for _, e := range p.entries {
		if p.availableLocked(e, now) {
			out = append(out, e)
		}
	}
	sortEntriesByLastUsed(out)
	return out
}

// refreshCandidates returns every entry holding a refresh token whose access
// token has already lapsed or is about to, least-recently-used first.
//
// It deliberately does NOT filter on availableLocked.  An account whose token
// has expired is by definition unavailable, so a list built from usable() can
// never contain the one account that most needs renewing: the module would
// never refresh it, the pool would stay empty, and every request would come
// back "not configured" until an operator re-ran the vendor's CLI.  That
// deadlock is exactly what this method exists to prevent.
func (p *pool) refreshCandidates(now time.Time, margin time.Duration) []*entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*entry
	for _, e := range p.entries {
		if e.acct.Disabled || !e.acct.refreshable() {
			continue
		}
		if e.acct.expired(now) || e.acct.needsRefresh(now, margin) {
			out = append(out, e)
		}
	}
	sortEntriesByLastUsed(out)
	return out
}

// pick returns the least-recently-used selectable entry not in skip, and marks
// it as used.  It never returns an entry that Status would call cooling,
// exhausted or invalid.
func (p *pool) pick(skip map[string]bool) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	var candidates []*entry
	for _, e := range p.entries {
		if skip != nil && skip[e.acct.id()] {
			continue
		}
		if p.availableLocked(e, now) {
			candidates = append(candidates, e)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sortEntriesByLastUsed(candidates)
	e := candidates[0]
	e.acct.LastUsed = now.Unix()
	return e
}

// usableEntry returns the entry for an id when it may serve a request right
// now and is not in skip, marking it used.  It is the affinity fast path: a
// conversation's bound account is served only while it is still healthy.
func (p *pool) usableEntry(id string, skip map[string]bool) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if skip != nil && skip[id] {
		return nil
	}
	now := p.now()
	for _, e := range p.entries {
		if e.acct.id() != id {
			continue
		}
		if !p.availableLocked(e, now) {
			return nil
		}
		e.acct.LastUsed = now.Unix()
		return e
	}
	return nil
}

// availableNow reports whether one entry may serve a request now.
func (p *pool) availableNow(e *entry) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.availableLocked(e, p.now())
}

// get returns the entry for an id, or nil.
func (p *pool) get(id string) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.acct.id() == id {
			return e
		}
	}
	return nil
}

// --- mutation --------------------------------------------------------------

// put inserts or replaces an account, returning the entry that now holds it.
// It persists the roster, because the record it carries is a credential.
func (p *pool) put(a account) *entry {
	p.mu.Lock()
	e := p.putLocked(a)
	p.saveLocked()
	p.mu.Unlock()
	return e
}

// revive clears the failure verdict on an entry.
func (p *pool) revive(e *entry) {
	if e == nil {
		return
	}
	p.mu.Lock()
	e.state = stateReady
	e.until = time.Time{}
	e.note = ""
	e.fails = 0
	p.mu.Unlock()
	p.flushState(true)
}

// markUsed records a successful use.
func (p *pool) markUsed(e *entry) {
	if e == nil {
		return
	}
	p.mu.Lock()
	e.acct.LastUsed = p.now().Unix()
	e.state = stateReady
	p.mu.Unlock()
	p.flushState(false)
}

// markFailure parks an entry according to its kind.
func (p *pool) markFailure(e *entry, kind errKind, msg string) {
	p.markFailureWith(e, kind, msg, 0)
}

// markFailureWith parks an entry, honouring a server-supplied Retry-After.  The
// header can only ever lengthen a park, never shorten it.
func (p *pool) markFailureWith(e *entry, kind errKind, msg string, retryAfter time.Duration) {
	if e == nil {
		return
	}
	d, state := cooldownFor(kind, p.cd)
	if retryAfter > d {
		d = retryAfter
	}
	now := p.now()
	p.mu.Lock()
	e.fails++
	e.note = cleanErrorText(msg)
	e.state = state
	if d > 0 {
		e.until = now.Add(d)
	} else {
		e.until = time.Time{}
	}
	p.mu.Unlock()
	p.flushState(true)
}

// markDead disables an entry for good.  Only a fresh authorisation revives it.
func (p *pool) markDead(e *entry, msg string) {
	if e == nil {
		return
	}
	p.mu.Lock()
	e.acct.Disabled = true
	e.state = stateInvalid
	e.note = cleanErrorText(msg)
	e.fails++
	p.mu.Unlock()
	p.save()
	p.flushState(true)
}

// remove drops an entry from the roster and persists the result.
func (p *pool) remove(id string) bool {
	p.mu.Lock()
	removed := false
	kept := p.entries[:0]
	for _, e := range p.entries {
		if e.acct.id() == id {
			removed = true
			continue
		}
		kept = append(kept, e)
	}
	p.entries = kept
	if removed {
		p.saveLocked()
	}
	p.mu.Unlock()
	if removed {
		p.flushState(true)
	}
	return removed
}

// setEnabled flips an account's disabled flag and persists it.
func (p *pool) setEnabled(id string, enabled bool) bool {
	p.mu.Lock()
	var found *entry
	for _, e := range p.entries {
		if e.acct.id() == id {
			e.acct.Disabled = !enabled
			if enabled {
				e.state = stateReady
				e.until = time.Time{}
				e.note = ""
				e.fails = 0
			} else {
				e.state = stateInvalid
			}
			found = e
			break
		}
	}
	if found != nil {
		p.saveLocked()
	}
	p.mu.Unlock()
	if found != nil {
		p.flushState(true)
	}
	return found != nil
}

// --- reporting -------------------------------------------------------------

// snapshot renders every account as a core.AccountStatus.
func (p *pool) snapshot() []core.AccountStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]core.AccountStatus, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, p.statusLocked(e, now))
	}
	return out
}

// statusLocked renders one entry.  The state is recomputed here rather than
// trusted from the field, so a park that has elapsed reads as ready.
func (p *pool) statusLocked(e *entry, now time.Time) core.AccountStatus {
	state := p.stateLocked(e, now)
	note := e.note
	if e.until.After(now) {
		note = withRemaining(note, e.until.Sub(now))
	}
	extra := map[string]any{"failures": e.fails}
	if e.acct.AccountID != "" {
		extra["account_id"] = e.acct.AccountID
	}
	if e.acct.ExpiresAt > 0 {
		extra["expires_at"] = time.Unix(e.acct.ExpiresAt, 0).UTC().Format(time.RFC3339)
		extra["expires_in"] = int64(e.acct.ExpiresAt - now.Unix())
	}
	if !e.until.IsZero() {
		extra["cooldown_until"] = e.until.UTC().Format(time.RFC3339)
	}
	if e.acct.RefreshToken != "" {
		extra["refreshable"] = true
	}
	st := core.AccountStatus{
		ID:       e.acct.id(),
		Label:    e.acct.label(),
		Enabled:  !e.acct.Disabled,
		State:    state,
		Note:     note,
		Extra:    extra,
		Identity: e.acct.AccountID,
	}
	if e.acct.ExpiresAt > 0 {
		st.ExpiresAt = time.Unix(e.acct.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	return st
}

// stateLocked recomputes one entry's state instead of trusting the field, so a
// park that has elapsed reads as ready and a lapsed token reads as expired.
//
// It is shared by Status and the panel listing on purpose: the two used to
// compute the state separately, and the panel's copy threw the answer away and
// rendered the stale field, which is how a row could read "ready" beside a
// negative expires_in while every request 503'd.
//
// The caller must hold p.mu.
func (p *pool) stateLocked(e *entry, now time.Time) string {
	state := e.state
	if state == "" {
		state = stateUnknown
	}
	switch {
	case e.acct.Disabled:
		state = stateInvalid
	case p.availableLocked(e, now):
		state = stateReady
	case e.until.After(now):
		// An active park keeps its more actionable label, even when the token
		// underneath it has also lapsed.
		state = stateCooling
		if e.state == stateExhausted {
			state = stateExhausted
		}
	case e.acct.expired(now):
		// The token has lapsed.  Reporting "ready" here would be a lie: the
		// pool will not hand this account to a request, and only a refresh can
		// change that.
		state = stateExpired
	}
	return state
}

// summary counts the roster by state, e.g. "2 ready, 1 exhausted".
func (p *pool) summary() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	counts := map[string]int{}
	var order []string
	for _, e := range p.entries {
		st := p.statusLocked(e, now)
		if _, seen := counts[st.State]; !seen {
			order = append(order, st.State)
		}
		counts[st.State]++
	}
	if len(order) == 0 {
		return "no accounts"
	}
	sort.SliceStable(order, func(i, j int) bool {
		return stateRank(order[i]) < stateRank(order[j])
	})
	parts := make([]string, 0, len(order))
	for _, s := range order {
		parts = append(parts, itoa(counts[s])+" "+s)
	}
	return strings.Join(parts, ", ")
}

// stateRank orders the summary so the healthy states come first.
func stateRank(s string) int {
	switch s {
	case stateReady:
		return 0
	case stateCooling:
		return 1
	case stateExhausted:
		return 2
	case stateExpired:
		return 3
	case stateUnknown:
		return 4
	case stateInvalid:
		return 5
	}
	return 6
}

// --- helpers ---------------------------------------------------------------

// cooldownFor maps a failure kind to a park duration and a state label.
//
// kindClient is deliberately absent.  A client error is a 404 for a model the
// vendor does not serve, a 400 this module built badly, or a local decode
// failure: the REQUEST was wrong, not the credential.  Parking the account
// would hide the bug and, far worse, answer 503 "no healthy account" for every
// OTHER model on that account until the park lapsed.  A live run hit exactly
// that: one chat naming a stale model id parked a perfectly good account, and
// the next chat naming a valid model was refused.
//
// kindOfErr's own doc comment already promises this ("anything else is a
// client-side failure that must not park the account"), so returning 0 here is
// the behaviour the module always claimed.
func cooldownFor(kind errKind, c cooldownConfig) (time.Duration, string) {
	switch kind {
	case kindQuota:
		return c.quota, stateExhausted
	case kindTransient, kindNetwork:
		return c.short, stateCooling
	case kindAuth:
		return c.def, stateCooling
	}
	return 0, stateUnknown
}

// sortEntriesByLastUsed is a stable insertion sort, so equal timestamps keep
// config order.
func sortEntriesByLastUsed(list []*entry) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].acct.LastUsed < list[j-1].acct.LastUsed; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// withRemaining appends a human cooldown remainder to a note.
func withRemaining(note string, left time.Duration) string {
	if left <= 0 {
		return note
	}
	rem := humanAge(left) + " left"
	if note == "" {
		return rem
	}
	return note + " (" + rem + ")"
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

// humanAge renders a duration as 0s / Ns / Nm / Nh.
func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Second:
		return "0s"
	case d < time.Minute:
		return itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return itoa(int(d/time.Minute)) + "m"
	default:
		return itoa(int(d/time.Hour)) + "h"
	}
}
