package openaicompat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// --- SSE reading ----------------------------------------------------------

type sseFrame struct {
	Event string
	Data  string
}

type sseReader struct {
	r *bufio.Reader
}

func newSSEReader(r io.Reader) *sseReader {
	return &sseReader{r: bufio.NewReaderSize(r, 64<<10)}
}

func (s *sseReader) next() (sseFrame, error) {
	var frame sseFrame
	sawData := false
	for {
		line, err := s.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if sawData {
					return frame, nil
				}
				return sseFrame{}, io.EOF
			}
			return sseFrame{}, err
		}
		if line == "" {
			if sawData {
				return frame, nil
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "data:"):
			v := strings.TrimPrefix(line, "data:")
			v = strings.TrimPrefix(v, " ")
			if frame.Data != "" {
				frame.Data += "\n"
			}
			frame.Data += v
			sawData = true
		case strings.HasPrefix(line, "event:"):
			frame.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "id:"), strings.HasPrefix(line, "retry:"):
		default:
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
				if frame.Data != "" {
					frame.Data += "\n"
				}
				frame.Data += trimmed
				sawData = true
			}
		}
	}
}

func (s *sseReader) readLine() (string, error) {
	line, err := s.r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line != "" {
			return strings.TrimRight(line, "\r\n"), nil
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// --- idle watchdog ---------------------------------------------------------

type idleReader struct {
	rc    io.ReadCloser
	idle  time.Duration
	timer *time.Timer
	mu    sync.Mutex
	stall bool
	done  bool
}

func newIdleReader(rc io.ReadCloser, idle time.Duration) *idleReader {
	r := &idleReader{rc: rc, idle: idle}
	if idle > 0 {
		r.timer = time.AfterFunc(idle, r.fire)
	}
	return r
}

func (r *idleReader) fire() {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.stall = true
	r.mu.Unlock()
	_ = r.rc.Close()
}

func (r *idleReader) arm() {
	if r.timer == nil {
		return
	}
	r.mu.Lock()
	done := r.done
	r.mu.Unlock()
	if done {
		return
	}
	r.timer.Reset(r.idle)
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	if n > 0 {
		r.arm()
	}
	return n, err
}

func (r *idleReader) timedOut() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stall
}

func (r *idleReader) Close() error {
	r.mu.Lock()
	already := r.done
	r.done = true
	r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
	}
	if already {
		return nil
	}
	return r.rc.Close()
}

// --- chat stream -------------------------------------------------------------

type chatStream struct {
	client    *Client
	resp      *httpResponse
	body      *idleReader
	reader    *sseReader
	prov      ProviderConfig
	accountID string
	cancel    context.CancelFunc

	closeOnce sync.Once
	closeErr  error
	released  bool

	done      bool
	failed    bool
	succeeded bool

	pending   []core.Event
	usage     *core.Usage
	usageSent bool
	finish    string

	toolCalls map[int]*core.ToolCallDelta
	toolOrder []int
}

// httpResponse is the stream's handle on the underlying HTTP response; it is
// an alias so this file does not need a separate import surface.
type httpResponse = httpResponseAlias

func newChatStream(c *Client, resp *httpResponseAlias, prov ProviderConfig, accountID string, cancel context.CancelFunc) *chatStream {
	body := newIdleReader(resp.Body, c.cfg.streamIdle())
	return &chatStream{
		client:    c,
		resp:      resp,
		body:      body,
		reader:    newSSEReader(body),
		prov:      prov,
		accountID: accountID,
		cancel:    cancel,
		finish:    "stop",
		toolCalls: make(map[int]*core.ToolCallDelta),
	}
}

func (s *chatStream) Recv() (core.Event, error) {
	for {
		if s.released {
			return core.Event{}, io.EOF
		}
		if len(s.pending) > 0 {
			ev := s.pending[0]
			s.pending = s.pending[1:]
			return ev, nil
		}
		if s.done {
			s.release()
			return core.Event{}, io.EOF
		}
		frame, err := s.reader.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.endStream()
				continue
			}
			return s.readFailure(err)
		}
		s.absorb(frame)
	}
}

func (s *chatStream) readFailure(err error) (core.Event, error) {
	s.done = true
	ctxErr := s.ctxErr()
	s.release()
	if ctxErr != nil {
		return core.Event{}, ctxErr
	}
	if s.body.timedOut() {
		msg := fmt.Sprintf("the stream stalled for %s", s.client.cfg.streamIdle())
		return core.Event{}, core.Fail(clientName, s.accountID, core.FailureUpstream, 0, fmt.Errorf("openai-compat: %s", msg))
	}
	msg := truncate(core.Redact(err.Error()), 300)
	return core.Event{}, core.Fail(clientName, s.accountID, core.FailureUpstream, 0, fmt.Errorf("openai-compat: stream read failed: %s", msg))
}

func (s *chatStream) ctxErr() error {
	if s.cancel == nil {
		return nil
	}
	if s.resp != nil && s.resp.Request != nil {
		if err := s.resp.Request.Context().Err(); err != nil {
			return err
		}
	}
	return nil
}

