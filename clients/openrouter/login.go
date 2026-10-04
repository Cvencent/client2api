package openrouter

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Panel login (PKCE).
//
// OpenRouter has no device-code flow: the documented way to mint a key for a
// third-party tool is the PKCE redirect.  The operator opens a vendor page,
// approves the app, and the browser lands back on a loopback listener this
// module owns with a one-shot code.  The module trades that code (plus the
// verifier it never leaves the process with) for an API key and stores it
// exactly like a pasted key.
//
// Everything here is loopback-only: the listener binds 127.0.0.1, the state
// check rejects a stray or replayed callback, and the code is exchanged before
// the session is marked successful.
// ---------------------------------------------------------------------------

const (
	// loginCallbackPath is the only path the loopback listener serves.
	loginCallbackPath = "/callback"
	// loginMaxBody bounds every vendor body the login flow will read.
	loginMaxBody = 1 << 20
	// loginKeyLabel names the key OpenRouter mints, so the operator can find it
	// again on the vendor's own keys page.
	loginKeyLabel = "client2api"
	// loginMaxTokenBytes bounds the random verifier/state.  32 bytes is the RFC
	// 7636 recommendation.
	loginTokenBytes = 32
)

// core.LoginProvider is what the panel drives: "add an account" here means the
// PKCE redirect, not a pasted key.
var _ core.LoginProvider = (*Client)(nil)

// callbackResult is one delivery from the loopback handler to PollLogin.
type callbackResult struct {
	code string
	err  string
}

// loginSession is one in-flight PKCE authorisation.
type loginSession struct {
	mu        sync.Mutex
	now       func() time.Time
	sessionID string
	state     string
	verifier  string
	label     string
	authURL   string
	expiresAt time.Time

	server *http.Server
	// results carries at most one callback; the handler drops a duplicate
	// rather than blocking on a full channel.
	results chan callbackResult

	status    string
	message   string
	accountID string
	closed    bool
}

func (l *loginSession) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// terminal reports whether the session has reached a state it can never leave.
func (l *loginSession) terminal() bool {
	switch l.status {
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
	l.status = state
	l.message = core.Redact(message)
	if accountID != "" {
		l.accountID = accountID
	}
}

func (l *loginSession) succeed(accountID, message string) {
	l.set(core.LoginSuccess, message, accountID)
}

func (l *loginSession) fail(message string) {
	l.set(core.LoginFailed, message, "")
}

// cancel stops the listener for good.  It is idempotent: a second call leaves a
// finished session alone and never double-closes the server.
func (l *loginSession) cancel() {
	l.mu.Lock()
	already := l.closed
	l.closed = true
	if !l.terminal() {
		l.status = core.LoginCancelled
		l.message = "cancelled"
	}
	srv := l.server
	l.mu.Unlock()
	if srv != nil && !already {
		_ = srv.Close()
	}
}

// deliver hands one callback result to PollLogin.  It never blocks: a duplicate
// or late callback is dropped, and the first result wins.
func (l *loginSession) deliver(res callbackResult) {
	select {
	case l.results <- res:
	default:
	}
}

// snapshot renders the session for the panel.  A pending session whose window
// has closed becomes failed here rather than hanging forever.
func (l *loginSession) snapshot() core.LoginState {
	l.mu.Lock()
	defer l.mu.Unlock()
	state, message := l.status, l.message
	if state == core.LoginPending && l.clock().After(l.expiresAt) {
		state, message = core.LoginFailed, "the login window expired; start again"
		l.status, l.message = state, message
	}
	return core.LoginState{
		SessionID: l.sessionID,
		State:     state,
		URL:       l.authURL,
		Message:   core.Redact(message),
		AccountID: l.accountID,
	}
}

// handleCallback is the loopback handler.  It validates state before it will
// look at the code, and always answers a human-readable page.
func (l *loginSession) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if errParam := strings.TrimSpace(q.Get("error")); errParam != "" {
		desc := firstNonEmpty(strings.TrimSpace(q.Get("error_description")), errParam)
		l.deliver(callbackResult{err: "the vendor refused the login: " + desc})
		writeLoginPage(w, http.StatusBadRequest, "登录失败", core.Redact(desc))
		return
	}
	if strings.TrimSpace(q.Get("state")) != l.state {
		l.deliver(callbackResult{err: "the callback carried the wrong state"})
		writeLoginPage(w, http.StatusBadRequest, "登录失败", "state 校验失败，请重新发起登录。")
		return
	}
	code := strings.TrimSpace(q.Get("code"))
	if code == "" {
		l.deliver(callbackResult{err: "the callback carried no authorization code"})
		writeLoginPage(w, http.StatusBadRequest, "登录失败", "回调缺少授权码，请重新发起登录。")
		return
	}
	l.deliver(callbackResult{code: code})
	writeLoginPage(w, http.StatusOK, "登录成功", "已收到授权，正在完成登录，可以关闭此页面。")
}

// writeLoginPage renders the one page the loopback listener ever serves.  It
// carries no script and no secret: the operator sees the outcome, nothing more.
func writeLoginPage(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte("<!doctype html><html lang=\"zh\"><head><meta charset=\"utf-8\">" +
		"<title>" + htmlEscape(title) + "</title></head>" +
		"<body style=\"font-family:system-ui,sans-serif;margin:4rem auto;max-width:32rem\">" +
		"<h1>" + htmlEscape(title) + "</h1><p>" + htmlEscape(detail) + "</p></body></html>"))
}

