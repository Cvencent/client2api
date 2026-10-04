package minimaxcode

// This file implements the panel's sign-in for MiniMax Code.
//
// The desktop client authenticates with the OAuth 2.0 Device Authorization
// Grant (RFC 8628), not with a loopback redirect, so there is nothing to listen
// on and nothing to launch: the module asks the vendor for a device code, hands
// the operator the verification URL and the short user code, and polls the token
// endpoint until the vendor says the sign-in was approved.  That is the same
// shape cline uses, so the panel's existing "设备码" rendering drives it with no
// new UI.
//
// The protocol was read off the desktop client's own bundle rather than guessed:
//
//	POST <accountOrigin>/oauth2/device/code
//	  client_id=mcode-public&scope=agent.default&audience=agent-backend
//	  &code_challenge=<S256>&code_challenge_method=S256
//	POST <accountOrigin>/oauth2/token
//	  grant_type=urn:ietf:params:oauth:grant-type:device_code
//	  &device_code=…&client_id=mcode-public&code_verifier=…
//
// Two vendor quirks are handled because the desktop client handles them: the
// token endpoint may answer a non-standard `status` field instead of an OAuth
// `error`, and the CN service has been seen to return a user code plus
// `expired_in` in milliseconds instead of a device code, in which case polling
// is keyed by the user code.  The PKCE verifier is generated either way.
//
// This module never launches a browser: it hands the URL to the panel, which is
// where the operator already is, so the flow also works over a remote
// connection.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

const (
	// mcodeScope and mcodeAudience are the values the desktop client sends.
	// They are not configurable: the vendor scopes the credential to this
	// audience, and a token minted for another one is refused by the model
	// endpoint.
	mcodeScope    = "agent.default"
	mcodeAudience = "agent-backend"

	// deviceGrantType is RFC 8628's URN, which this vendor implements verbatim.
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// defaultDeviceInterval is what the vendor's own client uses when the
	// device authorization response carries no interval.
	defaultDeviceInterval = 5 * time.Second

	// maxDeviceInterval caps the backoff a `slow_down` answer can produce.
	maxDeviceInterval = 60 * time.Second

	// fallbackLoginWindow bounds a sign-in whose response carried no expiry.
	fallbackLoginWindow = 10 * time.Minute

	// loginRequestTimeout bounds one HTTP call.  It is deliberately much
	// shorter than the sign-in window: a hung token poll must not make the
	// panel wait forever for an answer it can retry.
	loginRequestTimeout = 30 * time.Second
)

// loginSession is one in-flight device-code sign-in.
type loginSession struct {
	id           string
	verifier     string
	deviceCode   string
	userCode     string
	verification string

	// pollAs names the parameter the token endpoint is polled with.  It is
	// "device_code" for the standard flow and "user_code" for the CN variant
	// described above; the two are mutually exclusive.
	pollAs string

	// interval is how long the vendor asked us to wait between polls and
	// expiresAt when it stops accepting the authorization at all.
	interval  time.Duration
	expiresAt time.Time

	mu        sync.Mutex
	status    string
	message   string
	accountID string
	nextPoll  time.Time
	inFlight  bool
}

