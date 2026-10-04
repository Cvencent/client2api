package trae

// weblogin.go — panel-driven web OAuth login (core.LoginProvider).
//
// The desktop app is not required.  StartLogin binds a loopback listener, builds
// the CN console's /authorization URL with that listener as auth_callback_url,
// and hands the URL to the operator.  The operator signs in in a browser, the
// console redirects back to /authorize with the fresh credential in the query
// string, and this module exchanges it (ExchangeToken), confirms it
// (GetUserInfo) and stores it in the same account store the rest of the module
// uses.
//
// Everything here is ported from the MIT reference trae2api-web
// (internal/server/login.go, internal/server/callback.go, internal/upstream/*),
// which is the only place the web-login wire format is documented.  See
// README.md "Panel login" for the endpoints, the deviations from that reference,
// and what could not be verified offline.
//
// Design notes:
//   - A session owns its own context.  The panel's StartLogin ctx is the HTTP
//     request ctx and dies as soon as the response is written, so tying a
//     session to it would cancel every login immediately.
//   - The /authorize handler never writes credentials to disk and never runs the
//     exchange: it records the parsed credential on a buffered channel and
//     returns.  One goroutine per session owns the exchange and the store write,
//     so there is exactly one writer and no race with PollLogin.
//   - The listener is released on success, cancel and timeout, and the session
//     goroutine's own timer guarantees that even if nobody ever calls
//     CancelLogin the port is bound for at most login_session_ttl_sec.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// originLogin marks a credential this module obtained from the panel rather than
// from the desktop app's storage.json ("discovered") or from an explicit
// operator import ("imported"/"manual").
const originLogin = "login"

// loginCallbackPath is the redirect target registered with the console.  The
// reference hardcodes it (login.go:65).
const loginCallbackPath = "/authorize"

// loginTraceIDHexLen is the width of the login_trace_id the console echoes back.
// The reference defines it at callback.go:23.
const loginTraceIDHexLen = 16

// loginShutdownGrace bounds http.Server.Shutdown so a wedged connection cannot
// pin a session (or a test) forever.
const loginShutdownGrace = 5 * time.Second

// loginExchangeTimeout bounds the ExchangeToken + GetUserInfo pair.  The
// reference uses the module's generic request timeout (TW2A_TIMEOUT_SECONDS,
// default 120s); this is deliberately shorter because a human is waiting.
const loginExchangeTimeout = 60 * time.Second

// ---- config-independent helpers -------------------------------------------

// newHexID returns n random bytes as lowercase hex, the shape the reference
// mints machine/device ids in (login.go randomHex(16)).
func newHexID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand cannot realistically fail; degrade to a time-derived value
		// rather than aborting a login.
		return fmt.Sprintf("%0*x", n*2, uint64(time.Now().UnixNano()))
	}
	return hex.EncodeToString(b)
}

// machineTraceID is a verbatim port of the reference (callback.go:55-63): the
// console does not echo machine_id/device_id back, so the session is recovered
// from this trace id instead.
func machineTraceID(machineID, deviceID string) string {
	h := machineID + deviceID
	if len(h) >= loginTraceIDHexLen {
		return h[len(h)-loginTraceIDHexLen:]
	}
	return strings.Repeat("0", loginTraceIDHexLen-len(h)) + h
}

// parseJSONParam decodes a possibly-url-escaped JSON object from a query value,
// mirroring the reference (callback.go:77-92).  Values that are absent, empty or
// not objects yield nil rather than an error: the callback legitimately omits
// userInfo or userJwt depending on which auth_type the console used.
func parseJSONParam(raw string) map[string]any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	candidates := []string{raw}
	if unescaped, err := url.QueryUnescape(raw); err == nil && unescaped != raw {
		candidates = append(candidates, unescaped)
	}
	for _, cand := range candidates {
		var m map[string]any
		if err := json.Unmarshal([]byte(cand), &m); err == nil && m != nil {
			return m
		}
	}
	return nil
}

