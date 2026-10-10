package qwenwork

import (
	"context"
	"testing"

	"client2api/internal/core"
)

// TestQwenworkAdvertisesThePoolSurfaces proves the two optional capabilities
// are asserted and visible to the panel's type checks.
func TestQwenworkAdvertisesThePoolSurfaces(t *testing.T) {
	c := panelClient(t, twoAccountConfig, nil)
	if _, ok := core.HealthOf(c); !ok {
		t.Fatal("qwenwork does not implement core.HealthProvider")
	}
	if _, ok := core.PoolStatsOf(c); !ok {
		t.Fatal("qwenwork does not implement core.PoolStatsReporter")
	}
}

// TestQwenworkHealthReportsThePoolCensus pins the counts to the states the
// picker produces: ready for a live credential, cooling for a quota park, and
// disabled for a switched-off row.
func TestQwenworkHealthReportsThePoolCensus(t *testing.T) {
	c := panelClient(t, twoAccountConfig, nil)

	h := c.Health()
	if h.Total != 2 || h.Ready != 2 || !h.Servable {
		t.Fatalf("health = %+v, want two ready accounts and Servable", h)
	}

	// Park one account for a day: it is a quota park, so it must count as
	// cooling, not as a fault.
	e := c.pool.pick(nil)
	if e == nil {
		t.Fatal("the fixture pool has no pickable account")
	}
	c.pool.markFailureWith(e, kindQuota, "out of credit", 0)

	h = c.Health()
	if h.Cooling != 1 || h.Ready != 1 || !h.Servable {
		t.Fatalf("health after a quota park = %+v, want one cooling and one ready", h)
	}

	// Disabling every account leaves nothing usable.
	for _, ent := range c.pool.entries {
		if err := c.pool.setEnabled(ent.acct.id(), false); err != nil {
			t.Fatalf("setEnabled(%s): %v", ent.acct.id(), err)
		}
	}
	if h := c.Health(); h.Ready != 0 || h.Servable {
		t.Fatalf("health with everything off = %+v, want nothing ready and not servable", h)
	}
}

// TestQwenworkHealthWithNoAccountsIsExplained covers the empty pool.
func TestQwenworkHealthWithNoAccountsIsExplained(t *testing.T) {
	c := panelClient(t, "", nil)
	h := c.Health()
	if h.Servable || h.Total != 0 || h.Note == "" {
		t.Fatalf("health = %+v, want an empty, unservable pool with a note", h)
	}
}

// TestQwenworkPoolStatsTrackInFlightAndStickiness proves both numbers are real:
// the in-flight count follows a live stream, and the sticky count is the
// affinity table the picker writes.
func TestQwenworkPoolStatsTrackInFlightAndStickiness(t *testing.T) {
	c := panelClient(t, twoAccountConfig, chatTransport(chatSSEFrames))

	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight before any request = %d, want 0", got)
	}

	req := &core.ChatRequest{
		Model:          "pro",
		Messages:       []core.Message{{Role: "user", Content: "hi"}},
		ConversationID: "conv-1",
	}
	st, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := c.PoolStats().InFlight; got != 1 {
		t.Fatalf("in-flight with a live stream = %d, want 1", got)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight after Close = %d, want 0", got)
	}
	if got := c.PoolStats().StickySessions; got != 1 {
		t.Fatalf("sticky sessions = %d, want 1", got)
	}
}
