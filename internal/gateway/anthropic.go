package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Anthropic Messages API (/v1/messages)
// ---------------------------------------------------------------------------
//
// Claude Code and the Anthropic SDKs speak one wire protocol: POST /v1/messages
// with a top-level `system` string, a `messages` array whose content is a string
// or a list of typed blocks, and a `tools` array in Anthropic's own function
// shape.  Pointing Claude Code at this gateway used to be impossible: it only
// spoke Chat Completions and Responses, so the client had to go through an
// Anthropic-to-OpenAI relay, and those relays reject the encrypted thinking
// blocks and agent content Claude Code replays between turns.
//
// This file is the native surface that removes the relay.  It decodes the
// Anthropic request into the same core.ChatRequest every module already
// consumes, so routing, stickiness, failover and usage accounting are reused
// unchanged, and it renders the answer back as genuine Anthropic Messages SSE
// (message_start / content_block_* / message_delta / message_stop) rather than
// an OpenAI shim.
//
// Two protocol facts shape the translation and are worth stating up front:
//
//  1. Anthropic tool calls and tool results are content blocks, not separate
//     message fields.  tool_use becomes an assistant message's ToolCalls and a
//     user turn's tool_result blocks become role:"tool" messages, which is the
//     shape a module's OpenAI translation already expects.
//  2. The wire reports usage as input_tokens / output_tokens only, and thinking
//     as its own content block.  Both are mapped onto core.Usage and the
//     reasoning channel so the usage ledger stays accurate across protocols.

// ---------------------------------------------------------------------------
// inbound wire types
// ---------------------------------------------------------------------------

// anthropicRequest is the subset of /v1/messages this gateway understands.
// Unknown fields are ignored on purpose, so a newer client option never turns a
// working request into a 400.
type anthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	MaxTokens     *int               `json:"max_tokens,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream,omitempty"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    json.RawMessage    `json:"tool_choice,omitempty"`
	// Metadata carries the caller's user id in Anthropic's shape.
	Metadata json.RawMessage    `json:"metadata,omitempty"`
	Options  map[string]any     `json:"client2api,omitempty"`
	Thinking *anthropicThinking `json:"thinking,omitempty"`
	// ConversationID is not part of the Anthropic schema; a caller that names
	// its conversation keeps account stickiness by sending it explicitly.
	ConversationID      string `json:"conversation_id,omitempty"`
	ConversationIDCamel string `json:"conversationId,omitempty"`
}

// anthropicThinking carries the extended-thinking budget.  Anthropic expresses
// the level as a token budget rather than a named effort, so it is translated
// onto the closest named effort this gateway's modules understand.
type anthropicThinking struct {
	Type         string `json:"type,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// anthropicMessage is one conversation turn.  Content is either a string or a
// list of typed blocks.
type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicContentBlock is one typed block.  Type is the discriminator; only
// the fields relevant to that type are populated.
type anthropicContentBlock struct {
	Type string `json:"type"`
	// Text is the body of a "text" block and the prompt of a "tool_result".
	Text string `json:"text,omitempty"`
	// Thinking is the body of a "thinking" block.
	Thinking string `json:"thinking,omitempty"`
	// Signature is the opaque signature Claude Code replays beside a thinking
	// block.  It is carried through untouched.
	Signature string `json:"signature,omitempty"`
	// ToolUse describes an assistant tool call: id, name and an input object.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// ToolUseID names the call a "tool_result" block answers.
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Content is a tool_result's payload: a string or a list of text blocks.
	Content json.RawMessage `json:"content,omitempty"`
	// IsError marks a tool_result that reports a failure.
	IsError bool `json:"is_error,omitempty"`
	// Source and MediaType describe an "image" block.
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type,omitempty"`
		Data      string `json:"data,omitempty"`
		URL       string `json:"url,omitempty"`
	} `json:"source,omitempty"`
}

// anthropicTool is one tool declaration.  Anthropic names the schema
// input_schema; the module-facing core.Tool calls it Parameters.
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// ---------------------------------------------------------------------------
// request translation
// ---------------------------------------------------------------------------

