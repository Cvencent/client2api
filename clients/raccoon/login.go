package raccoon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Interactive login.
//
// Two paths, one contract:
//
//   - 微信扫码: the qrcode_code is generated HERE and the vendor only answers
//     polls about it, so the desktop app's unreachable office-raccoon://
//     callback is never involved.
//   - 短信验证码: the phone number is AES-128-CFB encrypted with the vendor's
//     public front-end key and gated behind an Aliyun slider captcha.
//
// Both end on the same account pool a pasted credential does, and the
// credential store is written before the session is marked successful.
//
// The QR/SMS page itself lives in login_page.go; this file is the API and
// session layer.
// ---------------------------------------------------------------------------

const (
	// qrCodeBytes is the size of the locally generated qrcode_code: 16 random
	// bytes rendered as 32 lowercase hex characters, matching the vendor's own
	// client.
	qrCodeBytes = 16

	// minQRPollInterval throttles the vendor poll.  The page polls every 2 s
	// and the panel may poll at the same time; without this the two would
	// double every request.
	minQRPollInterval = 1500 * time.Millisecond

	// loginBodyLimit bounds a local request body.
	loginBodyLimit = 64 << 10
)

// QR poll statuses as the vendor names them.
const (
	qrStatusPending  = "pending"
	qrStatusLogging  = "logging"
	qrStatusCanceled = "canceled"
	qrStatusSuccess  = "success"
)

// core.LoginProvider is what the panel drives: "add an account" opens a
// loopback page carrying the QR and SMS tabs.
var _ core.LoginProvider = (*Client)(nil)

// generateQRCode returns a fresh 32-hex-character qrcode_code.
func generateQRCode() (string, error) {
	buf := make([]byte, qrCodeBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("raccoon: qr code: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// qrLoginURL is the public page the QR image points at.  Scanning it in WeChat
// completes the authorisation; the vendor then flips the code to success.
func (c *Client) qrLoginURL(code string) string {
	q := url.Values{}
	q.Set("code", code)
	q.Set("appname", "商汤小浣熊官网")
	return c.cfg.baseURL() + "/login/mp?" + q.Encode()
}

// --- session --------------------------------------------------------------

// loginSession is one in-flight interactive login.  It owns its loopback
// server and all the sensitive state the page must never see (the qrcode_code,
// the target phone number).
type loginSession struct {
	mu        sync.Mutex
	now       func() time.Time
	sessionID string
	url       string
	expiresAt time.Time

	qrCode    string
	qrStatus  string
	phone     string
	expiredAt string

	state     string
	message   string
	accountID string

	lastPollAt time.Time
	polling    bool
	closed     bool

	server   *http.Server
	listener net.Listener
}

func (s *loginSession) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *loginSession) terminal() bool {
	switch s.state {
	case core.LoginSuccess, core.LoginFailed, core.LoginCancelled:
		return true
	}
	return false
}

// set records progress but never resurrects a finished session.
func (s *loginSession) set(state, message, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal() {
		return
	}
	s.state = state
	s.message = core.Redact(message)
	if accountID != "" {
		s.accountID = accountID
	}
}

func (s *loginSession) succeed(accountID, message string) {
	s.set(core.LoginSuccess, message, accountID)
}

// cancel closes the loopback listener for good and is idempotent.
func (s *loginSession) cancel() {
	s.mu.Lock()
	already := s.closed
	s.closed = true
	if !s.terminal() {
		s.state = core.LoginCancelled
		s.message = "cancelled"
	}
	srv := s.server
	s.mu.Unlock()
	if srv != nil && !already {
		_ = srv.Close()
	}
}

// snapshot renders the session for the panel.  A pending session whose window
// has closed becomes failed here rather than hanging forever.
func (s *loginSession) snapshot() core.LoginState {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, message := s.state, s.message
	if state == core.LoginPending && s.clock().After(s.expiresAt) {
		state, message = core.LoginFailed, "登录超时，请重新发起"
		s.state, s.message = state, message
	}
	return core.LoginState{
		SessionID: s.sessionID,
		State:     state,
		URL:       s.url,
		Message:   core.Redact(message),
		AccountID: s.accountID,
	}
}

// --- registry -------------------------------------------------------------

func (c *Client) putLogin(s *loginSession) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.logins == nil {
		c.logins = make(map[string]*loginSession)
	}
	now := c.now()
	for id, other := range c.logins {
		if other == nil {
			delete(c.logins, id)
			continue
		}
		other.mu.Lock()
		expired := now.After(other.expiresAt)
		other.mu.Unlock()
		if expired {
			other.cancel()
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

// --- core.LoginProvider ---------------------------------------------------

// StartLogin binds a loopback page and returns its URL.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return core.LoginState{}, fmt.Errorf("raccoon: cannot open a loopback listener: %w", err)
	}
	code, err := generateQRCode()
	if err != nil {
		_ = ln.Close()
		return core.LoginState{}, err
	}
	sessionID, err := generateQRCode()
	if err != nil {
		_ = ln.Close()
		return core.LoginState{}, err
	}
	sess := &loginSession{
		now:       c.now,
		sessionID: "raccoon:" + sessionID,
		url:       "http://" + ln.Addr().String() + pathLogin,
		expiresAt: c.now().Add(c.cfg.loginTimeout()),
		qrCode:    code,
		qrStatus:  qrStatusPending,
		state:     core.LoginPending,
		message:   "使用微信扫码或短信验证码登录",
	}
	mux := http.NewServeMux()
	mux.HandleFunc(pathLogin, c.handleLoginPage(sess))
	mux.HandleFunc(pathPoll, c.handleLoginPoll(sess))
	mux.HandleFunc(pathSmsSend, c.handleSmsSend(sess))
	mux.HandleFunc(pathSmsVerify, c.handleSmsVerify(sess))
	sess.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	sess.listener = ln
	c.putLogin(sess)
	go func() { _ = sess.server.Serve(ln) }()
	return sess.snapshot(), nil
}

// PollLogin advances the session by at most one vendor poll.  It never blocks:
// the panel owns the cadence, and a session with nothing new stays pending.
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
	c.pollQR(ctx, sess)
	return sess.snapshot(), nil
}

// CancelLogin stops a session and closes its loopback listener.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return errors.New("session id is required")
	}
	sess := c.loginByID(id)
	if sess == nil {
		return fmt.Errorf("login session %q not found", id)
	}
	sess.cancel()
	return nil
}

