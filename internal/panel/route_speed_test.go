package panel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

type routeSpeedStream struct {
	events []core.Event
	index  int
	closed bool
}

func (s *routeSpeedStream) Recv() (core.Event, error) {
	if s.index >= len(s.events) {
		return core.Event{}, io.EOF
	}
	ev := s.events[s.index]
	s.index++
	return ev, nil
}

func (s *routeSpeedStream) Close() error {
	s.closed = true
	return nil
}

type routeSpeedClient struct {
	*fakeClient
	account  string
	chatErr  error
	events   []core.Event
	started  chan<- string
	release  <-chan struct{}
	calls    atomic.Int64
	gotModel atomic.Value
	gotFree  atomic.Bool
	// chatStreamOverride, when set, replaces events as the stream the probe
	// consumes (used by tests that need controlled event timing).
	chatStreamOverride core.Stream
}

func (c *routeSpeedClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.calls.Add(1)
	c.gotModel.Store(req.Model)
	c.gotFree.Store(req.FreeModel)
	if c.started != nil {
		c.started <- c.fakeClient.name
	}
	if c.release != nil {
		<-c.release
	}
	if req.ServedBy != nil && c.account != "" {
		*req.ServedBy = c.account
	}
	if c.chatErr != nil {
		return nil, c.chatErr
	}
	if c.chatStreamOverride != nil {
		return c.chatStreamOverride, nil
	}
	return &routeSpeedStream{events: append([]core.Event(nil), c.events...)}, nil
}

func routeSpeedPost(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/route_speed", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRouteSpeedCallsEveryPlatformAndReportsTiming(t *testing.T) {
	alpha := &routeSpeedClient{
		fakeClient: &fakeClient{name: "alpha", models: []core.Model{{ID: "gpt-x"}}},
		account:    "alpha-account",
		events: []core.Event{
			{Type: core.EventDelta, Delta: "o"},
			{Type: core.EventDelta, Delta: "k"},
			{Type: core.EventUsage, Usage: &core.Usage{CompletionTokens: 3}},
			{Type: core.EventDone},
		},
	}
	beta := &routeSpeedClient{
		fakeClient: &fakeClient{name: "beta", models: []core.Model{{ID: "gpt-x"}}},
		account:    "beta-account",
		events: []core.Event{
			{Type: core.EventDelta, Delta: "o"},
			{Type: core.EventUsage, Usage: &core.Usage{CompletionTokens: 1}},
			{Type: core.EventDone},
		},
	}
	reg := registryOf(alpha, beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: 1},
		"beta":  {Priority: 2},
	})
	h := New(Options{Registry: reg})

	rec := routeSpeedPost(t, h, `{
		"model":"Auto/gpt-x",
		"catalogs":{
			"alpha":[{"id":"gpt-x","context_length":200000,"context_source":"manual"}],
			"beta":[{"id":"gpt-x","context_length":1048576,"context_source":"static"}]
		}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Model   string `json:"model"`
		Results []struct {
			Position         int     `json:"position"`
			Platform         string  `json:"platform"`
			ModelID          string  `json:"model_id"`
			OK               bool    `json:"ok"`
			AccountID        string  `json:"account_id"`
			FirstEventMS     int64   `json:"first_event_ms"`
			ElapsedMS        int64   `json:"elapsed_ms"`
			CompletionTokens int     `json:"completion_tokens"`
			TokensPerSecond  float64 `json:"tokens_per_second"`
			Context          int64   `json:"context_length"`
			ContextSrc       string  `json:"context_source"`
			Error            string  `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, rec.Body.String())
	}
	if got.Model != "gpt-x" {
		t.Fatalf("model = %q, want gpt-x", got.Model)
	}
	if len(got.Results) != 2 {
		t.Fatalf("results = %d, want 2: %+v", len(got.Results), got.Results)
	}
	if got.Results[0].Position != 1 || got.Results[0].Platform != "alpha" ||
		got.Results[1].Position != 2 || got.Results[1].Platform != "beta" {
		t.Fatalf("results are not in route order: %+v", got.Results)
	}
	first := got.Results[0]
	if !first.OK || first.ModelID != "gpt-x" || first.AccountID != "alpha-account" {
		t.Fatalf("alpha result = %+v, want a successful gpt-x call on alpha-account", first)
	}
	if first.FirstEventMS < 0 || first.ElapsedMS < 0 {
		t.Fatalf("alpha timing is negative: %+v", first)
	}
	if first.CompletionTokens != 3 {
		t.Fatalf("alpha tokens = %d, want 3", first.CompletionTokens)
	}
	// A 2-delta reply lands inside one timer tick, so no decode window is
	// measurable and the rate is deliberately left unset (UI shows a dash).
	if first.TokensPerSecond != 0 {
		t.Fatalf("alpha tokens_per_second = %.3f, want 0 (no measurable decode window)", first.TokensPerSecond)
	}
	if first.Context != 200000 || first.ContextSrc != "manual" ||
		got.Results[1].Context != 1048576 || got.Results[1].ContextSrc != "static" {
		t.Fatalf("speed results lost the route context: %+v", got.Results)
	}
	if alpha.calls.Load() != 1 || beta.calls.Load() != 1 {
		t.Fatalf("calls = alpha %d / beta %d, want 1 each", alpha.calls.Load(), beta.calls.Load())
	}
	if gotModel := alpha.gotModel.Load(); gotModel != "gpt-x" {
		t.Fatalf("alpha Chat model = %v, want gpt-x", gotModel)
	}
}

