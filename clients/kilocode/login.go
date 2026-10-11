package kilocode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Panel login: Kilo's device authorisation.
//
// Kilo Desktop and the `kilo` CLI sign in with a device grant.  The client
// mints a short code, the operator approves it in a browser, and the poll that
// sees the approval hands back a long-lived bearer token:
//
//	POST {auth}/api/device-auth/codes         -> {code, verificationUrl, expiresIn, ...}
//	GET  {auth}/api/device-auth/codes/{code}  -> 202 pending / 403 denied / 410 expired
//	                                             200 {status:"approved", token, userEmail}
//
// The token that comes back is the same bearer credential Kilo's own tools
// store in `~/.local/share/kilo/auth.json`, so a signed-in account lands on the
// same pool a pasted API key does.  Kilo issues the token with a ~1 year
// lifetime and publishes no rotation endpoint, so the module keeps it as-is; a
// token the gateway finally rejects parks the row and the panel offers a fresh
// sign-in.
// ---------------------------------------------------------------------------

const (
	// loginClientID is the public client id Kilo's own client sends.
	loginClientID = "kilo"
	// defaultDeviceInterval is the poll cadence used when the vendor omits one.
	defaultDeviceInterval = 4 * time.Second
	// loginDefaultTTL is the fallback device-code lifetime.
	loginDefaultTTL = 10 * time.Minute
	// loginMaxBody bounds every vendor body the login flow will read.
	loginMaxBody = 1 << 20
	// loginSessionTTL keeps a finished session around long enough for the
	// panel's final poll to read it.
	loginSessionTTL = 30 * time.Minute
	// oauthTokenFallbackTTL matches the vendor's own sign-in client, which
	// stamps the token a year out when the response carries no expiry claim.
	oauthTokenFallbackTTL = 365 * 24 * time.Hour
)

// oauthAccountID builds the pool id of a signed-in Kilo account.  The vendor
// user id is the stable identity, so signing in again updates the same row
// instead of stacking a new one.
func oauthAccountID(userID string) string {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return clientName + ":oauth"
	}
	return clientName + ":oauth:" + keyFingerprint(userID)
}

// core.LoginProvider is what the panel drives: "add an account" here means the
// device sign-in, not a pasted key.
var _ core.LoginProvider = (*Client)(nil)

// loginSession is one in-flight device authorisation.
type loginSession struct {
	mu         sync.Mutex
	now        func() time.Time
	sessionID  string
	deviceCode string
	code       string
	server     string
	url        string
	interval   time.Duration
	nextPoll   time.Time
	expiresAt  time.Time

	state     string
	message   string
	accountID string
	polling   bool
	cancelled bool
}

func (l *loginSession) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

func (l *loginSession) terminal() bool {
	switch l.state {
	case core.LoginSuccess, core.LoginFailed, core.LoginCancelled:
		return true
	}
	return false
}

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
	}
}

// beginPoll guards against two panel polls racing into the vendor at once, and
// throttles them to the vendor's own poll interval.
func (l *loginSession) beginPoll() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.polling || l.cancelled || l.terminal() {
		return false
	}
	now := l.clock()
	if now.After(l.expiresAt) {
		l.state, l.message = core.LoginFailed, "the device code expired; start again"
		return false
	}
	if !l.nextPoll.IsZero() && now.Before(l.nextPoll) {
		return false
	}
	l.polling = true
	return true
}

func (l *loginSession) endPoll() {
	l.mu.Lock()
	l.polling = false
	l.nextPoll = l.clock().Add(l.interval)
	l.mu.Unlock()
}

