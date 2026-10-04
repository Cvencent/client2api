package main

import (
	"testing"
	"time"
)

// TestExpiringSoonIsPartOfTheLiveSnapshot pins the config plumbing: both the
// module policy and the panel/balance-refresh snapshot must carry the new
// window, or a hot reload only updates half the system.
func TestExpiringSoonIsPartOfTheLiveSnapshot(t *testing.T) {
	cfg := &fileConfig{}
	cfg.Pool.ExpiringSoon = "90m"

	settings := cfg.liveSettings()
	if settings.Pool == nil || settings.Pool.ExpiringSoon == nil || *settings.Pool.ExpiringSoon != 90*time.Minute {
		t.Fatalf("liveSettings expiring window = %#v, want 90m", settings.Pool)
	}
	if got := cfg.liveSnapshot().ExpiringSoon; got != 90*time.Minute {
		t.Fatalf("liveSnapshot expiring window = %s, want 90m", got)
	}
}
