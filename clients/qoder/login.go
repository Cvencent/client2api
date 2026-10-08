package qoder

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// login.go implements core.LoginProvider: the panel hands the operator the
// vendor's own browser sign-in URL, then polls until the desktop client's
// device grant is confirmed.
//
// The flow is the one the Qoder CN Electron app runs (read out of its bundle,
// not guessed): a PKCE verifier/challenge pair, a random nonce and a stable
// machine id are put into /device/selectAccounts, and that device URL is
// wrapped in /users/sign-in as oauth_callback.  The token poll answers 404
// until the browser finishes and then returns the same device token the desktop
// client would have written to its safeStorage blob, so a panel login lands on
// exactly the account a local import would have produced.
//
// Nothing here mints a credential the operator did not ask for: the browser
// grant is confirmed by the vendor, and the refresh token is stored but never
// used (see accounts.go for why).

const (
	// pathDevicePoll is the OpenAPI route the browser grant is polled from.
	pathDevicePoll = "/api/v1/deviceToken/poll"
	// authDevicePath is the browser page that picks an account and confirms
	// the device grant.
	authDevicePath = "/device/selectAccounts"
	// authSignInPath is the page that wraps the device URL; it is what the
	// operator actually opens.
	authSignInPath = "/users/sign-in"
	// pkceVerifierLen matches the desktop client: 64 characters from the
	// RFC 7636 unreserved set.
	pkceVerifierLen = 64
	// loginSessionTTL is the desktop client's own device-window length.
	loginSessionTTL = 5 * time.Minute
)

const pkceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// deviceToken is the answer to one successful poll.  expires_at and
// refresh_token_expires_at are ISO strings; the *_in fields are the fallback
// the desktop client uses when the server omits them.
type deviceToken struct {
	Token                 string `json:"token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresAt             string `json:"expires_at"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresAt string `json:"refresh_token_expires_at"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
}

// loginSession is one in-flight browser grant.  It lives in memory only: a
// login that survives a restart is a login nobody can reason about.
type loginSession struct {
	id        string
	verifier  string
	nonce     string
	url       string
	expiresAt time.Time

	mu        sync.Mutex
	state     string
	message   string
	accountID string
	polling   bool
	cancelled bool
}

// terminal reports whether the session has reached a state it can never leave.
func (s *loginSession) terminal() bool {
	switch s.state {
	case core.LoginSuccess, core.LoginFailed, core.LoginCancelled:
		return true
	}
	return false
}

func (s *loginSession) snapshot() core.LoginState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, message := s.state, s.message
	if state == core.LoginPending && time.Now().After(s.expiresAt) {
		state, message = core.LoginFailed, "登录等待已超时，请重新发起登录。"
		s.state, s.message = state, message
	}
	return core.LoginState{
		SessionID: s.id,
		State:     state,
		URL:       s.url,
		Message:   core.Redact(message),
		AccountID: s.accountID,
	}
}

func (s *loginSession) succeed(accountID, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled {
		return
	}
	s.state = core.LoginSuccess
	s.accountID = accountID
	s.message = core.Redact(message)
}

func (s *loginSession) fail(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled || s.terminal() {
		return
	}
	s.state = core.LoginFailed
	s.message = core.Redact(message)
}

func (s *loginSession) note(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled || s.terminal() {
		return
	}
	s.message = core.Redact(message)
}

func (s *loginSession) markCancelled() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled = true
	s.state = core.LoginCancelled
	s.message = "已取消登录。"
	s.accountID = ""
}

// beginPoll claims the right to talk to the vendor, or reports that another
// poll, a cancel, an expired window or a finished session already owns it.
func (s *loginSession) beginPoll() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.polling || s.cancelled || s.terminal() {
		return false
	}
	if time.Now().After(s.expiresAt) {
		s.state, s.message = core.LoginFailed, "登录等待已超时，请重新发起登录。"
		return false
	}
	s.polling = true
	return true
}

func (s *loginSession) endPoll() {
	s.mu.Lock()
	s.polling = false
	s.mu.Unlock()
}

// StartLogin builds the vendor sign-in URL and records the pending session.
// It makes no network call: the panel's poll is what talks to the vendor.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	verifier := randomPKCEVerifier()
	nonce := newUUID()
	sess := &loginSession{
		id:        "qoder-login-" + nonce,
		verifier:  verifier,
		nonce:     nonce,
		url:       c.deviceLoginURL(pkceS256(verifier), nonce),
		expiresAt: time.Now().Add(loginSessionTTL),
		state:     core.LoginPending,
		message:   "在浏览器里打开登录链接并完成登录；完成后回到面板，稍等片刻即可看到账号。",
	}
	c.putLogin(sess)
	return sess.snapshot(), nil
}

// PollLogin advances a session by exactly one vendor poll.  The panel owns the
// cadence, so this method never starts a background poller.
func (c *Client) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return core.LoginState{}, errors.New("qoder: PollLogin: empty session id")
	}
	sess := c.loginByID(id)
	if sess == nil {
		return core.LoginState{}, fmt.Errorf("qoder: unknown login session %q", id)
	}
	if st := sess.snapshot(); st.State != core.LoginPending {
		return st, nil
	}
	if !sess.beginPoll() {
		return sess.snapshot(), nil
	}
	defer sess.endPoll()

	tok, pending, err := c.pollDeviceToken(ctx, sess.nonce, sess.verifier)
	switch {
	case err != nil:
		sess.fail("查询登录结果失败：" + redactErr(err) + "；请重新发起登录。")
		return sess.snapshot(), nil
	case pending:
		sess.note("等待浏览器里确认登录……")
		return sess.snapshot(), nil
	}

	acc, err := c.accountFromDeviceToken(ctx, tok)
	if err != nil {
		sess.fail(redactErr(err))
		return sess.snapshot(), nil
	}
	if err := c.store.put(acc); err != nil {
		sess.fail("登录成功，但凭据无法写入：" + redactErr(err))
		return sess.snapshot(), nil
	}
	sess.succeed(acc.ID, "登录成功，凭据已保存")
	return sess.snapshot(), nil
}

// CancelLogin stops a session.  Cancelling before the vendor hands over a token
// leaves nothing on disk.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return errors.New("qoder: CancelLogin: empty session id")
	}
	sess := c.loginByID(id)
	if sess == nil {
		return fmt.Errorf("qoder: unknown login session %q", id)
	}
	sess.markCancelled()
	return nil
}

// ---------------------------------------------------------------------------
// Session bookkeeping
// ---------------------------------------------------------------------------

func (c *Client) putLogin(sess *loginSession) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.logins == nil {
		c.logins = make(map[string]*loginSession)
	}
	now := time.Now()
	for id, other := range c.logins {
		if now.After(other.expiresAt) {
			delete(c.logins, id)
		}
	}
	c.logins[sess.id] = sess
}

func (c *Client) loginByID(id string) *loginSession {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.logins[id]
}

// ---------------------------------------------------------------------------
// URL construction
// ---------------------------------------------------------------------------

// deviceLoginURL renders the two nested URLs the desktop client opens.  The
// auth base is parsed rather than string-joined so an operator override with a
// port or a sub-path still produces a valid URL.
func (c *Client) deviceLoginURL(challenge, nonce string) string {
	base, err := url.Parse(c.cfg.authBase())
	if err != nil || base.Scheme == "" || base.Host == "" {
		base = &url.URL{Scheme: "https", Host: "qoder.cn"}
	}

	device := url.URL{Scheme: base.Scheme, Host: base.Host, Path: authDevicePath}
	deviceQuery := url.Values{}
	deviceQuery.Set("challenge", challenge)
	deviceQuery.Set("challenge_method", "S256")
	deviceQuery.Set("nonce", nonce)
	deviceQuery.Set("machine_id", c.machineID())
	deviceQuery.Set("client_id", c.cfg.authClientID())
	device.RawQuery = deviceQuery.Encode()

	signIn := url.URL{Scheme: base.Scheme, Host: base.Host, Path: authSignInPath}
	signInQuery := url.Values{}
	signInQuery.Set("biz_variant", authBizVariant)
	signInQuery.Set("oauth_callback", device.String())
	signIn.RawQuery = signInQuery.Encode()
	return signIn.String()
}

// machineID returns the stable device identity the login URL carries.  It is
// cached in memory and persisted under DataDir so a restart talks to the vendor
// as the same device instead of a brand-new one.
func (c *Client) machineID() string {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.machineIDCache != "" {
		return c.machineIDCache
	}
	if v := strings.TrimSpace(os.Getenv("CLIENT2API_QODER_MACHINE_ID")); uuidPattern.MatchString(v) {
		c.machineIDCache = v
		return v
	}
	path := c.machineIDPath()
	if path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			if v := strings.TrimSpace(string(raw)); uuidPattern.MatchString(v) {
				c.machineIDCache = v
				return v
			}
		}
	}
	id := newUUID()
	if path != "" {
		if err := core.WriteFileAtomic(path, []byte(id+"\n")); err != nil {
			c.logf("qoder: persisting machine id: %v", err)
		}
	}
	c.machineIDCache = id
	return id
}

func (c *Client) machineIDPath() string {
	if strings.TrimSpace(c.deps.DataDir) == "" {
		return ""
	}
	return filepath.Join(c.deps.DataDir, machineIDFile)
}

// ---------------------------------------------------------------------------
// Vendor calls
// ---------------------------------------------------------------------------

// pollDeviceToken performs one poll.  pending means "the browser has not
// confirmed yet" and is a normal answer (the vendor uses HTTP 404 for it).
func (c *Client) pollDeviceToken(ctx context.Context, nonce, verifier string) (*deviceToken, bool, error) {
	params := url.Values{}
	params.Set("nonce", nonce)
	params.Set("verifier", verifier)
	params.Set("challenge_method", "S256")

	ctx, cancel := context.WithTimeout(ctx, c.cfg.requestTimeout())
	defer cancel()

	endpoint := c.cfg.openAPIBase() + pathDevicePoll + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, fmt.Errorf("qoder: building the device poll request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if ua := c.cfg.userAgent(); ua != "" {
		req.Header.Set("User-Agent", ua)
	}

	resp, err := c.up.http.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("qoder: device poll: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, false, fmt.Errorf("qoder: reading the device poll: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, message := decodeErrorMessage(body)
		if message == "" {
			message = truncate(strings.TrimSpace(string(body)), 300)
		}
		return nil, false, &apiError{Status: resp.StatusCode, Message: message}
	}

	var tok deviceToken
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, false, fmt.Errorf("qoder: reading the device token: %w", err)
	}
	// The desktop client keeps waiting when the body is JSON but carries no
	// token yet; mirror that instead of failing a grant that is one poll away.
	if strings.TrimSpace(tok.Token) == "" || strings.TrimSpace(tok.RefreshToken) == "" {
		return nil, true, nil
	}
	return &tok, false, nil
}

// accountFromDeviceToken turns a vendor grant into the account the pool stores,
// reusing the same userinfo probe and identity merge a pasted token goes
// through so the two paths cannot drift.
func (c *Client) accountFromDeviceToken(ctx context.Context, tok *deviceToken) (account, error) {
	if tok == nil {
		return account{}, errors.New("qoder: device grant carried no token")
	}
	acc := account{storedAccount: storedAccount{
		Token:              strings.TrimSpace(tok.Token),
		RefreshToken:       strings.TrimSpace(tok.RefreshToken),
		ExpiresAtMS:        tokenExpiryMS(tok.ExpiresAt, tok.ExpiresIn),
		RefreshExpiresAtMS: tokenExpiryMS(tok.RefreshTokenExpiresAt, tok.RefreshTokenExpiresIn),
		Enabled:            true,
	}, origin: originStored}

	probeCtx, cancel := context.WithTimeout(ctx, c.cfg.probeTimeout())
	info, err := c.up.userInfo(probeCtx, acc.Token)
	cancel()
	if err != nil {
		if failureKind(err) == core.FailureAuth {
			return account{}, fmt.Errorf("厂商拒绝了这个新令牌：%s", redactErr(err))
		}
		// A flaky probe is not a bad credential: keep the grant and let the
		// account row show the failure instead of throwing the login away.
		c.logf("qoder: could not read userinfo after browser login: %s", redactErr(err))
	} else {
		acc = mergeAccount(acc, info)
	}

	acc.ID = defaultAccountID(acc)
	if acc.Label == "" {
		acc.Label = defaultAccountLabel(acc)
	}
	return acc, nil
}

// tokenExpiryMS prefers the server's ISO expiry and falls back to the relative
// lifetime the desktop client also receives.  0 means "unknown", never
// "expired": the vendor's 401 is the authority on whether a token still works.
func tokenExpiryMS(raw string, inSeconds int64) int64 {
	if ms := parseExpiryMS(strings.TrimSpace(raw)); ms > 0 {
		return ms
	}
	if inSeconds > 0 {
		return time.Now().Add(time.Duration(inSeconds) * time.Second).UnixMilli()
	}
	return 0
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// randomPKCEVerifier mints the RFC 7636 verifier the desktop client uses.
func randomPKCEVerifier() string {
	buf := make([]byte, pkceVerifierLen)
	if _, err := rand.Read(buf); err != nil {
		now := time.Now().UnixNano()
		for i := range buf {
			buf[i] = byte(now >> (uint(i%8) * 8))
		}
	}
	out := make([]byte, pkceVerifierLen)
	for i, b := range buf {
		out[i] = pkceAlphabet[int(b)%len(pkceAlphabet)]
	}
	return string(out)
}

// pkceS256 is the un-padded base64url SHA-256 challenge of a verifier.
func pkceS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// newUUID returns a v4-shaped identifier.  It never panics: on the (never
// observed) failure of crypto/rand it falls back to a time-derived value.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := time.Now().UnixNano()
		for i := 0; i < 8; i++ {
			b[i] = byte(now >> (8 * i))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
