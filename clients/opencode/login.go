package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

const (
	// oauthClientID is the public client id the console's device flow expects.
	oauthClientID = "opencode-cli"
	// oauthDeviceGrant is the RFC 8628 device-code grant type.
	oauthDeviceGrant = "urn:ietf:params:oauth:grant-type:device_code"
	// oauthRefreshSkew refreshes an access token five minutes before it expires,
	// matching the vendor's own CLI.
	oauthRefreshSkew = 5 * time.Minute
	// loginMaxBody bounds every console body this module will read.
	loginMaxBody = 1 << 20
	// defaultDeviceInterval is used when the console omits `interval`.
	defaultDeviceInterval = 5 * time.Second
	// maxDeviceInterval caps the slow_down backoff.
	maxDeviceInterval = 60 * time.Second
	// loginDefaultTTL is the fallback device-code lifetime.
	loginDefaultTTL = 10 * time.Minute
)

// ---------------------------------------------------------------------------
// Panel login.
//
// Two realms, one contract:
//
//   - "free" adds the anonymous credential (x-api-key: public) that OpenCode
//     Zen serves a small set of zero-cost models to without any account.
//   - "oauth" runs the OpenCode Console device-code flow and stores the
//     resulting access/refresh tokens.
//
// The realm plumbing is deliberately the same shape as workbuddy's: the module
// owns its sessions, the panel owns the polling cadence, and a finished login
// lands on the same account pool a pasted key does.
// ---------------------------------------------------------------------------

// Login realms.
const (
	realmFree  = "free"
	realmOAuth = "oauth"
)

// anonymousAccountID is the stable id of the single anonymous account.  It is
// fixed rather than fingerprinted so repeated "add free account" clicks update
// one row instead of stacking duplicates.
const anonymousAccountID = clientName + ":anonymous"

// freeModelAllowlist is the set of ids Zen publishes at zero cost.  The
// anonymous credential can reach all of them once a request carries the
// free-tier handshake (see freeTierTools in body.go), so this mirrors the live
// catalogue's zero-cost entries rather than a single fixed id.  A few of them
// are region-gated or momentarily without an upstream, which the operator can
// blacklist on the platform page; the first entry is the one the account test
// probes, so it stays one that answers everywhere.
var freeModelAllowlist = []string{
	"space-bunny-free",
	"big-pickle",
	"fledge-alpha-free",
	"longcat-2.5-preview-free",
	"mimo-v2.5-free",
	"mimo-v2.6-flash-free",
	"nemotron-3-ultra-free",
	"nemotron-3.5-lightning-free",
	"ling-3.0-flash-fin-free",
	"deepseek-v4-flash-free",
	"jev-1.13-free",
	"muse-spark-1.2-contributor-free",
	"muse-spark-1.3-contributor-free",
}

// normalizeLoginRealm folds the config vocabulary onto the two realms this
// module serves.  Anything unknown (including "") is free, which is the realm
// that needs no account.
func normalizeLoginRealm(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case realmOAuth, "console", "device", "device-code":
		return realmOAuth
	default:
		return realmFree
	}
}

// core.RealmLoginProvider is what the panel drives: "add an account" is not a
// single flow here, because an anonymous free credential and a signed-in
// Console account are different things.
var _ core.RealmLoginProvider = (*Client)(nil)

// StartLogin starts a login on the module's configured default realm.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	return c.StartLoginRealm(ctx, c.cfg.defaultRealm())
}

// LoginRealms lists the realms this module can add an account to.
func (c *Client) LoginRealms(context.Context) []core.LoginRealm {
	return []core.LoginRealm{
		{Code: realmFree, Name: "免费模式", Help: "OpenCode Zen 匿名免费模型，无需账号"},
		{Code: realmOAuth, Name: "OpenCode Console", Help: "设备码登录，使用你自己的 Console 账号"},
	}
}

// StartLoginRealm starts a login pinned to one realm.  An empty realm asks for
// the module's configured default.
func (c *Client) StartLoginRealm(ctx context.Context, want string) (core.LoginState, error) {
	c.ensure()
	realm := normalizeLoginRealm(firstNonEmpty(strings.TrimSpace(want), c.cfg.defaultRealm()))
	if realm == realmOAuth {
		return c.startOAuthLogin(ctx)
	}
	return c.startAnonymousLogin()
}

