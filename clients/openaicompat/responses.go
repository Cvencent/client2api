package openaicompat

// The OpenAI Responses wire (/v1/responses) is a second request shape some
// relays expose: agentrouter.org, for example, serves models that answer
// "does not support /v1/chat/completions, please use /v1/responses".  The
// wire carries the same conversation as the chat-completions shape but with
// different names -- input items instead of messages, function_call items
// instead of tool_calls, and a different event stream -- so the module keeps
// it in one file: body building, URL, and the SSE absorption half.

import (
	"encoding/json"
	"fmt"
	"strings"

	"client2api/internal/core"
)

// responsesFormat selects the OpenAI Responses wire; the empty string (the
// zero value) keeps the historical chat-completions wire.
const responsesFormat = "responses"

// normalizeAPIFormat maps the configured api_format onto the module's wire
// switch.  Anything unrecognised falls back to the chat-completions wire,
// which is what every relay has served historically.
func normalizeAPIFormat(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "responses", "openai-responses", "openai_responses":
		return responsesFormat
	default:
		// "", "chat", "openai-chat-completions" and every unknown value.
		return ""
	}
}

// responsesURL is the endpoint a Responses completion is POSTed to.
func (p ProviderConfig) responsesURL() string {
	return strings.TrimRight(p.base(), "/") + "/responses"
}

// --- request -----------------------------------------------------------------

type respTextInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type respImageInput struct {
	Type     string `json:"type"`
	ImageURL string `json:"image_url"`
	Detail   string `json:"detail,omitempty"`
}

