package qwenwork

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"client2api/internal/core"
)

// body.go renders the QwenWork agent envelope.
//
// The shape is the one documented in docs/upstream/qwenwork.md.  Where the
// reference implementation sends extra fields (is_retry, aliyun_user_type,
// display_name, model, api_key, url, feature_switches, features, chatPrompt,
// imageUrls) this module does not: the documented envelope is the contract, and
// the README records the divergence so it can be revisited against a live
// account.

// Model aliases.  Anything not listed is passed through unchanged, so a new
// vendor model works without a code change.
var modelAliases = map[string]string{
	"":                    "pro",
	"auto":                "pro",
	"advanced":            "pro",
	"pro":                 "pro",
	"lite":                "flash",
	"flash":               "flash",
	"max":                 "qwen3.8-max-preview",
	"qwen3.8-max":         "qwen3.8-max-preview",
	"qwen3.8-max-preview": "qwen3.8-max-preview",
}

// mapModel resolves a caller's model name onto a vendor model key.
func mapModel(name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	if v, ok := modelAliases[key]; ok {
		return v
	}
	return key
}

// errImageUnsupported is returned when a request carries image parts.  The
// documented envelope has nowhere to put them, and answering without the image
// would be a silent wrong answer, so the request is refused instead.
var errImageUnsupported = errors.New("qwenwork: image input is not supported by this client")

// ---------------------------------------------------------------------------
// envelope types
// ---------------------------------------------------------------------------

type agentEnvelope struct {
	RequestID    string         `json:"request_id"`
	RequestSetID string         `json:"request_set_id"`
	ChatRecordID string         `json:"chat_record_id"`
	SessionID    string         `json:"session_id"`
	Stream       bool           `json:"stream"`
	ChatTask     string         `json:"chat_task"`
	ChatContext  agentContext   `json:"chat_context"`
	IsReply      bool           `json:"is_reply"`
	Source       int            `json:"source"`
	Version      string         `json:"version"`
	AgentID      string         `json:"agent_id"`
	TaskID       string         `json:"task_id"`
	SessionType  string         `json:"session_type"`
	ModelConfig  agentModelCfg  `json:"model_config"`
	System       string         `json:"system"`
	Messages     []agentMessage `json:"messages"`
	Tools        []agentTool    `json:"tools,omitempty"`
	Parameters   map[string]any `json:"parameters"`
	Business     agentBusiness  `json:"business"`
}

type agentContext struct {
	Text  string            `json:"text"`
	Extra agentContextExtra `json:"extra"`
}

type agentContextExtra struct {
	ModelConfig     agentCtxModel `json:"modelConfig"`
	OriginalContent string        `json:"originalContent"`
}

type agentCtxModel struct {
	Key         string `json:"key"`
	IsReasoning bool   `json:"is_reasoning"`
}

type agentModelCfg struct {
	Key            string `json:"key"`
	Format         string `json:"format"`
	IsVL           bool   `json:"is_vl"`
	Source         string `json:"source"`
	MaxInputTokens int    `json:"max_input_tokens"`
}

type agentMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolCalls  []agentToolCall `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type agentToolCall struct {
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Function agentToolCallFunc `json:"function"`
}

type agentToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type agentTool struct {
	Type     string        `json:"type"`
	Function agentToolFunc `json:"function"`
}

type agentToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type agentBusiness struct {
	Product string `json:"product"`
	Type    string `json:"type"`
	Version string `json:"version"`
}

// ---------------------------------------------------------------------------
// building
// ---------------------------------------------------------------------------

// buildBody renders the envelope for one chat request.  defaultMaxTokens is the
// module's configured default, used when the caller did not ask for a limit.
func buildBody(req *core.ChatRequest, modelKey string, defaultMaxTokens int) ([]byte, error) {
	if req == nil {
		return nil, core.ErrUnsupported
	}
	system, messages, lastUser, err := splitMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	if lastUser == "" {
		// The vendor expects a prompt; a tool-result-only turn still needs one.
		lastUser = "ping"
	}

	id := newUUID()
	env := agentEnvelope{
		RequestID:    id,
		RequestSetID: id,
		ChatRecordID: id,
		SessionID:    newUUID(),
		Stream:       true,
		ChatTask:     "FREE_INPUT",
		ChatContext: agentContext{
			Text: lastUser,
			Extra: agentContextExtra{
				ModelConfig:     agentCtxModel{Key: modelKey, IsReasoning: false},
				OriginalContent: lastUser,
			},
		},
		IsReply:     false,
		Source:      1,
		Version:     "3",
		AgentID:     "agent_common",
		TaskID:      "common",
		SessionType: "qoder_work",
		ModelConfig: agentModelCfg{
			Key:            modelKey,
			Format:         "openai",
			IsVL:           true,
			Source:         "system",
			MaxInputTokens: defaultContextLength,
		},
		System:     system,
		Messages:   messages,
		Parameters: buildParameters(req, defaultMaxTokens),
		Business: agentBusiness{
			Product: cosyProduct,
			Type:    cosyBusinessType,
			Version: "1",
		},
	}
	if tools := buildTools(req); len(tools) > 0 {
		env.Tools = tools
	}
	return json.Marshal(env)
}

// splitMessages pulls the system prompt out of the conversation (the vendor
// takes it as a separate field), flattens the rest, and reports the text of the
// last user turn, which the envelope repeats in two places.
func splitMessages(in []core.Message) (string, []agentMessage, string, error) {
	var systemParts []string
	out := make([]agentMessage, 0, len(in))
	lastUser, firstUser := "", ""

	for _, m := range in {
		if hasImage(m) {
			return "", nil, "", errImageUnsupported
		}
		text := messageText(m)
		if m.Role == "system" || m.Role == "developer" {
			if text != "" {
				systemParts = append(systemParts, text)
			}
			continue
		}
		msg := agentMessage{
			Role:       m.Role,
			Content:    text,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, agentToolCall{
				ID:   tc.ID,
				Type: firstNonEmpty(tc.Type, "function"),
				Function: agentToolCallFunc{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			})
		}
		if m.Role == "user" && text != "" {
			if firstUser == "" {
				firstUser = text
			}
			lastUser = text
		}
		out = append(out, msg)
	}
	if lastUser == "" {
		lastUser = firstUser
	}
	return strings.Join(systemParts, "\n\n"), out, lastUser, nil
}

// hasImage reports whether a message carries an image part.
func hasImage(m core.Message) bool {
	for _, p := range m.Parts {
		if p.Type == "image_url" || p.ImageURL != "" {
			return true
		}
	}
	return false
}

// messageText flattens a message's content: the Content field when the caller
// used it, otherwise the text parts in order.
func messageText(m core.Message) string {
	if strings.TrimSpace(m.Content) != "" {
		return m.Content
	}
	var b strings.Builder
	for _, p := range m.Parts {
		if p.Type == "text" || p.Type == "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// buildTools maps the caller's tools onto the vendor's shape.  A tool choice of
// "none" suppresses the list entirely, which is how the reference says "do not
// call tools"; the choice itself is not forwarded.
func buildTools(req *core.ChatRequest) []agentTool {
	if !toolsRequested(req.ToolChoice) || len(req.Tools) == 0 {
		return nil
	}
	out := make([]agentTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		if t.Name == "" {
			continue
		}
		fn := agentToolFunc{Name: t.Name, Description: t.Description}
		if len(t.Parameters) > 0 {
			fn.Parameters = t.Parameters
		}
		out = append(out, agentTool{Type: "function", Function: fn})
	}
	return out
}

// toolsRequested reports whether tool definitions should be sent at all.
func toolsRequested(choice json.RawMessage) bool {
	s := strings.TrimSpace(string(choice))
	switch {
	case s == "" || s == "null":
		return true
	case strings.EqualFold(s, `"none"`):
		return false
	default:
		return true
	}
}

// buildParameters copies the sampling knobs the vendor accepts.  max_tokens is
// always present: the vendor wants a budget, and the model ceiling caps it.
func buildParameters(req *core.ChatRequest, defaultMaxTokens int) map[string]any {
	params := map[string]any{}
	if req.Temperature != nil {
		params["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		params["top_p"] = *req.TopP
	}
	if v, ok := optionFloat(req.Options, "presence_penalty"); ok {
		params["presence_penalty"] = v
	}
	if v, ok := optionFloat(req.Options, "frequency_penalty"); ok {
		params["frequency_penalty"] = v
	}
	max := defaultMaxTokens
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		max = *req.MaxTokens
	}
	if max > maxOutputTokens {
		max = maxOutputTokens
	}
	if max <= 0 {
		max = defaultMaxTokens
	}
	params["max_tokens"] = max
	return params
}

// optionFloat reads a numeric knob out of the caller's option bag, accepting
// the int/float shapes JSON decoding can produce as well as numeric strings,
// which some OpenAI-compatible callers send for these fields.
func optionFloat(opts map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		v, ok := opts[k]
		if !ok {
			continue
		}
		if f, ok := asFloat(v); ok {
			return f, true
		}
	}
	return 0, false
}

// asFloat coerces the scalar shapes a JSON bag can hold into a float64.
func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f, true
		}
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}
