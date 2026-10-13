package gateway

import (
	"testing"
	"time"
)

// TestSessionPlatformsGCReclaimsExpiredEntries pins the fix for the unbounded
// growth risk: a quiet conversation must eventually leave the table even when
// nothing ever touches that key again.
func TestSessionPlatformsGCReclaimsExpiredEntries(t *testing.T) {
	sp := newSessionPlatforms()
	base := time.Now()
	sp.now = func() time.Time { return base }
	sp.bind("expired", "alpha")
	sp.now = func() time.Time { return base.Add(sp.ttl / 2) }
	sp.bind("live", "beta")

	sp.now = func() time.Time { return base.Add(sp.ttl + time.Second) }
	if removed := sp.GC(); removed != 1 {
		t.Fatalf("GC removed %d entries, want 1", removed)
	}
	if got, ok := sp.resolve("live", nil); !ok || got != "beta" {
		t.Fatalf("resolve live = %q,%v, want the live entry retained", got, ok)
	}
	if got := len(sp.entries); got != 1 {
		t.Fatalf("entries after GC = %d, want 1", got)
	}
}

// TestSessionPlatformsGCKeepsLiveManualPins pins the obvious half of the
// operator contract: a manual pin inside its TTL is not collateral damage.
func TestSessionPlatformsGCKeepsLiveManualPins(t *testing.T) {
	sp := newSessionPlatforms()
	base := time.Now()
	sp.now = func() time.Time { return base }
	sp.Pin("pinned", "alpha", "m1")
	sp.bind("expired", "beta")

	sp.now = func() time.Time { return base.Add(sp.ttl / 2) }
	if removed := sp.GC(); removed != 0 {
		t.Fatalf("GC removed %d live entries, want 0", removed)
	}
	if got, ok := sp.resolve("pinned", nil); !ok || got != "alpha" {
		t.Fatalf("resolve pinned = %q,%v, want the manual pin retained", got, ok)
	}
}

// TestSessionPlatformsGCRunsInBackground proves StartGC actually sweeps: the
// table has to shrink without any request touching the expired key.
func TestSessionPlatformsGCRunsInBackground(t *testing.T) {
	sp := newSessionPlatforms()
	base := time.Now()
	sp.now = func() time.Time { return base }
	sp.bind("quiet", "alpha")
	sp.now = func() time.Time { return base.Add(sp.ttl + time.Second) }

	sp.setGCInterval(5 * time.Millisecond)
	sp.StartGC()
	defer sp.StopGC()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		sp.mu.Lock()
		n := len(sp.entries)
		sp.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background GC did not reclaim the expired entry")
}

// TestSessionPlatformsStartGCStopsCleanly pins the lifecycle: StartGC is
// idempotent and StopGC leaves no goroutine running.
func TestSessionPlatformsStartGCStopsCleanly(t *testing.T) {
	sp := newSessionPlatforms()
	sp.setGCInterval(5 * time.Millisecond)
	sp.StartGC()
	sp.StartGC()
	sp.StopGC()
	sp.StopGC()
}
