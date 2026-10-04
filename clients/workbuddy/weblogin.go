package workbuddy

// Browser ("panel") login: the vendor's own plugin OAuth, driven entirely from
// Go so the operator only has to open one URL.
//
// The flow, per realm:
//
//	POST base + "/v2/plugin/auth/state?platform=CLI"      body {}                -> {state, authUrl}
//	GET  base + "/v2/plugin/auth/token?state=<state>"                            -> {accessToken, refreshToken, expiresIn, domain}
//	GET  base + "/v2/plugin/login/account?state=<state>"  Authorization: Bearer  -> {uid, enterpriseId, nickname}
//
// `base` is the realm's chat host -- https://copilot.tencent.com for cn and
// https://www.workbuddy.ai for global -- and Origin/Referer follow the realm.
// Endpoints, header set and realm split are ported from workbuddy2api-panel
// internal/panel/login.go:36-114 (endpoints/headers/doJSON) and
// internal/panel/login.go:116-289 (the state/poll/persist flow); cmd/login/main.go:208-279
// documents the same three calls for the CLI.  Anything this module could not
// confirm offline is called out in README.md under "Panel login".

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

const (
	// loginPlatform is the plugin platform selector (reference login.go:36-49).
	loginPlatform = "CLI"

	loginStatePath   = "/v2/plugin/auth/state?platform=" + loginPlatform
	loginTokenPath   = "/v2/plugin/auth/token?state="
	loginAccountPath = "/v2/plugin/login/account?state="

	// defaultLoginTTL mirrors the reference panel's loginTTL (panel.go:118):
	// an authorisation URL that nobody finishes is reclaimed after 15 minutes.
	defaultLoginTTL = 15 * time.Minute
	// loginHTTPTimeout mirrors the reference loginHTTP client (login.go:52).
	loginHTTPTimeout = 30 * time.Second
	// loginMaxBodyBytes bounds every upstream body we are willing to read.
	loginMaxBodyBytes = 1 << 20
	// loginUIDMaxLen mirrors validUID (login.go:66-78).
	loginUIDMaxLen = 64

	// loginExpiredMessage is used both by snapshot() and by the poll path, so a
	// session that runs out of time reports one stable string no matter which
	// side notices first.
	loginExpiredMessage = "the authorisation window expired; start again"
	// loginCancelledMessage is what CancelLogin records.
	loginCancelledMessage = "cancelled"
	// loginWaitingMessage is the pending text when the vendor says nothing useful.
	loginWaitingMessage = "waiting for the vendor to confirm the sign-in"
)

// loginOutcome classifies one poll of the token endpoint.
type loginOutcome int

const (
	// loginPollPending: the operator has not finished in the browser yet.
	loginPollPending loginOutcome = iota
	// loginPollDone: the vendor issued a token and the account was fetched.
	loginPollDone
	// loginPollFatal: the attempt cannot succeed (bad state, vendor broken).
	loginPollFatal
)

// loginBusinessError is an HTTP 2xx whose envelope carries a non-zero business
// code -- how this API says "still waiting" (reference login.go:174-179).
type loginBusinessError struct {
	code   int
	msg    string
	status int
}

func (e *loginBusinessError) Error() string {
	msg := strings.TrimSpace(e.msg)
	if msg == "" {
		return fmt.Sprintf("the vendor rejected the request (code %d)", e.code)
	}
	return fmt.Sprintf("the vendor rejected the request (code %d): %s", e.code, msg)
}

// panelLogin is one in-flight browser authorisation.
type panelLogin struct {
	mu        sync.Mutex
	sessionID string
	realm     string
	url       string
	startedAt time.Time
	expiresAt time.Time

	state     string
	message   string
	accountID string

	// polling guards against two panel polls racing into the vendor at once.
	polling bool
	// cancelled is set by CancelLogin and is checked while holding mu, so a
	// cancel that lands mid-poll can never leave a credential behind.
	cancelled bool
}

