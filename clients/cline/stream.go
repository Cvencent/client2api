package cline

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"

	"client2api/internal/core"
)

// maxFrameBytes bounds one SSE frame so a hostile or broken stream cannot
// exhaust memory.
const maxFrameBytes = 1 << 20

// errEmptyStream is reported when the upstream closed without sending anything.
var errEmptyStream = errors.New("cline: upstream closed the stream without sending any data")

// --- frame reading ---------------------------------------------------------

// frame is one decoded SSE frame.
type frame struct {
	Event string
	Data  string
}

// parseFrame decodes one raw SSE block.  Comments and unknown fields are
// ignored, multiple data: lines are joined with a newline, and a frame whose
// payload is a bare JSON object (no data: prefix) is accepted too -- some
// gateways emit that.
func parseFrame(block string) (frame, bool) {
	var f frame
	var data []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment / keep-alive
		}
		if strings.HasPrefix(line, "event:") {
			f.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "{") {
			data = append(data, strings.TrimSpace(line))
		}
	}
	if len(data) == 0 {
		return frame{}, false
	}
	f.Data = strings.Join(data, "\n")
	return f, true
}

// readFrames walks an SSE body, calling onPayload for each frame's data.  A
// [DONE] payload stops the walk without being handed to the callback.
func readFrames(ctx context.Context, r io.Reader, onPayload func(payload string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), maxFrameBytes)
	var block strings.Builder
	flush := func() error {
		if block.Len() == 0 {
			return nil
		}
		text := block.String()
		block.Reset()
		f, ok := parseFrame(text)
		if !ok {
			return nil
		}
		if isDonePayload(f.Data) {
			return io.EOF
		}
		return onPayload(f.Data)
	}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		block.WriteString(line)
		block.WriteString("\n")
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// A final frame with no trailing blank line is still a frame.
	return flush()
}

// isDonePayload recognises the end-of-stream sentinel.
func isDonePayload(data string) bool {
	s := strings.TrimSpace(data)
	return strings.EqualFold(s, "[DONE]")
}

// --- chunk decoding --------------------------------------------------------

