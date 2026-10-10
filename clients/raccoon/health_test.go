package raccoon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestHealthReportsThePoolCensus(t *testing.T) {
	c := affinityTestClient(t)
	h := c.Health()
	if h.Total != 2 || h.Ready != 2 || !h.Servable {
		t.Fatalf("health = %+v, want two ready accounts and Servable", h)
	}

	e := c.pool.find("a1")
	c.pool.markFailureWith(e, kindQuota, "out of credit", time.Hour)
	cooling := c.Health()
	if cooling.Total != 2 || cooling.Ready != 1 || cooling.Cooling != 1 || !cooling.Servable {
		t.Fatalf("health after a park = %+v, want one cooling and still servable", cooling)
	}

	c.pool.markDead(c.pool.find("a2"), "session rejected")
	disabled := c.Health()
	if disabled.Disabled != 1 || disabled.Cooling != 1 || disabled.Ready != 0 || disabled.Servable {
		t.Fatalf("health after a dead credential = %+v", disabled)
	}
	if disabled.Note == "" {
		t.Fatal("a pool with nothing usable must explain why")
	}
}

func TestHealthWithNoAccountsIsExplained(t *testing.T) {
	c := newTestClient(t, t.TempDir(), "{}", nil)
	h := c.Health()
	if h.Servable || h.Total != 0 || h.Note == "" {
		t.Fatalf("health = %+v, want no accounts and a note", h)
	}
}

func TestPoolStatsTracksInFlightAndStickiness(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, `{"choices":[{"delta":{"content":"ok"}}]}`, `{"choices":[{"finish_reason":"stop"}]}`, `[DONE]`)
	}))
	defer ts.Close()

	c := newTestClient(t, t.TempDir(),
		fmt.Sprintf(`{"base_url":%q,"access_token":"tok","user_id":"u1"}`, ts.URL), ts.Client())

	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight before any request = %d, want 0", got)
	}
	stream, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:          "sn-kimi-k3",
		Messages:       []core.Message{{Role: "user", Content: "hi"}},
		ConversationID: "conv-1",
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := c.PoolStats().InFlight; got != 1 {
		t.Fatalf("in-flight with a live stream = %d, want 1", got)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight after Close = %d, want 0", got)
	}
	if got := c.PoolStats().StickySessions; got != 1 {
		t.Fatalf("sticky sessions = %d, want 1", got)
	}
}
