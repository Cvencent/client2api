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

// maxResponseBytes bounds a non-streaming response body.
const maxResponseBytes = 8 << 20

// envelope is the vendor's universal response shape.
type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Details string          `json:"details"`
	Data    json.RawMessage `json:"data"`
}

// apiError is one failed upstream call.
type apiError struct {
	Status  int
	Code    int
	Message string
}

func (e *apiError) Error() string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if msg == "" {
		msg = "upstream error"
	}
	if e.Code != 0 {
		return fmt.Sprintf("raccoon: %s (code %d)", msg, e.Code)
	}
	if e.Status >= 400 {
		return fmt.Sprintf("raccoon: HTTP %d: %s", e.Status, msg)
	}
	return "raccoon: " + msg
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
// authority, so a >= 400 response is an error even with no envelope.
func decodeEnvelope(status int, raw []byte) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		if status >= 400 {
			return nil, &apiError{Status: status, Message: truncate(string(raw), 300)}
		}
		return nil, &apiError{Status: status, Message: "unparseable response: " + truncate(string(raw), 300)}
	}
	if env.Code != 0 {
		msg := strings.TrimSpace(env.Message)
		if d := strings.TrimSpace(env.Details); d != "" {
			if msg == "" {
				msg = d
			} else {
				msg = msg + ": " + d
			}
		}
		return nil, &apiError{Status: status, Code: env.Code, Message: msg}
	}
	if status >= 400 {
		return nil, &apiError{Status: status, Message: http.StatusText(status)}
	}
	return env.Data, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// do performs one JSON request/response exchange and decodes `data` into out
// (when out is non-nil).
func (c *Client) do(ctx context.Context, method, path string, hdr http.Header, payload, out any) error {
	resp, err := c.send(ctx, method, path, hdr, payload)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("raccoon: reading response: %w", err)
	}
	data, err := decodeEnvelope(resp.StatusCode, raw)
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
// owns the body.
func (c *Client) send(ctx context.Context, method, path string, hdr http.Header, payload any) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("raccoon: encoding request: %w", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(orBackground(ctx), method, c.cfg.endpoint(path), body)
	if err != nil {
		return nil, fmt.Errorf("raccoon: building request: %w", err)
	}
	req.Header.Set("User-Agent", c.cfg.userAgent())
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	if payload != nil && req.Header.Get("Content-Type") == "" {
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
	hdr := raccoonHeaders(cred, "", "")
	var info userInfo
	err := c.do(ctx, http.MethodGet, pathUserInfo, hdr, nil, &info)
	return info, err
}

// fetchCatalog reads the live model catalogue. Failure returns an empty list
// (the caller then keeps the built-in table); it never breaks the module.
func (c *Client) fetchCatalog(ctx context.Context, cred credential) ([]core.Model, error) {
	ctx, cancel := withTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()
	hdr := raccoonHeaders(cred, "", "")
	resp, err := c.send(ctx, http.MethodGet, pathCatalog, hdr, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	if _, err := decodeEnvelope(resp.StatusCode, raw); err != nil {
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
	hdr := raccoonHeaders(cred, "", "")
	var b balanceData
	err := c.do(ctx, http.MethodGet, pathBalance, hdr, nil, &b)
	return b, err
}

type billItem struct {
	BizType   string     `json:"biz_type"`
	EventName string     `json:"event_name"`
	Points    *flexFloat `json:"points"`
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
	hdr := raccoonHeaders(cred, "", "")
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
