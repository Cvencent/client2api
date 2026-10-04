package zcode

// Panel login: the Z.AI "OAuth CLI" flow driven entirely from Go.
//
// Ported from the reference implementation at
// zcode2api-lab/dengyie/app/oauth.py (class ZaiAuthFlow) and its caller
// app/routes/admin_api.py.  The vendor binds the OAuth session to a device
// context, so the two requests that belong to the flow carry *only* the
// headers the official CLI sends:
//
//	init -> Authorization: Bearer <poll_token> + Content-Type: application/json
//	poll -> Authorization: Bearer <poll_token>
//
// No identity headers, no cookies, no User-Agent override.  The exchange walk
// that follows is an ordinary authenticated API sequence and does carry the
// business token.
//
// The flow never runs in the background: StartLogin performs init, and every
// PollLogin performs exactly one poll of the vendor, which is what the panel's
// timer already expects.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

const (
	// The two flow endpoints, relative to oauthAPIBase().
	oauthInitPath = "/oauth/cli/init"
	oauthPollPath = "/oauth/cli/poll"
	// oauthProvider is the only body field the reference sends on init.
	oauthProvider = "zai"

	// The exchange walk, relative to oauthExchangeBase().
	exchangeLoginPath    = "/api/auth/z/login"
	exchangeCustomerPath = "/api/biz/customer/getCustomerInfo"
	exchangeKeysPath     = "/api/biz/v1/organization/%s/projects/%s/api_keys"
	exchangeKeyName      = "zcode-api-key"

	// defaultLoginTTL mirrors the reference's LOGIN_FLOW_TTL (300s), which is
	// also the "expires_in" the vendor returns on init.
	defaultLoginTTL = 300 * time.Second

	// webLoginRetention keeps a finished or expired session answerable for a
	// while, so a panel that polls one beat too late is told "failed" rather
	// than "not found".
	webLoginRetention = 15 * time.Minute

	// webLoginFile holds the in-flight sessions in core.Deps.DataDir.  It is
	// the only file in this module that stores the poll token, so it is
	// written 0600 through core.WriteJSONAtomic and removed as soon as no
	// session is live.
	webLoginFile    = "web_login.json"
	webLoginVersion = 1

	// pollTokenBytes matches the reference's secrets.token_hex(32): 32 random
	// bytes rendered as 64 hex characters.
	pollTokenBytes = 32

	loginBodyLimit = 1 << 20
	loginNoteLimit = 240

	// originPanelLogin is the panel-visible provenance of a credential this
	// flow created.
	originPanelLogin = "panel-login"
	originStored     = "stored"
	originConfig     = "config"

	labelPanelLogin = "z.ai panel login"
)

// ---------------------------------------------------------------------------
// Session state
// ---------------------------------------------------------------------------

