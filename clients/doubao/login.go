package doubao

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// login.go is the LoginProvider: a controlled browser opens the Doubao sign-in
// page, the operator scans the QR code or enters a phone number there, and this
// module reads the resulting HttpOnly cookies out of that browser over CDP and
// stores them as an account.
//
// The browser is the only place the credential exists, which is why the flow
// hands the operator a real window instead of returning a URL to open
// elsewhere: the session cookies are HttpOnly, so the module cannot read them
// from a browser it does not itself drive.

const (
	loginSessionPrefix = "doubao-login-"
	// maxLoginSessionTTL bounds one hand-off even if the configured timeout is
	// absurdly large, so stale panel tabs cannot accumulate forever.
	maxLoginSessionTTL = 30 * time.Minute
)

// loginTTL is the configured hand-off ceiling, clamped so a typo cannot
// leave a pending session around forever.
func (c *Client) loginTTL() time.Duration {
	ttl := c.cfg.loginTimeout()
	if ttl <= 0 || ttl > maxLoginSessionTTL {
		return maxLoginSessionTTL
	}
	return ttl
}

// loginSession is one browser hand-off.  It holds no credential: the cookie is
// read from the browser at the end and stored on the account, not here.
type loginSession struct {
	id        string
	url       string
	startedAt time.Time
	state     string
	message   string
	accountID string

	// browser is the controlled browser once it has launched.  Nil until
	// StartLogin succeeds, and closed when the session ends.
	browser loginBrowser
	// handedOff records that a controlled window was actually opened, so the
	// waiting message can distinguish "the window never appeared" from "the
	// window is open but still signed out".
	handedOff bool
	closed    sync.Once
}

var _ core.LoginProvider = (*Client)(nil)

// StartLogin implements core.LoginProvider.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	pageURL := c.cfg.webBase() + "/"
	s := &loginSession{
		id:        newLoginID(),
		url:       pageURL,
		startedAt: time.Now(),
		state:     core.LoginPending,
	}
	br, err := launchLoginBrowser(ctx, c.cfg, pageURL, c.loginProfileRoot())
	if err != nil {
		s.message = fmt.Sprintf(
			"没能拉起受控浏览器（%s）。请在任意已登录豆包的浏览器里手动复制 Cookie，用账号页的「手动添加」粘贴进来。",
			truncate(core.Redact(err.Error()), 200))
	} else {
		s.browser = br
		s.handedOff = true
		s.message = "已打开一个独立的豆包登录窗口（新窗口，标题栏是「豆包」）。" +
			"请在这个新窗口里扫码或输入手机号完成登录——用你自己平时那个浏览器登录不算，" +
			"模块只读它自己开的这个窗口。登录后回到本面板等待即可。"
	}
	c.putLogin(s)
	return loginStateOf(s), nil
}

// PollLogin implements core.LoginProvider.  Pending is the normal answer while
// the operator is signing in.
func (c *Client) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return core.LoginState{}, errors.New("doubao: PollLogin: empty session id")
	}
	s, ok := c.getLogin(id)
	if !ok {
		return core.LoginState{}, fmt.Errorf("doubao: unknown login session %q (it may have been cancelled or this process restarted; start a new login)", id)
	}
	if s.state != core.LoginPending {
		return loginStateOf(s), nil
	}
	ttl := c.loginTTL()
	if time.Since(s.startedAt) > ttl {
		st, _ := c.updateLogin(id, func(s *loginSession) {
			s.state = core.LoginFailed
			s.message = fmt.Sprintf("登录窗口在 %s 后过期，请重新发起网页登录。", ttl)
		})
		c.closeLoginBrowser(s)
		return st, nil
	}
	if s.browser == nil {
		// Launch failed at StartLogin; there is nothing to poll, but the
		// operator can still add a cookie by hand.
		return loginStateOf(s), nil
	}

	cookieHeader, err := captureSessionCookies(ctx, s.browser, c.cfg.webBase()+"/")
	if err != nil {
		return c.keepWaiting(id, "仍在等待登录完成（读取浏览器 Cookie 失败："+truncate(core.Redact(err.Error()), 160)+"）")
	}
	if !hasSessionCookie(cookieHeader) {
		// Say what was actually seen.  "还没有检测到会话 Cookie" alone cannot
		// tell "the window was never opened" from "it opened but was signed in
		// somewhere else", and both look identical to the operator.
		return c.keepWaiting(id, waitingForLoginMessage(cookieHeader, s.handedOff))
	}

	acc := &account{Cookie: cookieHeader, Label: "web login", Enabled: true, Source: "login"}
	info, verr := c.up.accountInfo(ctx, acc)
	if verr != nil {
		return c.keepWaiting(id, "登录页已打开，但会话还不能用："+truncate(core.Redact(verr.Error()), 160))
	}
	acc.Label = info.displayName()
	acc.Identity = info.identity()
	rec, aerr := c.store.upsert(acc)
	if aerr != nil {
		st, _ := c.updateLogin(id, func(s *loginSession) {
			s.state = core.LoginFailed
			s.message = "读取到的会话无法保存：" + truncate(core.Redact(aerr.Error()), 160)
		})
		c.closeLoginBrowser(s)
		return st, nil
	}
	st, _ := c.updateLogin(id, func(s *loginSession) {
		s.state = core.LoginSuccess
		s.accountID = rec.ID
		s.message = "已从受控浏览器读取豆包会话并存入账号：" + info.displayName()
	})
	c.closeLoginBrowser(s)
	return st, nil
}

