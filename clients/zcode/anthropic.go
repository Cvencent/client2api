package zcode

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Error classification
// ---------------------------------------------------------------------------

// Failure kinds drive both the retry decision and the account state machine.
const (
	kindModel       = "model"
	kindCaptcha     = "captcha"
	kindConcurrency = "concurrency"
	kindRisk        = "risk"
	kindExhausted   = "exhausted"
	kindInvalid     = "invalid"
	kindRateLimit   = "rate_limit"
	kindOther       = "other"
)

// Vendor codes this module names.  They are the only numeric codes with a
// meaning of their own; every other refusal is keyed off the HTTP status.
const (
	codeModelNotAllowed = 3006 // this credential is not entitled to this model
	codeCaptcha         = 3007 // Aliyun verification parameter missing or stale
	codeConcurrency     = 3009 // concurrency slots full
	codeRiskControl     = 3012 // ZCode risk control ("unusual activity")
)

// exhaustKeywords: a 402, or any of these appearing in the body, means the
// credential has no quota left.
var exhaustKeywords = []string{"quota", "insufficient", "balance", "exhaust", "额度", "余额不足"}

// captchaKeywords distinguish a captcha challenge from a genuine auth failure.
var captchaKeywords = []string{"captcha", "verify"}

// upstreamError is a classified upstream rejection.
type upstreamError struct {
	Status int
	Code   int
	Kind   string
	Msg    string
}

func (e *upstreamError) Error() string {
	if e == nil {
		return "zcode: upstream error"
	}
	return "zcode upstream " + e.short()
}

// short renders a bounded, log-safe one-liner.
func (e *upstreamError) short() string {
	if e == nil {
		return "upstream error"
	}
	msg := truncate(strings.TrimSpace(e.Msg), 160)
	if msg == "" {
		msg = "no message"
	}
	if e.Code != 0 {
		return fmt.Sprintf("HTTP %d code %d: %s", e.Status, e.Code, msg)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, msg)
}

// classify turns an HTTP status plus body into a typed failure.  Order matters:
// a 429 that happens to mention "quota" is a rate limit, not an exhausted
// credential, so the status checks come first.
func classify(status int, body []byte) *upstreamError {
	e := &upstreamError{Status: status, Code: extractCode(body), Msg: extractMessage(body)}
	lower := strings.ToLower(string(body))

	switch {
	case e.Code == codeModelNotAllowed:
		e.Kind = kindModel
	case e.Code == codeCaptcha:
		e.Kind = kindCaptcha
	case e.Code == codeConcurrency:
		e.Kind = kindConcurrency
	case e.Code == codeRiskControl:
		e.Kind = kindRisk
	case status == http.StatusMethodNotAllowed:
		// 405 is the other shape risk control arrives in: the vendor's code
		// table pairs it with 3012, and this endpoint only ever serves POST,
		// so a 405 here is never a genuine "method not allowed".
		e.Kind = kindRisk
	case status == 402:
		e.Kind = kindExhausted
	case status == 401 || status == 403:
		if containsAny(lower, captchaKeywords) {
			e.Kind = kindCaptcha
		} else {
			e.Kind = kindInvalid
		}
	case status == 429:
		e.Kind = kindRateLimit
	case containsAny(lower, exhaustKeywords):
		e.Kind = kindExhausted
	default:
		e.Kind = kindOther
	}
	return e
}

// failureKind translates this module's private classification into the shared
// core.FailureKind vocabulary the gateway acts on.  It is the one place a zcode
// refusal becomes a cross-module decision, so it is kept in a single function
// and asserted directly by failure_test.go.
//
// Risk control is the case the contract exists for.  3012 -- which the vendor
// answers with HTTP 405 -- is decided from the egress IP, the device id and the
// request shape, not from the credential: cooling the account that received it
// and immediately spending the next one only exposes that one to the same
// refusal.  Reporting it as a WAF block is what feeds the gateway's IP-level
// gate, the only mechanism that stops the whole rotation.
func (e *upstreamError) failureKind() core.FailureKind {
	if e == nil {
		return core.FailureUpstream
	}
	if e.Code == codeRiskControl || e.Status == http.StatusMethodNotAllowed {
		return core.FailureWAF
	}
	switch e.Kind {
	case kindRisk:
		return core.FailureWAF
	case kindExhausted:
		return core.FailureQuota
	case kindRateLimit, kindConcurrency:
		return core.FailureRateLimited
	case kindInvalid:
		return core.FailureAuth
	case kindModel:
		// "this credential is not entitled to this model" is a property of the
		// request, not of the account, so another account cannot fix it -- and
		// the shared vocabulary has no model kind.  FailureOther is the one
		// kind the gateway never rotates, which is exactly the requirement.
		return core.FailureOther
	case kindCaptcha:
		// 3007, or a 403 that says captcha: the request carried no usable
		// verification parameter.  classify() deliberately keeps this apart
		// from a rejected credential, and a fresh parameter is minted per
		// request, so the next attempt may well clear it: transport-level.
		return core.FailureUpstream
	}
	return core.FailureUpstream
}