// toCoreRequestFromAnthropic converts an Anthropic request into a
// core.ChatRequest.  It mirrors toCoreRequest / toCoreRequestFromResponses so a
// module cannot tell which protocol fronted the call.
func toCoreRequestFromAnthropic(w *anthropicRequest) (*core.ChatRequest, error) {
	if w == nil {
		return nil, errors.New("empty request")
	}
	req := &core.ChatRequest{
		Model:          w.Model,
		Temperature:    w.Temperature,
		TopP:           w.TopP,
		MaxTokens:      w.MaxTokens,
		Stop:           w.StopSequences,
		Stream:         w.Stream,
		ConversationID: resolveAnthropicConversationID(w),
		Options:        optionsWithReasoningEffort(w.Options, anthropicReasoningEffort(w)),
	}
	if system := anthropicSystemText(w.System); system != "" {
		req.Messages = append(req.Messages, core.Message{Role: "system", Content: system})
	}
	for i, m := range w.Messages {
		msgs, err := translateAnthropicMessage(m)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		req.Messages = append(req.Messages, msgs...)
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("messages must not be empty")
	}
	for _, t := range w.Tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		req.Tools = append(req.Tools, core.Tool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}
	if tc := anthropicToolChoice(w.ToolChoice); tc != nil {
		req.ToolChoice = tc
	}
	return req, nil
}

// anthropicSystemText flattens `system`, which Anthropic sends either as a
// string or as a list of text blocks.
func anthropicSystemText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	switch trimmed[0] {
	case '"':
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return s
		}
	case '[':
		var blocks []anthropicContentBlock
		if json.Unmarshal(trimmed, &blocks) == nil {
			var sb strings.Builder
			for _, b := range blocks {
				if b.Type == "text" && b.Text != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(b.Text)
				}
			}
			return sb.String()
		}
	}
	return ""
}

// translateAnthropicMessage turns one Anthropic turn into one or more
// core.Messages.  A user turn may carry plain text, images and tool_result
// blocks at once; the tool results become their own role:"tool" messages so the
// vendor-facing translation, which keys tool output on the tool role, works
// unchanged.  An assistant turn carrying tool_use blocks keeps its text in the
// message body and its calls in ToolCalls.
func translateAnthropicMessage(m anthropicMessage) ([]core.Message, error) {
	role := strings.TrimSpace(m.Role)
	if role == "" {
		return nil, errors.New("role is required")
	}
	switch role {
	case "user", "assistant":
	default:
		// system is carried separately; any other role is not representable.
		if role == "system" {
			return nil, nil
		}
		return nil, fmt.Errorf("unsupported role %q", role)
	}

	text, parts, blocks, err := decodeAnthropicContent(m.Content)
	if err != nil {
		return nil, err
	}

	// Assistant: text plus tool calls on one message.
	if role == "assistant" {
		msg := core.Message{Role: "assistant", Content: text, Parts: parts}
		var thinking strings.Builder
		for _, b := range blocks {
			switch b.Type {
			case "thinking":
				// The provider's own signature is not forwarded: a module that
				// wants chain-of-thought continuity keeps its own.  The text is
				// preserved on the reasoning channel because some vendors do.
				if b.Thinking != "" {
					thinking.WriteString(b.Thinking)
				}
			case "tool_use":
				args := "{}"
				if len(b.Input) > 0 && string(bytes.TrimSpace(b.Input)) != "null" {
					args = string(b.Input)
				}
				msg.ToolCalls = append(msg.ToolCalls, core.ToolCall{
					ID:        b.ID,
					Type:      "function",
					Name:      b.Name,
					Arguments: args,
				})
			}
		}
		if thinking.Len() > 0 {
			msg.Reasoning = thinking.String()
		}
		out := []core.Message{}
		if msg.Content != "" || len(msg.Parts) > 0 || len(msg.ToolCalls) > 0 || msg.Reasoning != "" {
			out = append(out, msg)
		}
		return out, nil
	}

	// User: tool results are emitted first, in order, each as its own tool
	// message; the remaining text/images ride on a single user message.
	out := make([]core.Message, 0, len(blocks)+1)
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		text, parts := anthropicToolResultContent(b)
		out = append(out, core.Message{
			Role:       "tool",
			Content:    text,
			Parts:      parts,
			ToolCallID: b.ToolUseID,
		})
	}
	if text != "" || len(parts) > 0 {
		out = append(out, core.Message{Role: "user", Content: text, Parts: parts})
	}
	return out, nil
}

