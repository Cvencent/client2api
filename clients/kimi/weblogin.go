package kimi

// Web login for Kimi Code.
//
// This module used to be a pure observer of the CLI's own login: it could see
// that a credential file existed and nothing more, because chat ran through
// `kimi -p`.  That made the panel's login button useless on a machine without
// the CLI ("the kimi CLI was not found on PATH ...").
//
// Kimi Code's login is a plain RFC 8628 device-code flow against auth.kimi.com,
// and its coding API is OpenAI-shaped over HTTPS, so the module can own the
// whole thing itself:
//
//	POST {oauthHost}/api/oauth/device_authorization   client_id
//	  -> device_code, user_code, verification_uri_complete, expires_in, interval
//	POST {oauthHost}/api/oauth/token                  client_id, device_code, grant_type
//	  -> access_token, refresh_token, expires_in  (or error=authorization_pending|...)
//
// The resulting token is stored inside the module's DataDir (see token.go for
// the on-disk shape) and is used for direct HTTPS calls.  The CLI remains a
// supported path - it is simply no longer required, and `login_mode: "cli"`
// restores the old guided flow verbatim.
//
// Nothing in this file ever logs or returns the token itself.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"client2api/internal/core"
)

const (
	// defaultOAuthHost is the vendor's OAuth issuer.  It is overridable so tests
	// can point the flow at a local server.
	defaultOAuthHost = "https://auth.kimi.com"
	// kimiCodeClientID is the public client id the CLI uses.  Device-code
	// clients are not secret-bearing, so shipping it is intended.
	kimiCodeClientID = "17e5f671-d194-4dfb-9706-5516cb48c098"

	deviceAuthPath  = "/api/oauth/device_authorization"
	deviceTokenPath = "/api/oauth/token"

	deviceGrantType  = "urn:ietf:params:oauth:grant-type:device_code"
	refreshGrantType = "refresh_token"

	// deviceSessionPrefix marks a session owned by this file.  The guided CLI
	// flow keeps loginSessionPrefix, so the two can share one store without
	// colliding.
	deviceSessionPrefix = "kimi-device-"
	// webLoginID is the account id a successful device login reports.
	webLoginID = "kimi-web"

	defaultDeviceInterval = 5 * time.Second
	defaultDeviceExpiry   = 30 * time.Minute
	slowDownIncrement     = 5 * time.Second

	// loginModeDevice is the default: the panel drives the vendor's device
	// authorization directly.
	loginModeDevice = "device"
	// loginModeCLI restores the pre-existing guided flow (operator runs
	// `kimi login` in a terminal; the module polls for the credential to appear).
	loginModeCLI = "cli"
)

// Device-flow error codes (RFC 8628 §3.5).
const (
	errAuthorizationPending = "authorization_pending"
	errSlowDown             = "slow_down"
	errExpiredToken         = "expired_token"
	errAccessDenied         = "access_denied"
)

// HTTP is done through (*Client).httpClient (prompt.go), which is the injected
// client or a bounded default.  A timeout matters here: a panel poll that hangs
// forever blocks the operator.

// oauthHost resolves the issuer: config, then the CLI's own environment
// variables, then the vendor default.
func (c *Client) oauthHost() string {
	if h := trimHost(c.cfg.OAuthHost); h != "" {
		return h
	}
	if h := trimHost(firstNonEmpty(os.Getenv("KIMI_CODE_OAUTH_HOST"), os.Getenv("KIMI_OAUTH_HOST"))); h != "" {
		return h
	}
	return defaultOAuthHost
}

