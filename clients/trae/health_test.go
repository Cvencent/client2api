package trae

import (
	"errors"
	"net/http"
	"testing"

	"client2api/internal/core"
)

func TestHealthReportsThePoolCensus(t *testing.T) {
	a, b := testAuth("A", "ta"), testAuth("B", "tb")
	c := testClient(t, nil, []*Auth{a, b}, nil)

	h := c.Health()
	if h.Total != 2 || h.Ready != 2 || !h.Servable {
		t.Fatalf("health = %+v, want two ready accounts and Servable", h)
	}

	c.pool.MarkFailureForModel(a, "model-x", errors.New("429 too many requests"))
	cooling := c.Health()
	if cooling.Total != 2 || cooling.Ready != 1 || cooling.Cooling != 1 || !cooling.Servable {
		t.Fatalf("health after a park = %+v, want one cooling and still servable", cooling)
	}

	c.pool.MarkInvalid(b, "session rejected")
	disabled := c.Health()
	if disabled.Disabled != 1 || disabled.Cooling != 1 || disabled.Ready != 0 || disabled.Servable {
		t.Fatalf("health after a dead credential = %+v", disabled)
	}
	if disabled.Note == "" {
		t.Fatal("a pool with nothing usable must explain why")
	}
}

func TestHealthWithNoAccountsIsExplained(t *testing.T) {
	c := testClient(t, nil, nil, nil)
	h := c.Health()
	if h.Servable || h.Total != 0 || h.Note == "" {
		t.Fatalf("health = %+v, want no accounts and a note", h)
	}
}

func TestPoolStatsTracksInFlightAndStickiness(t *testing.T) {
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return sseResponse(`data: {"choices":[{"delta":{"content":"ok"}}]}` + "\n\n" +
			`data: {"choices":[{"finish_reason":"stop"}]}` + "\n\n" +
			"data: [DONE]\n\n"), nil
	})
	c := testClient(t, nil, []*Auth{testAuth("A", "ta")}, rt)
	c.affinity = core.NewAffinity(0)

	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight before any request = %d, want 0", got)
	}
	stream, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:          "auto",
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
