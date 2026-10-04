package openrouter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// --- SSE reading ----------------------------------------------------------
//
// This repository has no shared SSE helper (each vendor's framing quirks are
// its own problem), so the reader is written here.  It is deliberately
// tolerant: `data:` may appear without a space, frames may be separated by CRLF
// or a bare LF, comment lines (`: keep-alive`) and unknown fields are skipped,
// and a final frame that never got its terminating blank line is still
// returned.

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

// next returns one frame.  io.EOF means the upstream closed cleanly.
func (s *sseReader) next() (sseFrame, error) {
	var frame sseFrame
	// sawData, not "some line was seen", decides whether a frame is returned.
	// A block that carries only a comment (": keep-alive") or only an event
	// name has no payload: returning it would hand the caller an empty chunk
	// and would stop io.EOF from ever arriving on a comment-only body.
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
			// A comment, in practice a keep-alive.  Ignored, but it still
			// counts as traffic for the idle watchdog.
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
			// Fields this module does not use.
		default:
			// A vendor that ignores `stream:true` answers with a single JSON
			// body and no `data:` prefix.  A line that is not an SSE field but
			// looks like JSON is treated as data so that answer is not
			// silently dropped.
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

// readLine returns one line without its terminator.  A last line with no
// newline is returned with a nil error, not an error.
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

// --- idle watchdog --------------------------------------------------------
//
// An SSE stream is bounded by the gap BETWEEN frames, not by its total length:
// a long generation is fine, a stalled socket is not.  The watchdog closes the
// body when nothing arrives inside the window, which unblocks the in-flight
// Read with an error instead of letting the request hang forever.

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

// --- chat stream ----------------------------------------------------------

// chatStream turns the vendor's SSE frames into core.Events.
//
// It guarantees: io.EOF exactly once, at most one EventUsage, at most one
// EventDone, and no event at all after the stream has ended.
type chatStream struct {
	client    *Client
	resp      *http.Response
	body      *idleReader
	reader    *sseReader
	accountID string
	cancel    context.CancelFunc

	// holds records whether the caller took an in-flight slot from the pool.
	// Only the holder may give it back; a panel probe never took one.
	holds bool

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

	// toolCalls merges argument fragments by index so drainStream (and the
	// TestAccount probe) can report a complete call.
	toolCalls map[int]*core.ToolCallDelta
	toolOrder []int
}

func newChatStream(c *Client, resp *http.Response, accountID string, cancel context.CancelFunc, holds bool) *chatStream {
	body := newIdleReader(resp.Body, c.cfg.streamIdle())
	return &chatStream{
		client:    c,
		resp:      resp,
		body:      body,
		reader:    newSSEReader(body),
		accountID: accountID,
		cancel:    cancel,
		holds:     holds,
		finish:    "stop",
		toolCalls: make(map[int]*core.ToolCallDelta),
	}
}

// Recv returns the next event.  io.EOF is returned exactly once, after the
// final EventDone.
func (s *chatStream) Recv() (core.Event, error) {
	for {
		// A caller that closed the stream is finished with it: buffered events
		// are discarded rather than handed out after Close.
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
				// A clean upstream close without [DONE] is a normal end: many
				// providers simply stop after the last chunk.
				s.endStream()
				continue
			}
			return s.readFailure(err)
		}
		s.absorb(frame)
	}
}

func (s *chatStream) readFailure(err error) (core.Event, error) {
	// The stream is over either way, so a second Recv answers io.EOF rather
	// than re-reading a closed body.
	s.done = true
	// The request context is read BEFORE the slot is handed back: release()
	// calls cancel(), so asking afterwards would report every failure as a
	// caller cancellation and hide the real cause (an idle stall, say).
	ctxErr := s.ctxErr()
	s.release()
	if ctxErr != nil {
		return core.Event{}, ctxErr
	}
	if s.body.timedOut() {
		msg := fmt.Sprintf("the stream stalled for %s", s.client.cfg.streamIdle())
		s.noteStreamProblem(msg)
		return core.Event{}, core.Fail(clientName, s.accountID, core.FailureUpstream, 0, fmt.Errorf("openrouter: %s", msg))
	}
	msg := truncate(core.Redact(err.Error()), 300)
	s.noteStreamProblem(msg)
	return core.Event{}, core.Fail(clientName, s.accountID, core.FailureUpstream, 0, fmt.Errorf("openrouter: stream read failed: %s", msg))
}