func TestRouteSpeedKeepsGoingWhenOnePlatformFails(t *testing.T) {
	alpha := &routeSpeedClient{
		fakeClient: &fakeClient{name: "alpha", models: []core.Model{{ID: "gpt-x"}}},
		chatErr:    errors.New("upstream refused the probe"),
	}
	beta := &routeSpeedClient{
		fakeClient: &fakeClient{name: "beta", models: []core.Model{{ID: "gpt-x"}}},
		account:    "beta-account",
		events: []core.Event{
			{Type: core.EventDelta, Delta: "ok"},
			{Type: core.EventUsage, Usage: &core.Usage{CompletionTokens: 1}},
			{Type: core.EventDone},
		},
	}
	reg := registryOf(alpha, beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: 1},
		"beta":  {Priority: 2},
	})
	h := New(Options{Registry: reg})

	rec := routeSpeedPost(t, h, `{
		"model":"Auto/gpt-x",
		"catalogs":{
			"alpha":[{"id":"gpt-x"}],
			"beta":[{"id":"gpt-x"}]
		}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var got struct {
		Results []struct {
			Platform string `json:"platform"`
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("bad JSON: %v\n%s", err, rec.Body.String())
	}
	if len(got.Results) != 2 {
		t.Fatalf("results = %d, want 2: %+v", len(got.Results), got.Results)
	}
	if got.Results[0].OK || !strings.Contains(got.Results[0].Error, "upstream refused") {
		t.Fatalf("alpha result = %+v, want a recorded failure", got.Results[0])
	}
	if !got.Results[1].OK {
		t.Fatalf("beta result = %+v, want the later platform to still succeed", got.Results[1])
	}
	if alpha.calls.Load() != 1 || beta.calls.Load() != 1 {
		t.Fatalf("calls = alpha %d / beta %d, want 1 each", alpha.calls.Load(), beta.calls.Load())
	}
}

func TestRouteSpeedStreamsEachPlatformAsItCompletes(t *testing.T) {
	started := make(chan string, 2)
	releaseAlpha := make(chan struct{})
	alpha := &routeSpeedClient{
		fakeClient: &fakeClient{name: "alpha", models: []core.Model{{ID: "gpt-x"}}},
		account:    "alpha-account",
		started:    started,
		release:    releaseAlpha,
		events: []core.Event{
			{Type: core.EventDelta, Delta: "ok"},
			{Type: core.EventUsage, Usage: &core.Usage{CompletionTokens: 1}},
			{Type: core.EventDone},
		},
	}
	beta := &routeSpeedClient{
		fakeClient: &fakeClient{name: "beta", models: []core.Model{{ID: "gpt-x"}}},
		account:    "beta-account",
		started:    started,
		events: []core.Event{
			{Type: core.EventDelta, Delta: "ok"},
			{Type: core.EventUsage, Usage: &core.Usage{CompletionTokens: 1}},
			{Type: core.EventDone},
		},
	}
	reg := registryOf(alpha, beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: 1},
		"beta":  {Priority: 2},
	})
	server := httptest.NewServer(New(Options{Registry: reg}))
	defer server.Close()
	released := false
	defer func() {
		if !released {
			close(releaseAlpha)
		}
	}()

	req, err := http.NewRequest(http.MethodPost, server.URL+"/panel/api/route_speed", strings.NewReader(`{
		"model":"Auto/gpt-x",
		"catalogs":{
			"alpha":[{"id":"gpt-x"}],
			"beta":[{"id":"gpt-x"}]
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// Both calls must be in flight before either probe is released. This is the
	// regression guard against the old serial loop.
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for both platform probes to start")
		}
	}

	lines := make(chan string)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	readEvent := func() routeSpeedEvent {
		for {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatal("stream ended before the expected event")
					return routeSpeedEvent{}
				}
				line = strings.TrimSpace(line)
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				var event routeSpeedEvent
				if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
					t.Fatalf("bad SSE event: %v\n%s", err, line)
				}
				return event
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for the next SSE event")
				return routeSpeedEvent{}
			}
		}
	}

	// Alpha is deliberately still blocked. Beta must already have its own
	// flushed event rather than waiting for the whole route chain.
	first := readEvent()
	if first.Done || first.Result == nil || first.Result.Platform != "beta" || !first.Result.OK {
		t.Fatalf("first event = %+v, want a completed beta result", first)
	}

	close(releaseAlpha)
	released = true
	second := readEvent()
	if second.Done || second.Result == nil || second.Result.Platform != "alpha" || !second.Result.OK {
		t.Fatalf("second event = %+v, want the released alpha result", second)
	}
	last := readEvent()
	if !last.Done || last.Result != nil {
		t.Fatalf("last event = %+v, want the stream done marker", last)
	}
	if alpha.calls.Load() != 1 || beta.calls.Load() != 1 {
		t.Fatalf("calls = alpha %d / beta %d, want 1 each", alpha.calls.Load(), beta.calls.Load())
	}
}

func TestRouteSpeedRejectsGet(t *testing.T) {
	h := New(Options{Registry: registryOf()})
	rec := get(t, h, "/panel/api/route_speed")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}

func TestRouteSpeedPassesTheFreeModelFlagToTheModule(t *testing.T) {
	alpha := &routeSpeedClient{
		fakeClient: &fakeClient{name: "alpha", models: []core.Model{{ID: "gpt-x"}}},
		events: []core.Event{
			{Type: core.EventDelta, Delta: "ok"},
			{Type: core.EventDone},
		},
	}
	reg := registryOf(alpha)
	h := New(Options{Registry: reg})

	rec := routeSpeedPost(t, h, `{
		"model":"Auto/gpt-x",
		"catalogs":{"alpha":[{"id":"gpt-x","free":true}]}
	}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !alpha.gotFree.Load() {
		t.Fatal("route speed dropped the free-model flag before calling the platform")
	}
}

func TestRouteSpeedReportsMissingRegistry(t *testing.T) {
	h := New(Options{})
	rec := routeSpeedPost(t, h, `{"model":"Auto/gpt-x","catalogs":{}}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}