// StartLogin asks the vendor for a device authorization and returns what the
// operator has to do with it.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint := strings.TrimSpace(c.cfg.oauthDeviceURL())
	if endpoint == "" {
		return core.LoginState{}, errors.New("minimaxcode: no device authorization endpoint is configured")
	}

	verifier, err := randomVerifier()
	if err != nil {
		return core.LoginState{}, fmt.Errorf("minimaxcode: no entropy for a sign-in: %w", err)
	}

	form := url.Values{
		"client_id":             {oauthClientIDDesktop},
		"scope":                 {mcodeScope},
		"audience":              {mcodeAudience},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	body, err := c.oauthForm(ctx, endpoint, form)
	if err != nil {
		return core.LoginState{}, err
	}
	if code := stringField(body, "error"); code != "" {
		return core.LoginState{}, fmt.Errorf("minimaxcode: the vendor refused the device authorization (%s)%s",
			code, describeError(body))
	}

	userCode := stringField(body, "user_code")
	deviceCode := stringField(body, "device_code")
	verification := firstNonEmpty(
		stringField(body, "verification_uri"),
		stringField(body, "verification_url"),
	)
	if complete := stringField(body, "verification_uri_complete"); complete != "" {
		verification = complete
	}

	// The CN service has answered with user_code + expired_in (milliseconds)
	// and no device code.  That shape is not RFC 8628, but the desktop client
	// polls it by user code, so doing anything else would fail on exactly the
	// accounts this module exists for.
	expiredInMs, hasExpiredIn := numberField(body, "expired_in")
	expiresInSec, hasExpiresIn := numberField(body, "expires_in")
	pollAs := "device_code"
	if deviceCode == "" && userCode != "" && hasExpiredIn {
		deviceCode = userCode
		pollAs = "user_code"
	}

	if userCode == "" || deviceCode == "" || verification == "" {
		return core.LoginState{}, errors.New("minimaxcode: the vendor's device authorization response was incomplete")
	}

	interval := defaultDeviceInterval
	if raw, ok := numberField(body, "interval"); ok && raw > 0 {
		// In the user-code variant the interval is milliseconds; in the
		// standard one it is seconds.  The desktop client makes the same split.
		if pollAs == "user_code" {
			interval = time.Duration(raw) * time.Millisecond
		} else {
			interval = time.Duration(raw) * time.Second
		}
	}
	if interval <= 0 {
		interval = defaultDeviceInterval
	}
	if interval > maxDeviceInterval {
		interval = maxDeviceInterval
	}

	window := fallbackLoginWindow
	switch {
	case hasExpiresIn && expiresInSec > 0:
		window = time.Duration(expiresInSec) * time.Second
	case hasExpiredIn && expiredInMs > 0:
		window = time.Duration(expiredInMs) * time.Millisecond
	}

	sessionID, err := randomSessionID()
	if err != nil {
		return core.LoginState{}, fmt.Errorf("minimaxcode: no entropy for a sign-in session: %w", err)
	}
	now := time.Now()
	s := &loginSession{
		id:           sessionID,
		verifier:     verifier,
		deviceCode:   deviceCode,
		userCode:     userCode,
		verification: verification,
		pollAs:       pollAs,
		interval:     interval,
		expiresAt:    now.Add(window),
		status:       core.LoginPending,
		// The operator needs a moment to open the page at all, so the first
		// poll waits one interval like every other one.
		nextPoll: now.Add(interval),
	}
	c.putLogin(s)
	c.logf("minimaxcode: sign-in %s started, %s expires at %s", sessionID, verification, s.expiresAt.Format(time.RFC3339))
	return s.snapshot(), nil
}

// PollLogin reports the session, asking the vendor at most once per interval.
//
// The panel polls every two seconds, which is faster than the vendor wants, so
// this method is the rate limiter: a call that arrives early answers from the
// session instead of minting a request the vendor answers with `slow_down`.
func (c *Client) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sessionID = strings.TrimSpace(sessionID)
	s, ok := c.getLogin(sessionID)
	if !ok {
		return core.LoginState{}, fmt.Errorf("minimaxcode: no sign-in session %q", sessionID)
	}

	s.mu.Lock()
	status := s.status
	nextPoll := s.nextPoll
	expired := !s.expiresAt.IsZero() && time.Now().After(s.expiresAt)
	s.mu.Unlock()

	if status != core.LoginPending {
		return s.snapshot(), nil
	}
	if expired {
		s.finish(core.LoginFailed, "设备码已过期，请重新获取", "")
		return s.snapshot(), nil
	}
	if time.Now().Before(nextPoll) {
		return s.snapshot(), nil
	}
	return c.pollDeviceToken(ctx, s)
}

// CancelLogin forgets a sign-in.  Cancelling an unknown or already finished
// session is not an error: the panel may cancel a flow the vendor already
// resolved, and making that fail would only produce noise.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	_ = ctx
	sessionID = strings.TrimSpace(sessionID)
	if s, ok := c.getLogin(sessionID); ok {
		s.finish(core.LoginCancelled, "已取消", "")
	}
	c.dropLogin(sessionID)
	return nil
}