// decodeAnthropicContent flattens content into a text body, the non-text parts,
// and the raw blocks (for the tool_use / tool_result cases the caller needs).
func decodeAnthropicContent(raw json.RawMessage) (string, []core.ContentPart, []anthropicContentBlock, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return "", nil, nil, nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return "", nil, nil, errors.New("content: must be a string or an array of blocks")
		}
		return s, nil, nil, nil
	}
	if trimmed[0] != '[' {
		return "", nil, nil, errors.New("content: must be a string or an array of blocks")
	}
	var blocks []anthropicContentBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return "", nil, nil, fmt.Errorf("content: %w", err)
	}
	var sb strings.Builder
	var parts []core.ContentPart
	for _, b := range blocks {
		switch b.Type {
		case "text", "":
			if b.Text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(b.Text)
			parts = append(parts, core.ContentPart{Type: "text", Text: b.Text})
		case "image":
			if b.Source == nil {
				continue
			}
			url := b.Source.URL
			if url == "" && b.Source.Data != "" {
				mediaType := b.Source.MediaType
				if mediaType == "" {
					mediaType = "image/png"
				}
				url = "data:" + mediaType + ";base64," + b.Source.Data
			}
			if url != "" {
				parts = append(parts, core.ContentPart{Type: "image_url", ImageURL: url})
			}
		}
	}
	return sb.String(), parts, blocks, nil
}

// anthropicToolResultContent decodes a tool_result payload.  Text is flattened
// onto Content and images become image_url parts, matching the gateway-wide
// message contract.  The previous text-only helper silently turned a screenshot
// into an unreadable JSON block, which is what made view_image fail upstream.
func anthropicToolResultContent(b anthropicContentBlock) (string, []core.ContentPart) {
	trimmed := bytes.TrimSpace(b.Content)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return b.Text, nil
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return s, nil
		}
		return b.Text, nil
	}
	if trimmed[0] != '[' {
		return b.Text, nil
	}
	var blocks []anthropicContentBlock
	if json.Unmarshal(trimmed, &blocks) != nil {
		return b.Text, nil
	}
	var (
		texts []string
		parts []core.ContentPart
	)
	for _, inner := range blocks {
		switch inner.Type {
		case "text", "":
			if inner.Text != "" {
				texts = append(texts, inner.Text)
			}
		case "image":
			if inner.Source == nil {
				continue
			}
			url := inner.Source.URL
			if url == "" && inner.Source.Data != "" {
				mediaType := inner.Source.MediaType
				if mediaType == "" {
					mediaType = "image/png"
				}
				url = "data:" + mediaType + ";base64," + inner.Source.Data
			}
			if url != "" {
				parts = append(parts, core.ContentPart{Type: "image_url", ImageURL: url})
			}
		}
	}
	return strings.Join(texts, "\n"), parts
}

// anthropicToolResultText flattens a tool_result payload, which is either a
// string or a list of text blocks.
func anthropicToolResultText(b anthropicContentBlock) string {
	trimmed := bytes.TrimSpace(b.Content)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return b.Text
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return s
		}
	}
	if trimmed[0] == '[' {
		var blocks []anthropicContentBlock
		if json.Unmarshal(trimmed, &blocks) == nil {
			var sb strings.Builder
			for _, inner := range blocks {
				if inner.Text == "" {
					continue
				}
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(inner.Text)
			}
			return sb.String()
		}
	}
	return b.Text
}

// anthropicToolChoice maps Anthropic's tool_choice onto the OpenAI spelling a
// module expects.
func anthropicToolChoice(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return nil
	}
	switch obj.Type {
	case "any":
		return json.RawMessage(`"required"`)
	case "none":
		return json.RawMessage(`"none"`)
	case "tool":
		if obj.Name != "" {
			// Claude Code's own name is forwarded verbatim; the vendor schema
			// differs per module, so the neutral object form is used.
			b, _ := json.Marshal(map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": obj.Name,
				},
			})
			return b
		}
	}
	// "auto" and anything unrecognised leave the choice to the model.
	return nil
}

