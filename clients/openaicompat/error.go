package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"

	"client2api/internal/core"
)

// failureKind is this module's own, finer vocabulary, borrowed from the
// openrouter module: collapsing vendor errors early would lose the distinction
// between "this key is out of money" (park for a while) and "this model does
// not exist" (do not blame the account at all).
type failureKind string

const (
	kindNone       failureKind = ""
	kindCredit     failureKind = "credit"
	kindDailyQuota failureKind = "daily"
	kindRateLimit  failureKind = "rate_limit"
	kindAuth       failureKind = "auth"
	kindNotFound   failureKind = "not_found"
	kindBlocked    failureKind = "blocked"
	kindServer     failureKind = "server"
	kindClient     failureKind = "client"
)

var (
	creditMarkers = []string{
		"insufficient credit", "insufficient balance", "not enough credit",
		"out of credit", "no credit", "requires more credit", "add credit",
		"credit limit", "payment required", "exceeded your current quota",
		"quota exceeded", "insufficient_quota", "billing",
	}
	rateMarkers = []string{
		"rate limit", "rate-limit", "ratelimit", "too many requests",
		"requests per", "temporarily rate-limited", "slow down", "try again in",
	}
	authMarkers = []string{
		"no auth credentials", "missing authentication", "invalid api key",
		"invalid_api_key", "api key not found", "invalid key",
		"key was disabled", "key has been disabled", "revoked", "unauthorized",
		"authentication", "invalid credentials",
	}
	notFoundMarkers = []string{
		"model not found", "not a valid model", "unknown model",
		"invalid model", "does not exist", "no such model",
	}
	blockedMarkers = []string{
		"flagged", "moderation", "moderated", "content policy",
		"content_policy", "prohibited", "violates",
	}
)

func containsMarker(text string, markers []string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	for _, m := range markers {
		if m == "" {
			continue
		}
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// classifyFailure is a pure function of the status and the message text.
func classifyFailure(status int, text string) failureKind {
	if status == 402 {
		return kindCredit
	}
	if containsMarker(text, blockedMarkers) {
		return kindBlocked
	}
	if containsMarker(text, creditMarkers) {
		return kindCredit
	}
	if containsMarker(text, notFoundMarkers) {
		return kindNotFound
	}
	if containsMarker(text, authMarkers) {
		return kindAuth
	}
	if containsMarker(text, rateMarkers) {
		return kindRateLimit
	}
	switch {
	case status == 401 || status == 403:
		return kindAuth
	case status == 429:
		return kindRateLimit
	case status == 404:
		return kindNotFound
	case status >= 500:
		return kindServer
	case status >= 400:
		return kindClient
	}
	return kindNone
}

func coreKindFor(k failureKind) core.FailureKind {
	switch k {
	case kindCredit, kindDailyQuota:
		return core.FailureQuota
	case kindRateLimit:
		return core.FailureRateLimited
	case kindAuth:
		return core.FailureAuth
	case kindServer:
		return core.FailureUpstream
	case kindBlocked:
		return core.FailureContentBlocked
	default:
		return core.FailureOther
	}
}

// --- error envelope ----------------------------------------------------------

type compatErrorEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
		Type    string `json:"type"`
	} `json:"error"`
	Message string `json:"message"`
}

func parseErrorEnvelope(body []byte) (int, string) {
	var env compatErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, ""
	}
	code, _ := toInt(env.Error.Code)
	msg := strings.TrimSpace(env.Error.Message)
	if msg == "" {
		msg = strings.TrimSpace(env.Message)
	}
	if msg == "" {
		msg = strings.TrimSpace(env.Error.Type)
	}
	return code, msg
}

func errorTextOf(body []byte) string {
	_, msg := parseErrorEnvelope(body)
	if msg != "" {
		return truncate(msg, 300)
	}
	return truncate(strings.TrimSpace(string(body)), 300)
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// upstreamError is the transport-level detail inside a core.Failure.
type upstreamError struct {
	Op     string
	Status int
	Msg    string
}

func (e *upstreamError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("openai-compat: %s (HTTP %d): %s", e.Op, e.Status, e.Msg)
	}
	return fmt.Sprintf("openai-compat: %s: %s", e.Op, e.Msg)
}

// scrubFor strips anything credential-shaped out of a message.
func (c *Client) scrubFor(accountID, msg string) string {
	msg = core.Redact(msg)
	return truncate(msg, 300)
}

// classifyHTTP turns a non-2xx response into a typed failure.
func (c *Client) classifyHTTP(op, accountID string, status int, body []byte) error {
	_, msg := parseErrorEnvelope(body)
	if msg == "" {
		msg = truncate(strings.TrimSpace(string(body)), 300)
	}
	k := classifyFailure(status, msg)
	msg = c.scrubFor(accountID, msg)
	return core.Fail(clientName, accountID, coreKindFor(k), status, &upstreamError{Op: op, Status: status, Msg: msg})
}

// classifyUpstream turns a transport error into a typed failure.
func (c *Client) classifyUpstream(accountID string, err error) error {
	if err == nil {
		return nil
	}
	k := kindServer
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		k = kindNone
	} else if isTimeout(err) {
		k = kindServer
	}
	msg := truncate(core.Redact(err.Error()), 300)
	return core.Fail(clientName, accountID, coreKindFor(k), 0, &upstreamError{Op: "request", Msg: msg})
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// retryableChatError decides whether another provider is worth trying.
func retryableChatError(err error) bool {
	if err == nil {
		return false
	}
	var f *core.Failure
	if !errors.As(err, &f) {
		return false
	}
	switch f.Kind {
	case core.FailureQuota, core.FailureRateLimited, core.FailureAuth, core.FailureUpstream:
		return true
	}
	return false
}

// describeError is the one line Status()/Detail carries.
func describeError(err error) string {
	if err == nil {
		return ""
	}
	return truncate(core.Redact(err.Error()), 300)
}
