package minimaxcode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// managedFile holds the credentials an operator typed into the panel.  It is
// the only file this module writes that contains a secret, which is why it is
// separate from accounts.json: runtime state can be deleted freely, a credential
// cannot.
const managedFile = "managed_accounts.json"
const managedVersion = 1

// errNotRefreshable is the one honest answer for a row that holds a bare
// access token.  It names both routes that produce a refreshable row, because
// the older text only named the desktop client and would now send an operator
// hunting for a GUI sign-in when the panel can mint the same thing itself.
const errNotRefreshable = "this credential carries no refresh token: add the account with the panel's 登录 flow, or import the desktop client's store, so the module can renew it in place"

// managedAccount is one panel-added credential.
type managedAccount struct {
	ID      string `json:"id"`
	Label   string `json:"label,omitempty"`
	Token   string `json:"token"`
	BaseURL string `json:"base_url,omitempty"`
	Enabled bool   `json:"enabled"`

	// RefreshToken, ExpiresAt and ClientID are written only for rows the panel
	// signed in itself.  A device-code sign-in mints an access/refresh PAIR,
	// and MiniMax retires a refresh token the moment it is exchanged, so a row
	// that dropped its copy would die within the hour with no way back except
	// another manual sign-in.  A typed credential has none of the three, and
	// omitempty keeps this file byte-identical for those rows.
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
}

type managedStore struct {
	Version  int              `json:"version"`
	Accounts []managedAccount `json:"accounts"`
}

// loadManagedLocked folds the panel-added credentials into the pool.
func (p *pool) loadManagedLocked() {
	if p.managedLoaded {
		return
	}
	p.managedLoaded = true

	var store managedStore
	if err := core.ReadJSON(p.path(managedFile), &store); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			p.logf("minimaxcode: cannot read %s: %v", p.path(managedFile), err)
		}
		return
	}
	p.managed = store.Accounts
	for _, m := range store.Accounts {
		if strings.TrimSpace(m.Token) == "" {
			continue
		}
		p.putManagedLocked(&Account{
			ID:           m.ID,
			Label:        firstNonEmpty(m.Label, m.ID),
			Token:        m.Token,
			BaseURL:      m.BaseURL,
			Source:       sourceManaged,
			Origin:       managedFile,
			Enabled:      m.Enabled,
			Managed:      true,
			State:        stateReady,
			RefreshToken: m.RefreshToken,
			ClientID:     m.ClientID,
			ExpiresAt:    parseStoredTime(m.ExpiresAt),
		})
	}
}

// saveManagedLocked writes the panel-added credentials.  It is only ever called
// from a call that is allowed to write.
func (p *pool) saveManagedLocked() error {
	store := managedStore{Version: managedVersion}
	for _, a := range p.accounts {
		if a.Source != sourceManaged {
			continue
		}
		store.Accounts = append(store.Accounts, managedAccount{
			ID:      a.ID,
			Label:   a.Label,
			Token:   a.Token,
			BaseURL: a.BaseURL,
			Enabled: a.Enabled,
			// These three travel together: a row with a refresh token but no
			// client id cannot be renewed, and the rotation below relies on
			// every write replacing all of them at once.
			RefreshToken: a.RefreshToken,
			ExpiresAt:    formatStoredTime(a.ExpiresAt),
			ClientID:     a.ClientID,
		})
	}
	sort.Slice(store.Accounts, func(i, j int) bool { return store.Accounts[i].ID < store.Accounts[j].ID })
	return core.WriteJSONAtomic(p.path(managedFile), store)
}

// putManagedLocked installs a panel-owned row, replacing any row that already
// claims the same id.
//
// ensureLocked loads config, then discovery, then the panel store, and addLocked
// keeps the first claimant.  That is the right rule for two independent
// sources that happen to collide, but it is wrong here: a row in
// managed_accounts.json is an explicit operator action, and a sign-in has to be
// able to take over the id it names -- otherwise logging in from the panel
// would silently do nothing whenever the desktop client's store already held a
// row for the same account.
func (p *pool) putManagedLocked(a *Account) {
	for i, existing := range p.accounts {
		if existing.ID == a.ID {
			p.accounts[i] = a
			return
		}
	}
	p.addLocked(a)
}

