package trae

// accounts.go — the panel-facing account capability.
//
// The panel knows nothing about Trae.  It asks a module to describe its own
// credential form (AccountFields), list what it holds (Accounts) and act on one
// entry at a time.  Everything Trae-specific — the token pair, the device
// fingerprint, the fact that a pasted credential can be probed with a real
// SOLO chat — stays in this file.
//
// Three capabilities are implemented:
//
//	AccountManager     — list / add / remove / enable / test / refresh
//	CredentialImporter — find and import the desktop app's storage.json
//	LoginProvider      — drive the CN console's web OAuth from Go (weblogin.go)
//
// Storage: <DataDir>/accounts.json, mode 0600.  It holds live tokens in plain
// text, which is the same trust level as the storage.json it can import; it is
// never served to the panel, and no method here returns a token.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// The panel narrows the registered Client with these interfaces.  Failing
// to satisfy one is a compile error here rather than a silent 501 at runtime.
var (
	_ core.AccountManager     = (*Client)(nil)
	_ core.CredentialImporter = (*Client)(nil)
	_ core.LoginProvider      = (*Client)(nil)
	_ core.Reviver            = (*Client)(nil)
)

// accountsFileName is the store inside the module's DataDir.
const accountsFileName = "accounts.json"

// Origins recorded on a stored account.
const (
	originManual   = "manual"
	originImported = "imported"
)

// testPrompt is the smallest request that proves a credential works.  It is a
// real completion, not a health endpoint, because a Trae token can be valid and
// still be refused by the SOLO channel.
const testPrompt = "ping"

// storedAccount is one credential the panel owns.  It is the on-disk shape, so
// every field is explicitly tagged; the tokens are the only secrets in it.
type storedAccount struct {
	ID               string `json:"id"`
	Label            string `json:"label,omitempty"`
	Enabled          bool   `json:"enabled"`
	Origin           string `json:"origin,omitempty"`
	Source           string `json:"source,omitempty"`
	AccessToken      string `json:"access_token,omitempty"`
	RefreshToken     string `json:"refresh_token,omitempty"`
	UserID           string `json:"user_id,omitempty"`
	Username         string `json:"username,omitempty"`
	Email            string `json:"email,omitempty"`
	Host             string `json:"host,omitempty"`
	Region           string `json:"region,omitempty"`
	MachineID        string `json:"machine_id,omitempty"`
	DeviceID         string `json:"device_id,omitempty"`
	ExpiresAt        string `json:"expires_at,omitempty"`
	RefreshExpiresAt string `json:"refresh_expires_at,omitempty"`
	AddedAt          string `json:"added_at,omitempty"`
}

// accountStore is the whole on-disk state.  Disabled lists discovered accounts
// the operator switched off; they are not copied in, only suppressed, so a
// re-discovered credential keeps working when it is switched back on.
type accountStore struct {
	Accounts []storedAccount `json:"accounts"`
	Disabled []string        `json:"disabled,omitempty"`
}

// ---- store -----------------------------------------------------------------

// loadAccountStore reads the store, degrading to empty on any error: a module
// that refuses to start because its own cache is unreadable is worse than one
// that starts with discovery alone.
func loadAccountStore(dir string) *accountStore {
	s := &accountStore{}
	if strings.TrimSpace(dir) == "" {
		return s
	}
	raw, err := os.ReadFile(filepath.Join(dir, accountsFileName))
	if err != nil {
		return s
	}
	if err := json.Unmarshal(raw, s); err != nil {
		return &accountStore{}
	}
	return s
}

// save writes the store atomically so a crash cannot leave a half-written
// credential file behind.
func (s *accountStore) save(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("no data directory configured")
	}
	buf, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, accountsFileName+".tmp")
	if err := os.WriteFile(tmp, append(buf, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, accountsFileName))
}

// index returns the position of id, or -1.
func (s *accountStore) index(id string) int {
	for i := range s.Accounts {
		if s.Accounts[i].ID == id {
			return i
		}
	}
	return -1
}

// upsert inserts or replaces one account, preserving its position.
func (s *accountStore) upsert(sa storedAccount) {
	if i := s.index(sa.ID); i >= 0 {
		s.Accounts[i] = sa
		return
	}
	s.Accounts = append(s.Accounts, sa)
}

// remove deletes a stored account and forgets any suppression for it.
func (s *accountStore) remove(id string) bool {
	i := s.index(id)
	if i < 0 {
		return false
	}
	s.Accounts = append(s.Accounts[:i], s.Accounts[i+1:]...)
	s.unsuppress(id)
	return true
}

