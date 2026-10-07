package tabbit

// Interactive login for tabbit, and why it looks different from the other
// modules.
//
// Every other client in this repo ends a login by *storing* something: an
// access token, a refresh token, a device-code grant.  tabbit's sign-in happens
// inside the Tabbit browser, and what it mints is an httpOnly `token` cookie in
// that browser's profile -- a page opened anywhere else just bounces to the
// vendor's site.  So there are exactly two ways the session reaches this
// module, and login.go only drives the first step of each:
//
//  1. Import (preferred).  The operator signs in inside the Tabbit browser and
//     then uses the panel's 导入凭据 action, whose browser-cookie entry runs a
//     Playwright program through the Tabbit launcher to read the cookie out of
//     the running browser and store it as a web-token account (webimport.go).
//     The web transport then talks to web.tabbit.com directly.
//  2. The tabbit2api sidecar (alternative).  The sidecar drives the browser
//     itself, so this module only talks HTTP to it and never sees a credential.
//
// StartLogin therefore hands back the vendor URL -- which has to be opened in
// the Tabbit browser -- and says which of the two paths this machine can take.
// When the browser's own launcher is installed it uses it: the page is opened
// in the Tabbit browser and brought forward, and the state comes back with
// LocalApp set, which is the panel's signal to stop offering its own "open in
// browser" link and finish through HandoffPath instead.
//
// PollLogin only ever reports on the sidecar path: an imported cookie lands in
// the account table, not in a login session, so there is nothing to poll.  A
// hand-off session says exactly that -- it stays pending until the operator
// imports the cookie the browser just minted.
//
// Consequences that are deliberate, and that the panel and README repeat:
//
//   - A login session stores no credential: url is a public vendor URL.
//   - A successful sidecar login does NOT create an account row.  The account
//     table is the panel's editing surface (see accounts.go and
//     MODULE-CONTRACT), and there is nothing here to edit: the endpoint stays
//     where it already was, and Status() keeps reporting whether it answers.
//   - The session is pending, not failed, while the sidecar is down.  The
//     operator can complete the browser sign-in with the sidecar stopped; the
//     panel just cannot confirm it until the sidecar answers.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"client2api/internal/core"
)

const (
	// defaultWebHost is the host the Tabbit web app actually serves from, as
	// observed on a live session.  The upstream sidecar's own default is
	// "web.tabbit.ai", which is exactly the drift the README documents as a
	// hazard, so config.WebHost overrides this.
	defaultWebHost = "web.tabbit.com"

	// loginFlowQuery is the query string the Tabbit browser uses for its
	// sign-in flow.  callback=close makes the page close itself once it is
	// done, so there is nothing to copy back into this panel; flow and theme
	// are the vendor's own parameters and are passed through verbatim.
	loginFlowQuery = "callback=close&flow=history_opt_in&theme=mn"

	// loginPath is the web app's login entry point.
	loginPath = "/login"

	// loginSessionPrefix namespaces the ids this module hands out, so a session
	// id is self-describing in a log or a bug report.
	loginSessionPrefix = "tabbit-login-"

	// loginSessionTTL bounds how long one hand-off may be polled.  The browser
	// flow has no vendor-side expiry of its own -- nothing is minted and
	// nothing expires -- so this exists only so a panel window left open for
	// days cannot accumulate sessions forever.
	loginSessionTTL = 30 * time.Minute
)

var _ core.LoginProvider = (*Client)(nil)

// loginSession is one browser hand-off.  It holds no credential: url is a
// public vendor URL, and everything else is progress reporting.
type loginSession struct {
	id        string
	url       string
	startedAt time.Time
	state     string
	message   string
	accountID string
	// localApp is true when the page was handed to the Tabbit browser itself
	// rather than left for the operator to open, and handoffPath is the
	// credential the panel imports once that sign-in is finished.  Both are
	// false/empty for the ordinary sidecar hand-off.
	localApp    bool
	handoffPath string
}

// StartLogin implements core.LoginProvider.  It is deliberately not "start
// something": there is no OAuth client, no device code and no CLI to drive, so
// what it hands back is the browser URL plus a message that says whether the
// sidecar is already up.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.cfgErr != nil {
		return core.LoginState{}, fmt.Errorf("tabbit: %w", c.cfgErr)
	}
	loc := c.locate()
	url := loginURL(loc.webHost)
	// A bounded probe, never a spawn: it is what lets the message say whether
	// the sidecar is up and which host it drives, instead of just printing a
	// URL and letting the operator find out on their own.
	h := c.probe(ctx, loc, c.cfg.Timeouts.statusBudget())

	s := &loginSession{
		id:        newLoginID(),
		url:       url,
		startedAt: time.Now(),
		state:     core.LoginPending,
	}
	// The page has to be opened in the Tabbit browser, and this process
	// already knows how to drive that browser's launcher: hand the URL over
	// instead of telling the operator to paste it somewhere that cannot
	// complete the sign-in.  A machine without a usable launcher keeps the
	// old hand-off, with the failure spelled out.
	if err := c.openLoginPageInTabbit(ctx, url); err != nil {
		s.message = c.loginStartMessage(ctx, loc, h, url) + " " + handoffFailedMessage(err)
	} else {
		s.localApp = true
		s.handoffPath = browserCookiePath
		s.message = handoffMessage(url)
	}
	c.putLogin(s)
	return loginStateOf(s), nil
}

