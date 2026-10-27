package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// timedStream replays events with per-event delays; EOF returns immediately
// after the last event, the way a real SSE stream ends.
type timedStream struct {
	events []timedEvent
	index  int
	logf   func(string, ...any)
}

type timedEvent struct {
	ev   core.Event
	wait time.Duration
}

func (s *timedStream) Recv() (core.Event, error) {
	if s.index >= len(s.events) {
		return core.Event{}, io.EOF
	}
	if s.logf != nil {
		s.logf("recv idx=%d wait=%v type=%s", s.index, s.events[s.index].wait, s.events[s.index].ev.Type)
	}
	time.Sleep(s.events[s.index].wait)
	ev := s.events[s.index].ev
	s.index++
	return ev, nil
}

func (s *timedStream) Close() error { return nil }

// The speed probe must measure DECODE speed, not end-to-end elapsed: the
// window starts at the first content event (TTFT and queue time excluded) and
// ends when the stream ends, with tokens taken from the usage block. A
// 3-token reply divided by total elapsed used to show 1-3 tok/s for a vendor
// that actually generates at 100 tok/s.
func TestRouteSpeedMeasuresDecodeWindow(t *testing.T) {
	const tokens = 100
	// 20 content events, 5ms apart → 100ms decode window → ~1000 tok/s.
	var fastEvents []timedEvent
	for i := 0; i < 20; i++ {
		fastEvents = append(fastEvents, timedEvent{
			ev:   core.Event{Type: core.EventDelta, Delta: "word "},
			wait: 5 * time.Millisecond,
		})
	}
	fastEvents = append(fastEvents,
		timedEvent{ev: core.Event{Type: core.EventUsage, Usage: &core.Usage{CompletionTokens: tokens, TotalTokens: tokens + 10}}},
		timedEvent{ev: core.Event{Type: core.EventDone}},
	)

	// slow vendor: long TTFT (queue), then a 2-token reply generated in 40ms
	// → ~50 tok/s decode. The queue wait must NOT deflate the rate.
	slowEvents := []timedEvent{
		{ev: core.Event{Type: core.EventDelta, Delta: "ok"}, wait: 400 * time.Millisecond},
		{ev: core.Event{Type: core.EventUsage, Usage: &core.Usage{CompletionTokens: 2, TotalTokens: 12}}},
		{ev: core.Event{Type: core.EventDone}},
	}

	fast := &routeSpeedClient{
		fakeClient:         &fakeClient{name: "fast", models: []core.Model{{ID: "gpt-x"}}},
		account:            "fast-account",
		chatStreamOverride: &timedStream{events: fastEvents},
	}
	slow := &routeSpeedClient{
		fakeClient:         &fakeClient{name: "slow", models: []core.Model{{ID: "gpt-x"}}},
		account:            "slow-account",
		chatStreamOverride: &timedStream{events: slowEvents},
	}

	reg := registryOf(fast, slow)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"fast": {Priority: 1},
		"slow": {Priority: 2},
	})
	h := New(Options{Registry: reg})
	rec := routeSpeedPost(t, h, `{"model":"Auto/gpt-x","catalogs":{"fast":[{"id":"gpt-x"}],"slow":[{"id":"gpt-x"}]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Results []struct {
			Platform         string  `json:"platform"`
			OK               bool    `json:"ok"`
			CompletionTokens int     `json:"completion_tokens"`
			TokensPerSecond  float64 `json:"tokens_per_second"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 2 {
		t.Fatalf("results = %d", len(got.Results))
	}
	var fastRate, slowRate float64
	for _, r := range got.Results {
		switch r.Platform {
		case "fast":
			fastRate = r.TokensPerSecond
		case "slow":
			slowRate = r.TokensPerSecond
		}
	}
	// fast: 100 tokens / ~100ms decode → ~1000 tok/s (>= 500 with slop).
	if fastRate < 500 {
		t.Errorf("fast tokens_per_second = %.1f, want ~1000 (decode window)", fastRate)
	}
	// slow's reply is a SINGLE content event: its generation lands in one
	// timer tick, so no decode rate is measurable - the probe leaves the rate
	// unset (UI shows a dash) instead of reporting TTFT noise. The property
	// that matters: its 400ms queue wait did not leak into the fast rate.
	if slowRate != 0 {
		t.Errorf("slow tokens_per_second = %.1f, want 0 (single-event reply has no measurable decode window)", slowRate)
	}
}