// terminal reports whether the session has reached a state it can never leave.
func (l *panelLogin) terminal() bool {
	switch l.state {
	case core.LoginSuccess, core.LoginFailed, core.LoginCancelled:
		return true
	}
	return false
}

// set records progress, but never resurrects a finished session.
func (l *panelLogin) set(state, message, accountID string) {
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
func (l *panelLogin) snapshot() core.LoginState {
	l.mu.Lock()
	defer l.mu.Unlock()
	state, message := l.state, l.message
	if state == core.LoginPending && time.Now().After(l.expiresAt) {
		state, message = core.LoginFailed, loginExpiredMessage
		l.state, l.message = state, message
	}
	return core.LoginState{
		SessionID: l.sessionID,
		State:     state,
		URL:       l.url,
		Message:   core.Redact(message),
		AccountID: l.accountID,
		Realm:     l.realm,
	}
}

// markCancelled stops the session for good.
func (l *panelLogin) markCancelled() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cancelled = true
	l.state = core.LoginCancelled
	l.message = loginCancelledMessage
	l.accountID = ""
}

// beginPoll claims the right to talk to the vendor, or reports that another
// poll, a cancel or a finished session already owns the session.
func (l *panelLogin) beginPoll() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.polling || l.cancelled || l.terminal() {
		return false
	}
	if time.Now().After(l.expiresAt) {
		l.state, l.message = core.LoginFailed, loginExpiredMessage
		return false
	}
	l.polling = true
	return true
}

func (l *panelLogin) endPoll() {
	l.mu.Lock()
	l.polling = false
	l.mu.Unlock()
}

// loginCredential is a completed login waiting to be persisted.
type loginCredential struct {
	accessToken  string
	refreshToken string
	expiresAt    int64
	domain       string
	realm        string
	uid          string
	enterpriseID string
	nickname     string
}

// spec renders the credential in the shape AddAccount understands, so the
// credential lands on the same code path as an operator-pasted one (panel file
// name prefix, origin=panel, hot reload into the pool).
func (cr *loginCredential) spec() core.AccountSpec {
	fields := map[string]string{
		"access_token": cr.accessToken,
		"realm":        cr.realm,
		"uid":          cr.uid,
	}
	if cr.refreshToken != "" {
		fields["refresh_token"] = cr.refreshToken
	}
	if cr.domain != "" {
		fields["domain"] = cr.domain
	}
	if cr.enterpriseID != "" {
		fields["enterprise_id"] = cr.enterpriseID
	}
	if cr.nickname != "" {
		fields["nickname"] = cr.nickname
	}
	if cr.expiresAt > 0 {
		fields["expires_at"] = strconv.FormatInt(cr.expiresAt, 10)
	}
	return core.AccountSpec{Fields: fields}
}

// loginPollResult is the outcome of one poll.
type loginPollResult struct {
	outcome loginOutcome
	cred    *loginCredential // set when outcome is loginPollDone
	message string           // operator-facing text for pending/fatal outcomes
}

// --- realm plumbing --------------------------------------------------------

// normalizeLoginRealm folds the config vocabulary onto the two realms the
// vendor actually serves.  Anything unknown (including "") is cn, matching the
// reference panel's default (login.go:118-129).
func normalizeLoginRealm(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case realmGlobal, "intl", "international":
		return realmGlobal
	default:
		return realmCN
	}
}

// loginBase is the host serving the plugin auth endpoints for a realm.  The
// reference keeps upstreamBaseCN/upstreamBaseGlobal separate (login.go:26-49);
// here they are exactly this module's chat hosts, so the realm handling lives in
// one place instead of being duplicated.
func (u *Upstream) loginBase(realm string) string {
	if realm == realmGlobal {
		return u.globalChatBase()
	}
	if u != nil && u.ChatBaseCN != "" {
		return u.ChatBaseCN
	}
	return "https://copilot.tencent.com"
}