// noteStreamProblem records a mid-stream transport problem without parking the
// credential: a stalled connection says nothing about whether the key is good,
// and parking a healthy key for a flaky socket is worse than no badge at all.
func (s *chatStream) noteStreamProblem(msg string) {
	if s.client == nil || s.accountID == "" {
		return
	}
	s.client.pool.setNote(s.accountID, "stream: "+s.client.scrubFor(s.accountID, msg))
	s.client.persistState()
}

func (s *chatStream) ctxErr() error {
	if s.client == nil || s.cancel == nil {
		return nil
	}
	// The cancel func alone does not expose the cause, so the request context
	// error is read from the response request when it is available.
	if s.resp != nil && s.resp.Request != nil {
		if err := s.resp.Request.Context().Err(); err != nil {
			return err
		}
	}
	return nil
}

// absorb turns one frame into zero or more pending events.
func (s *chatStream) absorb(frame sseFrame) {
	data := strings.TrimSpace(frame.Data)
	if data == "" {
		// Keep-alive or comment.
		return
	}
	if isDonePayload(data) {
		s.endStream()
		return
	}
	var chunk oaiChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		// Not JSON: a vendor keep-alive that was not prefixed with a colon.
		return
	}
	if chunk.Error != nil {
		s.chunkFailure(chunk.Error)
		return
	}
	for _, choice := range chunk.Choices {
		delta := choice.Delta
		if isEmptyDelta(delta) && !isEmptyDelta(choice.Message) {
			// Some upstreams answer a stream request with a single non-streamed
			// message; accepting it keeps the turn from coming back empty.
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

// fragmentOf copies one wire fragment into the event shape.
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

// mergeToolCall accumulates argument fragments.  Arguments arrive split across
// frames, so they are CONCATENATED, never overwritten.
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

// chunkFailure reports a vendor error delivered inside the stream.
func (s *chatStream) chunkFailure(e *oaiChunkError) {
	msg := truncate(core.Redact(strings.TrimSpace(e.Message)), 300)
	if msg == "" {
		msg = "the vendor reported an error inside the stream"
	}
	k := classifyFailure(e.Code, e.Code, msg)
	if k == kindNone {
		k = kindServer
	}
	if s.client != nil {
		s.client.noteFailure(s.accountID, k, msg)
	}
	s.failed = true
	s.pending = append(s.pending, core.Event{
		Type: core.EventError,
		Err:  core.Fail(clientName, s.accountID, coreKindFor(k), e.Code, &upstreamError{Op: "stream", Status: e.Code, Msg: msg}),
	})
	s.endStream()
}

// endStream queues the terminal events exactly once.  A stream that already
// reported an error does not also get a EventDone.
func (s *chatStream) endStream() {
	if s.done {
		return
	}
	s.done = true
	if !s.failed {
		s.pending = append(s.pending, core.Event{Type: core.EventDone, Finish: s.finishReason()})
		s.succeeded = true
		if s.client != nil {
			if acct, ok := s.client.pool.byID(s.accountID); ok {
				s.client.noteSuccess(acct)
			}
		}
	}
}

func (s *chatStream) finishReason() string {
	if strings.TrimSpace(s.finish) == "" {
		return "stop"
	}
	return s.finish
}

// release closes the upstream body, cancels the request context and returns the
// account to the pool exactly once.
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
		if s.client != nil && s.holds && s.accountID != "" {
			s.client.pool.release(s.accountID)
		}
	})
}

// Close is idempotent and safe to call after the stream has ended.
func (s *chatStream) Close() error {
	s.release()
	return s.closeErr
}

// --- test/probe helper ----------------------------------------------------

// drainStream reads a stream to its end and returns the assembled answer.  It
// is the one place the fragmented tool-call arguments are joined, which is what
// makes the panel's TestAccount button show a real call.
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