// parseStoredTime and formatStoredTime keep the sign-in timestamps in RFC3339
// so the store stays readable by eye.  An unparseable value degrades to "no
// expiry recorded" rather than failing the whole load: the credential is still
// usable, and the vendor is the final judge of whether it is.
func parseStoredTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

func formatStoredTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// refreshable reports whether a row can be renewed in place.  A typed
// credential has neither half; a discovered row renews back into the desktop
// client's store; a row the panel signed in renews into managed_accounts.json.
func (a *Account) refreshable() bool {
	return a != nil && strings.TrimSpace(a.RefreshToken) != "" && (a.AuthPath != "" || a.Managed)
}

// reloadLocked rebuilds the account list from scratch.  It is used after a
// tombstone is cleared, because an account that was hidden is not in the slice
// and cannot simply be re-enabled in place.
func (p *pool) reloadLocked() {
	p.loaded = false
	p.managedLoaded = false
	p.accounts = nil
	p.cursor = 0
	p.ensureLocked()
}

// recordsForPanel renders the account table.  It is the editing surface, so it
// carries the non-secret extras an operator needs to tell two rows apart: where
// the credential came from, and a short tag proving two rows hold different
// tokens without either token being visible.
func (p *pool) recordsForPanel() []core.AccountRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()

	now := time.Now()
	out := make([]core.AccountRecord, 0, len(p.accounts))
	for _, a := range p.accounts {
		state := a.State
		if !a.Enabled {
			state = stateInvalid
		}
		note := a.LastError
		if note == "" && state == stateReady && !a.ExpiresAt.IsZero() && !a.ExpiresAt.After(now) {
			note = "the access token has expired; it will be refreshed on the next request"
		}
		fields := map[string]any{
			"source": a.Source,
			"origin": a.Origin,
			"tag":    credentialTag(a.Token),
		}
		if a.RefreshToken != "" {
			fields["refreshable"] = true
		}
		out = append(out, core.AccountRecord{
			ID:        a.ID,
			Label:     a.Label,
			Enabled:   a.Enabled,
			State:     state,
			ExpiresAt: timeToRFC3339(a.ExpiresAt),
			Note:      note,
			Fields:    fields,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AccountFields describes the add form.  The credential is the only required
// input; everything else has a sane default.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{
			Key:         "access_token",
			Label:       "Access token",
			Type:        "password",
			Required:    true,
			Placeholder: "mmoat_xxxxxxxx",
			Help:        "The mmoat_… token from ~/.minimax/auth/<buildEnv>/<region>/<clientId>/auth.json. The token is stored in this module's own data directory; the desktop client's file is never modified by adding a row here.",
		},
		{
			Key:   "label",
			Label: "Label",
			Type:  "text",
			Help:  "A name for this credential, shown in the account table.",
		},
		{
			Key:         "base_url",
			Label:       "Base URL",
			Type:        "text",
			Placeholder: defaultBaseURL,
			Help:        "Override the upstream for this account. Leave empty to use " + defaultBaseURL + ".",
		},
	}
}

// Accounts lists what this module holds.  It never fails just because the pool
// is empty.
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	return c.pool.recordsForPanel(), nil
}

