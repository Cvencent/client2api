package cline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// accounts.go is this module's optional panel surface: core.AccountManager,
// core.LoginProvider, core.CredentialImporter, core.BalanceProvider,
// core.ModelRefresher and core.Reviver.
//
// None of it is required by core.Client and none of it is visible to a sibling
// module: the panel discovers these capabilities by type assertion on the
// registered value (see internal/core/accounts.go), so cline can grow account
// management without the gateway or the panel learning about it.

var (
	_ core.AccountManager      = (*Client)(nil)
	_ core.LoginProvider       = (*Client)(nil)
	_ core.CredentialImporter  = (*Client)(nil)
	_ core.BalanceProvider     = (*Client)(nil)
	_ core.ModelRefresher      = (*Client)(nil)
	_ core.Reviver             = (*Client)(nil)
	_ core.ModelLimitsProvider = (*Client)(nil)
)

const (
	// originConfig marks an account the operator pinned in the config file or
	// the environment; those cannot be removed from the panel, because the
	// config is the authority and a restart would resurrect them.
	originConfig = "config"
	// originStored marks an account that lives only in this module's own
	// accounts.json (a device-flow login or a hand-added token).
	originStored = "stored"

	// probeTimeout bounds one TestAccount round trip.  It is shorter than the
	// chat timeout on purpose: a panel button must answer promptly.
	probeTimeout = 30 * time.Second
)

// ---------------------------------------------------------------------------
// core.AccountManager
// ---------------------------------------------------------------------------

// AccountFields describes the "add account" form.  Only the token is required:
// an operator pasting a credential out of Cline's own settings has the token
// and nothing else.
func (c *Client) AccountFields(ctx context.Context) []core.FieldSpec {
	return []core.FieldSpec{
		{
			Key:         "access_token",
			Label:       "Access token",
			Type:        "password",
			Required:    true,
			Placeholder: "workos:eyJ…",
			Help:        "The Cline access token, usually as stored by the client (a workos:-prefixed JWT). The prefix is added if you omit it; it must be present in the Authorization header.",
		},
		{
			Key:         "label",
			Label:       "Label",
			Type:        "text",
			Placeholder: "work laptop",
			Help:        "Shown in the panel instead of the account id. Never sent upstream.",
		},
		{
			Key:   "account_id",
			Label: "Account id",
			Type:  "text",
			Help:  "Optional. Cline's own account id (usr-…). It is NOT the JWT sub (user-…): the balance endpoint rejects the sub.",
		},
		{
			Key:   "nickname",
			Label: "Nickname",
			Type:  "text",
		},
		{
			Key:   "email",
			Label: "Email",
			Type:  "text",
		},
		{
			Key:   "refresh_token",
			Label: "Refresh token",
			Type:  "password",
			Help:  "Optional. With it the module renews the access token by itself; without it the account cannot be renewed and must be signed in again.",
		},
		{
			Key:         "expires_at",
			Label:       "Expires at",
			Type:        "text",
			Placeholder: "2026-09-25T05:23:47Z",
			Help:        "Optional. RFC 3339, \"2006-01-02 15:04:05\", or a unix timestamp. Empty means unknown, not expired.",
		},
	}
}

// Accounts lists every credential the module holds.  It never fails for an
// empty pool: an empty slice is a valid answer and the panel renders it as an
// empty table rather than an error.
func (c *Client) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	records := c.pool.panelRecords()
	configured := configuredIDs(c.cfg)
	for i := range records {
		origin := originStored
		if configured[records[i].ID] {
			origin = originConfig
		}
		records[i].Fields["origin"] = origin
		records[i].Fields["removable"] = origin != originConfig
	}
	return records, nil
}