// htmlEscape escapes the few characters that could break out of the page.
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}

// --- random and PKCE helpers ----------------------------------------------

// randomToken returns n bytes of cryptographic randomness as a base64url
// string without padding, which is the alphabet PKCE requires.
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// pkceChallenge derives the S256 challenge from a verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// hexToken is used for the session id, which is logged and rendered: a hex
// digest of the state carries no secret.
func hexToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// --- session registry -----------------------------------------------------

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

// StartLogin opens a loopback listener, builds the PKCE authorization URL and
// records a session.  The listener is already accepting by the time this
// returns, so a fast browser redirect cannot race the bind.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	c.ensure()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return core.LoginState{}, fmt.Errorf("openrouter login: cannot open a loopback listener: %w", err)
	}
	verifier, err := randomToken(loginTokenBytes)
	if err != nil {
		_ = ln.Close()
		return core.LoginState{}, fmt.Errorf("openrouter login: cannot generate a PKCE verifier: %w", err)
	}
	state, err := randomToken(loginTokenBytes)
	if err != nil {
		_ = ln.Close()
		return core.LoginState{}, fmt.Errorf("openrouter login: cannot generate a state: %w", err)
	}
	sessionID, err := hexToken(8)
	if err != nil {
		_ = ln.Close()
		return core.LoginState{}, fmt.Errorf("openrouter login: cannot generate a session id: %w", err)
	}

	callback := "http://" + ln.Addr().String() + loginCallbackPath
	authURL := c.cfg.authURL() + "?" + url.Values{
		"callback_url":          {callback},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
		"key_label":             {loginKeyLabel},
		"state":                 {state},
	}.Encode()

	sess := &loginSession{
		now:       c.now,
		sessionID: "pkce:" + sessionID,
		state:     state,
		verifier:  verifier,
		label:     loginKeyLabel,
		authURL:   authURL,
		expiresAt: c.now().Add(c.cfg.loginTimeout()),
		results:   make(chan callbackResult, 1),
		status:    core.LoginPending,
		message:   "在浏览器中打开链接并完成授权",
	}
	mux := http.NewServeMux()
	mux.HandleFunc(loginCallbackPath, sess.handleCallback)
	sess.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	c.putLogin(sess)
	go func() { _ = sess.server.Serve(ln) }()
	return sess.snapshot(), nil
}

// PollLogin advances a session by at most one callback.  The panel owns the
// cadence, so this method never blocks: a session with no callback yet simply
// stays pending.
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
	select {
	case res := <-sess.results:
		return c.completeLogin(ctx, sess, res), nil
	default:
		return sess.snapshot(), nil
	}
}

// completeLogin turns one delivered callback into an account.  A bad state or a
// refused exchange is a failed session, not a returned error: the panel renders
// it on the session, and the operator can simply start again.
func (c *Client) completeLogin(ctx context.Context, sess *loginSession, res callbackResult) core.LoginState {
	if res.err != "" {
		sess.fail(res.err)
		return sess.snapshot()
	}
	key, err := c.exchangeCode(ctx, sess, res.code)
	if err != nil {
		sess.fail(err.Error())
		return sess.snapshot()
	}
	rec := accountRecord{
		ID:      accountIDFor(key),
		Label:   firstNonEmpty(sess.label, "OpenRouter OAuth"),
		APIKey:  key,
		Source:  sourcePanel,
		AddedAt: c.now().UTC().Format(time.RFC3339),
	}
	c.pool.upsert(rec)
	c.persistCredentials()
	c.persistState()
	sess.succeed(rec.ID, "已登录 OpenRouter，凭证已保存")
	return sess.snapshot()
}

// exchangeCode trades the one-shot code and the PKCE verifier for an API key.
func (c *Client) exchangeCode(ctx context.Context, sess *loginSession, code string) (string, error) {
	payload, err := json.Marshal(map[string]string{
		"code":                  code,
		"code_verifier":         sess.verifier,
		"code_challenge_method": "S256",
	})
	if err != nil {
		return "", errors.New("could not build the token exchange request")
	}
	reqCtx, cancel := context.WithTimeout(ctxOrBackground(ctx), c.cfg.loginTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.cfg.keysURL(), bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("could not build the token exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The exchange is unauthenticated: the code is the credential.
	c.cfg.applyHeaders(req, "")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", errors.New(core.Redact("cannot reach the vendor: " + err.Error()))
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, loginMaxBody)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the vendor returned HTTP %d: %s",
			resp.StatusCode, truncate(core.Redact(errorTextOf(raw)), 200))
	}
	var doc struct {
		Key string `json:"key"`
	}
	if uerr := json.Unmarshal(raw, &doc); uerr != nil {
		return "", errors.New("the vendor returned an unrecognised response")
	}
	key := strings.TrimSpace(doc.Key)
	if key == "" {
		return "", errors.New("the vendor returned no API key")
	}
	return key, nil
}

// CancelLogin stops a session and closes its loopback listener.  Cancelling
// before the code is exchanged leaves nothing on disk.
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