// anthropicReasoningEffort maps Anthropic's thinking budget onto the closest
// named effort, so modules that speak a named ladder receive one.
func anthropicReasoningEffort(w *anthropicRequest) string {
	if w == nil {
		return ""
	}
	if w.Options != nil {
		if s, ok := w.Options["reasoning_effort"].(string); ok {
			if v := normalizeEffort(s); v != "" {
				return v
			}
		}
	}
	if w.Thinking == nil || strings.EqualFold(w.Thinking.Type, "disabled") {
		return ""
	}
	switch {
	case w.Thinking.BudgetTokens <= 0:
		return "medium"
	case w.Thinking.BudgetTokens <= 2048:
		return "low"
	case w.Thinking.BudgetTokens <= 8192:
		return "medium"
	default:
		return "high"
	}
}

func resolveAnthropicConversationID(w *anthropicRequest) string {
	if w == nil {
		return ""
	}
	if v := strings.TrimSpace(w.ConversationID); v != "" {
		return v
	}
	if v := strings.TrimSpace(w.ConversationIDCamel); v != "" {
		return v
	}
	if len(w.Metadata) > 0 {
		var meta struct {
			UserID string `json:"user_id"`
		}
		if json.Unmarshal(w.Metadata, &meta) == nil {
			return strings.TrimSpace(meta.UserID)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// outbound wire types
// ---------------------------------------------------------------------------

// anthropicUsage is the usage object Anthropic reports on every message.
type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CacheReadInputTokens / CacheCreationInputTokens are Anthropic's cache
	// accounting.  A module that reports cached tokens fills the first.
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// anthropicResponse is the buffered (stream:false) reply.
type anthropicResponse struct {
	ID         string              `json:"id"`
	Type       string              `json:"type"`
	Role       string              `json:"role"`
	Model      string              `json:"model"`
	Content    []anthropicOutBlock `json:"content"`
	StopReason string              `json:"stop_reason"`
	Usage      anthropicUsage      `json:"usage"`
	StopSeq    *string             `json:"stop_sequence,omitempty"`
}

// anthropicOutBlock is one block in a reply.
type anthropicOutBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// Thinking blocks carry a signature; this gateway does not mint one, so the
	// field is omitted and clients treat the block as unsigned.
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// toAnthropicUsage converts the module-neutral usage onto the wire shape.
func toAnthropicUsage(u *core.Usage) anthropicUsage {
	if u == nil {
		return anthropicUsage{}
	}
	out := anthropicUsage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
	}
	if u.CachedTokensKnown && u.CachedTokens > 0 {
		out.CacheReadInputTokens = u.CachedTokens
	}
	// Anthropic reports input excluding the cache read; the module-neutral
	// PromptTokens is the total, so the cached portion is subtracted when the
	// module knew it.  This keeps the two protocols' ledgers comparable and
	// avoids double-counting a cached prefix.
	if out.CacheReadInputTokens > 0 && out.InputTokens >= out.CacheReadInputTokens {
		out.InputTokens -= out.CacheReadInputTokens
	}
	return out
}

// anthropicStopReason maps a core finish reason onto Anthropic's vocabulary.
func anthropicStopReason(finish string, toolCalls int) string {
	if toolCalls > 0 {
		return "tool_use"
	}
	switch finish {
	case "length":
		return "max_tokens"
	case "stop":
		return "end_turn"
	case "content_filter":
		return "refusal"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// ---------------------------------------------------------------------------
// emitter
// ---------------------------------------------------------------------------

// anthropicEmitter renders a core.Stream as Anthropic Messages SSE (streaming)
// or one message object (buffered).
type anthropicEmitter struct{}

func (anthropicEmitter) emitStream(w http.ResponseWriter, r *http.Request, a emitArgs) {
	a.s.streamAnthropicSSE(w, r, a)
}

func (anthropicEmitter) emitBuffered(w http.ResponseWriter, r *http.Request, a emitArgs) {
	a.s.bufferAnthropic(w, r, a)
}

// collectAnthropic runs a stream to completion, assembling the Anthropic blocks
// plus the terminal reason and usage.  Both the buffered writer and the count
// path share it, so the two can never disagree about a finished turn.
type anthropicCollected struct {
	blocks    []anthropicOutBlock
	usage     *core.Usage
	finish    string
	toolCalls int
	streamErr error
}

func collectAnthropic(stream core.Stream, stat *chatStat) anthropicCollected {
	var (
		out       anthropicCollected
		text      strings.Builder
		reasoning strings.Builder
		toolOrder []int
		toolByIx  = map[int]*core.ToolCall{}
		finished  bool
	)
	for {
		ev, err := stream.Recv()
		if err != nil {
			// Preserve an EventError the module already reported: the io.EOF
			// that follows it only marks the end of the stream.
			if out.streamErr == nil {
				if errors.Is(err, io.EOF) && !finished {
					out.streamErr = io.ErrUnexpectedEOF
				} else if !errors.Is(err, io.EOF) {
					out.streamErr = err
				}
			}
			break
		}
		if stat != nil {
			stat.noteFirstFrame(time.Now())
		}
		switch ev.Type {
		case core.EventDelta:
			text.WriteString(ev.Delta)
			reasoning.WriteString(ev.Reasoning)
		case core.EventToolCall:
			if ev.ToolCall == nil {
				continue
			}
			tc, ok := toolByIx[ev.ToolCall.Index]
			if !ok {
				tc = &core.ToolCall{Type: "function"}
				toolByIx[ev.ToolCall.Index] = tc
				toolOrder = append(toolOrder, ev.ToolCall.Index)
			}
			if ev.ToolCall.ID != "" {
				tc.ID = ev.ToolCall.ID
			}
			if ev.ToolCall.Name != "" {
				tc.Name = ev.ToolCall.Name
			}
			tc.Arguments += ev.ToolCall.Arguments
		case core.EventUsage:
			if ev.Usage != nil {
				out.usage = ev.Usage
			}
		case core.EventDone:
			finished = true
			if ev.Finish != "" {
				out.finish = ev.Finish
			}
		case core.EventError:
			if ev.Err != nil {
				out.streamErr = ev.Err
			}
		}
	}
	if reasoning.Len() > 0 {
		out.blocks = append(out.blocks, anthropicOutBlock{Type: "thinking", Thinking: reasoning.String()})
	}
	if text.Len() > 0 {
		out.blocks = append(out.blocks, anthropicOutBlock{Type: "text", Text: text.String()})
	}
	sort.Ints(toolOrder)
	for _, ix := range toolOrder {
		tc := toolByIx[ix]
		if tc.Name == "" {
			continue
		}
		id := tc.ID
		if id == "" {
			id = newID("toolu_")
		}
		input := json.RawMessage(tc.Arguments)
		if len(bytes.TrimSpace(input)) == 0 {
			input = json.RawMessage(`{}`)
		}
		out.blocks = append(out.blocks, anthropicOutBlock{
			Type: "tool_use", ID: id, Name: tc.Name, Input: input,
		})
		out.toolCalls++
	}
	if out.finish == "" && !finished {
		out.finish = "stop"
	}
	return out
}

// bufferAnthropic writes the stream:false reply.
func (s *server) bufferAnthropic(w http.ResponseWriter, r *http.Request, a emitArgs) {
	res := collectAnthropic(a.stream, a.stat)
	if a.rec != nil {
		a.rec.setUsage(res.usage)
	}
	if a.stat != nil {
		a.stat.noteUsage(res.usage)
	}
	if res.streamErr != nil {
		status, _, _ := upstreamErrorShape(res.streamErr)
		if a.stat != nil {
			a.stat.status = status
		}
		s.fail(a.rec, res.streamErr)
		s.writeUpstreamError(w, a.client, a.hintCtx, res.streamErr)
		return
	}
	if len(res.blocks) == 0 {
		if a.stat != nil {
			a.stat.status = http.StatusBadGateway
		}
		s.fail(a.rec, errEmptyCompletion())
		s.writeUpstreamError(w, a.client, a.hintCtx, errEmptyCompletion())
		return
	}
	if a.stat != nil {
		a.stat.status = http.StatusOK
	}
	writeJSON(w, http.StatusOK, anthropicResponse{
		ID:         newID("msg_"),
		Type:       "message",
		Role:       "assistant",
		Model:      a.model,
		Content:    res.blocks,
		StopReason: anthropicStopReason(res.finish, res.toolCalls),
		Usage:      toAnthropicUsage(res.usage),
	})
}

// streamAnthropicSSE renders a live stream in Anthropic's event vocabulary.
//
// The sequence Claude Code requires is fixed: message_start, then per block a
// content_block_start / content_block_delta* / content_block_stop, then
// message_delta carrying stop_reason and usage, then message_stop.  Thinking
// text is emitted as a thinking_delta on its own block and regular text as a
// text_delta; tool calls are opened on first sight and their arguments streamed
// as input_json_delta fragments.
func (s *server) streamAnthropicSSE(w http.ResponseWriter, r *http.Request, a emitArgs) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		if a.stat != nil {
			a.stat.status = http.StatusInternalServerError
		}
		writeError(w, http.StatusInternalServerError, "server_error", "streaming unsupported by this server")
		return
	}

	streamID := newID("msg_")
	started := time.Now()
	var eventCount int
	terminal := "in_progress"
	var usage *core.Usage
	defer func() {
		if a.rec != nil {
			a.rec.setUsage(usage)
		}
		if a.stat != nil {
			a.stat.noteUsage(usage)
		}
		s.opts.Logger.Printf("stream: id=%s protocol=anthropic client=%s model=%q terminal=%s events=%d duration=%s",
			streamID, a.client.Name(), a.model, terminal, eventCount, time.Since(started).Round(time.Millisecond))
	}()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if a.stat != nil {
		a.stat.status = http.StatusOK
	}

	send := func(eventType string, payload any) bool {
		if err := sseFrame(w, eventType, payload); err != nil {
			terminal = "downstream_write_error"
			return false
		}
		flusher.Flush()
		eventCount++
		return true
	}

	if !send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            streamID,
			"type":          "message",
			"role":          "assistant",
			"model":         a.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         anthropicUsage{},
		},
	}) {
		s.fail(a.rec, errDownstreamWrite)
		return
	}

	// Open blocks are tracked by kind so a switch from thinking to text to tool
	// closes the previous block exactly once.
	type openBlock struct {
		index int
		kind  string
		id    string
		name  string
	}
	var open *openBlock
	nextIndex := 0
	toolCalls := 0
	finish := ""

	closeBlock := func() bool {
		if open == nil {
			return true
		}
		idx := open.index
		open = nil
		return send("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
	}
	beginBlock := func(kind, id, name string) bool {
		if !closeBlock() {
			return false
		}
		open = &openBlock{index: nextIndex, kind: kind, id: id, name: name}
		nextIndex++
		block := map[string]any{"type": kind}
		switch kind {
		case "text":
			block["text"] = ""
		case "thinking":
			block["thinking"] = ""
		case "tool_use":
			block["id"] = id
			block["name"] = name
			block["input"] = map[string]any{}
		}
		return send("content_block_start", map[string]any{
			"type": "content_block_start", "index": open.index, "content_block": block,
		})
	}

	for {
		ev, err := a.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			terminal = "upstream_error"
			_ = closeBlock()
			_ = send("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": err.Error()},
			})
			if a.stat != nil {
				a.stat.status = http.StatusBadGateway
			}
			s.fail(a.rec, err)
			return
		}
		if a.stat != nil {
			a.stat.noteFirstFrame(time.Now())
		}
		switch ev.Type {
		case core.EventDelta:
			if ev.Reasoning != "" {
				if open == nil || open.kind != "thinking" {
					if !beginBlock("thinking", "", "") {
						terminal = "downstream_write_error"
						s.fail(a.rec, errDownstreamWrite)
						return
					}
				}
				if !send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": open.index,
					"delta": map[string]any{"type": "thinking_delta", "thinking": ev.Reasoning},
				}) {
					terminal = "downstream_write_error"
					s.fail(a.rec, errDownstreamWrite)
					return
				}
			}
			if ev.Delta != "" {
				if open == nil || open.kind != "text" {
					if !beginBlock("text", "", "") {
						terminal = "downstream_write_error"
						s.fail(a.rec, errDownstreamWrite)
						return
					}
				}
				if !send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": open.index,
					"delta": map[string]any{"type": "text_delta", "text": ev.Delta},
				}) {
					terminal = "downstream_write_error"
					s.fail(a.rec, errDownstreamWrite)
					return
				}
			}
		case core.EventToolCall:
			if ev.ToolCall == nil {
				continue
			}
			if open == nil || open.kind != "tool_use" || ev.ToolCall.Name != "" {
				id := ev.ToolCall.ID
				if id == "" {
					id = newID("toolu_")
				}
				name := ev.ToolCall.Name
				if !beginBlock("tool_use", id, name) {
					terminal = "downstream_write_error"
					s.fail(a.rec, errDownstreamWrite)
					return
				}
				toolCalls++
			}
			if ev.ToolCall.Arguments != "" {
				if !send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": open.index,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.ToolCall.Arguments},
				}) {
					terminal = "downstream_write_error"
					s.fail(a.rec, errDownstreamWrite)
					return
				}
			}
		case core.EventUsage:
			if ev.Usage != nil {
				usage = ev.Usage
			}
		case core.EventDone:
			if ev.Finish != "" {
				finish = ev.Finish
			}
		case core.EventError:
			if ev.Err != nil {
				terminal = "upstream_error"
				_ = closeBlock()
				_ = send("error", map[string]any{
					"type":  "error",
					"error": map[string]any{"type": "api_error", "message": ev.Err.Error()},
				})
				if a.stat != nil {
					a.stat.status = http.StatusBadGateway
				}
				s.fail(a.rec, ev.Err)
				return
			}
		}
	}

	if !closeBlock() {
		terminal = "downstream_write_error"
		s.fail(a.rec, errDownstreamWrite)
		return
	}
	terminal = "ok"
	_ = send("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   anthropicStopReason(finish, toolCalls),
			"stop_sequence": nil,
		},
		"usage": toAnthropicUsage(usage),
	})
	_ = send("message_stop", map[string]any{"type": "message_stop"})
}