// AddAccount stores one credential and puts it straight into the pool, so the
// next request can use it without a restart.
func (c *Client) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	token := strings.TrimSpace(spec.Field("access_token"))
	if token == "" {
		return core.AccountRecord{}, errors.New("access_token is required")
	}
	if strings.ContainsAny(token, " \t\r\n") {
		return core.AccountRecord{}, errors.New("access_token must not contain whitespace")
	}
	// The stored value always carries the prefix: it is the prefix the upstream
	// expects in the Authorization header, so normalising it here means the
	// header is right no matter how the operator pasted the token.
	a := account{
		ID:           spec.Field("id"),
		Label:        firstNonEmpty(spec.Field("label"), spec.Label),
		AccessToken:  normalizeToken(token),
		RefreshToken: strings.TrimSpace(spec.Field("refresh_token")),
		AccountID:    spec.Field("account_id"),
		Email:        spec.Field("email"),
		Nickname:     spec.Field("nickname"),
		ExpiresAt:    parseExpiry(spec.Field("expires_at")),
		Note:         firstNonEmpty(spec.Field("label"), spec.Label),
		Disabled:     !spec.EnabledOr(true),
	}
	if a.AccountID == "" {
		a.AccountID = core.JWTClaim(bareToken(a.AccessToken), "clineUserId", "cline_user_id")
	}
	if a.ExpiresAt == 0 {
		a.ExpiresAt = core.JWTExpiry(bareToken(a.AccessToken))
	}

	e := c.pool.put(a)
	// put() merges rather than replaces, and mergeAccount ORs the Disabled
	// flags so a re-add can never quietly re-enable a credential the operator
	// parked.  Honour the form's explicit choice separately.
	c.pool.setEnabled(e.acct.id(), spec.EnabledOr(true))

	recs := c.pool.panelRecords()
	for i := range recs {
		if recs[i].ID == e.acct.id() {
			recs[i].Fields["origin"] = originStored
			recs[i].Fields["removable"] = true
			return recs[i], nil
		}
	}
	return core.AccountRecord{}, fmt.Errorf("account %q vanished after being added", e.acct.id())
}

// RemoveAccount deletes one credential.  A credential the config or the
// environment supplies cannot be deleted here: it would come back on the next
// restart, so the module says so instead of pretending.
func (c *Client) RemoveAccount(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("account id is required")
	}
	if configuredIDs(c.cfg)[id] {
		return fmt.Errorf("account %q comes from the configuration and cannot be removed here; drop it from clients.cline instead", id)
	}
	if !c.pool.remove(id) {
		return fmt.Errorf("account %q not found", id)
	}
	return nil
}

// SetAccountEnabled parks or revives one credential without deleting it.
func (c *Client) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	if id == "" {
		return errors.New("account id is required")
	}
	if !c.pool.setEnabled(id, enabled) {
		return fmt.Errorf("account %q not found", id)
	}
	return nil
}

// ReviveAccount clears every runtime penalty for one account and re-enables it.
//
// To the operator "revive" and "enable" are one intent, so this lifts the park
// and the disable flag together.  It never touches the credential: an account
// whose token is genuinely dead has to fail on its next real request, because
// that failure is the evidence the pool needs.
func (c *Client) ReviveAccount(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("account id is required")
	}
	e := c.pool.find(id)
	if e == nil {
		return fmt.Errorf("account %q not found", id)
	}
	c.pool.revive(e)
	return nil
}

// TestAccount sends one minimum-size real chat completion, pinned to the
// account under test, so a token that opens the gateway but cannot actually
// answer is reported as failed.  A refusal is a result, not an error: the panel
// needs to show *why* a credential is no good, and only an unknown id is a
// programming mistake.
func (c *Client) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	e := c.pool.find(id)
	if e == nil {
		return core.TestResult{}, fmt.Errorf("account %q not found", id)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	started := time.Now()
	acct := c.pool.accountOf(e)
	res := core.TestResult{AccountID: acct.id()}

	model := c.firstModelID()
	reply, err := c.probeChat(ctx, acct, model)
	res.ElapsedMS = time.Since(started).Milliseconds()
	if err != nil {
		msg := cleanErrorText(err.Error())
		c.pool.markFailure(e, kindOfErr(err), msg)
		c.noteUpstream(false, msg)
		res.Error = msg
		return res, nil
	}
	c.pool.markUsed(e)
	c.noteUpstream(true, "")
	res.OK = true
	res.Model = model
	res.Reply = reply
	return res, nil
}

