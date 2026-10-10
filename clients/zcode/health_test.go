package zcode

import (
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestHealthReportsThePoolCensus(t *testing.T) {
	c := affinityClient(t)
	h := c.Health()
	if h.Total != 2 || h.Ready != 2 || !h.Servable {
		t.Fatalf("health = %+v, want two ready credentials", h)
	}

	c.pool.mark("a1", func(a *Account) {
		a.State = stateCooling
		a.CooldownUntil = time.Now().Add(time.Hour)
	})
	cooling := c.Health()
	if cooling.Total != 2 || cooling.Ready != 1 || cooling.Cooling != 1 || !cooling.Servable {
		t.Fatalf("health after a park = %+v, want one cooling and still servable", cooling)
	}

	c.pool.mark("a2", func(a *Account) { a.Enabled = false })
	disabled := c.Health()
	if disabled.Disabled != 1 || disabled.Cooling != 1 || disabled.Ready != 0 || disabled.Servable {
		t.Fatalf("health after disabling = %+v", disabled)
	}
	if disabled.Note == "" {
		t.Fatal("a pool with nothing usable must explain why")
	}
}

func TestHealthWithNoAccountsIsExplained(t *testing.T) {
	isolateHome(t)
	c := newTestClient(t, `{"auto_discover":false}`, &fakeTransport{})
	h := c.Health()
	if h.Servable || h.Total != 0 || h.Note == "" {
		t.Fatalf("health = %+v, want no accounts and a note", h)
	}
}

func TestPoolStatsTracksInFlightAndStickiness(t *testing.T) {
	transport := &fakeTransport{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, nonStreamFixture), nil
	}}
	c := newTestClient(t, accountsConfigJSON(1), transport)
	c.pool.mu.Lock()
	c.pool.ensureLocked()
	if len(c.pool.accounts) == 0 {
		c.pool.mu.Unlock()
		t.Fatal("fixture has no account")
	}
	id := c.pool.accounts[0].ID
	c.pool.mu.Unlock()
	c.affinity = core.NewAffinity(0)

	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight before any request = %d, want 0", got)
	}
	stream, err := c.Chat(t.Context(), &core.ChatRequest{
		Model:          "glm-5.3",
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
		t.Fatalf("sticky sessions = %d, want 1 (account %s)", got, id)
	}
}
