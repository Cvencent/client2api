package codearts

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// sse_test.go covers the module's own SSE reader in its three layers: the
// transport shape (readFrame), the protocol shape (parseFrame/eventFor) and
// the concurrency shape (stream).  No network is involved anywhere: the
// "response body" is a strings.Reader or an io.Pipe.

// frameReader wraps a fixture body the way stream.run does.
func frameReader(body string) *bufio.Reader {
	return bufio.NewReaderSize(strings.NewReader(body), 64<<10)
}

// drain reads every payload a fixture contains, stopping at the first error.
func drain(t *testing.T, body string) ([]string, error) {
	t.Helper()
	r := frameReader(body)
	var out []string
	for i := 0; i < 100; i++ {
		payload, err := readFrame(r)
		if err != nil {
			return out, err
		}
		out = append(out, payload)
	}
	t.Fatal("readFrame did not terminate within 100 frames")
	return nil, nil
}

func TestReadFrameBasic(t *testing.T) {
	got, err := drain(t, "data: {\"a\":1}\n\ndata: {\"b\":2}\n\n")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	want := []string{`{"a":1}`, `{"b":2}`}
	if len(got) != len(want) {
		t.Fatalf("got %d frames %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("frame %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A colon line is a comment, which is also how the vendor keeps the connection
// alive.  It must not be mistaken for an event.
func TestReadFrameSkipsComments(t *testing.T) {
	got, err := drain(t, ": keep-alive\n\ndata: {\"a\":1}\n\n: ping\ndata: {\"b\":2}\n\n")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"b":2}` {
		t.Fatalf("got %q, want the two payloads with the comments dropped", got)
	}
}

// Several data: lines in one event are one payload joined with newlines.
func TestReadFrameJoinsMultilineData(t *testing.T) {
	got, err := drain(t, "data: {\"a\":\ndata: 1}\n\n")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if len(got) != 1 || got[0] != "{\"a\":\n1}" {
		t.Fatalf("got %q, want the two lines joined", got)
	}
}

// Some intermediaries strip the data: prefix entirely.
func TestReadFrameAcceptsBareJSON(t *testing.T) {
	got, err := drain(t, "  {\"a\":1}\n\n")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if len(got) != 1 || got[0] != `{"a":1}` {
		t.Fatalf("got %q, want the bare payload", got)
	}
}

// Named events, ids, retry hints and anything unrecognised are consumed and
// ignored.  An unknown line must never abort a stream that is otherwise fine.
func TestReadFrameIgnoresUnknownLines(t *testing.T) {
	got, err := drain(t, "event: message\nid: 42\nretry: 1000\nthis is not SSE at all\ndata: {\"a\":1}\n\n")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if len(got) != 1 || got[0] != `{"a":1}` {
		t.Fatalf("got %q, want just the payload", got)
	}
}

// The terminal frame is an internal sentinel, never a payload.
func TestReadFrameDone(t *testing.T) {
	got, err := drain(t, "data: {\"a\":1}\n\ndata: [DONE]\n\n")
	if !errors.Is(err, errStreamDone) {
		t.Fatalf("err = %v, want errStreamDone", err)
	}
	if len(got) != 1 || got[0] != `{"a":1}` {
		t.Fatalf("got %q, want the payload before the sentinel", got)
	}
}

func TestReadFrameCleanEOF(t *testing.T) {
	payload, err := readFrame(frameReader(""))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if payload != "" {
		t.Errorf("payload = %q, want empty", payload)
	}
}

// A body that ends immediately after the final payload, with no blank line to
// close the event, still yields that payload.  The vendor closes the
// connection after the last frame and does not owe the caller a blank line.
func TestReadFrameEndsRightAfterAPayload(t *testing.T) {
	got, err := drain(t, "data: {\"a\":1}\n\ndata: {\"b\":2}\n")
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if len(got) != 2 || got[1] != `{"b":2}` {
		t.Fatalf("got %q, want the trailing payload delivered", got)
	}
}

// A [DONE] with no blank line after it is still the end of the stream.
func TestReadFrameDoneWithoutABlankLine(t *testing.T) {
	_, err := readFrame(frameReader("data: [DONE]\n"))
	if !errors.Is(err, errStreamDone) {
		t.Fatalf("err = %v, want errStreamDone", err)
	}
}

// One frame cannot be unbounded: a wedged or hostile upstream must not be able
// to make the module allocate without limit.
func TestReadFrameRejectsAnOversizedFrame(t *testing.T) {
	body := "data: " + strings.Repeat("a", maxFrameBytes+64) + "\n\n"
	_, err := readFrame(frameReader(body))
	if err == nil {
		t.Fatal("an oversized frame was accepted")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("err = %v, want the oversized-frame error", err)
	}
}

// ---------------------------------------------------------------------------
// layer 2: the protocol shape
// ---------------------------------------------------------------------------

func TestParseFrameRejectsNonJSON(t *testing.T) {
	if _, err := parseFrame("not json at all"); err == nil {
		t.Fatal("a non-JSON frame was accepted")
	}
}

func TestParseFrameEmptyIsANoOp(t *testing.T) {
	f, err := parseFrame("   ")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if f != nil {
		t.Fatalf("frame = %+v, want nil", f)
	}
}

func TestEventForContentReasoningAndToolCalls(t *testing.T) {
	f, err := parseFrame(`{"choices":[{"index":0,"delta":{"content":"hi","reasoning_content":"why",` +
		`"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"p\":1}"}}]}}]}`)
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	events, err := eventFor(f)
	if err != nil {
		t.Fatalf("eventFor: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events %+v, want 3", len(events), events)
	}
	if events[0].Type != core.EventDelta || events[0].Delta != "hi" {
		t.Errorf("event 0 = %+v, want the content delta", events[0])
	}
	if events[1].Type != core.EventDelta || events[1].Reasoning != "why" {
		t.Errorf("event 1 = %+v, want the reasoning delta", events[1])
	}
	tc := events[2].ToolCall
	if events[2].Type != core.EventToolCall || tc == nil {
		t.Fatalf("event 2 = %+v, want a tool call", events[2])
	}
	if tc.Index != 0 || tc.ID != "call_1" || tc.Name != "read" || tc.Arguments != `{"p":1}` {
		t.Errorf("tool call = %+v, want the parsed call", tc)
	}
}

// A tool call with no index must still produce an event, at index 0.
func TestEventForToolCallWithoutIndex(t *testing.T) {
	f, err := parseFrame(`{"choices":[{"delta":{"tool_calls":[{"function":{"name":"read"}}]}}]}`)
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	events, _ := eventFor(f)
	if len(events) != 1 || events[0].ToolCall == nil || events[0].ToolCall.Index != 0 {
		t.Fatalf("got %+v, want one tool-call event at index 0", events)
	}
}

func TestEventForUsage(t *testing.T) {
	f, err := parseFrame(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,` +
		`"prompt_tokens_details":{"cached_tokens":6},"completion_tokens_details":{"reasoning_tokens":2}}}`)
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	events, err := eventFor(f)
	if err != nil {
		t.Fatalf("eventFor: %v", err)
	}
	if len(events) != 1 || events[0].Type != core.EventUsage || events[0].Usage == nil {
		t.Fatalf("got %+v, want one usage event", events)
	}
	u := *events[0].Usage
	if u.PromptTokens != 10 || u.CompletionTokens != 4 || u.CachedTokens != 6 || u.ReasoningTokens != 2 {
		t.Errorf("usage = %+v, want the parsed counters", u)
	}
	if u.TotalTokens != 14 {
		t.Errorf("TotalTokens = %d, want 14 derived from the two halves", u.TotalTokens)
	}
}