// webLoginEntry is the durable half of a session.  It is a separate struct so
// the session's mutex is never copied.
type webLoginEntry struct {
	SessionID string    `json:"session_id"`
	PollToken string    `json:"poll_token"`
	FlowID    string    `json:"flow_id"`
	URL       string    `json:"url,omitempty"`
	Code      string    `json:"code,omitempty"`
	StartedAt time.Time `json:"started_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// webLogin is one in-flight flow started from the panel.
type webLogin struct {
	webLoginEntry

	mu        sync.Mutex
	state     string
	message   string
	accountID string
}

func (s *webLogin) set(state, message, accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	s.message = core.Redact(message)
	s.accountID = accountID
}

// terminal reports whether the session has reached a final state.  A terminal
// session is answered from memory and never re-polls the vendor.
func (s *webLogin) terminal() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case core.LoginSuccess, core.LoginFailed, core.LoginCancelled:
		return true
	}
	return false
}

// expired reports whether the vendor's authorisation window has closed.
func (s *webLogin) expired(now time.Time) bool {
	return !s.ExpiresAt.IsZero() && !now.Before(s.ExpiresAt)
}

// snapshot renders the session for the panel.  It is a pure read: the expiry
// transition is applied by PollLogin, which also drops the persisted session.
func (s *webLogin) snapshot() core.LoginState {
	s.mu.Lock()
	state := s.state
	message := s.message
	accountID := s.accountID
	s.mu.Unlock()

	if state == "" {
		state = core.LoginPending
	}
	if state == core.LoginPending && s.expired(time.Now()) {
		state = core.LoginFailed
		message = "the authorisation window expired; start again"
	}
	return core.LoginState{
		SessionID: s.SessionID,
		State:     state,
		URL:       s.URL,
		Code:      s.Code,
		Message:   core.Redact(message),
		AccountID: accountID,
	}
}

type webLoginStore struct {
	Version  int             `json:"version"`
	Sessions []webLoginEntry `json:"sessions"`
}

// loginSessions is the Client's in-memory index of live flows, mirrored to
// webLoginFile so a restart can still answer a poll.
type loginSessions struct {
	mu     sync.Mutex
	loaded bool
	byID   map[string]*webLogin
}

// ensureLocked loads webLoginFile once.  Sessions that are past their
// retention window are not resurrected.
func (ls *loginSessions) ensureLocked(dir string, logf func(string, ...any)) {
	if ls.loaded {
		return
	}
	ls.loaded = true
	if ls.byID == nil {
		ls.byID = map[string]*webLogin{}
	}
	if dir == "" {
		return
	}
	var st webLoginStore
	err := core.ReadJSON(filepath.Join(dir, webLoginFile), &st)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && logf != nil {
			logf("zcode: cannot read %s: %v", webLoginFile, err)
		}
		return
	}
	now := time.Now()
	for _, e := range st.Sessions {
		if e.SessionID == "" || e.PollToken == "" || e.FlowID == "" {
			continue
		}
		if !now.Before(e.ExpiresAt.Add(webLoginRetention)) {
			continue
		}
		ls.byID[e.SessionID] = &webLogin{webLoginEntry: e, state: core.LoginPending}
	}
}

// saveLocked rewrites webLoginFile with the sessions that are still live, and
// removes the file once none is left.
func (ls *loginSessions) saveLocked(dir string, logf func(string, ...any)) {
	if dir == "" {
		return
	}
	now := time.Now()
	st := webLoginStore{Version: webLoginVersion}
	for _, s := range ls.byID {
		if s.terminal() || !now.Before(s.ExpiresAt.Add(webLoginRetention)) {
			continue
		}
		st.Sessions = append(st.Sessions, s.webLoginEntry)
	}
	path := filepath.Join(dir, webLoginFile)
	if len(st.Sessions) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && logf != nil {
			logf("zcode: cannot remove %s: %v", webLoginFile, err)
		}
		return
	}
	if err := core.WriteJSONAtomic(path, st); err != nil && logf != nil {
		logf("zcode: cannot persist %s: %v", webLoginFile, err)
	}
}

// ---------------------------------------------------------------------------
// Client plumbing
// ---------------------------------------------------------------------------

func (c *Client) logf() func(string, ...any) { return c.deps.Logf }

func (c *Client) putLogin(s *webLogin) {
	ls := c.logins
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.ensureLocked(c.pool.dir, c.logf())
	ls.byID[s.SessionID] = s
	ls.saveLocked(c.pool.dir, c.logf())
}

func (c *Client) loginByID(id string) (*webLogin, bool) {
	ls := c.logins
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.ensureLocked(c.pool.dir, c.logf())
	s, ok := ls.byID[id]
	return s, ok
}

// persistLogins rewrites the session file after a state transition.
func (c *Client) persistLogins() {
	ls := c.logins
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.ensureLocked(c.pool.dir, c.logf())
	ls.saveLocked(c.pool.dir, c.logf())
}

// pruneLogins forgets sessions that are past their retention window.  It runs
// on every StartLogin, which bounds the map without a background goroutine.
func (c *Client) pruneLogins() {
	ls := c.logins
	now := time.Now()
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.ensureLocked(c.pool.dir, c.logf())
	for id, s := range ls.byID {
		if now.Before(s.ExpiresAt.Add(webLoginRetention)) {
			continue
		}
		delete(ls.byID, id)
	}
	ls.saveLocked(c.pool.dir, c.logf())
}

// ---------------------------------------------------------------------------
// core.LoginProvider
// ---------------------------------------------------------------------------

// StartLogin implements core.LoginProvider.  It performs the flow's init call
// and returns a pending state whose URL the operator opens in a browser.
//
// Unlike qwenwork this does not short-circuit when an account is already
// usable: zcode's pool is explicitly multi-account, so a second panel login is
// a legitimate operation.
func (c *Client) StartLogin(ctx context.Context) (core.LoginState, error) {
	if c.pool.dir == "" {
		return core.LoginState{}, errors.New("no data directory: a credential could not be stored")
	}
	c.pruneLogins()

	token, err := newPollToken()
	if err != nil {
		return core.LoginState{}, err
	}
	flow, err := c.oauthInit(ctx, token)
	if err != nil {
		return core.LoginState{}, err
	}

	started := time.Now()
	sess := &webLogin{
		webLoginEntry: webLoginEntry{
			SessionID: shortID(),
			PollToken: token,
			FlowID:    flow.FlowID,
			URL:       flow.AuthorizeURL,
			Code:      userCodeFromURL(flow.AuthorizeURL),
			StartedAt: started,
			ExpiresAt: started.Add(c.cfg.loginTTL()),
		},
		state:   core.LoginPending,
		message: "open the URL in a browser and finish the Z.AI sign-in; this panel keeps polling",
	}
	c.putLogin(sess)
	return sess.snapshot(), nil
}

// PollLogin implements core.LoginProvider.  It polls the vendor exactly once
// and, when the vendor reports the flow as ready, performs the full
// access-token -> business-token -> organisation/project -> API-key walk and
// stores the result in the module's own account store.
func (c *Client) PollLogin(ctx context.Context, sessionID string) (core.LoginState, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return core.LoginState{}, errors.New("session id is required")
	}
	sess, ok := c.loginByID(sessionID)
	if !ok {
		return core.LoginState{}, fmt.Errorf("login session %q not found", sessionID)
	}
	if sess.terminal() {
		return sess.snapshot(), nil
	}
	if sess.expired(time.Now()) {
		sess.set(core.LoginFailed, "the authorisation window expired; start again", "")
		c.persistLogins()
		return sess.snapshot(), nil
	}

	// Every message this poll shows the operator is scrubbed against the
	// secrets the session is holding.  The vendor echoes request material back
	// in error text often enough that this is the difference between a note and
	// a leaked token.
	secrets := []string{sess.PollToken}

	data, err := c.oauthPoll(ctx, sess)
	if err != nil {
		var term *terminalLoginError
		if errors.As(err, &term) {
			sess.set(core.LoginFailed, scrubSecrets(term.Error(), secrets...), "")
			c.persistLogins()
			return sess.snapshot(), nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return sess.snapshot(), ctxErr
		}
		// A 5xx or a transport failure is transient: stay pending so the
		// operator can still finish the sign-in.
		sess.set(core.LoginPending, scrubSecrets(err.Error(), secrets...), "")
		return sess.snapshot(), nil
	}

	switch strings.ToLower(strings.TrimSpace(data.Status)) {
	case "failed", "denied", "rejected":
		sess.set(core.LoginFailed,
			scrubSecrets(firstNonEmpty(data.Message, data.Reason,
				"the authorisation was denied"), secrets...), "")
		c.persistLogins()
		return sess.snapshot(), nil
	case "ready":
		// fall through to the credential walk
	default:
		sess.set(core.LoginPending, "", "")
		return sess.snapshot(), nil
	}

	// The vendor just handed the credential over, so it joins the scrub set
	// before any of it can appear in a note.
	secrets = append(secrets, data.Token, data.accessToken())

	acct, note, err := c.credentialFromAuth(ctx, data)
	if err != nil {
		sess.set(core.LoginFailed, scrubSecrets(err.Error(), secrets...), "")
		c.persistLogins()
		return sess.snapshot(), nil
	}
	if err := c.pool.addManaged(acct); err != nil {
		var exists *errAccountExists
		if errors.As(err, &exists) {
			// The identical credential is already configured, so the operator
			// has what they asked for; report success rather than looping.
			id, _ := c.pool.accountIDForSecret(secretOfManaged(acct))
			sess.set(core.LoginSuccess, "this credential is already configured as account "+id, id)
			c.persistLogins()
			return sess.snapshot(), nil
		}
		sess.set(core.LoginFailed, scrubSecrets(err.Error(), secrets...), "")
		c.persistLogins()
		return sess.snapshot(), nil
	}
	sess.set(core.LoginSuccess, scrubSecrets(note, secrets...), acct.ID)
	c.persistLogins()
	return sess.snapshot(), nil
}

// CancelLogin implements core.LoginProvider.  Nothing is stored until the flow
// succeeds, so cancelling has no credential to clean up; it only stops the
// session from being polled any further.
func (c *Client) CancelLogin(ctx context.Context, sessionID string) error {
	_ = ctx
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return errors.New("session id is required")
	}
	sess, ok := c.loginByID(sessionID)
	if !ok {
		return fmt.Errorf("login session %q not found", sessionID)
	}
	if !sess.terminal() {
		sess.set(core.LoginCancelled, "cancelled", "")
	}
	c.persistLogins()
	return nil
}

// accountIDForSecret reports the live account that already carries this
// credential, so a repeated panel login can be reported as success.
func (p *pool) accountIDForSecret(secret string) (string, bool) {
	if secret == "" {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureLocked()
	for _, a := range p.accounts {
		if a.secret() == secret {
			return a.ID, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// The flow's two requests
// ---------------------------------------------------------------------------

type oauthFlow struct {
	FlowID       string `json:"flow_id"`
	AuthorizeURL string `json:"authorize_url"`
}

// oauthInit starts the flow.  The request carries only the two headers the
// official CLI sends.
func (c *Client) oauthInit(ctx context.Context, pollToken string) (oauthFlow, error) {
	body := map[string]string{"provider": oauthProvider}
	if id := strings.TrimSpace(c.cfg.OAuthClientID); id != "" {
		body["client_id"] = id
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return oauthFlow{}, redactedErr("the sign-in request could not be built: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.oauthAPIBase()+oauthInitPath, bytes.NewReader(payload))
	if err != nil {
		return oauthFlow{}, redactedErr("the sign-in request could not be built: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+pollToken)
	req.Header.Set("Content-Type", "application/json")

	raw, status, err := c.doLogin(req)
	if err != nil {
		return oauthFlow{}, err
	}
	if status < 200 || status >= 300 {
		return oauthFlow{}, loginHTTPError("start the Z.AI sign-in", status, raw)
	}
	var envelope struct {
		Data oauthFlow `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return oauthFlow{}, redactedErr("the Z.AI sign-in response could not be read: %v", err)
	}
	flow := oauthFlow{
		FlowID:       strings.TrimSpace(envelope.Data.FlowID),
		AuthorizeURL: strings.TrimSpace(envelope.Data.AuthorizeURL),
	}
	if flow.FlowID == "" || flow.AuthorizeURL == "" {
		return oauthFlow{}, errors.New("the Z.AI sign-in response was incomplete (no flow id or authorisation URL)")
	}
	return flow, nil
}

