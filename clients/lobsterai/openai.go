package lobsterai

import (
	"encoding/json"
	"fmt"
	"strings"

	"client2api/internal/core"
)

// This file holds the OpenAI-shaped wire types.  The upstream is OpenAI
// compatible, so the shapes are the standard ones; what is not standard is
// everything around them (SSE-only transport, no envelope on the response, no
// image input), and that lives in body.go and sse.go.

type oaiFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type oaiToolCall struct {
	ID       string      `json:"id,omitempty"`
	Type     string      `json:"type,omitempty"`
	Index    *int        `json:"index,omitempty"`
	Function oaiFunction `json:"function,omitempty"`
}

type oaiToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type oaiTool struct {
	Type     string     `json:"type"`
	Function oaiToolDef `json:"function"`
}

type oaiContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

type oaiMessage struct {
	Role             string        `json:"role"`
	Content          any           `json:"content"`
	Name             string        `json:"name,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []oaiToolCall `json:"tool_calls,omitempty"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// oaiChatRequest is the upstream chat body.
//
// Stream is not `omitempty` on purpose: this endpoint is SSE-only and a body
// without the field is a body without stream:true.
type oaiChatRequest struct {
	Model         string            `json:"model"`
	Messages      []oaiMessage      `json:"messages"`
	Stream        bool              `json:"stream"`
	StreamOptions *oaiStreamOptions `json:"stream_options,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	TopP          *float64          `json:"top_p,omitempty"`
	MaxTokens     *int              `json:"max_tokens,omitempty"`
	Stop          []string          `json:"stop,omitempty"`
	Tools         []oaiTool         `json:"tools,omitempty"`
	ToolChoice    json.RawMessage   `json:"tool_choice,omitempty"`
	User          string            `json:"user,omitempty"`
}

// buildChatBody renders a core.ChatRequest into the upstream body.
//
// Two rules are load-bearing and both are asserted by the tests:
//
//   - stream is ALWAYS true.  The upstream answers 500 to stream:false, so the
//     bridge always asks for a stream and re-aggregates when the caller wanted
//     one answer.
//   - tool_choice is normalised away when it means "no tools".  The vendor
//     rejects the empty string and the literal "none"; a map (a forced tool) is
//     passed through untouched.
//
// The request shape is validated here, before any network call, so an
// unsupported request fails identically whether or not the sidecar is up.
func buildChatBody(cfg Config, req *core.ChatRequest) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil request", core.ErrUnsupported)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("%w: model is required", core.ErrUnsupported)
	}

	messages := make([]oaiMessage, 0, len(req.Messages))
	for i := range req.Messages {
		msg, err := wireMessage(req.Messages[i], i)
		if err != nil {
			return nil, err
		}
		messages = append(messages, msg)
	}

	tools := make([]oaiTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		typ := firstNonEmpty(t.Type, "function")
		params := t.Parameters
		if len(strings.TrimSpace(string(params))) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, oaiTool{
			Type: typ,
			Function: oaiToolDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}

	body := oaiChatRequest{
		Model:       req.Model,
		Messages:    messages,
		Stream:      true,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Stop:        req.Stop,
		User:        req.User,
	}
	if len(tools) > 0 {
		body.Tools = tools
		body.ToolChoice = normalizeToolChoice(req.ToolChoice)
	}
	if cfg.IncludeUsage {
		body.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
	}
	return json.Marshal(body)
}

// wireMessage maps one core message.
//
// LobsterAI has no image input at all, so an image part is refused rather than
// silently dropped: a request that quietly loses its attachment produces a
// confident wrong answer, which is worse than an error.
func wireMessage(msg core.Message, index int) (oaiMessage, error) {
	out := oaiMessage{
		Role:             msg.Role,
		Name:             msg.Name,
		ToolCallID:       msg.ToolCallID,
		ReasoningContent: msg.Reasoning,
	}
	for _, part := range msg.Parts {
		if part.Type == "image_url" || part.ImageURL != "" {
			return oaiMessage{}, fmt.Errorf("%w: lobsterai accepts text only (message %d carries an image)", core.ErrUnsupported, index)
		}
	}
	if len(msg.Parts) > 0 {
		parts := make([]oaiContentPart, 0, len(msg.Parts))
		for _, part := range msg.Parts {
			if part.Type != "" && part.Type != "text" {
				return oaiMessage{}, fmt.Errorf("%w: unsupported content part %q in message %d", core.ErrUnsupported, part.Type, index)
			}
			parts = append(parts, oaiContentPart{Type: "text", Text: part.Text})
		}
		out.Content = parts
	} else {
		out.Content = msg.Content
	}
	if len(msg.ToolCalls) > 0 {
		calls := make([]oaiToolCall, 0, len(msg.ToolCalls))
		for _, call := range msg.ToolCalls {
			calls = append(calls, oaiToolCall{
				ID:   call.ID,
				Type: firstNonEmpty(call.Type, "function"),
				Function: oaiFunction{
					Name:      call.Name,
					Arguments: call.Arguments,
				},
			})
		}
		out.ToolCalls = calls
	}
	return out, nil
}

// normalizeToolChoice drops the spellings the vendor rejects and keeps a forced
// tool exactly as the caller wrote it.
func normalizeToolChoice(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	switch trimmed {
	case "", "null", `""`, `"none"`:
		return nil
	default:
		return raw
	}
}

// --- response types ---------------------------------------------------------

type oaiPromptDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type oaiCompletionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// oaiUsage accepts both the OpenAI spelling and the Anthropic aliases some
// gateways emit.
type oaiUsage struct {
	PromptTokens            int                   `json:"prompt_tokens"`
	CompletionTokens        int                   `json:"completion_tokens"`
	TotalTokens             int                   `json:"total_tokens"`
	InputTokens             int                   `json:"input_tokens"`
	OutputTokens            int                   `json:"output_tokens"`
	PromptTokensDetails     *oaiPromptDetails     `json:"prompt_tokens_details"`
	CompletionTokensDetails *oaiCompletionDetails `json:"completion_tokens_details"`
}

// toCore normalises the usage block.
func (u *oaiUsage) toCore() core.Usage {
	if u == nil {
		return core.Usage{}
	}
	out := core.Usage{
		PromptTokens:     firstPositive(u.PromptTokens, u.InputTokens),
		CompletionTokens: firstPositive(u.CompletionTokens, u.OutputTokens),
		TotalTokens:      u.TotalTokens,
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	if u.PromptTokensDetails != nil {
		out.CachedTokens = u.PromptTokensDetails.CachedTokens
		out.CachedTokensKnown = true
	}
	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	return out
}

func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// oaiDelta is both the streaming delta and the non-streaming message, because
// some frames of this upstream put the whole answer in `message`.
type oaiDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
	ToolCalls        []oaiToolCall   `json:"tool_calls,omitempty"`
}

// textOf renders a delta's content, which is either a string or a list of text
// parts depending on how the upstream felt that day.
func (d *oaiDelta) textOf() string {
	if d == nil {
		return ""
	}
	return flattenContent(d.Content)
}

// reasoningOf accepts either spelling of the reasoning channel.
func (d *oaiDelta) reasoningOf() string {
	if d == nil {
		return ""
	}
	return firstNonEmpty(d.ReasoningContent, d.Reasoning)
}

type oaiChoice struct {
	Index        int       `json:"index"`
	Delta        *oaiDelta `json:"delta,omitempty"`
	Message      *oaiDelta `json:"message,omitempty"`
	Text         string    `json:"text,omitempty"`
	FinishReason *string   `json:"finish_reason,omitempty"`
}

type oaiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

type oaiChunk struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   *oaiUsage   `json:"usage"`
	Error   *oaiError   `json:"error"`
}

// flattenContent renders a content field that may be a string or a list of
// {"type":"text","text":...} parts.
func flattenContent(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal([]byte(trimmed), &s); err == nil {
			return s
		}
		return ""
	}
	if trimmed[0] == '[' {
		var parts []oaiContentPart
		if err := json.Unmarshal([]byte(trimmed), &parts); err != nil {
			return ""
		}
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// normalizeFinish maps the vendor's finish reason onto the four the gateway
// understands.  An unknown reason becomes "stop" rather than being dropped: the
// stream must end with something a client can act on.
func normalizeFinish(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "":
		return ""
	case "stop", "end_turn", "eos":
		return "stop"
	case "length", "max_tokens", "max_output_tokens", "max_completion_tokens":
		return "length"
	case "tool_calls", "function_call", "tool_use":
		return "tool_calls"
	case "content_filter", "content_filtered":
		return "content_filter"
	default:
		return "stop"
	}
}

// isDonePayload recognises the stream terminator, with or without the space the
// spec asks for after "data:".
func isDonePayload(data string) bool {
	return strings.EqualFold(strings.TrimSpace(data), "[DONE]")
}