// jsonString reads a string-ish field, accepting the number shapes encoding/json
// produces for an untyped JSON number (callback.go:94-111).
func jsonString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", t))
	}
}

// jsonInt64 reads an integer-ish field (callback.go:162-181).
func jsonInt64(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch t := v.(type) {
	case float64:
		return int64(t)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return n
		}
		if f, err := t.Float64(); err == nil {
			return int64(f)
		}
	case string:
		if n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// callbackCredential is what the browser redirect delivered.  It is held in
// memory for the lifetime of one session and is never returned to the panel.
type callbackCredential struct {
	RefreshToken string
	AccessToken  string
	UID          string
	Nickname     string
	EnterpriseID string
	ExpiresAt    time.Time
}

// parseLoginCallback is a port of the reference ParseCallback (callback.go:121).
//
// Shape, in the reference's own words (callback.go:117):
//
//	http://127.0.0.1:18080/authorize?refreshToken=...&userInfo={...}&userJwt={...}
//
// The refreshToken is preferred; userJwt.RefreshToken is the fallback; when
// neither is present the callback must at least carry a usable userJwt.Token.
func parseLoginCallback(rawURL string) (*callbackCredential, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("empty callback URL")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse the callback URL: %w", err)
	}
	q := u.Query()

	cred := &callbackCredential{
		RefreshToken: strings.TrimSpace(q.Get("refreshToken")),
	}

	userInfo := parseJSONParam(q.Get("userInfo"))
	cred.UID = jsonString(userInfo, "UserID")
	cred.Nickname = jsonString(userInfo, "ScreenName")
	// The callback spells this TenantID even though GetUserInfo answers
	// EnterpriseID (callback.go:70-72).
	cred.EnterpriseID = jsonString(userInfo, "TenantID")

	userJwt := parseJSONParam(q.Get("userJwt"))
	jwtToken := jsonString(userJwt, "Token")
	jwtRefresh := jsonString(userJwt, "RefreshToken")

	if cred.RefreshToken == "" {
		cred.RefreshToken = jwtRefresh
	}
	if cred.RefreshToken == "" {
		cred.AccessToken = jwtToken
		if cred.AccessToken == "" {
			return nil, errors.New("callback missing refreshToken and userJwt.Token")
		}
		// epochTime already folds the millisecond form the console sometimes
		// sends (>1e12) into seconds.
		if exp := jsonInt64(userJwt, "TokenExpireAt"); exp > 0 {
			cred.ExpiresAt = epochTime(exp)
		}
	}
	return cred, nil
}

// ---- session ---------------------------------------------------------------

// webLoginSession is one in-flight login.  Every mutable field past the
// constructor is guarded by mu.
type webLoginSession struct {
	id        string
	createdAt time.Time
	url       string
	port      int
	machineID string
	deviceID  string

	// captured carries the parsed credential from the HTTP handler to the
	// session goroutine.  Buffered, so the handler never blocks; a second
	// redirect for the same session is dropped.
	captured chan *callbackCredential

	// cancel stops the session goroutine.  It is not derived from the caller's
	// context on purpose — see the file header.
	cancel context.CancelFunc

	srv *http.Server
	ln  net.Listener

	// serveDone is closed once the Serve goroutine has returned.  shutdown waits
	// on it: closing a listening socket on Windows only completes the pending
	// Accept when that goroutine unwinds, so without the wait a caller that
	// immediately rebinds the same fixed callback port can still see it busy.
	serveDone chan struct{}

	// done is closed once the listener is released.  CancelLogin waits on it so
	// a caller can immediately start a session on the same fixed port.
	done chan struct{}

	mu         sync.Mutex
	state      string
	message    string
	accountID  string
	finishedAt time.Time
}

func (s *webLoginSession) pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == core.LoginPending
}

