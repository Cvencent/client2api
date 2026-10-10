package openaicompat

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"client2api/internal/core"
)

// TestOpenAICompatAdvertisesThePoolSurfaces proves the two optional
// capabilities are asserted and visible to the panel's type checks.
func TestOpenAICompatAdvertisesThePoolSurfaces(t *testing.T) {
	c := testClient(t)
	if _, ok := core.HealthOf(c); !ok {
		t.Fatal("openaicompat does not implement core.HealthProvider")
	}
	if _, ok := core.PoolStatsOf(c); !ok {
		t.Fatal("openaicompat does not implement core.PoolStatsReporter")
	}
}

// TestOpenAICompatHealthReportsThePoolCensus pins the counts to the rows the
// panel renders: enabled is ready, disabled is a fault.
func TestOpenAICompatHealthReportsThePoolCensus(t *testing.T) {
	c := testClient(t)
	h := c.Health()
	if h.Total != 2 || h.Ready != 2 || !h.Servable {
		t.Fatalf("health = %+v, want two ready providers and Servable", h)
	}

	// Disable one provider, the way a config row's disabled field does.
	c.cfg.Providers[1].Disabled = true
	c.pool.reload(c.buildCredentials())
	h = c.Health()
	if h.Disabled != 1 || h.Ready != 1 || !h.Servable {
		t.Fatalf("health after disabling = %+v, want one disabled and one ready", h)
	}

	// Disable the other one: nothing is usable now, so the reason must be given.
	c.cfg.Providers[0].Disabled = true
	c.pool.reload(c.buildCredentials())
	if h := c.Health(); h.Ready != 0 || h.Servable || h.Note == "" {
		t.Fatalf("health with everything off = %+v, want nothing ready, unservable and explained", h)
	}
}

// TestOpenAICompatHealthWithNoProvidersIsExplained covers the empty pool.
func TestOpenAICompatHealthWithNoProvidersIsExplained(t *testing.T) {
	c := testClient(t)
	c.cfg.Providers = nil
	c.pool.reload(nil)
	h := c.Health()
	if h.Servable || h.Total != 0 || h.Note == "" {
		t.Fatalf("health = %+v, want an empty, unservable pool with a note", h)
	}
}

// TestOpenAICompatPoolStatsTrackInFlightAndFull proves the in-flight number is
// the real one: it follows a live stream and is released exactly once on Close,
// and saturation is reported when every enabled provider is at the ceiling.
func TestOpenAICompatPoolStatsTrackInFlightAndFull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	cfg := testConfig(t)
	cfg.MaxInFlight = 1
	// One provider only, so a single live stream saturates the pool.
	cfg.Providers = cfg.Providers[:1]
	for i := range cfg.Providers {
		cfg.Providers[i].BaseURL = srv.URL + "/" + cfg.Providers[i].ID
	}
	raw, _ := New(core.Deps{HTTPClient: srv.Client()})
	c := raw.(*Client)
	c.ensure()
	c.cfg = cfg
	c.pool.reload(c.buildCredentials())

	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight before any request = %d, want 0", got)
	}

	req := &core.ChatRequest{
		Model:    "groq/openai/gpt-oss-120b",
		Messages: []core.Message{{Role: "user", Content: "ping"}},
		Stream:   true,
	}
	req.SetAccountAcquirer(func(string) (func(), error) { return func() {}, nil })
	stream, err := c.Chat(t.Context(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	stats := c.PoolStats()
	if stats.InFlight != 1 {
		t.Fatalf("in-flight with a live stream = %d, want 1", stats.InFlight)
	}
	if stats.InFlightFull != 1 {
		t.Fatalf("in-flight-full with max_in_flight=1 = %d, want 1", stats.InFlightFull)
	}
	if h := c.Health(); h.Servable {
		t.Fatalf("health while every provider is saturated = %+v, want unservable", h)
	}

	// Draining and closing must release exactly one slot.
	_, _, _, _, _, derr := drainStream(stream)
	if derr != nil {
		t.Fatalf("drainStream: %v", derr)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight after Close = %d, want 0", got)
	}
	if got := c.PoolStats().InFlightFull; got != 0 {
		t.Fatalf("in-flight-full after Close = %d, want 0", got)
	}
}

// TestOpenAICompatInFlightSurvivesFailedCall proves a rejected upstream call
// does not leave a phantom slot behind.
func TestOpenAICompatInFlightSurvivesFailedCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()

	cfg := testConfig(t)
	for i := range cfg.Providers {
		cfg.Providers[i].BaseURL = srv.URL + "/" + cfg.Providers[i].ID
	}
	raw, _ := New(core.Deps{HTTPClient: srv.Client()})
	c := raw.(*Client)
	c.ensure()
	c.cfg = cfg
	c.pool.reload(c.buildCredentials())

	req := &core.ChatRequest{
		Model:    "groq/openai/gpt-oss-120b",
		Messages: []core.Message{{Role: "user", Content: "ping"}},
	}
	req.SetAccountAcquirer(func(string) (func(), error) { return func() {}, nil })
	if _, err := c.Chat(t.Context(), req); err == nil {
		t.Fatal("Chat against a 401 provider unexpectedly succeeded")
	}
	stats := c.PoolStats()
	if stats.InFlight != 0 {
		t.Fatalf("in-flight after a refused call = %d, want 0", stats.InFlight)
	}
	if stats.InFlightFull != 0 {
		t.Fatalf("in-flight-full after a refused call = %d, want 0", stats.InFlightFull)
	}
}
