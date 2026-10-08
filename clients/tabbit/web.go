package tabbit

// This file owns the DIRECT web transport.
//
// Until now this module could only drive the local tabbit2api sidecar, which is
// a browser bridge: the vendor session lived inside the Tabbit browser and this
// process never saw it.  The vendor's web app is also reachable over plain HTTP
// with nothing but the browser's own `token` cookie, which is what the Tabbit
// browser and the sidecar both use.  So this module can speak that protocol
// itself, and the operator can import the cookie out of the running browser
// (webimport.go) instead of installing a second program.
//
// The shapes below were read off a live session, not copied from the upstream
// project (which is GPL-3.0-only and never inspected):
//
//	POST /panel/session                                   -> {"chat_session_id": "..."}
//	GET  /proxy/v1/model_config/models?a=0&scene=chat      -> {"models": [...], "status": "success"}
//	POST /api/v3/chat/rooms/<room>/runs                    -> {"run_id": "...", "status": "QUEUED"}
//	POST /api/v3/chat/rooms/<room>/join                    -> text/event-stream
//	GET  /api/commerce/quota/v1/usage?user_id=<uid>        -> {"member_level": ..., "usage_percentage": ...}
//
// Two properties of that protocol drive the design here:
//
//   - Every request needs `cookie: token=<JWT>` and nothing else.  The
//     `x-req-ctx` header carries the official client version and is sent for
//     shape, not because the endpoints demand it.
//   - A room is single-use.  The stream ends with `room_reset_required`
//     (reason "terminal_snapshot_available"), so one chat request means one
//     fresh room, and multi-turn context has to be flattened into the single
//     `input_payload.content` field the submit endpoint accepts.

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
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

const (
	// defaultWebBase is the host the vendor's web app serves from.  It is
	// configurable because the vendor has moved it before ("Web-host drift" in
	// the README) and because the test suite points it at a local stand-in.
	defaultWebBase = "https://web.tabbit.com"

	// webClientVersion/webClientBuild are the official client's version, which
	// the app reports in the x-req-ctx header as "1.15.17(10115017)".  It is
	// the only version this module has ever seen, so it is what it sends.
	webClientVersion = "1.15.17"
	webClientBuild   = "10115017"

	// webUserAgent is the product token this module presents.  The endpoints do
	// not check it (the protocol was proven with a bare curl), so it carries no
	// browser chrome we would have to keep in sync.
	webUserAgent = "Tabbit/" + webClientVersion

	webSessionPath  = "/panel/session"
	webModelsPath   = "/proxy/v1/model_config/models"
	webRunsPathFmt  = "/api/v3/chat/rooms/%s/runs"
	webJoinPathFmt  = "/api/v3/chat/rooms/%s/join"
	webUsagePath    = "/api/commerce/quota/v1/usage"
	webTokenCookie  = "token"
	webSceneChat    = "chat"
	webSurface      = "session"
	webTaskName     = "chat"
	webCookieHost   = "web.tabbit.com"
	webAccountIDTag = "tabbit-web:"

	// webVerdictTTL is how long a successful verification is reused without
	// asking the vendor again.  The panel polls status every few seconds and
	// reads the same verdict twice per cycle (overview and /status), so this
	// has to outlast one poll before the result can be reused at all.
	webVerdictTTL = 15 * time.Second

	// webTrustWindow is how long a successful check still counts as ready when
	// the vendor cannot be reached at all.  Status() runs under a tight budget
	// because the panel polls it, so a slow catalogue read must not paint a
	// working cookie red: the account table already shows that same verdict,
	// and the badge may not contradict the row it labels.  Past this window an
	// unreachable vendor leaves the credential "unknown" instead of claiming
	// it works.  A refusal (401/403) is never softened this way; only a
	// transport failure is.
	webTrustWindow = 15 * time.Minute

	// webRateLimitCooldown keeps a session that just returned 429 out of the
	// picker long enough for the gateway's retries to reach another account.
	// Tabbit's 429 is often per-session or concurrency shaped, so this is a
	// short rest rather than a quota-disable verdict.
	webRateLimitCooldown = time.Minute

	// webRateLimitHold is how long the panel keeps calling a session that just
	// returned 429 "cooling".  It is deliberately longer than the picker's rest:
	// web.tabbit.com keeps answering the model catalogue while it refuses every
	// run with 429, so a successful catalogue probe must not repaint the row
	// green underneath a stream of failed candidates.  A completed chat stores a
	// fresh success verdict and clears this hold, so it never outlives the next
	// real answer.
	webRateLimitHold = 10 * time.Minute
)

