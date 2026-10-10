package cline

import (
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// TestHealthReportsThePoolCensus pins the health view to the same verdicts the
// picker uses: ready is an account a request could be handed to, cooling is a
// quota park that heals by time, and everything else (a disabled row, a lapsed
// token) is reported as not serving.
func TestHealthReportsThePoolCensus(t *testing.T) {
	f := newFakeServer(t)
	c := newTestClient(t, f, `"access_token":"workos:tok","account_id":"acct-1"`)

	h := c.Health()
	if got, want := h.Total, 1; got != want {
		t.Fatalf("Total = %d, want %d", got, want)
	}
	if h.Ready != 1 || !h.Servable {
		t.Fatalf("health = %+v, want one ready account and Servable", h)
	}

	// A cooling account is still a working credential; a disabled one is not.
	c.pool.markFailureWith(c.pool.get(c.pool.entries[0].acct.id()), kindQuota, "out of credit", time.Hour)
	cooling := c.Health()
	if cooling.Cooling != 1 || cooling.Ready != 0 || cooling.Servable {
		t.Fatalf("health after a park = %+v, want cooling and not servable", cooling)
	}
	if cooling.Note == "" {
		t.Fatal("a pool with nothing usable must explain why")
	}

	c.pool.setEnabled(c.pool.entries[0].acct.id(), false)
	disabled := c.Health()
	if disabled.Disabled != 1 || disabled.Ready != 0 {
		t.Fatalf("health after disabling = %+v, want one disabled account", disabled)
	}
}

// TestHealthWithNoAccountsIsExplained covers the empty pool: it is a
// configuration problem, so it must say so rather than look like an outage.
func TestHealthWithNoAccountsIsExplained(t *testing.T) {
	f := newFakeServer(t)
	c := newTestClient(t, f, "")
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
	f := newFakeServer(t)
	f.json(chatPath, http.StatusOK, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n")
	const token = "workos:tok"
	f.json(userInfoPath, http.StatusOK, `{"data":{"clineUserId":"usr-1","email":"a@b.c"}}`)
	c := newTestClient(t, f, `"access_token":"`+token+`","account_id":"usr-1"`)

	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight before any request = %d, want 0", got)
	}

	req := &core.ChatRequest{
		Model:          "cline-free/deepseek-v4.1-flash",
		Messages:       []core.Message{{Role: "user", Content: "hi"}},
		ConversationID: "conv-1",
	}
	stream, err := c.Chat(t.Context(), req)
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

	// The Chat above was conversation-scoped, so it must have left a binding.
	if got := c.PoolStats().StickySessions; got != 1 {
		t.Fatalf("sticky sessions = %d, want 1", got)
	}
}
