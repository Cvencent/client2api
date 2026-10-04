package kimi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"client2api/internal/core"
)

// stream is the core.Stream over one CLI child process.
//
// Lifecycle: startStream starts the process and runs run() in a goroutine.
// run() waits for the process, drains whatever is left of the NDJSON, emits the
// terminal events and closes the event channel.  Recv() returns io.EOF once the
// channel is closed; Close() (or the first io.EOF) tears the process down,
// releases the concurrency slot and deletes the request's media files exactly
// once.
type stream struct {
	ctx    context.Context
	cancel context.CancelFunc
	cmd    *exec.Cmd
	client *Client
	media  *mediaSet

	// accountID is the panel account this run was made as: the imported binding
	// that supplied the executable, or cli-login when it came from anywhere
	// else.  The process outcome is remembered against it (see health.go).
	accountID string

	events   chan core.Event
	finished chan struct{}
	out      *ndjsonWriter
	errTail  *tailBuffer

	once sync.Once

	// State owned by the single reader goroutine (run/handleLine) plus the
	// terminal flush.  textBuf accumulates assistant text so that a tool-call
	// envelope can be recognised structurally before any of it is shown.
	textBuf   strings.Builder
	usage     *core.Usage
	usageSent bool
	calls     int
	output    bool
}

// Recv implements core.Stream.
func (s *stream) Recv() (core.Event, error) {
	ev, ok := <-s.events
	if !ok {
		s.finish()
		return core.Event{}, io.EOF
	}
	return ev, nil
}

// Close implements core.Stream.  It is idempotent.
func (s *stream) Close() error {
	s.finish()
	return nil
}

// finish tears the request down exactly once: kill the child, wait for the
// reader goroutine, release the concurrency slot, delete the media files.
func (s *stream) finish() {
	s.once.Do(func() {
		s.cancel()
		select {
		case <-s.finished:
		case <-time.After(processKillGrace):
			// The child ignored the kill.  Do not hold the caller (or the
			// slot) hostage any longer than the grace period.
		}
		<-s.client.run.sem
		s.media.cleanup()
	})
}

// run is the reader goroutine: wait for the process, flush the tail, emit the
// terminal events, close the channel.
func (s *stream) run() {
	// Registered first, so they run last; the recover below is registered after
	// them and therefore runs first, while s.events is still open.
	defer close(s.finished)
	defer close(s.events)
	// A panic in the CLI reader would otherwise kill the process and every
	// request in flight.  emitFinal is the right channel here: it tolerates an
	// already-cancelled context, so the error still reaches the caller.
	defer func() {
		if r := recover(); r != nil {
			s.emitFinal(core.Event{
				Type: core.EventError,
				Err:  fmt.Errorf("kimi: the stream reader panicked: %v", r),
			})
		}
	}()

	waitErr := s.cmd.Wait()
	s.out.flush()
	s.finishUpstream(waitErr)
}

// emit sends one incremental event, abandoning the send if the request has been
// cancelled (the consumer is gone, or we are being torn down).
func (s *stream) emit(ev core.Event) {
	select {
	case s.events <- ev:
	case <-s.ctx.Done():
	}
}

// emitFinal sends a terminal event.  Terminal events must not be lost just
// because the context expired a moment ago (a timeout still has to be
// reported), so this only gives up after emitGrace.
func (s *stream) emitFinal(ev core.Event) {
	select {
	case s.events <- ev:
	case <-time.After(emitGrace):
	}
}

