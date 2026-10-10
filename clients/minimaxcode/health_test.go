package minimaxcode

import (
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestHealthReportsThePoolCensus(t *testing.T) {
	c, _ := newTestClient(t, oneAccount(), nil)
	h := c.Health()
	if h.Total != 1 || h.Ready != 1 || !h.Servable {
		t.Fatalf("health = %+v, want one ready account and Servable", h)
	}

	row := c.pool.find("acct-1")
	c.pool.mark("acct-1", func(a *Account) {
		a.State = stateCooling
		a.CooldownUntil = time.Now().Add(time.Hour)
		a.LastError = "rate limited"
	})
	if row == nil {
		t.Fatal("fixture lost the account")
	}
	cooling := c.Health()
	if cooling.Cooling != 1 || cooling.Ready != 0 || cooling.Servable {
		t.Fatalf("health after a park = %+v, want cooling and not servable", cooling)
	}
	if cooling.Note == "" {
		t.Fatal("a pool with nothing usable must explain why")
	}

	c.pool.mark("acct-1", func(a *Account) { a.Enabled = false })
	disabled := c.Health()
	if disabled.Disabled != 1 || disabled.Cooling != 0 || disabled.Ready != 0 {
		t.Fatalf("health after disabling = %+v, want one disabled account", disabled)
	}
}

func TestHealthWithNoAccountsIsExplained(t *testing.T) {
	c, _ := newTestClient(t, map[string]any{}, nil)
	h := c.Health()
	if h.Servable || h.Total != 0 || h.Note == "" {
		t.Fatalf("health = %+v, want no accounts and a note", h)
	}
}

func TestPoolStatsTracksInFlightAndStickiness(t *testing.T) {
	c, _ := newTestClient(t, oneAccount(), func(*http.Request) (*http.Response, error) {
		return newResponse(200, "application/json", reply), nil
	})
	c.affinity = core.NewAffinity(0)

	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight before any request = %d, want 0", got)
	}
	stream, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:          "MiniMax-M3",
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
