package loomy

import (
	"encoding/json"

	"client2api/internal/core"
)

// body.go translates a core.ChatRequest into the vendor's wire body.
//
// /chat/completions is OpenAI-compatible, so this is a translation rather than
// an encoding: the field names are the standard ones and the only vendor
// peculiarity is that the credit multiplier lives inside the model *name*, which
// is a display concern and never appears on the wire.

// wireBody is the request body.  Every optional field is a pointer or an
// omitempty slice so "the caller did not ask for this" and "the caller asked for
// zero" stay distinguishable -- the vendor applies its own default only when the
// field is absent.
type wireBody struct {
	Model           string          `json:"model"`
	Messages        []wireMessage   `json:"messages"`
	Stream          bool            `json:"stream"`
	MaxTokens       *int            `json:"max_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stop            []string        `json:"stop,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Tools           []wireTool      `json:"tools,omitempty"`
	ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

type wirePart struct {
	Type     string     `json:"type"`
	Text     string     `json:"text,omitempty"`
	ImageURL *wireImage `json:"image_url,omitempty"`
}

type wireImage struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

// buildBody renders the request.
//
// The caller is expected to have already resolved req.Model to the vendor's own
// id; an id this module does not recognise is passed through untouched, because
// the vendor is the authority on what it serves.
func buildBody(req *core.ChatRequest) ([]byte, error) {
	body := wireBody{
		Model:           req.Model,
		Messages:        make([]wireMessage, 0, len(req.Messages)),
		Stream:          true,
		MaxTokens:       req.MaxTokens,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		Stop:            req.Stop,
		ReasoningEffort: reasoningEffort(req),
	}

	for _, m := range req.Messages {
		body.Messages = append(body.Messages, wireMessage{
			Role:       m.Role,
			Content:    wireContent(m),
			ToolCalls:  wireToolCalls(m.ToolCalls),
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		})
	}

	for _, t := range req.Tools {
		if t.Name == "" {
			continue
		}
		var tool wireTool
		tool.Type = t.Type
		if tool.Type == "" {
			tool.Type = "function"
		}
		tool.Function.Name = t.Name
		tool.Function.Description = t.Description
		tool.Function.Parameters = t.Parameters
		body.Tools = append(body.Tools, tool)
	}

	// tool_choice is only meaningful alongside tools, and the vendor rejects a
	// bare JSON null, so it is dropped when empty.
	if len(body.Tools) > 0 && len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		body.ToolChoice = req.ToolChoice
	}

	return json.Marshal(body)
}

// wireContent renders a message body.
//
// A message with structured parts becomes a content array (this is how images
// travel); otherwise the plain string is used.  An assistant turn that carries
// only tool calls has no text at all, and `content` is then omitted rather than
// sent as an empty string, which some OpenAI-compatible servers reject.
func wireContent(m core.Message) any {
	if len(m.Parts) > 0 {
		parts := make([]wirePart, 0, len(m.Parts))
		for _, p := range m.Parts {
			if p.Type == "image_url" {
				parts = append(parts, wirePart{
					Type:     "image_url",
					ImageURL: &wireImage{URL: p.ImageURL, Detail: p.Detail},
				})
				continue
			}
			parts = append(parts, wirePart{Type: "text", Text: p.Text})
		}
		return parts
	}
	if m.Content == "" {
		return nil
	}
	return m.Content
}

func wireToolCalls(calls []core.ToolCall) []wireToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]wireToolCall, 0, len(calls))
	for _, call := range calls {
		var w wireToolCall
		w.ID = call.ID
		w.Type = call.Type
		if w.Type == "" {
			w.Type = "function"
		}
		w.Function.Name = call.Name
		w.Function.Arguments = call.Arguments
		out = append(out, w)
	}
	return out
}

// reasoningEffort pulls the caller's requested effort out of the gateway's
// options bag.
//
// The value is forwarded verbatim when present.  It is deliberately not
// validated against the model's advertised tiers: the vendor silently ignores
// reasoning_effort values it does not know (the reference confirms HTTP 200 for
// every spelling it tried), so a mismatch costs nothing, whereas dropping a
// legitimate value would silently change how hard the model thinks.  When the
// caller asked for nothing the field is omitted entirely and the vendor's own
// default applies.
func reasoningEffort(req *core.ChatRequest) string {
	for _, key := range []string{"reasoning_effort", "reasoningEffort", "reasoning", "effort"} {
		value, ok := req.Options[key]
		if !ok {
			continue
		}
		if text, ok := value.(string); ok && text != "" {
			return text
		}
	}
	return ""
}