// probeChat sends one minimum-size real completion through the given account
// and returns the first text it produced.
//
// A profile read proves the token is accepted; only a completion proves the
// account can actually answer, which is what the panel's 测试 button asks.
// The account is pinned directly through chatStream so the probe never
// rotates onto a different credential than the one under test.
func (c *Client) probeChat(ctx context.Context, acct account, model string) (string, error) {
	if strings.TrimSpace(model) == "" {
		return "", fmt.Errorf("cline: no model is available to probe with")
	}
	maxTokens := 16
	req := &core.ChatRequest{
		Model:     model,
		Messages:  []core.Message{{Role: "user", Content: "ping"}},
		MaxTokens: &maxTokens,
	}
	body, err := buildBody(req, c.cfg.maxTokens(), c.cfg.reasoningEffort())
	if err != nil {
		return "", err
	}
	resp, err := c.chatStream(ctx, acct, body)
	if err != nil {
		return "", err
	}
	stream := newClineStream(ctx, nil, resp.Body)
	defer stream.Close()

	var reply strings.Builder
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if reply.Len() > 0 {
				return reply.String(), nil
			}
			return "", err
		}
		switch event.Type {
		case core.EventDelta:
			reply.WriteString(event.Delta)
		case core.EventError:
			if event.Err != nil {
				if reply.Len() > 0 {
					return reply.String(), nil
				}
				return "", event.Err
			}
		case core.EventDone:
			// keep draining until the stream really ends
		}
	}
	if reply.Len() == 0 {
		return "", fmt.Errorf("cline: %s answered without any text", model)
	}
	return reply.String(), nil
}

