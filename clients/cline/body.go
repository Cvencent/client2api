package cline

import (
	"encoding/json"
	"strconv"
	"strings"

	"client2api/internal/core"
)

// --- wire shapes -----------------------------------------------------------
//
// The Cline chat endpoint is a STANDARD OpenAI /chat/completions shape, so
// there is no envelope to translate: the request and the SSE chunks both look
// like OpenAI's.

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
	Role             string        `json:"role"`
	Content          any           `json:"content"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	Name             string        `json:"name,omitempty"`
	ToolCalls        []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string        `json:"tool_call_id,omitempty"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiChatRequest struct {
	Model           string            `json:"model"`
	Messages        []oaiMessage      `json:"messages"`
	Stream          bool              `json:"stream"`
	StreamOptions   *oaiStreamOptions `json:"stream_options,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	TopP            *float64          `json:"top_p,omitempty"`
	MaxTokens       *int              `json:"max_tokens,omitempty"`
	Stop            []string          `json:"stop,omitempty"`
	Tools           []oaiTool         `json:"tools,omitempty"`
	ToolChoice      json.RawMessage   `json:"tool_choice,omitempty"`
	User            string            `json:"user,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
}

type oaiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
	Param   string `json:"param,omitempty"`
}

type oaiDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	Reasoning        string          `json:"reasoning,omitempty"`
	ToolCalls        []oaiToolCall   `json:"tool_calls,omitempty"`
}

type oaiChoice struct {
	Index        int         `json:"index"`
	Delta        oaiDelta    `json:"delta"`
	Message      *oaiMessage `json:"message,omitempty"`
	Text         string      `json:"text,omitempty"`
	FinishReason *string     `json:"finish_reason"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	PromptDetails    *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

// toCore maps the vendor's usage onto the shared shape, deriving the total when
// only its parts arrived.
func (u oaiUsage) toCore() core.Usage {
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
	if u.PromptDetails != nil {
		out.CachedTokens = u.PromptDetails.CachedTokens
	}
	if u.CompletionDetails != nil {
		out.ReasoningTokens = u.CompletionDetails.ReasoningTokens
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	return out
}

type oaiChunk struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   *oaiUsage   `json:"usage,omitempty"`
	Error   *oaiError   `json:"error,omitempty"`
}

// --- request building ------------------------------------------------------

// buildBody renders one /chat/completions request.
//
// defaultMaxTokens is the module's configured budget, applied when the caller
// sent none.  The model's own advertised ceiling is applied by the gateway
// through core.ModelLimitsProvider before the request reaches here.
func buildBody(req *core.ChatRequest, defaultMaxTokens int, defaultEffort string) ([]byte, error) {
	msgs, err := toWireMessages(req)
	if err != nil {
		return nil, err
	}
	out := oaiChatRequest{
		Model:         req.Model,
		Messages:      msgs,
		Stream:        true,
		StreamOptions: &oaiStreamOptions{IncludeUsage: true},
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		Stop:          req.Stop,
		User:          req.User,
	}
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		n := *req.MaxTokens
		out.MaxTokens = &n
	} else if defaultMaxTokens > 0 {
		n := defaultMaxTokens
		out.MaxTokens = &n
	}
	if tools, choice, ok := buildTools(req); ok {
		out.Tools = tools
		out.ToolChoice = choice
	}
	// reasoning_effort is always sent: omitting it means the model does not
	// think at all, which is never what a caller of this module wants by
	// default.  The vendor VALIDATES the value — an unknown tier fails the
	// whole request with `reasoning_effort: Invalid option: expected one of
	// "max"|"xhigh"|"high"|"medium"|"low"|"minimal"|"none"` — so the table in
	// reasoningEffort is load-bearing and its values are exactly the vendor's,
	// lower case.  An unrecognised tier from the caller is passed through
	// unchanged rather than rewritten, so the operator sees that message.
	if effort := reasoningEffort(req.Options, defaultEffort); effort != "" {
		out.ReasoningEffort = effort
	}
	return json.Marshal(out)
}

