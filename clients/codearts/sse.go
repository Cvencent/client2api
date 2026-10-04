package codearts

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

// sse.go is this module's own Server-Sent Events reader.
//
// The repository has no shared SSE helper — every module that needs one writes
// its own — so this file is self-contained and is built in three testable
// layers, the same way clients/qwenwork/sse.go is:
//
//	readFrame   reads one `data:` payload off a reader (the transport shape)
//	parseFrame  turns a payload into an event (the protocol shape)
//	stream      runs the reader on its own goroutine and hands core.Event
//	            values to Recv (the concurrency shape)
//
// Two timeouts are enforced while reading.  The first byte may take up to
// `firstTokenWait` (300 s by default) because the backend queues the request
// before it starts answering; every byte after that must arrive within
// `chunkWait` (600 s by default).  Both match the reference implementation's
// numbers and both are overridable through the environment names it uses.
// A timeout is a terminal error, never a clean end of stream.

// errStreamDone is the internal sentinel for the `[DONE]` frame.  It never
// escapes the module: Recv converts it into io.EOF.
var errStreamDone = errors.New("codearts: stream done")

// errIdleTimeout is what the watchdog returns when the upstream goes quiet.
var errIdleTimeout = errors.New("codearts: upstream stopped sending data (idle timeout)")

// maxFrameBytes bounds one SSE frame so a wedged or hostile upstream cannot
// make the module allocate without limit.
const maxFrameBytes = 1 << 20

// ---------------------------------------------------------------------------
// layer 1: the transport shape
// ---------------------------------------------------------------------------

// readFrame reads the next `data:` payload from r.
//
// It returns errStreamDone for the terminal `[DONE]` frame and io.EOF at the
// clean end of the stream.  Comment lines (`: keep-alive`) are skipped, a
// multi-line data block is joined with newlines, and a bare `{…}` line is
// accepted as a payload because some intermediaries strip the `data:` prefix.
//
// A body that ends immediately after a complete payload — with no blank line
// to close the event — yields that payload rather than an error: the vendor
// closes the connection after the last frame, and whether it wrote a trailing
// blank line is not something the caller should have to care about.
func readFrame(r *bufio.Reader) (string, error) {
	var (
		data   []string
		sawAny bool
	)
	for {
		line, err := r.ReadString('\n')
		if line == "" && err != nil {
			// The stream ended between events, or right after one.
			if sawAny && len(data) > 0 {
				payload := strings.Join(data, "\n")
				if payload == "[DONE]" {
					return "", errStreamDone
				}
				return payload, nil
			}
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			// Blank line: the end of one event.
			if !sawAny {
				continue
			}
			payload := strings.Join(data, "\n")
			if payload == "[DONE]" {
				return "", errStreamDone
			}
			return payload, nil
		case strings.HasPrefix(line, ":"):
			// A comment, which is also the keep-alive shape.
			continue
		case strings.HasPrefix(line, "data:"):
			sawAny = true
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") ||
			strings.HasPrefix(line, "retry:"):
			// Named events, ids and retry hints are not part of this
			// protocol; consuming the line is enough.
			continue
		case strings.HasPrefix(strings.TrimSpace(line), "{"):
			// A payload with no `data:` prefix at all.
			sawAny = true
			data = append(data, strings.TrimSpace(line))
		default:
			// Anything else is ignored rather than treated as an error:
			// unknown lines must not abort a stream that is otherwise fine.
			continue
		}
		if err != nil {
			// The stream ended mid-event.
			if sawAny && len(data) > 0 {
				payload := strings.Join(data, "\n")
				if payload == "[DONE]" {
					return "", errStreamDone
				}
				return payload, nil
			}
			return "", err
		}
		if total := len(strings.Join(data, "\n")); total > maxFrameBytes {
			return "", fmt.Errorf("codearts: an SSE frame exceeded %d bytes", maxFrameBytes)
		}
	}
}

// ---------------------------------------------------------------------------
// layer 2: the protocol shape
// ---------------------------------------------------------------------------

