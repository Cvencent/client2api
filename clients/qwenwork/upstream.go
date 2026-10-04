package qwenwork

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"client2api/internal/core"
)

// upstream.go is the transport layer: it builds signed requests against
// gateway.qwenwork.cn, turns responses into either a stream or a classified
// error, and runs the account retry loop.
//
// Error classification is the heart of it.  Everything the caller needs to
// decide -- retry here, retry elsewhere, cool this account for 15 s or for a
// day, disable it outright -- comes out of errKind.

// Vendor endpoints.  Only the path is ours to choose; the host comes from
// config.baseURL().
const (
	chatPath     = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common"
	modelsPath   = "/algo/api/v2/model/list"
	refreshPath  = "/api/v1/deviceToken/refresh"
	pollPath     = "/api/v1/deviceToken/poll"
	userInfoPath = "/api/v1/userinfo"
)

// errKind is the classification of one upstream failure.
type errKind int

const (
	kindNone      errKind = iota // not a failure we act on
	kindNetwork                  // the request never produced a response
	kindAuth                     // 401 / 403: the credential is stale, not dead
	kindQuota                    // 402, or a credit marker on a 429: out of credit
	kindTransient                // 5xx, or a plain 429: try again shortly
	kindClient                   // any other 4xx: our request was wrong
)

func (k errKind) String() string {
	switch k {
	case kindNetwork:
		return "network"
	case kindAuth:
		return "auth"
	case kindQuota:
		return "quota"
	case kindTransient:
		return "transient"
	case kindClient:
		return "client"
	default:
		return "none"
	}
}

// maxErrorText bounds anything we copy out of an upstream body before it can
// reach a log line or a Status detail.
const maxErrorText = 200