// webRateLimitHoldMax caps the growing rest a repeated 429 earns.  A burst of
// runs against one session is what trips the vendor's limiter, and each extra
// run deepens it: the hold doubles on every consecutive 429 so the module backs
// off instead of walking back into the same wall.  A completed run stores a
// success verdict and clears the hold.
const webRateLimitHoldMax = 30 * time.Minute

// webReqCtx is the x-req-ctx header the official client sends: base64 of
// "<version>(<build>)".  Derived rather than hard-coded so the two constants
// above cannot drift apart.
var webReqCtx = base64.StdEncoding.EncodeToString([]byte(webClientVersion + "(" + webClientBuild + ")"))

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

// webAuth is one web credential: the browser's session cookie plus everything
// this module can learn from it locally.  The token is never logged, never
// returned to the panel and never written anywhere but the account store.
type webAuth struct {
	accountID string
	label     string
	token     string
	uid       string
	expires   time.Time
	base      string
	source    string // panel | config | env
}

// webJWT is the part of the browser cookie this module cares about.  A Tabbit
// token is a signed JWT (iss https://web.tab-browser.com, scope "tab"); the
// signature is the vendor's business, and this module never tries to check it.
type webJWT struct {
	Sub   string `json:"sub"`
	Iss   string `json:"iss"`
	Scope string `json:"scope"`
	Exp   int64  `json:"exp"`
}

// parseWebToken decodes a JWT payload without verifying it.  Only the claims
// this module reports (the uid and the expiry) are read.
func parseWebToken(token string) (webJWT, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return webJWT{}, errors.New("it is not a JWT (want three dot-separated parts)")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if padded, perr := base64.URLEncoding.DecodeString(parts[1]); perr == nil {
			raw = padded
		} else {
			return webJWT{}, fmt.Errorf("the JWT payload is not base64url: %w", err)
		}
	}
	var claims webJWT
	if err := json.Unmarshal(raw, &claims); err != nil {
		return webJWT{}, fmt.Errorf("the JWT payload is not JSON: %w", err)
	}
	return claims, nil
}

// webAccountID is the stable identity of a web credential.  The uid is the
// operator's own account id and is already visible in the token, so it makes a
// far better row id than a fingerprint; a token without one falls back to a
// truncated digest, never to the token itself.
func webAccountID(claims webJWT, token string) string {
	if uid := strings.TrimSpace(claims.Sub); uid != "" {
		return webAccountIDTag + uid
	}
	sum := sha256.Sum256([]byte(token))
	return webAccountIDTag + hex.EncodeToString(sum[:8])
}

