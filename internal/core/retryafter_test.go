package core

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestRetryAfterOfReadsTheFailureHint pins the channel the gateway uses to
// honour a vendor's own Retry-After instead of its own default backoff.
func TestRetryAfterOfReadsTheFailureHint(t *testing.T) {
	f := Fail("cline", "acct-1", FailureRateLimited, 429, errors.New("slow down"))
	f.RetryAfter = 42 * time.Second
	if got := RetryAfterOf(f); got != 42*time.Second {
		t.Fatalf("direct hint = %v, want 42s", got)
	}
	if got := RetryAfterOf(fmt.Errorf("wrapped: %w", f)); got != 42*time.Second {
		t.Fatalf("wrapped hint = %v, want 42s", got)
	}
	if got := RetryAfterOf(errors.New("plain")); got != 0 {
		t.Fatalf("plain error hint = %v, want 0", got)
	}
	if got := RetryAfterOf(nil); got != 0 {
		t.Fatalf("nil error hint = %v, want 0", got)
	}
}
