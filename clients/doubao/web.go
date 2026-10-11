package doubao

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"client2api/internal/core"
)

// web.go is the HTTP surface the module shares with the desktop client's web
// app: the common query parameters every request carries, the cookie header, the
// JSON envelope, and the error classification the pool acts on.

// upstreamErrKind classifies one upstream failure.
type upstreamErrKind int

const (
	errTransport upstreamErrKind = iota
	errAuth
	errRateLimit
	errServer
)

// upstreamError is a typed failure so the pool and the panel can react
// differently to "sign in again" and "slow down".
type upstreamError struct {
	Kind    upstreamErrKind
	Status  int
	Message string
}

func (e *upstreamError) Error() string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	switch e.Kind {
	case errAuth:
		return "doubao: the cookie session is no longer valid (HTTP " + strconv.Itoa(e.Status) + "): " + msg
	case errRateLimit:
		return "doubao: rate limited (HTTP " + strconv.Itoa(e.Status) + "): " + msg
	default:
		return "doubao: upstream returned HTTP " + strconv.Itoa(e.Status) + ": " + msg
	}
}

// isAuthError reports whether err means the credential itself is dead.
func isAuthError(err error) bool {
	var ue *upstreamError
	if errors.As(err, &ue) {
		return ue.Kind == errAuth
	}
	return false
}

// isRateLimitError reports whether err means "slow down, try later".
func isRateLimitError(err error) bool {
	var ue *upstreamError
	if errors.As(err, &ue) {
		return ue.Kind == errRateLimit
	}
	return false
}

// upstream owns the shared HTTP client and the request shape.
type upstream struct {
	cfg    config
	client *http.Client
	logf   func(string, ...any)
}

func newUpstream(cfg config, shared *http.Client, logf func(string, ...any)) *upstream {
	client := shared
	if client == nil {
		client = &http.Client{Timeout: cfg.requestTimeout()}
	}
	return &upstream{cfg: cfg, client: client, logf: logf}
}

// log is a nil-safe scoped logger.
func (u *upstream) log(format string, args ...any) {
	if u.logf != nil {
		u.logf(format, args...)
	}
}

// commonParams are the query parameters the web app attaches to every call.
// They identify the client build, not the user; the account's identity is the
// cookie alone.
func (u *upstream) commonParams() url.Values {
	q := url.Values{}
	q.Set("aid", paramAid)
	q.Set("real_aid", paramRealAid)
	q.Set("pkg_type", paramPkgType)
	q.Set("device_platform", "web")
	q.Set("doubao_device_platform", "web")
	q.Set("samantha_web", "1")
	q.Set("region", paramRegion)
	q.Set("sys_region", paramRegion)
	q.Set("language", "zh")
	q.Set("use-olympus-account", "1")
	q.Set("version_code", paramVersion)
	return q
}

// getJSON performs a GET and decodes the JSON body.
func (u *upstream) getJSON(ctx context.Context, acc *account, path string, params url.Values, out any) error {
	q := u.commonParams()
	for k, vs := range params {
		for _, v := range vs {
			q.Set(k, v)
		}
	}
	endpoint := u.cfg.webBase() + path
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	return u.do(ctx, req, acc, out)
}

// postJSON performs a POST of a JSON body and decodes the JSON reply.
func (u *upstream) postJSON(ctx context.Context, acc *account, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	q := u.commonParams()
	endpoint := u.cfg.webBase() + path + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return u.do(ctx, req, acc, out)
}

// rawPost performs a POST and returns the response for streaming, without
// consuming the body.  The caller owns closing it.
func (u *upstream) rawPost(ctx context.Context, acc *account, path string, body any) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	q := u.commonParams()
	endpoint := u.cfg.webBase() + path + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	u.decorate(req, acc)
	client := u.streamClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("doubao: request failed: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return nil, classifyHTTP(resp.StatusCode, msg)
	}
	return resp, nil
}

// do sends an idempotent request, decodes the JSON reply and classifies errors.
func (u *upstream) do(ctx context.Context, req *http.Request, acc *account, out any) error {
	u.decorate(req, acc)
	resp, err := u.client.Do(req)
	if err != nil {
		return fmt.Errorf("doubao: request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("doubao: reading the response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return classifyHTTP(resp.StatusCode, body)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("doubao: decoding the response: %w", err)
	}
	return nil
}

// decorate attaches the headers every request carries.  The session is the
// cookie header; Agw-Js-Conv is the only extra header the web app adds.
func (u *upstream) decorate(req *http.Request, acc *account) {
	if req.Header.Get("Content-Type") == "" && req.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Agw-Js-Conv", agentJSConv)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	if acc != nil && strings.TrimSpace(acc.Cookie) != "" {
		req.Header.Set("Cookie", acc.Cookie)
	}
}

