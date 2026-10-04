package codearts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// login.go is the interactive half of the module: the browser sign-in flow and
// the older ticket flow it can fall back to.
//
// CodeArts credentials are minted by the IAM STS endpoint and are only
// reachable with a signed-in session, so there is no way to derive one from a
// pasted password.  Two flows are implemented:
//
//   - the portal OAuth code flow (preferred): the operator opens a URL, the
//     portal redirects back to a local server with an authorisation code, and
//     the code is exchanged for a credential over DPoP;
//   - the legacy ticket flow: the portal redirects back with a `secret`, and
//     the credential is polled from the snap-manager ticket endpoint.
//
// The second exists because the portal decides which flow to use, not this
// module: sending `code_challenge_method=S256` (the RFC 7636 spelling) or an
// `auth_callback_url` makes it choose the legacy one.  Handling both is
// therefore not defensive coding — it is the difference between a sign-in that
// completes and one that silently hangs.

// minCallbackPort is the lowest port the portal will redirect to.  A listener
// below it gets no callback at all, so the port is checked when it is bound
// rather than after the operator has already authorised.
const minCallbackPort = 10000

// callbackAttempts bounds how many ports are tried before giving up.
const callbackAttempts = 20

// ticketPath mints a credential from a legacy ticket.
const ticketPath = "/snap-manager/v1/login/ticket"

// ticketPollInterval is how often the legacy flow asks whether the operator
// has finished signing in.
const ticketPollInterval = time.Second

// ticketMaxAttempts bounds the legacy poll: 120 × 1s = 2 minutes, which is the
// reference implementation's own limit.
const ticketMaxAttempts = 120

// newPKCEChallenge mints a PKCE verifier and its S256 challenge.
func newPKCEChallenge() (string, string, error) {
	pair, err := newPKCEPair()
	if err != nil {
		return "", "", err
	}
	return pair.Verifier, pair.Challenge, nil
}

// ---------------------------------------------------------------------------
// the local callback server
// ---------------------------------------------------------------------------

// callbackResult is what the portal handed back.
type callbackResult struct {
	// code is the OAuth authorisation code (the modern flow).
	code string
	// secret is the legacy ticket secret.
	secret string
	// redirect is where the legacy flow wants the browser to end up.
	redirect string
	// err is set when the callback could not be used.
	err error
}

// localCallback is a one-shot HTTP server the portal redirects to.
type localCallback struct {
	port int
	ln   net.Listener
	srv  *http.Server

	result chan callbackResult
	once   sync.Once
}

// newLocalCallback binds a loopback listener on a port the portal will accept.
//
// It binds and re-binds until it lands on a port in range rather than picking
// one at random: on Windows an ephemeral port is always well above the
// threshold, but on other systems it need not be, and a low port fails only
// after the operator has already signed in — the worst possible time to find
// out.
func newLocalCallback() (*localCallback, error) {
	var lastErr error
	for i := 0; i < callbackAttempts; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("codearts: binding the login callback listener: %w", err)
		}
		addr, ok := ln.Addr().(*net.TCPAddr)
		if !ok || addr.Port < minCallbackPort {
			lastErr = fmt.Errorf("codearts: the portal will not redirect to port %d", addr.Port)
			_ = ln.Close()
			continue
		}
		return &localCallback{
			port:   addr.Port,
			ln:     ln,
			result: make(chan callbackResult, 1),
		}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("codearts: no usable callback port could be bound")
	}
	return nil, lastErr
}

// redirectURI is the exact value the authorisation request and the token
// exchange must agree on.
func (l *localCallback) redirectURI() string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", l.port, oauthRedirectPath)
}

// deliver records the callback exactly once; later requests are ignored, so a
// browser that retries the redirect cannot overwrite a good result.
func (l *localCallback) deliver(r callbackResult) {
	l.once.Do(func() {
		l.result <- r
	})
}

// serve starts answering.  The portal may hit either path: the OAuth flow uses
// the registered redirect path, the legacy flow uses /authentication.
func (l *localCallback) serve() {
	mux := http.NewServeMux()
	mux.HandleFunc(oauthRedirectPath, l.handle)
	mux.HandleFunc("/authentication", l.handle)
	l.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	core.GoSafe("codearts login callback", nil, func() {
		// A closed listener after the flow finishes is the normal path, so
		// the error is discarded rather than logged as a fault.
		_ = l.srv.Serve(l.ln)
	})
}