func webTokenExpiry(claims webJWT) time.Time {
	if claims.Exp <= 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

// webTokenHint renders a token's identity for a log or an account note without
// ever echoing the credential.
func webTokenHint(claims webJWT) string {
	uid := strings.TrimSpace(claims.Sub)
	if uid == "" {
		uid = "uid unknown"
	}
	if exp := webTokenExpiry(claims); !exp.IsZero() {
		return fmt.Sprintf("%s, expires %s", uid, exp.UTC().Format(time.RFC3339))
	}
	return uid
}

// ---------------------------------------------------------------------------
// Where the web API is, and which credential to use
// ---------------------------------------------------------------------------

// webBase resolves the web root: config first (it is how a test or a mirror is
// aimed), then the host stored with an account, then the vendor default.
func (c *Client) webBase() string {
	if b := strings.TrimRight(strings.TrimSpace(c.cfg.WebBaseURL), "/"); b != "" {
		return b
	}
	if ep, ok := c.firstEnabledWeb(); ok {
		if b := strings.TrimRight(strings.TrimSpace(ep.BaseURL), "/"); b != "" {
			return b
		}
	}
	return defaultWebBase
}

// staticWebToken is the credential from the config or the environment, used
// when no account was added through the panel.
func (c *Client) staticWebToken() (string, string) {
	switch {
	case strings.TrimSpace(c.cfg.WebToken) != "":
		return strings.TrimSpace(c.cfg.WebToken), "config"
	case c.env("CLIENT2API_TABBIT_WEB_TOKEN") != "":
		return c.env("CLIENT2API_TABBIT_WEB_TOKEN"), "env"
	case c.env("TABBIT_WEB_TOKEN") != "":
		return c.env("TABBIT_WEB_TOKEN"), "env"
	}
	return "", ""
}

// webAccounts lists every web-token account in the panel store.
func (c *Client) webAccounts() []storedEndpoint {
	var out []storedEndpoint
	for _, ep := range c.endpointsSnapshot() {
		if epKind(ep) == kindWebToken {
			out = append(out, ep)
		}
	}
	return out
}

// firstEnabledWeb returns the web account this module should use: the first
// enabled one, in store order.
func (c *Client) firstEnabledWeb() (storedEndpoint, bool) {
	eps := c.endpointsSnapshot()
	best, found := 0, false
	for _, ep := range eps {
		if epKind(ep) != kindWebToken || !ep.Enabled || strings.TrimSpace(ep.Token) == "" {
			continue
		}
		prio := core.AccountPriority("tabbit", ep.ID)
		if !found || prio < best {
			best, found = prio, true
		}
	}
	if !found {
		return storedEndpoint{}, false
	}
	for _, ep := range eps {
		if epKind(ep) == kindWebToken && ep.Enabled && strings.TrimSpace(ep.Token) != "" {
			if core.AccountPriority("tabbit", ep.ID) != best {
				continue
			}
			return ep, true
		}
	}
	return storedEndpoint{}, false
}

// firstAvailableWeb returns the first session that can serve right now.
// Unlike firstEnabledWeb it skips a session that just returned a retryable
// rate-limit, so a gateway retry rotates to the next account instead of
// walking back into the same 429.
func (c *Client) firstAvailableWeb() (storedEndpoint, bool) {
	eps := c.endpointsSnapshot()
	best, found := 0, false
	for _, ep := range eps {
		if epKind(ep) != kindWebToken || !c.endpointUsable(ep, true) {
			continue
		}
		prio := core.AccountPriority("tabbit", ep.ID)
		if !found || prio < best {
			best, found = prio, true
		}
	}
	if !found {
		return storedEndpoint{}, false
	}
	for _, ep := range eps {
		if epKind(ep) == kindWebToken && c.endpointUsable(ep, true) {
			if core.AccountPriority("tabbit", ep.ID) != best {
				continue
			}
			return ep, true
		}
	}
	return storedEndpoint{}, false
}

// webConfigured reports whether any web credential exists at all.
func (c *Client) webConfigured() bool {
	if _, ok := c.firstEnabledWeb(); ok {
		return true
	}
	token, _ := c.staticWebToken()
	return token != ""
}

// anyWebAccount reports whether the panel store holds at least one web
// session.  It is the "is this a panel-managed pool" test, so a pool that is
// entirely cooling can be told apart from a deployment that never added one.
func (c *Client) anyWebAccount() bool {
	for _, ep := range c.endpointsSnapshot() {
		if epKind(ep) == kindWebToken {
			return true
		}
	}
	return false
}

// anyAvailableWebAccount reports whether any stored session can serve right
// now under the module's own usability rules.
func (c *Client) anyAvailableWebAccount() bool {
	_, ok := c.firstAvailableWeb()
	return ok
}

// webRoute reports whether a call should go to the web API rather than the
// sidecar.
//
//	transport=web      always web
//	transport=sidecar  never web
//	transport=auto     web when a web credential exists AND it has not been
//	                   proven bad by an earlier call
//
// "Proven bad" means an authentication-shaped failure recorded by verifyWeb.
// A credential that has never been checked is trusted, so the first call
// reports the vendor's own error instead of silently falling back to a sidecar
// the operator may not even run.
func (c *Client) webRoute() bool {
	switch c.cfg.transport() {
	case transportWeb:
		return true
	case transportSidecar:
		return false
	}
	ep, ok := c.firstEnabledWeb()
	if !ok {
		token, _ := c.staticWebToken()
		return token != ""
	}
	if v, seen := c.webVerdictFor(ep.ID); seen && v.err != "" && !v.transient {
		return false
	}
	return true
}

// resolveWebAuth builds the credential for one call, preferring an account the
// operator added through the panel over a static config token.
func (c *Client) resolveWebAuth() (webAuth, error) {
	if ep, ok := c.firstEnabledWeb(); ok {
		return c.webAuthFrom(ep.Token, endpointLabel(ep), epOriginPanel, ep.BaseURL), nil
	}
	token, source := c.staticWebToken()
	if token == "" {
		return webAuth{}, fmt.Errorf(
			"%w: no Tabbit web credential is configured. Add one in the panel (kind \"web-token\"), "+
				"or press 导入凭据 to read it out of the running Tabbit browser, "+
				"or set clients.tabbit.web_token", core.ErrNotConfigured)
	}
	return c.webAuthFrom(token, "web token from "+source, source, ""), nil
}

func (c *Client) webAuthFrom(token, label, source, base string) webAuth {
	token = strings.TrimSpace(token)
	claims, _ := parseWebToken(token)
	wa := webAuth{
		accountID: webAccountID(claims, token),
		label:     label,
		token:     token,
		uid:       strings.TrimSpace(claims.Sub),
		expires:   webTokenExpiry(claims),
		source:    source,
		base:      strings.TrimRight(strings.TrimSpace(base), "/"),
	}
	if wa.base == "" {
		wa.base = strings.TrimRight(strings.TrimSpace(c.cfg.WebBaseURL), "/")
	}
	if wa.base == "" {
		wa.base = defaultWebBase
	}
	return wa
}

// ---------------------------------------------------------------------------
// Verification cache
// ---------------------------------------------------------------------------

// webVerdict is the last thing this module learned about one credential.
type webVerdict struct {
	at        time.Time
	err       string // "" means the credential answered
	models    int
	transient bool // a transport failure, not a verdict on the credential
	kind      core.FailureKind
	status    int

	// hold is the rest this refusal earned, when it is longer than the flat
	// cooldown.  It carries a repeated 429's exponential backoff from the
	// failure observer into the picker and the panel.
	hold time.Duration
}

// webCooling reports a session that is resting after a retryable rate-limit.
// The verdict is still shown in the panel; it only affects the request picker.
func (c *Client) webCooling(id string, now time.Time) bool {
	v, ok := c.webVerdictFor(id)
	if !ok || v.kind != core.FailureRateLimited || v.at.IsZero() {
		return false
	}
	hold := v.hold
	if hold <= 0 {
		hold = webRateLimitCooldown
	}
	return now.Sub(v.at) < hold
}

// webRateLimited reports a session the vendor refused with 429 recently enough
// that the panel should still call it cooling.  It is the longer-lived sibling
// of webCooling: the picker only needs to rest the session between the
// gateway's retries, while Status() and the account table have to keep telling
// the operator why every candidate is failing.
func (c *Client) webRateLimited(id string, now time.Time) (webVerdict, bool) {
	v, ok := c.webVerdictFor(id)
	if !ok || v.kind != core.FailureRateLimited || v.at.IsZero() {
		return webVerdict{}, false
	}
	if now.Sub(v.at) >= webRateLimitHold {
		hold := v.hold
		if hold <= 0 {
			hold = webRateLimitCooldown
		}
		// The display hold is the floor: a session the vendor just refused
		// stays orange at least long enough for the operator to see why, even
		// before the exponential backoff has grown past the flat cooldown.
		if hold < webRateLimitHold {
			hold = webRateLimitHold
		}
		if now.Sub(v.at) >= hold {
			return webVerdict{}, false
		}
	}
	hold := v.hold
	if hold <= 0 {
		hold = webRateLimitCooldown
	}
	// The display hold is the floor: a session the vendor just refused stays
	// orange at least long enough for the operator to see why, even before the
	// exponential backoff has grown past the flat cooldown.
	if hold < webRateLimitHold {
		hold = webRateLimitHold
	}
	if now.Sub(v.at) >= hold {
		return webVerdict{}, false
	}
	return v, true
}

// webRateLimitNote explains a cooling session, in the operator's own terms, plus
// the vendor's own words so the reason is never a guess.
func webRateLimitNote(v webVerdict) string {
	note := "web.tabbit.com is rate limiting this session; the next run can be tried in a few minutes"
	if v.err != "" {
		note += ": " + v.err
	}
	return note
}

func (c *Client) webVerdictFor(id string) (webVerdict, bool) {
	c.webMu.Lock()
	defer c.webMu.Unlock()
	v, ok := c.webChecks[id]
	return v, ok
}

func (c *Client) storeWebVerdict(id string, v webVerdict) {
	if strings.TrimSpace(id) == "" {
		return
	}
	c.webMu.Lock()
	defer c.webMu.Unlock()
	if c.webChecks == nil {
		c.webChecks = make(map[string]webVerdict)
	}
	c.webChecks[id] = v
}

// webVerified recently reports a credential that answered.  It is what lets the
// panel's poll loop (and Status) reuse one result instead of re-asking.
func (c *Client) webVerified(id string) (webVerdict, bool) {
	v, ok := c.webVerdictFor(id)
	if !ok || v.err != "" {
		return v, false
	}
	if time.Since(v.at) > webVerdictTTL {
		return v, false
	}
	return v, true
}

// webVerdictTrusted applies the trust rule to a verdict that was already read.
// A success counts for webTrustWindow; a refusal counts until the credential is
// proved good again, because that verdict carries the vendor's own reason and
// no amount of network trouble can improve on it.
func webVerdictTrusted(v webVerdict, ok bool) bool {
	if !ok {
		return false
	}
	if v.err == "" && (v.at.IsZero() || time.Since(v.at) > webTrustWindow) {
		return false
	}
	return true
}

// webTrusted reports the last verdict this module is still willing to act on,
// and whether there is one.  Status() (the badge) and Accounts() (the table)
// must decide "ready" through this one rule, or the same cookie can render red
// in one place and green in the other.
func (c *Client) webTrusted(id string) (webVerdict, bool) {
	v, ok := c.webVerdictFor(id)
	return v, webVerdictTrusted(v, ok)
}

// ---------------------------------------------------------------------------
// HTTP
// ---------------------------------------------------------------------------

func webURL(base, path string, query url.Values) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return "", errors.New("no tabbit web base URL")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("invalid tabbit web base URL %q: %w", base, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid tabbit web base URL %q: need scheme and host", base)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(strings.TrimSpace(path), "/")
	if query != nil {
		u.RawQuery = query.Encode()
	} else {
		u.RawQuery = ""
	}
	u.Fragment = ""
	return u.String(), nil
}

// webDo performs one web API call.  body is nil for the endpoints the vendor
// accepts with no body at all (POST /panel/session is one of them).
func (c *Client) webDo(ctx context.Context, wa webAuth, method, path string, query url.Values, body []byte, accept string) (*http.Response, error) {
	return c.webDoExtra(ctx, wa, method, path, query, body, accept, nil)
}

// webDoExtra is webDo plus a set of extra headers.  The join call needs the
// vendor's anti-replay triple (x-nonce / x-signature / x-timestamp), which is
// per-call, so it cannot live in the config's ExtraHeaders.
func (c *Client) webDoExtra(ctx context.Context, wa webAuth, method, path string, query url.Values, body []byte, accept string, extra http.Header) (*http.Response, error) {
	target, err := webURL(wa.base, path, query)
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", webTokenCookie+"="+wa.token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-Req-Ctx", webReqCtx)
	req.Header.Set("User-Agent", webUserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		for _, s := range v {
			req.Header.Add(k, s)
		}
	}
	// The operator's own headers win, so a mirror or a debug proxy can be
	// aimed without patching this module.
	for k, v := range c.cfg.ExtraHeaders {
		req.Header.Set(k, v)
	}
	return c.httpClient().Do(req)
}

