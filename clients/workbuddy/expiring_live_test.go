package workbuddy

import (
	"testing"
	"time"

	"client2api/internal/core"
)

// TestApplyPoolTuningInstallsExpiringWindow pins the missing hop between the
// live configuration and the pool that owns the expiry snapshots.
func TestApplyPoolTuningInstallsExpiringWindow(t *testing.T) {
	c := poolWithPolicy(t)
	window := 2 * time.Hour
	c.applyPoolTuning(&core.PoolTuning{ExpiringSoon: &window})
	if got := c.pool.expiringSoon; got != window {
		t.Fatalf("pool expiring window = %s, want %s", got, window)
	}
}