func trimHost(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

func (c *Client) oauthClientID() string {
	if v := strings.TrimSpace(c.cfg.OAuthClientID); v != "" {
		return v
	}
	return kimiCodeClientID
}

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

type deviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// tokenResponse covers both success and the RFC 8628 error envelope: the vendor
// answers a pending poll with HTTP 400 and {"error":"authorization_pending"},
// so the body has to be parsed before the status code is judged.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// deviceSession is one in-flight device-code login.  Like loginSession it lives
// in memory only: a login flow that survives a restart is a flow nobody can
// reason about.
type deviceSession struct {
	id        string
	startedAt time.Time
	state     string
	message   string
	accountID string

	oauthHost  string
	deviceCode string
	userCode   string
	verifyURL  string
	expiresAt  time.Time

	// interval is the vendor's requested poll spacing, grown by slow_down.
	interval time.Duration
	// nextPoll is the earliest time the next token poll is worth sending.
	nextPoll time.Time
	polls    int
}

func deviceStateOf(s *deviceSession) core.LoginState {
	return core.LoginState{
		SessionID: s.id,
		State:     s.state,
		URL:       s.verifyURL,
		Code:      s.userCode,
		Message:   s.message,
		AccountID: s.accountID,
	}
}

func (s *accountStore) putDevice(sess *deviceSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.device == nil {
		s.device = map[string]*deviceSession{}
	}
	s.device[sess.id] = sess
}

func (s *accountStore) getDevice(id string) (*deviceSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.device[id]
	return sess, ok
}

// ---------------------------------------------------------------------------
// core.LoginProvider dispatch
//
// The device flow is the default.  The guided CLI flow is still reachable -
// sessions are routed by their id prefix, and StartLogin consults login_mode -
// so an operator who prefers the CLI loses nothing.
// ---------------------------------------------------------------------------

// StartLogin implements core.LoginProvider.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	if c.cfg.loginMode() == loginModeCLI {
		return c.startCLILogin(ctx)
	}
	return c.startDeviceLogin(ctx)
}

// PollLogin implements core.LoginProvider.
func (c *Client) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return core.LoginState{}, errors.New("kimi: PollLogin: empty session id")
	}
	if strings.HasPrefix(id, deviceSessionPrefix) {
		st, err := c.pollDeviceLogin(ctx, id)
		if err == nil && st.State == core.LoginSuccess {
			// A fresh grant is new positive evidence about kimi-web, and it
			// arrives from outside the account machinery: the device-session
			// lock above is not held here, so a dead web login is revived right
			// away instead of waiting for the operator to toggle the row.
			c.revive(webLoginID)
		}
		return st, err
	}
	return c.pollCLILogin(ctx, id)
}

// CancelLogin implements core.LoginProvider.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return errors.New("kimi: CancelLogin: empty session id")
	}
	if strings.HasPrefix(id, deviceSessionPrefix) {
		return c.cancelDeviceLogin(id)
	}
	return c.cancelCLILogin(ctx, id)
}

// ---------------------------------------------------------------------------
// Device flow
// ---------------------------------------------------------------------------

func (c *Client) startDeviceLogin(ctx context.Context) (core.LoginState, error) {
	sess := &deviceSession{
		id:        deviceSessionPrefix + randHex(12),
		startedAt: time.Now(),
		state:     core.LoginPending,
		oauthHost: c.oauthHost(),
	}

	da, err := c.requestDeviceCode(ctx)
	if err != nil {
		// A failure here is an operator-visible fact (no network, vendor down,
		// client id retired), not a programming error, so it is reported as a
		// failed session rather than as an error return.
		sess.state = core.LoginFailed
		sess.message = core.Redact(err.Error())
		c.acct.putDevice(sess)
		return deviceStateOf(sess), nil
	}

	expiry := defaultDeviceExpiry
	if da.ExpiresIn > 0 {
		expiry = time.Duration(da.ExpiresIn) * time.Second
	}
	interval := defaultDeviceInterval
	if da.Interval > 0 {
		interval = time.Duration(da.Interval) * time.Second
	}

	sess.deviceCode = da.DeviceCode
	sess.userCode = da.UserCode
	sess.verifyURL = da.VerificationURIComplete
	sess.expiresAt = time.Now().Add(expiry)
	sess.interval = interval
	sess.nextPoll = time.Now()
	sess.message = fmt.Sprintf(
		"Open %s in a browser and confirm the code %s. This module talks to %s directly, so the kimi CLI is not needed. "+
			"The code expires in %s.",
		sess.verifyURL, sess.userCode, sess.oauthHost, expiry.Round(time.Second))
	if _, have := c.loadToken(); have {
		// "I signed in, so where is the account?" is a fair question: the panel
		// login is a single account (kimi-web), so signing in again *replaces*
		// its token rather than adding a second row.  Say so, and name the row,
		// before the operator goes looking for a new one.
		sess.message += fmt.Sprintf(" A panel login for %s is already stored at %s; confirming this code replaces it, "+
			"and the account stays a single row in the table (delete that row to sign out instead).",
			webLoginID, c.tokenPath())
	}

	c.acct.putDevice(sess)
	return deviceStateOf(sess), nil
}