// RefreshAccount renews one credential, or every credential when id is empty.
// It reports per-account outcomes: one account without a refresh token is not a
// reason to fail the whole call.
func (c *Client) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	var targets []*entry
	if id == "" {
		targets = c.pool.all()
	} else {
		e := c.pool.find(id)
		if e == nil {
			return nil, fmt.Errorf("account %q not found", id)
		}
		targets = []*entry{e}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	out := make([]core.RefreshResult, 0, len(targets))
	for _, e := range targets {
		r := core.RefreshResult{AccountID: e.acct.id()}
		switch {
		case c.pool.refreshTokenOf(e) == "":
			r.Error = "no refresh token stored; the device flow must mint a new credential"
		default:
			if err := c.tryRefresh(ctx, e); err != nil {
				r.Error = cleanErrorText(err.Error())
			} else {
				r.OK = true
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// core.BalanceProvider — the account endpoint that needs account_id, not sub
// ---------------------------------------------------------------------------

// AccountBalance asks the vendor what one account has left.
//
// The route is GET /api/v1/users/{account_id}/balance, so the id sent upstream is
// the credential's AccountID (usr-…), never the JWT sub (user_…): the endpoint
// answers 400 {"error":"Invalid request format"} for the sub.  A credential with
// no stored AccountID falls back to the id inside the credential itself, and a
// credential that names no id anywhere is reported as an error rather than
// guessed at.
func (c *Client) AccountBalance(ctx context.Context, id string, soon time.Duration) (core.Balance, error) {
	e := c.pool.find(id)
	if e == nil {
		return core.Balance{}, fmt.Errorf("account %q not found", id)
	}
	acct := c.pool.accountOf(e)
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	bal, err := c.balance(ctx, acct, acct.AccountID)
	if err != nil {
		return core.Balance{}, err
	}
	_ = soon // this vendor publishes one pooled balance, not expiring tranches
	return bal, nil
}

// ---------------------------------------------------------------------------
// core.LoginProvider — the WorkOS device flow, driven from the panel
// ---------------------------------------------------------------------------

// panelLogin is one device authorisation started from the panel.  It is
// deliberately in-memory: a restart drops the flow and the operator simply
// starts another one.  The URL is also mirrored to login.json, which is what
// Status reads to say "open this URL".
type panelLogin struct {
	deviceCode string
	url        string
	userCode   string
	startedAt  time.Time
	expiresAt  time.Time
	cancel     context.CancelFunc

	mu        sync.Mutex
	state     string
	message   string
	accountID string
}

func (l *panelLogin) set(state, message, accountID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.state = state
	l.message = message
	if accountID != "" {
		l.accountID = accountID
	}
}

func (l *panelLogin) snapshot() core.LoginState {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := core.LoginState{
		SessionID: l.deviceCode,
		State:     l.state,
		URL:       l.url,
		Code:      l.userCode,
		Message:   l.message,
		AccountID: l.accountID,
	}
	if l.state == core.LoginPending && time.Now().After(l.expiresAt) {
		st.State = core.LoginFailed
		st.Message = "the authorisation window expired; start again"
	}
	return st
}

// StartLogin begins a device authorisation and polls in the background until
// the human approves or the window closes.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	if c.accountsPath == "" {
		return core.LoginState{}, errors.New("no data directory: a credential could not be stored")
	}
	// An already-usable account must not block an explicit sign-in: the
	// operator pressed the button, so run the flow.  Authorising the same
	// vendor account updates that record (the pool keys on account id); a
	// different one adds a second.
	haveUsable := c.pool.ready()

	runCtx, cancel := context.WithTimeout(context.Background(), c.cfg.loginTimeout())
	rec, err := c.startDeviceAuth(runCtx)
	if err != nil {
		cancel()
		return core.LoginState{}, err
	}

	sess := &panelLogin{
		deviceCode: rec.DeviceCode,
		url:        rec.URL,
		userCode:   rec.UserCode,
		startedAt:  time.Now(),
		expiresAt:  time.Now().Add(c.cfg.loginTimeout()),
		cancel:     cancel,
		state:      core.LoginPending,
		message:    "open the URL in a browser and approve access",
	}
	if haveUsable {
		sess.message += " (an account is already usable; approving the same vendor account updates it, a different one adds a second)"
	}
	c.putLogin(sess)

	// Mirror the pending flow where Status already looks for it, so the client
	// card and the account screen tell the operator the same thing.
	if c.loginPath != "" {
		if err := core.WriteJSONAtomic(c.loginPath, rec); err != nil {
			c.deps.Log("cline: writing the login file: %v", err)
		}
	}

	core.GoSafe("cline panel login", func(msg string) { c.deps.Log("cline: %s", msg) },
		func() { c.runPanelLogin(runCtx, sess) })
	return sess.snapshot(), nil
}

// runPanelLogin polls the vendor until the human approves, the window closes or
// CancelLogin stops it, then records the outcome on the session.
func (c *Client) runPanelLogin(ctx context.Context, sess *panelLogin) {
	defer sess.cancel()
	interval := c.cfg.pollInterval()
	for {
		tokens, pending, err := c.pollDeviceAuth(ctx, sess.deviceCode)
		if err != nil {
			// A single failed poll is not fatal: the human may still be
			// clicking through the consent screen.
			c.deps.Log("cline: panel login poll: %v", scrubError(err))
		}
		if !pending && err == nil {
			grant, regErr := c.register(ctx, tokens.AccessToken, tokens.RefreshToken)
			if regErr != nil {
				sess.set(core.LoginFailed, "approved, but the Cline token could not be minted: "+cleanErrorText(regErr.Error()), "")
				return
			}
			acct := accountFromGrant(account{CreatedAt: time.Now().Unix()}, grant)
			if err := upsertStoredAccount(c.accountsPath, acct); err != nil {
				sess.set(core.LoginFailed, "approved, but the credential could not be stored: "+err.Error(), "")
				return
			}
			c.reloadAccounts()
			c.removeLoginFile()
			sess.set(core.LoginSuccess, "approved; the credential is stored and live", acct.id())
			return
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if errors.Is(ctx.Err(), context.Canceled) {
				sess.set(core.LoginCancelled, "cancelled", "")
			} else {
				sess.set(core.LoginFailed, "authorisation was not completed in time", "")
			}
			return
		case <-timer.C:
		}
	}
}

// PollLogin reports the state of one panel login.
func (c *Client) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	if sessionID == "" {
		return core.LoginState{}, errors.New("session id is required")
	}
	sess := c.loginByID(sessionID)
	if sess == nil {
		return core.LoginState{}, fmt.Errorf("login session %q not found", sessionID)
	}
	return sess.snapshot(), nil
}

// CancelLogin stops a pending flow.  It never logs anything out: only the
// vendor's consent screen can grant a credential, so abandoning the poll
// simply leaves no credential behind.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	sess := c.loginByID(sessionID)
	if sess == nil {
		return fmt.Errorf("login session %q not found", sessionID)
	}
	sess.cancel()
	sess.set(core.LoginCancelled, "cancelled", "")
	c.removeLoginFile()
	c.forgetLogin(sessionID)
	return nil
}