// pollDeviceToken performs exactly one token-endpoint exchange.
func (c *Client) pollDeviceToken(ctx context.Context, s *loginSession) (core.LoginState, error) {
	s.mu.Lock()
	if s.inFlight {
		// Another poll is already talking to the vendor; reporting the
		// current state is honest and avoids a duplicate request.
		s.mu.Unlock()
		return s.snapshot(), nil
	}
	s.inFlight = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inFlight = false
		s.mu.Unlock()
	}()

	asked := s.pollAs
	form := url.Values{
		"grant_type":    {deviceGrantType},
		"client_id":     {oauthClientIDDesktop},
		"code_verifier": {s.verifier},
	}
	if asked == "user_code" {
		form.Set("user_code", s.userCode)
	} else {
		form.Set("device_code", s.deviceCode)
	}

	body, err := c.oauthForm(ctx, c.cfg.oauthTokenURL(), form)
	if err != nil {
		// A transport failure is not a verdict: leave the session pending and
		// let the next poll try again, after backing off so a dead network does
		// not turn into a request storm.
		s.backOff("the token endpoint could not be reached: " + err.Error())
		return s.snapshot(), nil
	}

	status := strings.ToLower(stringField(body, "status"))
	switch status {
	case "pending":
		s.advance()
		return s.snapshot(), nil
	case "slow_down":
		s.slowDown()
		return s.snapshot(), nil
	case "denied", "access_denied":
		s.finish(core.LoginFailed, "你取消了这次授权", "")
		return s.snapshot(), nil
	case "expired", "expired_token":
		s.finish(core.LoginFailed, "设备码已过期，请重新获取", "")
		return s.snapshot(), nil
	}

	switch strings.ToLower(stringField(body, "error")) {
	case "":
		// No error: this is the grant.
	case "authorization_pending":
		s.advance()
		return s.snapshot(), nil
	case "slow_down":
		s.slowDown()
		return s.snapshot(), nil
	case "access_denied":
		s.finish(core.LoginFailed, "你取消了这次授权", "")
		return s.snapshot(), nil
	case "expired_token":
		s.finish(core.LoginFailed, "设备码已过期，请重新获取", "")
		return s.snapshot(), nil
	default:
		s.finish(core.LoginFailed, "授权失败："+stringField(body, "error")+describeError(body), "")
		return s.snapshot(), nil
	}

	access := stringField(body, "access_token")
	if access == "" {
		s.finish(core.LoginFailed, "厂商返回的凭据里没有 access token", "")
		return s.snapshot(), nil
	}
	refresh := stringField(body, "refresh_token")

	expiresAt := time.Time{}
	switch {
	case numberFieldOK(body, "expiresAtMs"):
		ms, _ := numberField(body, "expiresAtMs")
		expiresAt = time.UnixMilli(int64(ms))
	case numberFieldOK(body, "expires_in"):
		sec, _ := numberField(body, "expires_in")
		if sec > 0 {
			expiresAt = time.Now().Add(time.Duration(sec) * time.Second)
		}
	}

	region := c.cfg.oauthRegion()
	id := accountID("prod", region, oauthClientIDDesktop)
	label := firstNonEmpty(desktopUserLabel(), fmt.Sprintf("MiniMax Code (%s)", region))

	rec, err := c.adoptLoginCredential(id, label, oauthClientIDDesktop, access, refresh, expiresAt)
	if err != nil {
		s.finish(core.LoginFailed, err.Error(), "")
		return s.snapshot(), nil
	}

	message := ""
	if strings.TrimSpace(refresh) == "" {
		// Worth saying out loud: without a refresh token the row works until the
		// access token expires and then has to be signed in again by hand.
		message = "厂商没有返回 refresh token，这个账号到期后需要重新登录"
	}
	s.finish(core.LoginSuccess, message, rec.ID)
	c.logf("minimaxcode: sign-in %s finished as %s", s.id, rec.ID)
	return s.snapshot(), nil
}

