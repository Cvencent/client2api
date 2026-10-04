package opencode

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// core.AccountManager.
//
// The panel's account table is the editing surface: everything listed here can
// be removed, disabled and tested.  An endpoint the module merely *resolves*
// (the base URL, the vendor auth.json path) is configuration, and is reported
// in Status().Detail instead.
// ---------------------------------------------------------------------------

// credentialsPath is where this module stores the credentials it owns.
//
// An empty DataDir means "do not persist": writing a credential file into the
// process working directory would be worse than losing the accounts.
func (c *Client) credentialsPath() string {
	if strings.TrimSpace(c.deps.DataDir) == "" {
		return ""
	}
	return filepath.Join(c.deps.DataDir, credentialsFile)
}

// persist writes the credential file.  A failure is recorded, never returned:
// an unwritable data directory must not turn a chat that already succeeded into
// an error.
func (c *Client) persist() {
	path := c.credentialsPath()
	if path == "" {
		return
	}
	recs := c.pool.snapshot()
	out := make([]accountRecord, 0, len(recs))
	for _, r := range recs {
		// Credentials that came from the config or the environment are
		// re-derived on every start.  Copying them into our own data directory
		// would put a secret on disk that the operator deliberately keeps
		// elsewhere.
		if r.Source == sourceEnv || r.Source == sourceConfig {
			continue
		}
		out = append(out, r)
	}
	if err := saveCredentials(path, out); err != nil {
		c.noteError("saving credentials: " + describeError(err))
	}
}

// loadAccounts rebuilds the pool from every credential source.
//
// Persisted records keep their state (enabled flag, cooldown, error count)
// across a reload, and a record whose source file has since disappeared is
// kept: the panel is where accounts are deleted, so a vanished file must not
// silently delete an account.
func (c *Client) loadAccounts() {
	stored := loadCredentials(c.credentialsPath())
	byID := make(map[string]accountRecord, len(stored))
	for _, r := range stored {
		if r.ID != "" {
			byID[r.ID] = r
		}
	}

	cands := c.candidates()
	cands = append(cands, c.vendorCandidates()...)

	out := make([]accountRecord, 0, len(cands)+len(stored))
	seen := make(map[string]bool, len(cands)+len(stored))
	for _, cand := range cands {
		id := accountID(keyFingerprint(cand.key))
		if seen[id] {
			continue
		}
		seen[id] = true
		rec := accountRecord{
			ID:      id,
			Label:   cand.label,
			APIKey:  cand.key,
			Enabled: true,
			Source:  cand.source,
		}
		if old, ok := byID[id]; ok {
			rec.Enabled = old.Enabled
			rec.AddedAt = old.AddedAt
			rec.CooldownUntil = old.CooldownUntil
			rec.CooldownKind = old.CooldownKind
			rec.LastError = old.LastError
			rec.ErrCount = old.ErrCount
			if old.Label != "" {
				rec.Label = old.Label
			}
			if old.Note != "" {
				rec.Note = old.Note
			}
		}
		out = append(out, rec)
	}
	for _, r := range stored {
		if r.ID == "" || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		out = append(out, r)
	}

	c.pool.reload(out)
}

// AccountFields describes the manual-add form.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{
			Key:      "key",
			Label:    "API key",
			Type:     "password",
			Required: true,
			Help: "An OpenCode Zen API key from opencode.ai. " +
				"You can also use the Login tab to add an anonymous free account " +
				"or sign in to the OpenCode Console. " +
				"A pasted key is stored locally and never logged.",
		},
		{
			Key:      "label",
			Label:    "Label",
			Type:     "text",
			Required: false,
			Help:     "Optional name shown in the account table.",
		},
	}
}

// accountRecords projects the pool into the panel's non-secret view.
func (c *Client) accountRecords(now time.Time) []core.AccountRecord {
	snapshot := c.pool.snapshot()
	out := make([]core.AccountRecord, 0, len(snapshot))
	for i := range snapshot {
		a := &snapshot[i]
		out = append(out, core.AccountRecord{
			ID:      a.ID,
			Label:   a.Label,
			Enabled: a.Enabled,
			State:   stateOf(a, now),
			Note:    noteOf(a, now),
			// Identity stays empty: Zen's key is not an account identifier.
			Fields: accountFields(a),
		})
	}
	return out
}

