package gemini

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Google OAuth endpoints.  The identity disguise (the five headers) is used
// ONLY on the Cloud Code inference/quota endpoints; the token endpoints are
// ordinary Google OAuth, and the reference sends a default user agent there.
const (
	authEndpoint     = "https://accounts.google.com/o/oauth2/v2/auth"
	tokenEndpoint    = "https://oauth2.googleapis.com/token"
	userinfoEndpoint = "https://www.googleapis.com/oauth2/v2/userinfo"

	// callbackPath is the loopback path the redirect_uri names.  It must match
	// the OAuth client's registration or the exchange fails with
	// redirect_uri_mismatch.
	callbackPath = "/oauth-callback"
	// redirectHost must be "localhost", not "127.0.0.1": the vendor's client
	// registers the name.
	redirectHost = "localhost"
)

// scopes are the six the reference requests verbatim.  cloud-platform is
// required for the inference call; the last two are Cloud Code's own channels.
var scopes = []string{
	"openid",
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

// tokenGrant is a token-endpoint response.
type tokenGrant struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
}

// parseTokenGrant decodes a token-endpoint payload, turning the three failure
// shapes into useful messages.
func parseTokenGrant(payload []byte, previousRefresh string) (tokenGrant, error) {
	var raw struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		TokenType        string `json:"token_type"`
		ExpiresIn        int    `json:"expires_in"`
		Scope            string `json:"scope"`
		IDToken          string `json:"id_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return tokenGrant{}, fmt.Errorf("token endpoint returned unparseable JSON: %w", err)
	}
	if raw.Error != "" {
		if raw.ErrorDescription != "" {
			return tokenGrant{}, fmt.Errorf("%s: %s", raw.Error, raw.ErrorDescription)
		}
		return tokenGrant{}, fmt.Errorf("%s", raw.Error)
	}
	if raw.AccessToken == "" {
		return tokenGrant{}, fmt.Errorf("token endpoint returned no access_token")
	}
	grant := tokenGrant{
		AccessToken: raw.AccessToken,
		TokenType:   firstNonEmpty(raw.TokenType, "Bearer"),
		ExpiresIn:   raw.ExpiresIn,
		Scope:       raw.Scope,
		IDToken:     raw.IDToken,
	}
	if grant.ExpiresIn <= 0 {
		grant.ExpiresIn = 3600
	}
	grant.RefreshToken = firstNonEmpty(raw.RefreshToken, previousRefresh)
	return grant, nil
}

// identityFromIDToken decodes sub/email from an id_token WITHOUT verifying the
// signature: the value is only a display name and a grouping key, and the
// token endpoint's response is already protected by TLS.
func identityFromIDToken(idToken string) (sub, email string) {
	if idToken == "" {
		return "", ""
	}
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", ""
	}
	return claims.Sub, claims.Email
}

// expiryFrom computes an RFC3339 expiry `expiresIn` seconds from now.
func expiryFrom(expiresIn int) string {
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	return time.Now().Add(time.Duration(expiresIn) * time.Second).UTC().Format(time.RFC3339)
}

// postForm sends an application/x-www-form-urlencoded request.
func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		grant, perr := parseTokenGrant(body, "")
		if perr != nil {
			return nil, fmt.Errorf("token endpoint HTTP %d: %v", resp.StatusCode, perr)
		}
		_ = grant
		return nil, fmt.Errorf("token endpoint HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// exchangeCode swaps an authorization code for tokens.  The client_secret is
// mandatory: without it the endpoint answers "client_secret is missing".
func (c *Client) exchangeCode(ctx context.Context, code, redirectURI string) (tokenGrant, error) {
	form := url.Values{
		"client_id":     {c.cfg.clientID()},
		"client_secret": {c.cfg.clientSecret()},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {redirectURI},
	}
	body, err := c.postForm(ctx, tokenEndpoint, form)
	if err != nil {
		return tokenGrant{}, err
	}
	return parseTokenGrant(body, "")
}

// refreshGrant renews a credential with its refresh_token.  Google sometimes
// rotates the refresh token, so the response's new one must be persisted.
func (c *Client) refreshGrant(ctx context.Context, refreshToken string) (tokenGrant, error) {
	if refreshToken == "" {
		return tokenGrant{}, fmt.Errorf("no refresh_token; the account must be signed in again")
	}
	if err := c.cfg.requireOAuthClient(); err != nil {
		return tokenGrant{}, err
	}
	form := url.Values{
		"client_id":     {c.cfg.clientID()},
		"client_secret": {c.cfg.clientSecret()},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}
	body, err := c.postForm(ctx, tokenEndpoint, form)
	if err != nil {
		return tokenGrant{}, err
	}
	return parseTokenGrant(body, refreshToken)
}

// fetchUserInfo is the fallback identity read when the id_token is absent.
func (c *Client) fetchUserInfo(ctx context.Context, accessToken string) (sub, email string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userinfoEndpoint, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("userinfo HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	var info struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", "", err
	}
	return info.ID, info.Email, nil
}

// ---------------------------------------------------------------------------
// interactive login
// ---------------------------------------------------------------------------

// loginSession is one in-flight OAuth flow.
type loginSession struct {
	state      string
	redirect   string
	server     *http.Server
	listener   net.Listener
	loginURL   string
	done       chan struct{}
	mu         sync.Mutex
	finished   bool
	stateValue string // pending|success|failed|cancelled
	accountID  string
	message    string
	closeOnce  sync.Once
}

func (s *loginSession) close() {
	s.closeOnce.Do(func() {
		if s.listener != nil {
			_ = s.listener.Close()
		}
	})
}

func (s *loginSession) setResult(state, accountID, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.finished = true
	s.stateValue = state
	s.accountID = accountID
	s.message = message
	close(s.done)
}

func (s *loginSession) snapshot() (string, string, string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateValue, s.accountID, s.message, s.finished
}

// startOAuthFlow opens a loopback listener, mints the auth URL and returns the
// session.  It returns immediately (two-phase): the browser only allows a popup
// inside the click gesture, so blocking here would expire it.
func (c *Client) startOAuthFlow() (*loginSession, error) {
	if err := c.cfg.requireOAuthClient(); err != nil {
		return nil, err
	}
	state := randomHex(16)
	ln, port, err := listenLoopback(c.cfg.LoginPort)
	if err != nil {
		return nil, err
	}
	redirect := fmt.Sprintf("http://%s:%d%s", redirectHost, port, callbackPath)
	session := &loginSession{
		state:      state,
		redirect:   redirect,
		listener:   ln,
		done:       make(chan struct{}),
		stateValue: "pending",
	}
	session.loginURL = buildAuthURL(state, redirect, c.cfg.clientID())

	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if failure := q.Get("error"); failure != "" {
			desc := q.Get("error_description")
			http.Error(w, "authorization denied: "+failure, http.StatusBadRequest)
			session.setResult("cancelled", "", firstNonEmpty(desc, failure))
			session.close()
			return
		}
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			session.setResult("failed", "", "state mismatch (possible forged callback)")
			session.close()
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing authorization code", http.StatusBadRequest)
			session.setResult("failed", "", "callback carried no authorization code")
			session.close()
			return
		}
		_, _ = w.Write([]byte(callbackHTML))
		// Exchange asynchronously so the browser sees the page immediately.
		go c.finishLogin(session, code)
	})

	server := &http.Server{Handler: mux}
	session.server = server
	go func() { _ = server.Serve(ln) }()
	return session, nil
}

// finishLogin exchanges the code and stores the credential.
func (c *Client) finishLogin(session *loginSession, code string) {
	defer session.close()
	ctx, cancel := context.WithTimeout(context.Background(), c.cfg.loginTimeout())
	defer cancel()

	grant, err := c.exchangeCode(ctx, code, session.redirect)
	if err != nil {
		session.setResult("failed", "", "token exchange failed: "+err.Error())
		return
	}
	sub, email := identityFromIDToken(grant.IDToken)
	if sub == "" && email == "" {
		if s, e, ferr := c.fetchUserInfo(ctx, grant.AccessToken); ferr == nil {
			sub, email = s, e
		}
	}
	rec := c.recordFromGrant(grant, sub, email)
	if err := c.upsertAccount(rec); err != nil {
		session.setResult("failed", "", "storing credential failed: "+err.Error())
		return
	}
	c.deps.Log("gemini: signed in %s", maskSecret(firstNonEmpty(email, sub)))
	session.setResult("success", rec.ID, firstNonEmpty(email, sub, "signed in"))
}

// recordFromGrant builds an account record from a fresh grant, carrying the
// project and identity forward.
func (c *Client) recordFromGrant(grant tokenGrant, sub, email string) accountRecord {
	identity := firstNonEmpty(sub, email)
	rec := accountRecord{
		ID:           accountID(identity),
		Label:        firstNonEmpty(email, sub, identity),
		Identity:     identity,
		AccessToken:  grant.AccessToken,
		RefreshToken: grant.RefreshToken,
		TokenType:    grant.TokenType,
		ExpiresIn:    grant.ExpiresIn,
		Scope:        grant.Scope,
		Expiry:       expiryFrom(grant.ExpiresIn),
		Sub:          sub,
		Email:        email,
		Enabled:      true,
		AddedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	if c.cfg.Project != "" {
		rec.Project = c.cfg.Project
	}
	return rec
}

// buildAuthURL composes the authorization URL.
func buildAuthURL(state, redirectURI, clientID string) string {
	q := url.Values{
		"client_id":              {clientID},
		"response_type":          {"code"},
		"redirect_uri":           {redirectURI},
		"scope":                  {strings.Join(scopes, " ")},
		"state":                  {state},
		"access_type":            {"offline"},
		"include_granted_scopes": {"true"},
		"prompt":                 {"consent"},
	}
	return authEndpoint + "?" + q.Encode()
}

// listenLoopback opens a listener on 127.0.0.1.  Both 127.0.0.1 and [::1] are
// attempted by the reference; a single IPv4 listener is enough here because
// the redirect URI names `localhost`, which resolves to 127.0.0.1 in the
// common case, and the flow retries when a browser picked ::1.
func listenLoopback(preferred int) (net.Listener, int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(preferred))
	if err != nil && preferred != 0 {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		return nil, 0, err
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		ln.Close()
		return nil, 0, fmt.Errorf("unexpected listener address type")
	}
	return ln, addr.Port, nil
}

// callbackHTML is the page the operator sees after authorizing.
const callbackHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>授权完成</title>` +
	`<style>body{font:15px/1.7 -apple-system,"Segoe UI",sans-serif;display:flex;align-items:center;justify-content:center;height:100vh;margin:0;background:#0f1115;color:#e6e8eb}.b{text-align:center}.k{font-size:34px;margin-bottom:8px}</style>` +
	`</head><body><div class="b"><div class="k">&#10003;</div><div>授权已完成，可以关闭此页回到面板。</div></div></body></html>`