// webError turns a non-2xx web response into a message that carries the
// vendor's own words, so a failed call is never reported as "something went
// wrong".
func webErrorMessage(wa webAuth, what string, status int, raw []byte) error {
	msg := upstreamErrorMessage(raw)
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("the Tabbit web session was rejected (HTTP %d) for %s: %s. "+
			"The cookie has expired or was signed out; press 导入凭据 to read it again from the Tabbit browser",
			status, what, msg)
	case http.StatusTooManyRequests:
		return fmt.Errorf("Tabbit rate limited %s (HTTP 429): %s", what, msg)
	default:
		return fmt.Errorf("Tabbit answered HTTP %d for %s: %s", status, what, msg)
	}
}

func webError(wa webAuth, what string, status int, raw []byte) error {
	err := webErrorMessage(wa, what, status, raw)
	kind := core.FailureOther
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		kind = core.FailureAuth
	case http.StatusTooManyRequests:
		kind = core.FailureRateLimited
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		kind = core.FailureUpstream
	}
	return core.Fail("tabbit", wa.accountID, kind, status, err)
}

// ---------------------------------------------------------------------------
// Endpoints
// ---------------------------------------------------------------------------

// webCreateRoom opens a fresh chat session.  Every chat request needs its own
// room: the vendor resets a room once a run finishes.
func (c *Client) webCreateRoom(ctx context.Context, wa webAuth) (string, error) {
	resp, err := c.webDo(ctx, wa, http.MethodPost, webSessionPath, nil, nil, "application/json")
	if err != nil {
		return "", fmt.Errorf("opening a Tabbit chat session: %w", err)
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, 1<<20)
	if resp.StatusCode != http.StatusOK {
		return "", webError(wa, "POST "+webSessionPath, resp.StatusCode, raw)
	}
	var env struct {
		ChatSessionID string `json:"chat_session_id"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &env); err != nil {
		return "", fmt.Errorf("parsing the Tabbit session response: %w", err)
	}
	id := strings.TrimSpace(env.ChatSessionID)
	if id == "" {
		return "", errors.New("Tabbit opened a session but reported no chat_session_id")
	}
	return id, nil
}

// webModelEntry is one entry of the vendor's own catalogue.
type webModelEntry struct {
	DisplayName       string  `json:"display_name"`
	Icon              string  `json:"icon"`
	Description       string  `json:"description"`
	SupportsImages    bool    `json:"supports_images"`
	SupportsTools     bool    `json:"supports_tools"`
	SupportThinking   bool    `json:"support_thinking"`
	UseThinking       bool    `json:"use_thinking"`
	ModelAccessType   string  `json:"model_access_type"`
	SunsetSoon        bool    `json:"sunset_soon"`
	DisplayLabel      string  `json:"display_label"`
	DisplayMultiplier float64 `json:"display_multiplier"`
	SortOrder         int     `json:"sort_order"`
	CreatedAt         string  `json:"created_at"`
}

// webFetchModels reads the real catalogue from the vendor.  The identifier is
// display_name: the catalogue has no id field at all, and selected_model in a
// run body takes exactly this string.
func (c *Client) webFetchModels(ctx context.Context, wa webAuth) ([]core.Model, error) {
	mctx := ctx
	if d := c.cfg.Timeouts.modelsBudget(); d > 0 {
		var cancel context.CancelFunc
		mctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	q := url.Values{"a": {"0"}, "scene": {webSceneChat}}
	resp, err := c.webDo(mctx, wa, http.MethodGet, webModelsPath, q, nil, "application/json")
	if err != nil {
		return nil, fmt.Errorf("reading the Tabbit model list: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("reading the Tabbit model list: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, webError(wa, "GET "+webModelsPath, resp.StatusCode, raw)
	}
	return parseWebModels(raw, c.cfg.ModelPrefix)
}

// parseWebModels turns the vendor envelope into core models, keeping the
// vendor's own order and dropping anything without a display_name.
func parseWebModels(raw []byte, prefix string) ([]core.Model, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("empty model list")
	}
	var env struct {
		Models []webModelEntry `json:"models"`
		Status string          `json:"status"`
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return nil, fmt.Errorf("parsing the Tabbit model list: %w", err)
	}
	out := make([]core.Model, 0, len(env.Models))
	seen := make(map[string]bool, len(env.Models))
	for _, m := range env.Models {
		id := strings.TrimSpace(stripModelPrefix(m.DisplayName, prefix))
		key := strings.ToLower(id)
		if id == "" || seen[key] {
			continue
		}
		seen[key] = true
		extra := map[string]any{
			"tabbit_display_name": m.DisplayName,
			"scene":               webSceneChat,
		}
		if m.Description != "" {
			extra["description"] = m.Description
		}
		extra["supports_images"] = m.SupportsImages
		extra["supports_tools"] = m.SupportsTools
		extra["support_thinking"] = m.SupportThinking || m.UseThinking
		if m.ModelAccessType != "" {
			extra["model_access_type"] = m.ModelAccessType
		}
		if m.DisplayMultiplier != 0 {
			extra["display_multiplier"] = m.DisplayMultiplier
		}
		if m.DisplayLabel != "" {
			extra["display_label"] = m.DisplayLabel
		}
		extra["sort_order"] = m.SortOrder
		// The embedded table only fills gaps: it never overwrites a field the
		// vendor sent, and it never invents one for an id no source covers.
		if meta, ok := metaProvider.Lookup(id); ok {
			modelmeta.FillExtra(extra, meta, modelmeta.KeysCanonical)
		}
		out = append(out, core.Model{ID: id, OwnedBy: "tabbit", Extra: extra})
	}
	if len(out) == 0 {
		return nil, errNoUsableModels
	}
	return out, nil
}

// webUsage is the vendor's own quota report for the signed-in account.
type webUsage struct {
	MemberLevel         string     `json:"member_level"`
	UsagePercentage     flexNumber `json:"usage_percentage"`
	RemainingResetHours flexNumber `json:"remaining_reset_hours"`
	CurrentCycleStart   string     `json:"current_cycle_start"`
	CurrentCycleEnd     string     `json:"current_cycle_end"`
	IsSubscription      bool       `json:"is_subscription"`
}

// flexNumber accepts either a JSON number or a numeric string.  The live
// quota endpoint answers usage_percentage as "71.68%" and
// remaining_reset_hours as "28.95"; decoding those into a float64 field
// failed and left the whole balance read broken.
type flexNumber float64

func (n *flexNumber) UnmarshalJSON(raw []byte) error {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	s = strings.Trim(s, `"`)
	s = strings.TrimSpace(strings.TrimSuffix(s, "%"))
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("not a number: %q", s)
	}
	*n = flexNumber(v)
	return nil
}

