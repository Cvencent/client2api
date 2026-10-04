package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// The hand-written SSE reader.
// ---------------------------------------------------------------------------

func TestSSEReaderParsesAFrame(t *testing.T) {
	r := newSSEReader(strings.NewReader("event: message\ndata: {\"a\":1}\n\n"))
	frame, err := r.next()
	if err != nil {
		t.Fatalf("next = %v", err)
	}
	if frame.Event != "message" {
		t.Fatalf("Event = %q, want message", frame.Event)
	}
	if frame.Data != `{"a":1}` {
		t.Fatalf("Data = %q", frame.Data)
	}
	if _, err := r.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("second next = %v, want io.EOF", err)
	}
}

// A multi-line data field is joined with newlines, per the SSE spec.
func TestSSEReaderJoinsMultiLineData(t *testing.T) {
	r := newSSEReader(strings.NewReader("data: one\ndata: two\n\n"))
	frame, err := r.next()
	if err != nil {
		t.Fatalf("next = %v", err)
	}
	if frame.Data != "one\ntwo" {
		t.Fatalf("Data = %q, want the lines joined with a newline", frame.Data)
	}
}

// Zen emits both `data:{...}` (no space) and CRLF, and a keep-alive comment.
func TestSSEReaderToleratesVendorQuirks(t *testing.T) {
	body := ": keep-alive\r\n" +
		"data:{\"a\":1}\r\n" +
		"\r\n"
	r := newSSEReader(strings.NewReader(body))
	frame, err := r.next()
	if err != nil {
		t.Fatalf("next = %v", err)
	}
	if frame.Data != `{"a":1}` {
		t.Fatalf("Data = %q, want the comment skipped and no space tolerated", frame.Data)
	}
}

// A stream cut off without a trailing blank line still yields its last frame.
func TestSSEReaderReturnsAFinalFrameWithoutABlankLine(t *testing.T) {
	r := newSSEReader(strings.NewReader("data: tail"))
	frame, err := r.next()
	if err != nil {
		t.Fatalf("next = %v", err)
	}
	if frame.Data != "tail" {
		t.Fatalf("Data = %q, want the unterminated frame", frame.Data)
	}
	if _, err := r.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("second next = %v, want io.EOF", err)
	}
}

func TestSSEReaderEmptyBodyIsACleanEOF(t *testing.T) {
	r := newSSEReader(strings.NewReader(""))
	if _, err := r.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("next = %v, want io.EOF for an empty body", err)
	}
}

// ---------------------------------------------------------------------------
// The pump: idle timeout and cancellation.
// ---------------------------------------------------------------------------

func TestFramePumpIdleTimeout(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	pump := newFramePump(pr, nil)
	defer pump.stop()

	start := time.Now()
	_, err := pump.next(context.Background(), 60*time.Millisecond)
	if err == nil {
		t.Fatal("next = nil error, want an idle timeout on a silent upstream")
	}
	if !strings.Contains(err.Error(), "idle") {
		t.Fatalf("error = %q, want it to describe the idle timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the idle timeout took %s, want it to fire promptly", elapsed)
	}
}

func TestFramePumpHonoursContextCancellation(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	pump := newFramePump(pr, nil)
	defer pump.stop()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pump.next(ctx, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("next = %v, want context.Canceled", err)
	}
}

func TestFramePumpDeliversFrames(t *testing.T) {
	pr, pw := io.Pipe()
	pump := newFramePump(pr, nil)
	defer pump.stop()

	go func() {
		pw.Write([]byte("data: hello\n\n"))
		pw.Close()
	}()
	frame, err := pump.next(context.Background(), 2*time.Second)
	if err != nil {
		t.Fatalf("next = %v", err)
	}
	if frame.Data != "hello" {
		t.Fatalf("Data = %q", frame.Data)
	}
	if _, err := pump.next(context.Background(), 2*time.Second); !errors.Is(err, io.EOF) {
		t.Fatalf("second next = %v, want io.EOF after the writer closed", err)
	}
}

func TestFramePumpStopIsIdempotent(t *testing.T) {
	pr, pw := io.Pipe()
	pump := newFramePump(pr, nil)
	pump.stop()
	pump.stop()
	pw.Close()
}

