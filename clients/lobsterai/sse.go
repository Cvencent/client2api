package lobsterai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"client2api/internal/core"
)

// sseFrame is one event-stream frame.
type sseFrame struct {
	Event string
	Data  string
}

// sseReader reads server-sent events.
//
// It is tolerant on purpose, because the vendor's stream is not spec-perfect:
//
//   - `data:` may arrive with NO space after the colon.  The spec says there is
//     one; this upstream has been measured emitting `data:{"id":...}`, and a
//     reader that insists on the space silently returns nothing at all.
//   - frames may be separated by CRLF or by a bare LF.
//   - the last frame may be missing its terminating blank line.
//   - comment lines (`: keep-alive`) and unknown fields are skipped.
type sseReader struct {
	r *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(r, 64<<10)}
}

// next returns the next frame.  It returns io.EOF only when the stream is
// exhausted; a final frame without a trailing blank line is still returned.
func (s *sseReader) next() (sseFrame, error) {
	frame := sseFrame{}
	var data []string
	saw := false
	for {
		line, err := s.readLine()
		if err != nil {
			if saw {
				frame.Data = strings.Join(data, "\n")
				return frame, nil
			}
			return sseFrame{}, err
		}
		if line == "" {
			if !saw {
				continue
			}
			frame.Data = strings.Join(data, "\n")
			return frame, nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "data":
			data = append(data, value)
			saw = true
		case "event":
			frame.Event = value
			saw = true
		default:
			// id, retry, and whatever else this upstream invents.
		}
	}
}

