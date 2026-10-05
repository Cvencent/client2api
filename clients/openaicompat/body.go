package openaicompat

import (
	"encoding/json"
	"fmt"
	"strings"

	"client2api/internal/core"
)

// buildChatBody converts a gateway request into the OpenAI-compatible wire
// shape for one provider.  The model id on the wire is the unqualified form.
func buildChatBody(cfg Config, prov ProviderConfig, model string, req *core.ChatRequest) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil chat request", core.ErrUnsupported)
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("%w: a chat request needs a model id", core.ErrUnsupported)
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("%w: a chat request needs at least one message", core.ErrUnsupported)
	}

	body := oaiChatRequest{
		Model:       model,
		Messages:    make([]oaiMessage, 0, len(req.Messages)),
		Stream:      true,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
		User:        strings.TrimSpace(req.User),
	}
	for _, m := range req.Messages {
		body.Messages = append(body.Messages, wireMessage(m))
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		limit := *req.MaxTokens
		if cfg.usesLegacyMaxTokens() {
			body.MaxTokens = &limit
		} else {
			body.MaxCompletionTokens = &limit
		}
	}
	if tools := wireTools(req.Tools); len(tools) > 0 {
		body.Tools = tools
		body.ToolChoice = normalizeToolChoice(req.ToolChoice)
	}

	opts := req.Options
	if v, ok := optionFloat(opts, "presence_penalty"); ok {
		body.PresencePenalty = &v
	}
	if v, ok := optionFloat(opts, "frequency_penalty"); ok {
		body.FrequencyPenalty = &v
	}
	if v, ok := optionInt(opts, "top_k"); ok {
		body.TopK = &v
	}
	if v, ok := optionInt(opts, "seed"); ok {
		body.Seed = &v
	}
	if v, ok := optionBool(opts, "parallel_tool_calls"); ok {
		body.ParallelToolCalls = &v
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai-compat: encode request: %w", err)
	}
	return raw, nil
}

// --- request wire types ------------------------------------------------------

type oaiFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
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

type oaiImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

type oaiContentPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *oaiImageURL `json:"image_url,omitempty"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    any           `json:"content,omitempty"`
	Name       string        `json:"name,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	Reasoning  string        `json:"reasoning,omitempty"`
}

type oaiChatRequest struct {
	Model               string          `json:"model"`
	Messages            []oaiMessage    `json:"messages"`
	Stream              bool            `json:"stream"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	TopK                *int            `json:"top_k,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Stop                []string        `json:"stop,omitempty"`
	Tools               []oaiTool       `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	User                string          `json:"user,omitempty"`
	Seed                *int            `json:"seed,omitempty"`
	PresencePenalty     *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64        `json:"frequency_penalty,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
}

// wireMessage converts one gateway message.
func wireMessage(m core.Message) oaiMessage {
	out := oaiMessage{
		Role:       strings.TrimSpace(m.Role),
		Name:       strings.TrimSpace(m.Name),
		ToolCallID: strings.TrimSpace(m.ToolCallID),
		Reasoning:  strings.TrimSpace(m.Reasoning),
	}
	if out.Role == "" {
		out.Role = "user"
	}
	out.Content = wireContent(m)
	for _, tc := range m.ToolCalls {
		if strings.TrimSpace(tc.Name) == "" {
			continue
		}
		call := oaiToolCall{
			ID:   strings.TrimSpace(tc.ID),
			Type: strings.TrimSpace(tc.Type),
			Function: oaiFunction{
				Name:      strings.TrimSpace(tc.Name),
				Arguments: tc.Arguments,
			},
		}
		if call.Type == "" {
			call.Type = "function"
		}
		out.ToolCalls = append(out.ToolCalls, call)
	}
	return out
}

// wireContent prefers a plain string; the array form is used only when an
// image is present.
func wireContent(m core.Message) any {
	if len(m.Parts) == 0 {
		return m.Content
	}
	hasImage := false
	for _, p := range m.Parts {
		if isImagePart(p) {
			hasImage = true
			break
		}
	}
	if !hasImage {
		parts := make([]string, 0, len(m.Parts))
		for _, p := range m.Parts {
			if t := strings.TrimSpace(p.Text); t != "" {
				parts = append(parts, t)
			}
		}
		if len(parts) == 0 {
			return m.Content
		}
		return strings.Join(parts, "\n")
	}
	out := make([]oaiContentPart, 0, len(m.Parts))
	for _, p := range m.Parts {
		if isImagePart(p) {
			out = append(out, oaiContentPart{
				Type:     "image_url",
				ImageURL: &oaiImageURL{URL: p.ImageURL, Detail: p.Detail},
			})
			continue
		}
		out = append(out, oaiContentPart{Type: "text", Text: p.Text})
	}
	return out
}

