package raccoon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
)

// errSessionDead means the refresh token itself was rejected: the operator
// has to sign in again. It is never retried.
var errSessionDead = errors.New("raccoon: refresh token rejected, please sign in again")

// refreshCodeDead is the vendor's envelope code for a dead refresh token.
const refreshCodeDead = 200003

// codeIAMConfirmRequired is the vendor's "business session not confirmed"
// envelope code.  It arrives with HTTP 400 even though the bearer token is
// valid: the token has simply not been exchanged for a working business
// session yet.  Retrying it verbatim never helps, so it is confirmed and then
// retried once instead of being reported as an auth failure.
const codeIAMConfirmRequired = 200901

// maxResponseBytes bounds a non-streaming response body.
const maxResponseBytes = 8 << 20

// envelope is the vendor's universal response shape.
//
// Some refusals (notably tool-schema validation) come back in OpenAI's nested
// shape instead: `{"error":{"message":...,"type":...}}`. Both are read here, so
// the operator sees the vendor's real reason rather than a bare "Bad Request".
type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Details string          `json:"details"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// apiError is one failed upstream call.
//
// The web API and the IAM service report failures differently, so a failure
// carries both spellings: the web API uses a numeric `code` (100006 and
// friends) while IAM uses a symbolic one ("COMMON_INVALID_ARGUMENT"). Only one
// of the two is ever set.
type apiError struct {
	Status    int
	Code      int
	IAMCode   string
	Message   string
	RequestID string
	// RetryAfter is the vendor's own retry hint from the response headers,
	// already parsed and capped.  Zero when the answer said nothing (issue #9).
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = strings.TrimSpace(e.IAMCode)
	}
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if msg == "" {
		msg = "upstream error"
	}
	switch {
	case e.Code != 0:
		return fmt.Sprintf("raccoon: %s (code %d)", msg, e.Code)
	case e.IAMCode != "":
		return fmt.Sprintf("raccoon: %s (%s)", msg, e.IAMCode)
	case e.Status >= 400:
		return fmt.Sprintf("raccoon: HTTP %d: %s", e.Status, msg)
	default:
		return "raccoon: " + msg
	}
}

func statusOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

func codeOf(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return 0
}

// isAuthError reports a credential-level rejection (as opposed to a
// transient failure).
func isAuthError(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden || ae.Code == refreshCodeDead
}

// decodeEnvelope validates one response and returns its `data` member.
//
// `code == 0` is success. When `code` is absent the HTTP status is the
// authority, so a >= 400 response is an error even with no envelope.  header
// may be nil; when present, a 429's Retry-After hint is parsed onto the error
// so the cooldown honours the vendor's own window (issue #9).
//
// The IAM service answers with a different envelope (see iamEnvelope), so the
// symbolic shape is tried first and the numeric one second. Trying IAM first is
// safe because `code` there is a string: unmarshalling it into the numeric
// envelope fails, and the numeric path is the only one that can then succeed.
func decodeEnvelope(status int, raw []byte, header http.Header) (json.RawMessage, error) {
	ra := parseRetryAfter(header)
	if data, err, matched := decodeIAMEnvelope(status, raw); matched {
		return data, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		if status >= 400 {
			return nil, &apiError{Status: status, Message: truncate(string(raw), 300), RetryAfter: ra}
		}
		return nil, &apiError{Status: status, Message: "unparseable response: " + truncate(string(raw), 300)}
	}
	if env.Code != 0 || (env.Error != nil && env.Error.Message != "") {
		msg := strings.TrimSpace(env.Message)
		if env.Error != nil && strings.TrimSpace(env.Error.Message) != "" {
			msg = strings.TrimSpace(env.Error.Message)
		}
		if d := strings.TrimSpace(env.Details); d != "" {
			if msg == "" {
				msg = d
			} else {
				msg = msg + ": " + d
			}
		}
		return nil, &apiError{Status: status, Code: env.Code, Message: msg, RetryAfter: ra}
	}
	if status >= 400 {
		if env.Error != nil && strings.TrimSpace(env.Error.Message) != "" {
			return nil, &apiError{Status: status, Message: strings.TrimSpace(env.Error.Message), RetryAfter: ra}
		}
		return nil, &apiError{Status: status, Message: http.StatusText(status), RetryAfter: ra}
	}
	return env.Data, nil
}

// parseRetryAfter honours the reset headers, capped at two hours so a broken or
// hostile value cannot park an account for a week.  The three forms cline and
// trae already parse: "Retry-After" in seconds or as an HTTP date,
// "X-Ratelimit-Reset" as an epoch second count, and "Retry-After-Ms" in
// milliseconds (issue #9).
func parseRetryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	const cap = 2 * time.Hour
	for _, key := range []string{"Retry-After", "X-Ratelimit-Reset", "Retry-After-Ms"} {
		v := strings.TrimSpace(h.Get(key))
		if v == "" {
			continue
		}
		if key == "Retry-After-Ms" {
			if ms, err := strconv.ParseInt(v, 10, 64); err == nil && ms > 0 {
				d := time.Duration(ms) * time.Millisecond
				if d > cap {
					d = cap
				}
				return d
			}
			continue
		}
		if secs, err := strconv.ParseInt(v, 10, 64); err == nil && secs > 0 {
			d := time.Duration(secs) * time.Second
			if d > cap {
				d = cap
			}
			return d
		}
		if t, err := http.ParseTime(v); err == nil {
			d := time.Until(t)
			if d <= 0 {
				continue
			}
			if d > cap {
				d = cap
			}
			return d
		}
	}
	return 0
}

// iamEnvelope is the IAM service's response shape. `code` is a symbolic string
// ("OK" on success) rather than the web API's number, and failures carry their
// own `message`/`details` and a `request_id`.
type iamEnvelope struct {
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	Details   json.RawMessage `json:"details"`
	RequestID string          `json:"request_id"`
	Data      json.RawMessage `json:"data"`
}

// iamOK is the only success code IAM uses.
const iamOK = "OK"

// decodeIAMEnvelope decodes the IAM shape. matched is false when the body is
// not an IAM envelope at all (no string `code`), which leaves the caller free
// to parse it as the web API's numeric one.
func decodeIAMEnvelope(status int, raw []byte) (data json.RawMessage, err error, matched bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, nil, false
	}
	var env iamEnvelope
	if jerr := json.Unmarshal(trimmed, &env); jerr != nil {
		return nil, nil, false
	}
	code := strings.TrimSpace(env.Code)
	// A body with no string code is not an IAM envelope; a 404 page, for
	// example, must fall through to the numeric parser so it is reported as
	// the HTTP error it is.
	if code == "" {
		return nil, nil, false
	}
	// A purely numeric code belongs to the web API even when it arrives
	// quoted, so it must keep its numeric meaning (code 100006 and friends
	// are matched on it). Only a symbolic code is IAM's.
	if _, aerr := strconv.Atoi(code); aerr == nil {
		return nil, nil, false
	}
	if env.Code == iamOK && status < 400 {
		return env.Data, nil, true
	}
	msg := strings.TrimSpace(env.Message)
	if d := strings.TrimSpace(string(env.Details)); d != "" && d != "{}" && d != "null" {
		if msg == "" {
			msg = d
		} else if !strings.Contains(msg, d) {
			msg = msg + ": " + d
		}
	}
	return nil, &apiError{Status: status, IAMCode: env.Code, Message: msg, RequestID: strings.TrimSpace(env.RequestID)}, true
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// requestTooLarge builds the error for the vendor's 413. The vendor's own
// message is boilerplate and names no number; the useful facts are how large
// the request was and what to shrink, so this one error says both.
func requestTooLarge(encodedBytes int) *apiError {
	return &apiError{
		Status:  http.StatusRequestEntityTooLarge,
		Message: fmt.Sprintf("request body too large (%s): the upstream refuses oversized chat requests; shorten the conversation, drop large images or attachments, or trim tool definitions", humanBytes(encodedBytes)),
	}
}

// humanBytes renders a byte count the way a one-line error wants it.
func humanBytes(n int) string {
	const kb = 1 << 10
	const mb = 1 << 20
	switch {
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	case n >= kb:
		return fmt.Sprintf("%.0f KB", float64(n)/kb)
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// do performs one JSON request/response exchange and decodes `data` into out
// (when out is non-nil).
//
// It also repairs the one recoverable refusal the vendor has: a 200901
// `iam_confirm_required`.  The web client handles that by confirming the
// business session and replaying the request, which is what happens here, so
// an account signed in before this step existed heals itself instead of
// sitting in the pool looking broke.
func (c *Client) do(ctx context.Context, method, path string, hdr http.Header, payload, out any) error {
	err := c.doOnce(ctx, method, path, hdr, payload, out)
	if err == nil || path == pathIAMConfirm || !needsBusinessConfirm(err) || !hasBearer(hdr) {
		return err
	}
	if cerr := c.confirmBusinessSession(ctx, hdr); cerr != nil {
		// The original refusal is the one the operator can act on: a confirm
		// failure only adds noise, but it must not be swallowed silently.
		c.deps.Log("raccoon: business session confirm failed: %v", c.scrub(cerr.Error()))
		return err
	}
	return c.doOnce(ctx, method, path, hdr, payload, out)
}

// needsBusinessConfirm reports whether err is the vendor's "confirm the
// business session first" refusal.  The vendor keys it on HTTP 400 plus the
// numeric code, and so does this predicate.
func needsBusinessConfirm(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == http.StatusBadRequest && ae.Code == codeIAMConfirmRequired
}

// hasBearer reports whether hdr carries a usable bearer token.  Unauthenticated
// calls (the IAM send/login pair) must never trigger a confirm.
func hasBearer(hdr http.Header) bool {
	if hdr == nil {
		return false
	}
	v := strings.TrimSpace(hdr.Get("Authorization"))
	rest := strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
	return strings.HasPrefix(v, "Bearer ") && rest != ""
}

// confirmBusinessSession confirms the business session the bearer token was
// minted for: POST /api/web/auth/v1/iam/confirm with an empty body, exactly as
// the vendor's web client does.  It goes through doOnce so a refusal from the
// confirm endpoint itself can never recurse.
func (c *Client) confirmBusinessSession(ctx context.Context, hdr http.Header) error {
	if !hasBearer(hdr) {
		return errors.New("raccoon: business session confirm needs a bearer token")
	}
	return c.doOnce(ctx, http.MethodPost, pathIAMConfirm, hdr, map[string]any{}, nil)
}

// doOnce is one exchange with no retry logic.
func (c *Client) doOnce(ctx context.Context, method, path string, hdr http.Header, payload, out any) error {
	resp, err := c.send(ctx, method, path, hdr, payload)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("raccoon: reading response: %w", err)
	}
	data, err := decodeEnvelope(resp.StatusCode, raw, resp.Header)
	if err != nil {
		return err
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("raccoon: decoding response data: %w", err)
	}
	return nil
}

// send performs the HTTP exchange and returns the live response. The caller
// owns the body. Encoding happens here so callers that only need the response
// keep a single marshal.
func (c *Client) send(ctx context.Context, method, path string, hdr http.Header, payload any) (*http.Response, error) {
	if payload == nil {
		return c.sendMarshaled(ctx, method, path, hdr, nil)
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("raccoon: encoding request: %w", err)
	}
	return c.sendMarshaled(ctx, method, path, hdr, buf)
}

// sendMarshaled issues the request with a body that has already been encoded.
// postChat goes through it directly: the encoded size is what a 413 refusal is
// about, and the error should carry it instead of leaving the operator to
// guess which part of the conversation the vendor choked on.
func (c *Client) sendMarshaled(ctx context.Context, method, path string, hdr http.Header, body []byte) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(orBackground(ctx), method, c.cfg.endpoint(path), reader)
	if err != nil {
		return nil, fmt.Errorf("raccoon: building request: %w", err)
	}
	req.Header.Set("User-Agent", userAgentFor(c.cfg.userAgent(), hdr.Get("X-Client-Platform"), hdr.Get("X-Client-Version"), hdr.Get("X-Client-Device-ID")))
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// ---- endpoints -------------------------------------------------------

// refreshResult is the useful part of a successful token renewal.
type refreshResult struct {
	AccessToken    string     `json:"access_token"`
	RefreshToken   string     `json:"refresh_token"`
	ExpiresAt      flexString `json:"expires_at"`
	Nickname       string     `json:"nickname"`
	OfficeIdentity string     `json:"office_identity"`
	UserID         string     `json:"user_id"`
	Phone          string     `json:"phone"`
}

// refresh exchanges a refresh token for a new access token.
//
// Two details are load-bearing:
//   - `data.refresh_token` may be ABSENT, in which case the old one is kept —
//     overwriting it with "" would make the account unrefreshable after one
//     renewal;
//   - the new expiry is taken from the JWT `exp` of the new access token, so
//     the store and the token can never disagree.
func (c *Client) refresh(ctx context.Context, cred credential) (refreshResult, error) {
	rt := strings.TrimSpace(cred.RefreshToken)
	if rt == "" {
		return refreshResult{}, errors.New("raccoon: credential has no refresh token")
	}
	// The reference builds every request from the one universal header
	// helper, so the (possibly expired) access token and X-Org-Code go out
	// here too.
	hdr := raccoonHeaders(cred, "", "")

	var res refreshResult
	err := c.do(ctx, http.MethodPost, pathRefresh, hdr, map[string]string{"refresh_token": rt}, &res)
	if err != nil {
		if isAuthError(err) {
			return refreshResult{}, fmt.Errorf("%w: %v", errSessionDead, err)
		}
		return refreshResult{}, err
	}
	if strings.TrimSpace(res.AccessToken) == "" {
		return refreshResult{}, errors.New("raccoon: refresh response carried no access_token")
	}
	if strings.TrimSpace(res.RefreshToken) == "" {
		res.RefreshToken = rt
	}
	if exp := jwtExpiryMs(res.AccessToken); exp > 0 {
		res.ExpiresAt = flexString(strconv.FormatInt(exp, 10))
	}
	if strings.TrimSpace(res.ExpiresAt.String()) == "" {
		res.ExpiresAt = cred.ExpiresAt
	}
	return res, nil
}

type userInfo struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	OfficeIdentity string `json:"office_identity"`
	Phone          string `json:"phone"`
}

// fetchUserInfo is best effort: the reference returns an empty object on any
// failure rather than throwing, so a metadata hiccup never breaks chat.
func (c *Client) fetchUserInfo(ctx context.Context, cred credential) (userInfo, error) {
	hdr := raccoonHeaders(cred, c.cfg.platform(), c.cfg.version())
	var info userInfo
	err := c.do(ctx, http.MethodGet, pathUserInfo, hdr, nil, &info)
	return info, err
}

// fetchCatalog reads the live model catalogue. Failure returns an empty list
// (the caller then keeps the built-in table); it never breaks the module.
func (c *Client) fetchCatalog(ctx context.Context, cred credential) ([]core.Model, error) {
	ctx, cancel := withTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()
	hdr := raccoonHeaders(cred, c.cfg.platform(), c.cfg.version())
	resp, err := c.send(ctx, http.MethodGet, pathCatalog, hdr, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	if _, err := decodeEnvelope(resp.StatusCode, raw, resp.Header); err != nil {
		return nil, err
	}
	return parseCatalog(raw), nil
}

// balanceData mirrors `data` of GET /api/web/points/v1/balance. Pointers are
// used so an ABSENT pool can be told apart from a zero one.
type balanceData struct {
	AvailablePoints *flexFloat `json:"available_points"`
	RewardPoints    *flexFloat `json:"reward_points"`
	DailyPoints     *flexFloat `json:"daily_points"`
	TopupPoints     *flexFloat `json:"topup_points"`
	MonthlyPoints   *flexFloat `json:"monthly_points"`
}

func (c *Client) fetchBalance(ctx context.Context, cred credential) (balanceData, error) {
	hdr := raccoonHeaders(cred, c.cfg.platform(), c.cfg.version())
	var b balanceData
	err := c.do(ctx, http.MethodGet, pathBalance, hdr, nil, &b)
	return b, err
}

type billItem struct {
	BizType   string     `json:"biz_type"`
	EventName string     `json:"event_name"`
	Points    *flexFloat `json:"points"`
	// The timestamps the ledger stamps on a reward row. They are what lets the
	// check-in tell "today's 300 is already in hand" from "this attempt was
	// refused but the reward may still land later today". See checkin.go.
	CreatedAt string `json:"created_at"`
	GrantAt   string `json:"grant_at"`
	ExpireAt  string `json:"expire_at"`
}

type billsData struct {
	Items []billItem `json:"items"`
}

// fetchBills is the read-only credits history. It is only used to explain a
// balance, never to claim anything.
func (c *Client) fetchBills(ctx context.Context, cred credential, limit, offset int) ([]billItem, error) {
	if limit <= 0 {
		limit = 50
	}
	path := pathBills + "?paging.limit=" + strconv.Itoa(limit) + "&paging.offset=" + strconv.Itoa(offset)
	hdr := raccoonHeaders(cred, c.cfg.platform(), c.cfg.version())
	var b billsData
	err := c.do(ctx, http.MethodGet, path, hdr, nil, &b)
	return b.Items, err
}

// orBackground normalises a nil context. Several entry points (Models,
// RefreshModels, Status, Discover) are legitimately called with one, and both
// context.WithTimeout and http.NewRequestWithContext panic on a nil parent.
func orBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// withTimeout is context.WithTimeout with a nil-safe parent.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(orBackground(ctx), d)
}

// deadline returns a context with the module's default request timeout.
func (c *Client) deadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return withTimeout(ctx, requestTimeout)
}