var (
	// reCreditCode matches the vendor's quota-exhausted business code.  The
	// pattern is a compile-time constant, so MustCompile cannot panic here.
	reCreditCode = regexp.MustCompile(`\bcode["'\s:=]*(14018|14019|14020)\b`)
	reScriptTag  = regexp.MustCompile(`(?is)<script[\s\S]*?</script>`)
	reStyleTag   = regexp.MustCompile(`(?is)<style[\s\S]*?</style>`)
	reHTMLTag    = regexp.MustCompile(`<[^>]*>`)
	reSpace      = regexp.MustCompile(`\s+`)
	// reSecret scrubs anything that looks like a credential before a string is
	// logged.  This is the last line of defence before core.MaskSecret's
	// prefix-only rendering.
	reSecret = regexp.MustCompile(`(?i)(bearer[ ]+|"?(?:access_token|refresh_token|device_token|security_oauth_token|cosy-key|authorization|token)"?[ ]*[:=][ ]*"?)([A-Za-z0-9._~+/=-]{8,})`)
	// reJWT matches a bare JSON Web Token: three base64url segments joined by
	// dots.  An upstream body can echo one back without any "token=" label in
	// front of it, which reSecret would miss; every shape that can reach a log
	// line has to be scrubbed, not just the labelled ones.
	reJWT = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\b`)
	// reCOSY matches a COSY bearer credential, which is a dotted triple whose
	// first segment is the base64 of the compacted {"cosyVersion":...} header.
	reCOSY = regexp.MustCompile(`\bCOSY\.[A-Za-z0-9+/=_-]+\.[A-Za-z0-9+/=_-]+`)
)

// creditMarkers are the substrings the vendor uses to say "out of credit", in
// both languages it says them in.
var creditMarkers = []string{
	"credits exhausted", "insufficient credit", "no credit", "credit exhausted",
	"out of credit", "quota exceeded", "quota exhaust", "credit not enough",
	"not enough credit", "payment required",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// creditExhausted reports whether a body says the account is out of credit.
func creditExhausted(body string) bool {
	if reCreditCode.MatchString(body) {
		return true
	}
	lower := strings.ToLower(body)
	for _, m := range creditMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// classify maps an HTTP status (and its body, which is the only place the
// credit signal lives) onto an errKind.  The order matters: 402 is always
// quota, and 429 only means quota when the body says so.
func classify(status int, body string) errKind {
	switch {
	case status == http.StatusPaymentRequired:
		return kindQuota
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return kindAuth
	case status == http.StatusTooManyRequests:
		if creditExhausted(body) {
			return kindQuota
		}
		return kindTransient
	case status >= 500:
		return kindTransient
	case status >= 400:
		return kindClient
	default:
		return kindNone
	}
}

// failureKind projects this module's errKind onto the shared core.FailureKind
// contract, which is what internal/gateway rotates on.
//
// The two vocabularies are close but not identical.  kindTransient covers both
// a plain 429 ("slow down") and a 5xx ("try another account"); the gateway only
// needs the rotation decision, and both are retried, so it maps to
// FailureRateLimited.  kindClient maps to FailureOther, which core.Retryable
// reports false for: a request the vendor calls malformed is malformed on every
// account, and rotating on it only spends the rest of the pool.  kindNetwork
// and anything unclassified produced no vendor status, so they are
// FailureUpstream.
//
// This module has no WAF or risk-control code in its vocabulary: classify maps
// a 403 to kindAuth, i.e. a credential rejection.  If a 403 is ever an edge-WAF
// block rather than a credential rejection, it would be reported as FailureAuth
// and rotated -- see the note in README ("Known gaps").
func failureKind(k errKind) core.FailureKind {
	switch k {
	case kindQuota:
		return core.FailureQuota
	case kindTransient:
		return core.FailureRateLimited
	case kindAuth:
		return core.FailureAuth
	case kindClient:
		return core.FailureOther
	default:
		return core.FailureUpstream
	}
}

// cleanErrorText flattens an upstream body into one short, tag-free line and
// scrubs anything credential-shaped out of it.
func cleanErrorText(s string) string {
	if s == "" {
		return ""
	}
	s = reScriptTag.ReplaceAllString(s, " ")
	s = reStyleTag.ReplaceAllString(s, " ")
	s = reHTMLTag.ReplaceAllString(s, " ")
	s = reSpace.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	s = reSecret.ReplaceAllStringFunc(s, func(match string) string {
		loc := reSecret.FindStringSubmatchIndex(match)
		if len(loc) < 6 {
			return "***"
		}
		return match[:loc[2]] + core.MaskSecret(match[loc[4]:loc[5]])
	})
	// A credential with no label in front of it still has to go: an upstream
	// body that echoes a token back is exactly the case that leaks.
	s = reJWT.ReplaceAllStringFunc(s, core.MaskSecret)
	s = reCOSY.ReplaceAllStringFunc(s, core.MaskSecret)
	if len(s) > maxErrorText {
		s = s[:maxErrorText]
	}
	return s
}

// upstreamError is a failure that carries its own classification.
type upstreamError struct {
	Status     int
	Kind       errKind
	Message    string
	RetryAfter time.Duration
}

func (e *upstreamError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Status > 0 {
		return "upstream " + strconv.Itoa(e.Status)
	}
	return "upstream " + e.Kind.String() + " failure"
}

// asUpstreamError pulls an *upstreamError out of an error chain, if there is one.
func asUpstreamError(err error) (*upstreamError, bool) {
	var ue *upstreamError
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}

// newUpstreamError builds the classified error for a non-2xx response.
func newUpstreamError(status int, body string, header http.Header) *upstreamError {
	msg := "upstream " + strconv.Itoa(status)
	if text := cleanErrorText(body); text != "" {
		msg += ": " + text
	}
	return &upstreamError{
		Status:     status,
		Kind:       classify(status, body),
		Message:    msg,
		RetryAfter: parseRetryAfter(header),
	}
}

// parseRetryAfter honours Retry-After and X-Ratelimit-Reset, sanity-capped so a
// hostile or broken header cannot park an account for a week.
func parseRetryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	const cap = 2 * time.Hour
	for _, key := range []string{"Retry-After", "X-Ratelimit-Reset", "Retry-After-Ms"} {
		raw := strings.TrimSpace(h.Get(key))
		if raw == "" {
			continue
		}
		if key == "Retry-After-Ms" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 {
				return capDuration(time.Duration(n)*time.Millisecond, cap)
			}
			continue
		}
		if n, err := strconv.Atoi(raw); err == nil {
			if n > 0 {
				return capDuration(time.Duration(n)*time.Second, cap)
			}
			continue
		}
		if t, err := http.ParseTime(raw); err == nil {
			if d := time.Until(t); d > 0 {
				return capDuration(d, cap)
			}
		}
	}
	return 0
}

func capDuration(d, max time.Duration) time.Duration {
	if d > max {
		return max
	}
	return d
}

// ---------------------------------------------------------------------------
// request plumbing
// ---------------------------------------------------------------------------

// errIdleTimeout is returned when the upstream accepts the request and then
// goes quiet for longer than the idle timeout.
var errIdleTimeout = errors.New("qwenwork: upstream stopped sending data (idle timeout)")

// do performs a signed request and returns the response with its body wrapped
// in an idle watchdog.  The caller owns the body.
func (c *Client) do(ctx context.Context, method, rawURL, body string, hdrs map[string]string) (*http.Response, error) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if idle := c.cfg.idleTimeout(); idle > 0 {
		resp.Body = newWatchdogBody(ctx, resp.Body, idle)
	}
	return resp, nil
}

// doJSON performs a request and decodes a JSON response, returning a classified
// error for a non-2xx status.
func (c *Client) doJSON(ctx context.Context, method, rawURL, body string, hdrs map[string]string, out any) error {
	resp, err := c.do(ctx, method, rawURL, body, hdrs)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newUpstreamError(resp.StatusCode, string(raw), resp.Header)
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// httpClient returns the client every vendor-facing request goes through.
//
// A configured TLS profile wins: it is a client whose handshake imitates a
// named browser, and it carries its own transport, so falling back to the
// shared one would undo the point of naming a profile.  Otherwise the shared
// client from Deps is used, or a private one with a sane default transport
// when the core did not supply one.  It is never nil.
func (c *Client) httpClient() *http.Client {
	if c.vendorClient != nil {
		return c.vendorClient
	}
	if c.deps.HTTPClient != nil {
		return c.deps.HTTPClient
	}
	c.httpOnce.Do(func() {
		if c.deps.HTTPClient == nil {
			c.ownClient = &http.Client{}
		}
	})
	return c.ownClient
}

// ---------------------------------------------------------------------------
// the chat call
// ---------------------------------------------------------------------------

// chatStream issues one signed chat request.  A non-2xx response is drained,
// classified and returned as an *upstreamError; on success the caller owns the
// response body.
func (c *Client) chatStream(ctx context.Context, acct account, sess cosySession, body, modelKey string) (*http.Response, error) {
	rawURL := c.cfg.endpoint(chatPath)
	now := time.Now()
	hdrs, err := sess.headers(cosyRequest{
		UID:        acct.UID,
		Body:       body,
		RawURL:     rawURL,
		ModelKey:   modelKey,
		Accept:     "text/event-stream",
		UserAgent:  c.cfg.userAgent(),
		RequestID:  newUUID(),
		XRequestID: newUUID(),
		UnixDate:   now.Unix(),
	})
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodPost, rawURL, body, hdrs)
	if err != nil {
		return nil, &upstreamError{Kind: kindNetwork, Message: "upstream_error: " + cleanErrorText(err.Error())}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	return nil, newUpstreamError(resp.StatusCode, string(raw), resp.Header)
}

// ---------------------------------------------------------------------------
// models, refresh, device grants
// ---------------------------------------------------------------------------

// modelList fetches the live model catalogue with one account's signature.
func (c *Client) modelList(ctx context.Context, acct account, sess cosySession) ([]core.Model, error) {
	rawURL := c.cfg.endpoint(modelsPath)
	hdrs, err := sess.headers(cosyRequest{
		UID:        acct.UID,
		RawURL:     rawURL,
		Accept:     "application/json",
		UserAgent:  c.cfg.userAgent(),
		RequestID:  newUUID(),
		XRequestID: newUUID(),
		UnixDate:   time.Now().Unix(),
	})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Qwork []struct {
			Key             string `json:"key"`
			DisplayName     string `json:"display_name"`
			Enable          *bool  `json:"enable"`
			IsReasoning     bool   `json:"is_reasoning"`
			IsVL            bool   `json:"is_vl"`
			MaxInputTokens  int    `json:"max_input_tokens"`
			MaxOutputTokens int    `json:"max_output_tokens"`
			Credits         string `json:"credits"`
			Description     string `json:"description"`
		} `json:"qwork"`
	}
	if err := c.doJSON(ctx, http.MethodGet, rawURL, "", hdrs, &payload); err != nil {
		return nil, err
	}
	out := make([]core.Model, 0, len(payload.Qwork))
	for _, m := range payload.Qwork {
		if m.Key == "" || (m.Enable != nil && !*m.Enable) {
			continue
		}
		name := m.DisplayName
		if name == "" {
			name = m.Key
		}
		extra := map[string]any{
			"display_name": name,
			"reasoning":    m.IsReasoning,
			"vision":       m.IsVL,
		}
		if m.MaxInputTokens > 0 {
			extra["max_input_tokens"] = m.MaxInputTokens
			extra["context_length"] = m.MaxInputTokens
		}
		if m.MaxOutputTokens > 0 {
			extra["max_output_tokens"] = m.MaxOutputTokens
		}
		if m.Credits != "" {
			extra["credits"] = m.Credits
		}
		if m.Description != "" {
			extra["description"] = m.Description
		}
		out = append(out, core.Model{ID: m.Key, OwnedBy: "qwenwork", Extra: extra})
	}
	return out, nil
}

// deviceGrant is what the poll and refresh endpoints return.
type deviceGrant struct {
	Token        string `json:"token"`
	DeviceToken  string `json:"device_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    string `json:"expires_at"`
	UserID       string `json:"user_id"`
	UserName     string `json:"user_name"`
}

