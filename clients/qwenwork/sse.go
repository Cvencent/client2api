package qwenwork

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"client2api/internal/core"
)

// sse.go turns the upstream event stream into core.Events.
//
// Three layers, each testable on its own:
//
//	readFrames   -- SSE framing (data: lines, blank-line events, [DONE])
//	unwrapFrame  -- the vendor's envelope around each payload
//	parseChunk   -- the OpenAI-shaped chunk inside that envelope
//
// The vendor wraps errors inside a 200 response: a frame can carry
// {body: "...", statusCodeValue: 429} or {error: {...}}.  Both have to become a
// core.EventError, because the HTTP status is long gone by then.

var (
	// errStreamDone is the internal sentinel a frame handler returns for
	// "[DONE]"; readFrames turns it back into a clean nil.
	errStreamDone = errors.New("qwenwork: stream done")

	errFrameTooLarge = errors.New("qwenwork: upstream SSE frame exceeded the size limit")
	errEmptyStream   = errors.New("qwenwork: upstream stream ended without any data")
)

// maxFrameBytes bounds one SSE frame so a hostile or broken upstream cannot
// make us buffer without limit.
const maxFrameBytes = 1 << 20

// ---------------------------------------------------------------------------
// framing
// ---------------------------------------------------------------------------

// readFrames reads SSE events and hands each payload to onPayload.  Comment
// lines (`: ping` heartbeats) are ignored, multi-line data is joined with
// newlines, and a bare JSON line is accepted for gateways that omit the
// `data:` prefix.  It returns nil at EOF.
func readFrames(ctx context.Context, r io.Reader, onPayload func(payload string) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var data strings.Builder

	// stopped records that the handler answered errStreamDone, i.e. that it saw
	// the end-of-stream marker.  The loop then stops instead of delivering
	// whatever the upstream keeps sending after the marker.
	stopped := false

	flush := func() error {
		if data.Len() == 0 {
			return nil
		}
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" {
			return nil
		}
		err := onPayload(payload)
		if err == nil {
			return nil
		}
		if errors.Is(err, errStreamDone) {
			stopped = true
			return nil
		}
		return err
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, readErr := br.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if err := flush(); err != nil {
					return err
				}
			case strings.HasPrefix(line, ":"):
				// heartbeat or comment
			case strings.HasPrefix(line, "data:"):
				v := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(v)
				if data.Len() > maxFrameBytes {
					return errFrameTooLarge
				}
			case strings.HasPrefix(line, "{"):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(line)
				if data.Len() > maxFrameBytes {
					return errFrameTooLarge
				}
				if json.Valid([]byte(data.String())) {
					if err := flush(); err != nil {
						return err
					}
				}
			}
		}
		if stopped {
			return nil
		}
		if readErr != nil {
			if err := flush(); err != nil {
				return err
			}
			if stopped {
				return nil
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

// ---------------------------------------------------------------------------
// envelope
// ---------------------------------------------------------------------------

// unwrapFrame normalises one SSE payload into the raw OpenAI chunk it carries.
// It reports ok=false for frames that carry nothing (empty, "{}", "[DONE]"),
// and returns a non-empty message when the frame is an error.
func unwrapFrame(payload string) (raw string, ok bool, errMsg string) {
	p := strings.TrimSpace(payload)
	if p == "" || p == "[DONE]" || p == "{}" || p == "null" {
		return "", false, ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(p), &obj); err != nil {
		// Not JSON at all: the payload itself is the message.
		return p, true, ""
	}
	if obj == nil {
		return "", false, ""
	}
	if _, has := obj["choices"]; has {
		return p, true, ""
	}
	if t, _ := obj["object"].(string); t == "chat.completion.chunk" || t == "chat.completion" {
		return p, true, ""
	}
	if _, has := obj["error"]; has {
		return "", false, errorMessageOf(obj["error"], 0)
	}
	if code, has := numberField(obj, "statusCodeValue", "status_code", "status"); has && code >= 400 {
		return "", false, bodyCodeMessage(p, int(code))
	}
	body, has := obj["body"]
	if !has {
		// Not an envelope.  The vendor also sends bare chunks that carry no
		// choices at all, such as a usage-only frame at the end of a stream;
		// parseChunk is what decides whether it means anything.
		return p, true, ""
	}
	switch v := body.(type) {
	case string:
		s := strings.TrimSpace(v)
		if s == "" || s == "[DONE]" || s == "{}" || s == "null" || s == "[]" {
			return "", false, ""
		}
		if msg := bodyCodeMessage(s, 0); msg != "" {
			return "", false, msg
		}
		return s, true, ""
	case map[string]any:
		if len(v) == 0 {
			return "", false, ""
		}
		if _, has := v["error"]; has {
			return "", false, errorMessageOf(v["error"], 0)
		}
		b, err := json.Marshal(v)
		if err != nil {
			return "", false, ""
		}
		return string(b), true, ""
	default:
		return "", false, ""
	}
}

// bodyCodeMessage extracts a human message from an upstream error body.  The
// vendor signals a dead credential with the *string* code "400"/"401"/"403",
// which is why the code is compared as a string here.
func bodyCodeMessage(raw string, status int) string {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		if status >= 400 {
			return "upstream " + itoa(status) + ": " + cleanErrorText(raw)
		}
		return ""
	}
	if code, has := obj["code"]; has {
		if s, isStr := code.(string); isStr && (s == "400" || s == "401" || s == "403") {
			if msg, _ := obj["message"].(string); msg != "" {
				return "upstream " + s + ": " + cleanErrorText(msg)
			}
			return "upstream " + s
		}
	}
	if status >= 400 {
		if msg, _ := obj["message"].(string); msg != "" {
			return "upstream " + itoa(status) + ": " + cleanErrorText(msg)
		}
		if msg, _ := obj["msg"].(string); msg != "" {
			return "upstream " + itoa(status) + ": " + cleanErrorText(msg)
		}
		return "upstream " + itoa(status) + ": " + cleanErrorText(raw)
	}
	return ""
}

// errorMessageOf renders an {"error": ...} field, which may be a string or an
// object with message/type/code.
func errorMessageOf(v any, status int) string {
	switch e := v.(type) {
	case string:
		if e == "" {
			return ""
		}
		return cleanErrorText(e)
	case map[string]any:
		msg, _ := e["message"].(string)
		if msg == "" {
			msg, _ = e["msg"].(string)
		}
		code := ""
		if c, has := e["code"]; has {
			code = strings.TrimSpace(jsonValue(c))
		}
		switch {
		case msg != "" && code != "" && code != `""`:
			return cleanErrorText(msg) + " (code " + strings.Trim(code, `"`) + ")"
		case msg != "":
			return cleanErrorText(msg)
		case code != "" && code != `""`:
			return "upstream error code " + strings.Trim(code, `"`)
		case status > 0:
			return "upstream " + itoa(status)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// chunks
// ---------------------------------------------------------------------------

// sseChunk is one decoded upstream chunk, flattened.
type sseChunk struct {
	Delta     string
	Reasoning string
	ToolCalls []core.ToolCallDelta
	Finish    string
	Usage     *core.Usage
	Err       string
}

// empty reports whether a chunk carries nothing worth emitting.
func (c sseChunk) empty() bool {
	return c.Err == "" && c.Delta == "" && c.Reasoning == "" && len(c.ToolCalls) == 0 && c.Finish == "" && c.Usage == nil
}

// parseChunk decodes the OpenAI-shaped payload inside an unwrapped frame.
func parseChunk(raw string) (sseChunk, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil || obj == nil {
		return sseChunk{}, false
	}
	var out sseChunk
	if e, has := obj["error"]; has {
		out.Err = errorMessageOf(e, 0)
		if out.Err == "" {
			out.Err = "upstream reported an error"
		}
		return out, true
	}
	if _, has := obj["code"]; has {
		if msg := bodyCodeMessage(raw, 0); msg != "" {
			out.Err = msg
			return out, true
		}
	}
	if u := usageFromAny(obj["usage"]); u != nil {
		out.Usage = u
	}
	choices, _ := obj["choices"].([]any)
	if len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if fr, ok := choice["finish_reason"].(string); ok {
				out.Finish = fr
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				out.Delta = stringField(delta, "content")
				out.Reasoning = firstStringField(delta, "reasoning_content", "reasoning")
				out.ToolCalls = toolCallDeltas(delta["tool_calls"])
			} else if msg, ok := choice["message"].(map[string]any); ok {
				// some gateways answer a "stream" with one whole message
				out.Delta = stringField(msg, "content")
				out.Reasoning = firstStringField(msg, "reasoning_content", "reasoning")
				out.ToolCalls = toolCallDeltas(msg["tool_calls"])
			}
		}
	}
	return out, true
}

// toolCallDeltas flattens OpenAI-style tool-call deltas.
func toolCallDeltas(v any) []core.ToolCallDelta {
	list, _ := v.([]any)
	if len(list) == 0 {
		return nil
	}
	out := make([]core.ToolCallDelta, 0, len(list))
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		d := core.ToolCallDelta{Index: i}
		if n, ok := numberField(m, "index"); ok {
			d.Index = int(n)
		}
		d.ID = stringField(m, "id")
		if fn, ok := m["function"].(map[string]any); ok {
			d.Name = stringField(fn, "name")
			d.Arguments = stringField(fn, "arguments")
		}
		if d.ID == "" && d.Name == "" && d.Arguments == "" {
			continue
		}
		out = append(out, d)
	}
	return out
}

// usageFromAny reads a usage block, probing the aliases different builds of the
// vendor use for the cache and reasoning counters.  It returns nil when every
// counter is zero, so a caller can simply skip the event.
func usageFromAny(v any) *core.Usage {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	u := &core.Usage{}
	if n, ok := numberField(m, "prompt_tokens", "input_tokens"); ok {
		u.PromptTokens = int(n)
	}
	if n, ok := numberField(m, "completion_tokens", "output_tokens"); ok {
		u.CompletionTokens = int(n)
	}
	if n, ok := numberField(m, "total_tokens"); ok {
		u.TotalTokens = int(n)
	}
	if u.TotalTokens == 0 && (u.PromptTokens > 0 || u.CompletionTokens > 0) {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	if n, ok := numberField(m, "reasoning_tokens"); ok {
		u.ReasoningTokens = int(n)
	}
	if details, ok := m["completion_tokens_details"].(map[string]any); ok && u.ReasoningTokens == 0 {
		if n, ok := numberField(details, "reasoning_tokens"); ok {
			u.ReasoningTokens = int(n)
		}
	}
	for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if details, ok := m[key].(map[string]any); ok {
			if n, ok := numberField(details, "cached_tokens"); ok {
				u.CachedTokens = int(n)
				break
			}
		}
	}
	if u.CachedTokens == 0 {
		if n, ok := numberField(m, "prompt_cache_hit_tokens", "cache_read_input_tokens", "cached_tokens"); ok {
			u.CachedTokens = int(n)
		}
	}
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 {
		return nil
	}
	return u
}

// normalizeFinish maps the upstream finish reason onto the four values the core
// promises to emit.  sawToolCall decides the default, because a stream that
// produced tool calls and then ended without a reason means "tool_calls".
func normalizeFinish(finish string, sawToolCall bool) string {
	switch strings.ToLower(strings.TrimSpace(finish)) {
	case "stop", "end_turn", "eos":
		return "stop"
	case "length", "max_tokens", "max_output_tokens":
		return "length"
	case "tool_calls", "tool_call", "function_call":
		return "tool_calls"
	case "content_filter":
		return "content_filter"
	}
	if sawToolCall {
		return "tool_calls"
	}
	return "stop"
}

// ---------------------------------------------------------------------------
// the stream
// ---------------------------------------------------------------------------

// qwenStream is the core.Stream over one upstream response body.  The reader
// goroutine owns the body; Recv only drains a channel, so a caller that stops
// reading early still lets Close() tear everything down.
type qwenStream struct {
	ctx    context.Context
	cancel context.CancelFunc
	body   io.ReadCloser
	ch     chan core.Event

	// release cancels the per-request timeout context that Chat derived from
	// the caller's context.  It is invoked as soon as the reader goroutine
	// finishes, not only from Close, so a long-lived process does not pile up
	// timers for streams nobody will read again.
	release context.CancelFunc

	once     sync.Once
	closeErr error
}

// newQwenStream wraps one upstream response body.  release may be nil.
func newQwenStream(parent context.Context, release context.CancelFunc, body io.ReadCloser) *qwenStream {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	s := &qwenStream{
		ctx:     ctx,
		cancel:  cancel,
		body:    body,
		ch:      make(chan core.Event, 32),
		release: release,
	}
	// GoSafe is the last-resort net: run reports a panic as an event, but a
	// panic in that report path would still take the whole process down.
	core.GoSafe("qwenwork stream", nil, s.run)
	return s
}

// Recv returns the next event, or io.EOF exactly once at the end.
func (s *qwenStream) Recv() (core.Event, error) {
	ev, ok := <-s.ch
	if !ok {
		return core.Event{}, io.EOF
	}
	return ev, nil
}

// Close is idempotent and safe to call from any goroutine.
func (s *qwenStream) Close() error {
	s.once.Do(func() {
		s.cancel()
		if s.release != nil {
			s.release()
		}
		s.closeErr = s.body.Close()
	})
	return s.closeErr
}

// emit delivers one event unless the stream has been cancelled or the consumer
// has gone away.
func (s *qwenStream) emit(ev core.Event) bool {
	select {
	case s.ch <- ev:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// run reads the upstream body to its end and publishes the resulting events.
func (s *qwenStream) run() {
	defer close(s.ch)
	if s.release != nil {
		// Every event is already buffered by the time this runs, so cancelling
		// the request context here cannot truncate what the caller reads.
		defer s.release()
	}
	// Registered last, so it runs first — while s.ch is still open and s.ctx is
	// still alive, which is what lets the panic be reported as an event instead
	// of surfacing to the caller as a stream that simply stopped.
	defer func() {
		if r := recover(); r != nil {
			s.emit(core.Event{
				Type: core.EventError,
				Err:  fmt.Errorf("qwenwork: the stream reader panicked: %v", r),
			})
		}
	}()

	var (
		usage    *core.Usage
		finish   string
		sawTool  bool
		sawData  bool
		fatalErr error
	)

	err := readFrames(s.ctx, s.body, func(payload string) error {
		if strings.TrimSpace(payload) == "[DONE]" {
			return errStreamDone
		}
		raw, ok, errMsg := unwrapFrame(payload)
		if errMsg != "" {
			fatalErr = errors.New(errMsg)
			return errStreamDone
		}
		if !ok {
			return nil
		}
		chunk, ok := parseChunk(raw)
		if !ok || chunk.empty() {
			return nil
		}
		sawData = true
		if chunk.Err != "" {
			fatalErr = errors.New(chunk.Err)
			return errStreamDone
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if chunk.Finish != "" {
			finish = chunk.Finish
		}
		for i := range chunk.ToolCalls {
			sawTool = true
			tc := chunk.ToolCalls[i]
			if !s.emit(core.Event{Type: core.EventToolCall, ToolCall: &tc}) {
				return errStreamDone
			}
		}
		if chunk.Delta != "" || chunk.Reasoning != "" {
			if !s.emit(core.Event{Type: core.EventDelta, Delta: chunk.Delta, Reasoning: chunk.Reasoning}) {
				return errStreamDone
			}
		}
		return nil
	})

	switch {
	case s.ctx.Err() != nil:
		return
	case fatalErr != nil:
		s.emit(core.Event{Type: core.EventError, Err: fatalErr})
		return
	case err != nil:
		s.emit(core.Event{Type: core.EventError, Err: err})
		return
	case !sawData:
		s.emit(core.Event{Type: core.EventError, Err: errEmptyStream})
		return
	}

	if usage != nil {
		s.emit(core.Event{Type: core.EventUsage, Usage: usage})
	}
	s.emit(core.Event{Type: core.EventDone, Finish: normalizeFinish(finish, sawTool)})
}

// ---------------------------------------------------------------------------
// small JSON helpers
// ---------------------------------------------------------------------------

// stringField reads a string field, tolerating a null.
func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// firstStringField returns the first non-empty string field among keys.
func firstStringField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := stringField(m, k); s != "" {
			return s
		}
	}
	return ""
}

// numberField returns the first numeric field among keys.  A numeric string is
// accepted too: the vendor quotes some counters ("prompt_tokens": "12").
func numberField(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		if f, ok := asFloat(m[k]); ok {
			return f, true
		}
	}
	return 0, false
}