// waitingForLoginMessage renders the poll message for a window that has no
// session cookie yet.
//
// The terse "还没有检测到会话 Cookie" cannot tell apart the three states an
// operator actually hits, and each has a different remedy:
//
//   - no window was handed off (launch failed) -> use manual paste;
//   - the window is open but shows no cookie at all -> it is still loading, or
//     the operator is signing in somewhere else;
//   - the window has anonymous cookies (ttwid/s_v_web_id) but no sessionid ->
//     the page is up and the operator simply has not finished signing in THERE.
//
// So it names the cookie count and, when the jar is non-empty, says plainly
// that the sign-in has to happen in this window.
func waitingForLoginMessage(cookieHeader string, handedOff bool) string {
	if !handedOff {
		return "登录窗口没有打开。请用账号页的「手动添加」粘贴一个已登录豆包的浏览器的 Cookie。"
	}
	n := cookieCount(cookieHeader)
	switch {
	case n == 0:
		return "仍在等待登录完成：登录窗口还没有产生任何 Cookie（可能还在加载）。请稍等，或确认新窗口是否被其它窗口挡住了。"
	default:
		return fmt.Sprintf(
			"仍在等待登录完成：登录窗口当前有 %d 个匿名 Cookie，但**还没有 sessionid**，说明这个窗口里还没登录。"+
				"请在那个人独立的豆包窗口里完成登录（不是你自己平时用的浏览器）；登录成功后本面板会在几秒内自动完成。",
			n)
	}
}

// cookieCount counts the pairs in a rendered Cookie header.
func cookieCount(cookieHeader string) int {
	s := strings.TrimSpace(cookieHeader)
	if s == "" {
		return 0
	}
	n := 1
	for _, r := range s {
		if r == ';' {
			n++
		}
	}
	return n
}

// CancelLogin implements core.LoginProvider.  Idempotent.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return errors.New("doubao: CancelLogin: empty session id")
	}
	if s, ok := c.getLogin(id); ok {
		c.closeLoginBrowser(s)
	}
	c.dropLogin(id)
	return nil
}

// closeLoginBrowser closes the controlled browser exactly once.
func (c *Client) closeLoginBrowser(s *loginSession) {
	if s == nil {
		return
	}
	s.closed.Do(func() {
		if s.browser != nil {
			s.browser.Close()
		}
	})
}

// ---------------------------------------------------------------------------
// Session store
// ---------------------------------------------------------------------------

func (c *Client) putLogin(s *loginSession) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.logins == nil {
		c.logins = make(map[string]*loginSession)
	}
	cutoff := time.Now().Add(-c.loginTTL())
	for id, old := range c.logins {
		if old.startedAt.Before(cutoff) {
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

func (c *Client) updateLogin(id string, fn func(*loginSession)) (core.LoginState, bool) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	s, ok := c.logins[id]
	if !ok {
		return core.LoginState{}, false
	}
	fn(s)
	return loginStateOf(s), true
}

func (c *Client) dropLogin(id string) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	delete(c.logins, id)
}

func (c *Client) keepWaiting(id, msg string) (core.LoginState, error) {
	st, ok := c.updateLogin(id, func(s *loginSession) { s.message = core.Redact(msg) })
	if !ok {
		return core.LoginState{}, fmt.Errorf("doubao: unknown login session %q", id)
	}
	return st, nil
}

func loginStateOf(s *loginSession) core.LoginState {
	return core.LoginState{
		SessionID: s.id,
		State:     s.state,
		URL:       s.url,
		Message:   s.message,
		AccountID: s.accountID,
	}
}

func newLoginID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s%d", loginSessionPrefix, time.Now().UnixNano())
	}
	return loginSessionPrefix + hex.EncodeToString(buf[:])
}