// disabledSet renders the suppression list as a lookup.
func (s *accountStore) disabledSet() map[string]bool {
	out := make(map[string]bool, len(s.Disabled))
	for _, id := range s.Disabled {
		out[id] = true
	}
	return out
}

func (s *accountStore) suppressed(id string) bool {
	for _, d := range s.Disabled {
		if d == id {
			return true
		}
	}
	return false
}

func (s *accountStore) suppress(id string) {
	if !s.suppressed(id) {
		s.Disabled = append(s.Disabled, id)
	}
}

func (s *accountStore) unsuppress(id string) {
	out := s.Disabled[:0]
	for _, d := range s.Disabled {
		if d != id {
			out = append(out, d)
		}
	}
	s.Disabled = out
}

// setEnabled toggles a stored account, or suppresses a discovered one.  It
// reports whether the id was known at all.
func (s *accountStore) setEnabled(id string, enabled bool) bool {
	if i := s.index(id); i >= 0 {
		s.Accounts[i].Enabled = enabled
		return true
	}
	if enabled {
		if !s.suppressed(id) {
			return false
		}
		s.unsuppress(id)
		return true
	}
	s.suppress(id)
	return true
}

// ---- turning stored records into live accounts -----------------------------

// toAuth rebuilds a live account from its stored form.
func (sa storedAccount) toAuth(cfg *Config) *Auth {
	if sa.AccessToken == "" && sa.RefreshToken == "" {
		return nil
	}
	a := &Auth{
		AccessToken:  sa.AccessToken,
		RefreshToken: sa.RefreshToken,
		UserID:       sa.UserID,
		Username:     sa.Username,
		Email:        sa.Email,
		Host:         firstNonEmpty(sa.Host, cfg.authHost()),
		Region:       firstNonEmpty(sa.Region, cfg.Region),
		MachineID:    firstNonEmpty(sa.MachineID, cfg.MachineID),
		DeviceID:     firstNonEmpty(sa.DeviceID, cfg.DeviceID),
		Product:      firstNonEmpty(sa.Origin, originManual),
		Source:       sa.Source,
		Configured:   true,
		IDOverride:   sa.ID,
	}
	if t, ok := parseTime(sa.ExpiresAt); ok {
		a.ExpiresAt = t
	}
	if t, ok := parseTime(sa.RefreshExpiresAt); ok {
		a.RefreshExpiresAt = t
	}
	return a
}