// settle moves a pending session to a terminal state.  It is idempotent: the
// first terminal state wins, so a late exchange failure cannot overwrite a
// success and a cancel cannot overwrite either.
func (s *webLoginSession) settle(state, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != core.LoginPending {
		return
	}
	s.state = state
	s.message = message
	s.finishedAt = time.Now()
}

func (s *webLoginSession) succeed(accountID, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != core.LoginPending {
		return
	}
	s.state = core.LoginSuccess
	s.accountID = accountID
	s.message = message
	s.finishedAt = time.Now()
}

func (s *webLoginSession) snapshot() core.LoginState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return core.LoginState{
		SessionID: s.id,
		State:     s.state,
		URL:       s.url,
		Message:   core.Redact(s.message),
		AccountID: s.accountID,
	}
}

// expireIfStale fails a session whose TTL has elapsed.  The session goroutine's
// own timer does this too; doing it here as well means a poll that races the
// timer still reports a failure instead of a hang.
func (s *webLoginSession) expireIfStale(ttl time.Duration, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != core.LoginPending {
		return
	}
	if now.Sub(s.createdAt) < ttl {
		return
	}
	s.state = core.LoginFailed
	s.message = "login timed out after " + ttl.String() + "; start a new session"
	s.finishedAt = now
}

// finishedBefore reports whether the session reached a terminal state at least
// ttl ago, which is when the session map may drop it.
func (s *webLoginSession) finishedBefore(now time.Time, ttl time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == core.LoginPending || s.finishedAt.IsZero() {
		return false
	}
	return now.Sub(s.finishedAt) >= ttl
}

// shutdown releases the listener.  It is safe to call more than once.
func (s *webLoginSession) shutdown() {
	if s.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), loginShutdownGrace)
		if err := s.srv.Shutdown(ctx); err != nil {
			// Shutdown only fails when the grace expires; Close is the hammer.
			_ = s.srv.Close()
		}
		cancel()
	}
	if s.ln != nil {
		_ = s.ln.Close()
	}
	if s.serveDone != nil {
		select {
		case <-s.serveDone:
		case <-time.After(loginShutdownGrace):
		}
	}
}

// ---- LoginProvider ---------------------------------------------------------

// StartLogin opens a loopback listener, builds the console /authorization URL
// pointing at it, and returns a pending state whose URL the operator must open.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	if err := ctx.Err(); err != nil {
		return core.LoginState{}, err
	}
	if !c.cfg.loginEnabled() {
		return core.LoginState{}, errors.New("panel login is disabled (clients.trae.login_enabled = false)")
	}

	addr := net.JoinHostPort(c.cfg.loginCallbackHost(), strconv.Itoa(c.cfg.loginCallbackPort()))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return core.LoginState{}, fmt.Errorf("bind the login callback listener on %s: %w", addr, err)
	}
	tcp, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return core.LoginState{}, fmt.Errorf("unexpected listener address %q", ln.Addr())
	}
	port := tcp.Port

	machineID, deviceID := c.loginFingerprint()
	ttl := c.cfg.loginSessionTTL()

	sess := &webLoginSession{
		id:        newHexID(8),
		createdAt: c.now(),
		port:      port,
		machineID: machineID,
		deviceID:  deviceID,
		captured:  make(chan *callbackCredential, 1),
		done:      make(chan struct{}),
		serveDone: make(chan struct{}),
		// The session must start pending: settle/succeed/pending all gate on
		// it, so an uninitialised state would make the callback 410 and every
		// outcome a no-op.
		state: core.LoginPending,
	}
	sess.url = c.buildLoginURL(machineID, deviceID, c.loginCallbackURL(port))
	sess.message = "open the URL in a browser to sign in; this session expires in " + ttl.String()

	mux := http.NewServeMux()
	mux.HandleFunc(loginCallbackPath, c.authorizeHandler(sess))
	sess.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          nil, // http.Server's default logger; nothing sensitive
	}
	sess.ln = ln

	sessCtx, sessCancel := context.WithCancel(context.Background())
	sess.cancel = sessCancel

	c.loginMu.Lock()
	if c.logins == nil {
		c.logins = make(map[string]*webLoginSession)
	}
	c.reapLoginsLocked(c.now(), ttl)
	c.logins[sess.id] = sess
	c.loginMu.Unlock()

	core.GoSafe("trae login callback listener", func(msg string) { c.log("trae: %s", msg) }, func() {
		// close is still the last thing fn does on the success path; on a panic
		// GoSafe's report runs after it, and it writes nothing a waiter reads.
		defer close(sess.serveDone)
		if err := sess.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			c.log("login callback listener on %s stopped: %v", addr, core.Redact(err.Error()))
		}
	})
	core.GoSafe("trae login session", func(msg string) { c.log("trae: %s", msg) },
		func() { c.runLoginSession(sessCtx, sess, ttl) })

	c.log("login session %s listening on http://%s%s", sess.id, addr, loginCallbackPath)
	return sess.snapshot(), nil
}