// webFetchUsage asks the vendor how much of the account's quota is spent.  It
// needs the uid, which comes out of the token, not from the caller.
func (c *Client) webFetchUsage(ctx context.Context, wa webAuth) (webUsage, error) {
	var usage webUsage
	if strings.TrimSpace(wa.uid) == "" {
		return usage, errors.New("the token carries no uid, so the quota endpoint cannot be asked")
	}
	q := url.Values{"user_id": {wa.uid}}
	resp, err := c.webDo(ctx, wa, http.MethodGet, webUsagePath, q, nil, "application/json")
	if err != nil {
		return usage, fmt.Errorf("reading the Tabbit quota: %w", err)
	}
	defer resp.Body.Close()
	raw := readLimited(resp.Body, 1<<20)
	if resp.StatusCode != http.StatusOK {
		return usage, webError(wa, "GET "+webUsagePath, resp.StatusCode, raw)
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &usage); err != nil {
		return usage, fmt.Errorf("parsing the Tabbit quota response: %w", err)
	}
	return usage, nil
}

// usageNote renders a quota report for a test result.  It is deliberately not a
// core.Balance: the vendor reports a percentage of the quota, not a credit
// count, and turning one into the other would invent a unit the panel displays
// as "credits".
func usageNote(u webUsage) string {
	parts := make([]string, 0, 3)
	if u.MemberLevel != "" {
		parts = append(parts, "member "+u.MemberLevel)
	}
	parts = append(parts, fmt.Sprintf("%.1f%% of the quota used", float64(u.UsagePercentage)))
	if u.RemainingResetHours > 0 {
		parts = append(parts, fmt.Sprintf("resets in %.0fh", float64(u.RemainingResetHours)))
	}
	return strings.Join(parts, ", ")
}

