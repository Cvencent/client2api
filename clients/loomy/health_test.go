package loomy

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// TestHealthReportsThePoolCensus pins the health view to the same verdicts the
// picker uses: a live credential is ready, a quota park is cooling, and a
// session the vendor rejected is disabled.  A declared expiry is deliberately
// not counted as disabled because the picker still tries that row.
func TestHealthReportsThePoolCensus(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	seedAccount(t, c, "loomy-1", testToken, 0)

	h := c.Health()
	if h.Total != 1 || h.Ready != 1 || !h.Servable {
		t.Fatalf("health = %+v, want one ready account and Servable", h)
	}

	c.store.penalise("loomy-1", errors.New("upstream said slow down"), time.Now().UTC(), time.Hour)
	cooling := c.Health()
	if cooling.Cooling != 1 || cooling.Ready != 0 || cooling.Servable {
		t.Fatalf("health after a park = %+v, want cooling and not servable", cooling)
	}
	if cooling.Note == "" {
		t.Fatal("a pool with nothing usable must explain why")
	}

	// A declared expiry is not a local death sentence: the picker still offers
	// the row, so health must not classify it differently.
	seedAccount(t, c, "loomy-expired", testToken, time.Now().Add(-time.Hour).UnixMilli())
	expired := c.Health()
	if expired.Disabled != 0 || expired.Cooling != 1 || expired.Ready != 1 || expired.Total != 2 {
		t.Fatalf("health with a lapsed stamp = %+v, want it still counted as a pool member", expired)
	}

	c.store.penalise("loomy-1", errSessionExpired("loomy-1"), time.Now().UTC(), time.Hour)
	disabled := c.Health()
	if disabled.Disabled != 1 || disabled.Ready != 1 || disabled.Cooling != 0 {
		t.Fatalf("health after a rejected session = %+v, want one disabled account", disabled)
	}
}

func TestHealthWithNoAccountsIsExplained(t *testing.T) {
	c := newTestClient(t, "{}", nil)
	h := c.Health()
	if h.Servable {
		t.Fatalf("an empty pool must not be servable: %+v", h)
	}
	if h.Total != 0 || h.Note == "" {
		t.Fatalf("health = %+v, want no accounts and a note", h)
	}
}

// TestPoolStatsTracksInFlightAndStickiness proves the two numbers are the real
// ones: the in-flight count follows a live stream, and the sticky count is the
// affinity table the request path writes.
func TestPoolStatsTracksInFlightAndStickiness(t *testing.T) {
	rt := &stubTransport{handler: func(r *http.Request, body string) *http.Response {
		if r.URL.Path == "/api/v1/chat/completions" {
			return sseResponse("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
		}
		return jsonResponse(200, okEnvelope("null"))
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight before any request = %d, want 0", got)
	}

	stream, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:          "deepseek-v4-flash-0731",
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