// streamClient is a client with no overall timeout: a chat stream may run for
// minutes, and cancellation is the caller's context.
func (u *upstream) streamClient() *http.Client {
	base := u.client
	if base == nil {
		return &http.Client{}
	}
	cp := *base
	cp.Timeout = 0
	return &cp
}

// classifyHTTP turns a non-2xx status into a typed error.
func classifyHTTP(status int, body []byte) error {
	msg := extractErrorMessage(body)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &upstreamError{Kind: errAuth, Status: status, Message: msg}
	case status == http.StatusTooManyRequests:
		return &upstreamError{Kind: errRateLimit, Status: status, Message: msg}
	default:
		return &upstreamError{Kind: errServer, Status: status, Message: msg}
	}
}

// extractErrorMessage pulls the vendor's own message out of a JSON error body,
// falling back to a trimmed snippet.  It never returns more than 300 bytes and
// never includes a cookie.
func extractErrorMessage(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return ""
	}
	var envelope struct {
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Data    struct {
			Message string `json:"message"`
			Msg     string `json:"msg"`
		} `json:"data"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err == nil {
		if m := firstNonEmpty(envelope.Message, envelope.Msg, envelope.Data.Message, envelope.Data.Msg); m != "" {
			return truncate(core.Redact(m), 300)
		}
	}
	return truncate(core.Redact(string(trimmed)), 300)
}

// truncate bounds a message for the panel.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// accountInfo narrows the account/profile replies to the fields Status and the
// login flow use. A zero value means "not signed in".
type accountInfo struct {
	ID         string `json:"id"`
	UserID     string `json:"user_id"`
	UID        string `json:"uid"`
	Nickname   string `json:"nickname"`
	Name       string `json:"name"`
	UniqueID   string `json:"unique_id"`
	IsLogin    *bool  `json:"is_login"`
	AvatarURL  string `json:"avatar_url"`
	ScreenName string `json:"screen_name"`
	EntityID   string `json:"entity_id"`
	UserName   string `json:"user_name"`
}

// displayName is a non-secret label for the account.
func (i accountInfo) displayName() string {
	if n := firstNonEmpty(i.Nickname, i.Name, i.ScreenName, i.UserName); n != "" {
		return core.Redact(n)
	}
	if id := firstNonEmpty(i.ID, i.UserID, i.UID, i.UniqueID, i.EntityID); id != "" {
		return "uid:" + core.Redact(id)
	}
	return "signed in"
}

// identity returns a stable, non-secret account key when the vendor exposes
// one.  A fresh web login often rotates the cookie jar for the same user, so
// matching on the full jar would create a duplicate row every time.
func (i accountInfo) identity() string {
	id := firstNonEmpty(i.ID, i.UserID, i.UID, i.UniqueID, i.EntityID)
	if id == "" {
		return ""
	}
	return "db-user-" + core.Redact(id)
}

// signedIn reports whether the reply proves a live session.
func (i accountInfo) signedIn() bool {
	if i.IsLogin != nil {
		return *i.IsLogin
	}
	return firstNonEmpty(i.ID, i.UserID, i.UID, i.UniqueID, i.Nickname, i.Name, i.UserName, i.EntityID) != ""
}

// accountInfo probes one account. The current client keeps the display name
// and stable id on the profile brief endpoint; the older account endpoint is
// kept as a compatibility fallback for builds that only expose that shape.
func (u *upstream) accountInfo(ctx context.Context, acc *account) (accountInfo, error) {
	cctx, cancel := context.WithTimeout(ctx, u.cfg.probeTimeout())
	defer cancel()
	var profile struct {
		Data struct {
			ProfileBrief accountInfo `json:"profile_brief"`
		} `json:"data"`
	}
	if err := u.postJSON(cctx, acc, pathProfileBrief, map[string]any{"avatar_format": 0}, &profile); err == nil {
		if info := profile.Data.ProfileBrief; info.signedIn() {
			return info, nil
		}
	}

	var out struct {
		Data  accountInfo `json:"data"`
		Extra accountInfo `json:"extra"`
	}
	if err := u.postJSON(cctx, acc, pathAccountInfo, map[string]any{}, &out); err != nil {
		return accountInfo{}, err
	}
	// The reply nests the user under "data" on the current build; some builds
	// put it at the top level.  Try both.
	info := out.Data
	if !info.signedIn() {
		info = out.Extra
	}
	if !info.signedIn() {
		return accountInfo{}, &upstreamError{Kind: errAuth, Status: http.StatusUnauthorized, Message: "the cookie session is not signed in"}
	}
	return info, nil
}