// parseChunk decodes one SSE payload into the events it carries.
func parseChunk(raw string) ([]core.Event, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var chunk oaiChunk
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		// An unparseable frame is not fatal: a vendor heartbeat or a stray
		// line must not kill a stream that is otherwise healthy.
		return nil, nil
	}
	if chunk.Error != nil {
		return []core.Event{{
			Type: core.EventError,
			Err:  errors.New("cline: upstream error: " + cleanErrorText(chunk.Error.Message)),
		}}, nil
	}
	var events []core.Event
	for _, ch := range chunk.Choices {
		if d := flattenContent(ch.Delta.Content); d != "" {
			events = append(events, core.Event{Type: core.EventDelta, Delta: d})
		}
		if r := firstNonEmpty(ch.Delta.ReasoningContent, ch.Delta.Reasoning); r != "" {
			events = append(events, core.Event{Type: core.EventDelta, Reasoning: r})
		}
		if ch.Message != nil {
			if d := flattenAny(ch.Message.Content); d != "" {
				events = append(events, core.Event{Type: core.EventDelta, Delta: d})
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			events = append(events, core.Event{
				Type: core.EventToolCall,
				ToolCall: &core.ToolCallDelta{
					Index:     idx,
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
	}
	if chunk.Usage != nil {
		u := chunk.Usage.toCore()
		events = append(events, core.Event{Type: core.EventUsage, Usage: &u})
	}
	return events, nil
}

// flattenContent renders a delta's content member, which may be a bare string
// or an array of parts.
func flattenContent(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
		return ""
	}
	var parts []oaiContentPart
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// flattenAny renders a content member that arrived as an interface{} rather
// than a json.RawMessage.
func flattenAny(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.RawMessage:
		return flattenContent(t)
	case []byte:
		return flattenContent(t)
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return flattenContent(raw)
	}
}

// --- the stream ------------------------------------------------------------

// clineStream is the core.Stream over an upstream SSE body.
type clineStream struct {
	ctx     context.Context
	cancel  context.CancelFunc
	release context.CancelFunc
	body    io.ReadCloser
	ch      chan core.Event
	once    sync.Once
	closeMu sync.Mutex
	closeEr error
}

// newClineStream starts a stream over body.  release is the cancel func of the
// per-request timeout context; it is called when the stream ends.
func newClineStream(ctx context.Context, release context.CancelFunc, body io.ReadCloser) *clineStream {
	s := &clineStream{
		ctx:     ctx,
		body:    body,
		ch:      make(chan core.Event, 32),
		release: release,
	}
	core.GoSafe("cline stream", nil, s.run)
	return s
}

// Recv returns the next event, or io.EOF exactly once at a clean end.
func (s *clineStream) Recv() (core.Event, error) {
	ev, ok := <-s.ch
	if !ok {
		return core.Event{}, io.EOF
	}
	if ev.Type == core.EventError && ev.Err != nil {
		return ev, ev.Err
	}
	return ev, nil
}

// Close is idempotent: it cancels the request, closes the body and releases the
// per-request timeout.
func (s *clineStream) Close() error {
	s.once.Do(func() {
		s.closeMu.Lock()
		if s.cancel != nil {
			s.cancel()
		}
		if s.body != nil {
			s.closeEr = s.body.Close()
		}
		s.closeMu.Unlock()
		if s.release != nil {
			s.release()
		}
	})
	return s.closeEr
}

// emit delivers an event unless the caller has gone away.
func (s *clineStream) emit(ev core.Event) bool {
	select {
	case s.ch <- ev:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// run reads the body and emits events until it ends.
func (s *clineStream) run() {
	defer close(s.ch)
	defer func() {
		if r := recover(); r != nil {
			s.emit(core.Event{Type: core.EventError, Err: errors.New("cline: stream panicked")})
		}
	}()

	var (
		sawData     bool
		sawToolCall bool
		finish      string
		usage       *core.Usage
	)

	err := readFrames(s.ctx, s.body, func(payload string) error {
		events, err := parseChunk(payload)
		if err != nil {
			return err
		}
		for _, ev := range events {
			switch ev.Type {
			case core.EventDelta:
				sawData = true
				if !s.emit(ev) {
					return s.ctx.Err()
				}
			case core.EventToolCall:
				sawData = true
				sawToolCall = true
				if !s.emit(ev) {
					return s.ctx.Err()
				}
			case core.EventUsage:
				if ev.Usage != nil {
					u := *ev.Usage
					usage = &u
				}
			case core.EventError:
				return ev.Err
			}
		}
		// The finish reason rides on the same chunk as the last delta, so it is
		// read from the raw payload rather than from a synthesised event.
		if f := finishReasonOf(payload); f != "" {
			finish = f
		}
		return nil
	})

	if err != nil && !errors.Is(err, io.EOF) && s.ctx.Err() == nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// The caller went away; nothing to report.
		} else {
			s.emit(core.Event{Type: core.EventError, Err: err})
			return
		}
	}
	if s.ctx.Err() != nil {
		return
	}
	if !sawData {
		s.emit(core.Event{Type: core.EventError, Err: errEmptyStream})
		return
	}
	if usage != nil {
		s.emit(core.Event{Type: core.EventUsage, Usage: usage})
	}
	s.emit(core.Event{Type: core.EventDone, Finish: normalizeFinish(finish, sawToolCall)})
}

// finishReasonOf digs the first non-empty finish_reason out of a raw payload.
func finishReasonOf(payload string) string {
	var chunk struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &chunk); err != nil {
		return ""
	}
	for _, c := range chunk.Choices {
		if c.FinishReason != nil && strings.TrimSpace(*c.FinishReason) != "" {
			return strings.TrimSpace(*c.FinishReason)
		}
	}
	return ""
}

// normalizeFinish maps the vendor's finish reason onto the shared vocabulary.
func normalizeFinish(reason string, sawToolCall bool) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "stop", "end_turn", "eos":
		return "stop"
	case "length", "max_tokens":
		return "length"
	case "tool_calls", "function_call":
		return "tool_calls"
	case "content_filter":
		return "content_filter"
	}
	if sawToolCall {
		return "tool_calls"
	}
	if reason == "" {
		return "stop"
	}
	return reason
}