// The vendor reports a per-minute token limit inside a 200 stream rather than
// as a 429, so an in-frame error must surface as an error, not as text.
func TestEventForInFrameError(t *testing.T) {
	f, err := parseFrame(`{"error_code":"InferHub.ModelArts.81111.429","error_msg":"TPM limit reached"}`)
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	events, err := eventFor(f)
	if err == nil {
		t.Fatal("an error frame produced no error")
	}
	if len(events) != 0 {
		t.Errorf("got %d events alongside the error, want none", len(events))
	}
	var se *streamError
	if !errors.As(err, &se) {
		t.Fatalf("err = %T, want *streamError", err)
	}
	if !se.queueRetryable() {
		t.Errorf("code %q was not classified as a queue/rate-limit retry", se.code)
	}
	if !strings.Contains(err.Error(), "81111.429") {
		t.Errorf("err = %q, want the upstream code carried through", err.Error())
	}
}

// The nested error shape is used by some responses.
func TestEventForNestedError(t *testing.T) {
	f, err := parseFrame(`{"error":{"code":"APIG.0602","message":"Invalid token"}}`)
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	if _, err := eventFor(f); err == nil {
		t.Fatal("a nested error produced no error")
	}
}

// An auth error is not a queue error: retrying it would loop forever.
func TestEventForAuthErrorIsNotQueueRetryable(t *testing.T) {
	f, _ := parseFrame(`{"error_code":"APIG.0602","error_msg":"Invalid token"}`)
	_, err := eventFor(f)
	var se *streamError
	if !errors.As(err, &se) {
		t.Fatalf("err = %T, want *streamError", err)
	}
	if se.queueRetryable() {
		t.Error("an auth error was classified as queue-retryable")
	}
}

