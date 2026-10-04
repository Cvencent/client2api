package lobsterai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"client2api/internal/core"
)

// This file implements the browser hand-off sign-in.
//
// LobsterAI has no password exchange a bridge could perform.  The operator opens
// a page on the vendor's portal, approves the sign-in there, and the vendor
// redirects to a loopback URL carrying an authCode, which is then exchanged for
// a token pair.  So the module runs a tiny local HTTP server for exactly one
// request and nothing else.
//
// The module does NOT launch a browser: it hands the URL to the panel, which is
// where the operator already is.  That keeps the module usable over a remote
// connection and avoids a library deciding to spawn a process.

// loginCallback carries the authCode the browser dropped on the loopback
// listener.
type loginCallback struct {
	code string
	err  error
}

// loginSession is one in-flight sign-in.
type loginSession struct {
	id           string
	url          string
	state        string
	uuid         string
	firstKeyfrom string
	startedAt    time.Time

	server   *http.Server
	listener net.Listener

	callbacks chan loginCallback

	mu        sync.Mutex
	status    string
	message   string
	accountID string
	closed    bool
}

// StartLogin opens a loopback listener and returns the URL to visit.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	s, err := c.newLoginSession()
	if err != nil {
		return core.LoginState{}, err
	}
	c.putLogin(s)
	c.deps.Log("lobsterai: sign-in window open, visit %s", s.url)
	return s.snapshot(), nil
}

// PollLogin reports the session.  It re-reports a terminal state rather than
// probing again, so a panel that polls every second cannot restart a finished
// sign-in.
func (c *Client) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	s, ok := c.getLogin(sessionID)
	if !ok {
		return core.LoginState{}, fmt.Errorf("lobsterai: no sign-in session %q", sessionID)
	}
	s.mu.Lock()
	status := s.status
	s.mu.Unlock()
	if status != core.LoginPending {
		return s.snapshot(), nil
	}
	if c.now().Sub(s.startedAt) > c.cfg.loginTimeout() {
		s.finish(core.LoginFailed, "the sign-in window expired; start again", "")
		return s.snapshot(), nil
	}
	select {
	case cb := <-s.callbacks:
		if cb.err != nil {
			s.finish(core.LoginFailed, cb.err.Error(), "")
			return s.snapshot(), nil
		}
		c.completeLogin(ctx, s, cb.code)
	default:
	}
	return s.snapshot(), nil
}

// CancelLogin closes a sign-in window.  It is idempotent: cancelling a session
// that already finished, or never existed, is not an error.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.ensure()
	s, ok := c.getLogin(sessionID)
	if !ok {
		return nil
	}
	s.finish(core.LoginCancelled, "cancelled", "")
	c.dropLogin(sessionID)
	return nil
}

// newLoginSession mints the identifiers and starts the loopback listener.
//
// The state is the CSRF token the callback is checked against, and the uuid is
// the device id the vendor expects on the exchange and on every later renewal --
// both have to survive into the stored account or renewal stops working.
func (c *Client) newLoginSession() (*loginSession, error) {
	state, err := randomHex(16)
	if err != nil {
		return nil, fmt.Errorf("lobsterai: no entropy for a sign-in state: %w", err)
	}
	id, err := newUUIDErr()
	if err != nil {
		return nil, fmt.Errorf("lobsterai: no entropy for a sign-in id: %w", err)
	}
	s := &loginSession{
		id:           id,
		state:        state,
		uuid:         id,
		firstKeyfrom: millisString(c.now()),
		startedAt:    c.now(),
		status:       core.LoginPending,
		callbacks:    make(chan loginCallback, 1),
	}
	addr := fmt.Sprintf("127.0.0.1:%d", c.cfg.LoginPort)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("lobsterai: cannot listen on %s for the sign-in callback: %w", addr, err)
	}
	s.listener = ln
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		s.url = c.cfg.loginURL(c.cfg.callbackURL(tcp.Port), state)
	}
	if s.url == "" {
		ln.Close()
		return nil, fmt.Errorf("lobsterai: could not build the sign-in URL")
	}
	mux := http.NewServeMux()
	mux.HandleFunc(c.cfg.callbackPath(), s.handleCallback)
	s.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	core.GoSafe("lobsterai sign-in callback server", func(msg string) { c.noteError(msg) }, func() {
		if err := s.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.finish(core.LoginFailed, "the local sign-in listener stopped: "+err.Error(), "")
		}
	})
	return s, nil
}