// finishUpstream turns the process outcome into the trailing events.  Order is
// fixed by the contract: deltas, then at most one usage, then at most one done.
func (s *stream) finishUpstream(waitErr error) {
	s.flushText()

	var failure error
	switch {
	case errors.Is(s.ctx.Err(), context.DeadlineExceeded):
		failure = fmt.Errorf("kimi: request timed out after %s and the CLI process was killed", s.client.cfg.timeout())
	case errors.Is(s.ctx.Err(), context.Canceled):
		// The caller cancelled.  That is not an upstream failure to report.
		failure = nil
	case waitErr != nil:
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			if !s.output {
				failure = fmt.Errorf("kimi: CLI exited with %s%s", exitErr.ProcessState.String(), s.stderrSuffix())
			}
		} else {
			failure = fmt.Errorf("kimi: waiting for the CLI: %w", waitErr)
		}
	}

	if failure != nil {
		s.client.noteError(failure)
		s.client.noteCLIFailure(s.accountID, failure, s.ctx.Err())
		s.emitFinal(core.Event{Type: core.EventError, Err: failure})
	} else {
		s.client.clearError()
		// A completed run is positive evidence about the account; a caller
		// cancellation is not, because the account was never given the chance to
		// answer.  So only an untouched context counts as a use.
		if s.ctx.Err() == nil {
			s.client.markUsed(s.accountID)
		}
	}

	if u := s.usage; u != nil && !s.usageSent {
		s.usageSent = true
		s.emitFinal(core.Event{Type: core.EventUsage, Usage: u})
	}
	s.emitFinal(core.Event{Type: core.EventDone, Finish: s.finishReason()})
}

// finishReason maps the turn to an OpenAI finish reason.
func (s *stream) finishReason() string {
	if s.calls > 0 {
		return "tool_calls"
	}
	return "stop"
}

func (s *stream) stderrSuffix() string {
	if s.errTail == nil {
		return ""
	}
	tail := redactSecrets(s.errTail.String())
	if tail == "" {
		return ""
	}
	if len(tail) > 400 {
		tail = tail[len(tail)-400:]
	}
	return " (stderr: " + strings.ReplaceAll(tail, "\n", " ") + ")"
}

// ---------------------------------------------------------------------------
// Assistant text: buffering for structural tool-call detection
// ---------------------------------------------------------------------------

// emitText accumulates assistant text.  Plain text is streamed immediately as
// EventDelta fragments; text that could still turn out to be the prompt-injected
// tool-call envelope (it starts with "{") is buffered until the envelope can be
// decided, because leaking half a JSON object to the caller as content would be
// worse than a little latency.
func (s *stream) emitText(text string) {
	if text == "" {
		return
	}
	s.textBuf.WriteString(text)
	if !s.mayBeEnvelope() {
		s.flushText()
	}
}

func (s *stream) mayBeEnvelope() bool {
	if s.textBuf.Len() == 0 {
		return false
	}
	return strings.HasPrefix(strings.TrimLeft(s.textBuf.String(), " \t\r\n"), "{")
}

// flushText emits whatever is buffered: a tool-call envelope becomes
// EventToolCall values, anything else becomes one EventDelta.
func (s *stream) flushText() {
	if s.textBuf.Len() == 0 {
		return
	}
	text := s.textBuf.String()
	s.textBuf.Reset()

	if calls, ok := parseToolEnvelope(text); ok {
		s.emitEnvelopeCalls(calls)
		return
	}
	s.output = true
	s.emit(core.Event{Type: core.EventDelta, Delta: text})
}

// ---------------------------------------------------------------------------
// NDJSON writing side: line assembly
// ---------------------------------------------------------------------------

// ndjsonWriter implements io.Writer.  exec copies the child's stdout into it;
// the byte stream is cut into lines and each complete line is handed to the
// stream.  A trailing partial line is held until flush() at process exit, which
// is what makes a last record without a newline survive.
type ndjsonWriter struct {
	s   *stream
	mu  sync.Mutex
	buf []byte
}

func (w *ndjsonWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)

	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(w.buf[:i]))
		w.buf = append(w.buf[:0], w.buf[i+1:]...)
		w.s.handleLine(line)
	}
	if len(w.buf) == 0 {
		w.buf = nil
	}
	return len(p), nil
}

func (w *ndjsonWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) == 0 {
		return
	}
	line := strings.TrimSpace(string(w.buf))
	w.buf = nil
	w.s.handleLine(line)
}

// handleLine decodes one NDJSON record.  Malformed lines are ignored, matching
// the reference: the CLI interleaves progress output that is not JSON, and
// failing the whole request over it would be worse than skipping it.
func (s *stream) handleLine(line string) {
	if line == "" {
		return
	}
	if len(line) > maxRecordBytes {
		s.client.deps.Log("kimi: dropping a %d-byte NDJSON line (limit %d)", len(line), maxRecordBytes)
		return
	}
	var rec record
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		s.client.deps.Log("kimi: skipping a non-JSON output line (%d bytes)", len(line))
		return
	}
	s.handleRecord(&rec)
}
