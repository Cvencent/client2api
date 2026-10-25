package workbuddy

import (
	"errors"
	"testing"

	"client2api/internal/core"
)

// TestWorkbuddyAvailableReflectsPerModelParks pins the router-facing probe: a
// model parked on every credential is "platform exhausted", while one healthy
// credential -- or one unparked model on a parked credential -- keeps the
// platform available.  The check is a read, so it must not take a slot or move
// the rotation.
func TestWorkbuddyAvailableReflectsPerModelParks(t *testing.T) {
	c, _ := newTestClient(t, nil)
	c.refreshAccounts(true)
	auths := c.pool.Accounts()
	if len(auths) != 1 {
		t.Fatalf("test client holds %d accounts, want 1", len(auths))
	}

	if err := c.Available("model-x", false); err != nil {
		t.Fatalf("Available on a fresh pool = %v, want nil", err)
	}

	c.pool.MarkModelBlocked(auths[0], "model-x", ModelBlockReason)
	if err := c.Available("model-x", false); !errors.Is(err, core.ErrPlatformExhausted) {
		t.Fatalf("Available with the only model parked = %v, want ErrPlatformExhausted", err)
	}
	if err := c.Available("model-y", false); err != nil {
		t.Fatalf("a per-model park must not block another model: %v", err)
	}

	// The probe is read-only: the same park must still be reported by the
	// picker, and no in-flight slot may have been taken.
	if _, full := c.pool.InFlight(); full != 0 {
		t.Fatalf("the probe took %d in-flight slot(s)", full)
	}
	if c.pool.Ready() == false {
		t.Fatal("the probe must not park the credential itself")
	}
}