// close stops the server.  It is safe to call more than once.
func (l *localCallback) close() {
	if l == nil {
		return
	}
	if l.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = l.srv.Shutdown(ctx)
		return
	}
	if l.ln != nil {
		_ = l.ln.Close()
	}
}

// handle turns one redirect into a callbackResult.
func (l *localCallback) handle(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// The legacy flow hands back a secret and expects a redirect somewhere.
	if secret := strings.TrimSpace(q.Get("secret")); secret != "" {
		l.deliver(callbackResult{secret: secret, redirect: strings.TrimSpace(q.Get("redirect"))})
		l.reply(w, "Signing in…", "You can close this window once the panel reports success.")
		return
	}

	if code := strings.TrimSpace(q.Get("code")); code != "" {
		l.deliver(callbackResult{code: code})
		l.reply(w, "Signed in", "You can close this window and return to the panel.")
		return
	}

	// A denial, or a portal that answered with neither parameter.
	msg := strings.TrimSpace(q.Get("error_description"))
	if msg == "" {
		msg = strings.TrimSpace(q.Get("error"))
	}
	if msg == "" {
		msg = "the portal returned no authorisation code"
	}
	l.deliver(callbackResult{err: errors.New("codearts: " + msg)})
	l.reply(w, "Sign-in failed", msg)
}

