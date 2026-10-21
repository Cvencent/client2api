package codearts

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
	PromptDetails    *struct {
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
	}
	if u.PromptDetails != nil {
		out.CachedTokens = firstNonZero(u.PromptDetails.CachedTokens, u.PromptDetails.CacheHitTokens)
		out.CachedTokensKnown = true
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
	}
	return out
}

// streamError is an error the upstream delivered inside an HTTP 200 stream.
//
// CodeArts folds its refusals into the body, so the HTTP status carries no
// verdict and the vendor's own code and wording are the only evidence.  The
// kind is read from them here because an in-stream refusal the module cannot
// name is a refusal the gateway cannot park, rotate away from or attribute.
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

// kind reports how this in-stream refusal must be handled.
//
// The per-minute token ceiling (`InferHub.ModelArts.8111x.429`) is a plain rate
// limit: another credential has its own budget, so the caller may rotate.  The
// concurrency-queue code stays its own kind -- an in-stream refusal carries no
// queue position to poll for, so waiting on the queue-status endpoint would
// stall the request over a condition the endpoint cannot describe.
func (e *streamError) kind() errKind {
	if e == nil {
		return kindNone
	}
	switch {
	case isRateLimitErrorCode(e.code) || isRateLimitErrorCode(e.message):
		return kindRateLimit
	case isQueueErrorCode(e.code) || isQueueErrorCode(e.message):
		return kindQueue
	case isAuthBody(0, e.code) || isAuthBody(0, e.message):
		return kindAuth
	case reQuotaBody.MatchString(e.code) || reQuotaBody.MatchString(e.message):
		return kindQuota
	}
	return kindNone
}

// status is the HTTP status the same refusal would have carried had the vendor
// not folded it into a 200 response.  A zero means the module has no better
// answer than its own default.
func (e *streamError) status() int {
	switch e.kind() {
	case kindRateLimit:
		return http.StatusTooManyRequests
	case kindAuth:
		return http.StatusUnauthorized
	case kindQuota:
		return http.StatusPaymentRequired
	}
	return 0
}

// asStreamError unwraps a *streamError out of an error chain.
func asStreamError(err error) (*streamError, bool) {
	var se *streamError
	if errors.As(err, &se) {
		return se, true
	}
	return nil, false
}

// queueRetryable reports whether this in-stream error is one the caller should
// retry the whole request for.  CodeArts reports a per-minute token limit as
// `InferHub.ModelArts.81111.429` inside a 200 stream rather than as a 429.
func (e *streamError) queueRetryable() bool {
	if e == nil {
		return false
	}
	k := e.kind()
	return k == kindRateLimit || k == kindQueue
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

	// head holds events decoded from frames that were read BEFORE this stream
	// was built -- the first frame is consumed by the account retry loop so a
	// refusal the vendor folded into the 200 body is classified there rather
	// than surfacing to the caller.  They are delivered first, in order, so the
	// caller still sees every frame the vendor sent.
	head []core.Event

	// headErr is an in-stream refusal the retry loop chose NOT to act on: it
	// is neither a credential nor a rate problem, so no other account could
	// answer it and the vendor's own words must reach the caller unchanged.
	// It is delivered after head, exactly where the frame sat in the stream.
	headErr error

	// drained marks a stream the probe built after already reading everything
	// there was to read.  It has no reader goroutine, so Recv must do the
	// reader loop's cleanup -- releasing the request context -- itself.
	drained bool
}

// newStream builds a stream around an already-open response body.
//
// `body` must be the response body; `release` is called once when the stream
// finishes, and is where the caller puts whatever has to be undone (usually
// the chat context's cancel function).
func newStream(ctx context.Context, body io.ReadCloser, firstWait, chunkWait time.Duration, release func()) *stream {
	wd := newWatchdogBody(body, firstWait, chunkWait)
	return newStreamOver(ctx, wd, bufio.NewReaderSize(wd, 64<<10), release, nil)
}

// newStreamOver builds a stream that continues reading from br rather than from
// the start of body, with head delivered before anything br produces.
//
// br must already wrap body, because a caller that inspected the first frame
// must hand the SAME reader along: buffering the body twice would drop the
// bytes the first frame's read pulled past its own line.
func newStreamOver(ctx context.Context, body io.ReadCloser, br *bufio.Reader, release func(), head []core.Event) *stream {
	s := &stream{
		events:  make(chan core.Event, 32),
		done:    make(chan struct{}),
		release: release,
		body:    body,
		head:    head,
	}
	core.GoSafe("codearts stream", nil, func() {
		s.run(ctx, br)
	})
	return s
}

