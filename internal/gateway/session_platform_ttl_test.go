package gateway

import (
	"testing"
	"time"
)

// TestSessionPlatformStickinessFollowsTheConfiguredTTL pins the live knob:
// session_sticky.ttl must retune the gateway-level platform binding too, or
// the operator's window only half-applies.
func TestSessionPlatformStickinessFollowsTheConfiguredTTL(t *testing.T) {
	sp := newSessionPlatforms()
	sp.setTTL(5 * time.Minute)
	base := time.Now()
	sp.now = func() time.Time { return base }
	sp.bind("conv-1", "beta")
	sp.now = func() time.Time { return base.Add(6 * time.Minute) }
	if got, ok := sp.resolve("conv-1", nil); ok || got != "" {
		t.Fatalf("resolve after the configured ttl = %q,%v, want expired", got, ok)
	}
}