// ---------------------------------------------------------------------------
// endpoint
// ---------------------------------------------------------------------------

// handleMessages serves POST /v1/messages.
func (s *server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "use POST")
		return
	}
	s.stats.addRequest()

	rec := UsageRecord{At: time.Now(), StartedAt: time.Now()}
	defer func() {
		rec.At = time.Now()
		s.recordUsage(rec)
	}()
	stat := newChatStat(rec.StartedAt, "", false)
	defer stat.done()

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var wire anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
		stat.status = http.StatusBadRequest
		s.fail(&rec, err)
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	rec.Model = wire.Model
	stat.model = wire.Model
	stat.mode = chatMode(wire.Stream)

	req, err := toCoreRequestFromAnthropic(&wire)
	if err != nil {
		stat.status = http.StatusBadRequest
		s.fail(&rec, err)
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	req.ConversationRequestID = strings.TrimSpace(r.Header.Get("X-Conversation-Request-ID"))
	rec.SessionID = sessionIDFor(req)
	rec.ReasoningEffort = anthropicReasoningEffort(&wire)

	s.routeAndServeAnthropic(w, r, routeInput{
		requested:    wire.Model,
		req:          req,
		stream:       wire.Stream,
		modelForWire: wire.Model,
		rec:          &rec,
		stat:         stat,
		emitter:      anthropicEmitter{},
	})
}