// readLine returns one line without its terminator.  A last line with no
// newline is returned with a nil error, not as EOF.
func (s *sseReader) readLine() (string, error) {
	line, err := s.r.ReadString('\n')
	if err != nil {
		if len(line) > 0 {
			return strings.TrimRight(line, "\r\n"), nil
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// chatStream is the core.Stream over one upstream SSE response.
//
// It owns the HTTP response body, the account's in-flight slot and the request
// context's cancel function, and releases all three exactly once.
type chatStream struct {
	client    *Client
	resp      *http.Response
	reader    *sseReader
	accountID string
	account   *accountRecord
	cancel    context.CancelFunc

	closeOnce sync.Once
	closeErr  error

	// rawDone is set once the upstream stream is exhausted or a [DONE] was
	// seen, so Recv answers io.EOF exactly once.
	rawDone   bool
	succeeded bool

	// Aggregation state for a caller that asked for one answer.
	aggregate bool
	aggDone   bool
	aggText   strings.Builder
	aggReason strings.Builder
	aggCalls  []core.ToolCallDelta
	aggUsage  *core.Usage
	aggFinish string

	// pending queues the events one frame produces.
	pending []core.Event

	// Per-stream bookkeeping.  sawContent is what stops the message-vs-delta
	// fallback from concatenating the same answer twice.
	sawContent bool
	finish     string
	toolOrder  []int
	toolCalls  map[int]*core.ToolCallDelta
}

func newChatStream(client *Client, resp *http.Response, acct *accountRecord, cancel context.CancelFunc, aggregate bool) *chatStream {
	return &chatStream{
		client:    client,
		resp:      resp,
		reader:    newSSEReader(resp.Body),
		accountID: acct.ID,
		account:   acct,
		cancel:    cancel,
		aggregate: aggregate,
		toolCalls: map[int]*core.ToolCallDelta{},
	}
}

// Recv returns the next event.  It returns io.EOF exactly once, after the final
// EventDone.
func (s *chatStream) Recv() (core.Event, error) {
	if s.aggregate {
		return s.recvAggregated()
	}
	return s.recvRaw()
}

// Close is idempotent: it closes the body, releases the account slot and
// cancels the request context.
func (s *chatStream) Close() error {
	s.closeOnce.Do(func() {
		if s.resp != nil && s.resp.Body != nil {
			s.closeErr = s.resp.Body.Close()
		}
		if s.client != nil {
			s.client.pool.release(s.accountID)
		}
		if s.cancel != nil {
			s.cancel()
		}
	})
	return s.closeErr
}

// recvRaw is the streaming path: one upstream frame may produce several events.
func (s *chatStream) recvRaw() (core.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.rawDone {
			return core.Event{}, io.EOF
		}
		frame, err := s.reader.next()
		if err != nil {
			s.rawDone = true
			if err == io.EOF {
				// The upstream closed without a [DONE].  That is a fact about
				// the stream, not a failure: report a normal end carrying
				// whatever finish reason was seen.
				return s.endEvent(), nil
			}
			return core.Event{}, err
		}
		s.absorb(frame)
	}
}

// recvAggregated buffers the whole stream and hands the caller one answer.
//
// The upstream refuses stream:false, so a non-streaming caller is served by
// asking for a stream anyway and re-assembling it here.  The re-assembled shape
// is deliberately the same one a streaming caller would produce if it
// concatenated everything: one delta with the full text, one tool_call event
// per call, then usage and done.
func (s *chatStream) recvAggregated() (core.Event, error) {
	for {
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.aggDone {
			return core.Event{}, io.EOF
		}
		ev, err := s.recvRaw()
		if err != nil {
			if err == io.EOF {
				s.aggDone = true
				s.pending = s.aggregated()
				continue
			}
			return core.Event{}, err
		}
		switch ev.Type {
		case core.EventDelta:
			s.aggText.WriteString(ev.Delta)
			s.aggReason.WriteString(ev.Reasoning)
		case core.EventToolCall:
			if ev.ToolCall != nil {
				s.aggCalls = append(s.aggCalls, *ev.ToolCall)
			}
		case core.EventUsage:
			s.aggUsage = ev.Usage
		case core.EventError:
			return ev, nil
		case core.EventDone:
			s.aggFinish = ev.Finish
			s.aggDone = true
			s.pending = s.aggregated()
		}
	}
}

// aggregated renders the buffered stream as the event sequence a non-streaming
// caller gets.
func (s *chatStream) aggregated() []core.Event {
	out := make([]core.Event, 0, len(s.aggCalls)+3)
	if s.aggText.Len() > 0 || s.aggReason.Len() > 0 {
		out = append(out, core.Event{
			Type:      core.EventDelta,
			Delta:     s.aggText.String(),
			Reasoning: s.aggReason.String(),
		})
	}
	for i := range s.aggCalls {
		call := s.aggCalls[i]
		out = append(out, core.Event{Type: core.EventToolCall, ToolCall: &call})
	}
	if s.aggUsage != nil {
		out = append(out, core.Event{Type: core.EventUsage, Usage: s.aggUsage})
	}
	out = append(out, core.Event{Type: core.EventDone, Finish: s.aggFinish})
	return out
}

// endEvent emits the terminal event and teaches the pool that the account
// served a request.
func (s *chatStream) endEvent() core.Event {
	if !s.succeeded {
		s.succeeded = true
		if s.client != nil {
			s.client.noteSuccess(s.account)
		}
	}
	return core.Event{Type: core.EventDone, Finish: s.finish}
}

// absorb turns one frame into zero or more queued events.
func (s *chatStream) absorb(frame sseFrame) {
	data := strings.TrimSpace(frame.Data)
	if data == "" {
		return
	}
	if isDonePayload(data) {
		s.rawDone = true
		s.pending = append(s.pending, s.endEvent())
		return
	}
	var chunk oaiChunk
	dec := json.NewDecoder(strings.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&chunk); err != nil {
		// A frame that is not JSON is a keep-alive, not a failure.  Dropping it
		// is what keeps one stray newline from killing a good answer.
		return
	}
	if chunk.Error != nil {
		msg := firstNonEmpty(chunk.Error.Message, chunk.Error.Type, "upstream reported an error on the stream")
		s.pending = append(s.pending, core.Event{
			Type: core.EventError,
			Err:  fmt.Errorf("%s: %s", clientName, msg),
		})
		return
	}
	for i := range chunk.Choices {
		s.absorbChoice(&chunk.Choices[i])
	}
	if chunk.Usage != nil {
		usage := chunk.Usage.toCore()
		s.pending = append(s.pending, core.Event{Type: core.EventUsage, Usage: &usage})
	}
}

// absorbChoice maps one choice.
func (s *chatStream) absorbChoice(choice *oaiChoice) {
	if choice == nil {
		return
	}
	var (
		text      string
		reasoning string
		calls     []oaiToolCall
	)
	if choice.Delta != nil {
		text = choice.Delta.textOf()
		reasoning = choice.Delta.reasoningOf()
		calls = choice.Delta.ToolCalls
	}
	// Some frames carry the whole answer under "message" instead of a delta.
	// Only take it when no delta content has been seen at all: a stream that
	// carries both would otherwise yield the answer twice.
	if text == "" && reasoning == "" && len(calls) == 0 && choice.Message != nil && !s.sawContent {
		text = choice.Message.textOf()
		reasoning = choice.Message.reasoningOf()
		calls = choice.Message.ToolCalls
	}
	if text == "" && choice.Text != "" {
		text = choice.Text
	}
	if text != "" {
		s.sawContent = true
	}
	if text != "" || reasoning != "" {
		s.pending = append(s.pending, core.Event{Type: core.EventDelta, Delta: text, Reasoning: reasoning})
	}
	for _, frag := range calls {
		s.pending = append(s.pending, core.Event{Type: core.EventToolCall, ToolCall: s.mergeToolCall(frag)})
	}
	if choice.FinishReason != nil {
		if f := normalizeFinish(*choice.FinishReason); f != "" {
			s.finish = f
		}
	}
}

// mergeToolCall tracks a tool call across its fragments and returns the delta to
// forward.
//
// The upstream splits a call over several frames: the first carries the id, the
// type and the function name, the rest carry only a slice of the arguments
// string.  Arguments therefore have to be CONCATENATED, never overwritten --
// which is exactly the bug the vendor's own client had.
func (s *chatStream) mergeToolCall(frag oaiToolCall) *core.ToolCallDelta {
	idx := 0
	switch {
	case frag.Index != nil:
		idx = *frag.Index
	case len(s.toolOrder) > 0:
		// A fragment with no index belongs to the call in progress.
		idx = s.toolOrder[len(s.toolOrder)-1]
	}
	merged, ok := s.toolCalls[idx]
	if !ok {
		merged = &core.ToolCallDelta{Index: idx}
		s.toolCalls[idx] = merged
		s.toolOrder = append(s.toolOrder, idx)
	}
	if frag.ID != "" {
		merged.ID = frag.ID
	}
	if frag.Function.Name != "" {
		merged.Name = frag.Function.Name
	}
	merged.Arguments += frag.Function.Arguments

	return &core.ToolCallDelta{
		Index:     idx,
		ID:        frag.ID,
		Name:      frag.Function.Name,
		Arguments: frag.Function.Arguments,
	}
}

// drainSSE reads a whole SSE response into a single event list.  It exists for
// the module's own tests and for TestAccount, which needs the answer rather than
// a stream.
func drainSSE(s core.Stream) (text, reasoning string, calls []core.ToolCallDelta, usage *core.Usage, finish string, err error) {
	defer s.Close()

	// Tool-call fragments arrive split across frames: the first carries the id
	// and the name, the rest carry only a slice of the arguments, which must be
	// concatenated.  Collecting each fragment as its own call would hand the
	// caller a broken call with half an argument, so they are merged by index
	// exactly as the streaming path merges them.
	var (
		order   []int
		byIndex = map[int]*core.ToolCallDelta{}
	)
	defer func() {
		if len(order) == 0 {
			return
		}
		out := make([]core.ToolCallDelta, 0, len(order))
		for _, idx := range order {
			out = append(out, *byIndex[idx])
		}
		calls = out
	}()

	for {
		ev, recvErr := s.Recv()
		if recvErr != nil {
			if recvErr == io.EOF {
				return text, reasoning, calls, usage, finish, nil
			}
			return text, reasoning, calls, usage, finish, recvErr
		}
		switch ev.Type {
		case core.EventDelta:
			text += ev.Delta
			reasoning += ev.Reasoning
		case core.EventToolCall:
			if ev.ToolCall == nil {
				continue
			}
			frag := *ev.ToolCall
			merged, ok := byIndex[frag.Index]
			if !ok {
				cp := frag
				byIndex[frag.Index] = &cp
				order = append(order, frag.Index)
				continue
			}
			if merged.ID == "" {
				merged.ID = frag.ID
			}
			if merged.Name == "" {
				merged.Name = frag.Name
			}
			merged.Arguments += frag.Arguments
		case core.EventUsage:
			usage = ev.Usage
		case core.EventError:
			return text, reasoning, calls, usage, finish, ev.Err
		case core.EventDone:
			finish = ev.Finish
		}
	}
}
