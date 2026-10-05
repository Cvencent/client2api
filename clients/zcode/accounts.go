package zcode

// Panel account management.
//
// This file implements the two optional core capabilities the zcode module can
// honestly support:
//
//   - core.AccountManager: list / add / remove / enable / test / refresh the
//     credentials the module may use.
//   - core.CredentialImporter: report the credential stores found on this
//     machine and import selected ones into the module's own store.
//
// core.LoginProvider is implemented by weblogin.go: the panel can start the
// vendor's OAuth flow and store the API key or JWT it returns.  The JWT
// channel's per-request captcha is a separate concern, handled by the built-in
// browser solver or captcha_command; see README § Panel account management.
//
// Operator-supplied credentials are persisted in the module's own data dir, in
// managed_accounts.json, never in the gateway's config file.  That file is the
// only place in the module that holds a secret at rest other than the desktop
// client's own stores.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// The module advertises exactly these capabilities.
var (
	_ core.AccountManager     = (*Client)(nil)
	_ core.CredentialImporter = (*Client)(nil)
	_ core.LoginProvider      = (*Client)(nil)
	_ core.Reviver            = (*Client)(nil)
)

const (
	// managedFile lives in core.Deps.DataDir and holds operator-supplied
	// credentials.  accounts.json is already taken by the secret-free runtime
	// state, so the two never collide.
	managedFile    = "managed_accounts.json"
	managedVersion = 1

	managedIDPrefix = "zcode-managed:"
	// managedSource is the Account.Source value for credentials that came out
	// of managedFile.  Everything else (config, desktop discovery) is
	// "external": deleting such an account has to leave a tombstone.
	managedSource = managedFile

	kindAPIKey = "api-key"
	kindJWT    = "jwt"

	fieldKind    = "kind"
	fieldAPIKey  = "api_key"
	fieldJWT     = "jwt"
	fieldLabel   = "label"
	fieldRegion  = "region"
	fieldBaseURL = "base_url"

	regionZai      = "zai"
	regionBigmodel = "bigmodel"

	maxAPIKeyBytes = 512
	maxJWTBytes    = 8192
	maxLabelRunes  = 120

	probeReplyLimit = 400
	probeEventLimit = 5000
)

// ---------------------------------------------------------------------------
// Persisted operator accounts
// ---------------------------------------------------------------------------

