package zcode

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

const (
	accountsFile = "accounts.json"
	deviceFile   = "device.json"
	stateDirMode = 0o700
)

// pool owns credential selection, rotation and the on-disk runtime state.
// Every exported method is safe for concurrent use.
type pool struct {
	dir  string
	cfg  *Config
	http *http.Client
	logf func(string, ...any)

	mu       sync.Mutex
	loaded   bool
	accounts []*Account
	deviceID string
	cursor   int
	lastErr  string

	// managed holds the operator-supplied credentials persisted in the
	// module's own store (see accounts.go).  It is loaded by ensureLocked and
	// kept in sync by the AccountManager mutations.
	managed       []managedAccount
	managedLoaded bool
	// removed is the tombstone set: IDs the operator deleted that discovery
	// would otherwise re-create on the next load.
	removed map[string]bool

	reg regionCache

	// ver caches the client version the vendor's release manifest
	// advertises, so the module does not keep claiming to be a build the
	// vendor has stopped offering promotions to.  See version.go.
	ver versionCache
}

type regionCache struct {
	mu        sync.Mutex
	region    string
	sceneID   string
	prefix    string
	enabled   bool
	known     bool
	fetchedAt time.Time
	loaded    bool
}

func newPool(dir string, cfg *Config, httpClient *http.Client, logf func(string, ...any)) *pool {
	return &pool{dir: dir, cfg: cfg, http: httpClient, logf: logf}
}

func (p *pool) httpClient() *http.Client { return p.http }

// userAgent mirrors the version the requests claim, so the UA and
// X-ZCode-App-Version never disagree.
func (p *pool) userAgent() string { return "ZCode/" + p.appVersion() }

func (p *pool) log(format string, args ...any) {
	if p.logf != nil {
		p.logf(format, args...)
	}
}

// ensureLocked performs the one-time load.  Callers must hold p.mu.
func (p *pool) ensureLocked() {
	if p.loaded {
		return
	}
	p.loaded = true

	if p.dir != "" {
		if err := core.EnsureDir(p.dir); err != nil {
			p.log("zcode: cannot create data dir %s: %v", p.dir, err)
		}
	}

	// The tombstones are loaded before anything is added: an account the
	// operator deleted must not first claim a secret and then be removed,
	// because the duplicate-secret rule would have already rejected the other
	// name that same credential is discovered under.
	p.loadRemovedLocked()

	var list []*Account
	seenID := map[string]bool{}
	seenSecret := map[string]bool{}

	add := func(a Account) {
		if a.ID == "" || seenID[a.ID] || p.removed[a.ID] {
			return
		}
		sec := a.secret()
		if sec != "" {
			if seenSecret[sec] {
				return // the desktop config stores the same key under two names
			}
			seenSecret[sec] = true
		}
		seenID[a.ID] = true
		cp := a
		if cp.State == "" {
			cp.State = stateReady
		}
		list = append(list, &cp)
	}

	for _, c := range p.cfg.Accounts {
		mode := strings.ToLower(strings.TrimSpace(c.Mode))
		switch mode {
		case modeJWT, "bearer":
			mode = modeJWT
		default:
			mode = modeAPIKey
		}
		secret := strings.TrimSpace(c.APIKey)
		if mode == modeJWT {
			secret = strings.TrimSpace(c.JWT)
			if secret == "" {
				secret = strings.TrimSpace(c.APIKey)
			}
		}
		if secret == "" {
			p.log("zcode: configured account %q has no secret; ignored", firstNonEmpty(c.Label, c.ID))
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(c.Provider))
		if provider == "" {
			provider = providerForHost(c.BaseURL)
		}
		label := firstNonEmpty(c.Label, c.ID)
		id := c.ID
		if id == "" {
			id = "config:" + provider + ":" + mode + ":" + label
		}
		acct := Account{
			ID:       id,
			Label:    label,
			Provider: provider,
			Mode:     mode,
			BaseURL:  strings.TrimSpace(c.BaseURL),
			Enabled:  true,
			Source:   "config",
		}
		if c.Enabled != nil {
			acct.Enabled = *c.Enabled
		}
		if mode == modeJWT {
			acct.jwt = secret
			acct.UserID = jwtUserID(secret)
		} else {
			acct.apiKey = secret
		}
		add(acct)
	}

	if p.cfg.autoDiscover() {
		for _, d := range discover(p.logf) {
			add(d.Account)
		}
	}

	// Operator-supplied credentials come last: an explicit entry never
	// shadows a credential the machine already provides, it only survives
	// when the machine stops providing it (see README § Panel account
	// management).
	p.loadManagedLocked()
	for _, a := range p.managedAccountsLocked() {
		add(a)
	}

	p.accounts = list
	p.mergeStateLocked()
	p.loadDeviceLocked()
}

