package trae

// upstream.go — the transport layer: error taxonomy, request helpers and the
// three upstream calls (chat stream, token exchange, model catalogue).
//
// Ported from the MIT reference client2api-lab/_upstream/trae2api-web
// (internal/upstream/{client.go,headers.go,constants.go}).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"client2api/internal/core"
)

// Upstream endpoint paths, all relative to the chat host except the token
// exchange which lives on the account host.
const (
	epChat     = "/api/agent/v3/llm_utils_chat"
	epModels   = "/api/ide/v1/get_detail_param"
	epExchange = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	epUserInfo = "/cloudide/api/v3/trae/GetUserInfo"
)

// maxErrBody caps how much of an error body we keep for a message.
const maxErrBody = 1 << 20

// ---- error taxonomy -------------------------------------------------------

// ErrKind classifies an upstream failure so the pool can pick a cooldown.
type ErrKind int

const (
	ErrNone        ErrKind = iota // no failure
	ErrPlanLimit                  // 1005 — plan/entitlement exhausted
	ErrQuota                      // 4008 — account quota exhausted
	ErrAuth                       // 1001 / 4010 — credential rejected
	ErrParam                      // 4001 / 4023 — request not representable
	ErrSoftRate                   // 429 / 4011 — rate limited
	ErrRetryLater                 // 9074 — transient contention, retry later
	ErrSessionDead                // 401 — session expired, refresh first
	ErrNotFound                   // 404
	ErrServer                     // 5xx
	ErrClient                     // any other 4xx
	ErrTransport                  // network / connection failure
)

func (k ErrKind) String() string {
	switch k {
	case ErrNone:
		return "ok"
	case ErrPlanLimit:
		return "plan_limit"
	case ErrQuota:
		return "quota"
	case ErrAuth:
		return "auth"
	case ErrParam:
		return "param"
	case ErrSoftRate:
		return "soft_rate"
	case ErrRetryLater:
		return "retry_later"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrTransport:
		return "transport"
	}
	return "unknown"
}

// Error is a classified upstream failure.
type Error struct {
	Kind       ErrKind
	Code       int64 // vendor business code parsed from the body; 0 if the body carried none
	Status     int
	Msg        string
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	var b strings.Builder
	b.WriteString(e.Kind.String())
	if e.Code != 0 {
		fmt.Fprintf(&b, " code=%d", e.Code)
	}
	if e.Status != 0 {
		fmt.Fprintf(&b, " http=%d", e.Status)
	}
	if e.Msg != "" {
		b.WriteString(": ")
		b.WriteString(e.Msg)
	}
	return b.String()
}

// FailoverCode reports whether a business code means another account could
// plausibly succeed.  This is the documented failover set {4008, 1001, 4010};
// 1005 (plan_limit) is deliberately NOT in it.
func FailoverCode(code int64) bool {
	switch code {
	case 4008, 1001, 4010:
		return true
	}
	return false
}

// ClassifyCode maps an upstream business code to an ErrKind.  These codes
// arrive inside an HTTP 200 SSE stream (event:error) as well as in JSON error
// bodies.
func ClassifyCode(code int64) ErrKind {
	switch code {
	case 1005:
		return ErrPlanLimit
	case 1001, 4010:
		return ErrAuth
	case 4008:
		return ErrQuota
	case 4001, 4023:
		return ErrParam
	case 4011:
		return ErrSoftRate
	case 9074:
		return ErrRetryLater
	}
	if code >= 5000 {
		return ErrServer
	}
	return ErrClient
}

var codePattern = regexp.MustCompile(`"code"\s*:\s*(-?\d+)`)