// --- vendor calls ---------------------------------------------------------

// qrResult is one parsed QR poll.
type qrResult struct {
	status    string
	expiredAt string
	cred      *credential
}

// queryQR performs one vendor poll for a qrcode_code.  ANY failure degrades to
// pending: the poll runs every two seconds, and a transient error must not
// interrupt a scan in progress.  A `success` with no token is also pending —
// treating it as success would store an empty credential.
func (c *Client) queryQR(ctx context.Context, code string) qrResult {
	ctx, cancel := withTimeout(ctx, requestTimeout)
	defer cancel()
	var res struct {
		Status       string `json:"status"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiredAt    string `json:"expired_at"`
	}
	if err := c.do(ctx, http.MethodPost, pathQRLogin, nil, map[string]string{"qrcode_code": code}, &res); err != nil {
		return qrResult{status: qrStatusPending}
	}
	switch strings.TrimSpace(res.Status) {
	case qrStatusSuccess:
		if strings.TrimSpace(res.AccessToken) == "" {
			return qrResult{status: qrStatusPending}
		}
		cred := &credential{
			AccessToken:  strings.TrimSpace(res.AccessToken),
			RefreshToken: strings.TrimSpace(res.RefreshToken),
		}
		if ms := jwtExpiryMs(cred.AccessToken); ms > 0 {
			cred.ExpiresAt = flexString(strconv.FormatInt(ms, 10))
		}
		return qrResult{status: qrStatusSuccess, cred: cred}
	case qrStatusCanceled:
		return qrResult{status: qrStatusCanceled}
	case qrStatusLogging:
		return qrResult{status: qrStatusLogging, expiredAt: strings.TrimSpace(res.ExpiredAt)}
	default:
		return qrResult{status: qrStatusPending}
	}
}

// pollQR runs one throttled poll and folds the result into the session.
func (c *Client) pollQR(ctx context.Context, sess *loginSession) {
	sess.mu.Lock()
	if sess.polling || sess.closed || sess.terminal() {
		sess.mu.Unlock()
		return
	}
	now := c.now()
	if !sess.lastPollAt.IsZero() && now.Sub(sess.lastPollAt) < minQRPollInterval {
		sess.mu.Unlock()
		return
	}
	sess.polling = true
	sess.lastPollAt = now
	code := sess.qrCode
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		sess.polling = false
		sess.mu.Unlock()
	}()

	res := c.queryQR(ctx, code)
	switch res.status {
	case qrStatusSuccess:
		c.finishLogin(ctx, sess, *res.cred)
	case qrStatusCanceled:
		next, err := generateQRCode()
		sess.mu.Lock()
		if err == nil && !sess.terminal() {
			sess.qrCode = next
		}
		sess.qrStatus = qrStatusCanceled
		sess.message = "二维码已刷新，请重新扫码"
		sess.mu.Unlock()
	case qrStatusLogging:
		sess.mu.Lock()
		if !sess.terminal() {
			sess.qrStatus = qrStatusLogging
			sess.expiredAt = res.expiredAt
			sess.message = "已扫码，请在微信中确认"
		}
		sess.mu.Unlock()
	default:
		sess.mu.Lock()
		if !sess.terminal() {
			sess.qrStatus = qrStatusPending
		}
		sess.mu.Unlock()
	}
}

// sendSMS asks the vendor to text a verification code.  The phone travels
// encrypted, never in the clear.
func (c *Client) sendSMS(ctx context.Context, phone, captchaParam string) error {
	enc, err := encryptPhoneRandom(phone)
	if err != nil {
		return err
	}
	ctx, cancel := withTimeout(ctx, requestTimeout)
	defer cancel()
	err = c.do(ctx, http.MethodPost, pathSendSMS, nil, map[string]string{
		"captcha_param": captchaParam,
		"nation_code":   "86",
		"phone":         enc,
	}, nil)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == 100006 {
			return errors.New("raccoon: 图形验证码校验失败，请重新完成滑块验证")
		}
		return err
	}
	return nil
}

// loginWithSMS exchanges a verification code for a credential.
func (c *Client) loginWithSMS(ctx context.Context, phone, smsCode string) (credential, error) {
	enc, err := encryptPhoneRandom(phone)
	if err != nil {
		return credential{}, err
	}
	ctx, cancel := withTimeout(ctx, requestTimeout)
	defer cancel()
	var res struct {
		AccessToken    string `json:"access_token"`
		RefreshToken   string `json:"refresh_token"`
		OfficeIdentity string `json:"office_identity"`
	}
	if err := c.do(ctx, http.MethodPost, pathLoginSMS, nil, map[string]string{
		"nation_code": "86",
		"phone":       enc,
		"sms_code":    smsCode,
	}, &res); err != nil {
		return credential{}, err
	}
	tok := strings.TrimSpace(res.AccessToken)
	if tok == "" {
		return credential{}, errors.New("raccoon: 登录响应缺少 access_token")
	}
	cred := credential{
		AccessToken:    tok,
		RefreshToken:   strings.TrimSpace(res.RefreshToken),
		OfficeIdentity: strings.TrimSpace(res.OfficeIdentity),
	}
	if ms := jwtExpiryMs(tok); ms > 0 {
		cred.ExpiresAt = flexString(strconv.FormatInt(ms, 10))
	}
	return cred, nil
}

// finishLogin enriches a fresh credential, stores it and marks the session
// successful.  The credential is written through the same pool a pasted key
// uses, so it survives a restart and shows up in the account table.
func (c *Client) finishLogin(ctx context.Context, sess *loginSession, cred credential) {
	if info, err := c.fetchUserInfo(ctx, cred); err == nil {
		if info.ID != "" {
			cred.UserID = info.ID
		}
		if info.Name != "" {
			cred.Nickname = info.Name
		}
		if info.Phone != "" {
			cred.Phone = info.Phone
		}
		if info.OfficeIdentity != "" {
			cred.OfficeIdentity = info.OfficeIdentity
		}
	}
	e := c.pool.put(account{credential: cred, Label: cred.displayName(), Origin: originStored})
	c.grantLoginPointsBestEffort(ctx, e)
	sess.succeed(e.acct.id(), "已登录 "+e.acct.label())
}

// grantLoginPointsBestEffort fires the desktop login-points grant once, right
// after a successful sign-in, the way the vendor's own client does on launch.
//
// It deliberately does NOT go through Checkin: that path records a failure
// against the account, and a fresh credential must not be born with a note or
// a failure count because a best-effort reward call was refused.
func (c *Client) grantLoginPointsBestEffort(ctx context.Context, e *entry) {
	if e == nil {
		return
	}
	ctx, cancel := withTimeout(ctx, pointsGrantTimeout)
	defer cancel()
	var reply pointsGrantReply
	if err := c.do(ctx, http.MethodPost, pathPointsGrant,
		raccoonHeaders(e.acct.cred(), c.cfg.platform(), c.cfg.version()), nil, &reply); err != nil {
		c.deps.Log("raccoon: login points grant failed for %s: %v", e.acct.id(), c.scrub(err.Error()))
		return
	}
	c.pool.markUsed(e)
}
