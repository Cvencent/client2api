package gateway

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Inbound OpenAI wire types.  Only the fields client2api understands are
// declared; unknown fields are ignored on purpose so that new OpenAI options
// never break the gateway.
// ---------------------------------------------------------------------------

type chatMessage struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content,omitempty"`
	Name             string          `json:"name,omitempty"`
	ToolCalls        []chatToolCall  `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
}

type chatToolCall struct {
	Index    *int   `json:"index,omitempty"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

type chatRequest struct {
	Model               string          `json:"model"`
	Messages            []chatMessage   `json:"messages"`
	Tools               []chatTool      `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage,omitempty"`
	} `json:"stream_options,omitempty"`
	User    string         `json:"user,omitempty"`
	Options map[string]any `json:"client2api,omitempty"`

	// Thinking level.  Three spellings reach this endpoint: the flat
	// reasoning_effort a codex++ relay sends, the Responses-shaped reasoning
	// object, and the client2api options key.  Both fields decode leniently
	// so a caller that sends some other shape still gets a working request
	// instead of a 400.  See reasoningEffortFor.
	ReasoningEffort effortField `json:"reasoning_effort,omitempty"`
	Reasoning       effortField `json:"reasoning,omitempty"`

	// Conversation identity.  Clients that follow the OpenAI convention carry
	// it in the metadata object; others put it at the top level.  Both
	// spellings are accepted and resolved once, here, so that no module has to
	// guess which one arrived.
	ConversationID      string          `json:"conversation_id,omitempty"`
	ConversationIDCamel string          `json:"conversationId,omitempty"`
	Metadata            json.RawMessage `json:"metadata,omitempty"`
}

// ---------------------------------------------------------------------------
// Outbound OpenAI wire types
// ---------------------------------------------------------------------------

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

type chatChoice struct {
	Index        int          `json:"index"`
	Message      *chatMessage `json:"message,omitempty"`
	Delta        *chatMessage `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

type promptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type completionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type chatUsage struct {
	PromptTokens            int                      `json:"prompt_tokens"`
	CompletionTokens        int                      `json:"completion_tokens"`
	TotalTokens             int                      `json:"total_tokens"`
	PromptTokensDetails     *promptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *completionTokensDetails `json:"completion_tokens_details,omitempty"`
}

func toWireUsage(u *core.Usage) *chatUsage {
	if u == nil {
		return nil
	}
	out := &chatUsage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	if u.CachedTokens > 0 {
		out.PromptTokensDetails = &promptTokensDetails{CachedTokens: u.CachedTokens}
	}
	if u.ReasoningTokens > 0 {
		out.CompletionTokensDetails = &completionTokensDetails{ReasoningTokens: u.ReasoningTokens}
	}
	return out
}

type modelList struct {
	Object string       `json:"object"`
	Data   []modelEntry `json:"data"`
}

type modelEntry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// ContextLength is the resolved input window in tokens, if any layer knows
	// it. OpenAI's own object has no such field, but OpenRouter, vLLM and most
	// self-hosted gateways publish it top-level, and that is the field an
	// OpenAI-compatible client reads to size its context instead of guessing.
	ContextLength int64 `json:"context_length,omitempty"`
	// MaxOutputTokens is the resolved per-response output cap, if any layer
	// knows it. Zero means unknown and the key is omitted.
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
	// Extra carries the module's per-model facts that have no home in the
	// OpenAI model object: the credit multiplier, the currently effective
	// promotion, the effort ladder, the capability flags.  It is omitted
	// entirely when a module publishes none, so a plain catalogue still looks
	// exactly like the OpenAI one.
	Extra map[string]any `json:"extra,omitempty"`
}

type apiErrorEnvelope struct {
	Error apiError `json:"error"`
}

type apiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code,omitempty"`
	// GatewayHint is one actionable sentence for the operator, filled from
	// internal/hint when the failure carries advice worth acting on.  It sits
	// beside message rather than inside it so a client that parses `message`
	// sees byte-for-byte what the module said.
	GatewayHint string `json:"gateway_hint,omitempty"`
}

// ---------------------------------------------------------------------------
// Request translation
// ---------------------------------------------------------------------------

// effortField decodes a thinking level without ever failing the surrounding
// request.  A level arrives either as a bare string ("xhigh") or wrapped in
// the Responses shape ({"effort":"xhigh"}); anything else, including JSON
// null, reads as "" rather than turning the whole body into a 400.
type effortField string

func (e *effortField) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*e = effortField(s)
		return nil
	}
	var wrapped struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(b, &wrapped); err == nil {
		*e = effortField(wrapped.Effort)
	}
	return nil
}

// reasoningEffortFor is the thinking level a request asked for, normalised to
// the lower-case vocabulary the vendors use, or "" when it named none.
//
// The flat field wins because that is what the codex++ relay in front of this
// deployment sends.  Next comes the client2api options object, which is this
// gateway's documented channel and what an API caller sets deliberately; last
// the Responses-shaped object, which is an incidental copy.  An empty value
// never short-circuits the search, so a caller that sends a blank flat field
// alongside a real options value still reads as the value it meant.
func reasoningEffortFor(w *chatRequest) string {
	if w == nil {
		return ""
	}
	if v := normalizeEffort(string(w.ReasoningEffort)); v != "" {
		return v
	}
	if w.Options != nil {
		if s, ok := w.Options["reasoning_effort"].(string); ok {
			if v := normalizeEffort(s); v != "" {
				return v
			}
		}
	}
	return normalizeEffort(string(w.Reasoning))
}