// verifyWeb asks the vendor's catalogue endpoint whether a credential still
// works, records the verdict and returns the models it saw.  The catalogue is
// the cheapest honest check: it is read-only, it is the same endpoint Models()
// uses, and a rejected cookie fails it with the vendor's own message.
func (c *Client) verifyWeb(ctx context.Context, wa webAuth) ([]core.Model, error) {
	vctx := ctx
	if d := c.cfg.Timeouts.modelsBudget(); d > 0 {
		var cancel context.CancelFunc
		vctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	models, err := c.webFetchModels(vctx, wa)
	v := webVerdict{at: time.Now(), models: len(models)}
	if err != nil {
		v.err = core.Redact(truncate(err.Error(), 300))
		v.transient = !webAuthRejection(err)
	}
	c.storeWebVerdict(wa.accountID, v)
	return models, err
}

// webAuthRejection reports whether an error was the vendor refusing the
// credential, as opposed to the network being down.  Only a refusal makes the
// module stop trying the web transport in auto mode.
func webAuthRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "HTTP 403")
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// newUUID returns a random RFC 4122 version 4 UUID, the shape the web client
// uses for client_run_id, unique-uuid, page_instance_id and x-signature.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing must not stop a chat request; the ids only have
		// to be unique within this process.
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// randHex returns n random bytes rendered as hex, used for x-nonce.
func randHex(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

// deviceIDHash is the stable-per-(account, room) device fingerprint the join
// body carries.  The vendor does not validate it; it exists so the field is
// present and deterministic rather than random noise.
func deviceIDHash(uid, room string) string {
	sum := sha256.Sum256([]byte("client2api|" + uid + "|" + room))
	return hex.EncodeToString(sum[:])
}

// runID generates the "tab_"-prefixed ids the web client uses for runs.
func runID() string { return "tab_" + newUUID() }
