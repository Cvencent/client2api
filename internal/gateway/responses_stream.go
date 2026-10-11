package gateway

import (
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
// Responses rendering
// ---------------------------------------------------------------------------
//
// Rendering is where a protocol adapter earns its keep, and Codex's parser is
// strict in three places that are easy to get wrong:
//
//  1. It dispatches on the `type` field *inside* each data payload, not on the
//     SSE `event:` header.  The two must agree, so both are written from the
//     same string here.
//  2. `response.output_item.done` is the authoritative carrier: function-call
//     dispatch and assistant-message finalisation both happen on that event,
//     not on `.added`.  A stream that only emits deltas makes Codex show text
//     and never run a tool.
//  3. `response.completed` must contain `response.id`.  It has no default in the
//     decoder, so omitting it turns every successful turn into a stream failure.
//
// The sequence emitted here is therefore, in order: response.created, then for
// each output item its .added / deltas / .done, then response.completed.  Text
// and tool calls are emitted as separate items -- interleaved in the order the
// model produced them -- because that is the shape Codex replays back as input.

// responsesEmitter renders a core.Stream as Responses SSE (streaming) or a
// single response object (buffered).
type responsesEmitter struct{}

func (responsesEmitter) emitStream(w http.ResponseWriter, r *http.Request, a emitArgs) {
	a.s.streamResponsesSSE(w, r, a)
}

func (responsesEmitter) emitBuffered(w http.ResponseWriter, r *http.Request, a emitArgs) {
	a.s.bufferResponse(w, r, a)
}

// responseBaseFields carries what every envelope event repeats: the identity of
// the response and the model that produced it.
type responseBaseFields struct {
	id    string
	model string
}

func (b responseBaseFields) base(status string, output []responseOutput) responseObject {
	if output == nil {
		output = []responseOutput{}
	}
	return responseObject{
		ID:        b.id,
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    status,
		Model:     b.model,
		Output:    output,
	}
}

func (b responseBaseFields) result(finish string, output []responseOutput, usage *core.Usage) responseObject {
	final := b.base("completed", output)
	final.Usage = toResponseUsage(usage)
	if finish == "length" {
		final.Status = "incomplete"
		final.IncompleteDetails = &responseIncompleteDetails{Reason: "max_output_tokens"}
		for i := range final.Output {
			final.Output[i].Status = "incomplete"
		}
	}
	return final
}

// sseFrame writes one SSE event.  The header line and the payload's own type are
// written from one string so they can never disagree -- the mismatch Codex
// cannot tolerate.
func sseFrame(w io.Writer, eventType string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, b); err != nil {
		return err
	}
	return nil
}