// accessToken returns whichever field carries the access token.
func (g deviceGrant) accessToken() string {
	if g.Token != "" {
		return g.Token
	}
	return g.DeviceToken
}

// expiryUnix mirrors the reference: prefer expires_at, then expires_in (which
// may be seconds or milliseconds), then assume 30 days.
func (g deviceGrant) expiryUnix(now time.Time) int64 {
	if t := parseExpiry(g.ExpiresAt); t > 0 {
		return t
	}
	if g.ExpiresIn > 0 {
		ms := g.ExpiresIn
		if ms < 10_000_000 {
			ms *= 1000
		}
		return now.Add(time.Duration(ms) * time.Millisecond).Unix()
	}
	return now.Add(30 * 24 * time.Hour).Unix()
}

// refresh exchanges a refresh token for a fresh access token.
func (c *Client) refresh(ctx context.Context, refreshToken string) (deviceGrant, error) {
	var grant deviceGrant
	if refreshToken == "" {
		return grant, errors.New("qwenwork: no refresh token")
	}
	payload, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return deviceGrant{}, err
	}
	rawURL := c.cfg.endpoint(refreshPath)
	hdrs := map[string]string{
		"accept":       "application/json, text/plain, */*",
		"content-type": "application/json",
		"user-agent":   c.cfg.userAgent(),
	}
	if err := c.doJSON(ctx, http.MethodPost, rawURL, string(payload), hdrs, &grant); err != nil {
		return deviceGrant{}, err
	}
	if grant.accessToken() == "" {
		return deviceGrant{}, errors.New("qwenwork: refresh returned no access token")
	}
	return grant, nil
}

