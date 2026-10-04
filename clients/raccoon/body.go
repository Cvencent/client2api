package raccoon

import (
	"encoding/json"
	"strings"

	"client2api/internal/core"
)

// chatBody is the OpenAI-compatible request body raccoon's
// `/api/web/llm/v2/chat/completions` accepts.
type chatBody struct {
	Model       string          `json:"model"`
	Messages    []chatMessage   `json:"messages"`
	Stream      bool            `json:"stream"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        []string        `json:"stop,omitempty"`
	Tools       []chatTool      `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	ExtraBody   map[string]any  `json:"extra_body,omitempty"`
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          any            `json:"content,omitempty"`
	Name             string         `json:"name,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type chatToolCall struct {
	ID       string                   `json:"id,omitempty"`
	Type     string                   `json:"type"`
	Function chatToolCallFunctionBody `json:"function"`
}

type chatToolCallFunctionBody struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// buildChatBody translates a core.ChatRequest into the vendor's shape.
//
// `tools` MUST sit at the TOP LEVEL of the body: omitting it makes the model
// invent XML tool calls in prose instead of calling a function.
func buildChatBody(req *core.ChatRequest, model string, stream bool) chatBody {
	body := chatBody{
		Model:       model,
		Messages:    make([]chatMessage, 0, len(req.Messages)),
		Stream:      stream,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
	}
	for _, m := range req.Messages {
		cm := chatMessage{
			Role:             strings.TrimSpace(m.Role),
			Content:          messageContent(m),
			Name:             m.Name,
			ToolCallID:       m.ToolCallID,
			ReasoningContent: m.Reasoning,
		}
		if cm.Role == "" {
			cm.Role = "user"
		}
		for _, tc := range m.ToolCalls {
			typ := strings.TrimSpace(tc.Type)
			if typ == "" {
				typ = "function"
			}
			cm.ToolCalls = append(cm.ToolCalls, chatToolCall{
				ID:   tc.ID,
				Type: typ,
				Function: chatToolCallFunctionBody{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			})
		}
		body.Messages = append(body.Messages, cm)
	}
	for _, t := range req.Tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		typ := strings.TrimSpace(t.Type)
		if typ == "" {
			typ = "function"
		}
		body.Tools = append(body.Tools, chatTool{
			Type: typ,
			Function: chatFunction{
				Name:        name,
				Description: t.Description,
				Parameters:  t.Parameters,
			},
		})
	}
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		body.ToolChoice = req.ToolChoice
	}
	body.ExtraBody = extraBodyFor(reasoningEffort(req))
	return body
}

// messageContent renders a message body as either a plain string or an
// OpenAI-style content-part array.
func messageContent(m core.Message) any {
	if len(m.Parts) == 0 {
		return m.Content
	}
	parts := make([]map[string]any, 0, len(m.Parts)+1)
	if strings.TrimSpace(m.Content) != "" {
		parts = append(parts, map[string]any{"type": "text", "text": m.Content})
	}
	for _, p := range m.Parts {
		switch strings.ToLower(strings.TrimSpace(p.Type)) {
		case "", "text":
			if p.Text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": p.Text})
			}
		case "image_url", "image":
			url := strings.TrimSpace(p.ImageURL)
			if url == "" {
				continue
			}
			img := map[string]any{"url": url}
			if d := strings.TrimSpace(p.Detail); d != "" {
				img["detail"] = d
			}
			parts = append(parts, map[string]any{"type": "image_url", "image_url": img})
		}
	}
	if len(parts) == 0 {
		return m.Content
	}
	return parts
}

// reasoningEffort reads the caller's thinking control out of Options.
func reasoningEffort(req *core.ChatRequest) string {
	if req == nil || req.Options == nil {
		return ""
	}
	for _, k := range []string{"reasoning_effort", "thinking", "reasoning"} {
		v, ok := req.Options[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			return strings.TrimSpace(t)
		case bool:
			if t {
				return "on"
			}
			return "off"
		case map[string]any:
			if s, ok := t["type"].(string); ok {
				switch strings.ToLower(strings.TrimSpace(s)) {
				case "enabled", "enable", "on":
					return "on"
				case "disabled", "disable", "off":
					return "off"
				}
			}
		}
	}
	return ""
}

// extraBodyFor maps a reasoning effort onto the vendor's thinking dialect.
//
// Thinking control ONLY works inside `extra_body`; a top-level `thinking`
// field is silently ignored by the server. Effort "off" disables thinking and
// anything else (including "on") enables it. An absent effort sends no
// `extra_body` at all, which leaves the server's own default in place.
func extraBodyFor(effort string) map[string]any {
	e := strings.ToLower(strings.TrimSpace(effort))
	if e == "" {
		return nil
	}
	typ := "enabled"
	if e == "off" {
		typ = "disabled"
	}
	return map[string]any{"thinking": map[string]any{"type": typ}}
}