func (c *Client) pollDeviceLogin(ctx context.Context, id string) (core.LoginState, error) {
	sess, ok := c.acct.getDevice(id)
	if !ok {
		return core.LoginState{}, fmt.Errorf("kimi: unknown login session %q (it may have been cancelled, or this process was restarted; start a new login)", id)
	}

	c.acct.mu.Lock()
	if sess.state != core.LoginPending {
		state := deviceStateOf(sess)
		c.acct.mu.Unlock()
		return state, nil
	}
	// Read what the request needs, then release the lock: the token poll is a
	// network call and must not hold the store.
	host, deviceCode := sess.oauthHost, sess.deviceCode
	expiresAt, nextPoll := sess.expiresAt, sess.nextPoll
	c.acct.mu.Unlock()

	if !expiresAt.IsZero() && time.Now().After(expiresAt) {
		return c.failDevice(id, "the device code expired before it was confirmed; start a new login"), nil
	}
	// Honour the vendor's pacing.  The panel polls on its own schedule and may
	// poll faster than the vendor allows; answering "still pending" locally
	// avoids burning the rate limit.
	if !nextPoll.IsZero() && time.Now().Before(nextPoll) {
		sess, ok := c.acct.getDevice(id)
		if !ok {
			return core.LoginState{}, fmt.Errorf("kimi: unknown login session %q", id)
		}
		c.acct.mu.Lock()
		state := deviceStateOf(sess)
		c.acct.mu.Unlock()
		return state, nil
	}

	res, err := c.requestToken(ctx, host, deviceCode)
	if err != nil {
		// Transport-level failure: leave the session pending so a transient
		// outage does not kill a login the operator is halfway through.
		c.acct.mu.Lock()
		sess.polls++
		sess.nextPoll = time.Now().Add(sess.interval)
		state := deviceStateOf(sess)
		c.acct.mu.Unlock()
		state.Message = core.Redact(err.Error()) + " Retrying; this session is still pending."
		return state, nil
	}

	switch res.Error {
	case "":
		return c.finishDeviceLogin(id, host, res), nil

	case errAuthorizationPending:
		c.acct.mu.Lock()
		sess.polls++
		sess.nextPoll = time.Now().Add(sess.interval)
		state := deviceStateOf(sess)
		c.acct.mu.Unlock()
		return state, nil

	case errSlowDown:
		// RFC 8628: increase the interval by 5 seconds and keep waiting.
		c.acct.mu.Lock()
		sess.polls++
		sess.interval += slowDownIncrement
		sess.nextPoll = time.Now().Add(sess.interval)
		sess.message = fmt.Sprintf("Open %s in a browser and confirm the code %s. The vendor asked this module to slow down; polling every %s.",
			sess.verifyURL, sess.userCode, sess.interval.Round(time.Second))
		state := deviceStateOf(sess)
		c.acct.mu.Unlock()
		return state, nil

	case errExpiredToken:
		return c.failDevice(id, "the device code expired before it was confirmed; start a new login"), nil

	case errAccessDenied:
		return c.failDevice(id, "the authorization request was denied in the browser"), nil

	default:
		return c.failDevice(id, fmt.Sprintf("the authorization server rejected the request: %s", core.Redact(res.Error))), nil
	}
}

func (c *Client) failDevice(id, message string) core.LoginState {
	c.acct.mu.Lock()
	defer c.acct.mu.Unlock()
	sess, ok := c.acct.device[id]
	if !ok {
		return core.LoginState{SessionID: id, State: core.LoginFailed, Message: message}
	}
	sess.state = core.LoginFailed
	sess.message = message
	return deviceStateOf(sess)
}