// PollLogin reports the session's state.  It never performs I/O itself: the
// session goroutine owns the exchange, so polling is safe to call concurrently
// and as often as the panel likes.
func (c *Client) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	if err := ctx.Err(); err != nil {
		return core.LoginState{}, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return core.LoginState{}, errors.New("login session id is required")
	}
	sess := c.loginSession(sessionID)
	if sess == nil {
		return core.LoginState{}, fmt.Errorf("unknown login session %q", sessionID)
	}
	ttl := c.cfg.loginSessionTTL()
	sess.expireIfStale(ttl, c.now())
	return sess.snapshot(), nil
}

// CancelLogin stops the listener and discards the session.  Nothing is written
// to the account store unless the exchange already succeeded, so cancelling
// before the redirect leaves no credential behind.
//
// The session is kept (in state "cancelled") so a follow-up poll still gets a
// definite answer; it is reaped after one TTL.  Cancelling a session that
// already succeeded is a no-op and reports success — by then the credential is
// already stored and cancelling cannot un-store it.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("login session id is required")
	}
	sess := c.loginSession(sessionID)
	if sess == nil {
		return fmt.Errorf("unknown login session %q", sessionID)
	}
	if sess.cancel != nil {
		sess.cancel()
	}
	sess.settle(core.LoginCancelled, "login cancelled")

	// Wait for the listener to be released so a caller that pinned a fixed
	// callback port can immediately start again.  done is always closed by the
	// session goroutine, and the exchange it may be running is context-bounded.
	select {
	case <-sess.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// ShutdownLoginSessions cancels every in-flight session and releases every
// listener.
//
// NOTE: internal/core has no client shutdown hook today (only Stream.Close at
// core.go:148), so nothing in the gateway calls this yet.  It is the hook a
// future Close/Shutdown sweep would call.  Sessions are not left to leak in the
// meantime: each one's own TTL timer releases its port.
func (c *Client) ShutdownLoginSessions() error {
	c.loginMu.Lock()
	sessions := make([]*webLoginSession, 0, len(c.logins))
	for _, s := range c.logins {
		sessions = append(sessions, s)
	}
	c.logins = nil
	c.loginMu.Unlock()

	for _, s := range sessions {
		if s.cancel != nil {
			s.cancel()
		}
		s.settle(core.LoginCancelled, "login cancelled: the gateway is shutting down")
	}
	for _, s := range sessions {
		select {
		case <-s.done:
		case <-time.After(loginShutdownGrace):
		}
	}
	return nil
}

// ---- internals -------------------------------------------------------------

// runLoginSession owns one session's lifecycle: it waits for the redirect, the
// TTL or a cancel, then always releases the listener.
func (c *Client) runLoginSession(ctx context.Context, sess *webLoginSession, ttl time.Duration) {
	defer close(sess.done)
	defer sess.shutdown()

	timer := time.NewTimer(ttl)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		sess.settle(core.LoginCancelled, "login cancelled")
		return
	case <-timer.C:
		sess.settle(core.LoginFailed, "login timed out after "+ttl.String()+"; start a new session")
		return
	case cred := <-sess.captured:
		// A cancel that raced the redirect wins: never spend a credential the
		// operator already gave up on.
		if ctx.Err() != nil {
			sess.settle(core.LoginCancelled, "login cancelled")
			return
		}
		accountID, err := c.completeLogin(ctx, sess, cred)
		if err != nil {
			sess.settle(core.LoginFailed, err.Error())
			c.log("login session %s failed: %s", sess.id, core.Redact(err.Error()))
			return
		}
		sess.succeed(accountID, "signed in; the account is now in the pool")
		c.log("login session %s stored account %s", sess.id, accountID)
	}
}