// managedAccount is one credential the operator added or imported.  It is the
// only structure in the module that stores a secret on purpose; it is written
// 0600 through core.WriteJSONAtomic and never leaves the package.
type managedAccount struct {
	ID      string `json:"id"`
	Label   string `json:"label,omitempty"`
	Kind    string `json:"kind"`
	APIKey  string `json:"api_key,omitempty"`
	JWT     string `json:"jwt,omitempty"`
	Region  string `json:"region,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
	// UserID is the Zhipu account the credential belongs to, recorded at the
	// moment the sign-in flow had a token in hand.  An API key cannot be asked
	// who it belongs to afterwards, so this is the only chance to learn it; an
	// entry stored before this field existed simply has none, and the panel
	// keeps it as its own account rather than guessing.
	UserID string `json:"user_id,omitempty"`
	// Origin is empty for an entry the operator typed in and "panel-login"
	// for one the Z.AI sign-in flow created.  It is non-secret provenance.
	Origin string `json:"origin,omitempty"`

	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type managedStore struct {
	Version  int              `json:"version"`
	Accounts []managedAccount `json:"accounts"`
}

// account projects a stored credential onto the live pool representation.
func (m managedAccount) account() Account {
	mode := modeFromKind(m.Kind)
	a := Account{
		ID:       m.ID,
		Label:    m.Label,
		Provider: providerForRegion(m.Region),
		Mode:     mode,
		BaseURL:  strings.TrimSpace(m.BaseURL),
		Enabled:  true,
		Source:   managedSource,
		Origin:   m.Origin,
		State:    stateReady,
	}
	if m.Enabled != nil {
		a.Enabled = *m.Enabled
	}
	if mode == modeJWT {
		a.jwt = m.JWT
		a.UserID = firstNonEmpty(jwtUserID(m.JWT), m.UserID)
	} else {
		a.apiKey = m.APIKey
		a.UserID = m.UserID
	}
	return a
}

func secretOfManaged(m managedAccount) string {
	if modeFromKind(m.Kind) == modeJWT {
		return m.JWT
	}
	return m.APIKey
}

// ---------------------------------------------------------------------------
// Pool integration
// ---------------------------------------------------------------------------

func (p *pool) loadManagedLocked() {
	if p.managedLoaded {
		return
	}
	p.managedLoaded = true
	if p.dir == "" {
		return
	}
	var st managedStore
	err := core.ReadJSON(filepath.Join(p.dir, managedFile), &st)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.log("zcode: cannot read %s: %v", managedFile, err)
		}
		return
	}
	p.managed = st.Accounts
}

func (p *pool) managedAccountsLocked() []Account {
	out := make([]Account, 0, len(p.managed))
	for i := range p.managed {
		out = append(out, p.managed[i].account())
	}
	return out
}

func (p *pool) saveManagedLocked() {
	if p.dir == "" {
		return
	}
	st := managedStore{Version: managedVersion, Accounts: p.managed}
	if st.Accounts == nil {
		st.Accounts = []managedAccount{}
	}
	if err := core.WriteJSONAtomic(filepath.Join(p.dir, managedFile), st); err != nil {
		p.log("zcode: cannot persist %s: %v", managedFile, err)
	}
}

// updateManagedLocked replaces the stored entry with this id, reporting whether
// there was one.
func (p *pool) updateManagedLocked(id string, apply func(*managedAccount)) bool {
	for i := range p.managed {
		if p.managed[i].ID != id {
			continue
		}
		apply(&p.managed[i])
		return true
	}
	return false
}

func (p *pool) dropManagedLocked(id string) bool {
	for i := range p.managed {
		if p.managed[i].ID != id {
			continue
		}
		p.managed = append(p.managed[:i], p.managed[i+1:]...)
		return true
	}
	return false
}

func (p *pool) removedListLocked() []string {
	if len(p.removed) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.removed))
	for id := range p.removed {
		out = append(out, id)
	}
	sort.Strings(out) // stable file contents
	return out
}

func (p *pool) hasLocked(id string) bool {
	for _, a := range p.accounts {
		if a.ID == id {
			return true
		}
	}
	return false
}

func (p *pool) hasAccount(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	return p.hasLocked(id)
}

// freshManagedID allocates an unused ID for a hand-added credential.
func (p *pool) freshManagedID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	for i := 0; i < 16; i++ {
		id := managedIDPrefix + shortID()
		if !p.hasLocked(id) {
			return id
		}
	}
	return managedIDPrefix + shortID()
}

func shortID() string {
	u := strings.ReplaceAll(randomUUID(), "-", "")
	if len(u) > 12 {
		u = u[:12]
	}
	return u
}

// errAccountExists reports that a credential is already live in the pool.
// AddAccount surfaces it; Import treats it as a no-op.
type errAccountExists struct{ msg string }

func (e *errAccountExists) Error() string { return e.msg }

func errExists(format string, args ...any) error {
	return &errAccountExists{msg: fmt.Sprintf(format, args...)}
}

// addManaged persists a credential and puts it in the live pool.
func (p *pool) addManaged(m managedAccount) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	a := m.account()
	if a.secret() == "" {
		return fmt.Errorf("account %q carries no credential", m.ID)
	}
	if p.hasLocked(m.ID) {
		return errExists("account %q already exists", m.ID)
	}
	for _, existing := range p.accounts {
		if existing.secret() == a.secret() {
			return errExists(
				"this credential is already configured as account %q; delete it first or add a different one",
				existing.ID)
		}
	}
	// A credential that was deleted before is being deliberately re-added, so
	// the tombstone goes away.
	delete(p.removed, m.ID)

	cp := a
	p.accounts = append(p.accounts, &cp)
	if !p.updateManagedLocked(m.ID, func(existing *managedAccount) { *existing = m }) {
		p.managed = append(p.managed, m)
	}
	p.saveManagedLocked()
	p.saveLocked()
	return nil
}

func (p *pool) removeAccount(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	idx := -1
	for i, a := range p.accounts {
		if a.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("no account with id %q", id)
	}
	external := p.accounts[idx].Source != managedSource
	p.accounts = append(p.accounts[:idx], p.accounts[idx+1:]...)

	if external {
		// The credential still exists in the desktop client's own store, so
		// the deletion has to be remembered; otherwise discovery would bring
		// it straight back on the next load.
		if p.removed == nil {
			p.removed = map[string]bool{}
		}
		p.removed[id] = true
	} else if p.dropManagedLocked(id) {
		p.saveManagedLocked()
	}
	p.saveLocked()
	return nil
}

func (p *pool) setEnabled(id string, enabled bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	for _, a := range p.accounts {
		if a.ID != id {
			continue
		}
		a.Enabled = enabled
		if p.updateManagedLocked(id, func(m *managedAccount) {
			v := enabled
			m.Enabled = &v
			m.UpdatedAt = nowRFC3339()
		}) {
			p.saveManagedLocked()
		}
		p.saveLocked()
		return nil
	}
	return fmt.Errorf("no account with id %q", id)
}

// revive clears every runtime penalty on one account and switches it back on.
// It is the operator's override, so it reports whether the id existed at all.
//
// The account's own Enabled/State/CooldownUntil/Note are the whole penalty
// surface here: this module keeps no failure or breaker counters, and the
// credential itself (which lives in the managed store) is never rewritten.
func (p *pool) revive(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	for _, a := range p.accounts {
		if a.ID != id {
			continue
		}
		a.Enabled = true
		a.State = stateReady
		a.CooldownUntil = time.Time{}
		a.Note = ""
		a.LastError = ""
		if p.updateManagedLocked(id, func(m *managedAccount) {
			v := true
			m.Enabled = &v
			m.UpdatedAt = nowRFC3339()
		}) {
			p.saveManagedLocked()
		}
		p.saveLocked()
		return true
	}
	return false
}

// replaceSecret installs a jwt the desktop client has renewed.
func (p *pool) replaceSecret(id, secret, userID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	for _, a := range p.accounts {
		if a.ID != id || a.Mode != modeJWT {
			continue
		}
		a.jwt = secret
		a.UserID = userID
		if exp, ok := jwtExpiry(secret); ok {
			a.ExpiresAt = exp
		}
		a.State = stateReady
		a.LastError = ""
		a.CooldownUntil = time.Time{}
		if p.updateManagedLocked(id, func(m *managedAccount) {
			m.JWT = secret
			m.UpdatedAt = nowRFC3339()
		}) {
			p.saveManagedLocked()
		}
		p.saveLocked()
		return
	}
}

// accountByID returns a copy, so callers can hold it while the pool mutates.
func (p *pool) accountByID(id string) *Account {
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

func (p *pool) snapshotAccounts() []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	out := make([]*Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		cp := *a
		out = append(out, &cp)
	}
	return out
}

func (p *pool) recordByID(id string) (core.AccountRecord, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	now := time.Now()
	for _, a := range p.accounts {
		if a.ID == id {
			return recordFor(a, now, p.captchaReady()), true
		}
	}
	return core.AccountRecord{}, false
}

// records renders every account for the panel.  It never fails: an empty pool
// is an empty list, not an error.
func (p *pool) records() []core.AccountRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	now := time.Now()
	ready := p.captchaReady()
	out := make([]core.AccountRecord, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, recordFor(a, now, ready))
	}
	return out
}

// recordFor projects one account onto the panel's record shape.  Every value
// is either non-secret or derived irreversibly from the secret; the credential
// itself is never included, not even a prefix.
func recordFor(a *Account, now time.Time, captchaReady bool) core.AccountRecord {
	state := a.State
	if state == "" {
		state = stateUnknown
	}
	if state == stateCooling && !now.Before(a.CooldownUntil) {
		state = stateReady
	}
	note := a.Note
	if a.Mode == modeJWT && !captchaReady && note == "" {
		note = "JWT channel needs a captcha solver (no captcha_command and no Edge/Chrome found)"
	}
	if a.LastError != "" {
		if note != "" {
			note += "; "
		}
		note += a.LastError
	}
	expires := ""
	if exp, ok := jwtExpiry(a.secret()); ok {
		expires = exp.Format(time.RFC3339)
	} else if !a.ExpiresAt.IsZero() {
		expires = a.ExpiresAt.Format(time.RFC3339)
	}
	return core.AccountRecord{
		ID:        a.ID,
		Label:     firstNonEmpty(a.Label, a.ID),
		Enabled:   a.Enabled,
		State:     state,
		ExpiresAt: expires,
		Note:      note,
		// UserID is the Zhipu account this credential authenticates as.  It is
		// what lets the panel show the plan JWT and the coding-plan API key as
		// two channels of one account instead of two accounts.  It is empty for
		// a credential the vendor issued without ever naming its account, and
		// then the panel keeps that credential as its own account.
		Identity: a.UserID,
		Fields: map[string]any{
			"kind":     kindNameForMode(a.Mode),
			"provider": a.Provider,
			"source":   a.Source,
			"origin":   originOf(a),
			"managed":  a.Source == managedSource,
			// fingerprint lets an operator tell two credentials for the same
			// provider apart without revealing any part of either.
			"fingerprint": credentialTag(a.secret()),
		},
	}
}

// originOf labels where a credential came from, for the panel's account table.
// A panel-login account says so; anything else falls back to its source, and
// a config-supplied account has no source at all.
func originOf(a *Account) string {
	if a.Origin != "" {
		return a.Origin
	}
	if a.Source == managedSource {
		return originStored
	}
	if a.Source != "" {
		return a.Source
	}
	return originConfig
}

// ---------------------------------------------------------------------------
// Credential inventory
// ---------------------------------------------------------------------------

// credentialInventory is discover() plus the locations that were checked and
// found to hold nothing this module can read.  The second kind is reported so
// the panel can show the operator that the location was actually inspected.
func credentialInventory(logf func(string, ...any)) []credentialSource {
	out := discover(logf)
	out = append(out, desktopProfileSources()...)
	return out
}

// desktopProfileSources reports the desktop client's Electron profile.  It is
// not importable: the profile keeps whatever it has in Chromium leveldb and
// cache stores, which this module deliberately does not parse.
func desktopProfileSources() []credentialSource {
	appdata := strings.TrimSpace(os.Getenv("APPDATA"))
	if appdata == "" {
		return nil
	}
	dir := filepath.Join(appdata, "ZCode")
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}
	return []credentialSource{{
		Account: Account{Label: "ZCode desktop profile", Source: dir},
		Path:    dir,
		Kind:    "electron-profile",
		Note: "the desktop client's Electron profile (session, Local Storage, caches); " +
			"this module does not parse Chromium leveldb stores, so nothing here is importable",
		Importable: false,
	}}
}

func pathMatches(want, sourcePath string) bool {
	if want == sourcePath {
		return true
	}
	file := sourcePath
	if i := strings.Index(file, "#"); i >= 0 {
		file = file[:i]
	}
	return filepath.Clean(want) == filepath.Clean(file)
}

func managedAccountFromSource(s credentialSource) managedAccount {
	m := managedAccount{
		ID:        s.ID,
		Label:     s.Label,
		Kind:      kindNameForMode(s.Mode),
		Region:    regionForProvider(s.Provider),
		BaseURL:   strings.TrimSpace(s.BaseURL),
		CreatedAt: nowRFC3339(),
		UpdatedAt: nowRFC3339(),
	}
	enabled := s.Enabled
	m.Enabled = &enabled
	if s.Mode == modeJWT {
		m.JWT = s.jwt
	} else {
		m.APIKey = s.apiKey
	}
	return m
}

// managedIndex answers "is this credential already in the module's store?"
// without ever handing a secret to the caller.
type managedIndex struct {
	ids     map[string]bool
	secrets map[string]bool
}

func (p *pool) managedIndex() managedIndex {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	idx := managedIndex{ids: map[string]bool{}, secrets: map[string]bool{}}
	for i := range p.managed {
		m := p.managed[i]
		idx.ids[m.ID] = true
		if s := secretOfManaged(m); s != "" {
			idx.secrets[s] = true
		}
	}
	return idx
}

func (idx managedIndex) has(id, secret string) bool {
	if idx.ids[id] {
		return true
	}
	return secret != "" && idx.secrets[secret]
}

// ---------------------------------------------------------------------------
// core.AccountManager
// ---------------------------------------------------------------------------

// AccountFields implements core.AccountManager.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	_ = ctx
	return []core.FieldSpec{
		{
			Key: fieldKind, Label: "Credential kind", Type: "select", Required: true,
			Options: []string{kindAPIKey, kindJWT}, Default: kindAPIKey,
			Help: "api-key sends the vendor key header; jwt is a ZCode plan token and uses the built-in browser captcha (captcha_command is the fallback).",
		},
		{
			Key: fieldAPIKey, Label: "API key", Type: "password",
			Placeholder: "xxxxxxxx.xxxxxxxx",
			Help:        "Required when kind=api-key. Stored only in this module's data dir; never read back by the panel.",
		},
		{
			Key: fieldJWT, Label: "Plan JWT", Type: "textarea",
			Help: "Required when kind=jwt. Ignored when kind=api-key.",
		},
		{
			Key: fieldLabel, Label: "Label", Type: "text",
			Placeholder: "z.ai personal",
			Help:        "Optional. Shown in the account table; defaults to \"<region> <kind>\".",
		},
		{
			Key: fieldRegion, Label: "Region", Type: "select",
			Options: []string{regionZai, regionBigmodel}, Default: regionZai,
			Help: "Endpoint family: zai uses api.z.ai / zcode.z.ai, bigmodel uses open.bigmodel.cn.",
		},
		{
			Key: fieldBaseURL, Label: "Base URL override", Type: "text",
			Placeholder: "https://api.z.ai/api/anthropic",
			Help:        "Optional. Overrides the endpoint family for this one account.",
		},
	}
}

// Accounts implements core.AccountManager.
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	_ = ctx
	return c.pool.records(), nil
}

// AddAccount implements core.AccountManager.  The credential is validated,
// written to the module's own store and added to the live pool; it is never
// written to the gateway's config file.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	_ = ctx
	m, err := c.buildManagedAccount(spec)
	if err != nil {
		return core.AccountRecord{}, err
	}
	if err := c.pool.addManaged(m); err != nil {
		return core.AccountRecord{}, err
	}
	rec, ok := c.pool.recordByID(m.ID)
	if !ok {
		return core.AccountRecord{}, fmt.Errorf("account %q was persisted but is not selectable", m.ID)
	}
	return rec, nil
}

func (c *Client) buildManagedAccount(spec core.AccountSpec) (managedAccount, error) {
	kind, ok := parseKind(spec.FieldOr(fieldKind, kindAPIKey))
	if !ok {
		return managedAccount{}, fmt.Errorf("field %q: unknown credential kind %q (want %q or %q)",
			fieldKind, spec.Field(fieldKind), kindAPIKey, kindJWT)
	}
	region, ok := parseRegion(spec.FieldOr(fieldRegion, regionZai))
	if !ok {
		return managedAccount{}, fmt.Errorf("field %q: unknown region %q (want %q or %q)",
			fieldRegion, spec.Field(fieldRegion), regionZai, regionBigmodel)
	}

	// The panel's own form submits every capability field, so the label arrives
	// in Fields; a caller driving the panel API directly sets AccountSpec.Label.
	// Accept both, the way the other modules do.
	m := managedAccount{
		Kind:   kind,
		Region: region,
		Label:  sanitizeLabel(firstNonEmpty(spec.Field(fieldLabel), spec.Label)),
	}

	switch kind {
	case kindJWT:
		token := spec.Field(fieldJWT)
		if token == "" {
			return managedAccount{}, fmt.Errorf("field %q is required when %s=%q", fieldJWT, fieldKind, kindJWT)
		}
		if err := validateSecret(token, maxJWTBytes); err != nil {
			return managedAccount{}, fmt.Errorf("field %q %s", fieldJWT, err)
		}
		m.JWT = token
	default:
		key := spec.Field(fieldAPIKey)
		if key == "" {
			return managedAccount{}, fmt.Errorf("field %q is required when %s=%q", fieldAPIKey, fieldKind, kindAPIKey)
		}
		if looksLikeJWT(key) {
			return managedAccount{}, fmt.Errorf(
				"field %q looks like a plan JWT, not an API key; resubmit with %s=%q",
				fieldAPIKey, fieldKind, kindJWT)
		}
		if err := validateSecret(key, maxAPIKeyBytes); err != nil {
			return managedAccount{}, fmt.Errorf("field %q %s", fieldAPIKey, err)
		}
		m.APIKey = key
		m.UserID = userIDFromAPIKey(key)
	}

	if base := spec.Field(fieldBaseURL); base != "" {
		if err := validateBaseURL(base); err != nil {
			return managedAccount{}, fmt.Errorf("field %q %s", fieldBaseURL, err)
		}
		m.BaseURL = base
	}
	if m.Label == "" {
		m.Label = region + " " + kind
	}

	id := strings.TrimSpace(spec.ID)
	if id != "" {
		if !validAccountID(id) {
			return managedAccount{}, fmt.Errorf("account id %q contains unsupported characters", id)
		}
		if c.pool.hasAccount(id) {
			return managedAccount{}, errExists("account %q already exists", id)
		}
	} else {
		id = c.pool.freshManagedID()
	}
	m.ID = id

	now := nowRFC3339()
	m.CreatedAt = now
	m.UpdatedAt = now
	enabled := spec.EnabledOr(true)
	m.Enabled = &enabled
	return m, nil
}

// RemoveAccount implements core.AccountManager.  A credential that came from
// the machine rather than from this module cannot be erased from its real
// store, so the deletion is recorded as a tombstone instead.
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	_ = ctx
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("account id is required")
	}
	return c.pool.removeAccount(id)
}

// SetAccountEnabled implements core.AccountManager.
func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	_ = ctx
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("account id is required")
	}
	return c.pool.setEnabled(id, enabled)
}

// ReviveAccount implements core.Reviver.  It is the operator's override: it
// clears every runtime verdict on the account and switches it back on.  It
// deliberately never touches the credential itself -- a revived account whose
// key is genuinely dead must fail again on its next real call, which is the
// evidence the pool wants.
func (c *Client) ReviveAccount(ctx context.Context, id string) error {
	_ = ctx
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("account id is required")
	}
	if !c.pool.revive(id) {
		return fmt.Errorf("no account with id %q", id)
	}
	return nil
}

// TestAccount implements core.AccountManager.  It sends one minimal chat to the
// first configured model and reports what came back.
//
// An upstream refusal (expired key, quota, captcha, risk control) is a *result*
// about the credential, not a failure of the call: it is returned as
// TestResult{OK:false, Error:...} with a nil error.  Only an unknown account id
// is a real error.  Note that a probe which the upstream rejects as invalid
// moves the account into the invalid state, exactly as a real request would.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return core.TestResult{}, errors.New("account id is required")
	}
	acct := c.pool.accountByID(id)
	if acct == nil {
		return core.TestResult{}, fmt.Errorf("no account with id %q", id)
	}
	model := c.testModel()
	if model == "" {
		return core.TestResult{}, errors.New("no model is configured to test with")
	}

	res := core.TestResult{AccountID: id, Model: model}
	if acct.Mode == modeJWT && !c.pool.captchaReady() {
		res.Error = "the jwt channel is disabled: set captcha_command or install Edge/Chrome for the built-in captcha solver"
		return res, nil
	}

	start := time.Now()
	stream, err := c.attempt(ctx, probeRequest(model), acct, model)
	if err != nil {
		res.ElapsedMS = time.Since(start).Milliseconds()
		res.Error = testFailureText(err, acct.secret())
		return res, nil
	}
	reply, err := drainReply(stream)
	res.ElapsedMS = time.Since(start).Milliseconds()
	if err != nil {
		res.Error = testFailureText(err, acct.secret())
		return res, nil
	}

	res.OK = true
	res.Reply = truncate(strings.TrimSpace(reply), probeReplyLimit)
	c.pool.mark(id, func(a *Account) {
		a.State = stateReady
		a.LastError = ""
		a.CooldownUntil = time.Time{}
	})
	return res, nil
}

func (c *Client) testModel() string {
	for _, id := range c.cfg.modelIDs() {
		if id = strings.TrimSpace(id); id != "" {
			return id
		}
	}
	return ""
}

func probeRequest(model string) *core.ChatRequest {
	maxTokens := 16
	return &core.ChatRequest{
		Model:     model,
		Messages:  []core.Message{{Role: "user", Content: "Reply with the single word: ok"}},
		MaxTokens: &maxTokens,
	}
}

func drainReply(s core.Stream) (string, error) {
	if s == nil {
		return "", errors.New("the upstream returned no stream")
	}
	defer func() { _ = s.Close() }()

	var b strings.Builder
	for i := 0; i < probeEventLimit; i++ {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return b.String(), nil
		}
		if err != nil {
			return b.String(), err
		}
		switch ev.Type {
		case core.EventDelta:
			b.WriteString(ev.Delta)
			if b.Len() >= probeReplyLimit {
				return b.String(), nil
			}
		case core.EventError:
			if ev.Err != nil {
				return b.String(), ev.Err
			}
		case core.EventDone:
			return b.String(), nil
		}
	}
	return b.String(), nil
}

// testFailureText renders an upstream failure for the panel.  The credential is
// masked again on the way out, unconditionally: redact() leaves a secret shorter
// than eight bytes alone, and "never leak a credential" has to hold for those
// too.
func testFailureText(err error, secret string) string {
	var text string
	var ue *upstreamError
	if errors.As(err, &ue) {
		text = ue.short()
	} else {
		text = truncate(strings.TrimSpace(err.Error()), 300)
	}
	return maskCredential(text, secret)
}

// maskCredential replaces the credential wherever it appears.  core.MaskSecret
// reveals no character at all for a short secret, which is the safe default.
func maskCredential(text, secret string) string {
	if secret == "" {
		return text
	}
	return strings.ReplaceAll(text, secret, core.MaskSecret(secret))
}

// RefreshAccount implements core.AccountManager.  With an empty id it refreshes
// every account.  It never fails for a credential that simply cannot be
// renewed: that is reported per account.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	_ = ctx
	id = strings.TrimSpace(id)
	if id != "" {
		acct := c.pool.accountByID(id)
		if acct == nil {
			return nil, fmt.Errorf("no account with id %q", id)
		}
		return []core.RefreshResult{c.refreshOne(acct)}, nil
	}
	accounts := c.pool.snapshotAccounts()
	out := make([]core.RefreshResult, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, c.refreshOne(a))
	}
	return out, nil
}

// refreshOne has exactly one renewal path available: pick up a token the ZCode
// desktop client has already renewed in its own store.  There is no API this
// module can call to mint one, and api keys have no renewal concept at all.
func (c *Client) refreshOne(acct *Account) core.RefreshResult {
	res := core.RefreshResult{AccountID: acct.ID}
	if acct.Mode != modeJWT {
		res.Error = "api-key credentials cannot be renewed here: rotate the key in the vendor console and add the new one"
		return res
	}
	current := acct.jwt
	for _, s := range discover(c.deps.Logf) {
		if s.Mode != modeJWT || s.ID != acct.ID {
			continue
		}
		if s.jwt == current {
			res.Error = "the ZCode desktop client still holds the same token; renew it there first"
			return res
		}
		c.pool.replaceSecret(acct.ID, s.jwt, s.UserID)
		res.OK = true
		return res
	}
	res.Error = "no renewed token found: this module can only pick up a token the ZCode desktop client has already renewed"
	return res
}

// ---------------------------------------------------------------------------
// core.CredentialImporter
// ---------------------------------------------------------------------------

// Discover implements core.CredentialImporter.  It reports every location that
// was inspected, including the ones that turned out to hold nothing readable.
func (c *Client) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	_ = ctx
	idx := c.pool.managedIndex()
	sources := credentialInventory(c.deps.Logf)
	out := make([]core.DiscoveredCredential, 0, len(sources))
	for _, s := range sources {
		out = append(out, core.DiscoveredCredential{
			Path:       s.Path,
			Kind:       s.Kind,
			Label:      s.Label,
			Note:       s.Note,
			Importable: s.Importable,
			Imported:   s.Importable && idx.has(s.ID, s.secret()),
		})
	}
	return out, nil
}

// Import implements core.CredentialImporter.  With all=false and no paths it is
// a no-op.  Importing something already present is a no-op too, so a repeated
// import is harmless.
func (c *Client) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	_ = ctx
	if !all && len(paths) == 0 {
		return []core.AccountRecord{}, nil
	}
	sources := credentialInventory(c.deps.Logf)

	var chosen []credentialSource
	if all {
		for _, s := range sources {
			if s.Importable {
				chosen = append(chosen, s)
			}
		}
	} else {
		for _, want := range paths {
			want = strings.TrimSpace(want)
			if want == "" {
				continue
			}
			matched := false
			for _, s := range sources {
				if !s.Importable || !pathMatches(want, s.Path) {
					continue
				}
				matched = true
				chosen = append(chosen, s)
			}
			if !matched {
				return nil, fmt.Errorf("no importable credential at %q", want)
			}
		}
	}

	seen := map[string]bool{}
	out := make([]core.AccountRecord, 0, len(chosen))
	for _, s := range chosen {
		if seen[s.Path] {
			continue
		}
		seen[s.Path] = true

		if c.pool.managedIndex().has(s.ID, s.secret()) {
			continue // already imported
		}
		m := managedAccountFromSource(s)
		if err := c.pool.addManaged(m); err != nil {
			var dup *errAccountExists
			if errors.As(err, &dup) {
				continue // already live in the pool from the desktop config
			}
			return out, err
		}
		if rec, ok := c.pool.recordByID(m.ID); ok {
			out = append(out, rec)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Validation and small helpers
// ---------------------------------------------------------------------------

// userIDFromAPIKey extracts the account id from the vendor's dotted API-key
// form (<user-id>.<secret>).  The vendor's coding-plan keys are built that
// way, and the prefix is what lets a pasted key be grouped with the plan JWT
// for the same account.
func userIDFromAPIKey(key string) string {
	key = strings.TrimSpace(key)
	i := strings.IndexByte(key, '.')
	if i <= 0 {
		return ""
	}
	id := strings.TrimSpace(key[:i])
	if len(id) < 6 {
		return ""
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return id
}

func parseKind(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case kindAPIKey, "api_key", "apikey", "api key":
		return kindAPIKey, true
	case kindJWT, "bearer", "plan":
		return kindJWT, true
	}
	return "", false
}

func modeFromKind(kind string) string {
	if k, ok := parseKind(kind); ok && k == kindJWT {
		return modeJWT
	}
	return modeAPIKey
}

func kindNameForMode(mode string) string {
	if mode == modeJWT {
		return kindJWT
	}
	return kindAPIKey
}

func parseRegion(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case regionZai, "z.ai":
		return regionZai, true
	case regionBigmodel, "bigmodel.cn":
		return regionBigmodel, true
	}
	return "", false
}

func providerForRegion(region string) string {
	if r, ok := parseRegion(region); ok && r == regionBigmodel {
		return providerBigmodel
	}
	return providerZai
}

func regionForProvider(provider string) string {
	if strings.EqualFold(strings.TrimSpace(provider), providerBigmodel) {
		return regionBigmodel
	}
	return regionZai
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// credentialTag is an irreversible, non-reversible 8-hex-char tag for a
// credential.  It exists so two credentials for the same provider can be told
// apart in the panel without revealing any part of either.
//
// It is named credentialTag rather than fingerprint because this module also
// imports internal/fingerprint, and a package-level function of that name
// would shadow the import.
func credentialTag(secret string) string {
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:4])
}

func jwtExpiry(token string) (time.Time, bool) {
	secs := core.JWTExpiry(token)
	if secs <= 0 {
		return time.Time{}, false
	}
	return time.Unix(secs, 0).UTC(), true
}

func validAccountID(id string) bool {
	if id == "" || len(id) > 160 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':', c == '@', c == '/':
		default:
			return false
		}
	}
	return true
}

// validateSecret checks a credential's shape without ever quoting it.
func validateSecret(s string, max int) error {
	if len(s) > max {
		return fmt.Errorf("is longer than %d bytes", max)
	}
	if !printableASCII(s) {
		return errors.New("contains non-printable or non-ASCII characters")
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return errors.New("contains whitespace")
	}
	return nil
}

// validateBaseURL checks an endpoint override without echoing it back.
func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must start with http:// or https://")
	}
	if u.Host == "" {
		return errors.New("has no host")
	}
	return nil
}

func sanitizeLabel(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if runes := []rune(out); len(runes) > maxLabelRunes {
		out = string(runes[:maxLabelRunes])
	}
	return out
}