func (l *loginSession) markCancelled() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cancelled = true
	if !l.terminal() {
		l.state = core.LoginCancelled
		l.message = "cancelled"
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
		if other == nil || now.After(other.expiresAt.Add(loginSessionTTL)) {
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

// --- vendor calls ---------------------------------------------------------

// deviceCodeResponse is POST /api/device-auth/codes.  The vendor spells the
// last two fields camelCase; the snake_case aliases cover a spec-shaped mirror.
type deviceCodeResponse struct {
	Code                    string `json:"code"`
	UserCode                string `json:"user_code"`
	DeviceCode              string `json:"device_code"`
	VerificationURL         string `json:"verificationUrl"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

// deviceStatusResponse is the 200 body from GET /api/device-auth/codes/{code}.
type deviceStatusResponse struct {
	Status    string `json:"status"`
	Token     string `json:"token"`
	UserEmail string `json:"userEmail"`
}

// authJSON performs one auth-plane call and returns the raw body and status.
func (c *Client) authJSON(ctx context.Context, method, endpoint string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctxOrBackground(ctx), method, endpoint, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", c.cfg.UserAgent)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, loginMaxBody)
	return raw, resp.StatusCode, nil
}

// --- core.LoginProvider ---------------------------------------------------

// StartLogin asks the vendor for a device code and records a pending session.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	c.ensure()
	raw, status, err := c.authJSON(ctx, http.MethodPost, c.cfg.deviceCodeURL(),
		map[string]string{"client_id": loginClientID})
	if err != nil {
		return core.LoginState{}, errors.New(core.Redact("kilo login: cannot reach the vendor: " + err.Error()))
	}
	if status < 200 || status >= 300 {
		return core.LoginState{}, fmt.Errorf("kilo login: the vendor returned HTTP %d", status)
	}
	var dev deviceCodeResponse
	if err := json.Unmarshal(raw, &dev); err != nil {
		return core.LoginState{}, errors.New("kilo login: the vendor returned an unrecognised response")
	}
	code := strings.TrimSpace(firstNonEmpty(dev.Code, dev.UserCode))
	if code == "" {
		return core.LoginState{}, errors.New("kilo login: the vendor did not return a device code")
	}
	verify := strings.TrimSpace(firstNonEmpty(
		dev.VerificationURL, dev.VerificationURIComplete, dev.VerificationURI))
	if verify == "" {
		return core.LoginState{}, errors.New("kilo login: the vendor did not return a verification URL")
	}
	if strings.HasPrefix(verify, "/") {
		verify = c.cfg.deviceAuthBase() + verify
	}
	interval := defaultDeviceInterval
	if dev.Interval > 0 {
		interval = time.Duration(dev.Interval) * time.Second
	}
	ttl := loginDefaultTTL
	if dev.ExpiresIn > 0 {
		ttl = time.Duration(dev.ExpiresIn) * time.Second
	}
	sess := &loginSession{
		now:        c.now,
		sessionID:  "device:" + keyFingerprint(code),
		deviceCode: strings.TrimSpace(dev.DeviceCode),
		code:       code,
		server:     c.cfg.authBaseURL(),
		url:        verify,
		interval:   interval,
		expiresAt:  c.now().Add(ttl),
		state:      core.LoginPending,
		message:    "open the link and approve the code",
	}
	c.putLogin(sess)
	return sess.snapshot(), nil
}

// PollLogin advances a session by exactly one upstream poll.  The panel owns
// the cadence, so this method never runs a background poller.
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
	c.pollDeviceOnce(ctx, sess)
	return sess.snapshot(), nil
}

// CancelLogin stops a session.  Cancelling before the vendor hands over a
// token leaves nothing on disk.
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

// pollDeviceOnce performs one status poll and, on approval, stores the account.
func (c *Client) pollDeviceOnce(ctx context.Context, sess *loginSession) {
	raw, status, err := c.authJSON(ctx, http.MethodGet, c.cfg.deviceStatusURL(sess.code), nil)
	if err != nil {
		sess.set(core.LoginFailed, "cannot reach the vendor: "+core.Redact(err.Error()), "")
		return
	}
	switch status {
	case http.StatusAccepted: // 202: still waiting for the human
		sess.set(core.LoginPending, "waiting for approval in the browser", "")
		return
	case http.StatusForbidden: // 403: the operator refused
		sess.set(core.LoginFailed, "the login was denied", "")
		return
	case http.StatusGone: // 410: the code expired
		sess.set(core.LoginFailed, "the device code expired; start again", "")
		return
	case http.StatusOK:
		// fall through to the success body
	default:
		sess.set(core.LoginFailed, fmt.Sprintf("the vendor returned HTTP %d", status), "")
		return
	}
	var poll deviceStatusResponse
	if err := json.Unmarshal(raw, &poll); err != nil {
		sess.set(core.LoginFailed, "the vendor returned an unrecognised response", "")
		return
	}
	switch strings.ToLower(strings.TrimSpace(poll.Status)) {
	case "approved":
		if strings.TrimSpace(poll.Token) == "" {
			sess.set(core.LoginFailed, "the vendor approved the login but returned no token", "")
			return
		}
		c.finishDeviceLogin(ctx, sess, poll)
	case "denied":
		sess.set(core.LoginFailed, "the login was denied", "")
	case "expired":
		sess.set(core.LoginFailed, "the device code expired; start again", "")
	case "pending", "":
		sess.set(core.LoginPending, "waiting for approval in the browser", "")
	default:
		sess.set(core.LoginPending, "waiting for approval in the browser", "")
	}
}

// finishDeviceLogin stores the signed-in account and marks the session done.
func (c *Client) finishDeviceLogin(ctx context.Context, sess *loginSession, poll deviceStatusResponse) {
	access := strings.TrimSpace(poll.Token)
	userID, label := c.kiloIdentity(ctx, sess.server, access, poll.UserEmail)
	id := oauthAccountID(userID)
	if userID == "" {
		// Without a stable user id, key the row on the token so two different
		// sign-ins do not collapse into one row.
		id = oauthAccountID(access)
	}
	expiresAt := jwtExpiry(access)
	if expiresAt.IsZero() {
		expiresAt = c.now().Add(oauthTokenFallbackTTL)
	}
	rec := accountRecord{
		ID:           id,
		Label:        firstNonEmpty(label, "Kilo account"),
		AuthMode:     authModeOAuth,
		AccessToken:  access,
		RefreshToken: access,
		ExpiresAt:    expiresAt.UTC().Format(time.RFC3339),
		OAuthServer:  sess.server,
		Source:       sourcePanel,
		AddedAt:      c.now().UTC().Format(time.RFC3339),
	}
	c.pool.upsert(rec)
	c.persist()
	c.noteSuccess(rec)
	sess.set(core.LoginSuccess, "signed in to Kilo; the credential is stored", id)
	c.refreshModelsAsync()
}

// kiloIdentity returns the stable account id and a display label for a token.
// The vendor stamps kiloUserId into the JWT, so the common path is offline; a
// token without the claim falls back to the profile endpoint.
func (c *Client) kiloIdentity(ctx context.Context, server, access, email string) (string, string) {
	email = strings.TrimSpace(email)
	if sub := jwtStringClaim(access, "kiloUserId"); sub != "" {
		return sub, firstNonEmpty(email, sub)
	}
	server = strings.TrimRight(strings.TrimSpace(server), "/")
	if server == "" || strings.TrimSpace(access) == "" {
		return "", email
	}
	req, err := http.NewRequestWithContext(ctxOrBackground(ctx), http.MethodGet, server+"/api/user", nil)
	if err != nil {
		return "", email
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", email
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", email
	}
	var user struct {
		ID    string `json:"id"`
		Email string `json:"google_user_email"`
		Name  string `json:"google_user_name"`
	}
	if err := json.Unmarshal(readLimited(resp.Body, loginMaxBody), &user); err != nil {
		return "", email
	}
	return strings.TrimSpace(user.ID),
		firstNonEmpty(email, strings.TrimSpace(user.Email), strings.TrimSpace(user.Name))
}

// --- token helpers --------------------------------------------------------

// bearer returns the credential a request should carry: the OAuth access token
// for a signed-in account, the static key otherwise, with either field as a
// fallback for a record written by an older build.
func (a accountRecord) bearer() string {
	if a.authMode() == authModeOAuth {
		return firstNonEmpty(a.AccessToken, a.RefreshToken, a.APIKey)
	}
	return a.APIKey
}

// jwtPayload decodes the claims of a JWT.  A token that is not a JWT (or whose
// middle segment cannot be decoded) answers nil, which every caller treats as
// "no claim available".
func jwtPayload(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return nil
	}
	seg := strings.TrimRight(parts[1], "=")
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.StdEncoding,
	} {
		b, err := enc.DecodeString(seg)
		if err != nil {
			continue
		}
		var claims map[string]any
		if json.Unmarshal(b, &claims) == nil {
			return claims
		}
	}
	return nil
}

// jwtStringClaim reads one string claim out of a JWT, or "" when it is absent.
func jwtStringClaim(token, key string) string {
	claims := jwtPayload(token)
	if claims == nil {
		return ""
	}
	s, _ := claims[key].(string)
	return strings.TrimSpace(s)
}

// jwtExpiry reads the `exp` claim as an absolute time, or the zero time when
// the token is not a JWT or carries no numeric expiry.
func jwtExpiry(token string) time.Time {
	claims := jwtPayload(token)
	if claims == nil {
		return time.Time{}
	}
	switch v := claims["exp"].(type) {
	case float64:
		if v > 0 {
			return time.Unix(int64(v), 0).UTC()
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return time.Unix(n, 0).UTC()
		}
	}
	return time.Time{}
}