// completeLogin exchanges the callback credential for a usable token, confirms
// it against GetUserInfo, and persists it in the module's account store.
func (c *Client) completeLogin(ctx context.Context, sess *webLoginSession, cred *callbackCredential) (string, error) {
	exctx, cancel := context.WithTimeout(ctx, loginExchangeTimeout)
	defer cancel()

	a := &Auth{
		AccessToken:  strings.TrimSpace(cred.AccessToken),
		RefreshToken: strings.TrimSpace(cred.RefreshToken),
		UserID:       strings.TrimSpace(cred.UID),
		Username:     strings.TrimSpace(cred.Nickname),
		Host:         c.cfg.oauthHost(),
		Region:       c.cfg.Region,
		MachineID:    sess.machineID,
		DeviceID:     sess.deviceID,
		Source:       originLogin,
		Configured:   true,
	}
	if !cred.ExpiresAt.IsZero() {
		a.ExpiresAt = cred.ExpiresAt
	}

	// The console hands back a refresh token in the normal case; exchanging it
	// is what yields the access token the chat endpoints need.
	if a.RefreshToken != "" {
		if err := c.exchangeLoginToken(exctx, a); err != nil {
			return "", err
		}
	}
	if strings.TrimSpace(a.Token()) == "" {
		return "", errors.New("the login callback carried no usable access token")
	}

	uid, nickname, err := c.loginUserInfo(exctx, a)
	if err != nil {
		return "", err
	}
	if uid != "" {
		a.UserID = uid
	}
	if nickname != "" {
		a.Username = nickname
	}
	if strings.TrimSpace(a.UserID) == "" {
		return "", errors.New("the upstream returned no user id, so the credential cannot be keyed")
	}

	sa := fromAuth(a, originLogin, firstNonEmpty(a.Username, a.UserID))
	sa.Enabled = true
	sa.MachineID = sess.machineID
	sa.DeviceID = sess.deviceID
	if sa.ID == "" {
		return "", errors.New("the logged-in credential has no stable account id")
	}
	a.IDOverride = sa.ID

	if _, err := c.mutateStore(func(st *accountStore) bool {
		st.upsert(sa)
		st.unsuppress(sa.ID)
		return true
	}); err != nil {
		return "", fmt.Errorf("persist the logged-in account: %w", err)
	}
	c.rebuildPool()
	return sa.ID, nil
}

// loginExchangeResult is the ExchangeToken reply.
//
// The CN endpoint wraps the fields in "Result" (reference client.go:195-203 and
// trae2api src/auth.js:459-467, which carries the comment "the api.trae.cn
// endpoint wraps fields in Result{...}; legacy shape is lowercase at top
// level").  Both shapes are decoded so a wrapping change cannot read as an empty
// token.  The module's pre-existing refreshToken() only understands the flat
// shape; that is a known gap recorded in README "Known gaps" and is deliberately
// not changed here.
type loginExchangeResult struct {
	Token               string `json:"Token"`
	TokenExpireAt       int64  `json:"TokenExpireAt"`
	TokenExpireDuration int64  `json:"TokenExpireDuration"`
	RefreshToken        string `json:"RefreshToken"`
	RefreshExpireAt     int64  `json:"RefreshExpireAt"`

	Result *loginExchangeResult `json:"Result"`
}