// businessCode pulls a business code out of a JSON-ish error body.
func businessCode(body string) (int64, bool) {
	m := codePattern.FindStringSubmatch(body)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// classifyBody pairs Classify with the vendor's numeric code, so an *Error can
// keep both.  The code is parsed to classify the failure anyway; discarding it
// afterwards is what made the HTTP path disagree with the SSE path about the
// very same upstream refusal, and it is the number an operator greps for.
func classifyBody(status int, body []byte) (ErrKind, int64) {
	code, _ := businessCode(string(body))
	return Classify(status, body), code
}

// Classify maps an HTTP status plus response body to an ErrKind.
//
// The HTTP status wins over the body's business code when it is a 401.  The
// vendor sends its auth code (1001/4010) inside a 401 too, and those codes
// normally mean "rotate to another account, this one may recover".  On a 401
// the credential was refused at the transport-auth layer instead: no amount
// of waiting brings it back, only a re-login does.  Letting the code win
// demoted that to ErrAuth's 60-second cooling, so the pool re-selected a dead
// token every minute (one live account had 37 failures and was still marked
// ready).  The numeric code is still parsed by classifyBody and kept on the
// *Error, so the operator keeps the number to grep for.
func Classify(status int, body []byte) ErrKind {
	if status == http.StatusUnauthorized {
		return ErrSessionDead
	}
	text := string(body)
	if code, ok := businessCode(text); ok {
		if k := ClassifyCode(code); k != ErrClient {
			return k
		}
	}
	switch {
	case status == 429:
		return ErrSoftRate
	case status == 404:
		return ErrNotFound
	case status >= 500:
		return ErrServer
	case status >= 400:
		return ErrClient
	}
	return ErrNone
}

// parseRetryAfter honours a sane Retry-After style hint.
func parseRetryAfter(h http.Header) time.Duration {
	for _, key := range []string{"Retry-After", "Retry-After-Ms", "X-Ratelimit-Reset"} {
		v := strings.TrimSpace(h.Get(key))
		if v == "" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			continue
		}
		d := time.Duration(n) * time.Second
		if key == "Retry-After-Ms" {
			d = time.Duration(n) * time.Millisecond
		}
		if d > 0 && d <= retryAfterSanity {
			return d
		}
	}
	return 0
}

// retryAfterSanity caps how much trust we place in an upstream hint.
const retryAfterSanity = 2 * time.Hour

// truncate shortens a string for a log/message, appending an ellipsis.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---- request headers ------------------------------------------------------

// soloHeaders builds the full SOLO request header set.  Every version value is
// configurable; the defaults mirror the Go reference.
func (c *Client) soloHeaders(a *Auth, stream bool) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if stream {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
	h.Set("User-Agent", c.cfg.userAgent())
	h.Set("Authorization", "Cloud-IDE-JWT "+a.Token())
	h.Set("X-Cloudide-Token", a.Token())
	h.Set("X-Ide-Token", a.Token())
	if uid := a.UserID; uid != "" {
		h.Set("X-Uid", uid)
	}
	h.Set("X-App-Id", c.cfg.appID())
	h.Set("X-App-Version", c.cfg.appVersion())
	h.Set("X-Ide-Version", c.cfg.ideVersion())
	h.Set("X-Ide-Version-Code", c.cfg.ideVersionCode())
	h.Set("X-App-Version-Code", c.cfg.ideVersionCode())
	h.Set("X-Ide-Version-Type", c.cfg.ideVersionType())
	h.Set("X-Device-Type", c.cfg.deviceType())
	h.Set("X-OS-Version", c.cfg.osVersion())
	h.Set("X-Device-Brand", c.cfg.deviceBrand())
	h.Set("Request-Traffic-Type", c.cfg.trafficType())
	if mid := a.MachineID; mid != "" {
		h.Set("X-Machine-Id", mid)
	}
	if did := a.DeviceID; did != "" {
		h.Set("X-Device-Id", did)
	}
	if cpu := c.cfg.deviceCPU(); cpu != "" {
		h.Set("X-Device-Cpu", cpu)
	}
	// The Node reference sends both trace headers on every API request, not just
	// on streams (`trae2api/src/auth.js:1179-1181`), and it labels x-flow-traceparent
	// as the SOLO-only traceparent. Its version field is a literal "04" rather than
	// the W3C default "00", and the span is the first 16 hex of a random id — so the
	// pair is copied verbatim rather than "corrected" into a canonical W3C value.
	traceID := newTraceID()
	h.Set("X-Custom-Trace-Id", traceID)
	h.Set("X-Flow-Traceparent", "04-"+traceID+"-"+newSpanID()+"-01")
	if stream {
		trace := newTraceID()
		h.Set("X-Request-ID", trace)
		h.Set("X-Trae-Request-ID", trace)
	}
	for k, v := range c.cfg.ExtraHeaders {
		if v != "" {
			h.Set(k, v)
		}
	}
	return h
}

// oauthHeaders is the minimal header set used for the token exchange.
func (c *Client) oauthHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("User-Agent", c.cfg.userAgent())
	return h
}

// ---- requests -------------------------------------------------------------

