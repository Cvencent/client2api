package qoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"client2api/internal/core"
)

// chat.go owns the model-facing half of the module: the request body the
// gateway expects, the account rotation that runs one, and the stream the
// caller reads.
//
// The body is the OpenAI-shaped object the desktop client's own agent posts.
// The gateway answers with an SSE envelope rather than a bare OpenAI stream,
// which sse.go unwraps.

// chatClientType is the metadata.context.client_type the desktop client
// reports.  It is a protocol constant, not a knob: a different value is a
// different client.
const chatClientType = "5"

// mapGatewayModel normalises a model id that arrived through the gateway.  The
// routing prefix is already stripped by the registry; what is left is trimmed
// and matched case-insensitively against the catalogue, because the vendor's
// keys are lower-case and a caller typing "Auto" means the same thing.
func (c *Client) mapGatewayModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	for _, m := range c.modelCatalogue() {
		if strings.EqualFold(m.ID, model) {
			return m.ID
		}
	}
	return model
}

// ---------------------------------------------------------------------------
// request body
// ---------------------------------------------------------------------------

// chatEnvelope is the body of POST /algo/api/v2/service/pro/sse/agent_chat_generation.
type chatEnvelope struct {
	Model         string         `json:"model"`
	Messages      []any          `json:"messages"`
	Stream        bool           `json:"stream"`
	StreamOptions map[string]any `json:"stream_options"`
	Metadata      map[string]any `json:"metadata"`
	Tools         []any          `json:"tools,omitempty"`
	Stop          []string       `json:"stop,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	MaxTokens     *int           `json:"max_tokens,omitempty"`
}

// chatBody renders one canonical request as the exact byte string that is both
// signed and sent.
func chatBody(req *core.ChatRequest, model string) (string, error) {
	messages := make([]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		if rendered, ok := renderMessage(m); ok {
			messages = append(messages, rendered)
		}
	}
	envelope := chatEnvelope{
		Model:    model,
		Messages: messages,
		Stream:   true,
		// The gateway only reports the credit cost in the trailing usage
		// frame when it is asked for, which is what makes the balance the
		// panel shows match what the request really spent.
		StreamOptions: map[string]any{"include_usage": true},
		Metadata:      map[string]any{"context": chatContext(req)},
		Tools:         renderTools(req.Tools),
		Stop:          req.Stop,
	}
	envelope.Temperature = req.Temperature
	envelope.TopP = req.TopP
	envelope.MaxTokens = req.MaxTokens
	return jsonMarshalled(envelope)
}

// chatContext builds the metadata.context object.  Every id is either the
// caller's or freshly minted; the vendor uses them to group one turn and to
// attribute the spend.
func chatContext(req *core.ChatRequest) map[string]any {
	session := strings.TrimSpace(req.ConversationID)
	if session == "" {
		session = newUUID()
	}
	turn := strings.TrimSpace(req.ConversationRequestID)
	if turn == "" {
		turn = cosyRequestID()
	}
	return map[string]any{
		"request_id":     turn,
		"request_set_id": cosyRequestID(),
		"session_id":     session,
		"client_type":    chatClientType,
	}
}

// renderMessage renders one canonical message, reporting ok=false for a role
// the gateway does not accept.
func renderMessage(m core.Message) (any, bool) {
	role := strings.ToLower(strings.TrimSpace(m.Role))
	switch role {
	case "system", "user", "assistant", "tool":
	default:
		return nil, false
	}
	out := map[string]any{"role": role}
	if len(m.Parts) > 0 {
		if parts := renderParts(m.Parts); len(parts) > 0 {
			out["content"] = parts
		}
	}
	if _, has := out["content"]; !has {
		if role == "assistant" && len(m.ToolCalls) > 0 && strings.TrimSpace(m.Content) == "" {
			// An assistant turn that only calls tools has no text at all; the
			// gateway rejects a null content next to tool_calls.
		} else {
			out["content"] = m.Content
		}
	}
	if name := strings.TrimSpace(m.Name); name != "" {
		out["name"] = name
	}
	if role == "assistant" {
		if reasoning := strings.TrimSpace(m.Reasoning); reasoning != "" {
			out["reasoning_content"] = reasoning
		}
		if calls := renderToolCalls(m.ToolCalls); len(calls) > 0 {
			out["tool_calls"] = calls
		}
	}
	if role == "tool" {
		if id := strings.TrimSpace(m.ToolCallID); id != "" {
			out["tool_call_id"] = id
		}
	}
	return out, true
}

// renderParts renders a multimodal message body.
func renderParts(parts []core.ContentPart) []any {
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		switch strings.ToLower(strings.TrimSpace(p.Type)) {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": p.Text})
		case "image_url":
			image := map[string]any{"url": p.ImageURL}
			if detail := strings.TrimSpace(p.Detail); detail != "" {
				image["detail"] = detail
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": image})
		}
	}
	return out
}

// renderToolCalls renders an assistant turn's tool invocations.
func renderToolCalls(calls []core.ToolCall) []any {
	out := make([]any, 0, len(calls))
	for _, tc := range calls {
		kind := strings.TrimSpace(tc.Type)
		if kind == "" {
			kind = "function"
		}
		out = append(out, map[string]any{
			"id":   tc.ID,
			"type": kind,
			"function": map[string]any{
				"name":      tc.Name,
				"arguments": tc.Arguments,
			},
		})
	}
	return out
}

// renderTools renders the tool declarations offered to the model.
func renderTools(tools []core.Tool) []any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		kind := strings.TrimSpace(t.Type)
		if kind == "" {
			kind = "function"
		}
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if len(t.Parameters) > 0 {
			fn["parameters"] = json.RawMessage(t.Parameters)
		}
		out = append(out, map[string]any{"type": kind, "function": fn})
	}
	return out
}

// ---------------------------------------------------------------------------
// the request loop
// ---------------------------------------------------------------------------

// maxChatAttempts bounds how many accounts one request may burn through.  It is
// deliberately small: a request that failed on three credentials is a platform
// problem, not something to keep hammering.
const maxChatAttempts = 4

// Chat opens one streamed completion.
//
// Accounts are tried in the pool's own order (operator priority, then least
// recently used).  A credential the vendor rejects is parked; a transient
// failure cools the account down briefly; the caller only sees an error once
// every candidate has been tried.
func (c *Client) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, core.ErrUnsupported
	}
	model := c.mapGatewayModel(req.Model)
	if model == "" {
		return nil, core.ErrUnsupported
	}

	candidates := c.orderedCandidates(req, model)
	if len(candidates) == 0 {
		return nil, core.ErrNotConfigured
	}

	chatCtx, cancel := context.WithTimeout(ctx, c.cfg.chatTimeout())

	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < len(candidates) && i < maxChatAttempts; i++ {
		acc := candidates[i]
		if tried[acc.ID] {
			continue
		}
		tried[acc.ID] = true
		if err := chatCtx.Err(); err != nil {
			cancel()
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		if err := req.AcquireAccountSlot(acc.ID); err != nil {
			// A full account is skipped, not blamed.
			lastErr = err
			continue
		}
		c.bindServedConversation(req, acc.ID)
		c.inFlight.Add(1)
		// Recorded before the call so the usage ledger attributes the request
		// even when the stream never produces a frame.
		core.NoteServedBy(req, acc.ID)

		body, err := chatBody(req, model)
		if err != nil {
			req.ReleaseAccountSlot()
			c.inFlight.Add(-1)
			cancel()
			return nil, err
		}
		resp, err := c.openChat(chatCtx, acc, body)
		if err != nil {
			req.ReleaseAccountSlot()
			c.inFlight.Add(-1)
			if callerGone(err) {
				cancel()
				return nil, err
			}
			c.penalise(acc.ID, err, nowUTC())
			lastErr = err
			continue
		}
		c.store.reset(acc.ID, nowUTC())
		return newQoderStream(chatCtx, cancel, func() {
			c.inFlight.Add(-1)
			req.ReleaseAccountSlot()
		}, resp.Body), nil
	}

	cancel()
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, core.ErrNotConfigured
}

// ---------------------------------------------------------------------------
// the connectivity probe
// ---------------------------------------------------------------------------

// probeChat sends one minimum-size real completion and returns the first text
// it produced.
//
// A catalogue listing proves the token opens the gateway; only a completion
// proves the account can actually answer, which is what the panel's 测试 button
// is asked.  The prompt is one word because the probe is billed like any other
// request.
func (c *Client) probeChat(ctx context.Context, acc account) (string, error) {
	model := c.cfg.testModel()
	req := &core.ChatRequest{
		Model: model,
		Messages: []core.Message{{
			Role:    "user",
			Content: "ping",
		}},
		MaxTokens: intPtr(16),
	}
	body, err := chatBody(req, model)
	if err != nil {
		return "", err
	}
	resp, err := c.openChat(ctx, acc, body)
	if err != nil {
		return "", err
	}
	stream := newQoderStream(ctx, nil, nil, resp.Body)
	defer stream.Close()

	var reply strings.Builder
	for {
		event, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if reply.Len() > 0 {
				return reply.String(), nil
			}
			return "", err
		}
		switch event.Type {
		case core.EventDelta:
			reply.WriteString(event.Delta)
		case core.EventError:
			if event.Err != nil {
				if reply.Len() > 0 {
					return reply.String(), nil
				}
				return "", event.Err
			}
		case core.EventDone:
			// keep draining until the stream really ends
		}
	}
	if reply.Len() == 0 {
		return "", fmt.Errorf("qoder: %s answered without any text", model)
	}
	return reply.String(), nil
}

func intPtr(n int) *int { return &n }

// probeTimeoutContext bounds one connectivity probe.
func (c *Client) probeTimeoutContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, c.cfg.probeTimeout())
}