// newProbedStream reads the first frame before building the stream.
//
// CodeArts folds refusals into a 200 response, so "the request opened" says
// nothing about whether it will be served.  Reading one frame here is what lets
// the account retry loop rotate away from a rate-limited credential instead of
// handing the caller a turn that dies on its first read.  A frame that is not a
// refusal is replayed, so nothing is lost.
func newProbedStream(ctx context.Context, body io.ReadCloser, firstWait, chunkWait time.Duration, release func(), acctID string) (*stream, error) {
	wd := newWatchdogBody(body, firstWait, chunkWait)
	br := bufio.NewReaderSize(wd, 64<<10)

	payload, err := readFrame(br)
	if err != nil {
		if errors.Is(err, errStreamDone) || errors.Is(err, io.EOF) {
			// The vendor answered with nothing at all.  The reader loop
			// reports that as one done event, so the probe does the same
			// rather than inventing a different verdict.
			return newDrainedStream(wd, release, []core.Event{{Type: core.EventDone}}), nil
		}
		// A transport failure is delivered at Recv exactly as it always was:
		// the probe merely read it a moment earlier.
		return newTerminalStream(wd, release, err), nil
	}
	frame, err := parseFrame(payload)
	if err != nil {
		return newTerminalStream(wd, release, err), nil
	}
	events, err := eventFor(frame)
	if err != nil {
		// The vendor refused the request inside the body.  The per-minute token
		// ceiling is the one refusal another credential clears, so it is handed
		// back to the retry loop BEFORE any frame reaches the client: nothing
		// has been rendered yet, so rotating costs the caller nothing.
		//
		// Every other in-stream refusal stays what it always was -- a terminal
		// error the caller reads from Recv.  Rotating for one would either
		// re-send content the vendor objected to or park the request on a queue
		// this side cannot observe.  The module still classifies what it
		// recognises so the gateway can park the pair and explain the failure.
		if se, ok := asStreamError(err); ok && se.kind() == kindRateLimit {
			_ = wd.Close()
			return nil, err
		}
		return newTerminalStream(wd, release, classifyStreamRefusal(acctID, err)), nil
	}
	if len(events) == 0 {
		// A keep-alive-ish frame carries no event.  The reader loop treats it
		// as a no-op, so the probe must too, and simply keep reading.
		return newStreamOver(ctx, wd, br, release, nil), nil
	}
	return newStreamOver(ctx, wd, br, release, events), nil
}

// classifyStreamRefusal gives a named in-stream refusal the classification the
// gateway needs in order to park the (account, model) pair and explain the
// failure, while leaving the vendor's own wording on the error.  A refusal the
// module cannot name is passed through untouched, so an unknown code keeps
// reaching the caller verbatim instead of being guessed at.
func classifyStreamRefusal(acctID string, err error) error {
	se, ok := asStreamError(err)
	if !ok || se.kind() == kindNone {
		return err
	}
	return core.Fail("codearts", acctID, failureKind(se.kind()), se.status(), errors.New(core.Redact(se.Error())))
}

// newDrainedStream builds a stream that replays events and then reads clean.
// It has no reader goroutine: there is nothing left to read.
func newDrainedStream(body io.Closer, release func(), events []core.Event) *stream {
	s := &stream{
		events:  make(chan core.Event),
		done:    make(chan struct{}),
		release: release,
		body:    body,
		head:    events,
		drained: true,
	}
	close(s.events)
	return s
}

// newTerminalStream builds a stream whose one terminal error is delivered on
// the first Recv, after which it reads clean.  It is how a refusal the probe
// read -- or a transport failure it hit -- reaches the caller unchanged.
func newTerminalStream(body io.Closer, release func(), err error) *stream {
	s := newDrainedStream(body, release, nil)
	s.headErr = err
	return s
}

// Recv returns the next event.  It returns io.EOF exactly once at the clean
// end of the stream; every other error is terminal and carries the upstream's
// own message.
func (s *stream) Recv() (core.Event, error) {
	if ev, ok := s.takeHead(); ok {
		return ev, nil
	}
	if err := s.takeHeadErr(); err != nil {
		// A seeded refusal is terminal, exactly as a reader-loop error is:
		// nothing more will arrive, so the request context goes back now.
		s.releaseNow()
		return core.Event{}, err
	}
	select {
	case ev, ok := <-s.events:
		if ok {
			return ev, nil
		}
	case <-s.done:
	}
	s.mu.Lock()
	err := s.err
	s.err = nil // the error is delivered once, then the stream reads clean
	drained := s.drained
	s.mu.Unlock()
	if err != nil {
		s.releaseNow()
		return core.Event{}, err
	}
	if drained {
		// No reader goroutine ever ran for this stream, so the release the
		// reader loop would have performed belongs to Recv.
		s.releaseNow()
	}
	return core.Event{}, io.EOF
}

// releaseNow returns the request context once.  Both Close and the reader loop
// call it, so a stream that is drained and then closed does not cancel twice.
func (s *stream) releaseNow() {
	if release := s.takeRelease(); release != nil {
		release()
	}
}

// takeHead returns the next already-decoded event, if any.
func (s *stream) takeHead() (core.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.head) == 0 {
		return core.Event{}, false
	}
	ev := s.head[0]
	s.head = s.head[1:]
	return ev, true
}

// takeHeadErr delivers a seeded refusal exactly once, then reads clean.
func (s *stream) takeHeadErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.headErr
	s.headErr = nil
	return err
}

// Close releases the response body and the request context.  It is safe to
// call more than once, and safe to call while a reader goroutine is running.
func (s *stream) Close() error {
	s.closeOnce.Do(func() {
		close(s.done)
		if s.body != nil {
			_ = s.body.Close()
		}
		s.releaseNow()
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
func (s *stream) run(ctx context.Context, br *bufio.Reader) {
	defer func() {
		// Releasing here rather than only in Close is what keeps a fully
		// drained stream from leaking its request context.
		if release := s.takeRelease(); release != nil {
			release()
		}
	}()

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