// streamResponsesSSE renders a live stream.  It accumulates each output item as
// it arrives so that its `.done` event can carry the complete item, which is the
// event Codex acts on.
func (s *server) streamResponsesSSE(w http.ResponseWriter, r *http.Request, a emitArgs) {
	client := a.client
	stream := a.stream
	var (
		usage      *core.Usage
		finish     string
		textTail   strings.Builder
		reasonTail strings.Builder
	)
	started := time.Now()
	streamID := newID("st-")
	eventCount := 0
	deltaBytes := 0
	reasoningBytes := 0
	toolCalls := 0
	terminal := "in_progress"
	defer func() {
		ttfb := "-"
		if a.stat != nil && a.stat.ttfb > 0 {
			ttfb = a.stat.ttfb.Round(time.Millisecond).String()
		}
		finishText := finish
		if finishText == "" {
			finishText = "-"
		}
		completionTokens := -1
		if usage != nil {
			completionTokens = usage.CompletionTokens
		}
		// A turn that ends with "stop" and no tool call is either an ordinary
		// answer or the failure this gateway keeps chasing: the model narrates
		// an intent ("Let me ...") and stops before emitting the call.  Quoting
		// the end of the visible text is the only way to tell the two apart
		// after the fact, so the tail is logged for exactly that shape.
		note := ""
		if finish == "stop" && toolCalls == 0 {
			note = fmt.Sprintf(" stop_tail=%q stop_reason_tail=%q", textTail.String(), reasonTail.String())
		}
		s.opts.Logger.Printf("stream: id=%s protocol=responses client=%s model=%q terminal=%s finish=%s events=%d delta_bytes=%d reasoning_bytes=%d tool_calls=%d completion_tokens=%d ttfb=%s duration=%s%s",
			streamID, client.Name(), a.model, terminal, finishText, eventCount, deltaBytes, reasoningBytes, toolCalls, completionTokens, ttfb, time.Since(started).Round(time.Millisecond), note)
	}()
	// Token counts may arrive on the last event and this function has several
	// exits; one deferred copy covers all of them.
	defer func() {
		a.rec.setUsage(usage)
		a.stat.noteUsage(usage)
	}()

	flusher, ok := w.(http.Flusher)
	if !ok {
		terminal = "streaming_unsupported"
		a.stat.status = http.StatusInternalServerError
		writeError(w, http.StatusInternalServerError, "server_error", "streaming unsupported by this server")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	a.stat.status = http.StatusOK

	base := responseBaseFields{id: newID("resp_"), model: a.model}
	sequence := 0
	emit := func(kind string, payload map[string]any) bool {
		payload["type"] = kind
		payload["sequence_number"] = sequence
		if err := sseFrame(w, kind, payload); err != nil {
			terminal = "downstream_write_error"
			return false
		}
		sequence++
		flusher.Flush()
		return true
	}

	// response.created opens the turn.  A failure to write it is fatal: nothing
	// else can be delivered either.
	if !emit("response.created", map[string]any{
		"response": base.base("in_progress", nil),
	}) {
		s.fail(a.rec)
		return
	}

	// emitFailed closes a stream that broke after it had already opened.  A
	// response.failed looks like a clean terminal event to Codex, which is
	// exactly what is wanted: the turn ends with a reason rather than hanging.
	emitFailed := func(err error) {
		msg := fmt.Sprintf("[%s] %s", client.Name(), err.Error())
		s.fail(a.rec)
		status, _, _ := upstreamErrorShape(err)
		a.stat.status = status
		s.opts.Logger.Printf("responses stream: client %s: %v", client.Name(), err)
		if !emit("response.failed", map[string]any{
			"response": map[string]any{
				"id":         base.id,
				"object":     "response",
				"created_at": time.Now().Unix(),
				"status":     "failed",
				"model":      base.model,
				"output":     []any{},
				"error": map[string]any{
					"code":    errorCodeFor(err, http.StatusBadGateway),
					"message": msg,
				},
			},
		}) {
			terminal = "downstream_write_error"
		}
	}

	var (
		output       []responseOutput
		textIx       = -1
		reasoningIx  = -1
		toolByIx     = map[int]int{}
		countedTools = map[int]bool{}
		streamErr    error
		finished     bool
	)
	addItem := func(item responseOutput) (int, bool) {
		ix := len(output)
		output = append(output, item)
		wire := map[string]any{"type": item.Type, "id": item.ID, "status": "in_progress"}
		switch item.Type {
		case "message":
			wire["role"] = "assistant"
			wire["content"] = []any{}
		case "reasoning":
			wire["summary"] = []any{}
		case "function_call":
			wire["call_id"] = item.CallID
			wire["name"] = item.Name
			wire["arguments"] = ""
		}
		return ix, emit("response.output_item.added", map[string]any{"output_index": ix, "item": wire})
	}
	addText := func() bool {
		var ok bool
		textIx, ok = addItem(responseOutput{
			Type: "message", ID: "msg_" + base.id, Role: "assistant",
			Content: []responseOutputContent{{Type: "output_text", Annotations: []any{}}},
		})
		return ok && emit("response.content_part.added", map[string]any{
			"item_id": output[textIx].ID, "output_index": textIx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}

receive:
	for {
		if r.Context().Err() != nil {
			terminal = "caller_canceled"
			a.stat.status = http.StatusRequestTimeout
			s.fail(a.rec)
			return
		}
		ev, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if !finished {
					terminal = "unexpected_eof"
					a.stat.status = http.StatusBadGateway
					streamErr = io.ErrUnexpectedEOF
				}
				break
			}
			terminal = "upstream_read_error"
			a.stat.status = http.StatusBadGateway
			streamErr = err
			break
		}
		eventCount++
		a.stat.noteFirstFrame(time.Now())

		switch ev.Type {
		case core.EventDelta:
			if ev.Delta == "" && ev.Reasoning == "" {
				continue
			}
			deltaBytes += len(ev.Delta)
			reasoningBytes += len(ev.Reasoning)
			if ev.Reasoning != "" {
				appendTail(&reasonTail, ev.Reasoning, 200)
				if reasoningIx < 0 {
					var ok bool
					reasoningIx, ok = addItem(responseOutput{
						Type: "reasoning", ID: "rs_" + base.id,
						Summary: []responseOutputSummary{{Type: "summary_text"}},
					})
					if !ok || !emit("response.reasoning_summary_part.added", map[string]any{
						"item_id": output[reasoningIx].ID, "output_index": reasoningIx, "summary_index": 0,
						"part": responseOutputSummary{Type: "summary_text"},
					}) {
						return
					}
				}
				output[reasoningIx].Summary[0].Text += ev.Reasoning
				if !emit("response.reasoning_summary_text.delta", map[string]any{
					"item_id":       output[reasoningIx].ID,
					"output_index":  reasoningIx,
					"summary_index": 0,
					"delta":         ev.Reasoning,
				}) {
					return
				}
			}
			if ev.Delta != "" {
				appendTail(&textTail, ev.Delta, 200)
				if textIx < 0 && !addText() {
					return
				}
				output[textIx].Content[0].Text += ev.Delta
				if !emit("response.output_text.delta", map[string]any{
					"item_id":       output[textIx].ID,
					"output_index":  textIx,
					"content_index": 0,
					"delta":         ev.Delta,
				}) {
					return
				}
			}

		case core.EventToolCall:
			if ev.ToolCall == nil {
				continue
			}
			if !countedTools[ev.ToolCall.Index] {
				countedTools[ev.ToolCall.Index] = true
				toolCalls++
			}
			ix, ok := toolByIx[ev.ToolCall.Index]
			if !ok {
				callID := ev.ToolCall.ID
				if callID == "" {
					callID = newID("call_")
				}
				ix, ok = addItem(responseOutput{
					Type: "function_call", ID: newID("fc_"), CallID: callID, Name: ev.ToolCall.Name,
				})
				if !ok {
					return
				}
				toolByIx[ev.ToolCall.Index] = ix
			}
			tc := &output[ix]
			if ev.ToolCall.ID != "" {
				tc.CallID = ev.ToolCall.ID
			}
			if ev.ToolCall.Name != "" {
				tc.Name = ev.ToolCall.Name
			}
			tc.Arguments += ev.ToolCall.Arguments
			if ev.ToolCall.Arguments != "" {
				if !emit("response.function_call_arguments.delta", map[string]any{
					"item_id": tc.ID, "output_index": ix, "delta": ev.ToolCall.Arguments,
				}) {
					return
				}
			}

		case core.EventUsage:
			if ev.Usage != nil {
				usage = ev.Usage
			}

		case core.EventDone:
			finished = true
			finish = ev.Finish
			break receive

		case core.EventError:
			if ev.Err != nil {
				streamErr = ev.Err
				break receive
			}
		}
	}

	if streamErr != nil {
		if terminal == "in_progress" {
			terminal = "upstream_error_event"
		}
		emitFailed(streamErr)
		return
	}

	// A turn that produced neither visible text nor a tool call is not an
	// answer.  The reasoning-only shape is the failure this gateway keeps
	// chasing: the model thinks for a long time, stops ("finish=stop"), emits
	// no call, and the old code dressed that up as response.completed with an
	// empty message.  Codex accepted the dead turn and showed a half-answer, so
	// the operator had to type 继续.  Reporting a failure is the honest result
	// and the one a client can retry.
	if !sawAnyText(output) && !sawAnyToolCall(output) {
		terminal = "empty_completion"
		emitFailed(errEmptyCompletion())
		return
	}

	for i := range output {
		output[i].Status = "completed"
	}
	final := base.result(finish, output, usage)
	for ix, item := range final.Output {
		switch item.Type {
		case "message":
			if !emit("response.output_text.done", map[string]any{
				"item_id": item.ID, "output_index": ix, "content_index": 0, "text": item.Content[0].Text,
			}) || !emit("response.content_part.done", map[string]any{
				"item_id": item.ID, "output_index": ix, "content_index": 0, "part": item.Content[0],
			}) {
				return
			}
		case "reasoning":
			if !emit("response.reasoning_summary_text.done", map[string]any{
				"item_id": item.ID, "output_index": ix, "summary_index": 0, "text": item.Summary[0].Text,
			}) || !emit("response.reasoning_summary_part.done", map[string]any{
				"item_id": item.ID, "output_index": ix, "summary_index": 0, "part": item.Summary[0],
			}) {
				return
			}
		case "function_call":
			if !emit("response.function_call_arguments.done", map[string]any{
				"item_id": item.ID, "output_index": ix, "arguments": item.Arguments,
			}) {
				return
			}
		}
		if !emit("response.output_item.done", map[string]any{"output_index": ix, "item": item}) {
			return
		}
	}
	if emit("response."+final.Status, map[string]any{"response": final}) {
		if final.Status == "incomplete" {
			terminal = "incomplete"
		} else {
			terminal = "completed"
		}
	} else {
		s.fail(a.rec)
	}
}

// bufferResponse renders a non-streamed response: one response object whose
// output array carries every item the turn produced.
func (s *server) bufferResponse(w http.ResponseWriter, r *http.Request, a emitArgs) {
	client := a.client
	stream := a.stream

	var (
		text      strings.Builder
		reasoning strings.Builder
		toolOrder []int
		toolByIx  = map[int]*core.ToolCall{}
		usage     *core.Usage
		finish    string
		streamErr error
		finished  bool
	)
	defer func() {
		a.rec.setUsage(usage)
		a.stat.noteUsage(usage)
	}()

receive:
	for {
		if r.Context().Err() != nil {
			a.stat.status = http.StatusGatewayTimeout
			s.fail(a.rec)
			writeError(w, http.StatusGatewayTimeout, "upstream_error", "client disconnected")
			return
		}
		ev, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) && !finished {
				streamErr = io.ErrUnexpectedEOF
			} else if !errors.Is(err, io.EOF) {
				streamErr = err
			}
			break
		}
		a.stat.noteFirstFrame(time.Now())
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
				usage = ev.Usage
			}
		case core.EventDone:
			finished = true
			if ev.Finish != "" {
				finish = ev.Finish
			}
			break receive
		case core.EventError:
			if ev.Err != nil {
				streamErr = ev.Err
				break receive
			}
		}
	}

	if streamErr != nil {
		status, _, _ := upstreamErrorShape(streamErr)
		a.stat.status = status
		s.fail(a.rec)
		s.writeUpstreamError(w, client, a.hintCtx, streamErr)
		return
	}

	base := responseBaseFields{id: newID("resp_"), model: a.model}

	// Same rule as the streaming path: a turn with no visible text and no tool
	// call is a dead turn, not an answer, however much reasoning preceded it.
	if text.Len() == 0 && len(toolByIx) == 0 {
		status, _, _ := upstreamErrorShape(errEmptyCompletion())
		a.stat.status = status
		s.fail(a.rec)
		s.writeUpstreamError(w, client, a.hintCtx, errEmptyCompletion())
		return
	}

	var output []responseOutput

	if reasoning.Len() > 0 {
		output = append(output, responseOutput{
			Type:    "reasoning",
			ID:      "rs_" + base.id,
			Status:  "completed",
			Summary: []responseOutputSummary{{Type: "summary_text", Text: reasoning.String()}},
		})
	}
	// An assistant message is always emitted, even when empty, for the same
	// reason the Chat path always carries a content key: a client that validates
	// the item shape rejects a turn with no message.
	output = append(output, responseOutput{
		Type:   "message",
		ID:     "msg_" + base.id,
		Status: "completed",
		Role:   "assistant",
		Content: []responseOutputContent{{
			Type:        "output_text",
			Text:        text.String(),
			Annotations: []any{},
		}},
	})

	sort.Ints(toolOrder)
	for _, ix := range toolOrder {
		tc := toolByIx[ix]
		if tc.Name == "" {
			continue
		}
		callID := tc.ID
		if callID == "" {
			callID = newID("call_")
		}
		output = append(output, responseOutput{
			Type:      "function_call",
			ID:        "fc_" + callID,
			Status:    "completed",
			CallID:    callID,
			Name:      tc.Name,
			Arguments: tc.Arguments,
		})
	}

	a.stat.status = http.StatusOK
	final := base.result(finish, output, usage)
	writeJSON(w, http.StatusOK, final)
}

