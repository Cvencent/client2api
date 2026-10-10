package lobsterai

import (
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// TestLobsteraiAdvertisesHealthProvider proves the panel can see the optional
// health capability rather than reporting it as unimplemented.
func TestLobsteraiAdvertisesHealthProvider(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	if _, ok := core.HealthOf(f.client); !ok {
		t.Fatal("lobsterai does not implement core.HealthProvider")
	}
}

// TestLobsteraiHealthReportsPoolCensus pins the health counts to the states the
// panel renders: ready, cooling and invalid.
func TestLobsteraiHealthReportsPoolCensus(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	first := accountRecord{ID: "lobsterai:a", AccessToken: "access-a", Enabled: true}
	second := accountRecord{ID: "lobsterai:b", AccessToken: "access-b", Enabled: true}
	if !f.client.pool.upsert(first) || !f.client.pool.upsert(second) {
		t.Fatal("seeding the pool reported no change")
	}

	h := f.client.Health()
	if h.Total != 2 || h.Ready != 2 || h.Cooling != 0 || h.Disabled != 0 || !h.Servable {
		t.Fatalf("health = %+v, want two ready accounts and Servable", h)
	}

	if !f.client.pool.setCooldown(first.ID, time.Now().Add(time.Hour), "rate limited") {
		t.Fatal("setCooldown(first) reported no change")
	}
	h = f.client.Health()
	if h.Ready != 1 || h.Cooling != 1 || h.Disabled != 0 || !h.Servable {
		t.Fatalf("health after a cooldown = %+v, want one ready and one cooling", h)
	}

	if !f.client.pool.setEnabled(second.ID, false) {
		t.Fatal("setEnabled(second, false) reported no change")
	}
	h = f.client.Health()
	if h.Ready != 0 || h.Cooling != 1 || h.Disabled != 1 || h.Servable {
		t.Fatalf("health with nothing usable = %+v, want zero ready and not servable", h)
	}
}

// TestLobsteraiHealthWithNoAccountsIsExplained covers the empty-pool answer.
func TestLobsteraiHealthWithNoAccountsIsExplained(t *testing.T) {
	f := newFixture(t, nil, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	h := f.client.Health()
	if h.Servable || h.Total != 0 || h.Note == "" {
		t.Fatalf("health = %+v, want an empty, unservable pool with a note", h)
	}
}

// TestLobsteraiHealthInitialisesLazyState guards the gateway's call order: it
// reads Health before Status, so Health must perform the same lazy setup.
func TestLobsteraiHealthInitialisesLazyState(t *testing.T) {
	c := &Client{}
	if h := c.Health(); h.Servable || h.Total != 0 || c.pool == nil || c.now == nil {
		t.Fatalf("health = %+v, pool = %v, now set = %v; want lazy state initialised", h, c.pool, c.now != nil)
	}
}