// sseError is the error a frame can carry inside a 200 response.
type sseError struct {
	Code    string `json:"error_code"`
	Message string `json:"error_msg"`
	// Some shapes nest the same pair under `error`.
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// code returns the first non-empty error code the frame carries.
func (e *sseError) code() string {
	return firstNonEmpty(e.Code, e.Error.Code)
}

// message returns the first non-empty error message the frame carries.
func (e *sseError) message() string {
	return firstNonEmpty(e.Message, e.Error.Message)
}

// present reports whether the frame carried an error at all.
func (e *sseError) present() bool {
	return e != nil && (e.code() != "" || e.message() != "")
}

type sseDelta struct {
	Content          string        `json:"content"`
	ReasoningContent string        `json:"reasoning_content"`
	ToolCalls        []sseToolCall `json:"tool_calls"`
}

type sseToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type sseChoice struct {
	Index        *int     `json:"index"`
	Delta        sseDelta `json:"delta"`
	FinishReason string   `json:"finish_reason"`
}

type sseUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	PromptDetails    struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheHitTokens   int `json:"prompt_cache_hit_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// sseFrame is one decoded chunk.
type sseFrame struct {
	ID      string      `json:"id"`
	Model   string      `json:"model"`
	Choices []sseChoice `json:"choices"`
	Usage   *sseUsage   `json:"usage"`
	sseError
}

// parseFrame decodes one SSE payload.  A payload that is not JSON is an error:
// the vendor does not send anything else on this stream, so treating it as a
// no-op would hide a real protocol change.
func parseFrame(payload string) (*sseFrame, error) {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil, nil
	}
	var f sseFrame
	if err := json.Unmarshal([]byte(payload), &f); err != nil {
		return nil, fmt.Errorf("codearts: an SSE frame is not JSON: %s", cleanErrorText(payload))
	}
	return &f, nil
}

// eventFor turns a decoded frame into zero or more core.Events.
//
// An error frame that names a queue/rate-limit condition is reported as an
// error carrying the upstream's own text; the caller decides whether to retry
// the whole request.  A frame with neither text nor an error is a no-op — the
// backend sends keep-alive-ish frames with an empty delta and they must not
// produce an empty Event.
func eventFor(f *sseFrame) ([]core.Event, error) {
	if f == nil {
		return nil, nil
	}
	if f.present() {
		msg := cleanErrorText(firstNonEmpty(f.message(), f.code()))
		if msg == "" {
			msg = "the upstream reported an error inside the stream"
		}
		if code := f.code(); code != "" {
			msg = code + ": " + msg
		}
		return nil, &streamError{code: f.code(), message: msg}
	}

	var events []core.Event
	for _, ch := range f.Choices {
		if text := ch.Delta.Content; text != "" {
			events = append(events, core.Event{Type: core.EventDelta, Delta: text})
		}
		if text := ch.Delta.ReasoningContent; text != "" {
			events = append(events, core.Event{Type: core.EventDelta, Reasoning: text})
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
	if f.Usage != nil {
		u := usageFrom(f.Usage)
		events = append(events, core.Event{Type: core.EventUsage, Usage: &u})
	}
	return events, nil
}

// usageFrom normalises the vendor's usage object.
func usageFrom(u *sseUsage) core.Usage {
	if u == nil {
		return core.Usage{}
	}
	out := core.Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		ReasoningTokens:  u.CompletionDetails.ReasoningTokens,
		CachedTokens:     firstNonZero(u.PromptDetails.CachedTokens, u.PromptDetails.CacheHitTokens),
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	return out
}

// streamError is an error the upstream delivered inside an HTTP 200 stream.
type streamError struct {
	code    string
	message string
}

func (e *streamError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

// queueRetryable reports whether this in-stream error is one the caller should
// retry the whole request for.  CodeArts reports a per-minute token limit as
// `InferHub.ModelArts.81111.429` inside a 200 stream rather than as a 429.
func (e *streamError) queueRetryable() bool {
	if e == nil {
		return false
	}
	return isQueueErrorCode(e.code) || isQueueErrorCode(e.message)
}

// ---------------------------------------------------------------------------
// layer 3: the concurrency shape
// ---------------------------------------------------------------------------

// stream is the core.Stream this module hands to the gateway.
//
// It owns the response body and the context that bounds the request, and it
// releases both exactly once — whether the caller drains the stream, closes it
// early, or the request times out.
type stream struct {
	events chan core.Event
	done   chan struct{}

	release func()
	body    io.Closer

	closeOnce sync.Once

	mu   sync.Mutex
	err  error
	fin  string
	stop bool
}

// newStream builds a stream around an already-open response body.
//
// `body` must be the response body; `release` is called once when the stream
// finishes, and is where the caller puts whatever has to be undone (usually
// the chat context's cancel function).
func newStream(ctx context.Context, body io.ReadCloser, firstWait, chunkWait time.Duration, release func()) *stream {
	s := &stream{
		events:  make(chan core.Event, 32),
		done:    make(chan struct{}),
		release: release,
		body:    body,
	}
	wd := newWatchdogBody(body, firstWait, chunkWait)
	core.GoSafe("codearts stream", nil, func() {
		s.run(ctx, wd)
	})
	return s
}

// Recv returns the next event.  It returns io.EOF exactly once at the clean
// end of the stream; every other error is terminal and carries the upstream's
// own message.
func (s *stream) Recv() (core.Event, error) {
	select {
	case ev, ok := <-s.events:
		if ok {
			return ev, nil
		}
	case <-s.done:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		err := s.err
		s.err = nil // the error is delivered once, then the stream reads clean
		return core.Event{}, err
	}
	return core.Event{}, io.EOF
}

// Close releases the response body and the request context.  It is safe to
// call more than once, and safe to call while a reader goroutine is running.
func (s *stream) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		if s.body != nil {
			_ = s.body.Close()
		}
		if release := s.takeRelease(); release != nil {
			release()
		}
	})
	return nil
}

// takeRelease hands the release function to exactly one caller.  Both Close
// and the reader goroutine want it, and whichever arrives second must get nil:
// running it twice would cancel a context that a later stream may still be
// using, and reading the field from two goroutines at once would be a race.
func (s *stream) takeRelease() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	release := s.release
	s.release = nil
	return release
}

// run is the reader loop.  It is the only writer of the events channel and the
// only place the terminal error or finish reason is set.
func (s *stream) run(ctx context.Context, body io.Reader) {
	defer func() {
		// Releasing here rather than only in Close is what keeps a fully
		// drained stream from leaking its request context.
		if release := s.takeRelease(); release != nil {
			release()
		}
	}()

	br := bufio.NewReaderSize(body, 64<<10)
	var finish string
	for {
		if err := ctx.Err(); err != nil {
			s.finishWith(err, finish)
			return
		}
		payload, err := readFrame(br)
		if errors.Is(err, errStreamDone) {
			s.finishWith(nil, finish)
			return
		}
		if errors.Is(err, io.EOF) {
			// A stream that ended without [DONE] still ended cleanly: the
			// vendor closes the body after the final frame.
			s.finishWith(nil, finish)
			return
		}
		if err != nil {
			s.finishWith(err, finish)
			return
		}
		frame, err := parseFrame(payload)
		if err != nil {
			s.finishWith(err, finish)
			return
		}
		events, err := eventFor(frame)
		if err != nil {
			s.finishWith(err, finish)
			return
		}
		for _, ev := range events {
			if !s.send(ev) {
				s.finishWith(ctx.Err(), finish)
				return
			}
		}
		for _, ch := range frame.Choices {
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
	}
}

// send delivers one event, aborting when the stream is closed.
func (s *stream) send(ev core.Event) bool {
	select {
	case <-s.done:
		return false
	default:
	}
	select {
	case s.events <- ev:
		return true
	case <-s.done:
		return false
	}
}

// finishWith records the outcome, emits the terminal done event and closes the
// channel, exactly once.
func (s *stream) finishWith(err error, finish string) {
	s.mu.Lock()
	if s.stop {
		s.mu.Unlock()
		return
	}
	s.stop = true
	if err != nil {
		s.err = err
	}
	s.fin = finish
	s.mu.Unlock()

	if err == nil {
		// Only a clean end produces a done event; a failure produces the
		// error from Recv instead, so the gateway never sees "done" followed
		// by a failure.
		select {
		case s.events <- core.Event{Type: core.EventDone, Finish: finish}:
		case <-s.done:
		}
	}
	close(s.events)
	if err != nil {
		s.Close()
	}
}

// ---------------------------------------------------------------------------
// idle watchdog
// ---------------------------------------------------------------------------

// watchdogBody closes the connection when the upstream goes quiet.
//
// The timeout is not fixed: the first byte may take much longer than the ones
// after it, because the backend queues the request before it starts answering.
// The reader therefore arms `first` and switches to `next` after the first
// successful read.
type watchdogBody struct {
	rc    io.ReadCloser
	mu    sync.Mutex
	idle  time.Duration
	chunk time.Duration
	t     *time.Timer
	dead  bool
}

func newWatchdogBody(rc io.ReadCloser, first, next time.Duration) *watchdogBody {
	w := &watchdogBody{rc: rc, idle: first, chunk: next}
	w.t = time.AfterFunc(first, w.fire)
	return w
}

// fire is what happens when the timer expires: the body is closed, which
// unblocks any pending Read with an error.
func (w *watchdogBody) fire() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dead {
		return
	}
	w.dead = true
	_ = w.rc.Close()
}

// Read resets the timer and switches to the post-first-byte timeout.
func (w *watchdogBody) Read(p []byte) (int, error) {
	n, err := w.rc.Read(p)
	if n > 0 {
		w.mu.Lock()
		if !w.dead {
			w.idle = w.nextIdle()
			if w.t != nil {
				w.t.Reset(w.idle)
			}
		}
		w.mu.Unlock()
	}
	if err != nil {
		w.mu.Lock()
		dead := w.dead
		w.mu.Unlock()
		if dead {
			return n, errIdleTimeout
		}
	}
	return n, err
}

// Close stops the timer and closes the underlying body.
func (w *watchdogBody) Close() error {
	w.mu.Lock()
	w.dead = true
	if w.t != nil {
		w.t.Stop()
	}
	w.mu.Unlock()
	return w.rc.Close()
}

// nextIdle is the timeout to arm after data arrives: the chunk timeout.
func (w *watchdogBody) nextIdle() time.Duration {
	if w.chunk > 0 {
		return w.chunk
	}
	return w.idle
}
