package zcode

import (
	"bytes"
	"encoding/json"
	"strings"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Anthropic Messages wire types
// ---------------------------------------------------------------------------

// anthropicRequest is the request body the upstream expects.  System blocks are
// kept as raw JSON fragments so the identity preamble is transmitted byte for
// byte instead of being re-encoded.
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

// ---------------------------------------------------------------------------
// OpenAI -> Anthropic
// ---------------------------------------------------------------------------

// buildRequestBody translates a normalised core.ChatRequest into the Anthropic
// Messages shape.  Unrepresentable parts are dropped rather than turned into an
// error: a partially understood conversation is still a useful conversation.
func buildRequestBody(req *core.ChatRequest, cfg *Config, acct *Account, model string) (*anthropicRequest, error) {
	out := &anthropicRequest{
		Model:  model,
		Stream: req.Stream,
	}

	out.MaxTokens = defaultMaxTokens
	if cfg.MaxTokensDefault > 0 {
		out.MaxTokens = cfg.MaxTokensDefault
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		out.MaxTokens = *req.MaxTokens
	}

	if req.Temperature != nil {
		v := clamp01(*req.Temperature)
		out.Temperature = &v
	}
	if req.TopP != nil {
		v := clamp01(*req.TopP)
		out.TopP = &v
	}
	for _, s := range req.Stop {
		if s != "" {
			out.StopSequences = append(out.StopSequences, s)
		}
	}

	var systemParts []string
	for i := range req.Messages {
		m := req.Messages[i]
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			if txt := messageText(&m); txt != "" {
				systemParts = append(systemParts, txt)
			}
		case "tool", "function":
			out.Messages = append(out.Messages, anthropicMessage{
				Role: "user",
				Content: []any{map[string]any{
					"type":        "tool_result",
					"tool_use_id": m.ToolCallID,
					"content":     messageText(&m),
				}},
			})
		case "assistant":
			blocks := make([]any, 0, 1+len(m.ToolCalls))
			if txt := messageText(&m); txt != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": txt})
			}
			for _, tc := range m.ToolCalls {
				name := tc.Name
				if name == "" {
					name = tc.Type
				}
				if name == "" {
					continue
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  name,
					"input": toolInput(tc.Arguments),
				})
			}
			if len(blocks) == 0 {
				blocks = append(blocks, map[string]any{"type": "text", "text": ""})
			}
			out.Messages = append(out.Messages, anthropicMessage{Role: "assistant", Content: blocks})
		default:
			out.Messages = append(out.Messages, anthropicMessage{Role: "user", Content: userBlocks(&m)})
		}
	}
	if len(out.Messages) == 0 {
		out.Messages = []anthropicMessage{{Role: "user", Content: []any{map[string]any{"type": "text", "text": ""}}}}
	}

	out.System = buildSystem(cfg, systemParts, model)

	if tools := buildTools(req.Tools); len(tools) > 0 {
		out.Tools = tools
		out.ToolChoice = buildToolChoice(req.ToolChoice, &out.Tools)
	}

	if cfg.injectCacheControl() {
		markLastCacheControl(out.Messages)
	}

	if acct != nil && acct.Mode == modeJWT && acct.UserID != "" {
		out.Metadata = map[string]any{"user_id": acct.UserID}
	}

	return out, nil
}

// buildSystem assembles the system blocks: the mandatory identity preamble
// first, then the dynamic model statement, then whatever the caller supplied.
func buildSystem(cfg *Config, userParts []string, model string) []json.RawMessage {
	var sys []json.RawMessage
	if cfg.injectSystemBlocks() {
		sys = append(sys, identityBlocks()...)
		if model != "" {
			sys = append(sys, rawJSON(map[string]any{
				"type":          "text",
				"text":          "- You are powered by the model named " + model + ".",
				"cache_control": map[string]any{"type": "ephemeral"},
			}))
		}
	}
	for _, part := range userParts {
		sys = append(sys, rawJSON(map[string]any{"type": "text", "text": part}))
	}
	return sys
}

