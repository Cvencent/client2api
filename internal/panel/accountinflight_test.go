package panel

import (
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// fakeTracker stands in for the gateway's per-account in-flight counters.
type fakeTracker map[string]int

func (f fakeTracker) AccountInFlight(platform, accountID string) int {
	return f[platform+"\x1f"+accountID]
}

// statusAccounts builds one module whose Status reports two accounts: one with
// no in-flight bookkeeping of its own, one that reports its own lease count
// and its own ceiling.
func statusAccounts() []core.AccountStatus {
	return []core.AccountStatus{
		{ID: "acct-plain", Enabled: true, State: "ready"},
		{ID: "acct-own", Enabled: true, State: "ready", Extra: map[string]any{"in_flight": 5, "in_flight_limit": 1}},
	}
}

// TestStatusDecoratesTheInflightColumn pins the account pool's 在途 contract:
// the ceiling comes from the platform configuration, the current count comes
// from the module when it has one and from the gateway's own counters
// otherwise, and the two are visible on every platform -- not only on modules
// that keep an in-flight count of their own.
func TestStatusDecoratesTheInflightColumn(t *testing.T) {
	c := &fakeClient{name: "zcode", status: core.Status{Name: "zcode", Ready: true, Accounts: statusAccounts()}}
	reg := registryOf(c)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{"zcode": {MaxInFlightPerAccount: 3}})
	h := New(Options{
		Registry: reg,
		Started:  time.Now(),
		InFlight: fakeTracker{"zcode\x1facct-plain": 2, "zcode\x1facct-own": 7},
	})

	rec := get(t, h, "/panel/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	entry := onlyClient(t, decodeMap(t, rec))
	accounts, ok := entry["accounts"].([]any)
	if !ok || len(accounts) != 2 {
		t.Fatalf("accounts = %v, want two entries", entry["accounts"])
	}
	extraOf := func(id string) map[string]any {
		for _, raw := range accounts {
			a, _ := raw.(map[string]any)
			if a["id"] == id {
				ex, _ := a["extra"].(map[string]any)
				return ex
			}
		}
		t.Fatalf("no account row %q in %v", id, accounts)
		return nil
	}

	// A module that reports nothing still gets a live count (the gateway's own
	// bookkeeping) plus the configured ceiling.
	plain := extraOf("acct-plain")
	if plain["in_flight"] != float64(2) {
		t.Errorf("acct-plain in_flight = %v, want the gateway's 2", plain["in_flight"])
	}
	if plain["in_flight_limit"] != float64(3) {
		t.Errorf("acct-plain in_flight_limit = %v, want the platform setting 3", plain["in_flight_limit"])
	}

	// A module that keeps its own numbers owns the count, and the displayed
	// ceiling is the tighter of its own and the platform's.
	own := extraOf("acct-own")
	if own["in_flight"] != float64(5) {
		t.Errorf("acct-own in_flight = %v, want the module's 5", own["in_flight"])
	}
	if own["in_flight_limit"] != float64(1) {
		t.Errorf("acct-own in_flight_limit = %v, want the module's tighter 1", own["in_flight_limit"])
	}
}

// TestStatusUnlimitedPlatformKeepsTheInfinityRule: an explicit 0 (or an absent
// ceiling) means "no ceiling"; the column must not turn that into a number.
func TestStatusUnlimitedPlatformKeepsTheInfinityRule(t *testing.T) {
	c := &fakeClient{name: "zcode", status: core.Status{Name: "zcode", Accounts: statusAccounts()}}
	reg := registryOf(c)
	h := New(Options{Registry: reg, Started: time.Now(), InFlight: fakeTracker{}})

	rec := get(t, h, "/panel/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	entry := onlyClient(t, decodeMap(t, rec))
	accounts := entry["accounts"].([]any)
	a := accounts[0].(map[string]any)
	ex := a["extra"].(map[string]any)
	if got := ex["in_flight_limit"]; got != float64(0) {
		t.Errorf("in_flight_limit = %v, want 0 (the page renders that as ∞)", got)
	}
	// No traffic ever routed: the gateway's counter says 0, not "unknown".
	if got := ex["in_flight"]; got != float64(0) {
		t.Errorf("in_flight = %v, want 0", got)
	}
}

// TestStatusWithoutGatewayCountersLeavesTheCountBlank: with the platform
// policy but no gateway counters wired (an embedder or a narrow test), the
// ceiling still comes from the configuration but no in-flight number may be
// invented -- the cell draws — when the count is missing, and that is the
// honest answer.
func TestStatusWithoutGatewayCountersLeavesTheCountBlank(t *testing.T) {
	c := &fakeClient{name: "zcode", status: core.Status{Name: "zcode", Accounts: statusAccounts()}}
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := get(t, h, "/panel/api/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	entry := onlyClient(t, decodeMap(t, rec))
	accounts := entry["accounts"].([]any)
	plain := accounts[0].(map[string]any)
	ex := plain["extra"].(map[string]any)
	if _, ok := ex["in_flight"]; ok {
		t.Errorf("plain account in_flight = %v, want the key absent (nothing counts it)", ex["in_flight"])
	}
	if ex["in_flight_limit"] != float64(0) {
		t.Errorf("plain account in_flight_limit = %v, want 0 (platform default: unlimited)", ex["in_flight_limit"])
	}
	own := accounts[1].(map[string]any)
	ownEx := own["extra"].(map[string]any)
	if ownEx["in_flight"] != float64(5) || ownEx["in_flight_limit"] != float64(1) {
		t.Errorf("own account extra = %v, want the module's untouched numbers", ownEx)
	}
}
