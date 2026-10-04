package panel

import (
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// fakePoolClient is a module that keeps an account pool and can say whether it
// could serve a request right now.  Both surfaces are optional in core, so the
// status page must render them for the modules that opted in and stay silent
// for the ones that did not: "this client has no pool" and "this client's pool
// is idle" are different answers.
type fakePoolClient struct {
	*fakeClient
	pool   core.PoolStats
	health core.Health
}

func (f *fakePoolClient) PoolStats() core.PoolStats { return f.pool }

func (f *fakePoolClient) Health() core.Health { return f.health }

// onlyClient returns the single entry in the status page's client list.
func onlyClient(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	list, ok := body["clients"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("status clients = %v, want exactly one entry", body["clients"])
	}
	entry, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("client entry is %T, want an object", list[0])
	}
	return entry
}

// TestStatusReportsPoolAndHealthWhenTheModuleHasThem mirrors the reference's
// status payload: the in-flight numbers and the health census are part of the
// per-client block, not a separate endpoint.
func TestStatusReportsPoolAndHealthWhenTheModuleHasThem(t *testing.T) {
	c := &fakePoolClient{
		fakeClient: &fakeClient{name: "workbuddy"},
		pool:       core.PoolStats{InFlight: 2, InFlightFull: 1, StickySessions: 4},
		health:     core.Health{Servable: true, Ready: 3, Total: 4, Realms: map[string]bool{"cn": true}},
	}
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := get(t, h, "/panel/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeMap(t, rec)

	// This build has no mirror store, so "noop" is the only answer it can
	// give -- but it must always give one: the page renders it unconditionally.
	if got := body["redis_mode"]; got != "noop" {
		t.Errorf("redis_mode = %v, want noop", got)
	}

	entry := onlyClient(t, body)
	pool, ok := entry["pool"].(map[string]any)
	if !ok {
		t.Fatalf("client entry has no pool: %v", entry)
	}
	if pool["in_flight"] != float64(2) {
		t.Errorf("pool.in_flight = %v, want 2", pool["in_flight"])
	}
	if pool["in_flight_full"] != float64(1) {
		t.Errorf("pool.in_flight_full = %v, want 1", pool["in_flight_full"])
	}
	if pool["sticky_sessions"] != float64(4) {
		t.Errorf("pool.sticky_sessions = %v, want 4", pool["sticky_sessions"])
	}

	health, ok := entry["health"].(map[string]any)
	if !ok {
		t.Fatalf("client entry has no health: %v", entry)
	}
	if health["servable"] != true {
		t.Errorf("health.servable = %v, want true", health["servable"])
	}
	if health["ready"] != float64(3) || health["total"] != float64(4) {
		t.Errorf("health = %v, want ready=3 total=4", health)
	}
}

// TestStatusReportsAnUnservablePoolWithItsReason pins the useful half of the
// health contract: when a module says it cannot serve, the reason reaches the
// page instead of being flattened into a boolean.
func TestStatusReportsAnUnservablePoolWithItsReason(t *testing.T) {
	c := &fakePoolClient{
		fakeClient: &fakeClient{name: "workbuddy"},
		health: core.Health{
			Ready: 2,
			Total: 2,
			Note:  "all 2 usable account(s) are at their in-flight limit",
		},
	}
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	entry := onlyClient(t, decodeMap(t, get(t, h, "/panel/api/status")))
	health, ok := entry["health"].(map[string]any)
	if !ok {
		t.Fatalf("client entry has no health: %v", entry)
	}
	if health["servable"] != false {
		t.Errorf("health.servable = %v, want false", health["servable"])
	}
	if got, _ := health["note"].(string); got == "" {
		t.Errorf("health = %v, want the reason preserved", health)
	}
}

// TestStatusOmitsPoolAndHealthForAModuleWithoutThem is the other side of the
// opt-in contract: a module that never implemented the capabilities must not
// grow a row of zeroes, which would read as "idle" instead of "absent".
func TestStatusOmitsPoolAndHealthForAModuleWithoutThem(t *testing.T) {
	h := New(Options{Registry: registryOf(&fakeClient{name: "plain"}), Started: time.Now()})

	body := decodeMap(t, get(t, h, "/panel/api/status"))
	entry := onlyClient(t, body)
	if _, ok := entry["pool"]; ok {
		t.Errorf("a module without a pool reported one: %v", entry)
	}
	if _, ok := entry["health"]; ok {
		t.Errorf("a module without health reported it: %v", entry)
	}
	if got := body["redis_mode"]; got != "noop" {
		t.Errorf("redis_mode = %v, want noop even with no clients registered", got)
	}
}