// PollLogin implements core.LoginProvider.  Pending is the normal answer: the
// operator may take minutes in the browser, and the sidecar may not even be
// running yet.
func (c *Client) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return core.LoginState{}, errors.New("tabbit: PollLogin: empty session id")
	}
	s, ok := c.getLogin(id)
	if !ok {
		return core.LoginState{}, unknownLoginErr(id)
	}
	// A finished (or cancelled, or expired) session is terminal: re-report it
	// instead of probing again, so the panel's poll loop always ends.
	if s.state != core.LoginPending {
		return loginStateOf(s), nil
	}
	if time.Since(s.startedAt) > loginSessionTTL {
		st, _ := c.updateLogin(id, func(s *loginSession) {
			s.state = core.LoginFailed
			s.message = fmt.Sprintf(
				"this login window expired after %s without the sidecar seeing a signed-in browser; start a new login",
				loginSessionTTL)
		})
		return st, nil
	}

	if s.localApp {
		// A hand-off has nothing to poll: the credential lands in the browser
		// profile, and the panel finishes the flow by importing it.  Probing
		// here would also relaunch the browser under the operator's hands.
		return loginStateOf(s), nil
	}

	loc := c.locate()
	if strings.TrimSpace(loc.baseURL) == "" {
		return c.keepWaiting(id, "no sidecar endpoint is known yet: set clients.tabbit.base_url (or install tabbit2api), then poll again — or import the browser cookie instead, which needs no sidecar at all")
	}

	// The verdict is the catalogue, and nothing else: /health answers on a
	// sidecar that drives an unauthenticated browser too, so it cannot prove a
	// sign-in.  A model list can.
	models, err := c.fetchModels(ctx, loc)
	switch {
	case err == nil && len(models) > 0:
		c.storeCatalog(models, time.Now())
		c.setDiscovered(loc.baseURL)
		st, _ := c.updateLogin(id, func(s *loginSession) {
			s.state = core.LoginSuccess
			s.accountID = loc.baseURL
			s.message = fmt.Sprintf(
				"the sidecar %s now lists %d model(s), so the browser session is signed in. "+
					"Nothing was stored in this process: the Tabbit session lives in the browser profile, which the sidecar reads.",
				loc.baseURL, len(models))
		})
		return st, nil
	case errors.Is(err, errNoUsableModels):
		return c.keepWaiting(id, fmt.Sprintf(
			"the sidecar %s answered but listed no models, so the browser does not look signed in yet", loc.baseURL))
	default:
		msg := fmt.Sprintf("the sidecar at %s is not usable yet: %s", loc.baseURL, scrubSecret(err.Error(), loc.apiKey))
		if h := c.probe(ctx, loc, c.cfg.Timeouts.statusBudget()); h.ok {
			msg += fmt.Sprintf(" (/health answers: %s, %s%s)", versionLabel(h), modelCountLabel(h), hostSuffix(h, loc.webHost))
		}
		msg += ". Finish the sign-in in the Tabbit browser, then leave this window open."
		return c.keepWaiting(id, msg)
	}
}

// CancelLogin implements core.LoginProvider.  It is idempotent: cancelling a
// session that is already gone is a no-op, not an error, because the panel can
// legitimately race a close against a poll.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	id := strings.TrimSpace(sessionID)
	if id == "" {
		return errors.New("tabbit: CancelLogin: empty session id")
	}
	c.dropLogin(id)
	return nil
}

// ---------------------------------------------------------------------------
// Session store.  Lazily created so a Client assembled by a test without New
// keeps working.
// ---------------------------------------------------------------------------