// startAnonymousLogin stores the one anonymous credential.  It is idempotent:
// the id is fixed, so a second click updates the same row.
func (c *Client) startAnonymousLogin() (core.LoginState, error) {
	rec := accountRecord{
		ID:       anonymousAccountID,
		Label:    "OpenCode Free",
		AuthMode: "anonymous",
		APIKey:   "public",
		Enabled:  true,
		Source:   sourcePanel,
	}
	if !c.upsertAccount(rec) {
		return core.LoginState{}, fmt.Errorf("%w: the anonymous credential could not be stored", core.ErrUnsupported)
	}
	// Re-adding is the operator's recovery path for an account a vendor
	// refusal parked or disabled, so clear that state instead of leaving a
	// stale "cooling" note on a credential that was just re-supplied.
	c.pool.setEnabled(anonymousAccountID, true)
	c.persist()
	return core.LoginState{
		State:     core.LoginSuccess,
		AccountID: anonymousAccountID,
		Realm:     realmFree,
		Message:   "已添加匿名免费账号，可使用免费模型",
	}, nil
}

// ---------------------------------------------------------------------------
// Device-code session bookkeeping.
// ---------------------------------------------------------------------------

// loginSession is one in-flight device-code authorisation.
type loginSession struct {
	mu         sync.Mutex
	now        func() time.Time
	sessionID  string
	realm      string
	url        string
	code       string
	deviceCode string
	interval   time.Duration
	expiresAt  time.Time

	state     string
	message   string
	accountID string

	// polling guards against two panel polls racing into the vendor at once.
	polling bool
	// cancelled is set by CancelLogin and checked while holding mu.
	cancelled bool
}

// terminal reports whether the session has reached a state it can never leave.
func (l *loginSession) terminal() bool {
	switch l.state {
	case core.LoginSuccess, core.LoginFailed, core.LoginCancelled:
		return true
	}
	return false
}

// set records progress, but never resurrects a finished session.
func (l *loginSession) set(state, message, accountID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.terminal() {
		return
	}
	l.state = state
	l.message = core.Redact(message)
	if accountID != "" {
		l.accountID = accountID
	}
}

// snapshot renders the session for the panel.  A pending session whose window
// has closed becomes failed here rather than hanging forever.
func (l *loginSession) snapshot() core.LoginState {
	l.mu.Lock()
	defer l.mu.Unlock()
	state, message := l.state, l.message
	if state == core.LoginPending && l.clock().After(l.expiresAt) {
		state, message = core.LoginFailed, "the device code expired; start again"
		l.state, l.message = state, message
	}
	return core.LoginState{
		SessionID: l.sessionID,
		State:     state,
		URL:       l.url,
		Code:      l.code,
		Message:   core.Redact(message),
		AccountID: l.accountID,
		Realm:     l.realm,
	}
}

func (l *loginSession) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// markCancelled stops the session for good.
func (l *loginSession) markCancelled() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cancelled = true
	l.state = core.LoginCancelled
	l.message = "cancelled"
	l.accountID = ""
}

// beginPoll claims the right to talk to the vendor, or reports that another
// poll, a cancel or a finished session already owns the session.
func (l *loginSession) beginPoll() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.polling || l.cancelled || l.terminal() {
		return false
	}
	if l.clock().After(l.expiresAt) {
		l.state, l.message = core.LoginFailed, "the device code expired; start again"
		return false
	}
	l.polling = true
	return true
}

func (l *loginSession) endPoll() {
	l.mu.Lock()
	l.polling = false
	l.mu.Unlock()
}

func (l *loginSession) bumpInterval(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.interval >= maxDeviceInterval {
		return
	}
	l.interval += d
	if l.interval > maxDeviceInterval {
		l.interval = maxDeviceInterval
	}
}

func (c *Client) putLogin(s *loginSession) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.logins == nil {
		c.logins = make(map[string]*loginSession)
	}
	now := c.now()
	for id, other := range c.logins {
		if now.After(other.expiresAt) {
			delete(c.logins, id)
		}
	}
	c.logins[s.sessionID] = s
}

func (c *Client) loginByID(id string) *loginSession {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.logins[id]
}

// ---------------------------------------------------------------------------
// Console control-plane calls.
// ---------------------------------------------------------------------------