// fold prefers the nested Result copy, the same tolerance checkin.go:53-71 uses.
func (r loginExchangeResult) fold() loginExchangeResult {
	if r.Result != nil {
		return *r.Result
	}
	return r
}

// exchangeLoginToken posts the callback's refresh token to ExchangeToken and
// installs the resulting tokens on a.  Body and headers match the reference
// (client.go:171-221 / headers.go OAuthHeaders).
func (c *Client) exchangeLoginToken(ctx context.Context, a *Auth) error {
	body, err := json.Marshal(map[string]any{
		"ClientID":     c.cfg.clientID(),
		"RefreshToken": a.RefreshTokenValue(),
		"ClientSecret": "-",
		"UserID":       "",
	})
	if err != nil {
		return fmt.Errorf("encode the token exchange request: %w", err)
	}

	var raw loginExchangeResult
	if err := c.doJSON(ctx, c.cfg.oauthHost()+epExchange, c.oauthHeaders(), body, &raw); err != nil {
		return fmt.Errorf("exchange the login token: %w", err)
	}
	res := raw.fold()
	if strings.TrimSpace(res.Token) == "" {
		return errors.New("the token exchange returned no access token")
	}

	expires := epochTime(res.TokenExpireAt)
	if expires.IsZero() && res.TokenExpireDuration > 0 {
		expires = c.now().Add(time.Duration(res.TokenExpireDuration) * time.Second)
	}
	refreshExpires := epochTime(res.RefreshExpireAt)
	rotated := strings.TrimSpace(res.RefreshToken)
	if rotated == "" {
		rotated = a.RefreshTokenValue()
	}
	a.SetTokens(res.Token, rotated, expires, refreshExpires)
	return nil
}

// loginUserInfo confirms the credential and returns the account identity.  The
// reference posts {"ReqSource":"IDE","IDEVersion":...} with the access token in
// X-Cloudide-Token and reads {"Result":{UserID,ScreenName,EnterpriseID}}
// (client.go:403-431).
func (c *Client) loginUserInfo(ctx context.Context, a *Auth) (uid, nickname string, err error) {
	body, err := json.Marshal(map[string]any{
		"ReqSource":  "IDE",
		"IDEVersion": c.cfg.ideVersion(),
	})
	if err != nil {
		return "", "", fmt.Errorf("encode the user info request: %w", err)
	}

	h := c.oauthHeaders()
	h.Set("X-Cloudide-Token", a.Token())

	var raw struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
		// The flat shape is accepted too, for the same reason as above.
		UserID     string `json:"UserID"`
		ScreenName string `json:"ScreenName"`
	}
	if err := c.doJSON(ctx, c.cfg.oauthHost()+epUserInfo, h, body, &raw); err != nil {
		return "", "", fmt.Errorf("confirm the login: %w", err)
	}
	uid = firstNonEmpty(raw.Result.UserID, raw.UserID)
	nickname = firstNonEmpty(raw.Result.ScreenName, raw.ScreenName)
	return uid, nickname, nil
}

// authorizeHandler serves the console's redirect.
func (c *Client) authorizeHandler(sess *webLoginSession) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			c.writeLoginPage(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !sess.pending() {
			c.writeLoginPage(w, http.StatusGone, "this login session is no longer active; start a new one from the panel")
			return
		}

		// Rebuild the absolute URL the reference parses (login.go authorizeCallback
		// prepends the scheme and host for the same reason).
		raw := "http://" + r.Host + r.URL.RequestURI()
		cred, err := parseLoginCallback(raw)
		if err != nil {
			sess.settle(core.LoginFailed, err.Error())
			c.writeLoginPage(w, http.StatusBadRequest, err.Error())
			return
		}
		// Record only.  The session goroutine owns the exchange and the store
		// write, so this handler cannot race PollLogin.
		select {
		case sess.captured <- cred:
			c.writeLoginPage(w, http.StatusOK, "signed in — you can close this tab and return to the panel")
		default:
			c.writeLoginPage(w, http.StatusOK, "this login session already received a credential")
		}
	}
}