// loginOrigin is the Origin/Referer pair for a realm (reference login.go:29-32).
func loginOrigin(realm string) string {
	if realm == realmGlobal {
		return originRefererGlobal
	}
	return originRefererCN
}

func (c *Client) loginRealm() string { return normalizeLoginRealm(c.cfg.LoginRealm) }

func (c *Client) loginTTL() time.Duration {
	if c.cfg.LoginTTLSeconds != nil && *c.cfg.LoginTTLSeconds > 0 {
		return time.Duration(*c.cfg.LoginTTLSeconds) * time.Second
	}
	return defaultLoginTTL
}

func (c *Client) loginHTTP() *http.Client {
	if c != nil && c.up != nil && c.up.HTTP != nil {
		return c.up.HTTP
	}
	return &http.Client{Timeout: loginHTTPTimeout}
}

// loginHeaders is the header set of the reference implementation
// (login.go:54-61) plus X-CodeBuddy-Request, which this module sends on every
// other call.  X-User-Id / X-Machine-ID / X-Domain are deliberately absent:
// they are derived from an account, and the whole point of this flow is that no
// account exists yet.
func (c *Client) loginHeaders(req *http.Request, origin string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", codeBuddyCLIUA)
}

// loginJSON performs one control-plane call and unwraps the envelope.  It never
// puts the response body into an error: that body can contain the access token.
func (c *Client) loginJSON(ctx context.Context, method, endpoint, bearer string, body []byte, origin string) (json.RawMessage, int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	c.loginHeaders(req, origin)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.loginHTTP().Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("cannot reach the vendor: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, loginMaxBodyBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("cannot read the vendor response: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("the vendor returned HTTP %d", resp.StatusCode)
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("the vendor returned an unrecognised response (HTTP %d)", resp.StatusCode)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, &loginBusinessError{code: env.Code, msg: env.Msg, status: resp.StatusCode}
	}
	return env.Data, resp.StatusCode, nil
}

// loginPollVerdict decides whether a failed token call means "not yet" or
// "never".
//
// The reference treats every non-5xx failure as pending (login.go:174-179),
// because a pending login is served as HTTP 200 with a non-zero business code.
// This module additionally treats 401/403/404/410 as terminal: pending is
// expressed as code!=0 at HTTP 200, so a rejected or unknown state cannot mean
// "keep waiting".  429 and every other 4xx stay pending (rate limiting and WAF
// pages must not kill a session the operator is still completing).
func loginPollVerdict(status int, err error) loginOutcome {
	if err == nil {
		return loginPollDone
	}
	if status == 0 || status >= 500 {
		return loginPollFatal
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return loginPollFatal
	}
	return loginPollPending
}

func loginPendingMessage(err error) string {
	var be *loginBusinessError
	if errors.As(err, &be) {
		if msg := strings.TrimSpace(be.msg); msg != "" {
			return loginWaitingMessage + " (" + msg + ")"
		}
	}
	return loginWaitingMessage
}

func loginFailureMessage(err error) string {
	if err == nil {
		return "the sign-in attempt failed"
	}
	return "the sign-in attempt failed: " + err.Error()
}

// validLoginUID mirrors the reference guard (login.go:66-78).  The uid becomes
// part of a file name, so a uid carrying path separators is refused outright
// rather than sanitised into something that might collide.
func validLoginUID(uid string) bool {
	if uid == "" || len(uid) > loginUIDMaxLen {
		return false
	}
	for i := 0; i < len(uid); i++ {
		ch := uid[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9', ch == '-', ch == '_':
		default:
			return false
		}
	}
	return true
}

// --- session bookkeeping ---------------------------------------------------

func (c *Client) putLogin(s *panelLogin) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.logins == nil {
		c.logins = make(map[string]*panelLogin)
	}
	// Reclaim sessions nobody is going to finish, so "open the tab and walk
	// away" cannot pile up (reference panel.go:144-152).
	now := time.Now()
	for id, other := range c.logins {
		if now.After(other.expiresAt) {
			delete(c.logins, id)
		}
	}
	c.logins[s.sessionID] = s
}