// ---------------------------------------------------------------------------
// Wire decoding helpers.
// ---------------------------------------------------------------------------

func TestIsDonePayload(t *testing.T) {
	for _, in := range []string{"[DONE]", " [done] ", "[Done]"} {
		if !isDonePayload(in) {
			t.Fatalf("isDonePayload(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"", "{}", "done", "[DONE"} {
		if isDonePayload(in) {
			t.Fatalf("isDonePayload(%q) = true, want false", in)
		}
	}
}

func TestNormalizeFinish(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"stop":              "stop",
		"end_turn":          "stop",
		"eos":               "stop",
		"length":            "length",
		"max_tokens":        "length",
		"max_output_tokens": "length",
		"tool_calls":        "tool_calls",
		"function_call":     "tool_calls",
		"tool_use":          "tool_calls",
		"content_filter":    "content_filter",
		"content_filtered":  "content_filter",
		"something_else":    "stop",
	}
	for in, want := range cases {
		if got := normalizeFinish(in); got != want {
			t.Fatalf("normalizeFinish(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFlattenContent(t *testing.T) {
	cases := map[string]string{
		`"hello"`: "hello",
		`null`:    "",
		``:        "",
		`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`: "ab",
		`[{"type":"text","text":"only"}]`:                         "only",
		`123`:                                                     "",
	}
	for in, want := range cases {
		if got := flattenContent(json.RawMessage(in)); got != want {
			t.Fatalf("flattenContent(%s) = %q, want %q", in, got, want)
		}
	}
}

// Zen's converter writes usage in several dialects depending on the upstream
// protocol, so all of them have to land in the same core.Usage.
func TestUsageToCore(t *testing.T) {
	openai := oaiUsage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14}
	if got := openai.toCore(); got.PromptTokens != 10 || got.CompletionTokens != 4 || got.TotalTokens != 14 {
		t.Fatalf("openai dialect = %+v", got)
	}

	anthropic := oaiUsage{InputTokens: 7, OutputTokens: 3}
	got := anthropic.toCore()
	if got.PromptTokens != 7 || got.CompletionTokens != 3 {
		t.Fatalf("anthropic dialect = %+v, want the input/output aliases mapped", got)
	}
	if got.TotalTokens != 10 {
		t.Fatalf("TotalTokens = %d, want it filled from the sum when the vendor omits it", got.TotalTokens)
	}

	detailed := oaiUsage{
		PromptTokens:            20,
		CompletionTokens:        5,
		PromptTokensDetails:     &oaiPromptDetails{CachedTokens: 8},
		CompletionTokensDetails: &oaiCompletionDetails{ReasoningTokens: 2},
	}
	got = detailed.toCore()
	if got.CachedTokens != 8 {
		t.Fatalf("CachedTokens = %d, want 8", got.CachedTokens)
	}
	if got.ReasoningTokens != 2 {
		t.Fatalf("ReasoningTokens = %d, want 2", got.ReasoningTokens)
	}
}

// ---------------------------------------------------------------------------
// Frames into events.
// ---------------------------------------------------------------------------

func newStreamForTest(t *testing.T, c *Client, body string, aggregate bool) *chatStream {
	t.Helper()
	acct, ok := c.pool.firstReady(testNow, c.cfg.maxInFlight())
	if !ok {
		t.Fatal("the test client has no account")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return newChatStream(c, io.NopCloser(strings.NewReader(body)), acct, ctx, cancel, aggregate)
}

// The cost frame Zen appends carries an empty choices array and a field no
// OpenAI schema has.  It must not derail the reader.
func TestAbsorbIgnoresTheCostFrame(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	s := newStreamForTest(t, c, "", false)
	defer s.Close()

	s.absorb(sseFrame{Data: `{"id":"c1","choices":[],"cost":"0.0001"}`})
	if len(s.pending) != 0 {
		t.Fatalf("the cost frame produced %d events, want 0", len(s.pending))
	}
	if s.rawDone {
		t.Fatal("the cost frame ended the stream")
	}
}

func TestAbsorbDropsNonJSONKeepAlives(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	s := newStreamForTest(t, c, "", false)
	defer s.Close()

	s.absorb(sseFrame{Data: "ping"})
	if len(s.pending) != 0 {
		t.Fatalf("a keep-alive produced %d events, want 0", len(s.pending))
	}
}

func TestAbsorbTurnsAVendorErrorIntoAnErrorEvent(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	s := newStreamForTest(t, c, "", false)
	defer s.Close()

	s.absorb(sseFrame{Data: `{"error":{"type":"AuthError","message":"Missing API key."}}`})
	if len(s.pending) != 1 {
		t.Fatalf("events = %d, want 1", len(s.pending))
	}
	ev := s.pending[0]
	if ev.Type != core.EventError {
		t.Fatalf("event type = %q, want %q", ev.Type, core.EventError)
	}
	if ev.Err == nil || !strings.Contains(ev.Err.Error(), "Missing API key.") {
		t.Fatalf("error = %v, want the vendor's message", ev.Err)
	}
}

func TestAbsorbDoneQueuesTheDoneEvent(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	s := newStreamForTest(t, c, "", false)
	defer s.Close()

	s.absorb(sseFrame{Data: "[DONE]"})
	if !s.rawDone {
		t.Fatal("[DONE] did not end the stream")
	}
	if len(s.pending) != 1 || s.pending[0].Type != core.EventDone {
		t.Fatalf("events = %+v, want one done event", s.pending)
	}
}

// A converter that answers a stream request with one whole message must not
// have its answer emitted twice.
func TestAbsorbMessageFallbackEmitsOnlyOnce(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	s := newStreamForTest(t, c, "", false)
	defer s.Close()

	s.absorb(sseFrame{Data: `{"choices":[{"index":0,"message":{"role":"assistant","content":"whole answer"}}]}`})
	s.absorb(sseFrame{Data: `{"choices":[{"index":0,"message":{"role":"assistant","content":"whole answer"}}]}`})

	var text string
	for _, ev := range s.pending {
		if ev.Type == core.EventDelta {
			text += ev.Delta
		}
	}
	if text != "whole answer" {
		t.Fatalf("text = %q, want the whole message emitted exactly once", text)
	}
}

// ---------------------------------------------------------------------------
// Recv contract.
// ---------------------------------------------------------------------------

func TestRecvReturnsEOFExactlyOnce(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	s := newStreamForTest(t, c, "data: [DONE]\n\n", false)
	defer s.Close()

	var eofs int
	for i := 0; i < 4; i++ {
		_, err := s.Recv()
		if errors.Is(err, io.EOF) {
			eofs++
			continue
		}
		if err != nil {
			t.Fatalf("Recv = %v", err)
		}
	}
	if eofs != 3 {
		t.Fatalf("io.EOF returned %d times over 4 calls, want it once the stream is done and thereafter", eofs)
	}
}

// The gateway's streaming relay (internal/gateway/server.go:893) passes
// Arguments straight through, and its aggregator (:1000) does
// tc.Arguments += ... — so Recv must hand out the RAW fragment, never the
// running total, or every argument slice before the last is repeated.
func TestRecvEmitsRawToolCallFragments(t *testing.T) {
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"ci\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"ty\\\":\\\"SF\\\"}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	s := newStreamForTest(t, c, body, false)
	defer s.Close()

	var frags []core.ToolCallDelta
	for {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv = %v", err)
		}
		if ev.Type == core.EventToolCall && ev.ToolCall != nil {
			frags = append(frags, *ev.ToolCall)
		}
	}
	if len(frags) != 2 {
		t.Fatalf("tool call events = %d, want 2", len(frags))
	}
	if frags[0].Arguments != `{"ci` {
		t.Fatalf("first fragment = %q, want the raw %q", frags[0].Arguments, `{"ci`)
	}
	if frags[1].Arguments != `ty":"SF"}` {
		t.Fatalf("second fragment = %q, want the raw slice %q, not the accumulated value",
			frags[1].Arguments, `ty":"SF"}`)
	}
	if frags[0].Index != 0 || frags[1].Index != 0 {
		t.Fatalf("indices = %d/%d, want both 0", frags[0].Index, frags[1].Index)
	}
	if frags[0].ID != "call_1" || frags[0].Name != "get_weather" {
		t.Fatalf("first fragment lost its id/name: %+v", frags[0])
	}
	if concat := frags[0].Arguments + frags[1].Arguments; concat != `{"city":"SF"}` {
		t.Fatalf("a consumer concatenating the fragments gets %q, want %q", concat, `{"city":"SF"}`)
	}
}

// The aggregating path is the one caller that wants the merged value.
func TestAggregatedStreamEmitsMergedToolCall(t *testing.T) {
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"ci\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"ty\\\":\\\"SF\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	s := newStreamForTest(t, c, body, true)
	defer s.Close()

	var calls []core.ToolCallDelta
	for {
		ev, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv = %v", err)
		}
		if ev.Type == core.EventToolCall && ev.ToolCall != nil {
			calls = append(calls, *ev.ToolCall)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want exactly 1 merged call", len(calls))
	}
	if calls[0].Arguments != `{"city":"SF"}` {
		t.Fatalf("arguments = %q, want the merged value", calls[0].Arguments)
	}
}

func TestRecvDropsAnUnindexedFragmentOntoTheLastCall(t *testing.T) {
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"f\",\"arguments\":\"a\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"b\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	s := newStreamForTest(t, c, body, false)
	defer s.Close()

	_, _, calls, _, _, err := drain(s)
	if err != nil {
		t.Fatalf("drain = %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d, want the unindexed fragment folded onto index 0", len(calls))
	}
	if calls[0].Arguments != "ab" {
		t.Fatalf("arguments = %q, want %q", calls[0].Arguments, "ab")
	}
}

func TestRecvSurfacesAnIdleUpstream(t *testing.T) {
	// The reader yields one frame and then blocks forever: exactly the shape of
	// an upstream that has gone quiet mid-answer.
	pr, pw := io.Pipe()
	defer pw.Close()
	body := io.MultiReader(
		strings.NewReader("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\n"),
		pr,
	)
	c := newTestClient(t, Config{IdleTimeout: Duration(60 * time.Millisecond)})
	addAccount(t, c, "opencode:a", "sk-a")
	acct, _ := c.pool.firstReady(testNow, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newChatStream(c, io.NopCloser(body), acct, ctx, cancel, false)
	defer s.Close()

	if _, err := s.Recv(); err != nil {
		t.Fatalf("first Recv = %v", err)
	}
	_, err := s.Recv()
	if err == nil {
		t.Fatal("Recv = nil error, want the idle timeout surfaced")
	}
	if !strings.Contains(err.Error(), "idle") {
		t.Fatalf("error = %q, want it to mention the idle timeout", err)
	}
}

func TestRecvReasoningDeltaIsCarried(t *testing.T) {
	body := "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n" +
		"data: [DONE]\n\n"
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	s := newStreamForTest(t, c, body, false)
	defer s.Close()

	ev, err := s.Recv()
	if err != nil {
		t.Fatalf("Recv = %v", err)
	}
	if ev.Type != core.EventDelta || ev.Reasoning != "thinking" {
		t.Fatalf("event = %+v, want a delta carrying the reasoning text", ev)
	}
}

func TestRecvChoiceTextFallback(t *testing.T) {
	body := "data: {\"choices\":[{\"index\":0,\"text\":\"legacy\"}]}\n\n" +
		"data: [DONE]\n\n"
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	s := newStreamForTest(t, c, body, false)
	defer s.Close()

	ev, err := s.Recv()
	if err != nil {
		t.Fatalf("Recv = %v", err)
	}
	if ev.Delta != "legacy" {
		t.Fatalf("Delta = %q, want the legacy completion text", ev.Delta)
	}
}

func TestStreamFailureIsTypedAndReportedOnce(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	s := newStreamForTest(t, c, "", false)
	defer s.Close()

	first := s.fail(errors.New("connection reset by peer"))
	if _, ok := core.AsFailure(first); !ok {
		t.Fatalf("fail() = %v, want a *core.Failure", first)
	}
	acct, _ := c.pool.byID("opencode:a")
	if acct.ErrCount != 1 {
		t.Fatalf("ErrCount = %d, want the failure recorded exactly once", acct.ErrCount)
	}
	second := s.fail(errors.New("connection reset by peer"))
	if acct2, _ := c.pool.byID("opencode:a"); acct2.ErrCount != 1 {
		t.Fatalf("ErrCount = %d, want the second fail() to record nothing", acct2.ErrCount)
	}
	if second == nil {
		t.Fatal("the second fail() returned nil")
	}
}