// reply writes the small page the operator sees in the browser tab.
func (l *localCallback) reply(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>%s</title></head><body style="font-family:system-ui;padding:2rem"><h1>%s</h1><p>%s</p></body></html>`,
		htmlEscape(title), htmlEscape(title), htmlEscape(body))
}

// htmlEscape escapes the few characters that matter for the small result page.
// The values come from the portal's query string, so they are not trusted.
func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

// ---------------------------------------------------------------------------
// the flow
// ---------------------------------------------------------------------------

// runBrowserLogin drives the whole sign-in: serve the callback, wait for the
// portal to redirect, redeem what came back, and store the credential.
func (c *Client) runBrowserLogin(ctx context.Context, sess *panelLogin, ln *localCallback, key *dpopKeyPair) {
	if ln == nil {
		sess.set(core.LoginFailed, "the sign-in could not start: no callback listener", "")
		return
	}
	defer ln.close()
	ln.serve()

	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			sess.set(core.LoginCancelled, "cancelled", "")
		} else {
			sess.set(core.LoginFailed, "the sign-in was not completed in time", "")
		}
		return
	case res := <-ln.result:
		if res.err != nil {
			sess.set(core.LoginFailed, res.err.Error(), "")
			return
		}
		if res.secret != "" {
			c.completeTicketLogin(ctx, sess, res)
			return
		}
		c.completeCodeLogin(ctx, sess, ln, key, res.code)
	}
}

// completeCodeLogin redeems an authorisation code and stores the credential.
func (c *Client) completeCodeLogin(ctx context.Context, sess *panelLogin, ln *localCallback, key *dpopKeyPair, code string) {
	acct, err := c.exchangeAuthorizationCode(ctx, code, sess.verifier, ln.redirectURI(), key)
	if err != nil {
		sess.set(core.LoginFailed, "the portal authorised the sign-in, but the credential could not be fetched: "+core.Redact(err.Error()), "")
		return
	}
	c.storeLoginResult(sess, acct)
}

// completeTicketLogin polls the legacy ticket endpoint until the credential
// appears, then stores it.
func (c *Client) completeTicketLogin(ctx context.Context, sess *panelLogin, res callbackResult) {
	ticketID := sess.nonce
	deadline := time.Now().Add(c.cfg.loginTimeout())
	for i := 0; i < ticketMaxAttempts; i++ {
		if time.Now().After(deadline) || ctx.Err() != nil {
			break
		}
		acct, err := c.fetchTicketCredential(ctx, ticketID, res.secret)
		if err == nil {
			c.storeLoginResult(sess, acct)
			return
		}
		if !errors.Is(err, errTicketPending) {
			// A real failure is reported once; a pending ticket is not.
			c.deps.Log("codearts: polling the login ticket: %v", err)
		}
		if !core.SleepCtx(ctx, ticketPollInterval) {
			break
		}
	}
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		sess.set(core.LoginCancelled, "cancelled", "")
		return
	}
	sess.set(core.LoginFailed, "the sign-in was not confirmed by the vendor in time", "")
}

// errTicketPending means the operator has not finished signing in yet.  It is
// the expected answer for most of the poll, so it is kept distinct from a real
// failure.
var errTicketPending = errors.New("codearts: the login ticket is not ready yet")

// ticketCredential is the ticket endpoint's answer.
type ticketCredential struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SecurityToken   string `json:"security_token"`
	ExpiresAt       string `json:"expires_at"`
	Expiration      string `json:"expiration"`
	DomainID        string `json:"domain_id"`
	UserID          string `json:"user_id"`
	UserName        string `json:"user_name"`
	// Some responses wrap the credential.
	Credential *ticketCredential `json:"credential"`
}

// usable reports whether the response actually carried a credential.
func (t *ticketCredential) usable() bool {
	if t == nil {
		return false
	}
	if t.Credential != nil && t.Credential.usable() {
		return true
	}
	return strings.TrimSpace(t.AccessKeyID) != "" && strings.TrimSpace(t.SecretAccessKey) != ""
}

// unwrap returns the credential, following one level of nesting.
func (t *ticketCredential) unwrap() ticketCredential {
	if t == nil {
		return ticketCredential{}
	}
	if t.Credential != nil && t.Credential.usable() {
		return *t.Credential
	}
	return *t
}

// fetchTicketCredential asks the ticket endpoint once.
//
// This endpoint is NOT signed with the AK/SK — it is what produces them — so
// it carries only the plugin identity headers the reference implementation
// sends.
func (c *Client) fetchTicketCredential(ctx context.Context, ticketID, secret string) (account, error) {
	q := url.Values{}
	q.Set("ticket_id", ticketID)
	q.Set("secret", secret)
	rawURL := c.cfg.ticketURL() + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return account{}, fmt.Errorf("codearts: building the ticket request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("plugin-name", ticketPluginName)
	req.Header.Set("plugin-version", ticketPluginVersion)
	req.Header.Set("User-Agent", c.cfg.userAgent())

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return account{}, fmt.Errorf("codearts: calling the login ticket endpoint: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// A 404 or an empty body is the normal "not ready yet" answer while the
	// operator is still on the consent screen.
	var out ticketCredential
	if err := json.Unmarshal(raw, &out); err != nil {
		if resp.StatusCode >= 400 {
			return account{}, fmt.Errorf("codearts: the login ticket endpoint answered HTTP %d", resp.StatusCode)
		}
		return account{}, errTicketPending
	}
	if !out.usable() {
		if resp.StatusCode >= 400 {
			return account{}, fmt.Errorf("codearts: the login ticket endpoint answered HTTP %d: %s",
				resp.StatusCode, cleanErrorText(string(raw)))
		}
		return account{}, errTicketPending
	}
	t := out.unwrap()
	expiry := firstNonEmpty(t.ExpiresAt, t.Expiration)
	acct := account{
		AccessKeyID:     strings.TrimSpace(t.AccessKeyID),
		SecretAccessKey: strings.TrimSpace(t.SecretAccessKey),
		SecurityToken:   strings.TrimSpace(t.SecurityToken),
		ExpiresAt:       parseExpiry(expiry),
		DomainID:        strings.TrimSpace(t.DomainID),
		UserID:          strings.TrimSpace(t.UserID),
		UserName:        strings.TrimSpace(t.UserName),
	}
	if acct.SecurityToken == "" {
		// Without a security token the credential cannot sign a chat request,
		// so accepting it would only move the failure later.
		return account{}, errors.New("codearts: the login ticket returned a credential without a security token")
	}
	if acct.ExpiresAt == 0 {
		acct.ExpiresAt = time.Now().Add(24 * time.Hour).Unix()
	}
	acct.ID = acct.accountID()
	return acct, nil
}

// storeLoginResult persists a freshly minted credential and reports success.
func (c *Client) storeLoginResult(sess *panelLogin, acct account) {
	if c.accountsPath == "" {
		sess.set(core.LoginFailed, "signed in, but there is no data directory to store the credential in", "")
		return
	}
	if err := c.storeAccount(acct); err != nil {
		sess.set(core.LoginFailed, "signed in, but the credential could not be stored: "+err.Error(), "")
		return
	}
	c.removeLoginFile()
	sess.set(core.LoginSuccess, "signed in; the credential is stored and live", acct.ID)
}

// storeAccount writes one credential through the pool, so the running client
// starts using it immediately.
func (c *Client) storeAccount(acct account) error {
	now := time.Now().Unix()
	if acct.CreatedAt == 0 {
		acct.CreatedAt = now
	}
	acct.UpdatedAt = now
	c.pool.put(acct, true)
	c.pool.revive(acct.ID)
	return nil
}