// accountFields renders the non-secret columns for one account row.  A keyed
// account shows a masked key; an anonymous account says so, and an OAuth
// account shows the organisation it signed in to.
func accountFields(a *accountRecord) map[string]any {
	fields := map[string]any{
		"source":    sourceLabel(a.Source),
		"in_flight": a.inFlight,
		"auth_mode": a.authMode(),
	}
	switch a.authMode() {
	case "anonymous":
		fields["key"] = "public (anonymous)"
	case "oauth":
		fields["key"] = core.MaskSecret(a.AccessToken)
		if a.OrgName != "" {
			fields["org"] = a.OrgName
		} else if a.OrgID != "" {
			fields["org"] = a.OrgID
		}
	default:
		fields["key"] = core.MaskSecret(a.APIKey)
	}
	if a.Email != "" {
		fields["email"] = a.Email
	}
	return fields
}

// Accounts lists the accounts the panel may act on.
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	c.ensure()
	return c.accountRecords(c.now()), nil
}

// AddAccount adds a key pasted into the panel.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	c.ensure()
	key := strings.TrimSpace(spec.Field("key"))
	if key == "" {
		return core.AccountRecord{}, fmt.Errorf("%w: an API key is required", core.ErrUnsupported)
	}
	id := accountID(keyFingerprint(key))
	label := strings.TrimSpace(spec.Field("label"))
	if label == "" {
		label = spec.Label
	}
	rec := accountRecord{
		ID:      id,
		Label:   label,
		APIKey:  key,
		Enabled: spec.EnabledOr(true),
		Source:  sourcePanel,
		AddedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if !c.upsertAccount(rec) {
		return core.AccountRecord{}, fmt.Errorf("%w: the API key is empty", core.ErrUnsupported)
	}
	c.persist()
	now := c.now()
	records := c.accountRecords(now)
	for _, r := range records {
		if r.ID == id {
			return r, nil
		}
	}
	return core.AccountRecord{ID: id, Label: label, Enabled: rec.Enabled, State: "ready"}, nil
}

// upsertAccount validates and stores one record.
func (c *Client) upsertAccount(rec accountRecord) bool {
	if !rec.credentialPresent() {
		return false
	}
	if rec.ID == "" {
		rec.ID = accountID(keyFingerprint(firstNonEmpty(rec.APIKey, rec.AccessToken)))
	}
	if rec.Source == "" {
		rec.Source = sourceImport
	}
	if rec.Label == "" {
		rec.Label = sourceLabel(rec.Source)
	}
	return c.pool.upsert(rec)
}

// RemoveAccount deletes an account.  An unknown id is an error, not a no-op:
// the panel should learn that its table is stale.
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	c.ensure()
	if !c.pool.remove(id) {
		return fmt.Errorf("account %q not found", id)
	}
	c.persist()
	return nil
}

// SetAccountEnabled flips an account on or off.
//
// Enabling is idempotent but not a no-op: it also clears any parked failure
// state, so an operator can unpark an account whose switch is already on.
// Disabling an account that is already off does nothing.
func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	c.ensure()
	rec, ok := c.pool.byID(id)
	if !ok {
		return fmt.Errorf("account %q not found", id)
	}
	if !enabled && !rec.Enabled {
		return nil
	}
	c.pool.setEnabled(id, enabled)
	c.persist()
	return nil
}