func (c *Client) putLogin(s *loginSession) {
	c.loginMu.Lock()
	defer c.loginMu.Unlock()
	if c.logins == nil {
		c.logins = make(map[string]*loginSession)
	}
	// Sweep here rather than on a timer: sessions are only ever created by an
	// operator, so this is already rare enough to be the natural place.
	cutoff := time.Now().Add(-loginSessionTTL)
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

// updateLogin mutates one session under the lock and reports the new state.
// ok is false when the session is gone (cancelled while a poll was in flight).
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

// keepWaiting records a still-pending message.  The error return covers the one
// race that matters: the operator cancelled while the poll was on the wire.
func (c *Client) keepWaiting(id, msg string) (core.LoginState, error) {
	st, ok := c.updateLogin(id, func(s *loginSession) { s.message = core.Redact(msg) })
	if !ok {
		return core.LoginState{}, unknownLoginErr(id)
	}
	return st, nil
}

func unknownLoginErr(id string) error {
	return fmt.Errorf("tabbit: unknown login session %q (it may have been cancelled, or this process was restarted; start a new login)", id)
}

func loginStateOf(s *loginSession) core.LoginState {
	return core.LoginState{
		SessionID:   s.id,
		State:       s.state,
		URL:         s.url,
		Message:     s.message,
		AccountID:   s.accountID,
		LocalApp:    s.localApp,
		HandoffPath: s.handoffPath,
	}
}

// handoffMessage is what the operator reads once the login page has been placed
// in the Tabbit browser.  It is panel copy, not a log line, so it is written in
// the same language as the dialog around it -- and it names the panel button
// that finishes the flow, because that button is what replaces "open in browser".
func handoffMessage(url string) string {
	return fmt.Sprintf(
		"已唤起 Tabbit 客户端并打开登录页（%s）。请在客户端里完成登录，然后回到面板点「已完成登录，读取凭据」："+
			"面板会从那个浏览器读出新的 web-token 会话，就地更新这个账号。", url)
}

// handoffFailedMessage explains a launcher that could not be used and names the
// route that is left on such a machine: open the page in the Tabbit browser by
// hand, then import the cookie.  Handing back the raw launcher error matters --
// BROWSER_LAUNCH_FAILED tells the operator the browser itself needs attention.
func handoffFailedMessage(err error) string {
	return "没能调起 Tabbit 客户端（" + truncate(err.Error(), 200) + "）。" +
		"请在 Tabbit 浏览器里手动打开上面的链接完成登录，再用本面板的「导入凭据」把 web-token 会话读进来。"
}

// ---------------------------------------------------------------------------
// The URL and the first message
// ---------------------------------------------------------------------------

// loginURL builds the browser sign-in URL.  The host is configurable because
// the vendor has served the app from more than one host, and a login page on
// the wrong one fails in confusing ways (README, "Web-host drift").  It comes
// from `location`, so an operator who set the host in the panel gets it here
// too, not only in the account note.
func loginURL(host string) string {
	return "https://" + loginHost(host) + loginPath + "?" + loginFlowQuery
}

// loginHost reduces whatever the operator configured to a bare host.  An empty
// value means the documented default.
func loginHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return defaultWebHost
	}
	// Accept what an operator is most likely to paste: a full URL.
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?#"); i >= 0 {
		host = host[:i]
	}
	host = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(host), "/"))
	if host == "" {
		return defaultWebHost
	}
	return host
}

// loginStartMessage says what to do with the URL and which of the two paths
// this machine can actually take.  It asks Discover, which is a stat call and
// never a browser task, so the answer is about this machine rather than a guess.
func (c *Client) loginStartMessage(ctx context.Context, loc location, h healthState, url string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Open %s in the Tabbit browser (a normal browser just bounces to the vendor's site; the page closes itself when it is done). ", url)
	b.WriteString("Then bring the session into this module as a web-token account with this panel's 导入凭据 action: ")

	cookie, ok := c.discoverBrowserCookie(ctx)
	switch {
	case ok && cookie.Importable:
		fmt.Fprintf(&b, "import %q, which reads the cookie out of the running Tabbit browser and stores it for the web transport to use directly. ", cookie.Path)
	case ok:
		fmt.Fprintf(&b, "its browser-cookie entry (%q) cannot be used here: %s ", cookie.Path, cookie.Note)
	default:
		b.WriteString("the browser-cookie entry cannot be used here (no Tabbit launcher was found), so paste the `token` cookie in by hand. ")
	}

	if loc.baseURL == "" {
		b.WriteString("No sidecar endpoint is known either, so the imported cookie is the only path that can serve tabbit.")
		return b.String()
	}
	if h.ok {
		fmt.Fprintf(&b, "The sidecar %s is up (%s, %s%s); this panel will report success once it can list models again.",
			loc.baseURL, versionLabel(h), modelCountLabel(h), hostSuffix(h, loc.webHost))
	} else {
		fmt.Fprintf(&b, "The sidecar at %s is not answering yet (%s), so the sidecar path cannot be confirmed until it is running.",
			loc.baseURL, truncate(h.err, 160))
	}
	if hint := hostDriftHint(h, loginHost(loc.webHost)); hint != "" {
		b.WriteString(" " + hint)
	}
	return b.String()
}

func hostDriftHint(h healthState, used string) string {
	observed := strings.TrimSpace(h.host)
	if observed == "" || strings.EqualFold(observed, used) {
		return ""
	}
	return fmt.Sprintf("The sidecar reports it drives %s, while this URL uses %s; set clients.tabbit.web_host if the page does not load.", observed, used)
}

func newLoginID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand failing is not a reason to refuse the operator a login;
		// the id only has to be unique within this process.
		return fmt.Sprintf("%s%d", loginSessionPrefix, time.Now().UnixNano())
	}
	return loginSessionPrefix + hex.EncodeToString(buf[:])
}
