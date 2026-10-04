package opencode

import (
	"encoding/json"
	"fmt"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// The OpenAI chat-completions wire shape.
//
// This is not a guess about what each model wants: Zen's own server
// (packages/console/app/src/routes/zen/util/provider/provider.ts) is built on a
// neutral OpenAI-shaped intermediate — CommonRequest / CommonChunk /
// CommonResponse — and converts it to each model's native protocol
// (Anthropic Messages, Google generateContent, the Responses API, Jev's
// /systemone) on the way out and back.  So this module speaks exactly the
// neutral shape, and the field names below mirror the CommonRequest interface
// in that file.
// ---------------------------------------------------------------------------

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

// oaiImageURL mirrors CommonContentPart.image_url, which is `{url: string}` and
// carries no `detail`: Zen's neutral shape has no such field, so a caller's
// Detail is dropped rather than sent where it would be ignored or rejected.
type oaiImageURL struct {
	URL string `json:"url"`
}

type oaiContentPart struct {
	Type     string       `json:"type"`
	Text     string       `json:"text,omitempty"`
	ImageURL *oaiImageURL `json:"image_url,omitempty"`
}

type oaiMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content,omitempty"`
	Name    string `json:"name,omitempty"`
	// ToolCallID carries the id of the tool call a `tool` message answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ReasoningContent is accepted by the module on the way out so a caller can
	// replay a reasoning turn, but Zen's CommonMessage has no such field and its
	// converters drop unknown keys, so it may never reach the model.
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []oaiToolCall `json:"tool_calls,omitempty"`
}