// fromAuth snapshots a live account into its stored form.
func fromAuth(a *Auth, origin, label string) storedAccount {
	sa := storedAccount{
		ID:           a.ID(),
		Label:        label,
		Enabled:      true,
		Origin:       origin,
		Source:       a.Source,
		AccessToken:  a.Token(),
		RefreshToken: a.RefreshTokenValue(),
		UserID:       a.UserID,
		Username:     a.Username,
		Email:        a.Email,
		Host:         a.Host,
		Region:       a.Region,
		MachineID:    a.MachineID,
		DeviceID:     a.DeviceID,
		AddedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	if !a.Expiry().IsZero() {
		sa.ExpiresAt = a.Expiry().UTC().Format(time.RFC3339)
	}
	if !a.RefreshExpiresAt.IsZero() {
		sa.RefreshExpiresAt = a.RefreshExpiresAt.UTC().Format(time.RFC3339)
	}
	return sa
}

// mutateStore applies fn under the store lock and persists only when fn reports
// a change.  Every mutation goes through here, so the file and the in-memory
// copy cannot drift.
func (c *Client) mutateStore(fn func(s *accountStore) bool) (bool, error) {
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	if !fn(c.store) {
		return false, nil
	}
	return true, c.store.save(c.dataDir)
}

// rebuildPool re-derives the live pool from three sources, in priority order:
// panel-stored credentials, an explicit config credential, and the desktop
// app's storage.json.  Duplicate ids collapse, so the same account discovered
// twice does not double-count.
func (c *Client) rebuildPool() {
	c.storeMu.Lock()
	stored := append([]storedAccount(nil), c.store.Accounts...)
	suppressed := c.store.disabledSet()
	c.storeMu.Unlock()

	var auths []*Auth
	seen := map[string]bool{}
	add := func(a *Auth) {
		if a == nil {
			return
		}
		id := a.ID()
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		auths = append(auths, a)
	}

	for _, sa := range stored {
		if !sa.Enabled {
			continue
		}
		add(sa.toAuth(c.cfg))
	}
	for _, a := range discoverAuths(c.cfg, c.log) {
		if suppressed[a.ID()] {
			continue
		}
		add(a)
	}
	c.pool.Replace(auths)
}

// findAuth resolves a panel id to a live account.
func (c *Client) findAuth(id string) *Auth {
	for _, a := range c.pool.Accounts() {
		if a.ID() == id {
			return a
		}
	}
	return nil
}

// recordByID re-reads one record so a caller can return exactly what the panel
// will see next.
func (c *Client) recordByID(ctx context.Context, id string) (core.AccountRecord, bool) {
	recs, err := c.Accounts(ctx)
	if err != nil {
		return core.AccountRecord{}, false
	}
	for _, r := range recs {
		if r.ID == id {
			return r, true
		}
	}
	return core.AccountRecord{}, false
}

// ---- AccountManager --------------------------------------------------------

// AccountFields is the "add account" form.  Every field maps to a real Trae
// credential component; nothing here is invented for symmetry.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{
			Key: "access_token", Label: "Access token", Type: "password", Required: true,
			Placeholder: "eyJ…",
			Help:        "The Trae desktop credential. Use Discover + Import to read it out of storage.json, or paste one by hand. It is stored only in this module's data directory and is never returned to the panel.",
		},
		{
			Key: "refresh_token", Label: "Refresh token", Type: "password",
			Help: "Optional. Needed for Refresh. ExchangeToken rotates the refresh-token family server-side, which can sign the desktop app out, so self_renew stays off by default.",
		},
		{
			Key: "user_id", Label: "User ID", Type: "text",
			Placeholder: "3595881099822378",
			Help:        "Stable account key. Read from the token when left blank.",
		},
		{
			Key: "label", Label: "Label", Type: "text",
			Placeholder: "work laptop",
		},
		{
			Key: "region", Label: "Region", Type: "select",
			Options: []string{"", "CN", "INTL"},
			Help:    "Overrides the region carried by the credential.",
		},
		{
			Key: "host", Label: "Account host", Type: "text",
			Placeholder: defaultAuthHost,
			Help:        "OAuth host the token belongs to. Defaults to " + defaultAuthHost + ".",
		},
		{
			Key: "access_token_expiry", Label: "Token expiry", Type: "text",
			Placeholder: "RFC3339 or Unix seconds",
			Help:        "Read from the token's own exp claim when left blank.",
		},
		{
			Key: "machine_id", Label: "Machine ID", Type: "text",
			Help: "Device fingerprint. The upstream silently drops a fingerprint it has not seen before, so leave this blank to reuse whatever discovery found.",
		},
		{
			Key: "device_id", Label: "SOLO device ID", Type: "text",
			Help: "Numeric id from the iCubeAuthInfo://icube-dc:<id> key. Derived from machine_id when blank.",
		},
	}
}