// AddAccount stores one panel-typed credential.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	token := strings.TrimSpace(spec.Fields["access_token"])
	if token == "" {
		return core.AccountRecord{}, errors.New("access_token is required")
	}
	if len(token) > maxTokenBytes {
		return core.AccountRecord{}, fmt.Errorf("access_token is longer than %d bytes", maxTokenBytes)
	}

	label := truncateLabel(strings.TrimSpace(spec.Fields["label"]))
	baseURL := strings.TrimSpace(spec.Fields["base_url"])
	if len(baseURL) > maxURLBytes {
		return core.AccountRecord{}, fmt.Errorf("base_url is longer than %d bytes", maxURLBytes)
	}
	if baseURL != "" && !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return core.AccountRecord{}, errors.New("base_url must start with http:// or https://")
	}

	id := strings.TrimSpace(spec.ID)
	if id == "" {
		id = name + ":managed:" + credentialTag(token)
	}
	if label == "" {
		label = id
	}
	enabled := spec.Enabled == nil || *spec.Enabled

	// Re-adding an id that is hidden must bring it back rather than silently
	// doing nothing: the operator asked for a row and would otherwise see none.
	c.pool.mu.Lock()
	c.pool.ensureLocked()
	delete(c.pool.removed, id)
	replaced := false
	for _, a := range c.pool.accounts {
		if a.ID != id {
			continue
		}
		a.Label = label
		a.Token = token
		a.BaseURL = baseURL
		a.Enabled = enabled
		a.Source = sourceManaged
		a.Origin = managedFile
		a.Managed = true
		a.State = stateReady
		a.CooldownUntil = time.Time{}
		a.LastError = ""
		a.Failures = 0
		replaced = true
		break
	}
	if !replaced {
		c.pool.addLocked(&Account{
			ID:      id,
			Label:   label,
			Token:   token,
			BaseURL: baseURL,
			Source:  sourceManaged,
			Origin:  managedFile,
			Enabled: enabled,
			Managed: true,
			State:   stateReady,
		})
	}
	err := c.pool.saveManagedLocked()
	c.pool.saveLocked()
	c.pool.mu.Unlock()
	if err != nil {
		return core.AccountRecord{}, fmt.Errorf("minimaxcode: cannot store the account: %w", err)
	}

	for _, rec := range c.pool.recordsForPanel() {
		if rec.ID == id {
			return rec, nil
		}
	}
	return core.AccountRecord{ID: id, Label: label, Enabled: enabled, State: stateReady}, nil
}

// adoptLoginCredential installs the credential a panel sign-in just minted and
// returns the row as the panel should show it.
//
// The id is the same <buildEnv>/<region>/<clientId> triple discovery builds, so
// signing in from the panel lands on the row that already stands for this
// MiniMax Code account -- a second login replaces the credential instead of
// stacking up a duplicate row beside a broken one.
//
// When that row came from the desktop client's store, the managed copy takes it
// over and the store link is dropped.  That is deliberate: this module never
// writes its own credentials into a file another program owns, so the choice is
// between owning the row here or leaving the operator's fresh sign-in unused.
func (c *Client) adoptLoginCredential(id, label, clientID, access, refresh string, expiresAt time.Time) (core.AccountRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return core.AccountRecord{}, errors.New("minimaxcode: the sign-in returned no account id")
	}
	if strings.TrimSpace(label) == "" {
		label = id
	}
	clientID = strings.TrimSpace(clientID)

	c.pool.mu.Lock()
	c.pool.ensureLocked()
	delete(c.pool.removed, id)
	if existing := c.pool.accountLocked(id); existing != nil {
		existing.Label = label
		existing.Token = access
		existing.RefreshToken = refresh
		existing.ClientID = clientID
		existing.ExpiresAt = expiresAt
		existing.Source = sourceManaged
		existing.Origin = managedFile
		existing.Managed = true
		existing.Enabled = true
		existing.AuthPath = ""
		existing.RecordKey = ""
		existing.State = stateReady
		existing.CooldownUntil = time.Time{}
		existing.Failures = 0
		existing.LastError = ""
	} else {
		c.pool.putManagedLocked(&Account{
			ID:           id,
			Label:        label,
			Token:        access,
			Source:       sourceManaged,
			Origin:       managedFile,
			Enabled:      true,
			Managed:      true,
			State:        stateReady,
			RefreshToken: refresh,
			ClientID:     clientID,
			ExpiresAt:    expiresAt,
		})
	}
	err := c.pool.saveManagedLocked()
	c.pool.saveLocked()
	c.pool.mu.Unlock()
	if err != nil {
		return core.AccountRecord{}, fmt.Errorf("minimaxcode: cannot store the signed-in account: %w", err)
	}

	for _, rec := range c.pool.recordsForPanel() {
		if rec.ID == id {
			return rec, nil
		}
	}
	return core.AccountRecord{ID: id, Label: label, Enabled: true, State: stateReady}, nil
}