// loadRemovedLocked reads the deleted-account tombstones into p.removed, so a
// credential the operator has deleted cannot be re-added under its old id.
func (p *pool) loadRemovedLocked() {
	if p.dir == "" {
		return
	}
	var st persistedState
	if err := core.ReadJSON(filepath.Join(p.dir, accountsFile), &st); err != nil {
		return
	}
	if len(st.Removed) == 0 {
		return
	}
	if p.removed == nil {
		p.removed = make(map[string]bool, len(st.Removed))
	}
	for _, id := range st.Removed {
		p.removed[id] = true
	}
}

// mergeStateLocked overlays persisted runtime state onto the fresh accounts.
func (p *pool) mergeStateLocked() {
	if p.dir == "" {
		return
	}
	var st persistedState
	if err := core.ReadJSON(filepath.Join(p.dir, accountsFile), &st); err != nil {
		return
	}
	p.loadRemovedLocked()
	if len(st.Removed) > 0 {
		kept := p.accounts[:0]
		for _, a := range p.accounts {
			if !p.removed[a.ID] {
				kept = append(kept, a)
			}
		}
		p.accounts = kept
	}
	byID := make(map[string]persistedAccount, len(st.Accounts))
	for _, s := range st.Accounts {
		byID[s.ID] = s
	}
	for _, a := range p.accounts {
		s, ok := byID[a.ID]
		if !ok {
			continue
		}
		if s.Label != "" {
			a.Label = s.Label
		}
		if s.Enabled != nil {
			a.Enabled = *s.Enabled
		}
		if s.State != "" {
			a.State = s.State
		}
		a.CooldownUntil = s.CooldownUntil
		a.ExpiresAt = s.ExpiresAt
		a.Note = s.Note
		a.LastError = s.LastError
	}
}

func (p *pool) loadDeviceLocked() {
	if p.cfg.Identity.DeviceMid != "" {
		p.deviceID = p.cfg.Identity.DeviceMid
		return
	}
	if p.dir != "" {
		var d struct {
			DeviceMid string `json:"device_mid"`
		}
		if err := core.ReadJSON(filepath.Join(p.dir, deviceFile), &d); err == nil && d.DeviceMid != "" {
			p.deviceID = d.DeviceMid
			return
		}
	}
	p.deviceID = randomUUID()
	p.saveDeviceLocked()
}

func (p *pool) saveDeviceLocked() {
	if p.dir == "" || p.deviceID == "" {
		return
	}
	payload := map[string]string{"device_mid": p.deviceID}
	if err := core.WriteJSONAtomic(filepath.Join(p.dir, deviceFile), payload); err != nil {
		p.log("zcode: cannot persist device id: %v", err)
	}
}

