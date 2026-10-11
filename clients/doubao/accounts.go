package doubao

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
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

// noteLoginRequired is the LastError a row carries when the vendor has refused
// the cookie session itself.  No local retry fixes that; only a fresh sign-in
// through the web login does.
const noteLoginRequired = "login_required"

// account is one credential this module may relay with.
type account struct {
	ID      string
	Label   string
	Cookie  string
	Enabled bool
	Source  string

	// Identity is a stable, non-secret account key: the vendor's user id when
	// the account probe exposes one, otherwise a hash of the cookie jar.  It is
	// what lets a re-login replace the same account instead of piling up rows.
	Identity string

	// Runtime state, guarded by the store's mutex.
	State         string
	CooldownUntil time.Time
	Failures      int
	LastError     string
	LastUsed      time.Time
}

// selectable reports whether the account can serve a request right now.
func (a *account) selectable(now time.Time) bool {
	if a == nil || !a.Enabled {
		return false
	}
	switch a.State {
	case stateExhausted:
		return false
	case stateInvalid:
		return !a.CooldownUntil.IsZero() && !now.Before(a.CooldownUntil)
	case stateCooling:
		return !now.Before(a.CooldownUntil)
	default:
		return true
	}
}

// secret is the credential actually sent upstream: a `name=value; ...` string.
func (a *account) secret() string {
	if a == nil {
		return ""
	}
	return a.Cookie
}

