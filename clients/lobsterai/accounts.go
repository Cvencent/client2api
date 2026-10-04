package lobsterai

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// credentialsPath is where the account store lives.  An empty DataDir means
// "do not persist": writing a credential file into the process working
// directory would be worse than losing the accounts.
func (c *Client) credentialsPath() string {
	dir := strings.TrimSpace(c.deps.DataDir)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, credentialsFile)
}

// persist writes the account store back to disk.
//
// A failure is recorded, never returned: an unwritable data directory must not
// turn a chat that already succeeded into an error.
func (c *Client) persist() {
	path := c.credentialsPath()
	if path == "" {
		return
	}
	if err := saveCredentials(path, c.pool.snapshot()); err != nil {
		c.noteError("saving accounts: " + err.Error())
	}
}

// --- core.AccountManager ----------------------------------------------------

// AccountFields describes the manual-add form.
//
// The normal way to get an account here is the browser sign-in, so every field
// is optional; they exist so an operator holding a token from elsewhere can add
// it by hand.  The three keyfrom fields are asked for because a hand-added
// account cannot renew without them: the refresh call replays the values that
// were issued at login, and a token pasted without them stops working when it
// expires.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{Key: "access_token", Label: "Access token", Type: "password", Required: true,
			Help: "The token issued by the LobsterAI login. Prefer the sign-in button; this form is for a token you already have."},
		{Key: "refresh_token", Label: "Refresh token", Type: "password",
			Help: "Needed for the account to renew itself."},
		{Key: "uid", Label: "Account id", Type: "text",
			Help: "Optional. Derived from the token when left empty."},
		{Key: "user_id", Label: "User id", Type: "text",
			Help: "Optional. Replayed on renewal when the login response carried one."},
		{Key: "uuid", Label: "Device uuid", Type: "text",
			Help: "Optional. Replayed on renewal; a hand-added account without it may fail to renew."},
		{Key: "first_keyfrom", Label: "First keyfrom", Type: "text",
			Help: "Optional. Issued at login and replayed on renewal."},
		{Key: "latest_keyfrom", Label: "Latest keyfrom", Type: "text",
			Help: "Optional. Issued at login and replayed on renewal."},
		{Key: "label", Label: "Label", Type: "text",
			Help: "Optional. A name for the panel."},
	}
}

// accountRecords projects the pool into the panel's non-secret view.
func (c *Client) accountRecords(now time.Time) []core.AccountRecord {
	recs := c.pool.snapshot()
	out := make([]core.AccountRecord, 0, len(recs))
	for i := range recs {
		a := recs[i]
		fields := map[string]any{
			"uid":               a.UID,
			"nickname":          a.Nickname,
			"expires_at":        a.ExpiresAt,
			"has_refresh_token": a.RefreshToken != "",
		}
		out = append(out, core.AccountRecord{
			ID:        a.ID,
			Label:     firstNonEmpty(a.Label, a.Nickname, a.UID),
			Enabled:   a.Enabled,
			State:     stateOf(&a, now),
			ExpiresAt: a.ExpiresAt,
			Note:      noteOf(&a, now),
			Fields:    fields,
			Identity:  a.UID,
		})
	}
	return out
}

// Accounts lists the configured accounts.  It never returns a token.
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	return c.accountRecords(c.now()), nil
}

// AddAccount stores a hand-supplied credential.
//
// The credential is verified only if the operator presses Test; adding it must
// not depend on the upstream being reachable.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	token := spec.Field("access_token")
	if token == "" {
		return core.AccountRecord{}, fmt.Errorf("%w: an access token is required (use the sign-in button to obtain one)", core.ErrUnsupported)
	}
	uid := spec.Field("uid")
	if uid == "" {
		uid = tokenFingerprint(token)
	}
	rec := accountFromConfig(AccountConfig{
		Label:         spec.Field("label"),
		UID:           uid,
		UserID:        spec.Field("user_id"),
		UUID:          spec.Field("uuid"),
		AccessToken:   token,
		RefreshToken:  spec.Field("refresh_token"),
		FirstKeyfrom:  spec.Field("first_keyfrom"),
		LatestKeyfrom: spec.Field("latest_keyfrom"),
	}, c.now())
	if spec.Enabled != nil {
		rec.Enabled = *spec.Enabled
	}
	if err := c.upsertAccount(rec); err != nil {
		return core.AccountRecord{}, err
	}
	now := c.now()
	for _, out := range c.accountRecords(now) {
		if out.ID == rec.ID {
			return out, nil
		}
	}
	return core.AccountRecord{}, fmt.Errorf("account %q vanished right after it was stored", rec.ID)
}