type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiChatRequest struct {
	Model    string       `json:"model"`
	Messages []oaiMessage `json:"messages"`
	// Stream is always true: the module re-aggregates for a non-streaming
	// caller, which gives one code path and lets the vendor's usage frame be
	// read either way.
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

// defaultToolParameters is what a tool with no declared parameters is sent as:
// the vendor needs an object schema, and an absent one is a 400.
var defaultToolParameters = json.RawMessage(`{"type":"object","properties":{}}`)

// freeTierTools is the anonymous free-tier handshake.  Zen's console serves
// the *-free catalogue only to a request that looks like the vendor CLI's,
// and the part it inspects is the tool list: a streaming request declaring
// functions named bash and read is accepted, anything else comes back as
// FreeTierError.  The names are what matter; the schemas below are
// deliberately inert so a model is not tempted to call the placeholder.
var freeTierTools = []oaiTool{
	{
		Type: "function",
		Function: oaiToolDef{
			Name:        "bash",
			Description: "Reserved for gateway client identification. Never call this tool.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		},
	},
	{
		Type: "function",
		Function: oaiToolDef{
			Name:        "read",
			Description: "Reserved for gateway client identification. Never call this tool.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		},
	},
}

// ensureFreeTierTools appends whichever handshake tools the caller's own list
// is missing.  A caller that already declared bash or read keeps its own
// definition instead of gaining a duplicate name.
func ensureFreeTierTools(tools []oaiTool) []oaiTool {
	present := make(map[string]bool, len(tools)+len(freeTierTools))
	for _, t := range tools {
		present[t.Function.Name] = true
	}
	for _, want := range freeTierTools {
		if present[want.Function.Name] {
			continue
		}
		present[want.Function.Name] = true
		tools = append(tools, want)
	}
	return tools
}

// buildChatBody renders a core.ChatRequest as the JSON Zen expects.
//
// It validates and fails before any account is taken, so a request this module
// cannot express never costs an upstream call or a cooldown.
func buildChatBody(cfg Config, req *core.ChatRequest) ([]byte, error) {
	return buildChatBodyWith(cfg, req, false)
}

// buildFreeChatBody renders the same request carrying the anonymous free-tier
// signature.  Zen refuses its *-free catalogue to a bare request and answers
// FreeTierError("can only be used from within OpenCode"), so a request bound
// for the `public` credential has to look like the vendor's own CLI's.
func buildFreeChatBody(cfg Config, req *core.ChatRequest) ([]byte, error) {
	return buildChatBodyWith(cfg, req, true)
}

// buildChatBodyWith is the shared builder; free selects the anonymous
// free-tier handshake.
func buildChatBodyWith(cfg Config, req *core.ChatRequest, free bool) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil request", core.ErrUnsupported)
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("%w: messages are required", core.ErrUnsupported)
	}

	msgs := make([]oaiMessage, 0, len(req.Messages))
	for i, m := range req.Messages {
		wire, err := wireMessage(m, i)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, wire)
	}

	tools := make([]oaiTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		params := t.Parameters
		if len(params) == 0 || string(params) == "null" {
			params = defaultToolParameters
		}
		tools = append(tools, oaiTool{
			Type: firstNonEmpty(t.Type, "function"),
			Function: oaiToolDef{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}

	if free {
		tools = ensureFreeTierTools(tools)
	}

	out := oaiChatRequest{
		Model:       req.Model,
		Messages:    msgs,
		Stream:      true,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Stop:        req.Stop,
		User:        req.User,
	}
	if cfg.IncludeUsage {
		out.StreamOptions = &oaiStreamOptions{IncludeUsage: true}
	}
	if len(tools) > 0 {
		out.Tools = tools
		out.ToolChoice = normalizeToolChoice(req.ToolChoice)
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("%s: encoding request: %w", clientName, err)
	}
	return body, nil
}

// wireMessage converts one core.Message.
//
// `developer` is folded into `system` because that is the OpenAI convention and
// Zen's CommonMessage role union is system|user|assistant|tool.  Any other role
// is refused: silently relabelling it would change the meaning of the
// conversation.
func wireMessage(msg core.Message, index int) (oaiMessage, error) {
	role := firstNonEmpty(msg.Role, "user")
	switch role {
	case "developer":
		role = "system"
	case "system", "user", "assistant", "tool":
	default:
		return oaiMessage{}, fmt.Errorf("%w: unknown role %q (message %d)",
			core.ErrUnsupported, msg.Role, index)
	}

	out := oaiMessage{
		Role:             role,
		Name:             msg.Name,
		ToolCallID:       msg.ToolCallID,
		ReasoningContent: msg.Reasoning,
	}

	switch {
	case len(msg.Parts) > 0:
		parts := make([]oaiContentPart, 0, len(msg.Parts))
		for _, p := range msg.Parts {
			switch p.Type {
			case "text":
				parts = append(parts, oaiContentPart{Type: "text", Text: p.Text})
			case "image_url":
				if p.ImageURL == "" {
					return oaiMessage{}, fmt.Errorf(
						"%w: image part without a url (message %d)", core.ErrUnsupported, index)
				}
				parts = append(parts, oaiContentPart{
					Type:     "image_url",
					ImageURL: &oaiImageURL{URL: p.ImageURL},
				})
			default:
				return oaiMessage{}, fmt.Errorf("%w: unknown content part %q (message %d)",
					core.ErrUnsupported, p.Type, index)
			}
		}
		out.Content = parts
	case msg.Content != "":
		out.Content = msg.Content
	}

	if len(msg.ToolCalls) > 0 {
		calls := make([]oaiToolCall, 0, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			calls = append(calls, oaiToolCall{
				ID:       tc.ID,
				Type:     firstNonEmpty(tc.Type, "function"),
				Function: oaiFunction{Name: tc.Name, Arguments: tc.Arguments},
			})
		}
		out.ToolCalls = calls
	}

	return out, nil
}

// normalizeToolChoice drops the values Zen's neutral shape cannot carry.
//
// CommonRequest.tool_choice is `"auto" | "required" | {type:"function",
// function:{name}}` — there is no `"none"`, so a caller asking for no tools
// gets the field omitted instead of a value the converter would mangle.
func normalizeToolChoice(raw json.RawMessage) json.RawMessage {
	trimmed := trimJSON(raw)
	if trimmed == "" || trimmed == "null" || trimmed == `"none"` {
		return nil
	}
	return raw
}

// trimJSON trims surrounding whitespace and reports "" for an absent value.
func trimJSON(raw json.RawMessage) string {
	s := string(raw)
	start, end := 0, len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