// ---------------------------------------------------------------------------
// core.CredentialImporter — Cline's own on-disk settings
// ---------------------------------------------------------------------------

// Discover looks for a credential Cline itself has already stored.
//
// On a machine where Cline is installed the client keeps its provider settings
// in ~/.cline/data/settings/providers.json; where it is not installed the file
// simply does not exist and this reports an empty list rather than an error,
// because "no discoverable credential" is a fact, not a failure.  Nothing here
// reads a secret out of the vendor's keychain: only the plain JSON file.
func (c *Client) Discover(ctx context.Context) ([]core.DiscoveredCredential, error) {
	paths := clineSettingsPaths()
	out := make([]core.DiscoveredCredential, 0, len(paths))
	for _, p := range paths {
		rec := core.DiscoveredCredential{
			Path: p,
			Kind: "cline-settings",
		}
		raw, err := os.ReadFile(p)
		switch {
		case err == nil:
			fields := credentialFields(raw)
			rec.Note = "Cline provider settings"
			if fields["access_token"] == "" {
				rec.Note += " (no access token found)"
				break
			}
			rec.Label = labelFromToken(fields["access_token"])
			rec.Importable = true
			if fields["refresh_token"] == "" {
				rec.Note += " (no refresh token: the account cannot be renewed)"
			}
		case errors.Is(err, os.ErrNotExist):
			rec.Note = "not present"
		default:
			rec.Note = "unreadable: " + err.Error()
		}
		out = append(out, rec)
	}
	return out, nil
}

// Import copies one or more discovered credentials into this module's store.
// With all true it imports everything Discover marked importable.  A named path
// that holds no token is an error: the operator asked for something specific
// and silently importing nothing would be a lie.
func (c *Client) Import(ctx context.Context, paths []string, all bool) ([]core.AccountRecord, error) {
	if len(paths) == 0 && !all {
		return nil, nil
	}
	if all {
		found, err := c.Discover(ctx)
		if err != nil {
			return nil, err
		}
		for _, d := range found {
			if d.Importable {
				paths = append(paths, d.Path)
			}
		}
	}
	out := make([]core.AccountRecord, 0, len(paths))
	var failures []string
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		fields := credentialFields(raw)
		if fields["access_token"] == "" {
			failures = append(failures, p+": no access token found")
			continue
		}
		rec, err := c.AddAccount(ctx, core.AccountSpec{Fields: fields})
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		out = append(out, rec)
	}
	if len(out) == 0 && len(failures) > 0 {
		return nil, errors.New("nothing imported: " + strings.Join(failures, "; "))
	}
	return out, nil
}

// clineSettingsPaths returns the candidate settings locations, most specific
// first.  The home directory is resolved through os.UserHomeDir so a test can
// point HOME elsewhere and never touch a real profile.
func clineSettingsPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".cline", "data", "settings", "providers.json"),
	}
}

// findAccessToken digs an access token out of Cline's provider settings without
// assuming the exact schema: the file is the vendor's private business and this
// module only needs the one string out of it.
func findAccessToken(raw []byte) string {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return ""
	}
	return tokenFromAny(doc, 0)
}

// clineAuth is the credential block Cline's own settings file carries under
// providers.<name>.settings.auth.
type clineAuth struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"` // milliseconds since the epoch
	AccountID    string `json:"accountId"`
	Metadata     struct {
		UserInfo struct {
			ClineUserID string `json:"clineUserId"`
			Email       string `json:"email"`
			Name        string `json:"name"`
		} `json:"userInfo"`
	} `json:"metadata"`
}