func (s *chatStream) absorb(frame sseFrame) {
	data := strings.TrimSpace(frame.Data)
	if data == "" {
		return
	}
	if isDonePayload(data) {
		s.endStream()
		return
	}
	var chunk oaiChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return
	}
	if chunk.Error != nil {
		s.chunkFailure(chunk.Error)
		return
	}
	for _, choice := range chunk.Choices {
		delta := choice.Delta
		if isEmptyDelta(delta) && !isEmptyDelta(choice.Message) {
			delta = choice.Message
		}
		if text := delta.textOf(); text != "" {
			s.pending = append(s.pending, core.Event{Type: core.EventDelta, Delta: text})
		}
		if r := delta.reasoningOf(); r != "" {
			s.pending = append(s.pending, core.Event{Type: core.EventDelta, Reasoning: r})
		}
		if delta.Refusal != "" {
			s.pending = append(s.pending, core.Event{Type: core.EventDelta, Delta: delta.Refusal})
		}
		for _, frag := range delta.ToolCalls {
			s.mergeToolCall(frag)
			s.pending = append(s.pending, core.Event{Type: core.EventToolCall, ToolCall: fragmentOf(frag)})
		}
		if choice.FinishReason != nil && strings.TrimSpace(*choice.FinishReason) != "" {
			s.finish = normalizeFinish(*choice.FinishReason)
		}
	}
	if chunk.Usage != nil {
		if u := chunk.Usage.toCore(); u != nil && !s.usageSent {
			s.usage = u
			s.usageSent = true
			s.pending = append(s.pending, core.Event{Type: core.EventUsage, Usage: u})
		}
	}
}

func isEmptyDelta(d oaiDelta) bool {
	return len(d.Content) == 0 && d.Reasoning == "" && d.Refusal == "" && len(d.ToolCalls) == 0 && d.Role == ""
}

func fragmentOf(frag oaiToolCall) *core.ToolCallDelta {
	idx := 0
	if frag.Index != nil {
		idx = *frag.Index
	}
	return &core.ToolCallDelta{
		Index:     idx,
		ID:        frag.ID,
		Name:      frag.Function.Name,
		Arguments: frag.Function.Arguments,
	}
}

func (s *chatStream) mergeToolCall(frag oaiToolCall) {
	idx := 0
	if frag.Index != nil {
		idx = *frag.Index
	} else if len(s.toolOrder) > 0 {
		idx = s.toolOrder[len(s.toolOrder)-1]
	}
	call, ok := s.toolCalls[idx]
	if !ok {
		call = &core.ToolCallDelta{Index: idx}
		s.toolCalls[idx] = call
		s.toolOrder = append(s.toolOrder, idx)
	}
	if frag.ID != "" {
		call.ID = frag.ID
	}
	if frag.Function.Name != "" {
		call.Name = frag.Function.Name
	}
	call.Arguments += frag.Function.Arguments
}

func (s *chatStream) chunkFailure(e *oaiChunkError) {
	msg := truncate(core.Redact(strings.TrimSpace(e.Message)), 300)
	if msg == "" {
		msg = "the vendor reported an error inside the stream"
	}
	code, _ := toInt(e.Code)
	k := classifyFailure(code, msg)
	if k == kindNone {
		k = kindServer
	}
	s.failed = true
	s.pending = append(s.pending, core.Event{
		Type: core.EventError,
		Err:  core.Fail(clientName, s.accountID, coreKindFor(k), code, &upstreamError{Op: "stream", Status: code, Msg: msg}),
	})
	s.endStream()
}

func (s *chatStream) endStream() {
	if s.done {
		return
	}
	s.done = true
	if !s.failed {
		s.pending = append(s.pending, core.Event{Type: core.EventDone, Finish: s.finishReason()})
		s.succeeded = true
	}
}

func (s *chatStream) finishReason() string {
	if strings.TrimSpace(s.finish) == "" {
		return "stop"
	}
	return s.finish
}

func (s *chatStream) release() {
	s.closeOnce.Do(func() {
		s.released = true
		if s.body != nil {
			s.closeErr = s.body.Close()
		} else if s.resp != nil {
			s.closeErr = s.resp.Body.Close()
		}
		if s.cancel != nil {
			s.cancel()
		}
	})
}

// Close is idempotent and safe to call after the stream has ended.
func (s *chatStream) Close() error {
	s.release()
	return s.closeErr
}

// --- drain helper -------------------------------------------------------------

// drainStream reads a stream to its end and returns the assembled answer.
func drainStream(s core.Stream) (text, reasoning string, calls []core.ToolCallDelta, usage *core.Usage, finish string, err error) {
	defer s.Close()
	var b strings.Builder
	var rb strings.Builder
	byIndex := make(map[int]*core.ToolCallDelta)
	var order []int
	for {
		ev, rerr := s.Recv()
		if errors.Is(rerr, io.EOF) {
			return b.String(), rb.String(), orderedCalls(order, byIndex), usage, finish, nil
		}
		if rerr != nil {
			return b.String(), rb.String(), orderedCalls(order, byIndex), usage, finish, rerr
		}
		switch ev.Type {
		case core.EventDelta:
			b.WriteString(ev.Delta)
			rb.WriteString(ev.Reasoning)
		case core.EventToolCall:
			if ev.ToolCall == nil {
				continue
			}
			call, ok := byIndex[ev.ToolCall.Index]
			if !ok {
				call = &core.ToolCallDelta{Index: ev.ToolCall.Index}
				byIndex[ev.ToolCall.Index] = call
				order = append(order, ev.ToolCall.Index)
			}
			if ev.ToolCall.ID != "" {
				call.ID = ev.ToolCall.ID
			}
			if ev.ToolCall.Name != "" {
				call.Name = ev.ToolCall.Name
			}
			call.Arguments += ev.ToolCall.Arguments
		case core.EventUsage:
			usage = ev.Usage
		case core.EventDone:
			finish = ev.Finish
		case core.EventError:
			if ev.Err != nil {
				return b.String(), rb.String(), orderedCalls(order, byIndex), usage, finish, ev.Err
			}
		}
	}
}

func orderedCalls(order []int, byIndex map[int]*core.ToolCallDelta) []core.ToolCallDelta {
	if len(order) == 0 {
		return nil
	}
	out := make([]core.ToolCallDelta, 0, len(order))
	for _, i := range order {
		out = append(out, *byIndex[i])
	}
	return out
}
