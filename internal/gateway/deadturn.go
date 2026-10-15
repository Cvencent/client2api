package gateway

import (
	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// dead-turn retry
// ---------------------------------------------------------------------------
//
// Some models end an agent turn with "stop" after reasoning about what they are
// about to do, without emitting the tool call they just described.  Nothing is
// broken from the wire's point of view -- the stream terminated normally -- but
// the caller is left with a turn that has nothing to read and nothing to run,
// so Codex closes it and waits for the operator to type the next prompt.
//
// The retry below spends one more upstream attempt on exactly the shapes that
// cannot be a real answer:
//
//   - no visible text at all, or
//   - a short visible fragment after a long deliberation, on a request that
//     actually offered tools, which is the "narrated an intent" variant.
//
// A turn that produced a readable answer is never retried.  "Done." and a
// genuine short reply are indistinguishable from a truncated one, so guessing
// there would risk replacing a real answer; leaving them alone costs nothing but
// the interruption the operator already lives with.
//
// The retry is additive rather than a replacement: every event already
// forwarded to the caller stays, and only the dead attempt's terminal EventDone
// is swallowed.  A retry that cannot even be opened falls through to the
// original terminal event, so this can never turn a served turn into a failed
// one.
const (
	// deadTurnTextLimit bounds the visible answer that still counts as "the
	// model narrated an intent": past it the turn looks like a real answer.
	deadTurnTextLimit = 600
	// deadTurnReasoningFloor is how much deliberation a short visible answer
	// needs before it is suspicious.  A model that thinks briefly and answers
	// briefly ("Ok.", "Done.") is doing something ordinary.
	deadTurnReasoningFloor = 8000
)

// deadTurnStream forwards an attempt and, when it ends as a dead turn, serves a
// second attempt in its place.
type deadTurnStream struct {
	stream core.Stream
	req    *core.ChatRequest
	open   func() (core.Stream, error)
	logf   func(string, ...any)

	textBytes      int
	reasoningBytes int
	toolCalls      int
	retried        bool
}

// wrapDeadTurnRetry adds the retry.  open must return a fresh attempt against
// the same request; nil arguments leave the stream untouched so an embedder
// without a module to re-call is unaffected.
func wrapDeadTurnRetry(stream core.Stream, req *core.ChatRequest, open func() (core.Stream, error), logf func(string, ...any)) core.Stream {
	if stream == nil || open == nil {
		return stream
	}
	return &deadTurnStream{stream: stream, req: req, open: open, logf: logf}
}

func (d *deadTurnStream) Recv() (core.Event, error) {
	for {
		ev, err := d.stream.Recv()
		if err != nil {
			return ev, err
		}
		switch ev.Type {
		case core.EventDelta:
			d.textBytes += len(ev.Delta)
			d.reasoningBytes += len(ev.Reasoning)
		case core.EventToolCall:
			if ev.ToolCall != nil {
				d.toolCalls++
			}
		case core.EventDone:
			if d.dead(ev) {
				next, ok := d.retry()
				if ok {
					d.stream = next
					d.textBytes, d.reasoningBytes, d.toolCalls = 0, 0, 0
					continue
				}
			}
		}
		return ev, nil
	}
}

// dead reports whether the attempt that just ended carried nothing the caller
// can act on.
func (d *deadTurnStream) dead(ev core.Event) bool {
	if d.retried || ev.Finish != "stop" || d.toolCalls > 0 {
		return false
	}
	if d.textBytes == 0 {
		return true
	}
	// A short fragment only looks like a dropped tool call when the caller
	// offered tools: without them the model has no call to make, and
	// "Let me know if you need anything else" is an ordinary closing line.
	if d.req == nil || len(d.req.Tools) == 0 {
		return false
	}
	return d.textBytes < deadTurnTextLimit && d.reasoningBytes > deadTurnReasoningFloor
}

func (d *deadTurnStream) retry() (core.Stream, bool) {
	d.retried = true
	next, err := d.open()
	if err != nil {
		if d.logf != nil {
			d.logf("stream: dead turn (text=%d reasoning=%d bytes, no tool call) could not be retried: %v",
				d.textBytes, d.reasoningBytes, err)
		}
		return nil, false
	}
	if next == nil {
		return nil, false
	}
	if d.logf != nil {
		d.logf("stream: dead turn (text=%d reasoning=%d bytes, no tool call), retrying once",
			d.textBytes, d.reasoningBytes)
	}
	// The attempt that just ended is finished; releasing it here keeps the
	// vendor connection from lingering for the rest of the turn.
	d.stream.Close()
	return next, true
}

func (d *deadTurnStream) Close() error { return d.stream.Close() }