// snapshot renders the session for the panel.
func (s *loginSession) snapshot() core.LoginState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return core.LoginState{
		SessionID: s.id,
		State:     s.status,
		URL:       s.verification,
		Code:      s.userCode,
		Message:   s.message,
		AccountID: s.accountID,
	}
}

// finish moves a session to a terminal state exactly once, so a panel that
// polls a finished flow cannot restart or overwrite the outcome.
func (s *loginSession) finish(state, message, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != core.LoginPending {
		return
	}
	s.status = state
	s.message = message
	s.accountID = accountID
}

// advance schedules the next poll one interval out, which is what the vendor
// asked for when it answered "pending".
func (s *loginSession) advance() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextPoll = time.Now().Add(s.interval)
}

// slowDown widens the interval, which is the only correct answer to the
// vendor's "slow_down" and the reason polling too eagerly makes a sign-in take
// longer rather than finish sooner.
func (s *loginSession) slowDown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interval += defaultDeviceInterval
	if s.interval > maxDeviceInterval {
		s.interval = maxDeviceInterval
	}
	s.nextPoll = time.Now().Add(s.interval)
}

// backOff reschedules after a transport failure without inventing a new
// interval, and records the reason so a stuck sign-in is explainable.
func (s *loginSession) backOff(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.message = reason
	s.nextPoll = time.Now().Add(s.interval)
}

// oauthForm posts an application/x-www-form-urlencoded request and returns the
// JSON object the endpoint answered with.
//
// An OAuth error is a 200-or-4xx body carrying "error", not a transport failure,
// so this returns the body for the caller to classify and only reports an error
// when there is no readable body at all.
func (c *Client) oauthForm(ctx context.Context, endpoint string, form url.Values) (map[string]any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, errors.New("minimaxcode: no oauth endpoint is configured")
	}

	reqCtx, cancel := context.WithTimeout(ctx, loginRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("minimaxcode: cannot build the oauth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if defaultUserAgent != "" {
		req.Header.Set("User-Agent", defaultUserAgent)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("minimaxcode: the oauth endpoint could not be reached: %w", err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()

	body, derr := decodeJSONObject(raw)
	if derr != nil || body == nil {
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("minimaxcode: the oauth endpoint answered HTTP %d with an unreadable body", resp.StatusCode)
		}
		return nil, errors.New("minimaxcode: the oauth endpoint returned an unreadable body")
	}
	return body, nil
}

// decodeJSONObject decodes a JSON object with numbers kept exact, so a large
// epoch-millisecond value does not lose its low bits to a float64 on the way
// through.
func decodeJSONObject(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func stringField(doc map[string]any, key string) string {
	if doc == nil {
		return ""
	}
	v, ok := doc[key]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		return t.String()
	}
	return ""
}

// numberField reads a number that may also arrive as a numeric string, because
// OAuth endpoints are split on whether expires_in is "600" or 600.
func numberField(doc map[string]any, key string) (float64, bool) {
	if doc == nil {
		return 0, false
	}
	switch t := doc[key].(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

func numberFieldOK(doc map[string]any, key string) bool {
	_, ok := numberField(doc, key)
	return ok
}

// describeError appends the vendor's own explanation when it sent one.  It is
// the part an operator can act on ("the account is suspended in another
// region", say), so it is worth carrying into the panel.
func describeError(doc map[string]any) string {
	if doc == nil {
		return ""
	}
	desc := firstNonEmpty(stringField(doc, "error_description"), stringField(doc, "message"))
	if desc == "" {
		return ""
	}
	return "：" + truncate(desc, 200)
}

func randomVerifier() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// randomSessionID is opaque to the vendor: it exists so the panel can name one
// sign-in out of several.
func randomSessionID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "minimaxcode-login-" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// pkceChallenge is S256: base64url(sha256(verifier)) with no padding.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// The panel discovers the sign-in entry point by this interface, so the
// compiler enforces that this module keeps implementing it.
var _ core.LoginProvider = (*Client)(nil)