// pollGrant asks whether the user has finished authorising the device flow.
// A 404 or 202 means "not yet", which is not an error.
func (c *Client) pollGrant(ctx context.Context, nonce, verifier string) (deviceGrant, bool, error) {
	q := url.Values{
		"nonce":            {nonce},
		"verifier":         {verifier},
		"challenge_method": {"S256"},
	}.Encode()
	rawURL := c.cfg.endpoint(pollPath) + "?" + q
	hdrs := map[string]string{
		"accept":     "application/json",
		"user-agent": c.cfg.userAgent(),
	}
	resp, err := c.do(ctx, http.MethodGet, rawURL, "", hdrs)
	if err != nil {
		return deviceGrant{}, false, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return deviceGrant{}, false, err
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusAccepted {
		return deviceGrant{}, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return deviceGrant{}, false, newUpstreamError(resp.StatusCode, string(raw), resp.Header)
	}
	var grant deviceGrant
	if err := json.Unmarshal(raw, &grant); err != nil {
		return deviceGrant{}, false, err
	}
	if grant.accessToken() == "" {
		return deviceGrant{}, true, nil
	}
	return grant, false, nil
}

// ---------------------------------------------------------------------------
// the account retry loop
// ---------------------------------------------------------------------------

// openChat picks accounts, signs, and retries until one of them produces a
// stream.  The policy is the reference's:
//
//   - at most cfg.maxAttempts() accounts, least-recently-used first;
//   - a 401/403 gets exactly one refresh-and-retry per account;
//   - a failed refresh disables the account (the refresh token is dead too);
//   - a non-retryable classification stops the loop immediately.
//
// conversationKey is the stickiness key derived by Chat (see affinity.go).  It
// is passed through to pickAccount, which resolves a bound account first and
// binds the account it falls back to; an empty key means the request is not
// conversation-scoped and the pick degenerates exactly into the LRU pick.
//
// release is the cancel func of the per-request timeout context; it is handed
// to the returned stream so the stream can release it when it ends.
func (c *Client) openChat(ctx context.Context, release context.CancelFunc, req *core.ChatRequest, modelKey, conversationKey string) (core.Stream, error) {
	if c.pool.len() == 0 {
		return nil, core.ErrNotConfigured
	}

	body, err := buildBody(req, modelKey, c.cfg.maxTokens())
	if err != nil {
		return nil, err
	}
	bodyStr := string(body)

	// Best-effort pre-refresh: an account close to expiry is renewed before it
	// is used, so a request never starts on a token that is about to die.
	for _, e := range c.pool.usable() {
		if e.acct.needsRefresh(time.Now(), c.cfg.refreshMargin()) {
			_ = c.tryRefresh(ctx, e)
		}
	}

	skip := map[string]bool{}
	refreshed := map[string]bool{}
	attempts := c.cfg.maxAttempts()
	var lastErr error
	// lastAcct is the account that produced lastErr, so the terminal failure
	// can be attributed to it rather than to the client as a whole.
	var lastAcct account
	// issued counts the attempts actually sent upstream.  It is what the
	// backoff is derived from, and being non-zero is what proves another
	// account is worth waiting for.
	var issued int
	// busy records that the pool itself is fine but every account we reached
	// was at its per-account in-flight ceiling.
	busy := false

	for i := 0; i < attempts; i++ {
		e := c.pickAccount(skip, conversationKey, modelKey)
		if e == nil {
			break
		}
		// Take the account's in-flight slot before any upstream call.  A full
		// account is skipped, not blamed, so the next iteration uses another.
		if err := req.AcquireAccountSlot(e.acct.id()); err != nil {
			skip[e.acct.id()] = true
			busy = true
			continue
		}
		// Name the credential for the gateway's usage ledger and console row.
		// A retry overwrites this, so the value read once Chat returns is the
		// account of the attempt that actually got through.
		core.NoteServedBy(req, e.acct.id())
		// Pace between two upstream attempts.  The pick above proved that
		// another account is available, so this never sleeps after the last
		// attempt.  Without it a burst walks the whole pool back to back, and
		// since a quota exhaustion parks an account for quotaCooldown (24 h),
		// one bad minute can put every account into a day-long hole.
		if issued > 0 && !c.pace(ctx, issued-1) {
			// The caller's context died while we waited: stop at once rather
			// than holding a dead request for the rest of the backoff.
			return nil, classifyTerminal(lastErr, lastAcct)
		}

		sess, err := c.sessionFor(e.acct)
		if err != nil {
			c.pool.markFailure(e, kindClient, err.Error())
			skip[e.acct.id()] = true
			lastErr, lastAcct = err, e.acct
			issued++
			continue
		}

		issued++
		resp, err := c.chatStream(ctx, e.acct, sess, bodyStr, modelKey)
		if err == nil {
			c.pool.markUsed(e)
			c.noteUpstream(true, kindNone, "")
			return newQwenStream(ctx, release, resp.Body), nil
		}

		ue, ok := asUpstreamError(err)
		if !ok {
			// A context error or a local failure: do not blame the account.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, err
		}
		c.noteUpstream(false, ue.Kind, ue.Message)

		if ue.Kind == kindAuth && e.acct.RefreshToken != "" && !refreshed[e.acct.id()] {
			refreshed[e.acct.id()] = true
			if refreshErr := c.tryRefresh(ctx, e); refreshErr == nil {
				i-- // the retry does not consume an attempt slot
				continue
			} else {
				// The refresh token is dead too: park the account for good, and
				// report why the account died rather than the 401 that merely
				// revealed it.
				msg := cleanErrorText(refreshErr.Error())
				c.noteUpstream(false, kindAuth, msg)
				c.pool.markDead(e, msg)
				skip[e.acct.id()] = true
				lastErr, lastAcct = refreshErr, e.acct
				continue
			}
		}

		c.pool.markFailureWith(e, ue.Kind, ue.Message, ue.RetryAfter)
		skip[e.acct.id()] = true
		lastErr, lastAcct = ue, e.acct
		if !retryable(ue.Kind) {
			break
		}
	}

	if lastErr != nil {
		return nil, classifyTerminal(lastErr, lastAcct)
	}
	if busy {
		return nil, core.ErrBusy
	}
	return nil, core.ErrNotConfigured
}

// classifyTerminal wraps the retry loop's final error in the shared
// core.Failure contract, attributed to the account that produced it, so that
// internal/gateway can rotate on it and writeUpstreamError can turn it into a
// status and an error type.
//
// A local failure (a COSY session that could not be built, an unparseable
// vendor body) has no HTTP status and is treated as the client-side kind the
// pool already recorded for it -- core.FailureOther, which core.Retryable
// reports false for, so a malformed request does not rotate.
func classifyTerminal(err error, acct account) error {
	if err == nil {
		// Nothing failed: keep the module's existing "nothing to try" answer.
		return core.ErrNotConfigured
	}
	kind := kindClient
	status := 0
	if ue, ok := asUpstreamError(err); ok {
		kind = ue.Kind
		status = ue.Status
	}
	return core.Fail("qwenwork", acct.id(), failureKind(kind), status, err)
}

// pace waits out the rotation backoff before another upstream attempt.  failed
// is the 0-based index of the attempt that just failed.  A false return means
// the caller's context died while waiting, so the loop must stop.
//
// core.RotateBackoffBase is the base because this module has no backoff knob of
// its own, and sharing the gateway's base keeps the two rotations from
// compounding into a long stall.  BackoffFrom already applies core.JitterDur,
// so it must not be wrapped again: a second jitter widens the spread to about
// +/-56% of the base and makes this wait drift from the gateway's rotation.
func (c *Client) pace(ctx context.Context, failed int) bool {
	base := c.paceBaseOverride
	if base <= 0 {
		base = core.RotateBackoffBase
	}
	return core.SleepCtx(ctx, core.BackoffFrom(base, failed))
}

// tryRefresh renews one account's token.  A nil error means the account is
// usable again; otherwise the error explains what the token endpoint said.
func (c *Client) tryRefresh(ctx context.Context, e *entry) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()
	grant, err := c.refresh(ctx, e.acct.RefreshToken)
	if err != nil {
		c.deps.Log("qwenwork: refresh for %s failed: %v", e.acct.id(), err)
		return err
	}
	acct := e.acct
	acct.AccessToken = grant.accessToken()
	if grant.RefreshToken != "" {
		acct.RefreshToken = grant.RefreshToken
	}
	acct.ExpiresAt = grant.expiryUnix(time.Now())
	c.pool.put(acct)
	c.invalidateSession(acct)
	c.pool.revive(e)
	c.deps.Log("qwenwork: refreshed token for %s", e.acct.id())
	return nil
}

