package loomy

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// fixtureStream builds a chatStream over a fixed body with a cancel that is
// never used, which is what a test wants: the body simply ends.
func fixtureStream(t *testing.T, body string) *chatStream {
	t.Helper()
	return newChatStream(io.NopCloser(strings.NewReader(body)), time.Minute, func() {})
}

// drain reads until the stream ends and returns everything it produced.
func drain(t *testing.T, s *chatStream) []core.Event {
	t.Helper()
	var out []core.Event
	for {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Recv returned %v", err)
		}
		out = append(out, ev)
	}
}

func TestStreamParsesDeltasUsageAndDone(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		``,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		``,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: {"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	events := drain(t, fixtureStream(t, body))
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(events), events)
	}
	if events[0].Type != core.EventDelta || events[0].Delta != "Hel" {
		t.Errorf("event 0 = %+v", events[0])
	}
	if events[1].Type != core.EventDelta || events[1].Delta != "lo" {
		t.Errorf("event 1 = %+v", events[1])
	}
	if events[2].Type != core.EventUsage {
		t.Fatalf("event 2 = %+v, want a usage event", events[2])
	}
	if events[2].Usage == nil || events[2].Usage.PromptTokens != 10 ||
		events[2].Usage.CompletionTokens != 5 || events[2].Usage.TotalTokens != 15 {
		t.Errorf("usage = %+v", events[2].Usage)
	}
	if events[3].Type != core.EventDone || events[3].Finish != "stop" {
		t.Errorf("event 3 = %+v, want a done event finishing with stop", events[3])
	}
}