func buildTools(tools []core.Tool) []anthropicTool {
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			name = strings.TrimSpace(t.Type)
		}
		if name == "" {
			continue
		}
		schema := t.Parameters
		if len(schema) == 0 || !json.Valid(schema) {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, anthropicTool{
			Name:        name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return out
}

// buildToolChoice maps the OpenAI tool_choice shape onto Anthropic's.
func buildToolChoice(choice json.RawMessage, tools *[]anthropicTool) json.RawMessage {
	raw := strings.TrimSpace(string(choice))
	if raw == "" || raw == "null" {
		return nil
	}
	if strings.HasPrefix(raw, `"`) {
		var s string
		if json.Unmarshal(choice, &s) != nil {
			return nil
		}
		switch s {
		case "none":
			*tools = nil
			return nil
		case "required", "any":
			return json.RawMessage(`{"type":"any"}`)
		default: // "auto"
			return nil
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(choice, &obj) != nil {
		return nil
	}
	if obj.Type == "function" && obj.Function.Name != "" {
		b, err := marshalNoEscape(map[string]any{"type": "tool", "name": obj.Function.Name})
		if err != nil {
			return nil
		}
		return json.RawMessage(b)
	}
	return nil
}

// markLastCacheControl appends an ephemeral cache marker to the final content
// block of the last message, mirroring the official client.  Anthropic
// silently ignores the marker below its cache threshold, so it is safe to add
// unconditionally.
func markLastCacheControl(msgs []anthropicMessage) {
	for i := len(msgs) - 1; i >= 0; i-- {
		blocks := msgs[i].Content
		if len(blocks) == 0 {
			continue
		}
		last, ok := blocks[len(blocks)-1].(map[string]any)
		if !ok {
			continue
		}
		if _, exists := last["cache_control"]; exists {
			return
		}
		last["cache_control"] = map[string]any{"type": "ephemeral"}
		return
	}
}

// ---------------------------------------------------------------------------
// message helpers
// ---------------------------------------------------------------------------

// messageText flattens a message's textual content.
func messageText(m *core.Message) string {
	if m == nil {
		return ""
	}
	if strings.TrimSpace(m.Content) != "" {
		return m.Content
	}
	var parts []string
	for _, p := range m.Parts {
		if p.Type == "text" || p.Type == "" {
			if p.Text != "" {
				parts = append(parts, p.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// userBlocks turns a user message into Anthropic content blocks.  Image URLs
// can only be forwarded when they are inline data URLs; remote links are
// dropped (Anthropic has no way to fetch them).
func userBlocks(m *core.Message) []any {
	blocks := make([]any, 0, len(m.Parts)+1)
	if len(m.Parts) == 0 {
		return []any{map[string]any{"type": "text", "text": m.Content}}
	}
	for _, p := range m.Parts {
		switch p.Type {
		case "text", "":
			if p.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
			}
		case "image_url":
			if b := imageBlock(p.ImageURL); b != nil {
				blocks = append(blocks, b)
			}
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
	}
	return blocks
}

func imageBlock(url string) map[string]any {
	if !strings.HasPrefix(url, "data:") {
		return nil
	}
	head, b64, found := strings.Cut(url, ",")
	if !found || b64 == "" {
		return nil
	}
	mediaType := strings.TrimPrefix(head, "data:")
	if semi := strings.Index(mediaType, ";"); semi >= 0 {
		mediaType = mediaType[:semi]
	}
	if mediaType == "" {
		mediaType = "image/png"
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       b64,
		},
	}
}

// toolInput converts an OpenAI arguments string into an Anthropic input object.
func toolInput(args string) any {
	s := strings.TrimSpace(args)
	if s == "" {
		return map[string]any{}
	}
	if json.Valid([]byte(s)) {
		var v map[string]any
		if err := json.Unmarshal([]byte(s), &v); err == nil && v != nil {
			return v
		}
	}
	return map[string]any{"_raw": args}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// ---------------------------------------------------------------------------
// encoding helpers
// ---------------------------------------------------------------------------

// marshalNoEscape encodes without HTML escaping so the transmitted bytes match
// the source text exactly.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// rawJSON marshals a value into a raw fragment, degrading to an empty object.
func rawJSON(v any) json.RawMessage {
	b, err := marshalNoEscape(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(b)
}