// RemoveAccount deletes one credential.
//
// A panel-added credential is genuinely deleted.  A credential discovered in the
// desktop client's store cannot be: that file belongs to another program, and
// deleting it would sign the user out of MiniMax Code.  Such a row is hidden
// instead, and the tombstone is recorded so the next discovery does not bring it
// straight back.  Re-enabling the row clears the tombstone.
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("an account id is required")
	}

	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()
	c.pool.ensureLocked()

	var target *Account
	for _, a := range c.pool.accounts {
		if a.ID == id {
			target = a
			break
		}
	}
	if target == nil {
		if c.pool.removed[id] {
			return nil // already hidden: the operator's intent is satisfied
		}
		return fmt.Errorf("account %q is not held by this client", id)
	}

	if target.Source == sourceManaged {
		kept := c.pool.accounts[:0]
		for _, a := range c.pool.accounts {
			if a.ID != id {
				kept = append(kept, a)
			}
		}
		c.pool.accounts = kept
		if c.pool.cursor >= len(c.pool.accounts) {
			c.pool.cursor = 0
		}
	} else {
		if c.pool.removed == nil {
			c.pool.removed = map[string]bool{}
		}
		c.pool.removed[id] = true
		kept := c.pool.accounts[:0]
		for _, a := range c.pool.accounts {
			if a.ID != id {
				kept = append(kept, a)
			}
		}
		c.pool.accounts = kept
		if c.pool.cursor >= len(c.pool.accounts) {
			c.pool.cursor = 0
		}
	}

	if err := c.pool.saveManagedLocked(); err != nil {
		return fmt.Errorf("minimaxcode: cannot store the account list: %w", err)
	}
	c.pool.saveLocked()
	return nil
}

// SetAccountEnabled toggles one credential without deleting it.
func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("an account id is required")
	}

	c.pool.mu.Lock()
	defer c.pool.mu.Unlock()
	c.pool.ensureLocked()

	if c.pool.removed[id] {
		if !enabled {
			return nil
		}
		// The row is not in the slice, so it has to be rediscovered rather
		// than flipped in place.
		delete(c.pool.removed, id)
		c.pool.reloadLocked()
		for _, a := range c.pool.accounts {
			if a.ID == id {
				a.Enabled = true
				break
			}
		}
		if err := c.pool.saveManagedLocked(); err != nil {
			return fmt.Errorf("minimaxcode: cannot store the account list: %w", err)
		}
		c.pool.saveLocked()
		return nil
	}

	for _, a := range c.pool.accounts {
		if a.ID != id {
			continue
		}
		a.Enabled = enabled
		if enabled {
			// Re-enabling is an explicit statement that the operator believes
			// the credential works, so the penalty it was carrying is cleared.
			a.State = stateReady
			a.CooldownUntil = time.Time{}
			a.LastError = ""
			a.Failures = 0
		}
		if err := c.pool.saveManagedLocked(); err != nil {
			return fmt.Errorf("minimaxcode: cannot store the account list: %w", err)
		}
		c.pool.saveLocked()
		return nil
	}
	return fmt.Errorf("account %q is not held by this client", id)
}

// TestAccount probes one credential against the live upstream.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	id = strings.TrimSpace(id)
	acct := c.pool.find(id)
	if acct == nil {
		return core.TestResult{}, fmt.Errorf("account %q is not held by this client", id)
	}

	model := c.firstModel()
	if model == "" {
		return core.TestResult{}, errors.New("minimaxcode: no model is known, so there is nothing to probe with")
	}

	started := time.Now()
	reply, err := c.probe(ctx, acct, model)
	result := core.TestResult{
		OK:        err == nil,
		AccountID: acct.ID,
		Model:     model,
		Reply:     truncate(reply, probeReplyLimit),
		ElapsedMS: time.Since(started).Milliseconds(),
	}
	if err != nil {
		result.Error = redactSecret(err.Error(), acct.secret())
		c.pool.noteFailure(acct.ID, failureKindOf(err), truncate(result.Error, 200))
		return result, nil
	}
	c.pool.noteSuccess(acct.ID)
	return result, nil
}