// findAuth pulls Cline's structured credential block out of a settings file.
// It walks the document for the first object carrying a string accessToken and
// decodes that object, so the layout above it may change without breaking the
// import.
func findAuth(raw []byte) (clineAuth, bool) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return clineAuth{}, false
	}
	obj := authObjectFromAny(doc, 0)
	if obj == nil {
		return clineAuth{}, false
	}
	blob, err := json.Marshal(obj)
	if err != nil {
		return clineAuth{}, false
	}
	var a clineAuth
	if err := json.Unmarshal(blob, &a); err != nil {
		return clineAuth{}, false
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return clineAuth{}, false
	}
	return a, true
}

// authObjectFromAny returns the first nested object that looks like Cline's
// auth block.  Keys are walked in sorted order so a file holding several
// providers is read the same way twice.
func authObjectFromAny(v any, depth int) map[string]any {
	if depth > 8 {
		return nil
	}
	switch t := v.(type) {
	case map[string]any:
		if s, ok := t["accessToken"].(string); ok && strings.TrimSpace(s) != "" {
			return t
		}
		for _, k := range sortedKeys(t) {
			if got := authObjectFromAny(t[k], depth+1); got != nil {
				return got
			}
		}
	case []any:
		for _, item := range t {
			if got := authObjectFromAny(item, depth+1); got != nil {
				return got
			}
		}
	}
	return nil
}

// credentialFields turns a Cline settings file into the account fields this
// module stores.
//
// The structured auth block is preferred whenever the file has one, because it
// carries the refresh token and the expiry beside the access token.  Reading
// only the token -- which is what this module used to do -- imports an account
// that looks healthy and can never be renewed: Cline's access tokens last about
// an hour, and once one lapses there is nothing left to refresh it from, so the
// account is stuck until an operator signs the vendor client in again.
//
// The generic token walk stays as the fallback for a file whose shape is not
// Cline's own: a hand-made credential file, or a fixture.
func credentialFields(raw []byte) map[string]string {
	if a, ok := findAuth(raw); ok {
		fields := map[string]string{
			"access_token": normalizeToken(a.AccessToken),
			"label":        "imported from Cline",
		}
		// The account id is NOT the JWT sub: the balance endpoint rejects the
		// sub and wants usr-….
		if id := firstNonEmpty(a.AccountID, a.Metadata.UserInfo.ClineUserID); id != "" {
			fields["account_id"] = id
		}
		if rt := strings.TrimSpace(a.RefreshToken); rt != "" {
			fields["refresh_token"] = rt
		}
		if a.ExpiresAt > 0 {
			// parseExpiry reads a value above 1e12 as milliseconds.
			fields["expires_at"] = strconv.FormatInt(a.ExpiresAt, 10)
		}
		if e := strings.TrimSpace(a.Metadata.UserInfo.Email); e != "" {
			fields["email"] = e
		}
		if n := strings.TrimSpace(a.Metadata.UserInfo.Name); n != "" {
			fields["nickname"] = n
		}
		return fields
	}
	tok := findAccessToken(raw)
	if tok == "" {
		return map[string]string{}
	}
	return map[string]string{
		"access_token": normalizeToken(tok),
		"label":        "imported from Cline",
		"account_id":   core.JWTClaim(bareToken(tok), "clineUserId", "cline_user_id"),
	}
}

// tokenFromAny walks a decoded JSON document looking for the first string that
// reads like a Cline access token.
func tokenFromAny(v any, depth int) string {
	if depth > 8 {
		return ""
	}
	switch t := v.(type) {
	case string:
		if looksLikeAccessToken(t) {
			return t
		}
	case []any:
		for _, item := range t {
			if got := tokenFromAny(item, depth+1); got != "" {
				return got
			}
		}
	case map[string]any:
		// Prefer an explicitly named key before walking everything.
		for _, k := range []string{"accessToken", "access_token", "clineAccessToken", "token"} {
			if s, ok := t[k].(string); ok && looksLikeAccessToken(s) {
				return s
			}
		}
		for _, k := range sortedKeys(t) {
			if got := tokenFromAny(t[k], depth+1); got != "" {
				return got
			}
		}
	}
	return ""
}