// respInputItem is one entry of the input array.  A message item carries
// Role+Content; a function_call item carries CallID+Name+Arguments; a
// function_call_output item carries CallID+Output.  omitempty keeps each
// shape's JSON to exactly the fields its type uses.
type respInputItem struct {
	Type      string `json:"type,omitempty"`
	Role      string `json:"role,omitempty"`
	Content   any    `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

type respTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type respRequest struct {
	Model           string          `json:"model"`
	Instructions    string          `json:"instructions,omitempty"`
	Input           []respInputItem `json:"input"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	Stream          bool            `json:"stream"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Tools           []respTool      `json:"tools,omitempty"`
	ToolChoice      string          `json:"tool_choice,omitempty"`
}

// buildResponsesBody converts a gateway request into the Responses wire shape.
// The model id on the wire is the unqualified form, exactly like the chat wire.
func buildResponsesBody(cfg Config, prov ProviderConfig, model string, req *core.ChatRequest) ([]byte, error) {
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

	body := respRequest{
		Model:       model,
		Stream:      true,
		Temperature: req.Temperature,
		TopP:        req.TopP,
	}
	items := make([]respInputItem, 0, len(req.Messages))
	var instructions []string
	for _, m := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		switch role {
		case "system", "developer":
			// The Responses wire carries the system prompt in the
			// instructions field, the way every coding client sends it.
			if t := strings.TrimSpace(messageTextOf(&m)); t != "" {
				instructions = append(instructions, t)
			}
		case "assistant":
			if t := strings.TrimSpace(messageTextOf(&m)); t != "" {
				items = append(items, respInputItem{Type: "message", Role: "assistant", Content: t})
			}
			for _, tc := range m.ToolCalls {
				name := strings.TrimSpace(tc.Name)
				if name == "" {
					continue
				}
				args := strings.TrimSpace(tc.Arguments)
				if args == "" {
					args = "{}"
				}
				items = append(items, respInputItem{
					Type:      "function_call",
					CallID:    strings.TrimSpace(tc.ID),
					Name:      name,
					Arguments: args,
				})
			}
		case "tool", "function":
			items = append(items, respInputItem{
				Type:   "function_call_output",
				CallID: strings.TrimSpace(m.ToolCallID),
				Output: messageTextOf(&m),
			})
		default:
			items = append(items, respUserItem(&m))
		}
	}
	if len(items) == 0 {
		items = append(items, respInputItem{Type: "message", Role: "user", Content: ""})
	}
	body.Input = items
	body.Instructions = strings.Join(instructions, "\n\n")

	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		limit := *req.MaxTokens
		body.MaxOutputTokens = &limit
	}
	if tools := respWireTools(req.Tools); len(tools) > 0 {
		body.Tools = tools
		// The chat wire downgrades every forced choice to "auto" because
		// relays answer a forced choice with a not-supported error; the
		// Responses wire gets the same treatment.  "none" disables the
		// tools outright and passes through.
		if strings.EqualFold(strings.TrimSpace(string(req.ToolChoice)), `"none"`) {
			body.ToolChoice = "none"
		} else {
			body.ToolChoice = "auto"
		}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai-compat: encode request: %w", err)
	}
	return raw, nil
}

// respUserItem converts one user turn.  Text-only turns use the plain string
// content every implementation accepts; an image forces the typed-part form
// with input_image entries.
func respUserItem(m *core.Message) respInputItem {
	if !messageHasImage(m) {
		return respInputItem{Type: "message", Role: "user", Content: messageTextOf(m)}
	}
	parts := make([]any, 0, len(m.Parts))
	for _, p := range m.Parts {
		if isImagePart(p) {
			parts = append(parts, respImageInput{Type: "input_image", ImageURL: p.ImageURL, Detail: p.Detail})
			continue
		}
		if strings.TrimSpace(p.Text) != "" {
			parts = append(parts, respTextInput{Type: "input_text", Text: p.Text})
		}
	}
	if len(parts) == 0 {
		return respInputItem{Type: "message", Role: "user", Content: messageTextOf(m)}
	}
	return respInputItem{Type: "message", Role: "user", Content: parts}
}

// messageTextOf flattens one gateway message to plain text, mirroring
// wireContent's string path.
func messageTextOf(m *core.Message) string {
	if m == nil {
		return ""
	}
	if len(m.Parts) == 0 {
		return m.Content
	}
	parts := make([]string, 0, len(m.Parts))
	for _, p := range m.Parts {
		if isImagePart(p) {
			continue
		}
		if t := strings.TrimSpace(p.Text); t != "" {
			parts = append(parts, t)
		}
	}
	if len(parts) == 0 {
		return m.Content
	}
	return strings.Join(parts, "\n")
}

func messageHasImage(m *core.Message) bool {
	for _, p := range m.Parts {
		if isImagePart(p) {
			return true
		}
	}
	return false
}

// respWireTools converts tool definitions to the Responses flat shape: the
// chat wire nests name/parameters under "function", the Responses wire lifts
// them to the top level.
func respWireTools(in []core.Tool) []respTool {
	out := make([]respTool, 0, len(in))
	for _, t := range in {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		out = append(out, respTool{
			Type:        "function",
			Name:        name,
			Description: t.Description,
			Parameters:  normalizeToolSchema(t.Parameters),
		})
	}
	return out
}

// --- response stream ------------------------------------------------------------

type respUsage struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	TotalTokens         int `json:"total_tokens"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (u *respUsage) toCore() *core.Usage {
	if u == nil {
		return nil
	}
	out := &core.Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}
	if u.InputTokensDetails != nil {
		out.CachedTokens = u.InputTokensDetails.CachedTokens
		out.CachedTokensKnown = true
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	return out
}

type respResponseEnvelope struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage *respUsage `json:"usage"`
}

type respOutputItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// respStreamEvent is one data payload of the Responses event stream.  Every
// event carries its discriminator in "type"; the rest of the fields depend on
// that type, so they stay optional pointers/maps here.
type respStreamEvent struct {
	Type     string                `json:"type"`
	Response *respResponseEnvelope `json:"response"`
	Item     *respOutputItem       `json:"item"`
	// OutputIndex keys function_call items: argument deltas and the done
	// item are matched back to the call announced by output_item.added.
	OutputIndex *int   `json:"output_index"`
	Delta       string `json:"delta"`
	// Top-level error events ({type:"error"}) carry code/message directly.
	Code    any    `json:"code"`
	Message string `json:"message"`
	// response.failed carries the error inside response.error, decoded above.
}

// respCall buffers one function_call announced on the stream.  Argument
// deltas accumulate here; the fragment the gateway sees is emitted once, on
// output_item.done, with the complete arguments -- tool callers assemble
// fragments by concatenation, so a single complete fragment is the shape
// least likely to break on a relay that skips or repeats deltas.
type respCall struct {
	id      string
	name    string
	args    string
	emitted bool
}

