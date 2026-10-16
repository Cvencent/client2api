package panel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

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
	calls    atomic.Int64
	gotModel atomic.Value
	gotFree  atomic.Bool
}

func (c *routeSpeedClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	c.calls.Add(1)
	c.gotModel.Store(req.Model)
	c.gotFree.Store(req.FreeModel)
	if req.ServedBy != nil && c.account != "" {
		*req.ServedBy = c.account
	}
	if c.chatErr != nil {
		return nil, c.chatErr
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
			"alpha":[{"id":"gpt-x"}],
			"beta":[{"id":"gpt-x"}]
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
	if first.CompletionTokens != 3 || first.TokensPerSecond <= 0 {
		t.Fatalf("alpha tokens = %d / %.3f, want 3 and a positive rate", first.CompletionTokens, first.TokensPerSecond)
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