// Accounts lists every credential the module can see, without secrets.  A
// disabled credential is still listed so the operator can switch it back on.
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	c.storeMu.Lock()
	stored := append([]storedAccount(nil), c.store.Accounts...)
	suppressed := append([]string(nil), c.store.Disabled...)
	c.storeMu.Unlock()

	byID := make(map[string]storedAccount, len(stored))
	for _, sa := range stored {
		byID[sa.ID] = sa
	}
	states := map[string]core.AccountStatus{}
	for _, as := range c.pool.Snapshot() {
		states[as.ID] = as
	}

	out := make([]core.AccountRecord, 0, len(states)+len(suppressed))
	seen := map[string]bool{}

	for _, a := range c.pool.Accounts() {
		id := a.ID()
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true

		state := stateUnknown
		note := ""
		if as, ok := states[id]; ok {
			state, note = as.State, as.Note
		}
		rec := core.AccountRecord{
			ID:      id,
			Label:   a.Label(),
			Enabled: state == stateReady || state == stateCooling,
			State:   state,
			Note:    note,
			Fields: map[string]any{
				"origin":            "discovered",
				"managed":           false,
				"product":           a.Product,
				"source":            a.Source,
				"user_id":           a.UserID,
				"region":            a.Region,
				"host":              a.Host,
				"has_refresh_token": a.RefreshTokenValue() != "",
			},
		}
		if !a.Expiry().IsZero() {
			rec.ExpiresAt = a.Expiry().UTC().Format(time.RFC3339)
		}
		if sa, ok := byID[id]; ok {
			rec.Fields["origin"] = firstNonEmpty(sa.Origin, originManual)
			rec.Fields["managed"] = true
			rec.Enabled = sa.Enabled
			if sa.Label != "" {
				rec.Label = sa.Label
			}
			if sa.AddedAt != "" {
				rec.Fields["added_at"] = sa.AddedAt
			}
		}
		out = append(out, rec)
	}

	// A stored account that is switched off leaves the pool, so it has to be
	// listed from the store itself or the operator could never switch it back on.
	for _, sa := range stored {
		if sa.ID == "" || seen[sa.ID] {
			continue
		}
		seen[sa.ID] = true
		out = append(out, core.AccountRecord{
			ID:      sa.ID,
			Label:   firstNonEmpty(sa.Label, sa.Username, sa.UserID, sa.ID),
			Enabled: sa.Enabled,
			State:   stateInvalid,
			Note:    "disabled by operator",
			Fields: map[string]any{
				"origin":   firstNonEmpty(sa.Origin, originManual),
				"managed":  true,
				"disabled": true,
			},
		})
	}

	for _, id := range suppressed {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		label := id
		if sa, ok := byID[id]; ok && sa.Label != "" {
			label = sa.Label
		}
		out = append(out, core.AccountRecord{
			ID:      id,
			Label:   label,
			Enabled: false,
			State:   stateInvalid,
			Note:    "disabled by operator",
			Fields:  map[string]any{"origin": "discovered", "managed": true},
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// AddAccount validates and stores one hand-entered credential.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	access := spec.Field("access_token")
	refresh := spec.Field("refresh_token")
	if access == "" && refresh == "" {
		return core.AccountRecord{}, errors.New("access_token is required")
	}

	// The spec carries the label at the top level; the form may also post it as
	// a field.  Either way an operator-supplied name must win over the derived
	// user id, or the panel shows a wall of numbers.
	label := firstNonEmpty(spec.Field("label"), spec.Label)

	a := &Auth{
		AccessToken:  access,
		RefreshToken: refresh,
		UserID:       spec.Field("user_id"),
		Host:         firstNonEmpty(spec.Field("host"), c.cfg.authHost()),
		Region:       firstNonEmpty(spec.Field("region"), c.cfg.Region),
		MachineID:    firstNonEmpty(spec.Field("machine_id"), c.cfg.MachineID),
		DeviceID:     firstNonEmpty(spec.Field("device_id"), c.cfg.DeviceID),
		Product:      originManual,
		Configured:   true,
	}
	if t, ok := parseTime(spec.Field("access_token_expiry")); ok {
		a.ExpiresAt = t
	} else if t := jwtExpiry(access); !t.IsZero() {
		a.ExpiresAt = t
	}

	sa := fromAuth(a, originManual, label)
	if sa.ID == "" {
		return core.AccountRecord{}, errors.New("cannot derive an account id: provide user_id or a token")
	}
	sa.Enabled = spec.EnabledOr(true)
	a.IDOverride = sa.ID

	if _, err := c.mutateStore(func(s *accountStore) bool {
		s.upsert(sa)
		s.unsuppress(sa.ID)
		return true
	}); err != nil {
		return core.AccountRecord{}, fmt.Errorf("persist account: %w", err)
	}
	c.rebuildPool()

	if rec, ok := c.recordByID(ctx, sa.ID); ok {
		return rec, nil
	}
	return core.AccountRecord{
		ID: sa.ID, Label: firstNonEmpty(sa.Label, sa.ID),
		Enabled: sa.Enabled, State: stateUnknown,
	}, nil
}

// RemoveAccount deletes a credential the panel added.  A discovered credential
// is not the panel's to delete; disabling it is the honest operation.
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	changed, err := c.mutateStore(func(s *accountStore) bool { return s.remove(id) })
	if err != nil {
		return fmt.Errorf("persist account removal: %w", err)
	}
	if !changed {
		return fmt.Errorf("account %q was not added through the panel; disable it instead", id)
	}
	c.rebuildPool()
	return nil
}

// SetAccountEnabled toggles a credential.  Stored credentials flip a flag;
// discovered ones are suppressed without being copied in.
func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	if id == "" {
		return errors.New("account id is required")
	}
	// Refuse a bogus id.  Without this, disabling an unknown id would silently
	// add a suppression entry for an account that never existed.
	//
	// A suppressed id counts as known: a DISCOVERED credential has no stored
	// record to flip, so switching it off removes it from the pool and leaves
	// nothing behind but its suppression entry.  Without this arm, `index` and
	// `findAuth` both miss it and the account can never be switched back on --
	// one click on "disable" would brick it until the store file was edited by
	// hand.
	c.storeMu.Lock()
	known := c.store.index(id) >= 0 || c.store.suppressed(id)
	c.storeMu.Unlock()
	if !known && c.findAuth(id) == nil {
		return fmt.Errorf("account %q not found", id)
	}
	changed, err := c.mutateStore(func(s *accountStore) bool { return s.setEnabled(id, enabled) })
	if err != nil {
		return fmt.Errorf("persist account state: %w", err)
	}
	if !changed {
		return fmt.Errorf("account %q not found", id)
	}
	c.rebuildPool()
	return nil
}