// upsertAccount stores a record and persists the store.
func (c *Client) upsertAccount(rec accountRecord) error {
	if strings.TrimSpace(rec.ID) == "" {
		return fmt.Errorf("%w: an account needs an id", core.ErrUnsupported)
	}
	if rec.AccessToken == "" && rec.RefreshToken == "" {
		return fmt.Errorf("%w: an account needs at least one token", core.ErrUnsupported)
	}
	c.pool.upsert(rec)
	c.persist()
	return nil
}

func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	if !c.pool.remove(id) {
		return fmt.Errorf("account %q not found", id)
	}
	c.persist()
	return nil
}

func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	if !c.pool.setEnabled(id, enabled) {
		if c.pool.byID(id) == nil {
			return fmt.Errorf("account %q not found", id)
		}
		return nil
	}
	c.persist()
	return nil
}

// TestAccount sends one tiny completion through this exact account.
//
// Every failure is reported inside the result, because "the account is dead" is
// the answer the operator asked for, not a Go error.  A Go error is reserved for
// an account that does not exist.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	acct := c.pool.byID(id)
	if acct == nil {
		return core.TestResult{}, fmt.Errorf("account %q not found", id)
	}
	start := c.now()
	finish := func(r core.TestResult) core.TestResult {
		r.ElapsedMS = c.now().Sub(start).Milliseconds()
		return r
	}
	res := core.TestResult{AccountID: id}

	models := c.modelIDs()
	if len(models) == 0 {
		res.Error = "no model is known yet"
		return finish(res), nil
	}
	res.Model = models[0]

	ctx, cancel := context.WithTimeout(ctx, c.cfg.chatTimeout())
	defer cancel()
	if err := c.ensureFresh(ctx, acct); err != nil {
		res.Error = c.describeError(err)
		return finish(res), nil
	}
	probe := &core.ChatRequest{
		Model:     res.Model,
		Stream:    true,
		MaxTokens: intPtr(16),
		Messages:  []core.Message{{Role: "user", Content: "ping"}},
	}
	stream, err := c.openChatFor(ctx, acct, probe)
	if err != nil {
		res.Error = c.describeError(err)
		return finish(res), nil
	}
	text, _, _, _, _, err := drainSSE(stream)
	if err != nil {
		res.Error = c.describeError(err)
		return finish(res), nil
	}
	res.OK = true
	res.Reply = truncate(strings.TrimSpace(text), 200)
	return finish(res), nil
}

// RefreshAccount renews one account, or every account when id is empty.
//
// It always returns one result per attempted account and a Go error only for an
// unknown id, matching the AccountManager contract.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	var targets []accountRecord
	if strings.TrimSpace(id) == "" {
		targets = c.pool.snapshot()
	} else {
		acct := c.pool.byID(id)
		if acct == nil {
			return nil, fmt.Errorf("account %q not found", id)
		}
		targets = append(targets, *acct)
	}
	results := make([]core.RefreshResult, 0, len(targets))
	for i := range targets {
		acct := targets[i]
		res := core.RefreshResult{AccountID: acct.ID}
		switch {
		case strings.TrimSpace(acct.RefreshToken) == "":
			res.Error = "no refresh token is stored; sign in again"
		default:
			if err := c.refreshCredential(ctx, &acct); err != nil {
				res.Error = c.describeError(err)
				break
			}
			res.OK = true
		}
		results = append(results, res)
	}
	c.persist()
	return results, nil
}

// openChatFor opens a stream through one named account, without touching the
// pool's in-flight accounting or rotating.
func (c *Client) openChatFor(ctx context.Context, acct *accountRecord, req *core.ChatRequest) (core.Stream, error) {
	body, err := buildChatBody(c.cfg, req)
	if err != nil {
		return nil, err
	}
	version := c.clientVersion(ctx)
	spec := requestSpec{
		method:  http.MethodPost,
		url:     c.cfg.chatURL(),
		body:    body,
		bearer:  acct.AccessToken,
		version: version,
		accept:  "text/event-stream, application/json",
		timeout: c.cfg.chatTimeout(),
	}
	resp, err := c.do(ctx, spec)
	if err != nil {
		return nil, c.classifyErrFor(acct, err)
	}
	if resp.StatusCode != http.StatusOK {
		errBody := readLimited(resp.Body, maxErrorBody)
		resp.Body.Close()
		return nil, c.classifyHTTP("chat", acct.ID, resp.StatusCode, errBody)
	}
	return newChatStream(c, resp, acct, func() {}, !req.Stream), nil
}

