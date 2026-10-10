package opencode

import (
	"testing"
	"time"

	"client2api/internal/core"
)

// TestOpencodeAdvertisesHealthProvider proves the panel can see the optional
// health capability rather than reporting it as unimplemented.
func TestOpencodeAdvertisesHealthProvider(t *testing.T) {
	c := newTestClient(t, Config{})
	if _, ok := core.HealthOf(c); !ok {
		t.Fatal("opencode does not implement core.HealthProvider")
	}
}

// TestOpencodeHealthReportsPoolCensus pins the health counts to the states the
// panel renders: ready, cooling, exhausted and invalid.
func TestOpencodeHealthReportsPoolCensus(t *testing.T) {
	c := newTestClient(t, Config{})
	addAccount(t, c, "opencode:a", "sk-a")
	addAccount(t, c, "opencode:b", "sk-b")

	h := c.Health()
	if h.Total != 2 || h.Ready != 2 || h.Cooling != 0 || h.Disabled != 0 || !h.Servable {
		t.Fatalf("health = %+v, want two ready accounts and Servable", h)
	}

	if !c.pool.setCooldown("opencode:a", testNow.Add(time.Hour), "out of credit") {
		t.Fatal("setCooldown(opencode:a) reported no change")
	}
	h = c.Health()
	if h.Ready != 1 || h.Cooling != 1 || h.Disabled != 0 || !h.Servable {
		t.Fatalf("health after an exhausted park = %+v, want one ready and one cooling", h)
	}

	if !c.pool.setEnabled("opencode:b", false) {
		t.Fatal("setEnabled(opencode:b, false) reported no change")
	}
	h = c.Health()
	if h.Ready != 0 || h.Cooling != 1 || h.Disabled != 1 || h.Servable {
		t.Fatalf("health with nothing usable = %+v, want zero ready and not servable", h)
	}
}

// TestOpencodeHealthWithNoAccountsIsExplained covers the empty-pool answer.
func TestOpencodeHealthWithNoAccountsIsExplained(t *testing.T) {
	c := newTestClient(t, Config{})
	h := c.Health()
	if h.Servable || h.Total != 0 || h.Note == "" {
		t.Fatalf("health = %+v, want an empty, unservable pool with a note", h)
	}
}

// TestOpencodeHealthInitialisesLazyState guards the gateway's call order: it
// reads Health before Status, so Health must perform the same lazy setup.
func TestOpencodeHealthInitialisesLazyState(t *testing.T) {
	t.Setenv(apiKeyEnv, "")
	c := &Client{}
	if h := c.Health(); h.Servable || h.Total != 0 || c.pool == nil || c.now == nil {
		t.Fatalf("health = %+v, pool = %v, now set = %v; want lazy state initialised", h, c.pool, c.now != nil)
	}
}