// ReviveAccount clears every runtime penalty and re-enables a parked
// credential.  It never touches the credential itself: an account whose token
// is genuinely dead has to fail on its next real request, because that failure
// is the evidence the pool needs.
func (c *Client) ReviveAccount(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("account id is required")
	}
	a := c.findAuth(id)
	if a == nil {
		// A disabled credential is absent from the pool, so the selector cannot
		// find it.  To the operator "revive" and "enable" are one intent.
		c.storeMu.Lock()
		known := c.store.index(id) >= 0 || c.store.suppressed(id)
		c.storeMu.Unlock()
		if !known {
			return fmt.Errorf("account %q not found", id)
		}
		if err := c.SetAccountEnabled(ctx, id, true); err != nil {
			return err
		}
		a = c.findAuth(id)
		if a == nil {
			return fmt.Errorf("account %q was enabled but did not reload", id)
		}
	}
	c.pool.Revive(a)
	return nil
}

// TestAccount runs one real completion against the upstream with this account.
//
// An upstream rejection is a RESULT (OK false plus the vendor's message), not a
// Go error: the panel is asking "does this credential work", and "no, because
// the plan expired" is a valid answer.  Only an unknown id is a Go error.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	a := c.findAuth(id)
	if a == nil {
		return core.TestResult{}, fmt.Errorf("account %q not found or not enabled", id)
	}

	model := c.cfg.defaultModel()
	maxTok := 16
	req := &core.ChatRequest{
		Model:     model,
		Messages:  []core.Message{{Role: "user", Content: testPrompt}},
		MaxTokens: &maxTok,
		Stream:    false,
	}
	body, err := BuildBody(req, c.cfg.payloadOptions())
	if err != nil {
		return core.TestResult{}, fmt.Errorf("build test request: %w", err)
	}

	rctx, cancel := context.WithTimeout(ctx, c.cfg.timeout())
	defer cancel()

	res := core.TestResult{AccountID: id, Model: model}
	start := time.Now()

	rc, apiErr, transportErr := c.chatStream(rctx, a, body)
	switch {
	case transportErr != nil:
		c.pool.MarkFailure(a, transportErr)
		res.Error = transportErr.Error()
	case apiErr != nil:
		c.pool.MarkFailure(a, apiErr)
		res.Error = apiErr.Error()
	default:
		reply, derr := drainStream(newStream(c, a, rc))
		if derr != nil {
			c.pool.MarkFailure(a, derr)
			res.Error = derr.Error()
		} else {
			c.pool.MarkSuccess(a)
			res.OK = true
			res.Reply = truncate(strings.TrimSpace(reply), 200)
		}
	}

	res.ElapsedMS = time.Since(start).Milliseconds()
	return res, nil
}

// RefreshAccount renews one credential, or every credential when id is empty.
// Per-account failures are reported inside the results; only a malformed
// request (an id that matches nothing) is a Go error.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	var targets []*Auth
	for _, a := range c.pool.Accounts() {
		if id == "" || a.ID() == id {
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no account matches %q", id)
	}

	out := make([]core.RefreshResult, 0, len(targets))
	for _, a := range targets {
		rr := core.RefreshResult{AccountID: a.ID()}
		if a.RefreshTokenValue() == "" {
			rr.Error = "no refresh token stored for this account"
			out = append(out, rr)
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, c.cfg.timeout())
		err := c.refreshToken(rctx, a)
		cancel()
		if err != nil {
			rr.Error = err.Error()
			out = append(out, rr)
			continue
		}
		rr.OK = true
		out = append(out, rr)
		c.persistRefreshed(a)
	}
	c.rebuildPool()
	return out, nil
}

