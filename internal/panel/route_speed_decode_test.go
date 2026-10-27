package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

// The speed probe measures THROUGHPUT over the response window, from request
// start to the last content frame.
//
// It must not divide by "first content frame -> last content frame".  That span
// collapsed whenever a vendor flushed its answer in bursts, which is what the
// relay in front of DeepSeek-V4.1-Flash did: ~100 completion tokens arrived in
// a few frames a couple of milliseconds apart, so the probe reported
// 400-4400 tok/s for a vendor generating at 100-200 tok/s and the route order
// followed buffer sizes instead of speed.  Burst and smooth delivery of the same
// token count must therefore land in the same range.
func TestRouteSpeedMeasuresThroughputNotFlushWindow(t *testing.T) {
	const tokens = 100

	// Same payload, two delivery styles, both of which a real relay produces.
	burst := []timedEvent{
		{ev: core.Event{Type: core.EventDelta, Delta: strings.Repeat("word ", 25)}, wait: 500 * time.Millisecond},
		{ev: core.Event{Type: core.EventDelta, Delta: strings.Repeat("word ", 25)}},
		{ev: core.Event{Type: core.EventDelta, Delta: strings.Repeat("word ", 25)}},
		{ev: core.Event{Type: core.EventDelta, Delta: strings.Repeat("word ", 25)}},
		{ev: core.Event{Type: core.EventUsage, Usage: &core.Usage{CompletionTokens: tokens, TotalTokens: tokens + 10}}},
		{ev: core.Event{Type: core.EventDone}},
	}
	// The smooth vendor takes the same 500ms to deliver the same payload, but
	// spreads it evenly instead of flushing it all at the end.
	var smooth []timedEvent
	for i := 0; i < 4; i++ {
		smooth = append(smooth, timedEvent{
			ev:   core.Event{Type: core.EventDelta, Delta: strings.Repeat("word ", 25)},
			wait: 500 * time.Millisecond / 4,
		})
	}
	smooth = append(smooth,
		timedEvent{ev: core.Event{Type: core.EventUsage, Usage: &core.Usage{CompletionTokens: tokens, TotalTokens: tokens + 10}}},
		timedEvent{ev: core.Event{Type: core.EventDone}},
	)

	burstClient := &routeSpeedClient{
		fakeClient:         &fakeClient{name: "burst", models: []core.Model{{ID: "gpt-x"}}},
		account:            "burst-account",
		chatStreamOverride: &timedStream{events: burst},
	}
	smoothClient := &routeSpeedClient{
		fakeClient:         &fakeClient{name: "smooth", models: []core.Model{{ID: "gpt-x"}}},
		account:            "smooth-account",
		chatStreamOverride: &timedStream{events: smooth},
	}

	reg := registryOf(burstClient, smoothClient)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"burst":  {Priority: 1},
		"smooth": {Priority: 2},
	})
	h := New(Options{Registry: reg})
	rec := routeSpeedPost(t, h, `{"model":"Auto/gpt-x","catalogs":{"burst":[{"id":"gpt-x"}],"smooth":[{"id":"gpt-x"}]}}`)
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
	rates := map[string]float64{}
	for _, r := range got.Results {
		if !r.OK || r.CompletionTokens != tokens {
			t.Fatalf("%s result = %+v, want ok with %d tokens", r.Platform, r, tokens)
		}
		rates[r.Platform] = r.TokensPerSecond
	}
	// 100 tokens over ~500ms of response (the first frame waits 500ms, the rest
	// are immediate) with a few ms of slop on either side: ~200 tok/s, never
	// the multi-thousand value the flush window used to produce.
	for _, name := range []string{"burst", "smooth"} {
		if rates[name] < 120 || rates[name] > 400 {
			t.Errorf("%s tokens_per_second = %.1f, want ~200 (response window, not flush window)", name, rates[name])
		}
	}
	// The two delivery styles must agree closely: the metric is the vendor's
	// speed, not how the relay chose to buffer it.
	if diff := rates["burst"] - rates["smooth"]; diff > 60 || diff < -60 {
		t.Errorf("burst rate %.1f vs smooth rate %.1f: delivery style changed the reported speed", rates["burst"], rates["smooth"])
	}
}

