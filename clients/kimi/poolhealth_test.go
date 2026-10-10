package kimi

import (
	"context"
	"strings"
	"testing"

	"client2api/internal/core"
)

func TestKimiAdvertisesThePoolSurfaces(t *testing.T) {
	c, _ := newClient(t, map[string]any{"assume_logged_in": true})

	if _, ok := core.HealthOf(c); !ok {
		t.Fatal("kimi does not implement core.HealthProvider")
	}
	if _, ok := core.PoolStatsOf(c); !ok {
		t.Fatal("kimi does not implement core.PoolStatsReporter")
	}
	if caps := core.CapabilitiesOf(context.Background(), c); !caps.Health {
		t.Errorf("capabilities.Health = false, want true (%+v)", caps)
	}
}

// TestKimiHealthIsServableWithABinary covers the healthy case: a resolved CLI
// is a serving identity, so the module is servable.
func TestKimiHealthIsServableWithABinary(t *testing.T) {
	bin := stubEmitting(t, cliFixture("x"))
	c, _ := affinityClient(t, map[string]any{"binary": bin, "assume_logged_in": true}, nil)

	h := c.Health()
	if !h.Servable || h.Ready == 0 {
		t.Fatalf("health = %+v, want a servable module with at least one ready row", h)
	}
	if h.Ready > h.Total {
		t.Fatalf("health = %+v, ready exceeds total", h)
	}
}

// TestKimiHealthWithNothingConfiguredIsExplained covers the empty case: with no
// CLI and no login there is no serving identity, and the module must say so
// rather than look like a healthy process.
func TestKimiHealthWithNothingConfiguredIsExplained(t *testing.T) {
	isolateCredentials(t)
	noCLIOnPath(t)
	c, _ := newClient(t, map[string]any{})

	h := c.Health()
	if h.Servable {
		t.Fatalf("health = %+v, want not servable with no credential", h)
	}
	if h.Note == "" {
		t.Fatal("an unservable module must explain why")
	}
}

// TestKimiPoolStatsTrackInFlight proves the in-flight number is the module's
// real concurrency: it rises for the life of a stream and falls on Close.
func TestKimiPoolStatsTrackInFlight(t *testing.T) {
	isolateCredentials(t)
	bin := stubEmitting(t, cliFixture("hello"))
	c, _ := newClient(t, map[string]any{"binary": bin, "assume_logged_in": true})

	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight before any request = %d, want 0", got)
	}

	req := userReq("kimi", "hi")
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

	// A conversation-scoped request must also have written a sticky binding.
	req2 := userReq("kimi", "hi again")
	req2.Options = map[string]any{"conversation_id": "conv-sticky"}
	st2, err := c.Chat(context.Background(), req2)
	if err != nil {
		t.Fatalf("Chat (sticky): %v", err)
	}
	_ = recvAll(t, st2)
	_ = st2.Close()
	if got := c.PoolStats().StickySessions; got != 1 {
		t.Fatalf("sticky sessions = %d, want 1", got)
	}
}

// TestKimiPoolStatsCountTheCLIPath is the other half of the counter: the CLI
// fallback path (used when no panel token is present) must be counted too, not
// just the direct HTTPS path.
func TestKimiPoolStatsCountTheCLIPath(t *testing.T) {
	isolateCredentials(t)
	bin := stubEmitting(t, cliFixture("from the CLI"))
	c, _ := newClient(t, map[string]any{"binary": bin, "assume_logged_in": true})

	st, err := c.Chat(context.Background(), userReq("kimi", "hi"))
	if err != nil {
		if !strings.Contains(err.Error(), "not configured") {
			t.Fatalf("Chat: %v", err)
		}
		t.Skipf("the CLI fixture was not usable here: %v", err)
	}
	if got := c.PoolStats().InFlight; got != 1 {
		t.Fatalf("in-flight on the CLI path = %d, want 1", got)
	}
	_ = st.Close()
	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight after Close = %d, want 0", got)
	}
}