// toWireMessages translates the conversation.  Images are passed through as
// data-URL content parts: unlike the qwenwork envelope, this endpoint is plain
// OpenAI, so it has somewhere to put them.
func toWireMessages(req *core.ChatRequest) ([]oaiMessage, error) {
	out := make([]oaiMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		wire := oaiMessage{
			Role:             m.Role,
			Name:             m.Name,
			ToolCallID:       m.ToolCallID,
			ReasoningContent: m.Reasoning,
		}
		if len(m.Parts) > 0 {
			parts := make([]oaiContentPart, 0, len(m.Parts))
			for _, p := range m.Parts {
				switch p.Type {
				case "image_url":
					if strings.TrimSpace(p.ImageURL) == "" {
						continue
					}
					parts = append(parts, oaiContentPart{
						Type:     "image_url",
						ImageURL: &oaiImageURL{URL: p.ImageURL, Detail: p.Detail},
					})
				default:
					if p.Text == "" {
						continue
					}
					parts = append(parts, oaiContentPart{Type: "text", Text: p.Text})
				}
			}
			if len(parts) == 1 && parts[0].Type == "text" {
				wire.Content = parts[0].Text
			} else if len(parts) > 0 {
				wire.Content = parts
			} else {
				wire.Content = m.Content
			}
		} else {
			wire.Content = m.Content
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]oaiToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				typ := tc.Type
				if typ == "" {
					typ = "function"
				}
				calls = append(calls, oaiToolCall{
					ID:       tc.ID,
					Type:     typ,
					Function: oaiFunction{Name: tc.Name, Arguments: tc.Arguments},
				})
			}
			wire.ToolCalls = calls
		}
		out = append(out, wire)
	}
	return out, nil
}

// buildTools translates the tool definitions.  A tool_choice of "none"
// suppresses them entirely rather than sending an empty list.
func buildTools(req *core.ChatRequest) ([]oaiTool, json.RawMessage, bool) {
	if len(req.Tools) == 0 || toolsSuppressed(req.ToolChoice) {
		return nil, nil, false
	}
	out := make([]oaiTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		typ := t.Type
		if typ == "" {
			typ = "function"
		}
		out = append(out, oaiTool{
			Type:     typ,
			Function: oaiToolDef{Name: t.Name, Description: t.Description, Parameters: params},
		})
	}
	if len(out) == 0 {
		return nil, nil, false
	}
	return out, req.ToolChoice, true
}

// toolsSuppressed reports whether a tool_choice of "none" was sent.
func toolsSuppressed(choice json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(choice))
	return trimmed == `"none"` || trimmed == "null" || trimmed == ""
}

// --- reasoning effort ------------------------------------------------------

// reasoningEffortTable maps the caller-facing tier to the wire value.
//
// The vendor VALIDATES this field and rejects anything outside its own set with
// `reasoning_effort: Invalid option: expected one of
// "max"|"xhigh"|"high"|"medium"|"low"|"minimal"|"none"`, failing the whole
// stream.  The spelling is therefore load-bearing: the wire values are
// lower-case and are not the tier names this module used to send ("High",
// "Extra", …), and every request with the built-in default used to fail.
//
// The remote catalogue never publishes per-model tiers, so one table serves
// every model.
var reasoningEffortTable = map[string]string{
	"none":    "none",
	"minimal": "minimal",
	"low":     "low",
	"medium":  "medium",
	"high":    "high",
	"xhigh":   "xhigh",
	"max":     "max",
}

// defaultReasoningEffort is the tier used when neither the request nor the
// config names one.  It is never "" -- omitting reasoning_effort means the model
// does not think at all.
const defaultReasoningEffort = "high"

// reasoningEffort resolves the wire value for one request: the per-request
// option first, then the configured default, then the built-in default.
//
// The value is passed through verbatim when it is not a known tier.  The vendor
// then refuses the request with an explicit message naming the accepted set, so
// the operator sees their own typo rather than a silent rewrite to some other
// tier they never asked for.
func reasoningEffort(options map[string]any, configured string) string {
	for _, key := range []string{"reasoning_effort", "reasoningEffort", "reasoning", "effort"} {
		v, ok := options[key]
		if !ok {
			continue
		}
		s := optionString(v)
		if s == "" {
			continue
		}
		return mapEffort(s)
	}
	if configured != "" {
		return mapEffort(configured)
	}
	return mapEffort(defaultReasoningEffort)
}

// mapEffort applies the table, passing an unknown tier through unchanged.
func mapEffort(tier string) string {
	tier = strings.TrimSpace(tier)
	if tier == "" {
		return ""
	}
	if wire, ok := reasoningEffortTable[strings.ToLower(tier)]; ok {
		return wire
	}
	return tier
}

// optionString renders a ChatRequest.Options value as a string.  The options map
// is decoded from arbitrary JSON, so every plausible spelling is accepted.
func optionString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	case map[string]any:
		return firstNonEmpty(pickString(t, "type", "name", "value", "effort"))
	}
	return ""
}
