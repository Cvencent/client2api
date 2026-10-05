package minimaxcode

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// Account states.  They are the module's own vocabulary; core.AccountStatus
// carries them through to the panel unchanged, which is why they are the words
// a human would use.
const (
	stateReady     = "ready"
	stateCooling   = "cooling"
	stateExhausted = "exhausted"
	stateInvalid   = "invalid"
)

// Account sources.
const (
	sourceConfig     = "config"
	sourceDiscovered = "discovered"
	sourceManaged    = "managed"
)

// noteLoginRequired is the LastError a row carries when the vendor has killed
// the authorization itself -- an expired access token AND a refresh token it
// refuses to exchange.  No local retry fixes that; only a fresh sign-in in the
// MiniMax Code client does.  The word is the panel's own vocabulary (see
// NOTE_WORDS in index.html), so the row reads "需要重新登录" instead of the
// downstream "HTTP 401" that hid the real cause.
const noteLoginRequired = "login_required"

// persistedFile holds the runtime state that must survive a restart.  It
// deliberately contains no credentials: the tokens live in the desktop client's
// own store, or in the module's managed_accounts.json.
const persistedFile = "accounts.json"
const persistedVersion = 1

type persistedAccount struct {
	ID            string `json:"id"`
	State         string `json:"state,omitempty"`
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
	Failures      int    `json:"failures,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastUsed      int64  `json:"last_used,omitempty"`
}

type persistedState struct {
	Version  int                `json:"version"`
	Accounts []persistedAccount `json:"accounts"`
	Removed  []string           `json:"removed,omitempty"`
}

// Account is one credential this module may relay with.
type Account struct {
	ID      string
	Label   string
	Token   string
	BaseURL string
	Source  string
	Origin  string
	Enabled bool

	// Managed is true when the row was added through the panel, which is what
	// makes it removable.  A discovered row can only be hidden.
	Managed bool

	// Refresh bookkeeping: where the token came from and how to write a new one
	// back.  A managed or config account has none of this and simply cannot be
	// refreshed.
	AuthPath   string
	RecordKey  string
	Generation int

	// RefreshToken is kept only for credentials read from the desktop client's
	// store; a config or panel account has none and cannot be refreshed.
	RefreshToken string

	// ClientID is the OAuth client the credential was minted for.  The vendor
	// only accepts the refresh when the same client id is presented, and the
	// two clients in the wild (mcode-public for the desktop app, mcode_tool
	// for the CLI) are not interchangeable.  Discovered rows derive it from
	// the store path; a row the panel signed in carries it explicitly.
	ClientID string

	// ExpiresAt is the access token's expiry, when the store recorded one.
	ExpiresAt time.Time

	// Runtime state, guarded by the pool's mutex.
	State         string
	CooldownUntil time.Time
	Failures      int
	LastError     string
	LastUsed      time.Time
}

// secret returns the credential actually sent upstream.
func (a *Account) secret() string {
	if a == nil {
		return ""
	}
	return a.Token
}

// pool owns every credential and every piece of runtime state.
type pool struct {
	dir  string
	cfg  *Config
	http *http.Client
	logf func(string, ...any)

	mu       sync.Mutex
	loaded   bool
	accounts []*Account
	cursor   int
	lastErr  string

	// dirty records that a health transition happened since the last write, and
	// wrote that a file exists to be corrected.  Together they let saveLocked
	// skip a rewrite when nothing actually changed: noteSuccess runs on every
	// successful request, and re-marshalling plus fsyncing the whole pool per
	// request while holding mu is pure cost.
	dirty bool
	wrote bool

	managed       []managedAccount
	managedLoaded bool
	removed       map[string]bool

	// refreshMu guards refreshLocks, the per-account single-flight table for
	// token refreshes.  See refreshLock for why it is per account.
	refreshMu    sync.Mutex
	refreshLocks map[string]*sync.Mutex
}

func newPool(dir string, cfg *Config, hc *http.Client, logf func(string, ...any)) *pool {
	return &pool{dir: dir, cfg: cfg, http: hc, logf: logf}
}

// ensureLocked loads the account list once.  Every failure inside it degrades
// rather than panics: a malformed state file must not take the module down.
func (p *pool) ensureLocked() {
	if p.loaded {
		return
	}
	p.loaded = true
	if p.removed == nil {
		p.removed = map[string]bool{}
	}
	if err := core.EnsureDir(p.dir); err != nil {
		p.logf("minimaxcode: cannot create %s: %v", p.dir, err)
	}

	state := p.loadStateLocked()
	for _, id := range state.Removed {
		if id != "" {
			p.removed[id] = true
		}
	}

	for _, ac := range p.cfg.Accounts {
		if strings.TrimSpace(ac.Token) == "" {
			continue
		}
		id := strings.TrimSpace(ac.ID)
		if id == "" {
			id = accountID("config", "", credentialTag(ac.Token))
		}
		p.addLocked(&Account{
			ID:      id,
			Label:   firstNonEmpty(strings.TrimSpace(ac.Label), id),
			Token:   strings.TrimSpace(ac.Token),
			BaseURL: strings.TrimSpace(ac.BaseURL),
			Source:  sourceConfig,
			Origin:  "configs/client2api.json#clients.minimaxcode.accounts",
			Enabled: ac.Enabled == nil || *ac.Enabled,
			State:   stateReady,
		})
	}

	if p.cfg.autoDiscover() {
		for _, cred := range discoverCredentials(p.cfg.authDir(), p.logf) {
			p.addLocked(&Account{
				ID:           cred.ID,
				Label:        firstNonEmpty(cred.Label, cred.ID),
				Token:        cred.Access,
				RefreshToken: cred.Refresh,
				Source:       sourceDiscovered,
				Origin:       cred.Path,
				Enabled:      true,
				State:        stateReady,
				AuthPath:     cred.Path,
				RecordKey:    cred.RecordKey,
				Generation:   cred.Generation,
				ExpiresAt:    cred.ExpiresAt,
			})
		}
	}

	// A signed-out desktop client leaves no record behind, so discovery finds
	// nothing for a credential it once held.  Re-materialise the rows the
	// persisted state still remembers: with no token they stay unselectable,
	// and the panel can show why and offer the revive that picks the
	// credential up again after the operator signs back in.
	var signedOut []string
	if p.cfg.autoDiscover() {
		for _, saved := range state.Accounts {
			if saved.ID == "" || p.removed[saved.ID] || p.accountLocked(saved.ID) != nil {
				continue
			}
			if ph, ok := p.signedOutPlaceholderLocked(saved.ID); ok {
				p.addLocked(ph)
				signedOut = append(signedOut, saved.ID)
			}
		}
	}

	p.loadManagedLocked()
	p.applyStateLocked(state)

	// The remembered note is whatever the last live attempt said; for a
	// row whose store is gone the honest note is the remedy, not the 401.
	for _, id := range signedOut {
		// A managed row that happens to share the id was installed by the
		// panel sign-in and owns its own credential; the empty desktop store
		// behind the old id says nothing about it.
		if a := p.accountLocked(id); a != nil && a.Source != sourceManaged {
			a.State = stateInvalid
			a.LastError = noteLoginRequired
		}
	}
}

// signedOutPlaceholderLocked rebuilds a row for a discovered credential whose
// token has disappeared from the desktop client's store.  The id encodes the
// store layout (accountID joins the name, buildEnv, region and clientId), so
// the auth.json can be found again for the revive that follows a fresh
// sign-in.
//
// The directory must still look like a store -- an auth.json or the
// auth-state.json beside it -- or a config id that happens to parse the same
// way would resurrect a row that never had a file behind it.
func (p *pool) signedOutPlaceholderLocked(id string) (*Account, bool) {
	rest := strings.TrimPrefix(id, name+":")
	if rest == id || rest == "" {
		return nil, false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || parts[0] == sourceConfig || parts[0] == sourceManaged {
		return nil, false
	}
	root := strings.TrimSpace(p.cfg.authDir())
	if root == "" {
		return nil, false
	}
	dir := filepath.Join(root, parts[0], parts[1], parts[2])
	if !fileExists(filepath.Join(dir, "auth.json")) && !fileExists(filepath.Join(dir, "auth-state.json")) {
		return nil, false
	}
	authPath := filepath.Join(dir, "auth.json")
	return &Account{
		ID:       id,
		Label:    firstNonEmpty(desktopUserLabel(), id),
		Source:   sourceDiscovered,
		Origin:   authPath,
		Enabled:  true,
		State:    stateInvalid,
		AuthPath: authPath,
	}, true
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// addLocked appends an account, de-duplicating on id and on the credential
// itself.  A duplicate id from a different source loses: the first source to
// claim an id owns it, and the order is config, then discovery, then panel.
func (p *pool) addLocked(a *Account) {
	if a == nil || a.ID == "" {
		return
	}
	if p.removed[a.ID] {
		return
	}
	for _, existing := range p.accounts {
		if existing.ID == a.ID {
			return
		}
		if a.Token != "" && existing.Token == a.Token {
			return
		}
	}
	if a.State == "" {
		a.State = stateReady
	}
	p.accounts = append(p.accounts, a)
}

// applyStateLocked folds persisted runtime state back onto the accounts.
func (p *pool) applyStateLocked(state persistedState) {
	index := map[string]persistedAccount{}
	for _, row := range state.Accounts {
		index[row.ID] = row
	}
	for _, a := range p.accounts {
		if a.Source == sourceManaged {
			// saveLocked never records a managed row: the managed store owns
			// it in full.  A stale entry under the same id (from the days it
			// was a discovered row) must not be pasted onto it.
			continue
		}
		row, ok := index[a.ID]
		if !ok {
			continue
		}
		if row.State != "" {
			a.State = row.State
		}
		a.Failures = row.Failures
		a.LastError = row.LastError
		a.CooldownUntil = msToTime(row.CooldownUntil)
		a.LastUsed = msToTime(row.LastUsed)
	}
}

func (p *pool) loadStateLocked() persistedState {
	var state persistedState
	if err := core.ReadJSON(p.path(persistedFile), &state); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.logf("minimaxcode: cannot read %s: %v", p.path(persistedFile), err)
		}
		return persistedState{}
	}
	return state
}

// saveLocked writes the runtime state.  It is best-effort: losing a cooldown
// across a restart is a nuisance, not a reason to fail a request.
//
// The write happens under p.mu on purpose, so two concurrent failures cannot
// interleave snapshots and leave the older one on disk.  What is guarded against
// is the rewrite, not the lock: unless a transition actually happened there is
// nothing to record, and an unchanged pool skips the marshal and the fsync
// entirely.
func (p *pool) saveLocked() {
	if !p.dirty {
		return
	}
	state := persistedState{Version: persistedVersion}
	for _, a := range p.accounts {
		if a.Source == sourceManaged {
			continue // the managed store owns that row in full
		}
		state.Accounts = append(state.Accounts, persistedAccount{
			ID:            a.ID,
			State:         a.State,
			CooldownUntil: timeToMs(a.CooldownUntil),
			Failures:      a.Failures,
			LastError:     a.LastError,
			LastUsed:      timeToMs(a.LastUsed),
		})
	}
	state.Removed = p.removedListLocked()
	if len(state.Accounts) == 0 && len(state.Removed) == 0 && !p.wrote {
		// Nothing to remember and no file to correct.  Writing here would
		// create the directory and an empty document on every single start.
		p.dirty = false
		return
	}
	if err := core.WriteJSONAtomic(p.path(persistedFile), state); err != nil {
		// A failed write costs a cooldown, not a request: the account stays
		// parked in memory and only the restart is worse off.  The flag stays
		// set so the next transition tries again.
		p.logf("minimaxcode: cannot write %s: %v", p.path(persistedFile), err)
		return
	}
	p.wrote = true
	p.dirty = false
}

// markDirtyLocked records that the persisted state no longer matches memory.
// Callers hold p.mu.
func (p *pool) markDirtyLocked() { p.dirty = true }

func (p *pool) removedListLocked() []string {
	if len(p.removed) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.removed))
	for id := range p.removed {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (p *pool) path(name string) string {
	return filepath.Join(p.dir, name)
}

// selectableLocked reports whether an account may be chosen right now.
func (p *pool) selectableLocked(a *Account, now time.Time) bool {
	if a == nil || !a.Enabled {
		return false
	}
	switch a.State {
	case stateInvalid, stateExhausted:
		return false
	case stateCooling:
		return !now.Before(a.CooldownUntil)
	default:
		return true
	}
}

// accountLocked returns the pool's own row for an id, or nil.  The caller must
// hold p.mu.  It exists so the state transitions below can be addressed by id
// rather than by pointer: callers hold snapshots, not live pointers (see the
// README's data-race rule), and a row removed mid-request must be ignored
// rather than dereferenced.
func (p *pool) accountLocked(id string) *Account {
	if id == "" {
		return nil
	}
	for _, a := range p.accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// usable reports whether one account may be chosen right now.  It is exactly
// selectableLocked applied to one id, read under the pool lock, and that is the
// point: it is the same predicate next() uses, so a conversation binding can
// never resolve to an account the picker would refuse.  A looser notion here
// would let stickiness park a conversation on a cooling or parked credential,
// and a stricter one would drop good bindings.
//
// There is deliberately no model parameter.  minimaxcode's health is NOT
// model-scoped: noteFailure parks an account for the whole module, whatever
// model asked, so "usable for model M" and "usable" are the same question here.
// A per-model variant would be a distinction the picker does not make.
func (p *pool) usable(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	return p.selectableLocked(p.accountLocked(id), time.Now())
}

// next returns the next usable account, skipping the ones the caller has
// already tried.  It advances a cursor so a healthy pool is used evenly.
func (p *pool) next(exclude map[string]bool) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	now := time.Now()
	n := len(p.accounts)
	best, found := 0, false
	for _, a := range p.accounts {
		if exclude != nil && exclude[a.ID] {
			continue
		}
		if !p.selectableLocked(a, now) {
			continue
		}
		prio := core.AccountPriority("minimaxcode", a.ID)
		if !found || prio < best {
			best, found = prio, true
		}
	}
	if !found {
		return nil
	}
	for i := 0; i < n; i++ {
		idx := (p.cursor + i) % n
		a := p.accounts[idx]
		if exclude != nil && exclude[a.ID] {
			continue
		}
		if !p.selectableLocked(a, now) {
			continue
		}
		if core.AccountPriority("minimaxcode", a.ID) != best {
			continue
		}
		p.cursor = (idx + 1) % n
		if exclude != nil {
			exclude[a.ID] = true
		}
		return a
	}
	return nil
}

// usableCount is how many accounts could serve a request right now.
func (p *pool) usableCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	now := time.Now()
	n := 0
	for _, a := range p.accounts {
		if p.selectableLocked(a, now) {
			n++
		}
	}
	return n
}

// selectableAccounts is a read-only snapshot; it does not move the cursor.
func (p *pool) selectableAccounts() []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	now := time.Now()
	out := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		if p.selectableLocked(a, now) {
			out = append(out, a)
		}
	}
	return out
}

// find returns the account with this id, or nil.
func (p *pool) find(id string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	for _, a := range p.accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// selectableSnapshots is the copy-valued twin of selectableAccounts.  A caller
// that is about to read credential fields must use this one: those fields are
// rewritten under the pool lock by a concurrent refresh, so handing out live
// pointers would be a data race.
func (p *pool) selectableSnapshots() []Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	now := time.Now()
	out := make([]Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		if p.selectableLocked(a, now) {
			out = append(out, *a)
		}
	}
	return out
}

// refreshLock returns the mutex that serialises token refreshes for one account.
// The vendor rotates the refresh token on every exchange, so two requests that
// both decide to renew would race: the loser's write-back would put a token the
// vendor has already retired into the store, breaking the desktop client too.
// One refresher per account at a time, and the waiter re-reads the result.
func (p *pool) refreshLock(id string) *sync.Mutex {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	if p.refreshLocks == nil {
		p.refreshLocks = map[string]*sync.Mutex{}
	}
	m, ok := p.refreshLocks[id]
	if !ok {
		m = &sync.Mutex{}
		p.refreshLocks[id] = m
	}
	return m
}

// current snapshots one account's credential state under the pool lock, so a
// caller that just acquired a refresh lock can see what another refresher did.
func (p *pool) current(id string) (Account, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	for _, a := range p.accounts {
		if a.ID == id {
			return *a, true
		}
	}
	return Account{}, false
}

// mark applies a mutation to one account under the pool lock.
func (p *pool) mark(id string, apply func(*Account)) {
	if apply == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	for _, a := range p.accounts {
		if a.ID == id {
			apply(a)
			return
		}
	}
}

// noteFailure records a classified rejection, addressed by account id.  The
// state transition depends on the kind: an auth rejection parks the account
// until it is fixed by hand, a quota rejection parks it until the vendor says
// otherwise, and a rate limit only cools it.
//
// The id form is deliberate.  Callers hold snapshots (see the README's
// data-race rule), so the transition is applied to the pool's own row under the
// lock; an id the pool no longer holds is ignored, because a row removed
// mid-request must not turn a vendor error into a panic.
func (p *pool) noteFailure(id string, kind core.FailureKind, msg string) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	a := p.accountLocked(id)
	if a == nil {
		return
	}

	a.Failures++
	a.LastError = msg
	p.lastErr = msg

	switch kind {
	case core.FailureAuth:
		a.State = stateInvalid
	case core.FailureQuota:
		a.State = stateExhausted
	case core.FailureRateLimited, core.FailureWAF, core.FailureUpstream:
		a.State = stateCooling
		a.CooldownUntil = now.Add(p.cooldown())
	}
	p.markDirtyLocked()
	p.saveLocked()
}

// noteSuccess clears the penalties a previous failure applied, addressed by id
// for the same reason noteFailure is.
func (p *pool) noteSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	a := p.accountLocked(id)
	if a == nil {
		return
	}
	// Only a real transition is worth recording: an account that is already
	// ready has nothing new to persist, so the common "another good request"
	// path does not touch the disk at all.
	if a.State != stateReady || !a.CooldownUntil.IsZero() || a.LastError != "" || a.Failures != 0 {
		a.State = stateReady
		a.CooldownUntil = time.Time{}
		a.LastError = ""
		a.Failures = 0
		p.markDirtyLocked()
	}
	a.LastUsed = time.Now()
	p.saveLocked()
}

// markNeedsLogin records that the vendor has killed the authorization itself,
// so the row explains the real problem (需要重新登录) instead of the downstream
// 401 that followed from it.  The account is parked exactly like any other
// FailureAuth, because a dead refresh token is not something a retry can fix.
func (p *pool) markNeedsLogin(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	a := p.accountLocked(id)
	if a == nil {
		return
	}
	a.State = stateInvalid
	a.LastError = noteLoginRequired
	p.lastErr = noteLoginRequired
	p.markDirtyLocked()
	p.saveLocked()
}

// dropPenalty clears the runtime penalties on one row without touching its
// credential.  It is the plain core.Reviver contract, and the only thing this
// module can offer a credential it did not discover (a typed one).
func (p *pool) dropPenalty(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	a := p.accountLocked(id)
	if a == nil {
		return false
	}
	a.State = stateReady
	a.CooldownUntil = time.Time{}
	a.Failures = 0
	a.LastError = ""
	p.markDirtyLocked()
	p.saveLocked()
	return true
}

// persistManaged rewrites the panel's account store.  The renewal path needs
// it: it updates the row under mark(), and a rotated refresh token that is not
// on disk before the process ends is a credential the vendor has already
// retired.
func (p *pool) persistManaged() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	return p.saveManagedLocked()
}

// adoptCredential installs a credential read back from the desktop client's
// own store and clears the health penalty that made the row unselectable.
//
// This is how a "revive" works for minimaxcode: the module cannot mint a token
// (the vendor retires a whole authorization, not just the access token), so the
// only legitimate source of a working credential is the fresh sign-in the
// operator performed in MiniMax Code.  Adopting it is not the same as inventing
// one -- it is the operator's own fix, picked up where the vendor client left
// it.  A row that is not held (removed mid-flight) is left alone.
func (p *pool) adoptCredential(id string, cred credential) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	a := p.accountLocked(id)
	if a == nil {
		return false
	}
	a.Token = cred.Access
	a.RefreshToken = cred.Refresh
	a.ExpiresAt = cred.ExpiresAt
	a.Generation = cred.Generation
	if strings.TrimSpace(cred.RecordKey) != "" {
		a.RecordKey = cred.RecordKey
	}
	a.State = stateReady
	a.CooldownUntil = time.Time{}
	a.Failures = 0
	a.LastError = ""
	p.markDirtyLocked()
	p.saveLocked()
	return true
}

func (p *pool) cooldown() time.Duration {
	d := time.Duration(p.cfg.CooldownSeconds) * time.Second
	if d <= 0 {
		d = time.Duration(defaultCooldownSeconds) * time.Second
	}
	return d
}

// summary reports readiness and a one-line explanation for the panel.
func (p *pool) summary() (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	total := len(p.accounts)
	if total == 0 {
		return false, "no account: add one in the panel, or sign in to MiniMax Code on this machine so its credential store can be read"
	}

	now := time.Now()
	usable, cooling, exhausted, invalid := 0, 0, 0, 0
	for _, a := range p.accounts {
		switch {
		case p.selectableLocked(a, now):
			usable++
		case !a.Enabled:
			invalid++
		case a.State == stateCooling:
			cooling++
		case a.State == stateExhausted:
			exhausted++
		default:
			invalid++
		}
	}

	var b strings.Builder
	b.WriteString(itoa(usable))
	b.WriteString(" of ")
	b.WriteString(itoa(total))
	b.WriteString(" account(s) usable")
	if cooling > 0 {
		b.WriteString(", cooling=")
		b.WriteString(itoa(cooling))
	}
	if exhausted > 0 {
		b.WriteString(", exhausted=")
		b.WriteString(itoa(exhausted))
	}
	if invalid > 0 {
		b.WriteString(", invalid=")
		b.WriteString(itoa(invalid))
	}
	if p.lastErr != "" {
		b.WriteString("; last error: ")
		b.WriteString(truncate(p.lastErr, 200))
	}
	return usable > 0, b.String()
}

// accountsForStatus renders the rows the panel shows.
func (p *pool) accountsForStatus() []core.AccountStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	now := time.Now()
	out := make([]core.AccountStatus, 0, len(p.accounts))
	for _, a := range p.accounts {
		state := a.State
		if !a.Enabled {
			state = stateInvalid
		}
		note := a.LastError
		if note == "" && state == stateReady && !a.ExpiresAt.IsZero() && !a.ExpiresAt.After(now) {
			note = "the access token has expired; it will be refreshed on the next request"
		}
		out = append(out, core.AccountStatus{
			ID:        a.ID,
			Label:     a.Label,
			Enabled:   a.Enabled,
			State:     state,
			ExpiresAt: timeToRFC3339(a.ExpiresAt),
			Note:      note,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func timeToMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func timeToRFC3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
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