// persistRefreshed writes a rotated token pair back to the store so a restart
// does not resurrect the old one.  Discovered credentials have nowhere to be
// written and are left alone.
func (c *Client) persistRefreshed(a *Auth) {
	id := a.ID()
	_, _ = c.mutateStore(func(s *accountStore) bool {
		i := s.index(id)
		if i < 0 {
			return false
		}
		s.Accounts[i].AccessToken = a.Token()
		s.Accounts[i].RefreshToken = a.RefreshTokenValue()
		if exp := a.Expiry(); !exp.IsZero() {
			s.Accounts[i].ExpiresAt = exp.UTC().Format(time.RFC3339)
		}
		if exp := a.RefreshExpiry(); !exp.IsZero() {
			s.Accounts[i].RefreshExpiresAt = exp.UTC().Format(time.RFC3339)
		}
		return true
	})
}

// ---- CredentialImporter ----------------------------------------------------

// Discover lists the storage.json files the desktop app could have written.
// It is read-only: a candidate is only decrypted in memory to decide whether it
// is importable.
func (c *Client) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	c.storeMu.Lock()
	have := map[string]bool{}
	for _, sa := range c.store.Accounts {
		if sa.Source != "" {
			have[sa.Source] = true
		}
	}
	c.storeMu.Unlock()

	paths := storageCandidates(c.cfg)
	out := make([]core.DiscoveredCredential, 0, len(paths))
	for _, path := range paths {
		dc := core.DiscoveredCredential{Path: path, Kind: "storage.json"}
		st, err := os.Stat(path)
		if err != nil {
			dc.Label = productOf(path)
			if errors.Is(err, os.ErrNotExist) {
				dc.Note = "not present"
			} else {
				dc.Note = err.Error()
			}
			out = append(out, dc)
			continue
		}
		dc.Label = fmt.Sprintf("%s (%.1f KiB)", productOf(path), float64(st.Size())/1024)
		dc.Imported = have[path]

		a, err := loadAuthFromStorage(path, c.cfg)
		if err != nil {
			dc.Note = err.Error()
			out = append(out, dc)
			continue
		}
		dc.Importable = true
		dc.Note = fmt.Sprintf("account %s, %s", a.Masked(), expiryState(a.Expiry()))
		out = append(out, dc)
	}
	return out, nil
}

// Import copies the named storage.json credentials into the store.  Paths that
// fail are reported in the error while the readable ones are still imported.
func (c *Client) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	wanted := paths
	if all {
		found, err := c.Discover(ctx)
		if err != nil {
			return nil, err
		}
		wanted = nil
		for _, dc := range found {
			if dc.Importable {
				wanted = append(wanted, dc.Path)
			}
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	var (
		out  []core.AccountRecord
		errs []string
	)
	for _, path := range wanted {
		a, err := loadAuthFromStorage(path, c.cfg)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		sa := fromAuth(a, originImported, firstNonEmpty(a.Username, a.UserID, a.Product))
		if sa.ID == "" {
			errs = append(errs, path+": credential has no usable identity")
			continue
		}
		if _, err := c.mutateStore(func(s *accountStore) bool {
			s.upsert(sa)
			s.unsuppress(sa.ID)
			return true
		}); err != nil {
			errs = append(errs, fmt.Sprintf("%s: persist: %v", path, err))
			continue
		}
		if rec, ok := c.recordByID(ctx, sa.ID); ok {
			out = append(out, rec)
		}
	}
	c.rebuildPool()

	if len(errs) > 0 {
		return out, errors.New(strings.Join(errs, "; "))
	}
	return out, nil
}

// ---- helpers ---------------------------------------------------------------

// productOf recovers the Trae product directory name from a storage.json path.
func productOf(path string) string {
	// <APPDATA>\<product>\User\globalStorage\storage.json
	dir := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	if base := filepath.Base(dir); base != "." && base != string(filepath.Separator) {
		return base
	}
	return filepath.Base(path)
}

// drainStream reads a completion to its end and returns the concatenated text.
func drainStream(s core.Stream) (string, error) {
	defer s.Close()
	var sb strings.Builder
	for {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return sb.String(), nil
		}
		if err != nil {
			return sb.String(), err
		}
		switch ev.Type {
		case core.EventDelta:
			sb.WriteString(ev.Delta)
		case core.EventError:
			if ev.Err != nil {
				return sb.String(), ev.Err
			}
		case core.EventDone:
			return sb.String(), nil
		}
	}
}

// jwtExpiry reads the exp claim out of an unverified JWT payload.  The value is
// only used to pre-fill a form field and to schedule a refresh, so verifying the
// signature would buy nothing: the upstream is the only authority on whether
// the token is good.
func jwtExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}
	}
	return epochTime(claims.Exp)
}