// deviceToken is the union of the device-flow success and error bodies.
type deviceToken struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int    `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// loginJSON performs one Console call.  orgID, when set, is sent as the
// console's x-org-id header -- the inference endpoint uses x-opencode-org-id
// instead, which is why this does not go through do().
//
// The body is never put into the returned error: it can carry a token.
func (c *Client) loginJSON(ctx context.Context, method, url, bearer, orgID string, body []byte) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if orgID != "" {
		req.Header.Set("x-org-id", orgID)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, loginMaxBody)
	return raw, resp.StatusCode, nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// ---------------------------------------------------------------------------
// Device-code flow.
// ---------------------------------------------------------------------------

// startOAuthLogin asks the console for a device code and records a session.
func (c *Client) startOAuthLogin(ctx context.Context) (core.LoginState, error) {
	body := mustJSON(map[string]string{"client_id": oauthClientID})
	raw, status, err := c.loginJSON(ctx, http.MethodPost, c.cfg.authBaseURL()+"/auth/device/code", "", "", body)
	if err != nil {
		return core.LoginState{}, errors.New(core.Redact("OpenCode Console login: cannot reach the vendor: " + err.Error()))
	}
	if status < 200 || status >= 300 {
		return core.LoginState{}, fmt.Errorf("OpenCode Console login: the vendor returned HTTP %d", status)
	}
	var dev struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if uerr := json.Unmarshal(raw, &dev); uerr != nil || strings.TrimSpace(dev.DeviceCode) == "" {
		return core.LoginState{}, errors.New("OpenCode Console login: the vendor did not return a device code")
	}
	url := c.cfg.resolveAuthURL(firstNonEmpty(dev.VerificationURIComplete, dev.VerificationURI))
	if url == "" {
		return core.LoginState{}, errors.New("OpenCode Console login: the vendor did not return a verification URL")
	}
	interval := defaultDeviceInterval
	if dev.Interval > 0 {
		interval = time.Duration(dev.Interval) * time.Second
	}
	ttl := loginDefaultTTL
	if dev.ExpiresIn > 0 {
		ttl = time.Duration(dev.ExpiresIn) * time.Second
	}
	now := c.now()
	sess := &loginSession{
		now:        c.now,
		sessionID:  "oauth:" + keyFingerprint(dev.DeviceCode),
		realm:      realmOAuth,
		url:        url,
		code:       strings.TrimSpace(dev.UserCode),
		deviceCode: dev.DeviceCode,
		interval:   interval,
		expiresAt:  now.Add(ttl),
		state:      core.LoginPending,
		message:    "在浏览器中打开链接并输入设备码完成登录",
	}
	c.putLogin(sess)
	return sess.snapshot(), nil
}

// PollLogin advances a session by exactly one upstream poll.  The panel owns
// the cadence, so this method does not run a background poller: a session that
// nobody polls simply expires.
func (c *Client) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return core.LoginState{}, errors.New("session id is required")
	}
	sess := c.loginByID(id)
	if sess == nil {
		return core.LoginState{}, fmt.Errorf("login session %q not found", id)
	}
	if st := sess.snapshot(); st.State != core.LoginPending {
		return st, nil
	}
	if !sess.beginPoll() {
		return sess.snapshot(), nil
	}
	defer sess.endPoll()

	res := c.pollOAuthOnce(ctx, sess)
	switch res.outcome {
	case loginPollDone:
		c.completeOAuthLogin(sess, res.cred)
	case loginPollFatal:
		sess.set(core.LoginFailed, res.message, "")
	default:
		sess.set(core.LoginPending, res.message, "")
	}
	return sess.snapshot(), nil
}

// CancelLogin stops a session.  Cancelling before the vendor hands over a token
// leaves nothing on disk.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return errors.New("session id is required")
	}
	sess := c.loginByID(id)
	if sess == nil {
		return fmt.Errorf("login session %q not found", id)
	}
	sess.markCancelled()
	return nil
}

// loginOutcome classifies one poll of the token endpoint.
type loginOutcome int

const (
	loginPollPending loginOutcome = iota
	loginPollDone
	loginPollFatal
)

type loginPollResult struct {
	outcome loginOutcome
	cred    *accountRecord
	message string
}

// pollOAuthOnce performs one token poll and, on success, fetches the workspace.
func (c *Client) pollOAuthOnce(ctx context.Context, sess *loginSession) loginPollResult {
	body := mustJSON(map[string]string{
		"client_id":   oauthClientID,
		"grant_type":  oauthDeviceGrant,
		"device_code": sess.deviceCode,
	})
	raw, status, err := c.loginJSON(ctx, http.MethodPost, c.cfg.authBaseURL()+"/auth/device/token", "", "", body)
	if err != nil {
		return loginPollResult{outcome: loginPollFatal, message: "cannot reach the vendor: " + core.Redact(err.Error())}
	}
	// The RFC device flow answers "not yet" with HTTP 400 and an error body, so
	// 400 is a normal pending response here, not a failure.
	if status != http.StatusOK && status != http.StatusBadRequest {
		return loginPollResult{outcome: loginPollFatal, message: fmt.Sprintf("the vendor returned HTTP %d", status)}
	}
	var tok deviceToken
	if uerr := json.Unmarshal(raw, &tok); uerr != nil {
		return loginPollResult{outcome: loginPollFatal, message: "the vendor returned an unrecognised response"}
	}
	if strings.TrimSpace(tok.AccessToken) != "" {
		return c.finishOAuthLogin(ctx, tok)
	}
	switch tok.Error {
	case "authorization_pending":
		return loginPollResult{outcome: loginPollPending, message: "等待浏览器中确认登录"}
	case "slow_down":
		sess.bumpInterval(5 * time.Second)
		return loginPollResult{outcome: loginPollPending, message: "等待浏览器中确认登录"}
	case "expired_token":
		return loginPollResult{outcome: loginPollFatal, message: "设备码已过期，请重新发起登录"}
	case "access_denied":
		return loginPollResult{outcome: loginPollFatal, message: "登录被拒绝"}
	default:
		msg := strings.TrimSpace(firstNonEmpty(tok.ErrorDescription, tok.Error))
		if msg == "" {
			msg = "the vendor rejected the login"
		}
		return loginPollResult{outcome: loginPollFatal, message: msg}
	}
}

// finishOAuthLogin fetches the user, workspace and workspace model list, then
// stores the login separately from the workspace's inference readiness.
func (c *Client) finishOAuthLogin(ctx context.Context, tok deviceToken) loginPollResult {
	base := c.cfg.authBaseURL()
	access := strings.TrimSpace(tok.AccessToken)

	var user struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if raw, status, err := c.loginJSON(ctx, http.MethodGet, base+"/api/user", access, "", nil); err == nil && status == http.StatusOK {
		_ = json.Unmarshal(raw, &user)
	}

	var orgs []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if raw, status, err := c.loginJSON(ctx, http.MethodGet, base+"/api/orgs", access, "", nil); err == nil && status == http.StatusOK {
		_ = json.Unmarshal(raw, &orgs)
	}
	orgID, orgName := "", ""
	if len(orgs) > 0 {
		orgID = strings.TrimSpace(orgs[0].ID)
		orgName = strings.TrimSpace(orgs[0].Name)
	}

	expiresAt := c.now()
	if tok.ExpiresIn > 0 {
		expiresAt = expiresAt.Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	seed := firstNonEmpty(strings.TrimSpace(user.ID), orgID, strings.TrimSpace(tok.RefreshToken), access)
	rec := accountRecord{
		ID:           accountID("oauth:" + keyFingerprint(seed)),
		Label:        firstNonEmpty(strings.TrimSpace(user.Email), orgName, "OpenCode Console"),
		AuthMode:     "oauth",
		AccessToken:  access,
		RefreshToken: strings.TrimSpace(tok.RefreshToken),
		ExpiresAt:    expiresAt.UTC().Format(time.RFC3339),
		OrgID:        orgID,
		OrgName:      orgName,
		Email:        strings.TrimSpace(user.Email),
		Enabled:      true,
		Source:       sourcePanel,
	}
	c.resolveWorkspace(ctx, &rec)
	if !c.upsertAccount(rec) {
		return loginPollResult{outcome: loginPollFatal, message: "the credential could not be stored"}
	}
	c.persist()
	return loginPollResult{outcome: loginPollDone, cred: &rec}
}

// completeOAuthLogin persists a finished login and marks the session successful.
func (c *Client) completeOAuthLogin(sess *loginSession, cred *accountRecord) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.cancelled || cred == nil {
		return
	}
	sess.state = core.LoginSuccess
	sess.accountID = cred.ID
	sess.message = core.Redact("已登录 " + firstNonEmpty(cred.Label, cred.ID) + "，凭证已保存")
	if !cred.inferenceReady() {
		sess.message += "；Console 登录有效，Zen 推理配置尚不可用：" + cred.InferenceError
	}
}

// ---------------------------------------------------------------------------
// Workspace model list and refresh.
// ---------------------------------------------------------------------------

// zenConfigResponse is the slice of GET /api/config this module reads.
type zenConfigResponse struct {
	Config struct {
		DisabledProviders []string `json:"disabled_providers"`
		Provider          struct {
			Opencode *struct {
				API     string `json:"api"`
				Options struct {
					APIKey  string            `json:"apiKey"`
					BaseURL string            `json:"baseURL"`
					Headers map[string]string `json:"headers"`
				} `json:"options"`
				Whitelist []string `json:"whitelist"`
				Models    map[string]struct {
					Disabled bool            `json:"disabled"`
					Provider json.RawMessage `json:"provider"`
					Cost     *struct {
						Input  float64 `json:"input"`
						Output float64 `json:"output"`
					} `json:"cost"`
				} `json:"models"`
			} `json:"opencode"`
		} `json:"provider"`
	} `json:"config"`
}

// resolveWorkspace replaces inference configuration, including clearing stale
// keys and models when the workspace withdraws access. No Console response body
// is included in errors because it may contain secrets.
func (c *Client) resolveWorkspace(ctx context.Context, acct *accountRecord) {
	acct.APIKey = ""
	acct.InferenceBaseURL = ""
	acct.InferenceHeaders = nil
	acct.AllowedModels = nil
	acct.InferenceError = "Console login saved; workspace has no Zen inference configuration"
	if strings.TrimSpace(acct.OrgID) == "" || strings.TrimSpace(acct.AccessToken) == "" {
		acct.InferenceError = "Console workspace or login token is missing; sign in again"
		return
	}
	raw, status, err := c.loginJSON(ctx, http.MethodGet, c.cfg.authBaseURL()+"/api/config", acct.AccessToken, acct.OrgID, nil)
	if err != nil {
		acct.InferenceError = "cannot reach Console workspace configuration; refresh account to retry"
		return
	}
	if status != http.StatusOK {
		acct.InferenceError = fmt.Sprintf("Console workspace configuration returned HTTP %d; refresh account to retry", status)
		return
	}
	var doc zenConfigResponse
	if uerr := json.Unmarshal(raw, &doc); uerr != nil {
		acct.InferenceError = "Console workspace configuration is not valid JSON"
		return
	}
	zen := doc.Config.Provider.Opencode
	if containsString(doc.Config.DisabledProviders, "opencode") || zen == nil {
		acct.InferenceError = "Console login is valid; Zen provider is disabled or absent in this workspace. Enable/configure Zen in Console, then refresh the account."
		return
	}
	base := strings.TrimSpace(firstNonEmpty(zen.Options.BaseURL, zen.API))
	if base != "" && !validBaseURL(base) {
		acct.InferenceError = "workspace Zen inference baseURL is invalid"
		return
	}
	key := strings.TrimSpace(zen.Options.APIKey)
	// Never resolve a Console token onto the default public Zen endpoint.
	if strings.Contains(key, "{env:") {
		if key != "{env:OPENCODE_CONSOLE_TOKEN}" || publicZenBase(base) {
			acct.InferenceError = "workspace inference credential requires an unsupported environment reference"
			return
		}
		// Retain the reference in a header so refreshed tokens are used.
		acct.InferenceHeaders = map[string]string{"Authorization": "Bearer " + key}
		key = ""
	}
	for name, value := range zen.Options.Headers {
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if strings.ContainsAny(name+value, "\r\n") || name == "" {
			acct.InferenceError = "workspace inference header is invalid"
			return
		}
		if strings.Contains(value, "{env:") &&
			(!strings.Contains(value, "{env:OPENCODE_CONSOLE_TOKEN}") ||
				strings.Contains(strings.ReplaceAll(value, "{env:OPENCODE_CONSOLE_TOKEN}", ""), "{env:") ||
				publicZenBase(base)) {
			acct.InferenceError = "workspace inference header requires an unsupported environment reference"
			return
		}
		if (name == "Authorization" || name == "X-Api-Key") &&
			strings.Contains(value, acct.AccessToken) &&
			publicZenBase(base) {
			acct.InferenceError = "workspace did not supply a separate Zen inference credential"
			return
		}
		if acct.InferenceHeaders == nil {
			acct.InferenceHeaders = make(map[string]string)
		}
		acct.InferenceHeaders[name] = value
	}
	if (key == acct.AccessToken || strings.HasPrefix(key, "st_")) &&
		publicZenBase(base) {
		acct.InferenceError = "workspace did not supply a separate Zen API key"
		return
	}
	acct.APIKey = key
	acct.InferenceBaseURL = base
	acct.InferenceError = ""
	if !acct.inferenceReady() {
		acct.InferenceError = "Console login is valid; workspace has no Zen API key. Configure Zen in Console or add a Zen API key."
	}
	whitelist := make(map[string]bool, len(zen.Whitelist))
	for _, id := range zen.Whitelist {
		whitelist[strings.TrimSpace(id)] = true
	}
	var out []string
	if len(zen.Models) == 0 {
		for id := range whitelist {
			if id != "" && !anonymousModelAllowed(id, c.cfg) {
				out = append(out, id)
			}
		}
	}
	for id, m := range zen.Models {
		id = strings.TrimSpace(id)
		if id == "" || m.Disabled {
			continue
		}
		if len(whitelist) > 0 && !whitelist[id] {
			continue
		}
		if len(m.Provider) > 0 && string(m.Provider) != "null" && string(m.Provider) != `""` {
			continue
		}
		if m.Cost != nil && m.Cost.Input <= 0 && m.Cost.Output <= 0 {
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	acct.AllowedModels = out
	if (len(zen.Models) > 0 || len(whitelist) > 0) && len(out) == 0 {
		acct.InferenceError = "workspace has no enabled models supported by this OpenAI chat adapter"
	}
}

// Compare destinations rather than URL spellings before resolving Console
// credentials. Default ports, host case and trailing slashes are equivalent.
func publicZenBase(base string) bool {
	if strings.TrimSpace(base) == "" {
		return true
	}
	u, err := url.Parse(base)
	if err != nil {
		return true
	}
	port := u.Port()
	defaultPort := port == "" || (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80")
	return strings.EqualFold(u.Hostname(), "opencode.ai") && defaultPort &&
		strings.TrimRight(path.Clean(u.Path), "/") == "/zen/v1"
}

// oauthExpiring reports whether an OAuth account's access token is within the
// refresh window.  An absent or unparseable expiry means "leave it alone": a
// needless refresh is churn, and a broken clock must not spin.
func (a *accountRecord) oauthExpiring(now time.Time) bool {
	if a == nil || a.authMode() != "oauth" {
		return false
	}
	raw := strings.TrimSpace(a.ExpiresAt)
	if raw == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return false
	}
	return !now.Add(oauthRefreshSkew).Before(t)
}

// ensureFreshOAuth refreshes an OAuth credential in place when it is about to
// expire, updating both the pool and the caller's copy.  A non-OAuth account
// and a token that is still fresh are no-ops.
func (c *Client) ensureFreshOAuth(ctx context.Context, acct *accountRecord) error {
	if acct == nil || !acct.oauthExpiring(c.now()) {
		return nil
	}
	next, err := c.refreshOAuthToken(ctx, acct)
	if err != nil {
		return err
	}
	*acct = *next
	return nil
}

// refreshOAuthToken exchanges a refresh token for a new access token and
// persists the result under the same account id.
func (c *Client) refreshOAuthToken(ctx context.Context, acct *accountRecord) (*accountRecord, error) {
	body := mustJSON(map[string]string{
		"client_id":     oauthClientID,
		"grant_type":    "refresh_token",
		"refresh_token": acct.RefreshToken,
	})
	raw, status, err := c.loginJSON(ctx, http.MethodPost, c.cfg.authBaseURL()+"/auth/device/token", "", "", body)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, errors.New(scrubAccount("OpenCode token refresh: cannot reach the vendor: "+err.Error(), acct))
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("OpenCode token refresh: the vendor returned HTTP %d", status)
	}
	var tok deviceToken
	if uerr := json.Unmarshal(raw, &tok); uerr != nil {
		return nil, errors.New("OpenCode token refresh: the vendor returned an unrecognised response")
	}
	if strings.TrimSpace(tok.AccessToken) == "" {
		return nil, fmt.Errorf("OpenCode token refresh: %s", scrubAccount(firstNonEmpty(tok.ErrorDescription, tok.Error, "no access token"), acct))
	}
	expiresAt := c.now()
	if tok.ExpiresIn > 0 {
		expiresAt = expiresAt.Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	next := *acct
	next.AccessToken = strings.TrimSpace(tok.AccessToken)
	next.RefreshToken = firstNonEmpty(strings.TrimSpace(tok.RefreshToken), acct.RefreshToken)
	next.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
	c.resolveWorkspace(ctx, &next)
	if !c.pool.upsert(next) {
		return nil, errors.New("OpenCode token refresh: the refreshed credential could not be stored")
	}
	c.persist()
	return &next, nil
}