// TestStreamIgnoresCommentsAndUnrelatedFields covers the SSE bookkeeping lines:
// a leading `:` comment is a keep-alive, and `event:`/`id:`/`retry:` must be
// skipped rather than concatenated into the JSON payload.
func TestStreamIgnoresCommentsAndUnrelatedFields(t *testing.T) {
	body := strings.Join([]string{
		`: this is a keep-alive comment`,
		`event: message`,
		`id: 42`,
		`retry: 1000`,
		`data: {"choices":[{"delta":{"content":"a"}}]}`,
		``,
		`: another comment`,
		`data: {"choices":[{"delta":{"content":"b"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	events := drain(t, fixtureStream(t, body))
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	if events[0].Delta != "a" || events[1].Delta != "b" {
		t.Errorf("payloads were corrupted by the bookkeeping lines: %+v", events)
	}
	if events[2].Type != core.EventDone {
		t.Errorf("event 2 = %+v, want done", events[2])
	}
}

func TestStreamMultiLineDataIsJoinedWithNewline(t *testing.T) {
	// Two data lines that only form valid JSON once joined.
	body := "data: {\"choices\":[{\"delta\":\n" +
		"data: {\"content\":\"joined\"}}]}\n\n" +
		"data: [DONE]\n\n"

	events := drain(t, fixtureStream(t, body))
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	if events[0].Type != core.EventDelta || events[0].Delta != "joined" {
		t.Errorf("the joined frame did not decode: %+v", events[0])
	}
}

func TestStreamAcceptsEmptyDataTerminator(t *testing.T) {
	// Some proxies terminate with a bare `data:` instead of `data: [DONE]`.
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n" +
		"data:\n\n"

	events := drain(t, fixtureStream(t, body))
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	if events[0].Delta != "x" {
		t.Errorf("event 0 = %+v", events[0])
	}
	if events[1].Type != core.EventDone {
		t.Errorf("event 1 = %+v, want done", events[1])
	}
}

func TestStreamReportsUnexpectedEOFWithoutTerminator(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"tail\"}}]}\n\n"
	s := fixtureStream(t, body)
	if ev, err := s.Recv(); err != nil || ev.Type != core.EventDelta || ev.Delta != "tail" {
		t.Fatalf("first event = %+v, %v; want the partial delta", ev, err)
	}
	ev, err := s.Recv()
	if err != nil {
		t.Fatalf("second Recv returned %v", err)
	}
	if ev.Type != core.EventError || ev.Err == nil {
		t.Fatalf("second event = %+v, want a stream error", ev)
	}
	if !strings.Contains(ev.Err.Error(), "terminal frame") {
		t.Errorf("unexpected error: %v", ev.Err)
	}
}

func TestStreamFinalFrameWithoutTrailingBlankLine(t *testing.T) {
	// No trailing newline at all: the last frame is delivered, then the missing
	// terminal frame is reported as an error instead of a successful done event.
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"last\"}}]}"
	s := fixtureStream(t, body)
	if ev, err := s.Recv(); err != nil || ev.Type != core.EventDelta || ev.Delta != "last" {
		t.Fatalf("first event = %+v, %v; want the final delta", ev, err)
	}
	if ev, err := s.Recv(); err != nil || ev.Type != core.EventError {
		t.Fatalf("second event = %+v, %v; want unexpected EOF error", ev, err)
	}
}

// TestStreamPrefersReasoningContent pins the field precedence the reference
// documents: `reasoning_content` wins over `reasoning`.
func TestStreamPrefersReasoningContent(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think A\",\"reasoning\":\"think B\",\"content\":\"answer\"}}]}\n\n" +
		"data: [DONE]\n\n"
	events := drain(t, fixtureStream(t, body))
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	if events[0].Type != core.EventDelta {
		t.Fatalf("event 0 = %+v", events[0])
	}
	if events[0].Reasoning != "think A" {
		t.Errorf("reasoning = %q, want reasoning_content to win", events[0].Reasoning)
	}
	if events[0].Delta != "answer" {
		t.Errorf("delta = %q", events[0].Delta)
	}
}

func TestStreamFallsBackToReasoningField(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"reasoning\":\"only\"}}]}\n\n" +
		"data: [DONE]\n\n"
	events := drain(t, fixtureStream(t, body))
	if len(events) != 2 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	if events[0].Reasoning != "only" {
		t.Errorf("reasoning = %q", events[0].Reasoning)
	}
}

func TestStreamToolCallFragments(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"ci\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"ty\\\":\\\"SF\\\"}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"

	events := drain(t, fixtureStream(t, body))
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	if events[0].Type != core.EventToolCall || events[0].ToolCall == nil {
		t.Fatalf("event 0 = %+v, want a tool call", events[0])
	}
	if events[0].ToolCall.ID != "call_1" || events[0].ToolCall.Name != "get_weather" {
		t.Errorf("tool call head = %+v", events[0].ToolCall)
	}
	if events[0].ToolCall.Arguments != `{"ci` {
		t.Errorf("first argument fragment = %q", events[0].ToolCall.Arguments)
	}
	if events[1].Type != core.EventToolCall || events[1].ToolCall.Arguments != `ty":"SF"}` {
		t.Errorf("second argument fragment = %+v", events[1])
	}
}

// TestStreamBusinessFailureInsideTheStream is the trap the reference calls out:
// a 200 response can still carry a vendor error, so it must be recognised.
func TestStreamBusinessFailureInsideTheStream(t *testing.T) {
	body := "data: {\"code\":\"100002\",\"desc\":\"缺少 token\"}\n\n"
	s := fixtureStream(t, body)
	_, err := s.Recv()
	if err == nil {
		t.Fatalf("expected the in-stream business failure to be reported")
	}
	if !strings.Contains(err.Error(), "100002") {
		t.Errorf("error should carry the vendor code, got %q", err.Error())
	}
	if got := failureKind(err); got != core.FailureSessionDead {
		t.Errorf("failureKind = %q, want the session-dead kind", got)
	}
}

func TestStreamErrorObjectIsReported(t *testing.T) {
	body := "data: {\"error\":{\"message\":\"boom\",\"code\":\"500\"}}\n\n"
	s := fixtureStream(t, body)
	_, err := s.Recv()
	if err == nil {
		t.Fatalf("expected the error object to be reported")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry the message, got %q", err.Error())
	}
}

func TestStreamRejectsNonJSONFrame(t *testing.T) {
	s := fixtureStream(t, "data: <html>nope</html>\n\n")
	_, err := s.Recv()
	if err == nil {
		t.Fatalf("expected a non-JSON frame to be an error")
	}
	if !strings.Contains(err.Error(), "not JSON") {
		t.Errorf("unexpected error text: %q", err.Error())
	}
}

func TestStreamRecomputesMissingTotalTokens(t *testing.T) {
	body := "data: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\n" +
		"data: [DONE]\n\n"
	events := drain(t, fixtureStream(t, body))
	if len(events) != 2 {
		t.Fatalf("got %d events: %+v", len(events), events)
	}
	if events[0].Usage == nil || events[0].Usage.TotalTokens != 10 {
		t.Fatalf("total tokens were not recomputed: %+v", events[0].Usage)
	}
}

// TestStreamIdleTimeout proves a stalled stream fails with a named error rather
// than blocking forever.  The cancel handed to the stream is what unblocks the
// body read in production (it cancels the request context); here it closes the
// pipe, which is the same effect.
func TestStreamIdleTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	released := make(chan struct{})
	var once sync.Once
	s := newChatStream(pr, 40*time.Millisecond, func() {
		pr.CloseWithError(errors.New("request cancelled"))
		once.Do(func() { close(released) })
	})
	defer s.Close()

	_, err := s.Recv()
	if err == nil {
		t.Fatalf("expected an idle timeout error")
	}
	if !strings.Contains(err.Error(), "idle") {
		t.Errorf("the error should name the idle timeout, got %q", err.Error())
	}
	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatalf("the idle timer never fired")
	}
}

func TestStreamCloseIsIdempotent(t *testing.T) {
	cancelled := 0
	s := newChatStream(io.NopCloser(strings.NewReader("")), time.Minute, func() { cancelled++ })
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if cancelled != 1 {
		t.Errorf("cancel ran %d times, want exactly 1", cancelled)
	}
	// A closed stream still answers io.EOF rather than panicking.
	if _, err := s.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("Recv after Close = %v, want io.EOF", err)
	}
}

// TestStreamUsageOnlyEmittedOnce guards against the done frame being queued
// twice, and pins the rule that a terminator ends the stream for good: content
// sent after it is not part of this completion.
func TestStreamUsageOnlyEmittedOnce(t *testing.T) {
	body := "data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
		"data: [DONE]\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"ignored\"}}]}\n\n" +
		"data: [DONE]\n\n"
	events := drain(t, fixtureStream(t, body))
	done, usage := 0, 0
	for _, ev := range events {
		switch ev.Type {
		case core.EventDone:
			done++
		case core.EventUsage:
			usage++
		case core.EventDelta:
			t.Errorf("a delta arrived after the terminator: %+v", ev)
		}
	}
	if done != 1 || usage != 1 {
		t.Fatalf("got %d done and %d usage events, want exactly 1 each: %+v", done, usage, events)
	}
}
