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
	sp.bind("conv-b", "beta")
	sp.bind("conv-a", "alpha")

	got := sp.List()
	if len(got) != 2 {
		t.Fatalf("List = %#v, want two live routes", got)
	}
	if got[0].Key != "conv-a" || got[0].Platform != "alpha" {
		t.Fatalf("first route = %#v, want conv-a/alpha", got[0])
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