// A keep-alive-ish frame with an empty delta must not become an empty event.
func TestEventForEmptyDeltaIsANoOp(t *testing.T) {
	f, err := parseFrame(`{"choices":[{"index":0,"delta":{}}]}`)
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	events, err := eventFor(f)
	if err != nil {
		t.Fatalf("eventFor: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("got %+v, want no events", events)
	}
}

func TestUsageFromCacheHitAlias(t *testing.T) {
	f, err := parseFrame(`{"choices":[],"usage":{"prompt_tokens":3,"prompt_tokens_details":{"prompt_cache_hit_tokens":9}}}`)
	if err != nil {
		t.Fatalf("parseFrame: %v", err)
	}
	events, err := eventFor(f)
	if err != nil {
		t.Fatalf("eventFor: %v", err)
	}
	if len(events) != 1 || events[0].Usage == nil {
		t.Fatalf("got %+v, want one usage event", events)
	}
	u := *events[0].Usage
	if u.CachedTokens != 9 {
		t.Errorf("CachedTokens = %d, want the prompt_cache_hit_tokens alias", u.CachedTokens)
	}
	if u.TotalTokens != 3 {
		t.Errorf("TotalTokens = %d, want the prompt half when there is no completion half", u.TotalTokens)
	}
}

// ---------------------------------------------------------------------------
// layer 3: the concurrency shape
// ---------------------------------------------------------------------------

// collect drains a stream to its terminal condition.
func collect(t *testing.T, s *stream) ([]core.Event, error) {
	t.Helper()
	var out []core.Event
	for i := 0; i < 200; i++ {
		ev, err := s.Recv()
		if err != nil {
			return out, err
		}
		out = append(out, ev)
	}
	t.Fatal("the stream did not terminate within 200 events")
	return nil, nil
}

// bodyStream builds a stream over a fixed body, with generous timeouts.
func bodyStream(t *testing.T, body string, release func()) *stream {
	t.Helper()
	return newStream(context.Background(), io.NopCloser(strings.NewReader(body)), time.Minute, time.Minute, release)
}

func TestStreamRecvYieldsEventsThenEOF(t *testing.T) {
	s := bodyStream(t, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"+
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n"+
		"data: [DONE]\n\n", nil)
	defer s.Close()

	events, err := collect(t, s)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d events %+v, want 3 (two deltas and done)", len(events), events)
	}
	if events[0].Delta != "a" || events[1].Delta != "b" {
		t.Errorf("deltas = %q,%q, want a,b", events[0].Delta, events[1].Delta)
	}
	if events[2].Type != core.EventDone {
		t.Errorf("last event = %+v, want done", events[2])
	}
}

func TestStreamEmitsDoneWithFinishReason(t *testing.T) {
	s := bodyStream(t, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":\"stop\"}]}\n\n"+
		"data: [DONE]\n\n", nil)
	defer s.Close()

	events, err := collect(t, s)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	last := events[len(events)-1]
	if last.Type != core.EventDone || last.Finish != "stop" {
		t.Fatalf("last event = %+v, want done with finish_reason stop", last)
	}
}