// persistedAccount is the on-disk shape.  It carries the cookie (that IS the
// credential) but nothing else secret.
type persistedAccount struct {
	ID            string `json:"id"`
	Label         string `json:"label,omitempty"`
	Cookie        string `json:"cookie"`
	Enabled       bool   `json:"enabled"`
	Source        string `json:"source,omitempty"`
	Identity      string `json:"identity,omitempty"`
	State         string `json:"state,omitempty"`
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
	Failures      int    `json:"failures,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	LastUsed      int64  `json:"last_used,omitempty"`
}

// persistedState is the runtime state file.  It is separate from accounts.json
// so a credential backup does not smuggle penalties back in.
type persistedState struct {
	Version  int                `json:"version"`
	Accounts []persistedAccount `json:"accounts"`
}

const stateVersion = 1

// store owns every credential and its runtime state.
type store struct {
	mu           sync.Mutex
	accounts     []*account
	dir          string
	accountsPath string
	statePath    string
}

// loadStore reads both files.  A missing file is not an error; a corrupt one is
// reported and the store comes back empty.
func loadStore(accountsPath, statePath string) (*store, error) {
	s := &store{accountsPath: accountsPath, statePath: statePath}
	if accountsPath == "" {
		return s, nil
	}
	var acc persistedState
	if err := core.ReadJSON(accountsPath, &acc); err != nil {
		if !os.IsNotExist(err) {
			return s, fmt.Errorf("reading %s: %w", accountsPath, err)
		}
	}
	var st persistedState
	if statePath != "" {
		if err := core.ReadJSON(statePath, &st); err != nil && !os.IsNotExist(err) {
			return s, fmt.Errorf("reading %s: %w", statePath, err)
		}
	}
	stateByID := map[string]persistedAccount{}
	for _, a := range st.Accounts {
		stateByID[a.ID] = a
	}
	for _, a := range acc.Accounts {
		if strings.TrimSpace(a.Cookie) == "" {
			continue
		}
		rec := &account{
			ID:       a.ID,
			Label:    a.Label,
			Cookie:   a.Cookie,
			Enabled:  a.Enabled,
			Source:   firstNonEmpty(a.Source, "store"),
			Identity: firstNonEmpty(a.Identity, cookieIdentity(a.Cookie)),
			State:    stateReady,
		}
		if rt, ok := stateByID[rec.ID]; ok {
			rec.State = firstNonEmpty(rt.State, stateReady)
			rec.Failures = rt.Failures
			rec.LastError = rt.LastError
			if rt.CooldownUntil > 0 {
				rec.CooldownUntil = time.UnixMilli(rt.CooldownUntil)
			}
			if rt.LastUsed > 0 {
				rec.LastUsed = time.UnixMilli(rt.LastUsed)
			}
		}
		s.accounts = append(s.accounts, rec)
	}
	return s, nil
}

// save writes both files.  It must be called with the mutex held.
func (s *store) save() error {
	if s.accountsPath == "" {
		return nil
	}
	accs := make([]persistedAccount, 0, len(s.accounts))
	states := make([]persistedAccount, 0, len(s.accounts))
	for _, a := range s.accounts {
		accs = append(accs, persistedAccount{
			ID:       a.ID,
			Label:    a.Label,
			Cookie:   a.Cookie,
			Enabled:  a.Enabled,
			Source:   a.Source,
			Identity: a.Identity,
			State:    a.State,
		})
		states = append(states, persistedAccount{
			ID:            a.ID,
			State:         a.State,
			CooldownUntil: unixMilliOrZero(a.CooldownUntil),
			Failures:      a.Failures,
			LastError:     a.LastError,
			LastUsed:      unixMilliOrZero(a.LastUsed),
		})
	}
	if err := core.WriteJSONAtomic(s.accountsPath, persistedState{Version: stateVersion, Accounts: accs}); err != nil {
		return err
	}
	if s.statePath != "" {
		if err := core.WriteJSONAtomic(s.statePath, persistedState{Version: stateVersion, Accounts: states}); err != nil {
			return err
		}
	}
	return nil
}

// applyConfig folds config-file accounts into the store.  A config account with
// the same id as a stored one wins nothing: the stored credential is the
// operator's live session and must not be silently overwritten by a stale
// config entry.
func (s *store) applyConfig(entries []account) {
	if len(entries) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	known := map[string]bool{}
	for _, a := range s.accounts {
		known[a.ID] = true
	}
	added := false
	for _, e := range entries {
		if strings.TrimSpace(e.Cookie) == "" || known[e.ID] {
			continue
		}
		known[e.ID] = true
		e.State = stateReady
		if e.Identity == "" {
			e.Identity = cookieIdentity(e.Cookie)
		}
		s.accounts = append(s.accounts, &e)
		added = true
	}
	if added {
		_ = s.save()
	}
}

// clearExpiredParkLocked lets a timed verdict lapse.  Auth failures use
// stateInvalid for the panel while still carrying a cooldown, so the account
// becomes retryable after the same deadline instead of staying dead forever.
func clearExpiredParkLocked(a *account, now time.Time) {
	if a.CooldownUntil.IsZero() || now.Before(a.CooldownUntil) {
		return
	}
	if a.State == stateCooling || a.State == stateInvalid {
		a.State = stateReady
		a.LastError = ""
		a.CooldownUntil = time.Time{}
	}
}

// snapshot returns a copy of every account, sorted by id for a stable panel.
func (s *store) snapshot() []account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]account, 0, len(s.accounts))
	// Expired parks lapse lazily, so a snapshot never shows a stale verdict.
	now := time.Now()
	for _, a := range s.accounts {
		clearExpiredParkLocked(a, now)
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// pick selects the next usable account, preferring the one used longest ago.
func (s *store) pick() (*account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var best *account
	for _, a := range s.accounts {
		clearExpiredParkLocked(a, now)
		if !a.selectable(now) {
			continue
		}
		if best == nil || a.LastUsed.Before(best.LastUsed) {
			best = a
		}
	}
	if best == nil {
		return nil, core.ErrNotConfigured
	}
	cp := *best
	return &cp, nil
}

// byID returns a copy of one account, or false.
func (s *store) byID(id string) (account, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID == id {
			return *a, true
		}
	}
	return account{}, false
}

func (s *store) noteSuccess(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID != id {
			continue
		}
		a.State = stateReady
		a.Failures = 0
		a.LastError = ""
		a.LastUsed = time.Now()
	}
	_ = s.save()
}

// failureCooldown is the policy shared by every upstream call: a dead
// credential gets the long auth cooldown, a transient failure the ordinary one.
func (c *Client) failureCooldown(auth bool) time.Duration {
	if auth {
		return c.cfg.authCooldown()
	}
	return c.cfg.cooldown()
}

// noteFailure records an upstream failure.  auth marks a dead session; the rest
// park for cooldown.
func (s *store) noteFailure(id string, auth bool, cooldown time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cooldown <= 0 {
		cooldown = defaultCooldown
	}
	for _, a := range s.accounts {
		if a.ID != id {
			continue
		}
		a.Failures++
		a.LastError = redactErr(err)
		if auth {
			a.State = stateInvalid
			a.LastError = noteLoginRequired
			a.CooldownUntil = time.Now().Add(cooldown)
		} else {
			a.State = stateCooling
			a.CooldownUntil = time.Now().Add(cooldown)
		}
	}
	_ = s.save()
}

// revive clears every runtime penalty on one account.
func (s *store) revive(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID != id {
			continue
		}
		a.State = stateReady
		a.Failures = 0
		a.LastError = ""
		a.CooldownUntil = time.Time{}
		a.Enabled = true
		_ = s.save()
		return nil
	}
	return fmt.Errorf("doubao: unknown account %q", id)
}

// add stores one credential and returns a copy.
func (s *store) add(a *account) (account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(a.Cookie) == "" {
		return account{}, errors.New("doubao: cookie is empty")
	}
	if a.ID == "" {
		a.ID = "doubao-" + shortHash(a.Cookie)
	}
	if a.Identity == "" {
		a.Identity = cookieIdentity(a.Cookie)
	}
	for _, existing := range s.accounts {
		if existing.ID == a.ID {
			return account{}, fmt.Errorf("doubao: account %q already exists", a.ID)
		}
	}
	a.State = stateReady
	s.accounts = append(s.accounts, a)
	if err := s.save(); err != nil {
		return account{}, err
	}
	return *a, nil
}

// upsert replaces the credential of an existing identity, or adds a new row.
// It is what the web login uses, so re-signing in does not pile up rows.
func (s *store) upsert(a *account) (account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(a.Cookie) == "" {
		return account{}, errors.New("doubao: cookie is empty")
	}
	if a.Identity == "" {
		a.Identity = cookieIdentity(a.Cookie)
	}
	for _, existing := range s.accounts {
		if existing.Identity == a.Identity || (a.ID != "" && existing.ID == a.ID) {
			existing.Cookie = a.Cookie
			existing.Label = firstNonEmpty(a.Label, existing.Label)
			existing.State = stateReady
			existing.Failures = 0
			existing.LastError = ""
			existing.CooldownUntil = time.Time{}
			existing.Enabled = true
			if err := s.save(); err != nil {
				return account{}, err
			}
			return *existing, nil
		}
	}
	if a.ID == "" {
		a.ID = "doubao-" + shortHash(a.Cookie)
	}
	a.State = stateReady
	a.Enabled = true
	s.accounts = append(s.accounts, a)
	if err := s.save(); err != nil {
		return account{}, err
	}
	return *a, nil
}

func (s *store) remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, a := range s.accounts {
		if a.ID == id {
			s.accounts = append(s.accounts[:i], s.accounts[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("doubao: unknown account %q", id)
}

func (s *store) setEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.accounts {
		if a.ID == id {
			a.Enabled = enabled
			return s.save()
		}
	}
	return fmt.Errorf("doubao: unknown account %q", id)
}

// ---------------------------------------------------------------------------
// AccountManager
// ---------------------------------------------------------------------------

// accountFields is the panel's add form.  The cookie is the credential; the
// label is just the operator's name for it.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{
			Key:         "cookie",
			Label:       "会话 Cookie",
			Type:        "textarea",
			Required:    true,
			Placeholder: "sessionid=...; ttwid=...",
			Help:        "从已登录 www.doubao.com 的浏览器复制 Cookie 请求头。优先用网页登录自动读取。",
		},
		{
			Key:   "label",
			Label: "备注",
			Type:  "text",
			Help:  "可选：给这个账号起个名字，方便在账号列表里认出来。",
		},
	}
}

// Accounts lists what the module holds.  It never fails just because the pool
// is empty.
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	now := time.Now()
	snap := c.store.snapshot()
	out := make([]core.AccountRecord, 0, len(snap))
	for i := range snap {
		out = append(out, accountRecord(&snap[i], now))
	}
	return out, nil
}

// AddAccount stores one credential.  The cookie is validated by the caller's
// TestAccount; here it is only shape-checked.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	cookie := strings.TrimSpace(spec.Field("cookie"))
	if cookie == "" {
		return core.AccountRecord{}, errors.New("doubao: the cookie field is required")
	}
	label := firstNonEmpty(spec.Field("label"), spec.Label)
	acc := &account{
		ID:      strings.TrimSpace(spec.ID),
		Label:   label,
		Cookie:  cookie,
		Enabled: spec.EnabledOr(true),
		Source:  "manual",
	}
	rec, err := c.store.add(acc)
	if err != nil {
		return core.AccountRecord{}, err
	}
	return accountRecord(&rec, time.Now()), nil
}

// RemoveAccount deletes one credential by id.
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	return c.store.remove(id)
}

// SetAccountEnabled toggles one credential without deleting it.
func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	return c.store.setEnabled(id, enabled)
}

// TestAccount probes one credential against the live upstream.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	acc, ok := c.store.byID(id)
	if !ok {
		return core.TestResult{}, fmt.Errorf("doubao: unknown account %q", id)
	}
	start := time.Now()
	info, err := c.up.accountInfo(ctx, &acc)
	res := core.TestResult{AccountID: id, Model: c.testModel()}
	if err != nil {
		res.OK = false
		res.Error = redactErr(err)
		res.ElapsedMS = time.Since(start).Milliseconds()
		c.noteFailure(err)
		c.store.noteFailure(id, isAuthError(err), c.failureCooldown(isAuthError(err)), err)
		return res, nil
	}
	res.OK = true
	res.Reply = info.displayName()
	res.ElapsedMS = time.Since(start).Milliseconds()
	c.store.noteSuccess(id)
	return res, nil
}

// RefreshAccount renews one credential, or every credential when id is empty.
// This module cannot mint a cookie, so "refresh" means "re-validate"; a dead
// session is reported for the operator to re-login.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	var ids []string
	if strings.TrimSpace(id) != "" {
		ids = []string{id}
	} else {
		for _, a := range c.store.snapshot() {
			ids = append(ids, a.ID)
		}
	}
	out := make([]core.RefreshResult, 0, len(ids))
	for _, aid := range ids {
		res, err := c.TestAccount(ctx, aid)
		if err != nil {
			out = append(out, core.RefreshResult{AccountID: aid, OK: false, Error: redactErr(err)})
			continue
		}
		out = append(out, core.RefreshResult{AccountID: aid, OK: res.OK, Error: res.Error})
	}
	return out, nil
}

// accountRecord renders one account for the panel.  The cookie is never copied
// into a record.
func accountRecord(a *account, now time.Time) core.AccountRecord {
	state := a.State
	if state == "" {
		state = stateReady
	}
	if state == stateCooling && !now.Before(a.CooldownUntil) {
		state = stateReady
	}
	rec := core.AccountRecord{
		ID:       a.ID,
		Label:    a.Label,
		Enabled:  a.Enabled,
		State:    state,
		Identity: a.Identity,
		Fields: map[string]any{
			"kind": "cookie",
		},
	}
	if (state == stateCooling || state == stateInvalid) && !a.CooldownUntil.IsZero() {
		rec.ExpiresAt = a.CooldownUntil.UTC().Format(time.RFC3339)
	}
	if a.LastError != "" {
		rec.Note = core.Redact(a.LastError)
	}
	return rec
}

// accountStatus renders one account for Status.
func accountStatus(a *account, now time.Time) core.AccountStatus {
	state := a.State
	if state == "" || (state == stateCooling && !now.Before(a.CooldownUntil)) {
		state = stateReady
	}
	st := core.AccountStatus{
		ID:       a.ID,
		Label:    a.Label,
		Enabled:  a.Enabled,
		State:    state,
		Identity: a.Identity,
	}
	if (state == stateCooling || state == stateInvalid) && !a.CooldownUntil.IsZero() {
		st.ExpiresAt = a.CooldownUntil.UTC().Format(time.RFC3339)
	}
	if a.LastError != "" {
		st.Note = core.Redact(a.LastError)
	}
	return st
}

// configuredAccounts flattens the config credential sources into one list.
func configuredAccounts(cfg config) []account {
	var out []account
	seen := map[string]bool{}
	add := func(a account) {
		if strings.TrimSpace(a.Cookie) == "" {
			return
		}
		if a.ID == "" {
			a.ID = "doubao-" + shortHash(a.Cookie)
		}
		if seen[a.ID] {
			return
		}
		seen[a.ID] = true
		a.Source = "config"
		a.Identity = cookieIdentity(a.Cookie)
		out = append(out, a)
	}
	for _, e := range cfg.Accounts {
		enabled := e.Enabled == nil || *e.Enabled
		add(account{ID: strings.TrimSpace(e.ID), Label: strings.TrimSpace(e.Label), Cookie: strings.TrimSpace(e.Cookie), Enabled: enabled})
	}
	add(account{Label: strings.TrimSpace(cfg.Label), Cookie: strings.TrimSpace(cfg.Cookie), Enabled: true})
	add(account{Cookie: strings.TrimSpace(os.Getenv("CLIENT2API_DOUBAO_COOKIE")), Enabled: true})
	return out
}

// ---------------------------------------------------------------------------
// Cookie helpers
// ---------------------------------------------------------------------------

// cookieIdentity is a stable, non-secret fingerprint of a cookie jar.  It is
// used to recognise a re-login of the same session without storing anything
// that reveals the credential.
func cookieIdentity(cookie string) string {
	sum := sha256.Sum256([]byte(canonicalCookie(cookie)))
	return "db-" + hex.EncodeToString(sum[:6])
}

// shortHash is the id suffix for a cookie with no explicit id.
func shortHash(cookie string) string {
	sum := sha256.Sum256([]byte(canonicalCookie(cookie)))
	return hex.EncodeToString(sum[:5])
}

// canonicalCookie normalises a cookie header so the same jar written in a
// different order fingerprints the same.
func canonicalCookie(raw string) string {
	parts := splitCookiePairs(raw)
	sort.Slice(parts, func(i, j int) bool { return parts[i][0] < parts[j][0] })
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p[0])
		b.WriteString("=")
		b.WriteString(p[1])
		b.WriteString(";")
	}
	return b.String()
}

// splitCookiePairs parses a `name=value; name=value` header, dropping blanks.
func splitCookiePairs(raw string) [][2]string {
	fields := strings.Split(raw, ";")
	out := make([][2]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		name, value, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		out = append(out, [2]string{name, strings.TrimSpace(value)})
	}
	return out
}

// cookieValue reads one named cookie out of a header string.
func cookieValue(raw, name string) string {
	for _, p := range splitCookiePairs(raw) {
		if strings.EqualFold(p[0], name) {
			return p[1]
		}
	}
	return ""
}

// hasSessionCookie reports whether the jar carries the primary Doubao session
// cookie.  A jar without it cannot authenticate.
func hasSessionCookie(raw string) bool {
	return cookieValue(raw, "sessionid") != "" || cookieValue(raw, "sessionid_ss") != ""
}

// cookieHeaderFromValues renders a cookie set as a request header.
func cookieHeaderFromValues(pairs [][2]string) string {
	var b strings.Builder
	for _, p := range pairs {
		if p[0] == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		b.WriteString(p[0])
		b.WriteString("=")
		b.WriteString(p[1])
	}
	return b.String()
}

func unixMilliOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}
