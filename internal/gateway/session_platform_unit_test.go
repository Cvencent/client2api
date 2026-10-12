package gateway

import (
	"testing"
	"time"
)

// TestSessionPlatformStickinessDropsAnUnusableBinding pins the fallback rule:
// a binding to a platform that no longer appears in the candidate list must be
// forgotten, not force a failure.
func TestSessionPlatformStickinessDropsAnUnusableBinding(t *testing.T) {
	sp := newSessionPlatforms()
	sp.bind("conv-1", "beta")
	if got, ok := sp.resolve("conv-1", func(name string) bool { return name == "alpha" }); ok || got != "" {
		t.Fatalf("resolve = %q,%v, want the unusable binding dropped", got, ok)
	}
	if _, ok := sp.resolve("conv-1", nil); ok {
		t.Fatal("the dropped binding came back")
	}
}

// TestSessionPlatformStickinessExpiresIdleBindings pins the idle window: the
// table must not keep a conversation pinned forever.
func TestSessionPlatformStickinessExpiresIdleBindings(t *testing.T) {
	sp := newSessionPlatforms()
	base := time.Now()
	sp.now = func() time.Time { return base }
	sp.bind("conv-1", "beta")
	sp.now = func() time.Time { return base.Add(sp.ttl + time.Second) }
	if got, ok := sp.resolve("conv-1", nil); ok || got != "" {
		t.Fatalf("resolve after ttl = %q,%v, want expired", got, ok)
	}
}

// TestSessionPlatformStickinessIgnoresEmptyKeys keeps unscoped requests on the
// old rotation path.
func TestSessionPlatformStickinessIgnoresEmptyKeys(t *testing.T) {
	sp := newSessionPlatforms()
	sp.bind("", "beta")
	if got, ok := sp.resolve("", nil); ok || got != "" {
		t.Fatalf("resolve empty key = %q,%v, want no binding", got, ok)
	}
}

// TestSessionPlatformsListLiveRoutes pins the gateway half of the management
// dialog: rows carry the platform and expiry, and expired rows disappear before
// the next routing decision can touch them.
func TestSessionPlatformsListLiveRoutes(t *testing.T) {
	sp := newSessionPlatforms()
	base := time.Now()
	sp.now = func() time.Time { return base }
	sp.bindModel("conv-b", "beta", "m2")
	sp.bindModel("conv-a", "alpha", "m1")

	got := sp.List()
	if len(got) != 2 {
		t.Fatalf("List = %#v, want two live routes", got)
	}
	if got[0].Key != "conv-a" || got[0].Platform != "alpha" {
		t.Fatalf("first route = %#v, want conv-a/alpha", got[0])
	}
	if got[0].Model != "m1" {
		t.Fatalf("first route model = %q, want m1", got[0].Model)
	}
	if want := base.Add(sp.ttl); !got[0].ExpiresAt.Equal(want) {
		t.Fatalf("expiry = %s, want %s", got[0].ExpiresAt, want)
	}

	sp.now = func() time.Time { return base.Add(sp.ttl + time.Second) }
	if got := sp.List(); len(got) != 0 {
		t.Fatalf("expired routes leaked into List: %#v", got)
	}
}

// TestSessionPlatformsForgetReportsTheChange keeps the switch response honest:
// a second click on an already-cleared route must not pretend it did work.
func TestSessionPlatformsForgetReportsTheChange(t *testing.T) {
	sp := newSessionPlatforms()
	sp.bind("conv-1", "beta")
	if !sp.Forget("conv-1") {
		t.Fatal("first Forget reported no change")
	}
	if sp.Forget("conv-1") {
		t.Fatal("second Forget reported a change")
	}
}

// TestSessionPlatformsManualPinSurvivesFailoverBinding pins the force-switch
// contract: a platform chosen in the panel is an operator decision, not the
// last-success heuristic that an automatic failover may replace.
func TestSessionPlatformsManualPinSurvivesFailoverBinding(t *testing.T) {
	sp := newSessionPlatforms()
	if !sp.Pin("conv-1", "molly", "gpt-6.1-sol") {
		t.Fatal("Pin reported no change")
	}

	// The next request fails on Molly and succeeds on CCTQ. The automatic
	// success hook must not silently undo the operator's choice.
	sp.bindModel("conv-1", "cctq", "gpt-6.1-sol")

	got, ok := sp.resolve("conv-1", nil)
	if !ok || got != "molly" {
		t.Fatalf("resolve = %q,%v, want the manual molly pin to survive failover", got, ok)
	}
	rows := sp.List()
	if len(rows) != 1 || rows[0].Platform != "molly" || rows[0].Model != "gpt-6.1-sol" {
		t.Fatalf("routes = %#v, want the molly pin preserved", rows)
	}
}

// TestSessionPlatformsManualPinSurvivesTemporaryDegradation keeps the explicit
// target recorded after its first upstream failure. Automatic stickiness should
// still fall through to a healthy sibling for the current request, but the next
// turn must retry the operator's chosen platform instead of forgetting it.
func TestSessionPlatformsManualPinSurvivesTemporaryDegradation(t *testing.T) {
	sp := newSessionPlatforms()
	if !sp.Pin("conv-1", "molly", "gpt-6.1-sol") {
		t.Fatal("Pin reported no change")
	}

	got, ok := sp.resolve("conv-1", func(name string) bool { return name != "molly" })
	if !ok || got != "molly" {
		t.Fatalf("resolve = %q,%v, want the degraded manual pin retained", got, ok)
	}
	if rows := sp.List(); len(rows) != 1 || rows[0].Platform != "molly" {
		t.Fatalf("routes = %#v, want the manual pin retained after degradation", rows)
	}
}