func intPtr(v int) *int { return &v }

// --- core.CredentialImporter ------------------------------------------------

// discoverDirs are the places a credential file may be dropped for import.
//
// LobsterAI has no CLI that writes a credential file this module could find, so
// the importer is deliberately narrow: the module's own data directory, and an
// "import" subdirectory inside it.  Guessing at another product's private
// directory would be a lie dressed as a feature.
func (c *Client) discoverDirs() []string {
	dir := strings.TrimSpace(c.deps.DataDir)
	if dir == "" {
		return nil
	}
	return []string{dir, filepath.Join(dir, "import")}
}

// Discover reports the credential files it can see.  It is best effort: an
// unreadable directory is skipped, not reported as a failure.
func (c *Client) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	known := map[string]bool{}
	for _, rec := range c.pool.snapshot() {
		known[rec.UID] = true
	}
	var out []core.DiscoveredCredential
	seen := map[string]bool{}
	for _, dir := range c.discoverDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if seen[path] {
				continue
			}
			seen[path] = true
			raw, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			accounts, err := decodeCredentialsBody(raw)
			if err != nil || len(accounts) == 0 {
				continue
			}
			label := accounts[0].Label
			if label == "" {
				label = firstNonEmpty(accounts[0].Nickname, accounts[0].UID)
			}
			imported := true
			for _, acct := range accounts {
				uid := acct.UID
				if uid == "" {
					uid = tokenFingerprint(acct.AccessToken)
				}
				if !known[uid] {
					imported = false
				}
			}
			out = append(out, core.DiscoveredCredential{
				Path:       path,
				Kind:       "lobsterai-credential",
				Label:      firstNonEmpty(label, entry.Name()),
				Note:       fmt.Sprintf("%d account(s) in this file", len(accounts)),
				Importable: true,
				Imported:   imported,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Import loads credentials from the given paths, or from everything Discover
// found when all is true.
//
// Naming a path explicitly returns that path's real error; a sweep skips a
// broken file, because one unreadable leftover must not hide the good ones.
func (c *Client) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	if all {
		found, err := c.Discover(ctx)
		if err != nil {
			return nil, err
		}
		paths = nil
		for _, d := range found {
			paths = append(paths, d.Path)
		}
	}
	explicit := !all
	var imported []accountRecord
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			if explicit {
				return nil, err
			}
			c.noteError("importing " + path + ": " + err.Error())
			continue
		}
		accounts, err := decodeCredentialsBody(raw)
		if err != nil {
			if explicit {
				return nil, fmt.Errorf("importing %s: %w", path, err)
			}
			c.noteError("importing " + path + ": " + err.Error())
			continue
		}
		for _, cfg := range accounts {
			rec := accountFromConfig(cfg, c.now())
			if rec.AccessToken == "" && rec.RefreshToken == "" {
				continue
			}
			if err := c.upsertAccount(rec); err != nil {
				if explicit {
					return nil, err
				}
				c.noteError("importing " + path + ": " + err.Error())
				continue
			}
			imported = append(imported, rec)
		}
	}
	now := c.now()
	byID := map[string]core.AccountRecord{}
	for _, rec := range c.accountRecords(now) {
		byID[rec.ID] = rec
	}
	out := make([]core.AccountRecord, 0, len(imported))
	for _, rec := range imported {
		if view, ok := byID[rec.ID]; ok {
			out = append(out, view)
		}
	}
	return out, nil
}

// BundleImporter support: the vendor hands out a single JSON credential file, so
// a bundle is just one of those.
func (c *Client) ImportBundle(ctx context.Context, name string, data []byte) (core.BundleImportReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	report := core.BundleImportReport{}
	accounts, err := decodeCredentialsBody(data)
	if err != nil {
		report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", name, err))
		return report, nil
	}
	report.Total = len(accounts)
	for _, cfg := range accounts {
		rec := accountFromConfig(cfg, c.now())
		if rec.AccessToken == "" && rec.RefreshToken == "" {
			report.Skipped++
			continue
		}
		if err := c.upsertAccount(rec); err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		report.Imported++
	}
	return report, nil
}

var (
	_ core.AccountManager     = (*Client)(nil)
	_ core.CredentialImporter = (*Client)(nil)
	_ core.BundleImporter     = (*Client)(nil)
)