func (c *Client) finishDeviceLogin(id, host string, res tokenResponse) core.LoginState {
	tok := storedToken{
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		TokenType:    res.TokenType,
		Scope:        res.Scope,
		OAuthHost:    host,
		ObtainedAt:   time.Now(),
	}
	if res.ExpiresIn > 0 {
		tok.ExpiresAt = time.Now().Add(time.Duration(res.ExpiresIn) * time.Second).Unix()
	}

	c.acct.mu.Lock()
	defer c.acct.mu.Unlock()

	sess, ok := c.acct.device[id]
	if !ok {
		return core.LoginState{SessionID: id, State: core.LoginFailed, Message: "the login session disappeared before it completed"}
	}

	if err := c.saveToken(tok); err != nil {
		sess.state = core.LoginFailed
		sess.message = "signed in, but the token could not be stored: " + core.Redact(err.Error())
		return deviceStateOf(sess)
	}

	sess.state = core.LoginSuccess
	sess.accountID = webLoginID
	sess.message = fmt.Sprintf("signed in via %s. The token is stored at %s and is used for direct HTTPS calls; the kimi CLI is not required.",
		host, c.tokenPath())
	c.run.invalidateCredCache()
	return deviceStateOf(sess)
}

func (c *Client) cancelDeviceLogin(id string) error {
	c.acct.mu.Lock()
	defer c.acct.mu.Unlock()
	sess, ok := c.acct.device[id]
	if !ok {
		return fmt.Errorf("kimi: unknown login session %q", id)
	}
	sess.state = core.LoginCancelled
	sess.message = "cancelled by the operator. If the code was already confirmed in the browser, the grant stays valid at the vendor and a new login will simply be refused."
	return nil
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func (c *Client) requestDeviceCode(ctx context.Context) (deviceAuthResponse, error) {
	var out deviceAuthResponse

	form := url.Values{}
	form.Set("client_id", c.oauthClientID())

	endpoint := c.oauthHost() + deviceAuthPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return out, fmt.Errorf("kimi: building the device authorization request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// The CLI hand-writes its OAuth form posts rather than going through its
	// generated SDK, so they carry the device block and product User-Agent but
	// none of the x-stainless-* set.  See identity.go.
	applyIdentityHeaders(req.Header, c.identity(), false)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return out, fmt.Errorf("kimi: the device authorization request to %s failed: %w", c.oauthHost(), err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, fmt.Errorf("kimi: reading the device authorization response failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("kimi: device authorization failed (HTTP %d)%s", resp.StatusCode, upstreamDetail(body))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("kimi: the device authorization response was not JSON: %w", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return out, errors.New("kimi: the device authorization response carried no device code")
	}
	if out.VerificationURIComplete == "" {
		out.VerificationURIComplete = out.VerificationURI
	}
	return out, nil
}

// requestToken sends one device-code token poll.  A returned error means the
// exchange could not be judged at all (transport, malformed body); a judged
// answer - success or an RFC 8628 error code - comes back in the value.
func (c *Client) requestToken(ctx context.Context, host, deviceCode string) (tokenResponse, error) {
	var out tokenResponse

	form := url.Values{}
	form.Set("client_id", c.oauthClientID())
	form.Set("device_code", deviceCode)
	form.Set("grant_type", deviceGrantType)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+deviceTokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return out, fmt.Errorf("kimi: building the token request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// The CLI hand-writes its OAuth form posts rather than going through its
	// generated SDK, so they carry the device block and product User-Agent but
	// none of the x-stainless-* set.  See identity.go.
	applyIdentityHeaders(req.Header, c.identity(), false)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return out, fmt.Errorf("kimi: the token request to %s failed: %w", host, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, fmt.Errorf("kimi: reading the token response failed: %w", err)
	}
	if resp.StatusCode >= 500 {
		return out, fmt.Errorf("kimi: the authorization server is unavailable (HTTP %d)%s", resp.StatusCode, upstreamDetail(body))
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("kimi: the token response was not JSON (HTTP %d)%s", resp.StatusCode, upstreamDetail(body))
	}
	if out.AccessToken == "" && out.Error == "" {
		return out, fmt.Errorf("kimi: the token response was neither a token nor an error (HTTP %d)%s", resp.StatusCode, upstreamDetail(body))
	}
	return out, nil
}

// renewToken exchanges the stored refresh token for a new access token.
//
// The device flow's access token is short lived (the vendor states 30 minutes),
// while its refresh token is not, so a credential obtained from the panel has to
// be renewed or the account dies within the hour and looks broken.  The refresh
// grant is the only renewal this module can attempt; a vendor that answers with
// an OAuth error leaves the caller to report "sign in again".
//
// The refreshed grant is persisted before it is returned.  A vendor that rotates
// refresh tokens invalidates the stored one the moment it answers, so a renewal
// that succeeded but could not be written is worse than one that never ran -
// that case is reported as an error naming the path.
func (c *Client) renewToken(ctx context.Context, tok storedToken) (storedToken, error) {
	var out tokenResponse

	host := trimHost(tok.OAuthHost)
	if host == "" {
		host = c.oauthHost()
	}

	form := url.Values{}
	form.Set("client_id", c.oauthClientID())
	form.Set("grant_type", refreshGrantType)
	form.Set("refresh_token", tok.RefreshToken)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+deviceTokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		return tok, fmt.Errorf("kimi: building the refresh request failed: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// The CLI hand-writes its OAuth form posts rather than going through its
	// generated SDK, so they carry the device block and product User-Agent but
	// none of the x-stainless-* set.  See identity.go.
	applyIdentityHeaders(req.Header, c.identity(), false)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return tok, fmt.Errorf("kimi: the refresh request to %s failed: %w", host, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tok, fmt.Errorf("kimi: reading the refresh response failed: %w", err)
	}
	if resp.StatusCode >= 500 {
		// Text is spelled out rather than composed from Where/Status so the
		// wording the panel has always shown does not change; the status is
		// still carried, which is what keeps this out of the "dead credential"
		// bucket (a 5xx is transient).
		return tok, &upstreamStatusError{
			Status: resp.StatusCode,
			Text:   fmt.Sprintf("kimi: the authorization server is unavailable (HTTP %d)%s", resp.StatusCode, upstreamDetail(body)),
		}
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return tok, &upstreamStatusError{
			Status: resp.StatusCode,
			Text:   fmt.Sprintf("kimi: the refresh response was not JSON (HTTP %d)%s", resp.StatusCode, upstreamDetail(body)),
		}
	}
	if out.AccessToken == "" {
		detail := upstreamDetail(body)
		if detail == "" && out.Error != "" {
			detail = ": " + core.Redact(strings.Join(strings.Fields(out.Error+" "+out.ErrorDescription), " "))
		}
		// The vendor answered, understood the grant and refused it.  That is the
		// one vendor answer this module remembers as dead: no retry can fix it,
		// only a new sign-in can.
		return tok, &credentialRefusedError{Status: resp.StatusCode, Detail: detail}
	}

	next := tok
	next.AccessToken = out.AccessToken
	// A rotation replaces the refresh token; a server that repeats the old one
	// (or omits it) keeps what we already hold.
	if v := strings.TrimSpace(out.RefreshToken); v != "" {
		next.RefreshToken = v
	}
	if out.ExpiresIn > 0 {
		next.ExpiresAt = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second).Unix()
	}
	if out.Scope != "" {
		next.Scope = out.Scope
	}
	if out.TokenType != "" {
		next.TokenType = out.TokenType
	}
	next.ObtainedAt = time.Now().UTC()

	if err := c.saveToken(next); err != nil {
		return tok, fmt.Errorf("kimi: the refreshed token could not be stored, so it will have to be obtained again: %w", err)
	}
	return next, nil
}

// upstreamDetail renders an upstream error body for a human, redacted and
// bounded.  Upstream text is untrusted and can echo a token back at us.
func upstreamDetail(body []byte) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return ""
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return ": " + core.Redact(strings.Join(strings.Fields(s), " "))
}
