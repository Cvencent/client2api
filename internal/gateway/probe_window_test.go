package gateway

import (
	"errors"
	"testing"

	"client2api/internal/core"
)

// The probe's job is to decide, before anything reaches the caller, whether a
// platform has already refused.  Its bound is the first frame the caller could
// start reading: a vendor can put its refusal behind a role preamble, an empty
// delta or a usage frame, and none of those carry anything to show.
func TestProbeSkipsFramesTheCallerCannotSee(t *testing.T) {
	stream := &fakeStream{events: []core.Event{
		{Type: core.EventDelta},                       // empty delta
		{Type: core.EventUsage, Usage: &core.Usage{}}, // bookkeeping
		{Type: core.EventDelta, Delta: "hello"},       // first visible byte
		{Type: core.EventDelta, Delta: " world"},      // belongs to the emitter
		{Type: core.EventDone, Finish: "stop"},
	}}

	head, err := probeFirstFrame(stream)
	if err != nil {
		t.Fatalf("probeFirstFrame: %v", err)
	}
	// Everything up to and including the first visible delta is handed back, in
	// order, so the emitter replays the stream unchanged.
	if len(head) != 3 {
		t.Fatalf("head has %d frame(s), want 3 up to the first visible delta", len(head))
	}
	if head[2].Delta != "hello" {
		t.Fatalf("head[2].Delta = %q, want the first visible delta", head[2].Delta)
	}
	// The frames after the visible byte are left in the stream.
	next, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv after probe: %v", err)
	}
	if next.Delta != " world" {
		t.Fatalf("next frame = %q, want the untouched remainder", next.Delta)
	}
}

// A refusal behind invisible frames is still recoverable, which is the whole
// point of extending the probe past the first frame.  This is the shape the
// live loomy failure had: a refusal dressed up as HTTP 200 and delivered inside
// the stream.
func TestProbeRecoversARefusalBehindEmptyFrames(t *testing.T) {
	want := core.Fail("loomy", "acct", core.FailureUpstream, 0, errors.New("回答生成中断，请重试"))
	stream := &fakeStream{events: []core.Event{
		{Type: core.EventDelta},
		{Type: core.EventUsage, Usage: &core.Usage{}},
		{Type: core.EventError, Err: want},
	}}

	_, err := probeFirstFrame(stream)
	if err == nil {
		t.Fatal("probeFirstFrame swallowed a retryable refusal behind invisible frames")
	}
	if !core.Retryable(core.FailureKindOf(err)) {
		t.Fatalf("err kind = %q, want a retryable kind so the chain can move on", core.FailureKindOf(err))
	}
}

// Anything the caller can already watch stream closes the probe window, and
// that includes reasoning: buffering a thinking model's reasoning would stall
// its stream until the first content frame, which for a long-thinking model can
// be most of the turn.
func TestProbeStopsAtVisibleContent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event core.Event
	}{
		{"content", core.Event{Type: core.EventDelta, Delta: "partial answer"}},
		{"reasoning", core.Event{Type: core.EventDelta, Reasoning: "thinking out loud"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := &fakeStream{events: []core.Event{
				tc.event,
				{Type: core.EventError, Err: core.Fail("x", "a", core.FailureUpstream, 0, errors.New("boom"))},
			}}

			head, err := probeFirstFrame(stream)
			if err != nil {
				t.Fatalf("probeFirstFrame returned an error after visible content: %v", err)
			}
			if len(head) != 1 {
				t.Fatalf("head has %d frame(s), want just the visible one", len(head))
			}
			// The error stays in the stream for the emitter to report, because
			// the answer is already committed.
			ev, err := stream.Recv()
			if err != nil {
				t.Fatalf("Recv: %v", err)
			}
			if ev.Type != core.EventError {
				t.Fatalf("next event = %v, want the error left for the emitter", ev.Type)
			}
		})
	}
}

// A tool call is an answer the caller acts on, so it closes the probe window
// exactly like text does.
func TestProbeStopsAtAToolCall(t *testing.T) {
	stream := &fakeStream{events: []core.Event{
		{Type: core.EventToolCall, ToolCall: &core.ToolCallDelta{Index: 0, Name: "read"}},
		{Type: core.EventDone, Finish: "tool_calls"},
	}}

	head, err := probeFirstFrame(stream)
	if err != nil {
		t.Fatalf("probeFirstFrame: %v", err)
	}
	if len(head) != 1 || head[0].Type != core.EventToolCall {
		t.Fatalf("head = %+v, want the tool call alone", head)
	}
}

// A stream that closes while nothing visible has been produced is still worth a
// sibling platform.
func TestProbeRecoversABreakBeforeVisibleContent(t *testing.T) {
	stream := &fakeStream{events: []core.Event{
		{Type: core.EventUsage, Usage: &core.Usage{}},
		{Type: core.EventDone, Finish: "stop"},
	}}
	// A stream whose events run out answers io.EOF.  The terminal frame stops the
	// probe, so the interesting shape is a close with no terminal frame at all.
	silent := &fakeStream{events: []core.Event{{Type: core.EventUsage, Usage: &core.Usage{}}}}
	if _, err := probeFirstFrame(silent); err == nil {
		t.Fatal("probeFirstFrame treated a silent close as success")
	}
	// And a terminal frame is a normal end, not a failure to route around.
	if _, err := probeFirstFrame(stream); err != nil {
		t.Fatalf("probeFirstFrame on a completed stream: %v", err)
	}
}

// A content-policy refusal is the platform's answer and must reach the caller
// rather than being retried on another account.
func TestProbeShowsANonRetryableRefusal(t *testing.T) {
	refusal := core.Fail("x", "a", core.FailureContentBlocked, 0, errors.New("content policy"))
	stream := &fakeStream{events: []core.Event{{Type: core.EventError, Err: refusal}}}

	head, err := probeFirstFrame(stream)
	if err != nil {
		t.Fatalf("probeFirstFrame: %v", err)
	}
	if len(head) != 1 || head[0].Type != core.EventError {
		t.Fatal("a content-policy refusal was not passed through to the caller")
	}
}
