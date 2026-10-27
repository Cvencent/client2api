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

// A multi-event stream's rate is completion tokens over request start ->
// lastContent -- queueing and prompt processing stay in the denominator
// because they are part of what the caller waits for; only the trailing
// usage/done frames are excluded.  The numbers are exact here: 40 tokens over
// a 30ms window on a 50ms call.
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
		// the rated window is 30ms (start -> lastContent): a denominator that
		// regressed to total elapsed would read 800 tok/s (40/50ms) instead of
		// 1333.
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
	// 40 tokens / 30ms = 1333 tok/s.  Exact, so a small epsilon is all we need.
	if want := 4000.0 / 3.0; math.Abs(res.TokensPerSecond-want) > 1e-6 {
		t.Errorf("tokens_per_second = %.3f, want %.1f (tokens / whole-response window)", res.TokensPerSecond, want)
	}
}

// A single content event with the usage frame landing in the same instant as
// the first content frame brackets a 10ms window (request start -> the content
// stamp); the rate is well-defined there (5 tokens / 10ms = 500 tok/s), so the
// probe reports it instead of leaving a dash.
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
	// The window is request start (0ms) -> the single content event (10ms);
	// the trailing usage/done/EOF stamps are excluded, so 5/10ms = 500.
	if want := 500.0; math.Abs(res.TokensPerSecond-want) > 1e-6 {
		t.Errorf("tokens_per_second = %.3f, want %.1f (start -> lastContent window)", res.TokensPerSecond, want)
	}
}

// A stream that stops without ever delivering a content frame cannot bracket a
// window from request start to lastContent.  It used to fall back to elapsed
// minus TTFT (with TTFT unset, i.e. the whole call); the throughput semantics
// report no rate instead -- there is no observable content window, and a rate
// computed from trailing bookkeeping frames is TTFT noise.  (A vendor that
// sends an intact single-event reply is the case above: that stays blank too.)
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
	if res.TokensPerSecond != 0 {
		t.Errorf("tokens_per_second = %.3f, want 0 (no content window to divide by)", res.TokensPerSecond)
	}
}
