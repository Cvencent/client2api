package minimaxcode

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"client2api/internal/core"
)

// Failure kinds this module can distinguish.  They are internal labels: the
// gateway owns the wire vocabulary (error.code), and core.Failure carries the
// vendor's own words in the message.
const (
	kindModel     = "model"
	kindInvalid   = "invalid"
	kindRateLimit = "rate-limit"
	kindExhausted = "exhausted"
	kindRisk      = "risk"
	kindUpstream  = "upstream"
	kindOther     = "other"
)

// Vendor codes seen on this upstream.  They live only inside the message text;
// the wire vocabulary stays the gateway's.
const (
	codeTokenRequired = 401
	codeNoRoute       = 50115 // "direct_route_not_configured"
)

// upstreamError is one classified rejection from the vendor.
type upstreamError struct {
	Status int
	Code   int
	Kind   string
	Msg    string
}

func (e *upstreamError) Error() string { return e.short() }

func (e *upstreamError) short() string {
	if e.Code != 0 {
		return fmt.Sprintf("HTTP %d code %d: %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Msg)
}

// failureKind maps the vendor's verdict onto the core vocabulary.
//
// The one decision worth spelling out is kindModel: a model the vendor will not
// route is not fixed by trying another account, so it must NOT rotate.  Every
// other kind here is retryable, which is what makes rotation useful.
func (e *upstreamError) failureKind() core.FailureKind {
	switch e.Kind {
	case kindModel:
		return core.FailureOther
	case kindInvalid:
		return core.FailureAuth
	case kindRateLimit:
		return core.FailureRateLimited
	case kindExhausted:
		return core.FailureQuota
	case kindRisk:
		return core.FailureWAF
	case kindUpstream:
		return core.FailureUpstream
	default:
		return core.FailureOther
	}
}

// classify turns a status code and body into a verdict.  Status comes first on
// purpose: a 429 that happens to mention "balance" is a rate limit, not an
// exhausted credential, and treating it as the latter would park a good account
// for hours.
func classify(status int, body []byte) *upstreamError {
	msg := extractMessage(body)
	code := extractCode(body)

	switch {
	case status == 401:
		return &upstreamError{Status: status, Code: code, Kind: kindInvalid, Msg: msg}
	case status == 402:
		return &upstreamError{Status: status, Code: code, Kind: kindExhausted, Msg: msg}
	case status == 403:
		if containsAny(strings.ToLower(msg), riskWords) {
			return &upstreamError{Status: status, Code: code, Kind: kindRisk, Msg: msg}
		}
		return &upstreamError{Status: status, Code: code, Kind: kindInvalid, Msg: msg}
	case status == 429:
		return &upstreamError{Status: status, Code: code, Kind: kindRateLimit, Msg: msg}
	case status == 400 || status == 404 || status == 422:
		if containsAny(strings.ToLower(msg), modelWords) || code == codeNoRoute {
			return &upstreamError{Status: status, Code: code, Kind: kindModel, Msg: msg}
		}
		return &upstreamError{Status: status, Code: code, Kind: kindOther, Msg: msg}
	case status >= 500:
		return &upstreamError{Status: status, Code: code, Kind: kindUpstream, Msg: msg}
	}

	lower := strings.ToLower(msg)
	switch {
	case containsAny(lower, exhaustedWords):
		return &upstreamError{Status: status, Code: code, Kind: kindExhausted, Msg: msg}
	case containsAny(lower, riskWords):
		return &upstreamError{Status: status, Code: code, Kind: kindRisk, Msg: msg}
	case containsAny(lower, modelWords):
		return &upstreamError{Status: status, Code: code, Kind: kindModel, Msg: msg}
	default:
		return &upstreamError{Status: status, Code: code, Kind: kindOther, Msg: msg}
	}
}

// classifyEnvelope catches a failure hidden inside an HTTP 200.  MiniMax Code
// reports its own errors in base_resp.status_code, and the Anthropic envelope
// uses a top-level "error" member, so both shapes are checked.
func classifyEnvelope(status int, body []byte) *upstreamError {
	var doc struct {
		Type     string          `json:"type"`
		Error    json.RawMessage `json:"error"`
		BaseResp struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil
	}
	if doc.BaseResp.StatusCode != 0 {
		msg := strings.TrimSpace(doc.BaseResp.StatusMsg)
		if msg == "" {
			msg = "the upstream reported a non-zero base_resp.status_code"
		}
		return classifyEnvelopeCode(status, doc.BaseResp.StatusCode, msg)
	}
	if len(doc.Error) > 0 && string(doc.Error) != "null" {
		msg := extractMessage(body)
		return &upstreamError{Status: status, Kind: kindOther, Msg: msg}
	}
	if doc.Type == "error" {
		return &upstreamError{Status: status, Kind: kindOther, Msg: extractMessage(body)}
	}
	return nil
}

// classifyEnvelopeCode maps the vendor's own in-body status code.
func classifyEnvelopeCode(status, code int, msg string) *upstreamError {
	lower := strings.ToLower(msg)
	kind := kindOther
	switch {
	case code == 401 || code == 1004:
		kind = kindInvalid
	case containsAny(lower, exhaustedWords):
		kind = kindExhausted
	case containsAny(lower, riskWords):
		kind = kindRisk
	case containsAny(lower, modelWords):
		kind = kindModel
	}
	return &upstreamError{Status: status, Code: code, Kind: kind, Msg: msg}
}

var (
	exhaustedWords = []string{"insufficient", "balance", "quota", "exhaust", "no resource", "recharge", "欠费", "余额不足"}
	riskWords      = []string{"unusual activity", "risk", "blocked", "风控", "异常"}
	modelWords     = []string{"model", "not found", "no such", "unsupported", "not configured", "does not exist", "invalid_model"}
)

// extractCode finds the vendor's numeric code in any of the shapes this
// upstream uses.
func extractCode(body []byte) int {
	var doc struct {
		Code      int `json:"code"`
		ErrorCode int `json:"errorCode"`
		Error     struct {
			Code int `json:"code"`
		} `json:"error"`
		BaseResp struct {
			StatusCode int `json:"status_code"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0
	}
	switch {
	case doc.BaseResp.StatusCode != 0:
		return doc.BaseResp.StatusCode
	case doc.ErrorCode != 0:
		return doc.ErrorCode
	case doc.Error.Code != 0:
		return doc.Error.Code
	default:
		return doc.Code
	}
}

// extractMessage pulls the most human-readable message out of a body, trying
// the shapes in the order they appear on this upstream, and truncating so a
// megabyte of HTML never reaches a log line or the panel.
func extractMessage(body []byte) string {
	var doc struct {
		Message     string          `json:"message"`
		Msg         string          `json:"msg"`
		ErrorReason string          `json:"errorReason"`
		Error       json.RawMessage `json:"error"`
		BaseResp    struct {
			StatusMsg string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return truncate(strings.TrimSpace(string(body)), 300)
	}
	if s := strings.TrimSpace(doc.BaseResp.StatusMsg); s != "" {
		return truncate(s, 300)
	}
	if s := strings.TrimSpace(doc.Message); s != "" {
		return truncate(s, 300)
	}
	if s := strings.TrimSpace(doc.Msg); s != "" {
		return truncate(s, 300)
	}
	if len(doc.Error) > 0 && string(doc.Error) != "null" {
		var nested struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		}
		if err := json.Unmarshal(doc.Error, &nested); err == nil {
			if s := strings.TrimSpace(nested.Message); s != "" {
				return truncate(s, 300)
			}
			if s := strings.TrimSpace(nested.Type); s != "" {
				return truncate(s, 300)
			}
		}
		var plain string
		if err := json.Unmarshal(doc.Error, &plain); err == nil && strings.TrimSpace(plain) != "" {
			return truncate(strings.TrimSpace(plain), 300)
		}
	}
	if s := strings.TrimSpace(doc.ErrorReason); s != "" {
		return truncate(s, 300)
	}
	return truncate(strings.TrimSpace(string(body)), 300)
}

func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// anthropicUsage is the token accounting both the non-stream body and the SSE
// frames use.
type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func usageToCore(u anthropicUsage) core.Usage {
	prompt := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	return core.Usage{
		PromptTokens:     prompt,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      prompt + u.OutputTokens,
		CachedTokens:     u.CacheReadInputTokens,
	}
}

// anthropicContentBlock is one entry of a Messages response's content array.
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
	Role       string                  `json:"role"`
	Model      string                  `json:"model"`
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

// finishReason maps an Anthropic stop_reason onto the OpenAI vocabulary the
// gateway speaks.
func finishReason(stop string) string {
	switch stop {
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

// responseEvents turns a buffered non-streaming body into the same event
// sequence a stream would have produced, so the gateway has exactly one code
// path for both.
func responseEvents(raw []byte, model string) ([]core.Event, error) {
	var resp anthropicResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("minimaxcode: cannot decode the upstream response: %w", err)
	}

	events := make([]core.Event, 0, len(resp.Content)*2+2)
	toolIndex := 0
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
			if len(block.Input) > 0 {
				args = string(block.Input)
			}
			events = append(events, core.Event{
				Type:     core.EventToolCall,
				ToolCall: &core.ToolCallDelta{Index: toolIndex, ID: block.ID, Name: block.Name, Arguments: args},
			})
			toolIndex++
		default:
			// Unknown block kinds are skipped, never fatal: a block this
			// module does not model is still not a reason to fail a reply
			// that already contains usable text.
		}
	}

	usage := usageToCore(resp.Usage)
	events = append(events, core.Event{Type: core.EventUsage, Usage: &usage})

	finish := finishReason(resp.StopReason)
	if finish == "" {
		finish = "stop"
	}
	events = append(events, core.Event{Type: core.EventDone, Finish: finish})
	return events, nil
}

// readLimited drains a body up to max bytes.  Every read in this module is
// bounded: an upstream that answers a 400 with a gigabyte must not become this
// process's memory problem.
func readLimited(r io.Reader, max int64) []byte {
	if r == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(r, max))
	if err != nil && len(b) == 0 {
		return nil
	}
	return b
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// printableASCII reports whether a header value is safe to put on the wire.
func printableASCII(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// errUpstream is a small helper so callers can build a failure from a
// classified verdict without repeating the wrapping.
func errUpstream(ue *upstreamError) error {
	if ue == nil {
		return errors.New("minimaxcode: upstream error")
	}
	return ue
}
