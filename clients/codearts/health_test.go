package codearts

import (
	"testing"

	"client2api/internal/core"
)

// TestCodeartsAdvertisesThePoolSurfaces proves the two optional capabilities
// are asserted and visible to the panel's type checks.
func TestCodeartsAdvertisesThePoolSurfaces(t *testing.T) {
	c := affinityTestClient(t)
	if _, ok := core.HealthOf(c); !ok {
		t.Fatal("codearts does not implement core.HealthProvider")
	}
	if _, ok := core.PoolStatsOf(c); !ok {
		t.Fatal("codearts does not implement core.PoolStatsReporter")
	}
}

// TestCodeartsHealthReportsThePoolCensus pins the counts to the states the
// picker produces: ready for a live credential, cooling for a park that heals
// by time, and disabled for a switched-off or dead row.
func TestCodeartsHealthReportsThePoolCensus(t *testing.T) {
	c := affinityTestClient(t)

	h := c.Health()
	if h.Total != 2 || h.Ready != 2 || !h.Servable {
		t.Fatalf("health = %+v, want two ready credentials and Servable", h)
	}

	// Park one account for a day.  It is a quota park, so it must count as
	// cooling, not as a fault.
	e := c.pool.pick(nil)
	if e == nil {
		t.Fatal("the fixture pool has no pickable account")
	}
	c.pool.markFailure(e, kindQuota, "out of credit")

	h = c.Health()
	if h.Cooling != 1 || h.Ready != 1 || !h.Servable {
		t.Fatalf("health after a quota park = %+v, want one cooling and one ready", h)
	}

	// Disabling the remaining account leaves nothing usable.
	for _, ent := range c.pool.all() {
		if err := c.pool.setEnabled(ent.id(), false); err != nil {
			t.Fatalf("setEnabled(%s): %v", ent.id(), err)
		}
	}
	if h := c.Health(); h.Ready != 0 || h.Servable || h.Note == "" {
		t.Fatalf("health with everything off = %+v, want nothing ready, unservable and explained", h)
	}
}

// TestCodeartsHealthWithNoAccountsIsExplained covers the empty pool.
func TestCodeartsHealthWithNoAccountsIsExplained(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	h := c.Health()
	if h.Servable || h.Total != 0 || h.Note == "" {
		t.Fatalf("health = %+v, want an empty, unservable pool with a note", h)
	}
}

// TestCodeartsPoolStatsTrackInFlightAndStickiness proves both numbers are real:
// the in-flight count follows a live stream, and the sticky count is the
// affinity table the picker writes.
func TestCodeartsPoolStatsTrackInFlightAndStickiness(t *testing.T) {
	c := affinityTestClient(t)

	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight before any request = %d, want 0", got)
	}

	c.inFlight.Add(1)
	tracked := core.TrackStream(&blockingStream{}, func() { c.inFlight.Add(-1) })

	if got := c.PoolStats().InFlight; got != 1 {
		t.Fatalf("in-flight with a live stream = %d, want 1", got)
	}
	if err := tracked.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight after Close = %d, want 0", got)
	}

	req := &core.ChatRequest{ConversationID: "conv-1"}
	c.bindServedConversation(req, "a1")
	if got := c.PoolStats().StickySessions; got != 1 {
		t.Fatalf("sticky sessions = %d, want 1", got)
	}
}

// blockingStream is a minimal core.Stream whose Close is idempotent.
type blockingStream struct{}

func (s *blockingStream) Recv() (core.Event, error) { return core.Event{}, nil }
func (s *blockingStream) Close() error              { return nil }