// appendTail keeps only the last n bytes of s in dst, so a diagnostic log line
// can quote how a turn ended without holding the whole answer.  The cut can fall
// mid-rune, which is acceptable because the value is only ever printed.
func appendTail(dst *strings.Builder, s string, n int) {
	if dst == nil || s == "" || n <= 0 {
		return
	}
	dst.WriteString(s)
	if dst.Len() <= n {
		return
	}
	trimmed := dst.String()[dst.Len()-n:]
	dst.Reset()
	dst.WriteString(trimmed)
}

// errEmptyCompletion is the error reported when a model turn ended without
// producing anything the caller can use: no visible text and no tool call.  It
// is deliberately a distinct message so the operator can tell it apart from a
// transport failure in the log and in the client's error banner.
func errEmptyCompletion() error {
	return errors.New("upstream returned an empty completion: the model ended the turn without text or a tool call")
}

// sawAnyText reports whether any output item carries visible assistant text.
func sawAnyText(output []responseOutput) bool {
	for _, item := range output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Text != "" {
				return true
			}
		}
	}
	return false
}

// sawAnyToolCall reports whether any output item is a function call with a name.
func sawAnyToolCall(output []responseOutput) bool {
	for _, item := range output {
		if item.Type == "function_call" && item.Name != "" {
			return true
		}
	}
	return false
}
