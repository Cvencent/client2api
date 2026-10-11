package autoclaw

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

var errSessionDead = errors.New("autoclaw: refresh token rejected, please sign in again")

const (
	refreshCodeDead  = 400000
	maxResponseBytes = 8 << 20
)

// envelope is the AutoClaw/GLM gateway response shape.  The desktop client has
// shipped both `msg` and `message`, so both are accepted.
type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Msg     string          `json:"msg"`
	Details string          `json:"details"`
	Data    json.RawMessage `json:"data"`
}

func (e envelope) text() string {
	if s := strings.TrimSpace(e.Message); s != "" {
		return s
	}
	if s := strings.TrimSpace(e.Msg); s != "" {
		return s
	}
	return strings.TrimSpace(e.Details)
}

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
		return fmt.Sprintf("autoclaw: %s (code %d)", msg, e.Code)
	}
	if e.Status >= 400 {
		return fmt.Sprintf("autoclaw: HTTP %d: %s", e.Status, msg)
	}
	return "autoclaw: " + msg
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

func isAuthError(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) {
		return false
	}
	switch ae.Code {
	case refreshCodeDead, 410000:
		return true
	}
	return ae.Status == http.StatusUnauthorized || ae.Status == http.StatusForbidden
}

func decodeEnvelope(status int, raw []byte) (json.RawMessage, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		if status >= 400 {
			return nil, &apiError{Status: status, Message: truncate(string(raw), 300)}
		}
		return nil, &apiError{Status: status, Message: "unparseable response: " + truncate(string(raw), 300)}
	}
	if env.Code != 0 {
		return nil, &apiError{Status: status, Code: env.Code, Message: env.text()}
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
	return s[:n] + "..."
}

func (c *Client) do(ctx context.Context, method, path string, hdr http.Header, payload, out any) error {
	resp, err := c.send(ctx, method, path, hdr, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("autoclaw: reading response: %w", err)
	}
	data, err := decodeEnvelope(resp.StatusCode, raw)
	if err != nil {
		return err
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("autoclaw: decoding response data: %w", err)
	}
	return nil
}

func (c *Client) send(ctx context.Context, method, path string, hdr http.Header, payload any) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("autoclaw: encoding request: %w", err)
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(orBackground(ctx), method, c.cfg.endpoint(path), body)
	if err != nil {
		return nil, fmt.Errorf("autoclaw: building request: %w", err)
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
	return c.httpc.Do(req)
}

type refreshResult struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	UserID       string `json:"user_id"`
	Phone        string `json:"phone"`
}

func (c *Client) refresh(ctx context.Context, cred credential) (refreshResult, error) {
	rt := strings.TrimSpace(cred.RefreshToken)
	if rt == "" {
		return refreshResult{}, errors.New("autoclaw: credential has no refresh token")
	}
	payload := map[string]string{
		"refresh_token": rt,
		"source_id":     "autoclaw",
		"device_id":     strings.TrimSpace(cred.DeviceID),
	}
	var res refreshResult
	err := c.do(ctx, http.MethodPost, pathRefresh, identityHeaders(cred, c.cfg.version()), payload, &res)
	if err != nil && codeOf(err) == 400002 {
		err = c.do(ctx, http.MethodPost, pathAgentRefresh, identityHeaders(cred, c.cfg.version()), payload, &res)
	}
	if err != nil {
		if isAuthError(err) {
			return refreshResult{}, fmt.Errorf("%w: %v", errSessionDead, err)
		}
		return refreshResult{}, err
	}
	if strings.TrimSpace(res.AccessToken) == "" {
		return refreshResult{}, errors.New("autoclaw: refresh response carried no access_token")
	}
	if strings.TrimSpace(res.RefreshToken) == "" {
		res.RefreshToken = rt
	}
	return res, nil
}

type userInfo struct {
	UserID        string `json:"user_id"`
	ID            string `json:"id"`
	Name          string `json:"user_name"`
	Nickname      string `json:"nickname"`
	Phone         string `json:"user_phone"`
	NumericUserID any    `json:"numeric_user_id"`
	FirstLogin    bool   `json:"first_login"`
	WebFirstLogin bool   `json:"web_first_login"`
	IsGuest       bool   `json:"is_guest"`
}

func (u userInfo) identity() string {
	if s := strings.TrimSpace(u.UserID); s != "" {
		return s
	}
	return strings.TrimSpace(u.ID)
}

func (u userInfo) display() string {
	if s := strings.TrimSpace(u.Name); s != "" {
		return s
	}
	return strings.TrimSpace(u.Nickname)
}

func (u userInfo) numericID() string {
	switch v := u.NumericUserID.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case json.Number:
		return v.String()
	}
	return ""
}

func (c *Client) fetchUserInfo(ctx context.Context, cred credential) (userInfo, error) {
	payload := map[string]string{
		"source_id": "autoclaw",
		"device_id": strings.TrimSpace(cred.DeviceID),
	}
	var info userInfo
	err := c.do(ctx, http.MethodPost, pathUserInfo, identityHeaders(cred, c.cfg.version()), payload, &info)
	return info, err
}

func (c *Client) fetchCatalog(ctx context.Context, cred credential) ([]core.Model, error) {
	ctx, cancel := withTimeout(ctx, c.cfg.modelsTimeout())
	defer cancel()
	hdr := identityHeaders(cred, c.cfg.version())
	resp, err := c.send(ctx, http.MethodGet, pathModelConfig, hdr, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	if _, err := decodeEnvelope(resp.StatusCode, raw); err != nil {
		return nil, err
	}
	models := parseCatalog(raw)
	if len(models) == 0 {
		return nil, errors.New("autoclaw: model catalogue came back empty")
	}
	return models, nil
}

func orBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(orBackground(ctx), d)
}

func (c *Client) deadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return withTimeout(ctx, requestTimeout)
}
