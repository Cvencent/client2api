package qoder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
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
//
// Two transports exist behind this method.  The two zero-cost catalogue keys
// (dfmodel / qfmodel) are served by driving the vendor's own agent CLI, because
// the inference edge refuses them to anything else; every other model keeps
// using the signed gateway path this module has always used.  The split is by
// model and is decided here, once, so neither path can be reached by accident.
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
	if cliModel(model) {
		return c.chatViaCLI(ctx, req, model, "")
	}

	candidates := c.orderedCandidates(req, model)
	if len(candidates) == 0 {
		return nil, core.ErrNotConfigured
	}

	chatCtx, cancel := context.WithTimeout(ctx, c.cfg.chatTimeout())

	tried := map[string]bool{}
	var lastErr error
	// issued counts actual upstream attempts. A busy account is skipped
	// without consuming one of those attempts, so a concurrent request can
	// continue down the candidate list instead of failing on the lock race.
	issued := 0
	for i := 0; i < len(candidates); i++ {
		if issued >= maxChatAttempts {
			break
		}
		acc := candidates[i]
		if tried[acc.ID] {
			continue
		}
		tried[acc.ID] = true
		if acc.cliOnly() {
			// A CLI-only account has no device token, so the signed gateway
			// route can only answer 401 and park a working account.  Its models
			// are the two free keys, which never reach this loop.
			continue
		}
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
		issued++
		resp, err := c.openChat(chatCtx, acc, body, model)
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
		return newQoderStreamFor(chatCtx, cancel, func() {
			c.inFlight.Add(-1)
			req.ReleaseAccountSlot()
		}, resp.Body, acc.ID), nil
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
	if acc.cliOnly() {
		// A CLI-only account has no device token, so the signed probe route
		// would only return 401 and park a working account.  Its test goes
		// through the same transport it serves with.
		model = "dfmodel"
	}
	req := &core.ChatRequest{
		Model: model,
		Messages: []core.Message{{
			Role:    "user",
			Content: "ping",
		}},
		MaxTokens: intPtr(16),
	}
	// A free model is only reachable through the CLI bridge, so the probe has
	// to use the same transport a caller would.  Otherwise the panel's "测试"
	// button would report a failure for an account that serves fine.
	if cliModel(model) {
		stream, err := c.chatViaCLI(ctx, req, model, acc.ID)
		if err != nil {
			return "", err
		}
		return drainCLIProbe(stream)
	}
	body, err := chatBody(req, model)
	if err != nil {
		return "", err
	}
	resp, err := c.openChat(ctx, acc, body, model)
	if err != nil {
		return "", err
	}
	stream := newQoderStreamFor(ctx, nil, nil, resp.Body, acc.ID)
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

// ---------------------------------------------------------------------------
// the CLI transport
// ---------------------------------------------------------------------------

// cliCandidates is the account list a CLI-served model may use: the pool's
// normal order, minus everything that has no local CLI login to serve from.
//
// The filter is a filesystem check on the CLI's own marker.  An account that
// was imported as a device token but never signed in through the panel's CLI
// login has no config root, so it cannot answer a free-model request at all --
// and handing it the request would burn an attempt on a turn the worker can
// only refuse with "Not logged in".
func (c *Client) cliCandidates(req *core.ChatRequest, accountList []account, prefer string) []account {
	prefer = strings.TrimSpace(prefer)
	out := make([]account, 0, len(accountList))
	for _, acc := range accountList {
		if prefer != "" && acc.ID != prefer {
			continue
		}
		if c.cliAccountReady(acc) {
			out = append(out, acc)
		}
	}
	return out
}

// chatViaCLI runs one free-model turn through the vendor's agent CLI.
//
// The account order is the pool's, so an operator's priority list and the
// conversation affinity both apply exactly as they do on the gateway path.  An
// account is only blamed when the worker itself says the credential is the
// problem; a process that could not start is a host problem and rotates.
// prefer, when non-empty, restricts the candidates to that one account.  It is
// what the panel's per-account test button uses: testing account A must not
// answer with account B just because B was least recently used.
func (c *Client) chatViaCLI(ctx context.Context, req *core.ChatRequest, model, prefer string) (core.Stream, error) {
	inst, err := findCLI()
	if err != nil {
		return nil, err
	}
	sticky, err := promptFromRequest(req)
	if err != nil {
		return nil, err
	}
	// Only computed for the cold path, but computed once: the pool decides per
	// attempt whether a worker had to be started, and a fresh worker needs the
	// whole history or it would answer with no context.
	fresh, err := flattenRequest(req)
	if err != nil {
		return nil, err
	}
	ordered := c.orderedCandidates(req, model)
	if len(ordered) == 0 {
		return nil, core.ErrNotConfigured
	}
	candidates := c.cliCandidates(req, ordered, prefer)
	if len(candidates) == 0 {
		// Distinguish "no accounts at all" from "no account signed in to the
		// local CLI": the remedies are different, and the panel prints this
		// verbatim.
		if c.store.count() == 0 {
			return nil, core.ErrNotConfigured
		}
		return nil, errCLINotSignedIn
	}

	chatCtx, cancel := context.WithTimeout(ctx, c.cfg.chatTimeout())
	// A conversation-scoped request shares one worker across turns; a stateless
	// one gets a session of its own, because the caller sent a full history and
	// reusing another request's transcript would corrupt both.
	conversation := conversationKey(req)
	if conversation == "" {
		conversation = "req-" + newUUID()
	}

	var lastErr error
	for i, acc := range candidates {
		if i >= maxChatAttempts {
			break
		}
		if err := chatCtx.Err(); err != nil {
			cancel()
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, err
		}
		if err := req.AcquireAccountSlot(acc.ID); err != nil {
			lastErr = err
			continue
		}
		c.inFlight.Add(1)
		core.NoteServedBy(req, acc.ID)

		stream, err := c.cliPoolFor().beginLease(chatCtx,
			inst,
			c.cliDirForAccount(acc),
			acc.ID,
			conversation,
			model,
			c.cliEffort(req),
			sticky,
			fresh,
		)
		if err != nil {
			c.inFlight.Add(-1)
			req.ReleaseAccountSlot()
			if callerGone(err) || errors.Is(err, context.Canceled) {
				cancel()
				return nil, err
			}
			// A worker that will not start is this host's problem, not the
			// account's: try the next account without parking this one.
			c.logf("qoder: cli worker for %s: %v", acc.ID, err)
			lastErr = err
			continue
		}
		c.bindServedConversation(req, acc.ID)
		c.store.reset(acc.ID, nowUTC())
		return newCLIStream(chatCtx, cancel, func() {
			c.inFlight.Add(-1)
			req.ReleaseAccountSlot()
		}, stream, acc.ID), nil
	}
	cancel()
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, core.ErrNotConfigured
}

// cliEffort resolves the reasoning tier the CLI is asked for.
//
// The CLI validates the value against its own ladder, so a caller's spelling is
// passed through only when it is one of the tiers the vendor documents; a
// value the caller invented would fail the whole turn.
func (c *Client) cliEffort(req *core.ChatRequest) string {
	if req == nil {
		return ""
	}
	for _, key := range []string{"reasoning_effort", "reasoningEffort", "effort"} {
		raw, ok := req.Options[key]
		if !ok {
			continue
		}
		if tier := cliEffortTier(optionStringValue(raw)); tier != "" {
			return tier
		}
	}
	return cliEffortTier(c.cfg.CLIReasoningEffort)
}

// cliEffortTiers are the tiers the CN CLI accepts for --reasoning-effort.
var cliEffortTiers = map[string]bool{
	"minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true,
}

// cliEffortTier normalises one spelling, or returns "" when it is not a tier
// the CLI would accept.
func cliEffortTier(raw string) string {
	tier := strings.ToLower(strings.TrimSpace(raw))
	if tier == "" {
		return ""
	}
	switch tier {
	case "none", "off", "disabled", "false", "0":
		// The CLI has no "off"; omitting the flag is how thinking stays off.
		return ""
	}
	if cliEffortTiers[tier] {
		return tier
	}
	return ""
}

// optionStringValue renders a ChatRequest.Options value as a string.
func optionStringValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return ""
}

// drainCLIProbe reads a CLI-served probe turn and returns its text.
func drainCLIProbe(stream core.Stream) (string, error) {
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
		}
	}
	if reply.Len() == 0 {
		return "", errors.New("qoder: the local CLI answered without any text")
	}
	return reply.String(), nil
}