// RefreshAccount renews one credential, or every refreshable credential when id
// is empty.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	id = strings.TrimSpace(id)

	// Work on snapshots taken under the pool lock.  A credential field must
	// never be read while a concurrent refresh is rewriting it.
	var targets []Account
	if id == "" {
		for _, a := range c.pool.selectableSnapshots() {
			if a.refreshable() {
				targets = append(targets, a)
			}
		}
	} else {
		acct, ok := c.pool.current(id)
		if !ok {
			return nil, fmt.Errorf("account %q is not held by this client", id)
		}
		// A credential typed into the panel is an access token on its own: it
		// has no refresh token and no store to write back to.  Saying so is
		// better than sending an empty refresh token upstream and reporting
		// whatever the vendor makes of that.
		if !acct.refreshable() {
			return []core.RefreshResult{{
				AccountID: acct.ID,
				OK:        false,
				Error:     errNotRefreshable,
			}}, nil
		}
		targets = append(targets, acct)
	}

	if len(targets) == 0 {
		return nil, errors.New("minimaxcode: no account holds a refresh token, so there is nothing to renew")
	}

	out := make([]core.RefreshResult, 0, len(targets))
	for _, a := range targets {
		out = append(out, c.renewOne(ctx, a))
	}
	return out, nil
}

// renewOne renews one credential under the per-account refresh lock, so a manual
// refresh never races an automatic one.  The vendor retires a refresh token the
// moment it is exchanged, so the loser of that race would write a token the
// vendor has already retired into the desktop client's store.
func (c *Client) renewOne(ctx context.Context, a Account) core.RefreshResult {
	lock := c.pool.refreshLock(a.ID)
	lock.Lock()
	defer lock.Unlock()

	cur, ok := c.pool.current(a.ID)
	if !ok {
		return core.RefreshResult{AccountID: a.ID, OK: false, Error: "the account is no longer held by this client"}
	}
	if !cur.refreshable() {
		return core.RefreshResult{
			AccountID: cur.ID,
			OK:        false,
			Error:     errNotRefreshable,
		}
	}

	access, refresh, expiresAt, err := refreshCredential(ctx, c.http, c.cfg.oauthTokenURL(), c.refreshClientIDs(cur), cur.RefreshToken)
	if err != nil {
		return core.RefreshResult{AccountID: cur.ID, OK: false, Error: redactSecret(err.Error(), cur.secret())}
	}
	// Where the new pair lands depends on who owns the row.  A discovered row
	// bodies to the desktop client, so the rotation is written back into its
	// store.  A row the panel signed in has no store behind it and is persisted
	// into managed_accounts.json instead.  Doing that second write is not
	// optional: MiniMax retires the old refresh token the moment this exchange
	// succeeds, so a renewal nobody saves is a renewal that is already lost.
	if cur.AuthPath != "" {
		if werr := writeBackCredential(cur.AuthPath, cur.RecordKey, access, refresh, expiresAt, cur.Generation+1); werr != nil {
			return core.RefreshResult{
				AccountID: cur.ID,
				OK:        true,
				Error:     "the token was renewed but could not be written back to the desktop client's store: " + werr.Error(),
			}
		}
	}
	c.pool.mark(cur.ID, func(acct *Account) {
		acct.Token = access
		if strings.TrimSpace(refresh) != "" {
			acct.RefreshToken = refresh
		}
		if !expiresAt.IsZero() {
			acct.ExpiresAt = expiresAt
		}
		acct.Generation++
		acct.State = stateReady
		acct.LastError = ""
		acct.Failures = 0
	})
	if cur.Managed {
		if werr := c.pool.persistManaged(); werr != nil {
			return core.RefreshResult{
				AccountID: cur.ID,
				OK:        true,
				Error:     "the token was renewed but could not be written to the panel's account store: " + werr.Error(),
			}
		}
	}
	return core.RefreshResult{AccountID: cur.ID, OK: true}
}