// This is the shape that produced the 4000-5000 tok/s complaint.  Measured
// through the gateway against DeepSeek-V4.1-Flash on OfficeAce: 110 completion
// tokens, ~0.9s first token, then 11 frames delivered over ~190ms because the
// relay flushes the answer in bursts.  Dividing 110 tokens by that 190ms span
// "measures" 580 tok/s (and 26ms spans in back-to-back runs produced >4000),
// while the request really took ~1.1s, i.e. ~100 tok/s.
func TestRouteSpeedBurstDeliveryDoesNotInflateTheRate(t *testing.T) {
	events := []timedEvent{
		{ev: core.Event{Type: core.EventDelta, Delta: "1 2 3 4 5 6 7 8 9 10 "}, wait: 900 * time.Millisecond},
	}
	for i := 0; i < 9; i++ {
		events = append(events, timedEvent{
			ev:   core.Event{Type: core.EventDelta, Delta: "11 12 13 14 "},
			wait: 20 * time.Millisecond,
		})
	}
	events = append(events,
		timedEvent{ev: core.Event{Type: core.EventUsage, Usage: &core.Usage{CompletionTokens: 110, TotalTokens: 159}}},
		timedEvent{ev: core.Event{Type: core.EventDone}},
	)

	client := &routeSpeedClient{
		fakeClient:         &fakeClient{name: "officeace", models: []core.Model{{ID: "deepseek-v4.1-flash"}}},
		account:            "oa-account",
		chatStreamOverride: &timedStream{events: events},
	}
	reg := registryOf(client)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{"officeace": {Priority: 1}})
	h := New(Options{Registry: reg})
	rec := routeSpeedPost(t, h, `{"model":"Auto/deepseek-v4.1-flash","catalogs":{"officeace":[{"id":"deepseek-v4.1-flash"}]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Results []struct {
			OK               bool    `json:"ok"`
			CompletionTokens int     `json:"completion_tokens"`
			TokensPerSecond  float64 `json:"tokens_per_second"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 1 || !got.Results[0].OK {
		t.Fatalf("results = %+v, want one ok result", got.Results)
	}
	rate := got.Results[0].TokensPerSecond
	// 110 tokens over ~1.08s is ~100 tok/s.  Anything approaching the token
	// count itself means the denominator collapsed to the flush window.
	if rate < 70 || rate > 160 {
		t.Fatalf("tokens_per_second = %.1f, want ~100 (110 tokens over the ~1.1s response)", rate)
	}
}

// A definite answer with no measurable content window reports no rate at all,
// rather than inventing one from a 1ms floor.
func TestRouteSpeedLeavesRateUnsetWithoutContent(t *testing.T) {
	events := []timedEvent{
		{ev: core.Event{Type: core.EventUsage, Usage: &core.Usage{CompletionTokens: 7, TotalTokens: 17}}, wait: 50 * time.Millisecond},
		{ev: core.Event{Type: core.EventDone}},
	}
	client := &routeSpeedClient{
		fakeClient:         &fakeClient{name: "silent", models: []core.Model{{ID: "gpt-x"}}},
		account:            "silent-account",
		chatStreamOverride: &timedStream{events: events},
	}
	reg := registryOf(client)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{"silent": {Priority: 1}})
	h := New(Options{Registry: reg})
	rec := routeSpeedPost(t, h, `{"model":"Auto/gpt-x","catalogs":{"silent":[{"id":"gpt-x"}]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Results []struct {
			OK               bool    `json:"ok"`
			CompletionTokens int     `json:"completion_tokens"`
			TokensPerSecond  float64 `json:"tokens_per_second"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 1 || !got.Results[0].OK || got.Results[0].CompletionTokens != 7 {
		t.Fatalf("results = %+v, want one ok result with 7 tokens", got.Results)
	}
	if got.Results[0].TokensPerSecond != 0 {
		t.Errorf("tokens_per_second = %.1f, want unset (no content frame arrived)", got.Results[0].TokensPerSecond)
	}
}