// saveLocked writes the secret-free runtime state.
func (p *pool) saveLocked() {
	if p.dir == "" {
		return
	}
	st := persistedState{
		Version:  stateVersion,
		Accounts: make([]persistedAccount, 0, len(p.accounts)),
		Removed:  p.removedListLocked(),
	}
	for _, a := range p.accounts {
		enabled := a.Enabled
		st.Accounts = append(st.Accounts, persistedAccount{
			ID:            a.ID,
			Label:         a.Label,
			Enabled:       &enabled,
			State:         a.State,
			CooldownUntil: a.CooldownUntil,
			ExpiresAt:     a.ExpiresAt,
			Note:          a.Note,
			LastError:     a.LastError,
		})
	}
	if err := core.WriteJSONAtomic(filepath.Join(p.dir, accountsFile), st); err != nil {
		p.log("zcode: cannot persist account state: %v", err)
	}
}

func (p *pool) deviceMid() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	return p.deviceID
}

// captchaReady reports whether the JWT channel can be used at all.
func (p *pool) captchaReady() bool {
	return strings.TrimSpace(p.cfg.CaptchaCommand) != ""
}

// selectableLocked applies the lifecycle state machine.
func (p *pool) selectableLocked(a *Account, now time.Time) bool {
	if !a.Enabled {
		return false
	}
	switch a.State {
	case stateInvalid, stateExhausted:
		return false
	case stateCooling:
		if now.Before(a.CooldownUntil) {
			return false
		}
	}
	if a.Mode == modeJWT && !p.captchaReady() {
		return false
	}
	return true
}

// selectableAccounts returns the accounts that could serve a request right now,
// in the order the pool would try them, as copies the caller may keep.  It is
// the read-only counterpart of next(): it never rotates the cursor and never
// mutates the pool, so an out-of-band probe (the model list, a credential
// test) can walk the same order without disturbing live traffic.
func (p *pool) selectableAccounts() []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	now := time.Now()
	if len(p.accounts) == 0 {
		return nil
	}
	out := make([]*Account, 0, len(p.accounts))
	start := p.cursor % len(p.accounts)
	for i := 0; i < len(p.accounts); i++ {
		a := p.accounts[(start+i)%len(p.accounts)]
		if !p.selectableLocked(a, now) {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	return out
}

// planAccounts returns the accounts that can serve plan billing -- the JWT
// channel -- as copies the caller may keep, in pool order.
//
// It is deliberately NOT selectableAccounts.  selectableLocked drops every JWT
// account while captcha_command is unset (see the modeJWT arm above), which is
// right for picking an account to send chat to and wrong for the task board:
// the board still has to be able to name the account and say why it cannot
// claim.  So this mirrors find(): Enabled is honoured, lifecycle state is not,
// and an operator asking about a cooling account gets a real answer.
func (p *pool) planAccounts() []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	out := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		if a.Mode != modeJWT || !a.Enabled {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	return out
}

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

// usableByID answers the same question selectableLocked answers for next(), but
// for one named account.  It exists so that the conversation-affinity predicate
// (see affinity.go) is exactly as strict as the picker: a binding that points at
// a disabled, parked or exhausted account must resolve as unusable and be
// dropped, not be served because it was found in the table.
//
// It deliberately does NOT go through find(), which ignores the lifecycle state
// because an operator asking about a cooling account is asking a real question.
// Stickiness is not an operator question; it is a selection.
func (p *pool) usableByID(id string) bool {
	if id == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	now := time.Now()
	for _, a := range p.accounts {
		if a.ID == id {
			return p.selectableLocked(a, now)
		}
	}
	return false
}

// next returns the next selectable account, rotating the cursor and recording
// the choice in exclude so one request never retries the same account.
func (p *pool) next(exclude map[string]bool) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	now := time.Now()
	n := len(p.accounts)
	best, found := 0, false
	for _, a := range p.accounts {
		if exclude[a.ID] || !p.selectableLocked(a, now) {
			continue
		}
		prio := core.AccountPriority("zcode", a.ID)
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
		if exclude[a.ID] || !p.selectableLocked(a, now) {
			continue
		}
		if core.AccountPriority("zcode", a.ID) != best {
			continue
		}
		p.cursor = (idx + 1) % n
		exclude[a.ID] = true
		return a
	}
	return nil
}