// Discover lists the credentials the desktop client has on this machine.  It is
// read-only.
func (c *Client) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	root := c.cfg.authDir()
	found := discoverCredentials(root, c.logf)

	held := map[string]bool{}
	for _, rec := range c.pool.recordsForPanel() {
		held[rec.ID] = true
	}

	out := make([]core.DiscoveredCredential, 0, len(found))
	for _, cred := range found {
		kind := "auth.json"
		if cred.BuildEnv != "" || cred.Region != "" {
			kind = strings.Trim(strings.Join([]string{cred.BuildEnv, cred.Region, cred.ClientID}, "/"), "/")
		}
		note := "MiniMax Code credential"
		if !cred.ExpiresAt.IsZero() {
			note += ", expires " + cred.ExpiresAt.UTC().Format(time.RFC3339)
		}
		out = append(out, core.DiscoveredCredential{
			Path:       cred.Path,
			Kind:       kind,
			Label:      cred.Label,
			Note:       note,
			Importable: true,
			Imported:   held[cred.ID],
		})
	}
	return out, nil
}

// Import copies the named discovered credentials into this module's own store.
//
// Import is the only call in this interface allowed to write, and it writes
// nothing outside the module's data directory: the desktop client's auth.json is
// read, never modified, so importing cannot disturb the user's signed-in client.
func (c *Client) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	found := discoverCredentials(c.cfg.authDir(), c.logf)

	want := map[string]bool{}
	for _, p := range paths {
		if p = strings.TrimSpace(p); p != "" {
			want[p] = true
		}
	}

	var chosen []credential
	for _, cred := range found {
		switch {
		case want[cred.Path]:
			chosen = append(chosen, cred)
		case all:
			chosen = append(chosen, cred)
		}
	}
	if len(chosen) == 0 {
		if len(paths) > 0 || all {
			return nil, errors.New("nothing to import: no importable credential was named")
		}
		return nil, nil
	}

	var out []core.AccountRecord
	for _, cred := range chosen {
		rec, err := c.AddAccount(ctx, core.AccountSpec{
			ID:      cred.ID,
			Label:   cred.Label,
			Enabled: boolPtr(true),
			Fields: map[string]string{
				"access_token": cred.Access,
				"label":        cred.Label,
			},
		})
		if err != nil {
			return out, fmt.Errorf("minimaxcode: cannot import %s: %w", cred.Path, err)
		}
		out = append(out, rec)
	}
	return out, nil
}

// probe performs one small completion with one account.
func (c *Client) probe(ctx context.Context, acct *Account, model string) (string, error) {
	maxTokens := 32
	req := &core.ChatRequest{
		Model:     model,
		Messages:  []core.Message{{Role: "user", Content: "say hi in 3 words"}},
		MaxTokens: &maxTokens,
	}
	stream, err := c.attempt(ctx, acct, model, req)
	if err != nil {
		return "", err
	}
	return drainReply(stream)
}

// firstModel is the model a probe uses: the first one the catalogue offers.
func (c *Client) firstModel() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, m := range c.models {
		if id := strings.TrimSpace(m.ID); id != "" {
			return id
		}
	}
	return ""
}

// failureKindOf classifies an arbitrary error for the pool's penalty record.
func failureKindOf(err error) core.FailureKind {
	var ue *upstreamError
	if errors.As(err, &ue) {
		return ue.failureKind()
	}
	if errors.Is(err, core.ErrNotConfigured) {
		return core.FailureAuth
	}
	return core.FailureUpstream
}

func truncateLabel(s string) string {
	if len([]rune(s)) <= maxLabelRunes {
		return s
	}
	return string([]rune(s)[:maxLabelRunes])
}

func boolPtr(v bool) *bool { return &v }