func (c *Client) loginByID(id string) *panelLogin {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.logins[id]
}

// --- core.LoginProvider ----------------------------------------------------

// The realm-aware form is what the panel drives: this module serves two separate
// upstream services, so "add an account" is not a single flow.
var _ core.RealmLoginProvider = (*Client)(nil)

// StartLogin starts a login on the module's configured default realm.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	return c.StartLoginRealm(ctx, "")
}

// LoginRealms lists the realms this module can add an account to.  The vendor
// runs two separate services -- a mainland one and an international one -- with
// their own credential stores and their own model catalogues, so the realm has
// to be chosen before the flow starts: a credential from one realm can never
// serve the models the other realm lists.
func (c *Client) LoginRealms(context.Context) []core.LoginRealm {
	return []core.LoginRealm{
		{Code: realmCN, Name: "国内版", Help: "CodeBuddy（copilot.tencent.com）：国内账号与国内模型目录"},
		{Code: realmGlobal, Name: "国际版", Help: "WorkBuddy（workbuddy.ai）：国际账号与国际模型目录"},
	}
}

// StartLoginRealm asks the vendor for an authorisation URL on one realm.  An
// empty realm asks for the module's configured default, which is exactly what
// StartLogin passes -- so an operator who never touches the picker sees the
// behaviour this module had before the picker existed.
func (c *Client) StartLoginRealm(ctx context.Context, want string) (core.LoginState, error) {
	// Adding a second account is a legitimate operation: the pool being usable
	// says nothing about whether the operator wants another credential.  The
	// old short circuit returned success without an authorisation URL, which the
	// panel rendered as a bogus "open this link" step.  Always start a real
	// session; AddAccount de-duplicates by uid, so re-signing into an existing
	// account updates it rather than creating a duplicate.
	realm := c.loginRealm()
	if strings.TrimSpace(want) != "" {
		realm = normalizeLoginRealm(want)
	}
	sess, err := c.startLoginSession(ctx, realm)
	if err != nil {
		return core.LoginState{}, err
	}
	return sess.snapshot(), nil
}

// startLoginSession is the body of StartLoginRealm without the "an account is
// already usable" short-circuit.  The auto-login flow calls it directly: that
// flow exists to add ANOTHER account, so a usable pool must not stop it.
func (c *Client) startLoginSession(ctx context.Context, realm string) (*panelLogin, error) {
	if c.accountsDir() == "" {
		return nil, errors.New("no data directory: a credential could not be stored")
	}
	origin := loginOrigin(realm)
	data, _, err := c.loginJSON(ctx, http.MethodPost, c.up.loginBase(realm)+loginStatePath, "", []byte("{}"), origin)
	if err != nil {
		return nil, errors.New(core.Redact("auth state: " + err.Error()))
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if uerr := json.Unmarshal(data, &st); uerr != nil || strings.TrimSpace(st.State) == "" || strings.TrimSpace(st.AuthURL) == "" {
		return nil, errors.New("auth state: the vendor did not return an authorisation URL")
	}

	now := time.Now()
	sess := &panelLogin{
		sessionID: strings.TrimSpace(st.State),
		realm:     realm,
		url:       strings.TrimSpace(st.AuthURL),
		startedAt: now,
		expiresAt: now.Add(c.loginTTL()),
		state:     core.LoginPending,
		message:   loginWaitingMessage,
	}
	c.putLogin(sess)
	c.up.log("workbuddy: browser login started realm=%s", realm)
	return sess, nil
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
	// Finished or expired sessions never touch the network again.
	if st := sess.snapshot(); st.State != core.LoginPending {
		return st, nil
	}
	if !sess.beginPoll() {
		return sess.snapshot(), nil
	}
	defer sess.endPoll()

	res := c.pollLoginOnce(ctx, sess)
	switch res.outcome {
	case loginPollDone:
		c.completeLogin(ctx, sess, res.cred)
	case loginPollFatal:
		sess.set(core.LoginFailed, res.message, "")
	default:
		sess.set(core.LoginPending, res.message, "")
	}
	return sess.snapshot(), nil
}

// CancelLogin stops a session.  Cancelling before the vendor hands over a token
// leaves nothing on disk: completeLogin takes the session lock, so a cancel
// either lands before the credential is written or the credential was already
// obtained and is kept.  The session itself is kept until it expires, so the
// panel can still read back LoginCancelled.
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
	c.up.log("workbuddy: browser login cancelled")
	return nil
}