// looksLikeAccessToken is deliberately loose: the on-disk value is a
// workos:-prefixed JWT, but a token pasted from elsewhere may be a bare JWT.
func looksLikeAccessToken(s string) bool {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, tokenPrefix) {
		return true
	}
	return strings.HasPrefix(s, "eyJ") && strings.Count(s, ".") == 2 && len(s) > 40
}

// labelFromToken renders a human label for a discovered token.
func labelFromToken(tok string) string {
	if sub := core.JWTClaim(bareToken(tok), "clineUserId", "cline_user_id"); sub != "" {
		return sub
	}
	return "Cline account"
}

// sortedKeys returns a map's keys in a deterministic order, so discovery walks
// a document the same way twice.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// panel-login bookkeeping
// ---------------------------------------------------------------------------

func (c *Client) putLogin(sess *panelLogin) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	c.panelLogins = map[string]*panelLogin{sess.deviceCode: sess}
}

func (c *Client) loginByID(id string) *panelLogin {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.panelLogins[id]
}

func (c *Client) forgetLogin(id string) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	delete(c.panelLogins, id)
}

func (c *Client) removeLoginFile() {
	if c.loginPath == "" {
		return
	}
	if err := os.Remove(c.loginPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		c.deps.Log("cline: removing the login file: %v", err)
	}
}

// ---------------------------------------------------------------------------
// pool helpers used by the panel surface
// ---------------------------------------------------------------------------

// panelRecords renders every entry, including parked and disabled ones, so an
// operator can always see and revive what they turned off.
func (p *pool) panelRecords() []core.AccountRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	out := make([]core.AccountRecord, 0, len(p.entries))
	for _, e := range p.entries {
		rec := core.AccountRecord{
			ID:      e.acct.id(),
			Label:   e.acct.label(),
			Enabled: !e.acct.Disabled,
			State:   p.stateLocked(e, now),
			Note:    core.Redact(e.note),
			Fields: map[string]any{
				"failures": e.fails,
			},
		}
		if e.acct.AccountID != "" {
			rec.Fields["account_id"] = e.acct.AccountID
		}
		if e.acct.Nickname != "" {
			rec.Fields["nickname"] = e.acct.Nickname
		}
		if e.acct.Email != "" {
			rec.Fields["email"] = e.acct.Email
		}
		if e.acct.RefreshToken != "" {
			rec.Fields["refreshable"] = true
		}
		if e.acct.ExpiresAt > 0 {
			expiry := time.Unix(e.acct.ExpiresAt, 0).UTC()
			rec.ExpiresAt = expiry.Format(time.RFC3339)
			rec.Fields["expires_in"] = int64(time.Until(expiry).Seconds())
		}
		if e.until.After(now) {
			rec.Fields["cooldown_until"] = e.until.UTC().Format(time.RFC3339)
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// find returns the live entry for an id, or nil.
func (p *pool) find(id string) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.acct.id() == id {
			return e
		}
	}
	return nil
}

// all returns every entry in a stable order.
func (p *pool) all() []*entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*entry, len(p.entries))
	copy(out, p.entries)
	sort.Slice(out, func(i, j int) bool { return out[i].acct.id() < out[j].acct.id() })
	return out
}

// accountOf copies one entry's credential out from under the lock.
func (p *pool) accountOf(e *entry) account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return e.acct
}

// refreshTokenOf reads one entry's refresh token, or "".
func (p *pool) refreshTokenOf(e *entry) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return e.acct.RefreshToken
}

// configuredIDs returns the set of account ids the config and environment
// supply, so the panel can tell a pinned credential from a stored one.
func configuredIDs(cfg config) map[string]bool {
	out := map[string]bool{}
	for _, a := range configuredAccounts(cfg) {
		out[a.id()] = true
	}
	return out
}

// firstModelID names a model the account could reach, for TestResult.
func (c *Client) firstModelID() string {
	models := c.models.snapshot()
	if len(models) == 0 {
		models = fallbackCatalog()
	}
	if len(models) == 0 {
		return ""
	}
	return models[0].ID
}