// find returns the account with this panel id, or nil.
//
// It hands back a copy for the same reason selectableAccounts does: the caller
// is an out-of-band probe (a balance read, a claim) that must not observe a
// concurrent rotation, and the copy keeps the pool's own record untouched.
// Unlike selectableAccounts it ignores the lifecycle state, because an operator
// asking about a cooling account is asking a real question.
func (p *pool) find(id string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	for _, a := range p.accounts {
		if a.ID == id {
			cp := *a
			return &cp
		}
	}
	return nil
}

// mark applies a state transition and persists it.
func (p *pool) mark(id string, apply func(*Account)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	for _, a := range p.accounts {
		if a.ID != id {
			continue
		}
		apply(a)
		p.saveLocked()
		return
	}
}

// accountsForStatus renders the pool for core.Status.
func (p *pool) accountsForStatus() []core.AccountStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	out := make([]core.AccountStatus, 0, len(p.accounts))
	now := time.Now()
	for _, a := range p.accounts {
		state := a.State
		if state == "" {
			state = stateUnknown
		}
		if state == stateCooling && !now.Before(a.CooldownUntil) {
			state = stateReady
		}
		note := a.Note
		if a.Mode == modeJWT && !p.captchaReady() && note == "" {
			note = "JWT channel needs a captcha solver (captcha_command not configured)"
		}
		if a.LastError != "" {
			if note != "" {
				note += "; "
			}
			note += "last error: " + a.LastError
		}
		expires := ""
		if !a.ExpiresAt.IsZero() {
			expires = a.ExpiresAt.Format(time.RFC3339)
		}
		out = append(out, core.AccountStatus{
			ID:        a.ID,
			Label:     firstNonEmpty(a.Label, a.ID),
			Enabled:   a.Enabled,
			State:     state,
			ExpiresAt: expires,
			Note:      note,
			Extra: map[string]any{
				"provider": a.Provider,
				"mode":     a.Mode,
				"source":   a.Source,
			},
		})
	}
	return out
}

// summary renders the one-line detail for core.Status.
func (p *pool) summary() (ready bool, detail string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	if strings.TrimSpace(p.cfg.UpstreamBase) != "" {
		return true, "upstream_base override active -> " + p.cfg.UpstreamBase + " (no credentials required)"
	}

	now := time.Now()
	var apiKeys, jwts, readyN, cooling, exhausted, invalid int
	for _, a := range p.accounts {
		switch a.Mode {
		case modeJWT:
			jwts++
		default:
			apiKeys++
		}
		switch a.State {
		case stateInvalid:
			invalid++
		case stateExhausted:
			exhausted++
		case stateCooling:
			if now.Before(a.CooldownUntil) {
				cooling++
			}
		}
		if p.selectableLocked(a, now) {
			readyN++
		}
	}

	if len(p.accounts) == 0 {
		return false, "no zcode credentials found (looked in config, ~/.zcode/v2/config.json, ~/.zcode/v2/credentials.json)"
	}

	detail = itoa(len(p.accounts)) + " account(s): " + itoa(apiKeys) + " api-key, " + itoa(jwts) + " jwt"
	detail += "; usable=" + itoa(readyN)
	if cooling > 0 {
		detail += ", cooling=" + itoa(cooling)
	}
	if exhausted > 0 {
		detail += ", exhausted=" + itoa(exhausted)
	}
	if invalid > 0 {
		detail += ", invalid=" + itoa(invalid)
	}
	if jwts > 0 && !p.captchaReady() {
		detail += "; jwt channel disabled (captcha_command not configured)"
	}
	if p.lastErr != "" {
		detail += "; last error: " + p.lastErr
	}
	return readyN > 0, detail
}

func (p *pool) setLastErr(msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.lastErr = msg
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

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

// randomUUID returns a v4-shaped identifier.  It never panics: on the (never
// observed) failure of crypto/rand it falls back to a time-derived value.
func randomUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			b[i] = byte(now >> (8 * i))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