func isImagePart(p core.ContentPart) bool {
	if strings.EqualFold(strings.TrimSpace(p.Type), "image_url") {
		return p.ImageURL != ""
	}
	return false
}

func wireTools(in []core.Tool) []oaiTool {
	out := make([]oaiTool, 0, len(in))
	for _, t := range in {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		kind := strings.TrimSpace(t.Type)
		if kind == "" {
			kind = "function"
		}
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, oaiTool{
			Type: kind,
			Function: oaiToolDef{
				Name:        name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out
}

func normalizeToolChoice(raw json.RawMessage) json.RawMessage {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == `""` {
		return nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if strings.TrimSpace(asString) == "" {
			return nil
		}
	}
	return raw
}

// --- response wire types ------------------------------------------------------

type oaiUsage struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	TotalTokens             int `json:"total_tokens"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *oaiUsage) toCore() *core.Usage {
	if u == nil {
		return nil
	}
	out := &core.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	if u.PromptTokensDetails != nil {
		out.CachedTokens = u.PromptTokensDetails.CachedTokens
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	return out
}

type oaiDelta struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Reasoning string          `json:"reasoning"`
	Refusal   string          `json:"refusal"`
	ToolCalls []oaiToolCall   `json:"tool_calls"`
}

func (d oaiDelta) textOf() string      { return flattenContent(d.Content) }
func (d oaiDelta) reasoningOf() string { return d.Reasoning }

func flattenContent(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(raw, &str); err == nil {
			return str
		}
		return ""
	}
	var parts []oaiContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

type oaiChoice struct {
	Index        int      `json:"index"`
	Delta        oaiDelta `json:"delta"`
	Message      oaiDelta `json:"message"`
	FinishReason *string  `json:"finish_reason"`
}

type oaiChunkError struct {
	Code    any    `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type"`
}

type oaiChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Model   string         `json:"model"`
	Choices []oaiChoice    `json:"choices"`
	Usage   *oaiUsage      `json:"usage"`
	Error   *oaiChunkError `json:"error"`
}

func normalizeFinish(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "tool_calls", "tool_call", "function_call":
		return "tool_calls"
	case "length", "max_tokens":
		return "length"
	case "content_filter":
		return "content_filter"
	case "stop", "end_turn", "error", "":
		return "stop"
	default:
		return "stop"
	}
}

func isDonePayload(data string) bool {
	return strings.EqualFold(strings.TrimSpace(data), "[DONE]")
}

// parseModelsBody decodes `{"data":[...]}`.  A model with no id is skipped.
func parseModelsBody(body []byte) ([]core.Model, error) {
	var env struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("openai-compat: model list: %w", err)
	}
	out := make([]core.Model, 0, len(env.Data))
	seen := make(map[string]bool, len(env.Data))
	for _, m := range env.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, core.Model{ID: id, OwnedBy: "", Extra: map[string]any{}})
	}
	return out, nil
}

// --- option helpers -----------------------------------------------------------

func optionFloat(opts map[string]any, key string) (float64, bool) {
	if opts == nil {
		return 0, false
	}
	v, ok := opts[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		var f float64
		if err := json.Unmarshal([]byte(n), &f); err != nil {
			return 0, false
		}
		return f, true
	}
	return 0, false
}

func optionInt(opts map[string]any, key string) (int, bool) {
	f, ok := optionFloat(opts, key)
	if !ok {
		return 0, false
	}
	return int(f), true
}

func optionBool(opts map[string]any, key string) (bool, bool) {
	if opts == nil {
		return false, false
	}
	v, ok := opts[key]
	if !ok {
		return false, false
	}
	switch b := v.(type) {
	case bool:
		return b, true
	case string:
		switch strings.ToLower(strings.TrimSpace(b)) {
		case "true", "1", "yes":
			return true, true
		case "false", "0", "no":
			return false, true
		}
	}
	return false, false
}