// writeAnthropicError renders the Anthropic error envelope, which wraps the same
// facts in {"type":"error","error":{...}} rather than OpenAI's flat {"error":…}.
// A client that reads `error.type` instead of an HTTP status would otherwise
// mis-classify every refusal.
func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	if typ == "invalid_request_error" {
		typ = "invalid_request_error"
	}
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    typ,
			"message": msg,
		},
	})
}

// routeAndServeAnthropic is routeAndServe with an Anthropic-shaped failure
// envelope.  The orchestration is identical; only the pre-stream error writes
// differ, because the shared router writes OpenAI-shaped errors and Claude Code
// parses Anthropic ones.
func (s *server) routeAndServeAnthropic(w http.ResponseWriter, r *http.Request, in routeInput) {
	s.routeAndServeMessages(w, r, in, writeAnthropicError)
}

// handleCountTokens serves POST /v1/messages/count_tokens.  The count is a local
// estimate from the same heuristic the context guard uses, because no vendor
// publishes a tokenizer this gateway could call: it exists so a client that
// budgets its context can do so, and it is deliberately cheap and offline.
func (s *server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "use POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	var wire anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}
	if !s.exposedModelAllowed(r, wire.Model) {
		writeError(w, http.StatusNotFound, "invalid_request_error",
			fmt.Sprintf("model %q is not exposed by this gateway", wire.Model))
		return
	}
	req, err := toCoreRequestFromAnthropic(&wire)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": estimateRequestTokens(req)})
}
