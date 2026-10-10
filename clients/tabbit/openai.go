package tabbit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// The sidecar speaks OpenAI on its chat face.  These are the wire shapes this
// module produces and tolerates.  They were written from the documented
// surface (docs/upstream/tabbit.md), not copied from the upstream sources.
// ---------------------------------------------------------------------------

type oaiFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type oaiToolCall struct {
	ID       string      `json:"id,omitempty"`
	Type     string      `json:"type,omitempty"`
	Index    *int        `json:"index,omitempty"`
	Function oaiFunction `json:"function"`
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
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL    string `json:"url"`
		Detail string `json:"detail,omitempty"`
	} `json:"image_url,omitempty"`
}

type oaiMessage struct {
	Role             string        `json:"role"`
	Content          any           `json:"content,omitempty"`
	Name             string        `json:"name,omitempty"`
	ToolCalls        []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

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

// ---------------------------------------------------------------------------
// Responses / SSE frames
// ---------------------------------------------------------------------------

type oaiError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    any    `json:"code,omitempty"`
}

type oaiDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
	ToolCalls        []oaiToolCall   `json:"tool_calls,omitempty"`
}

type oaiChoice struct {
	Index        int       `json:"index"`
	Delta        *oaiDelta `json:"delta,omitempty"`
	Message      *oaiDelta `json:"message,omitempty"`
	Text         string    `json:"text,omitempty"` // legacy completions shape
	FinishReason *string   `json:"finish_reason,omitempty"`
}

type oaiPromptDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type oaiCompletionDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// Anthropic-flavoured aliases, tolerated in case they leak through the
	// sidecar's /v1/messages face.
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`

	PromptTokensDetails     *oaiPromptDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *oaiCompletionDetails `json:"completion_tokens_details,omitempty"`
}

func (u *oaiUsage) toCore() core.Usage {
	out := core.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if out.PromptTokens == 0 {
		out.PromptTokens = u.InputTokens
	}
	if out.CompletionTokens == 0 {
		out.CompletionTokens = u.OutputTokens
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

type oaiChunk struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   *oaiUsage   `json:"usage"`
	Error   *oaiError   `json:"error"`
}

// ---------------------------------------------------------------------------
// Request building
// ---------------------------------------------------------------------------

// buildChatBody renders a canonical request as the sidecar's OpenAI body.  The
// model id is re-prefixed ("priority" -> "tabbit/priority") and the upstream
// call is always streamed, because this module re-frames SSE into core.Events
// regardless of what the caller asked for.
func buildChatBody(cfg Config, req *core.ChatRequest) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil request", core.ErrUnsupported)
	}
	body := oaiChatRequest{
		Model:       addModelPrefix(req.Model, cfg.ModelPrefix),
		Stream:      true,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Stop:        req.Stop,
		User:        req.User,
	}
	if body.Model == "" {
		return nil, fmt.Errorf("%w: model is required", core.ErrUnsupported)
	}
	if cfg.includeUsage() {
		body.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
	}
	for _, m := range req.Messages {
		msg := oaiMessage{
			Role:             m.Role,
			Name:             m.Name,
			ToolCallID:       m.ToolCallID,
			ReasoningContent: m.Reasoning,
		}
		if len(m.Parts) > 0 {
			msg.Content = toWireParts(m.Parts)
		} else {
			msg.Content = m.Content
		}
		for _, tc := range m.ToolCalls {
			typ := tc.Type
			if typ == "" {
				typ = "function"
			}
			msg.ToolCalls = append(msg.ToolCalls, oaiToolCall{
				ID:       tc.ID,
				Type:     typ,
				Function: oaiFunction{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
		body.Messages = append(body.Messages, msg)
	}
	for _, t := range req.Tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		params := t.Parameters
		if len(bytes.TrimSpace(params)) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		body.Tools = append(body.Tools, oaiTool{
			Type: "function",
			Function: oaiToolDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	if len(body.Tools) > 0 {
		body.ToolChoice = req.ToolChoice
	}
	return json.Marshal(body)
}

func toWireParts(parts []core.ContentPart) []oaiContentPart {
	out := make([]oaiContentPart, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "image_url":
			part := oaiContentPart{Type: "image_url"}
			part.ImageURL = &struct {
				URL    string `json:"url"`
				Detail string `json:"detail,omitempty"`
			}{URL: p.ImageURL, Detail: p.Detail}
			out = append(out, part)
		default:
			out = append(out, oaiContentPart{Type: "text", Text: p.Text})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Decoding helpers
// ---------------------------------------------------------------------------

// flattenContent accepts either the string form or the array-of-parts form that
// OpenAI-shaped servers use for streamed content.
func flattenContent(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	switch trimmed[0] {
	case '"':
		var s string
		if json.Unmarshal(trimmed, &s) != nil {
			return ""
		}
		return s
	case '[':
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(trimmed, &parts) != nil {
			return ""
		}
		var sb strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "text", "input_text", "output_text", "":
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// normalizeFinish maps whatever the upstream calls a stop into one of the four
// OpenAI finish reasons the core is allowed to emit.
func normalizeFinish(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "":
		return ""
	case "stop", "end_turn", "stop_sequence", "eos", "complete", "completed":
		return "stop"
	case "length", "max_tokens", "max_output_tokens", "max_completion_tokens":
		return "length"
	case "tool_calls", "tool_use", "tool_call", "function_call":
		return "tool_calls"
	case "content_filter", "safety", "refusal":
		return "content_filter"
	default:
		// An unknown reason must still be a legal OpenAI finish reason.
		return "stop"
	}
}

// isDonePayload reports whether an SSE data payload terminates the stream.
func isDonePayload(data string) bool {
	d := strings.TrimSpace(data)
	return strings.EqualFold(d, "[DONE]") || strings.EqualFold(d, "[done]")
}