func normalizeEffort(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// resolveConversationID finds the caller's conversation id.  The metadata
// object is checked before the top level, and the snake_case spelling before
// the camelCase one.
//
// It deliberately does NOT fall back to "user".  X-Conversation-ID names a
// conversation, and substituting an end-user id would make the vendor's
// per-conversation accounting aggregate across unrelated chats.
func resolveConversationID(w *chatRequest) string {
	if w == nil {
		return ""
	}
	if len(w.Metadata) > 0 {
		var meta struct {
			ConversationID      string `json:"conversation_id"`
			ConversationIDCamel string `json:"conversationId"`
		}
		// A malformed metadata object is not fatal: the request is still
		// perfectly serviceable without a conversation id.
		if err := json.Unmarshal(w.Metadata, &meta); err == nil {
			if v := strings.TrimSpace(meta.ConversationID); v != "" {
				return v
			}
			if v := strings.TrimSpace(meta.ConversationIDCamel); v != "" {
				return v
			}
		}
	}
	if v := strings.TrimSpace(w.ConversationID); v != "" {
		return v
	}
	return strings.TrimSpace(w.ConversationIDCamel)
}

func toCoreRequest(w *chatRequest) (*core.ChatRequest, error) {
	req := &core.ChatRequest{
		Model:          w.Model,
		ToolChoice:     w.ToolChoice,
		Temperature:    w.Temperature,
		TopP:           w.TopP,
		MaxTokens:      w.MaxTokens,
		Stop:           parseStop(w.Stop),
		Stream:         w.Stream,
		User:           w.User,
		ConversationID: resolveConversationID(w),
		Options:        optionsWithReasoningEffort(w.Options, reasoningEffortFor(w)),
	}
	if req.MaxTokens == nil {
		req.MaxTokens = w.MaxCompletionTokens
	}
	if len(w.Messages) == 0 {
		return nil, fmt.Errorf("messages must not be empty")
	}
	for i, m := range w.Messages {
		if strings.TrimSpace(m.Role) == "" {
			return nil, fmt.Errorf("messages[%d].role is required", i)
		}
		text, parts, err := parseContent(m.Content)
		if err != nil {
			return nil, fmt.Errorf("messages[%d].content: %w", i, err)
		}
		msg := core.Message{
			Role:       m.Role,
			Content:    text,
			Parts:      parts,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
			Reasoning:  m.ReasoningContent,
		}
		for _, tc := range m.ToolCalls {
			t := core.ToolCall{ID: tc.ID, Type: tc.Type, Name: tc.Function.Name, Arguments: tc.Function.Arguments}
			if t.Type == "" {
				t.Type = "function"
			}
			msg.ToolCalls = append(msg.ToolCalls, t)
		}
		req.Messages = append(req.Messages, msg)
	}
	for _, t := range w.Tools {
		if t.Type != "" && t.Type != "function" {
			// Non-function tools are not representable upstream; skip rather
			// than fail, so a mixed request still works.
			continue
		}
		req.Tools = append(req.Tools, core.Tool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		})
	}
	return req, nil
}

func optionsWithReasoningEffort(options map[string]any, effort string) map[string]any {
	if effort == "" {
		return options
	}
	out := make(map[string]any, len(options)+1)
	for k, v := range options {
		out[k] = v
	}
	out["reasoning_effort"] = effort
	return out
}

// parseContent accepts either the string form or the array-of-parts form and
// returns the flattened text plus the parts in order.
func parseContent(raw json.RawMessage) (string, []core.ContentPart, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", nil, nil
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return "", nil, err
		}
		return s, nil, nil
	case '[':
		var parts []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL *struct {
				URL    string `json:"url"`
				Detail string `json:"detail"`
			} `json:"image_url"`
		}
		if err := json.Unmarshal(trimmed, &parts); err != nil {
			return "", nil, err
		}
		var sb strings.Builder
		out := make([]core.ContentPart, 0, len(parts))
		for _, p := range parts {
			switch p.Type {
			case "text", "input_text", "":
				sb.WriteString(p.Text)
				out = append(out, core.ContentPart{Type: "text", Text: p.Text})
			case "image_url":
				if p.ImageURL != nil {
					out = append(out, core.ContentPart{
						Type:     "image_url",
						ImageURL: p.ImageURL.URL,
						Detail:   p.ImageURL.Detail,
					})
				}
			}
		}
		return sb.String(), out, nil
	default:
		return "", nil, fmt.Errorf("must be a string or an array of parts")
	}
}

// parseStop accepts "stop" or ["stop", ...].
func parseStop(raw json.RawMessage) []string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	switch trimmed[0] {
	case '"':
		var s string
		if json.Unmarshal(trimmed, &s) == nil && s != "" {
			return []string{s}
		}
	case '[':
		var ss []string
		if json.Unmarshal(trimmed, &ss) == nil {
			return ss
		}
	}
	return nil
}

// newID returns an OpenAI-shaped object id.
func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return prefix + "fallback"
	}
	return prefix + hex.EncodeToString(b[:])
}
