package minimaxcode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"client2api/internal/core"
)

// anthropicRequest is the Messages body this upstream accepts.  MiniMax Code
// posts this shape natively -- it is not a translation layer the vendor runs,
// it is the wire format, which is why the field names are Anthropic's.
type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        []json.RawMessage  `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	Metadata      map[string]any     `json:"metadata,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content []any  `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// buildRequestBody rewrites an OpenAI-shaped request into the Messages body.
//
// It is deliberately lossy-tolerant: a message shape this module does not model
// is dropped rather than turned into an error, because a partially understood
// conversation is still a useful conversation.  What it will not do is invent
// a field the vendor has not been observed to accept.
func buildRequestBody(req *core.ChatRequest, cfg *Config, acct *Account, model string) (*anthropicRequest, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: empty request", core.ErrUnsupported)
	}

	maxTokens := cfg.MaxTokensDefault
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}

	out := &anthropicRequest{
		Model:     model,
		MaxTokens: maxTokens,
		Stream:    req.Stream,
		Messages:  make([]anthropicMessage, 0, len(req.Messages)),
	}

	var system []string
	for i := range req.Messages {
		m := &req.Messages[i]
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			if text := messageText(m); text != "" {
				system = append(system, text)
			}
		case "user":
			blocks := userBlocks(m)
			if len(blocks) == 0 {
				continue
			}
			out.Messages = append(out.Messages, anthropicMessage{Role: "user", Content: blocks})
		case "tool", "function":
			// A tool result is a user-role message carrying a tool_result
			// block; there is no "tool" role in the Messages API.
			block := map[string]any{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     messageText(m),
			}
			out.Messages = append(out.Messages, anthropicMessage{Role: "user", Content: []any{block}})
		case "assistant":
			blocks := assistantBlocks(m)
			if len(blocks) == 0 {
				continue
			}
			out.Messages = append(out.Messages, anthropicMessage{Role: "assistant", Content: blocks})
		default:
			// Unknown role: treat as user text so the turn is not lost.
			if text := messageText(m); text != "" {
				out.Messages = append(out.Messages, anthropicMessage{Role: "user", Content: []any{map[string]any{"type": "text", "text": text}}})
			}
		}
	}

	if len(out.Messages) == 0 {
		return nil, fmt.Errorf("%w: the request has no message the upstream can accept", core.ErrUnsupported)
	}

	for _, s := range system {
		out.System = append(out.System, rawJSON(map[string]any{"type": "text", "text": s}))
	}

	if req.Temperature != nil {
		v := clamp01(*req.Temperature)
		out.Temperature = &v
	}
	if req.TopP != nil {
		v := clamp01(*req.TopP)
		out.TopP = &v
	}
	if len(req.Stop) > 0 {
		out.StopSequences = append([]string(nil), req.Stop...)
	}
	if tools := buildTools(req.Tools); len(tools) > 0 {
		out.Tools = tools
		if tc := buildToolChoice(req.ToolChoice); len(tc) > 0 {
			out.ToolChoice = tc
		}
	}
	if req.User != "" {
		out.Metadata = map[string]any{"user_id": req.User}
	}
	return out, nil
}

func buildTools(tools []core.Tool) []anthropicTool {
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		schema := t.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, anthropicTool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	return out
}

// buildToolChoice converts the OpenAI tool_choice into the Messages equivalent.
// "none" is expressed by omitting tools entirely, which the caller does not do
// here -- an unsupported choice is dropped rather than guessed at.
func buildToolChoice(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return json.RawMessage(`{"type":"auto"}`)
		case "required", "any":
			return json.RawMessage(`{"type":"any"}`)
		default:
			return nil
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if name := strings.TrimSpace(obj.Function.Name); name != "" {
			return rawJSON(map[string]any{"type": "tool", "name": name})
		}
	}
	return nil
}

// assistantBlocks renders an assistant turn, including any tool calls it made.
func assistantBlocks(m *core.Message) []any {
	blocks := make([]any, 0, len(m.ToolCalls)+2)
	if text := messageText(m); text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": text})
	}
	for _, tc := range m.ToolCalls {
		if strings.TrimSpace(tc.Name) == "" {
			continue
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    tc.ID,
			"name":  tc.Name,
			"input": toolInput(tc.Arguments),
		})
	}
	return blocks
}

// userBlocks renders a user turn, including any inline images.
func userBlocks(m *core.Message) []any {
	blocks := make([]any, 0, len(m.Parts)+1)
	text := strings.TrimSpace(m.Content)
	if text != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
	}
	for _, p := range m.Parts {
		switch strings.ToLower(strings.TrimSpace(p.Type)) {
		case "text", "":
			if p.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
			}
		case "image_url", "image":
			if b := imageBlock(p.ImageURL); b != nil {
				blocks = append(blocks, b)
			}
		}
	}
	return blocks
}

// imageBlock accepts only an inline data: URL.  A remote URL would have to be
// fetched by this process and re-encoded, which would make the relay a proxy
// for arbitrary fetches; the vendor's own client uploads instead.
func imageBlock(url string) map[string]any {
	url = strings.TrimSpace(url)
	if !strings.HasPrefix(url, "data:") {
		return nil
	}
	rest := url[len("data:"):]
	mediaType, data, ok := strings.Cut(rest, ";base64,")
	if !ok || mediaType == "" || data == "" {
		return nil
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       data,
		},
	}
}

// messageText is the plain-text view of a message: Content when set, otherwise
// the concatenation of its text parts.
func messageText(m *core.Message) string {
	if m == nil {
		return ""
	}
	if strings.TrimSpace(m.Content) != "" {
		return m.Content
	}
	var b strings.Builder
	for _, p := range m.Parts {
		if strings.ToLower(strings.TrimSpace(p.Type)) == "text" || p.Type == "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// toolInput turns the OpenAI arguments string into the object the Messages API
// expects.  A malformed string is passed through as a string rather than
// dropped, so the model still sees what the caller sent.
func toolInput(args string) any {
	args = strings.TrimSpace(args)
	if args == "" {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err == nil {
		return v
	}
	return args
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}

// rawJSON marshals a value that is about to be embedded as a raw message.
func rawJSON(v any) json.RawMessage {
	b, err := marshalNoEscape(v)
	if err != nil {
		return nil
	}
	return b
}

// marshalNoEscape encodes without HTML escaping so a body never contains
// \u003c where the client sent <.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