// noteUpstream records the last upstream verdict so Status can distinguish
// "upstream unreachable" from "no credential" without making a network call
// itself.
func (c *Client) noteUpstream(ok bool, kind errKind, msg string) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if ok {
		c.upstreamOK = true
		c.lastErr = ""
		c.lastErrKind = kindNone
		c.lastErrAt = time.Time{}
		return
	}
	c.upstreamOK = false
	c.lastErrKind = kind
	c.lastErr = cleanErrorText(msg)
	c.lastErrAt = time.Now()
}

// ---------------------------------------------------------------------------
// idle watchdog
// ---------------------------------------------------------------------------

// watchdogBody closes the upstream body when it stops producing data.  A
// silently dead TCP connection is the failure mode a streaming proxy cannot
// otherwise see: without this, Recv would block until the client gave up.
type watchdogBody struct {
	rc     io.ReadCloser
	cancel context.CancelFunc
	timer  *time.Timer
	idle   time.Duration
	fired  atomic.Bool
	once   atomic.Bool
	closed atomic.Bool
}

func newWatchdogBody(parent context.Context, rc io.ReadCloser, idle time.Duration) *watchdogBody {
	// The cancel is derived from the request context, so closing the body also
	// releases the connection rather than leaking it.
	_, cancel := context.WithCancel(parent)
	w := &watchdogBody{rc: rc, cancel: cancel, idle: idle}
	w.timer = time.AfterFunc(idle, func() {
		w.fired.Store(true)
		cancel()
		rc.Close()
	})
	return w
}

func (w *watchdogBody) Read(p []byte) (int, error) {
	if !w.closed.Load() {
		w.timer.Reset(w.idle)
	}
	n, err := w.rc.Read(p)
	w.timer.Stop()
	if err != nil && w.fired.Load() {
		return n, errIdleTimeout
	}
	return n, err
}

func (w *watchdogBody) Close() error {
	if w.closed.Swap(true) {
		return nil
	}
	w.timer.Stop()
	w.cancel()
	return w.rc.Close()
}
