package raccoon

import (
	"bytes"
	"encoding/json"
	"strings"

	"client2api/internal/core"
)

// chatBody is the OpenAI-compatible request body raccoon's
// `/api/web/llm/v2/chat/completions` accepts.
type chatBody struct {
	Model           string          `json:"model"`
	Messages        []chatMessage   `json:"messages"`
	Stream          bool            `json:"stream"`
	MaxTokens       *int            `json:"max_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stop            []string        `json:"stop,omitempty"`
	Tools           []chatTool      `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Thinking        *chatThinking   `json:"thinking,omitempty"`
}

type chatThinking struct {
	Type string `json:"type"`
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
				Parameters:  sanitizeToolParameters(t.Parameters),
			},
		})
	}
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		body.ToolChoice = req.ToolChoice
	}
	applyThinkingControl(&body, reasoningEffort(req))
	return body
}

// emptyToolSchema is the canonical schema for a tool that publishes none.
// SenseNova's validator rejects an absent/empty `parameters` ("is not valid
// under any of the schemas listed in the 'anyOf' keyword"), which is exactly
// what Codex's dynamic and MCP tools can produce.
var emptyToolSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// hasUsableToolSchema reports whether raw is a non-empty JSON object. A missing,
// null or `{}` schema is substituted rather than forwarded.
func hasUsableToolSchema(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return false
	}
	if string(trimmed) == "{}" {
		return false
	}
	return true
}

// sanitizeToolParameters removes regex constructs that SenseNova's tool-schema
// validator refuses even though they are valid JSON Schema. In particular,
// Codex MCP tools may publish a propertyNames pattern containing the
// ECMAScript null escape `\0`; sending it makes the chat endpoint answer HTTP
// 400. Dropping that escape keeps the useful part of the pattern (for example
// "no '=' in an environment key") while leaving runtime validation to the MCP
// tool that owns the schema.
func sanitizeToolParameters(raw json.RawMessage) json.RawMessage {
	if !hasUsableToolSchema(raw) {
		return emptyToolSchema
	}
	if !bytes.Contains(raw, []byte(`\0`)) && !bytes.Contains(raw, []byte{0}) {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var schema any
	if err := dec.Decode(&schema); err != nil {
		return raw
	}
	if !sanitizeToolPatterns(schema) {
		return raw
	}
	out, err := json.Marshal(schema)
	if err != nil {
		return raw
	}
	return out
}

func sanitizeToolPatterns(v any) bool {
	changed := false
	switch node := v.(type) {
	case map[string]any:
		if raw, ok := node["pattern"].(string); ok {
			clean := strings.ReplaceAll(raw, `\0`, "")
			clean = strings.ReplaceAll(clean, "\x00", "")
			if clean != raw {
				node["pattern"] = clean
				changed = true
			}
		}
		for _, child := range node {
			if sanitizeToolPatterns(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range node {
			if sanitizeToolPatterns(child) {
				changed = true
			}
		}
	}
	return changed
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

// applyThinkingControl maps a reasoning effort onto the fields SenseNova
// actually reads. "off" is reasoning_effort=none, not an OpenAI extra_body
// extension; enabling thinking uses the vendor's top-level object.
func applyThinkingControl(body *chatBody, effort string) {
	e := strings.ToLower(strings.TrimSpace(effort))
	if e == "" {
		return
	}
	switch e {
	case "off", "none", "disabled", "disable", "false", "0":
		body.ReasoningEffort = "none"
	default:
		body.Thinking = &chatThinking{Type: "enabled"}
	}
}