// writeLoginPage renders the minimal operator-facing page.  Everything shown
// goes through core.Redact, and the page is escaped.
func (c *Client) writeLoginPage(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<!doctype html><html><head><meta charset=\"utf-8\">"+
		"<title>Trae login</title></head>"+
		"<body style=\"font-family:system-ui,sans-serif;padding:2rem;line-height:1.5\">"+
		"<p>%s</p></body></html>", html.EscapeString(core.Redact(msg)))
}

// buildLoginURL is a verbatim port of the reference BuildLoginURL
// (callback.go:28-51).  The parameters outside the url.Values block are appended
// by hand in the reference; the order is preserved so the URL is byte-comparable
// with a captured one.
func (c *Client) buildLoginURL(machineID, deviceID, callbackURL string) string {
	v := url.Values{}
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", c.cfg.loginPluginVersion())
	v.Set("auth_type", "local")
	v.Set("client_id", c.cfg.clientID())
	v.Set("redirect", "0")

	return c.cfg.consoleHost() + "/authorization?" + v.Encode() +
		"&login_trace_id=" + url.QueryEscape(machineTraceID(machineID, deviceID)) +
		"&auth_callback_url=" + url.QueryEscape(callbackURL) +
		"&machine_id=" + url.QueryEscape(machineID) +
		"&device_id=" + url.QueryEscape(deviceID) +
		"&x_device_id=" + url.QueryEscape(deviceID) +
		"&x_machine_id=" + url.QueryEscape(machineID) +
		"&x_device_brand=PC" +
		"&x_device_type=PC" +
		"&x_os_version=1.0" +
		"&x_app_version=" + url.QueryEscape(c.cfg.loginAppVersion()) +
		"&x_app_type=stable"
}

// loginCallbackURL is the redirect target the console will hit: the reference's
// "http://127.0.0.1:" + port + "/authorize" (login.go:65).
func (c *Client) loginCallbackURL(port int) string {
	return "http://" + net.JoinHostPort(c.cfg.loginCallbackHost(), strconv.Itoa(port)) + loginCallbackPath
}

// loginFingerprint picks the machine/device pair the login page and the stored
// credential will share.
//
// The reference mints a fresh random hex32 pair per login (login.go randomHex),
// which is the only option when the desktop app was never installed.  When this
// machine already has a fingerprint — from config or from a discovered account —
// reusing it is strictly better: the module's own docs note that the upstream
// silently drops a fingerprint it has not seen before, so a random pair can
// produce a credential that authenticates but is never billed to a real device.
func (c *Client) loginFingerprint() (machineID, deviceID string) {
	machineID = c.cfg.MachineID
	deviceID = c.cfg.DeviceID
	if machineID == "" || deviceID == "" {
		for _, a := range c.pool.Accounts() {
			if machineID == "" {
				machineID = strings.TrimSpace(a.MachineID)
			}
			if deviceID == "" {
				deviceID = strings.TrimSpace(a.DeviceID)
			}
			if machineID != "" && deviceID != "" {
				break
			}
		}
	}
	if machineID == "" {
		machineID = newHexID(16)
	}
	if deviceID == "" {
		deviceID = newHexID(16)
	}
	return machineID, deviceID
}

func (c *Client) loginSession(id string) *webLoginSession {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	return c.logins[id]
}

// reapLoginsLocked drops sessions that finished more than ttl ago, so the map
// cannot grow without bound while still leaving the panel ample time to read a
// final state.
func (c *Client) reapLoginsLocked(now time.Time, ttl time.Duration) {
	for id, s := range c.logins {
		if s.finishedBefore(now, ttl) {
			delete(c.logins, id)
		}
	}
}