// doJSON performs a buffered JSON request and decodes the response into out.
// Non-2xx responses come back as a classified *Error.
func (c *Client) doJSON(ctx context.Context, url string, headers http.Header, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = headers
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &Error{Kind: ErrTransport, Msg: err.Error()}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	if err != nil {
		return &Error{Kind: ErrTransport, Msg: err.Error()}
	}
	if resp.StatusCode >= 400 {
		kind, code := classifyBody(resp.StatusCode, raw)
		return &Error{
			Kind:       kind,
			Status:     resp.StatusCode,
			Code:       code,
			Msg:        truncate(string(raw), 200),
			RetryAfter: parseRetryAfter(resp.Header),
		}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode upstream response: %w", err)
		}
	}
	return nil
}

// chatStream opens the streaming chat endpoint.  A non-2xx status is returned
// as a classified *Error with a nil body so the caller can fail over.
func (c *Client) chatStream(ctx context.Context, a *Auth, body []byte) (io.ReadCloser, *Error, error) {
	url := c.cfg.chatHost() + epChat
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header = c.soloHeaders(a, true)

	resp, err := c.streamClient.Do(req)
	if err != nil {
		return nil, nil, &Error{Kind: ErrTransport, Msg: err.Error()}
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		kind, code := classifyBody(resp.StatusCode, raw)
		return nil, &Error{
			Kind:       kind,
			Status:     resp.StatusCode,
			Code:       code,
			Msg:        truncate(string(raw), 200),
			RetryAfter: parseRetryAfter(resp.Header),
		}, nil
	}
	return resp.Body, nil, nil
}

// exchangeResult is the token exchange response.
type exchangeResult struct {
	Token               string `json:"Token"`
	TokenExpireAt       int64  `json:"TokenExpireAt"`
	TokenExpireDuration int64  `json:"TokenExpireDuration"`
	RefreshToken        string `json:"RefreshToken"`
	RefreshExpireAt     int64  `json:"RefreshExpireAt"`
}

// refreshToken exchanges the refresh token for a fresh access token.  On ANY
// failure the stored credential is left untouched so the old refresh token can
// still be retried later.
func (c *Client) refreshToken(ctx context.Context, a *Auth) error {
	host := strings.TrimRight(a.Host, "/")
	if host == "" {
		host = c.cfg.authHost()
	}
	body, err := json.Marshal(map[string]any{
		"ClientID":     c.cfg.clientID(),
		"RefreshToken": a.RefreshTokenValue(),
		"ClientSecret": "-",
		"UserID":       "",
	})
	if err != nil {
		return err
	}
	var res exchangeResult
	if err := c.doJSON(ctx, host+epExchange, c.oauthHeaders(), body, &res); err != nil {
		return err
	}
	if strings.TrimSpace(res.Token) == "" {
		return &Error{Kind: ErrAuth, Msg: "refresh failed: no token in response — re-login required"}
	}
	accessExpiry := epochTime(res.TokenExpireAt)
	if accessExpiry.IsZero() && res.TokenExpireDuration > 0 {
		accessExpiry = time.Now().Add(time.Duration(res.TokenExpireDuration) * time.Second)
	}
	refreshExpiry := epochTime(res.RefreshExpireAt)
	// The upstream rotates the refresh token; keep the old one if the response
	// omitted a replacement.
	rotated := strings.TrimSpace(res.RefreshToken)
	if rotated == "" {
		rotated = a.RefreshTokenValue()
	}
	a.SetTokens(res.Token, rotated, accessExpiry, refreshExpiry)
	return nil
}

// modelEntry is one entry of the upstream model catalogue.
type modelEntry struct {
	ConfigName    string `json:"config_name"`
	DisplayConfig struct {
		DisplayName string `json:"display_name"`
	} `json:"display_config"`
}

// fetchModels asks the upstream for the model catalogue of the configured
// channel.
func (c *Client) fetchModels(ctx context.Context, a *Auth) ([]core.Model, error) {
	body, err := json.Marshal(map[string]any{
		"function":            c.cfg.functionName(),
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	})
	if err != nil {
		return nil, err
	}
	var res struct {
		ConfigInfoList []modelEntry `json:"config_info_list"`
	}
	if err := c.doJSON(ctx, c.cfg.chatHost()+epModels, c.soloHeaders(a, false), body, &res); err != nil {
		return nil, err
	}
	out := make([]core.Model, 0, len(res.ConfigInfoList))
	for _, e := range res.ConfigInfoList {
		name := strings.TrimSpace(e.ConfigName)
		if name == "" {
			continue
		}
		m := core.Model{ID: name, OwnedBy: c.name}
		if e.DisplayConfig.DisplayName != "" {
			m.Extra = map[string]any{"name": e.DisplayConfig.DisplayName}
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, errors.New("upstream returned an empty model catalogue")
	}
	return out, nil
}
