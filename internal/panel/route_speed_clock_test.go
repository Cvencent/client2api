package panel

import (
	"io"
	"math"
	"testing"
	"time"

	"client2api/internal/core"
)

// steppedClock is a clock the test moves by hand.  The stream below advances it
// as each event is delivered, so the probe's readings are exact instead of
// depending on where the wall clock happens to fall inside a timer tick -- the
// instability that made the old tick-based assertion untrustworthy.
type steppedClock struct{ at time.Time }

func (c *steppedClock) now() time.Time { return c.at }

// clockedStep is one scripted delivery: the clock reading when the event is
// received plus the event itself.  A step with an empty Type marks end of
// stream (EOF), which is also where the probe reads `elapsed`.
type clockedStep struct {
	at time.Time
	ev core.Event
}

type clockedStream struct {
	clk   *steppedClock
	steps []clockedStep
	index int
}

func (s *clockedStream) Recv() (core.Event, error) {
	if s.index >= len(s.steps) {
		return core.Event{}, io.EOF
	}
	step := s.steps[s.index]
	s.index++
	s.clk.at = step.at
	if step.ev.Type == "" {
		return core.Event{}, io.EOF
	}
	return step.ev, nil
}

func (s *clockedStream) Close() error { return nil }

// speedPanelWithClock builds a one-platform panel whose route-speed probe reads
// the scripted clock and consumes the scripted stream.  Everything the probe
// measures is therefore decided by the test, not by wall-clock jitter.
func speedPanelWithClock(clk *steppedClock, stream core.Stream) *panel {
	client := &routeSpeedClient{
		fakeClient:         &fakeClient{name: "fast", models: []core.Model{{ID: "gpt-x"}}},
		account:            "fast-account",
		chatStreamOverride: stream,
	}
	return &panel{opts: Options{Registry: registryOf(client)}, now: clk.now}
}

func runSpeedStep(t *testing.T, p *panel) routeSpeedResult {
	t.Helper()
	res := p.speedRouteStep(t.Context(), core.RoutePlanStep{Position: 1, Platform: "fast", ModelID: "gpt-x"})
	if !res.OK {
		t.Fatalf("probe failed: %+v", res)
	}
	return res
}

// A multi-event stream's rate is completion tokens divided by the generation
// window -- the gap between the FIRST and the LAST content event -- so
// queueing/TTFT and the trailing usage/done frames never deflate it.  The
// numbers are exact here: 40 tokens over a 20ms window on a 50ms call.
func TestRouteSpeedRateUsesTheContentWindow(t *testing.T) {
	const tokens = 40
	base := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	clk := &steppedClock{at: base}
	stream := &clockedStream{clk: clk, steps: []clockedStep{
		{at: base.Add(10 * time.Millisecond), ev: core.Event{Type: core.EventDelta, Delta: "1 "}},
		{at: base.Add(30 * time.Millisecond), ev: core.Event{Type: core.EventDelta, Delta: "2 "}},
		{at: base.Add(30 * time.Millisecond), ev: core.Event{Type: core.EventUsage, Usage: &core.Usage{CompletionTokens: tokens}}},
		{at: base.Add(30 * time.Millisecond), ev: core.Event{Type: core.EventDone}},
		// The stream ends 20ms after the last content event.  Elapsed is 50ms,
		// the generation window is 20ms: a denominator that regressed to total
		// elapsed would read 800 tok/s (40/50ms) instead of 2000.
		{at: base.Add(50 * time.Millisecond)},
	}}
	res := runSpeedStep(t, speedPanelWithClock(clk, stream))

	if res.FirstEventMS != 10 {
		t.Errorf("first_event_ms = %d, want 10 (TTFT is reported separately)", res.FirstEventMS)
	}
	if res.ElapsedMS != 50 {
		t.Errorf("elapsed_ms = %d, want 50", res.ElapsedMS)
	}
	if res.CompletionTokens != tokens {
		t.Errorf("completion_tokens = %d, want %d", res.CompletionTokens, tokens)
	}
	// 40 tokens / 20ms = 2000 tok/s.  Exact, so a small epsilon is all we need.
	if want := 2000.0; math.Abs(res.TokensPerSecond-want) > 1e-6 {
		t.Errorf("tokens_per_second = %.3f, want %.1f (tokens / generation window)", res.TokensPerSecond, want)
	}
}

// A single content event leaves a zero-width window: a decode rate cannot be
// measured, so the probe leaves it unset and the UI shows a dash.  The stream is
// deliberately slow to end (elapsed - TTFT is 30ms) so this case cannot be
// confused with the elapsed-minus-TTFT fallback below.
func TestRouteSpeedLeavesTheRateBlankForASingleContentEvent(t *testing.T) {
	base := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	clk := &steppedClock{at: base}
	stream := &clockedStream{clk: clk, steps: []clockedStep{
		{at: base.Add(10 * time.Millisecond), ev: core.Event{Type: core.EventDelta, Delta: "ok"}},
		{at: base.Add(10 * time.Millisecond), ev: core.Event{Type: core.EventUsage, Usage: &core.Usage{CompletionTokens: 5}}},
		{at: base.Add(10 * time.Millisecond), ev: core.Event{Type: core.EventDone}},
		{at: base.Add(40 * time.Millisecond)},
	}}
	res := runSpeedStep(t, speedPanelWithClock(clk, stream))

	if res.FirstEventMS != 10 || res.ElapsedMS != 40 {
		t.Errorf("timing = first %dms / elapsed %dms, want 10 / 40", res.FirstEventMS, res.ElapsedMS)
	}
	// elapsed - TTFT is 30ms (nonzero), and total elapsed is 40ms: neither must
	// leak in.  A single event has no measurable window, so the rate stays 0.
	if res.TokensPerSecond != 0 {
		t.Errorf("tokens_per_second = %.3f, want 0 (no measurable decode window)", res.TokensPerSecond)
	}
}

// A stream that stops without ever delivering a content frame leaves nothing to
// bracket, so the probe falls back to elapsed minus TTFT.  With no content
// event, TTFT is unset and the fallback is the whole call -- the path a vendor
// that drops the stream without a terminal frame leaves us on.  (A vendor that
// does send an intact single-event reply is the case above: that stays blank.)
func TestRouteSpeedFallsBackToElapsedWithoutAContentFrame(t *testing.T) {
	const tokens = 10
	base := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	clk := &steppedClock{at: base}
	stream := &clockedStream{clk: clk, steps: []clockedStep{
		{at: base.Add(20 * time.Millisecond), ev: core.Event{Type: core.EventUsage, Usage: &core.Usage{CompletionTokens: tokens}}},
		// No EventDone: the stream ends without a terminal frame.
		{at: base.Add(20 * time.Millisecond)},
	}}
	res := runSpeedStep(t, speedPanelWithClock(clk, stream))

	if res.FirstEventMS != 0 {
		t.Errorf("first_event_ms = %d, want 0 (no content event arrived)", res.FirstEventMS)
	}
	if res.ElapsedMS != 20 {
		t.Errorf("elapsed_ms = %d, want 20", res.ElapsedMS)
	}
	// 10 tokens over the 20ms call (elapsed - TTFT, TTFT unset) = 500 tok/s.
	if want := 500.0; math.Abs(res.TokensPerSecond-want) > 1e-6 {
		t.Errorf("tokens_per_second = %.3f, want %.1f (elapsed minus TTFT fallback)", res.TokensPerSecond, want)
	}
}