// handleCallback is the one request the loopback server answers.
func (s *loginSession) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("state") != s.state {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		s.finish(core.LoginFailed, "the sign-in callback carried the wrong state", "")
		return
	}
	code := firstNonEmpty(q.Get("code"), q.Get("authCode"))
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		s.finish(core.LoginFailed, "the sign-in callback carried no auth code", "")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, "<!doctype html><meta charset=\"utf-8\"><title>LobsterAI</title><h3>登录成功，可以关闭此窗口了</h3>")
	select {
	case s.callbacks <- loginCallback{code: code}:
	default:
	}
}

// completeLogin exchanges the auth code and stores the account.
func (c *Client) completeLogin(ctx context.Context, s *loginSession, code string) {
	// The exchange records the client version, so it is worth one bounded fetch
	// here rather than signing in with the fallback the config supplies.
	c.refreshVersionIfStale(ctx)
	version := c.clientVersion(ctx)
	acct, err := c.exchange(ctx, code, s.uuid, s.firstKeyfrom, version)
	if err != nil {
		s.finish(core.LoginFailed, "the token exchange failed: "+c.describeError(err), "")
		return
	}
	if err := c.upsertAccount(acct); err != nil {
		s.finish(core.LoginFailed, err.Error(), "")
		return
	}
	c.deps.Log("lobsterai: signed in as %s", acct.ID)
	s.finish(core.LoginSuccess, "signed in as "+firstNonEmpty(acct.Nickname, acct.UID), acct.ID)
}

// --- session store ----------------------------------------------------------

func (c *Client) putLogin(s *loginSession) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.logins == nil {
		c.logins = map[string]*loginSession{}
	}
	ttl := c.cfg.loginTimeout()
	for id, old := range c.logins {
		if c.now().Sub(old.startedAt) > ttl {
			old.shutdown()
			delete(c.logins, id)
		}
	}
	c.logins[s.id] = s
}

func (c *Client) getLogin(id string) (*loginSession, bool) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	s, ok := c.logins[id]
	return s, ok
}

func (c *Client) dropLogin(id string) {
	c.loginMu.Lock()
	s := c.logins[id]
	delete(c.logins, id)
	c.loginMu.Unlock()
	if s != nil {
		s.shutdown()
	}
}

// --- session state ----------------------------------------------------------

// snapshot renders the session for the panel.
func (s *loginSession) snapshot() core.LoginState {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg := s.message
	if s.status == core.LoginPending {
		msg = "Open the sign-in URL, approve it in the browser, then poll again."
	}
	return core.LoginState{
		SessionID: s.id,
		State:     s.status,
		URL:       s.url,
		Message:   msg,
		AccountID: s.accountID,
	}
}

// finish records a terminal state and closes the listener.  The first terminal
// state wins: a late callback must not overwrite a cancellation.
func (s *loginSession) finish(status, message, accountID string) {
	s.mu.Lock()
	changed := s.status == core.LoginPending
	if changed {
		s.status = status
		s.message = message
		if accountID != "" {
			s.accountID = accountID
		}
	}
	s.mu.Unlock()
	if changed {
		s.shutdown()
	}
}

// shutdown stops the loopback listener.  It is idempotent, and it waits briefly
// for the in-flight callback response to be written rather than cutting it off.
func (s *loginSession) shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	server := s.server
	s.mu.Unlock()
	if server == nil {
		return
	}
	// A panic here would otherwise take the whole gateway down from a goroutine
	// nobody is watching.  There is no client reference on the session and no
	// per-session logger, so the trace goes to stderr via GoSafe's fallback.
	core.GoSafe("lobsterai sign-in shutdown", nil, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
}

// --- identifiers ------------------------------------------------------------

// newUUIDErr returns a random UUIDv4.
func newUUIDErr() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// newUUID is newUUIDErr with a non-random fallback, for callers that cannot
// report an error.  A UUID the vendor merely stores does not have to be
// cryptographic; it does have to exist.
func newUUID() string {
	if v, err := newUUIDErr(); err == nil {
		return v
	}
	return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
}

// randomHex returns n random bytes as hex.
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

var _ core.LoginProvider = (*Client)(nil)