// A body that ends without [DONE] still ended cleanly: the vendor closes the
// connection after the last frame.
func TestStreamEndsCleanlyWithoutDone(t *testing.T) {
	s := bodyStream(t, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n", nil)
	defer s.Close()

	events, err := collect(t, s)
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	if len(events) != 2 || events[1].Type != core.EventDone {
		t.Fatalf("got %+v, want one delta and a done", events)
	}
}

// A failure inside the stream is delivered as the terminal error, and no done
// event is emitted before it — the gateway must never see done-then-failure.
func TestStreamDeliversATerminalErrorOnce(t *testing.T) {
	s := bodyStream(t, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"+
		"data: {\"error_code\":\"APIG.0602\",\"error_msg\":\"Invalid token\"}\n\n", nil)
	defer s.Close()

	events, err := collect(t, s)
	if err == nil {
		t.Fatal("the in-stream error was swallowed")
	}
	if errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want the upstream's own error", err)
	}
	if !strings.Contains(err.Error(), "Invalid token") {
		t.Errorf("err = %q, want the upstream message carried through", err.Error())
	}
	for _, ev := range events {
		if ev.Type == core.EventDone {
			t.Errorf("a done event was emitted alongside a failure: %+v", events)
		}
	}
	// The error is delivered once; the next read is a clean end.
	if _, err := s.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("second Recv err = %v, want io.EOF", err)
	}
}

// A non-JSON frame is a protocol change, not something to skip quietly.
func TestStreamFailsOnANonJSONFrame(t *testing.T) {
	s := bodyStream(t, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: garbage\n\n", nil)
	defer s.Close()

	_, err := collect(t, s)
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want a decode failure", err)
	}
}

// Close releases the body and the request context, and is safe to call twice.
func TestStreamCloseIsIdempotentAndReleases(t *testing.T) {
	var releases int
	s := bodyStream(t, "data: [DONE]\n\n", func() { releases++ })
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if releases != 1 {
		t.Errorf("release ran %d times, want exactly 1", releases)
	}
}

// A fully drained stream must still release its request context: the release
// runs from the reader goroutine, not only from Close.
func TestStreamReleasesOnADrainedStream(t *testing.T) {
	released := make(chan struct{})
	s := bodyStream(t, "data: [DONE]\n\n", func() { close(released) })
	defer s.Close()

	if _, err := collect(t, s); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("a fully drained stream never released its context")
	}
}

// An upstream that goes quiet must fail the stream rather than hang it
// forever.  The watchdog closes the body, which unblocks the pending read.
func TestStreamIdleTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	// A generous chunk window so only the first-byte window can fire.
	s := newStream(context.Background(), pr, 20*time.Millisecond, time.Minute, nil)
	defer s.Close()

	_, err := s.Recv()
	if !errors.Is(err, errIdleTimeout) {
		t.Fatalf("err = %v, want errIdleTimeout", err)
	}
}

// The watchdog switches to the (longer) chunk window once data has arrived, so
// a slow generation is not killed by the first-token deadline.
func TestWatchdogSwitchesToTheChunkWindow(t *testing.T) {
	pr, pw := io.Pipe()
	w := newWatchdogBody(pr, 20*time.Millisecond, 5*time.Second)
	defer w.Close()

	// io.Pipe writes block until they are read, so the writer has to be its
	// own goroutine.  The pause between the two chunks is four times the
	// first-token window: if the timer were not re-armed, the second read
	// would fail.
	go func() {
		_, _ = pw.Write([]byte("hello"))
		time.Sleep(80 * time.Millisecond)
		_, _ = pw.Write([]byte("world"))
		_ = pw.Close()
	}()

	buf := make([]byte, 16)
	if n, err := io.ReadFull(w, buf[:5]); err != nil || n != 5 {
		t.Fatalf("first read = %d, %v; want 5 bytes and no error", n, err)
	}
	if n, err := io.ReadFull(w, buf[:5]); err != nil || n != 5 {
		t.Fatalf("second read = %d, %v; want 5 bytes — the watchdog did not switch to the chunk window", n, err)
	}
	if string(buf[:5]) != "world" {
		t.Errorf("second chunk = %q, want world", buf[:5])
	}
}

// Once the watchdog has fired, every subsequent read reports the idle timeout
// rather than the bare closed-pipe error the close produced.
func TestWatchdogReportsTheIdleTimeoutNotTheClosedPipe(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	w := newWatchdogBody(pr, 20*time.Millisecond, time.Minute)
	defer w.Close()

	buf := make([]byte, 8)
	time.Sleep(60 * time.Millisecond)
	_, err := w.Read(buf)
	if !errors.Is(err, errIdleTimeout) {
		t.Fatalf("err = %v, want errIdleTimeout", err)
	}
}
