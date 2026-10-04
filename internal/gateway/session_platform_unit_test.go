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