func (s *chatStream) absorbResponses(data string) {
	if strings.EqualFold(strings.TrimSpace(data), "[DONE]") {
		// The Responses stream has no [DONE] sentinel, but a relay that
		// reuses its chat SSE plumbing may send one.
		s.terminated = true
		s.endStream()
		return
	}
	var ev respStreamEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return
	}
	switch ev.Type {
	case "response.output_text.delta":
		if ev.Delta != "" {
			s.pending = append(s.pending, core.Event{Type: core.EventDelta, Delta: ev.Delta})
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if ev.Delta != "" {
			s.pending = append(s.pending, core.Event{Type: core.EventDelta, Reasoning: ev.Delta})
		}
	case "response.output_item.added":
		if ev.Item != nil && ev.Item.Type == "function_call" {
			idx := s.respCallIndex(ev)
			call := s.respCall(idx)
			call.id, call.name = ev.Item.CallID, ev.Item.Name
			// Announce the tool early so a caller that renders tool names
			// as they start sees it; arguments follow on the done item.
			s.pending = append(s.pending, core.Event{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{
				Index: idx, ID: call.id, Name: call.name,
			}})
		}
	case "response.function_call_arguments.delta":
		if ev.Delta == "" {
			return
		}
		idx := s.respCallIndex(ev)
		s.respCall(idx).args += ev.Delta
	case "response.output_item.done":
		if ev.Item == nil || ev.Item.Type != "function_call" {
			return
		}
		idx := s.respCallIndex(ev)
		call := s.respCall(idx)
		if ev.Item.CallID != "" {
			call.id = ev.Item.CallID
		}
		if ev.Item.Name != "" {
			call.name = ev.Item.Name
		}
		args := ev.Item.Arguments
		if strings.TrimSpace(args) == "" {
			args = call.args
		}
		s.pending = append(s.pending, core.Event{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{
			Index: idx, ID: call.id, Name: call.name, Arguments: args,
		}})
		delete(s.respCalls, idx)
	case "response.completed", "response.incomplete":
		status := ""
		if ev.Response != nil {
			status = strings.ToLower(strings.TrimSpace(ev.Response.Status))
			if u := ev.Response.Usage; u != nil {
				if cu := u.toCore(); cu != nil && !s.usageSent {
					s.usage = cu
					s.usageSent = true
					s.pending = append(s.pending, core.Event{Type: core.EventUsage, Usage: cu})
				}
			}
			if ev.Response.Error != nil && strings.TrimSpace(ev.Response.Error.Message) != "" {
				s.chunkFailure(&oaiChunkError{Code: ev.Response.Error.Code, Message: ev.Response.Error.Message})
				return
			}
		}
		s.terminated = true
		s.finish = "stop"
		if ev.Type == "response.incomplete" || status == "incomplete" {
			// The run hit max_output_tokens (or a policy cut it short);
			// "length" is the chat-wire spelling of that outcome.
			s.finish = "length"
		}
		s.endStream()
	case "response.failed":
		code, msg := any(nil), "the vendor reported an error inside the stream"
		if ev.Response != nil && ev.Response.Error != nil {
			code, msg = ev.Response.Error.Code, ev.Response.Error.Message
		}
		s.chunkFailure(&oaiChunkError{Code: code, Message: msg})
	case "error":
		s.chunkFailure(&oaiChunkError{Code: ev.Code, Message: ev.Message})
	default:
		// response.created / response.in_progress / response.queued /
		// content_part.* / *_text.done / reasoning_summary_part.*: nothing
		// the gateway consumer needs.
	}
}

// respCallIndex keys a streamed function_call by its output_index so argument
// deltas and the done item land on the call that announced them.
func (s *chatStream) respCallIndex(ev respStreamEvent) int {
	if ev.OutputIndex != nil {
		return *ev.OutputIndex
	}
	return len(s.respCalls)
}

// respCall returns the buffered call for one output index, creating it on
// first sight.
func (s *chatStream) respCall(idx int) *respCall {
	call, ok := s.respCalls[idx]
	if !ok {
		call = &respCall{}
		s.respCalls[idx] = call
	}
	return call
}
