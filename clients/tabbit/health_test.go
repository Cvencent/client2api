package tabbit

import (
	"testing"
	"time"

	"client2api/internal/core"
)

// TestTabbitAdvertisesThePoolSurfaces proves the two optional capabilities are
// asserted and visible to the panel's type checks.
func TestTabbitAdvertisesThePoolSurfaces(t *testing.T) {
	c := tabbitPanelClient(t)
	if _, ok := core.HealthOf(c); !ok {
		t.Fatal("tabbit does not implement core.HealthProvider")
	}
	if _, ok := core.PoolStatsOf(c); !ok {
		t.Fatal("tabbit does not implement core.PoolStatsReporter")
	}
}

// TestTabbitHealthWithNoEndpointsIsExplained covers the empty store.
func TestTabbitHealthWithNoEndpointsIsExplained(t *testing.T) {
	c := tabbitPanelClient(t)
	h := c.Health()
	if h.Servable || h.Total != 0 || h.Note == "" {
		t.Fatalf("health = %+v, want an empty, unservable pool with a note", h)
	}
}

// TestTabbitHealthInitialisesLazyState guards the gateway's call order: it
// reads Health before Status, so Health must perform the same lazy setup.
func TestTabbitHealthInitialisesLazyState(t *testing.T) {
	c := &Client{}
	h := c.Health()
	if h.Servable || h.Total != 0 {
		initialised := c.acct != nil && c.webChecks != nil
		t.Fatalf("health = %+v, initialised = %v; want lazy state initialised", h, initialised)
	}
}

// TestTabbitHealthAggregatesSidecarAndWebRows proves the census folds both
// transports out of the same store Accounts() reads: an enabled sidecar is
// ready, a disabled row is a fault, and a web row follows the verdict cache.
func TestTabbitHealthAggregatesSidecarAndWebRows(t *testing.T) {
	c := tabbitPanelClient(t)
	sidecar := addEndpoint(t, c, map[string]string{"kind": kindSidecar, "base_url": "127.0.0.1:5111"})
	web := addEndpoint(t, c, map[string]string{
		"kind":  kindWebToken,
		"token": webJWTFor(t, "uid-web-0009", time.Now().Add(time.Hour)),
	})

	h := c.Health()
	if h.Total != 2 || h.Ready != 2 || !h.Servable {
		t.Fatalf("health = %+v, want two ready rows and Servable", h)
	}

	// A rate-limited web session is resting, not dead.
	c.storeWebVerdict(web.ID, webVerdict{at: time.Now(), kind: core.FailureRateLimited})
	h = c.Health()
	if h.Cooling != 1 || h.Ready != 1 || !h.Servable {
		t.Fatalf("health after a web 429 = %+v, want one cooling and one ready", h)
	}

	// Disabling the sidecar leaves only the cooling web row: nothing serves.
	if err := c.SetAccountEnabled(nil, sidecar.ID, false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	h = c.Health()
	if h.Ready != 0 || h.Cooling != 1 || h.Disabled != 1 || h.Servable || h.Note == "" {
		t.Fatalf("health with nothing usable = %+v, want zero ready and a reason", h)
	}
}

// TestTabbitHealthTreatsARejectedWebCookieAsInvalid proves a non-transient
// verdict from the vendor is a fault, not a cooling park.
func TestTabbitHealthTreatsARejectedWebCookieAsInvalid(t *testing.T) {
	c := tabbitPanelClient(t)
	web := addEndpoint(t, c, map[string]string{
		"kind":  kindWebToken,
		"token": webJWTFor(t, "uid-web-0010", time.Now().Add(time.Hour)),
	})
	c.storeWebVerdict(web.ID, webVerdict{at: time.Now(), err: "the Tabbit web session was rejected (HTTP 401)", kind: core.FailureAuth})
	h := c.Health()
	if h.Disabled != 1 || h.Ready != 0 || h.Servable {
		t.Fatalf("health with a rejected cookie = %+v, want one disabled and unservable", h)
	}
}

// TestTabbitPoolStatsTrackInFlightAndStickiness proves both numbers are real:
// the in-flight count follows a live stream, and the sticky count is the
// affinity table the picker writes.
func TestTabbitPoolStatsTrackInFlightAndStickiness(t *testing.T) {
	f := newFakeWeb(t)
	c, first := newWebClient(t, f)

	if got := c.PoolStats().InFlight; got != 0 {
		t.Fatalf("in-flight before any request = %d, want 0", got)
	}

	stream, err := c.Chat(t.Context(), chatWithKey("conv-1"))
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
	_ = first
}