// classifyEnvelope reports a failure hidden inside an HTTP 200 body (3006 and
// 3009 both arrive that way).  It returns nil for a healthy payload.
func classifyEnvelope(status int, body []byte) *upstreamError {
	if status >= 400 {
		return classify(status, body)
	}
	var probe struct {
		Code  *int            `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return nil
	}
	if probe.Code != nil && *probe.Code != 0 {
		return classify(status, body)
	}
	if len(probe.Error) > 0 {
		trimmed := strings.TrimSpace(string(probe.Error))
		if trimmed != "" && trimmed != "null" {
			return classify(status, body)
		}
	}
	return nil
}

// extractCode reads the ZCode numeric code from either envelope shape.
func extractCode(body []byte) int {
	var doc struct {
		Code *int `json:"code"`
	}
	if json.Unmarshal(body, &doc) == nil && doc.Code != nil {
		return *doc.Code
	}
	return 0
}

// extractMessage digs the most useful human message out of an error body.
func extractMessage(body []byte) string {
	var doc struct {
		Message string          `json:"message"`
		Msg     string          `json:"msg"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &doc) == nil {
		if strings.TrimSpace(doc.Message) != "" {
			return doc.Message
		}
		if strings.TrimSpace(doc.Msg) != "" {
			return doc.Msg
		}
		if len(doc.Error) > 0 {
			var nested struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(doc.Error, &nested) == nil && strings.TrimSpace(nested.Message) != "" {
				return nested.Message
			}
			var plain string
			if json.Unmarshal(doc.Error, &plain) == nil && strings.TrimSpace(plain) != "" {
				return plain
			}
		}
	}
	text := strings.TrimSpace(string(body))
	if len(text) > 300 {
		text = text[:300]
	}
	return text
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Anthropic Messages response
// ---------------------------------------------------------------------------

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// usageToCore converts Anthropic accounting into OpenAI accounting.  Anthropic
// reports cache reads separately from input_tokens, while OpenAI folds them
// into prompt_tokens and additionally reports the cached subset.
func usageToCore(u anthropicUsage) core.Usage {
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	completion := u.OutputTokens
	return core.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      prompt + completion,
		CachedTokens:     u.CacheReadInputTokens,
	}
}

type anthropicContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Signature string          `json:"signature"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Type       string                  `json:"type"`
	Model      string                  `json:"model"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

// finishReason maps Anthropic stop reasons onto OpenAI finish reasons.
func finishReason(stop string) string {
	switch strings.ToLower(strings.TrimSpace(stop)) {
	case "end_turn", "stop_sequence", "stop", "pause_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	case "":
		return ""
	default:
		return "stop"
	}
}

// responseEvents turns a non-streaming Anthropic message into the event
// sequence a core.Stream would have produced.
func responseEvents(raw []byte, model string) ([]core.Event, error) {
	var resp anthropicResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	_ = model

	events := make([]core.Event, 0, len(resp.Content)+2)
	toolIndex := 0
	hasTool := false
	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				events = append(events, core.Event{Type: core.EventDelta, Delta: block.Text})
			}
		case "thinking":
			if block.Thinking != "" {
				events = append(events, core.Event{Type: core.EventDelta, Reasoning: block.Thinking})
			}
		case "tool_use":
			args := "{}"
			if len(block.Input) > 0 && json.Valid(block.Input) {
				args = string(block.Input)
			}
			events = append(events, core.Event{
				Type: core.EventToolCall,
				ToolCall: &core.ToolCallDelta{
					Index:     toolIndex,
					ID:        block.ID,
					Name:      block.Name,
					Arguments: args,
				},
			})
			toolIndex++
			hasTool = true
		}
	}

	usage := usageToCore(resp.Usage)
	events = append(events, core.Event{Type: core.EventUsage, Usage: &usage})

	finish := finishReason(resp.StopReason)
	if finish == "" {
		if hasTool {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	events = append(events, core.Event{Type: core.EventDone, Finish: finish})
	return events, nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// redact removes a credential from a message before it is logged or persisted.
func redact(msg, secret string) string {
	if secret == "" || len(secret) < 8 {
		return msg
	}
	return strings.ReplaceAll(msg, secret, core.MaskSecret(secret))
}

// printableASCII reports whether a value is safe to place in an HTTP header.
func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