// TestAccount sends one tiny request with the account's own key.
//
// Every outcome other than "no such account" is the answer the operator asked
// for, so it goes into the result rather than into a Go error.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	c.ensure()
	start := c.now()
	finish := func(r core.TestResult) core.TestResult {
		r.AccountID = id
		r.ElapsedMS = c.now().Sub(start).Milliseconds()
		return r
	}

	acct, ok := c.pool.byID(id)
	if !ok {
		return core.TestResult{}, fmt.Errorf("account %q not found", id)
	}
	if !acct.credentialPresent() {
		return finish(core.TestResult{OK: false, Error: "this account has no credential"}), nil
	}

	// Probe with a freshly refreshed OAuth token when one is about to expire.
	if err := c.ensureFreshOAuth(ctx, acct); err != nil {
		return finish(core.TestResult{OK: false, Error: describeError(err)}), nil
	}

	model := c.probeModel(acct)
	probe := &core.ChatRequest{
		Model: model,
		Messages: []core.Message{
			{Role: "user", Content: "Reply with the single word: pong"},
		},
	}
	// The anonymous credential is only served the free catalogue when the
	// request carries the free-tier handshake, so probing it with a bare body
	// would park a healthy account.
	build := buildChatBody
	if acct.authMode() == "anonymous" {
		build = buildFreeChatBody
	}
	body, err := build(c.cfg, probe)
	if err != nil {
		return finish(core.TestResult{OK: false, Model: model, Error: describeError(err)}), nil
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()

	spec := acct.authSpec(requestSpec{
		method: "POST",
		url:    c.cfg.chatURL(),
		body:   body,
		accept: "text/event-stream",
	})
	if acct.authMode() == "anonymous" {
		spec.session = c.freeTierSession()
	}
	resp, err := c.do(ctx, spec)
	if err != nil {
		return finish(core.TestResult{OK: false, Model: model, Error: scrubSecret(describeError(err), acct.secret())}), nil
	}
	if resp.StatusCode != 200 {
		errBody := readLimited(resp.Body, maxErrorBody)
		resp.Body.Close()
		verdict := classify(resp.StatusCode, errBody)
		c.noteFailure(acct, verdict.kind, verdict.text())
		return finish(core.TestResult{
			OK:    false,
			Model: model,
			Error: scrubSecret(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(verdict.text(), 200)), acct.secret()),
		}), nil
	}

	stream := newChatStream(c, resp.Body, acct, ctx, cancel, false)
	text, _, _, _, _, derr := drain(stream)
	if derr != nil {
		msg := scrubSecret(describeError(derr), acct.secret())
		c.noteFailure(acct, kindTransport, msg)
		return finish(core.TestResult{OK: false, Model: model, Error: msg}), nil
	}
	c.noteSuccess(acct)
	reply := strings.TrimSpace(text)
	if reply == "" {
		reply = "(the model returned no text)"
	}
	return finish(core.TestResult{
		OK:    true,
		Model: model,
		Reply: truncate(reply, 200),
	}), nil
}

// RefreshAccount re-imports the credential sources.
//
// There is nothing to refresh on a Zen key itself — no OAuth, no session — so
// this re-reads opencode's auth.json and the import directory, which is what
// picks up a key the operator rotated there.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	c.ensure()
	if _, ok := c.pool.byID(id); !ok {
		return nil, fmt.Errorf("account %q not found", id)
	}

	var found []string
	if p := c.vendorAuthPathFor(); p != "" {
		if res := readVendorAuthFile(p); res.present && len(res.recs) > 0 {
			found = append(found, p)
		}
	}
	for _, dir := range c.discoverDirs() {
		entries, err := filepathGlobJSON(dir)
		if err != nil {
			continue
		}
		found = append(found, entries...)
	}
	if len(found) > 0 {
		if _, err := c.Import(ctx, found, false); err != nil {
			return []core.RefreshResult{{AccountID: id, OK: false, Error: describeError(err)}}, nil
		}
	}

	rec, ok := c.pool.byID(id)
	if !ok {
		return []core.RefreshResult{{
			AccountID: id,
			OK:        false,
			Error:     "not found in any credential source after re-import",
		}}, nil
	}
	return []core.RefreshResult{{
		AccountID: id,
		OK:        rec.APIKey != "",
		Error:     "",
	}}, nil
}

// filepathGlobJSON lists the *.json files in a directory.
func filepathGlobJSON(dir string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	return matches, nil
}

var (
	_ core.AccountManager     = (*Client)(nil)
	_ core.CredentialImporter = (*Client)(nil)
)