// pollLoginOnce performs the token + account calls for one poll.
func (c *Client) pollLoginOnce(ctx context.Context, sess *panelLogin) loginPollResult {
	base := c.up.loginBase(sess.realm)
	origin := loginOrigin(sess.realm)

	raw, status, err := c.loginJSON(ctx, http.MethodGet, base+loginTokenPath+sess.sessionID, "", nil, origin)
	switch loginPollVerdict(status, err) {
	case loginPollFatal:
		return loginPollResult{outcome: loginPollFatal, message: loginFailureMessage(err)}
	case loginPollPending:
		return loginPollResult{outcome: loginPollPending, message: loginPendingMessage(err)}
	}

	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if uerr := json.Unmarshal(raw, &tok); uerr != nil || strings.TrimSpace(tok.AccessToken) == "" {
		// HTTP 200, code 0, but no token: the reference calls this "waiting for
		// login" too (login.go:186-189).
		return loginPollResult{outcome: loginPollPending, message: loginWaitingMessage}
	}

	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if acctRaw, _, aerr := c.loginJSON(ctx, http.MethodGet, base+loginAccountPath+sess.sessionID, tok.AccessToken, nil, origin); aerr == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}
	uid := strings.TrimSpace(acct.UID)
	if uid == "" {
		return loginPollResult{
			outcome: loginPollFatal,
			message: "the vendor issued a token but no account id; start again",
		}
	}
	if !validLoginUID(uid) {
		return loginPollResult{
			outcome: loginPollFatal,
			message: "the vendor returned an unusable account id; refusing to store it",
		}
	}

	// The realm the operator picked wins unless the token's domain proves the
	// account belongs to the other one (reference login.go:228-233).
	realm := sess.realm
	if realm != realmGlobal && isGlobalDomain(tok.Domain) {
		realm = realmGlobal
	}
	var expiresAt int64
	if tok.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return loginPollResult{
		outcome: loginPollDone,
		cred: &loginCredential{
			accessToken:  strings.TrimSpace(tok.AccessToken),
			refreshToken: strings.TrimSpace(tok.RefreshToken),
			expiresAt:    expiresAt,
			domain:       strings.TrimSpace(tok.Domain),
			realm:        realm,
			uid:          uid,
			enterpriseID: strings.TrimSpace(acct.EnterpriseID),
			nickname:     strings.TrimSpace(acct.Nickname),
		},
	}
}

// completeLogin persists a finished login and marks the session successful.
// It holds the session lock across the write so a concurrent CancelLogin cannot
// observe a half-finished transition.
func (c *Client) completeLogin(ctx context.Context, sess *panelLogin, cred *loginCredential) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.cancelled || cred == nil {
		return
	}
	rec, err := c.AddAccount(ctx, cred.spec())
	if err != nil {
		sess.state = core.LoginFailed
		sess.message = core.Redact("the credential could not be stored: " + err.Error())
		return
	}
	sess.state = core.LoginSuccess
	sess.accountID = rec.ID
	sess.message = core.Redact(loginSuccessMessage(rec))
}

func loginSuccessMessage(rec core.AccountRecord) string {
	if rec.Label != "" {
		return "signed in as " + rec.Label + "; the credential is stored and usable now"
	}
	return "signed in; the credential is stored and usable now"
}