// oauthPollData is the poll payload.  Field names come from the reference's
// mock upstream and its admin_api.py consumer.
type oauthPollData struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
	// Token is the ZCode gateway JWT, when the vendor returns one.
	Token string `json:"token"`
	Zai   struct {
		AccessToken  string `json:"access_token"`
		AccessToken2 string `json:"accessToken"`
	} `json:"zai"`
	AccessToken  string `json:"access_token"`
	AccessToken2 string `json:"accessToken"`
}

// accessToken is the Z.AI OAuth access token the exchange walk consumes.
func (d oauthPollData) accessToken() string {
	return firstNonEmpty(
		strings.TrimSpace(d.Zai.AccessToken),
		strings.TrimSpace(d.Zai.AccessToken2),
		strings.TrimSpace(d.AccessToken),
		strings.TrimSpace(d.AccessToken2),
	)
}

// oauthPoll performs the single poll.  The request carries only the bearer
// token: the vendor binds the session to a device context and rejects extra
// headers.
//
// A 4xx is terminal (the vendor will never accept this flow again) and an
// unreadable 2xx body is treated as terminal too, because a 200 that is not
// JSON is not a transient condition.  5xx and transport failures are reported
// as transient.
func (c *Client) oauthPoll(ctx context.Context, sess *webLogin) (oauthPollData, error) {
	rawURL := c.cfg.oauthAPIBase() + oauthPollPath + "/" + url.PathEscape(sess.FlowID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return oauthPollData{}, redactedErr("the poll request could not be built: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+sess.PollToken)

	raw, status, err := c.doLogin(req)
	if err != nil {
		return oauthPollData{}, err
	}
	if status >= 400 && status < 500 {
		if detail := upstreamMessage(raw); detail != "" {
			return oauthPollData{}, terminalErr("the vendor rejected the sign-in poll (HTTP %d): %s", status, detail)
		}
		return oauthPollData{}, terminalErr("the vendor rejected the sign-in poll (HTTP %d)", status)
	}
	if status < 200 || status >= 300 {
		return oauthPollData{}, redactedErr("the Z.AI sign-in endpoint is unavailable (HTTP %d)", status)
	}
	var envelope struct {
		Data oauthPollData `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return oauthPollData{}, terminalErr("the vendor returned an unreadable sign-in response")
	}
	return envelope.Data, nil
}

// doLogin runs one request and returns the body plus the status code.
func (c *Client) doLogin(req *http.Request) ([]byte, int, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return nil, 0, ctxErr
		}
		return nil, 0, redactedErr("the Z.AI endpoint could not be reached: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, loginBodyLimit))
	if err != nil {
		if ctxErr := req.Context().Err(); ctxErr != nil {
			return nil, resp.StatusCode, ctxErr
		}
		return nil, resp.StatusCode, redactedErr("the Z.AI response could not be read: %v", err)
	}
	return raw, resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// access token -> API key
// ---------------------------------------------------------------------------

// credentialFromAuth turns a "ready" poll payload into a stored credential and
// a panel-facing note.
//
// Deviation from the reference, on purpose: the reference prefers the ZCode
// gateway JWT and exchanges for an API key in the background.  Here the API
// key is preferred, because a JWT account needs captcha_command to be usable
// at all.  If the exchange fails we still keep the JWT, exactly like the
// reference, and say so in the note.
func (c *Client) credentialFromAuth(ctx context.Context, data oauthPollData) (managedAccount, string, error) {
	jwt := strings.TrimSpace(data.Token)
	access := data.accessToken()
	// The poll usually hands back the plan JWT beside the OAuth access token,
	// and the JWT is the only thing in this response that names the account.
	// Reading it now is what lets the panel group this credential with the
	// others the same Zhipu account already holds: an API key cannot be asked
	// who it belongs to once the response is gone.
	userID := jwtUserID(jwt)

	if access == "" {
		if jwt == "" {
			return managedAccount{}, "", errors.New(
				"the vendor reported the sign-in as complete but returned no credential")
		}
		acct, err := c.panelAccount(kindJWT, jwt, userID)
		if err != nil {
			return managedAccount{}, "", err
		}
		return acct, "signed in; the vendor returned a plan JWT only", nil
	}

	key, err := c.exchangeAPIKey(ctx, access)
	if err != nil {
		// An access token is a *sign-in* token, not an account credential: it
		// is short-lived and the vendor never issued it for API use.  Storing
		// it would persist something that silently expires and would mask the
		// real failure, so the walk stops here and says so.  (The branch above
		// is different: when the vendor returns no access token at all, the JWT
		// is the credential it actually issued for this plan.)
		return managedAccount{}, "", errors.New(scrubSecrets(err.Error(), access, jwt))
	}
	acct, acctErr := c.panelAccount(kindAPIKey, key, userID)
	if acctErr != nil {
		return managedAccount{}, "", acctErr
	}
	return acct, "signed in; the API key was stored as a new account", nil
}

// panelAccount builds the stored entry for a credential the panel obtained.
// userID is the account the credential belongs to, when the sign-in response
// named one; it is recorded because it cannot be recovered afterwards.
func (c *Client) panelAccount(kind, secret, userID string) (managedAccount, error) {
	limit := maxAPIKeyBytes
	if kind == kindJWT {
		limit = maxJWTBytes
	}
	if err := validateSecret(secret, limit); err != nil {
		return managedAccount{}, redactedErr("the vendor returned an unusable credential: %v", err)
	}
	now := nowRFC3339()
	enabled := true
	m := managedAccount{
		ID:        c.pool.freshManagedID(),
		Label:     labelPanelLogin,
		Kind:      kind,
		Region:    regionZai,
		UserID:    strings.TrimSpace(userID),
		Origin:    originPanelLogin,
		Enabled:   &enabled,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if kind == kindJWT {
		m.JWT = secret
	} else {
		m.APIKey = secret
	}
	return m, nil
}

// exchangeAPIKey walks access token -> business token -> organisation and
// project -> API key, and returns "<apiKey>.<secretKey>", the credential shape
// the module's api-key accounts already use.
func (c *Client) exchangeAPIKey(ctx context.Context, accessToken string) (string, error) {
	bizToken, err := c.exchangeLogin(ctx, accessToken)
	if err != nil {
		return "", err
	}
	// From here on the business token travels on every request, so any error
	// text the vendor echoes back is scrubbed against the tokens this walk
	// holds.  exchangeLogin's own failure needs no scrub: the access token was
	// the only secret in play and it never reached a response body we quote.
	scrub := func(err error) error {
		if err == nil {
			return nil
		}
		return errors.New(scrubSecrets(err.Error(), accessToken, bizToken))
	}
	orgID, projID, err := c.exchangeOrg(ctx, bizToken)
	if err != nil {
		return "", scrub(err)
	}
	keyURL := c.cfg.oauthExchangeBase() + fmt.Sprintf(exchangeKeysPath,
		url.PathEscape(orgID), url.PathEscape(projID))
	keyID, err := c.exchangeKey(ctx, bizToken, keyURL)
	if err != nil {
		return "", scrub(err)
	}
	secret, err := c.exchangeSecret(ctx, bizToken, keyURL, keyID)
	if err != nil {
		return "", scrub(err)
	}
	return keyID + "." + secret, nil
}

func (c *Client) exchangeLogin(ctx context.Context, accessToken string) (string, error) {
	payload, err := json.Marshal(map[string]string{"token": accessToken})
	if err != nil {
		return "", redactedErr("the exchange request could not be built: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.oauthExchangeBase()+exchangeLoginPath, bytes.NewReader(payload))
	if err != nil {
		return "", redactedErr("the exchange request could not be built: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	raw, status, err := c.doLogin(req)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", loginHTTPError("exchange the sign-in for a business token", status, raw)
	}
	var envelope struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			AccessToken2 string `json:"accessToken"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", redactedErr("the business-token response could not be read: %v", err)
	}
	token := firstNonEmpty(
		strings.TrimSpace(envelope.Data.AccessToken),
		strings.TrimSpace(envelope.Data.AccessToken2),
	)
	if token == "" {
		return "", errors.New("the vendor returned no business token")
	}
	return token, nil
}

// exchangeOrg picks the organisation and project to hold the API key.  The
// reference prefers the ones literally named "默认机构" / "默认项目" and falls
// back to the first entry; we additionally skip entries without an id, which
// the reference would turn into a request against an empty path segment.
func (c *Client) exchangeOrg(ctx context.Context, bizToken string) (string, string, error) {
	req, err := c.bizGet(ctx, c.cfg.oauthExchangeBase()+exchangeCustomerPath, bizToken)
	if err != nil {
		return "", "", err
	}
	raw, status, err := c.doLogin(req)
	if err != nil {
		return "", "", err
	}
	if status < 200 || status >= 300 {
		return "", "", loginHTTPError("read the organisation list", status, raw)
	}
	var envelope struct {
		Data struct {
			Organizations []struct {
				OrganizationID   string `json:"organizationId"`
				OrganizationName string `json:"organizationName"`
				Projects         []struct {
					ProjectID   string `json:"projectId"`
					ProjectName string `json:"projectName"`
				} `json:"projects"`
			} `json:"organizations"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", "", redactedErr("the organisation response could not be read: %v", err)
	}

	var orgID string
	var projects []struct {
		ProjectID   string `json:"projectId"`
		ProjectName string `json:"projectName"`
	}
	for _, org := range envelope.Data.Organizations {
		id := strings.TrimSpace(org.OrganizationID)
		if id == "" {
			continue
		}
		if orgID == "" {
			orgID, projects = id, org.Projects
		}
		if strings.Contains(org.OrganizationName, "默认机构") {
			orgID, projects = id, org.Projects
			break
		}
	}
	if orgID == "" {
		return "", "", errors.New("the account has no usable organisation")
	}

	var projID string
	for _, proj := range projects {
		id := strings.TrimSpace(proj.ProjectID)
		if id == "" {
			continue
		}
		if projID == "" {
			projID = id
		}
		if strings.Contains(proj.ProjectName, "默认项目") {
			projID = id
			break
		}
	}
	if projID == "" {
		return "", "", errors.New("the organisation has no usable project")
	}
	return orgID, projID, nil
}

// exchangeKey reuses the "zcode-api-key" entry or creates it.
func (c *Client) exchangeKey(ctx context.Context, bizToken, keyURL string) (string, error) {
	req, err := c.bizGet(ctx, keyURL, bizToken)
	if err != nil {
		return "", err
	}
	raw, status, err := c.doLogin(req)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", loginHTTPError("list the API keys", status, raw)
	}
	if keyID := findAPIKey(raw); keyID != "" {
		return keyID, nil
	}

	payload, err := json.Marshal(map[string]string{"name": exchangeKeyName})
	if err != nil {
		return "", redactedErr("the API-key request could not be built: %v", err)
	}
	create, err := http.NewRequestWithContext(ctx, http.MethodPost, keyURL, bytes.NewReader(payload))
	if err != nil {
		return "", redactedErr("the API-key request could not be built: %v", err)
	}
	create.Header.Set("Authorization", "Bearer "+bizToken)
	create.Header.Set("Content-Type", "application/json")
	raw, status, err = c.doLogin(create)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", loginHTTPError("create an API key", status, raw)
	}
	if keyID := findAPIKey(raw); keyID != "" {
		return keyID, nil
	}
	return "", errors.New("the vendor did not return an API key")
}

// findAPIKey reads "data": {...,"apiKey":...} or "data": [{...},...].
func findAPIKey(raw []byte) string {
	var list struct {
		Data []struct {
			Name   string `json:"name"`
			APIKey string `json:"apiKey"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, k := range list.Data {
			if k.Name == exchangeKeyName {
				return strings.TrimSpace(k.APIKey)
			}
		}
	}
	var one struct {
		Data struct {
			APIKey string `json:"apiKey"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &one); err == nil {
		return strings.TrimSpace(one.Data.APIKey)
	}
	return ""
}

func (c *Client) exchangeSecret(ctx context.Context, bizToken, keyURL, keyID string) (string, error) {
	req, err := c.bizGet(ctx, keyURL+"/copy/"+url.PathEscape(keyID), bizToken)
	if err != nil {
		return "", err
	}
	raw, status, err := c.doLogin(req)
	if err != nil {
		return "", err
	}
	if status < 200 || status >= 300 {
		return "", loginHTTPError("read the API key secret", status, raw)
	}
	var envelope struct {
		Data struct {
			SecretKey string `json:"secretKey"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", redactedErr("the API-key response could not be read: %v", err)
	}
	secret := strings.TrimSpace(envelope.Data.SecretKey)
	if secret == "" {
		return "", errors.New("the vendor did not return the API key secret")
	}
	return secret, nil
}

func (c *Client) bizGet(ctx context.Context, rawURL, bizToken string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, redactedErr("the exchange request could not be built: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bizToken)
	return req, nil
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// terminalLoginError marks a poll failure the flow cannot recover from.
type terminalLoginError struct{ msg string }

func (e *terminalLoginError) Error() string { return e.msg }

func terminalErr(format string, args ...any) error {
	return &terminalLoginError{msg: core.Redact(fmt.Sprintf(format, args...))}
}

// redactedErr builds an error whose text has been through core.Redact, so no
// code path can leak a token to the panel.
func redactedErr(format string, args ...any) error {
	return errors.New(core.Redact(fmt.Sprintf(format, args...)))
}

// scrubSecrets removes credential material the flow is currently holding from a
// message that is about to be shown to the operator.
//
// core.Redact recognises credential *shapes* (a bearer header, a "token": "..."
// pair, a JWT), but the vendor is free to quote one of our own tokens inside an
// ordinary prose error -- "the token <hex> was rejected" matches no shape at
// all.  Passing the live secrets in explicitly closes that gap, and it is the
// only defence that does, so every message that embeds an upstream body goes
// through here.
func scrubSecrets(text string, secrets ...string) string {
	text = core.Redact(text)
	for _, secret := range secrets {
		// Short strings are not credentials and replacing them would mangle
		// ordinary prose.
		if len(secret) < 8 {
			continue
		}
		text = strings.ReplaceAll(text, secret, "<redacted>")
	}
	return text
}

// loginHTTPError renders a non-2xx vendor response without echoing anything
// secret from the body.
func loginHTTPError(action string, status int, raw []byte) error {
	if detail := upstreamMessage(raw); detail != "" {
		return redactedErr("the vendor refused to %s (HTTP %d): %s", action, status, detail)
	}
	return redactedErr("the vendor refused to %s (HTTP %d)", action, status)
}

// upstreamMessage extracts a short message from an error envelope
// ({"error":{"message":...}}, {"message":...} or {"data":{"message":...}}).
func upstreamMessage(raw []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
		Data    struct {
			Message string `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return ""
	}
	return shortNote(firstNonEmpty(envelope.Error.Message, envelope.Message, envelope.Data.Message))
}

// shortNote trims a vendor message to something a panel can display.
func shortNote(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) > loginNoteLimit {
		return string(runes[:loginNoteLimit]) + "…"
	}
	return s
}

// newPollToken mirrors secrets.token_hex(32).
func newPollToken() (string, error) {
	buf := make([]byte, pollTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", redactedErr("no randomness available for the sign-in token: %v", err)
	}
	return hex.EncodeToString(buf), nil
}

// userCodeFromURL picks up a short human-facing code when the vendor put one
// in the authorisation URL.  The reference flow does not document one, so an
// empty result is normal and the panel then shows only the URL.
func userCodeFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	q := u.Query()
	for _, key := range []string{"user_code", "usercode", "code"} {
		if v := strings.TrimSpace(q.Get(key)); v != "" {
			return v
		}
	}
	return ""
}
