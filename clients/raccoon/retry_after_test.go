package raccoon

import (
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// decodeEnvelope parses the 429's Retry-After hint onto the error, and
// classifyFailure hands it to the gateway so the park honours the vendor's own
// window (issue #9).
func TestRetryAfterRidesOnRateLimitedFailure(t *testing.T) {
	h := http.Header{"Retry-After": {"75"}}
	_, err := decodeEnvelope(http.StatusTooManyRequests, []byte(`{"code":1,"message":"too many requests"}`), h)
	ae, ok := err.(*apiError)
	if !ok {
		t.Fatalf("err = %T, want *apiError", err)
	}
	if ae.RetryAfter != 75*time.Second {
		t.Fatalf("apiError.RetryAfter = %v, want 75s", ae.RetryAfter)
	}

	f, ok := core.AsFailure(classifyFailure(ae, "raccoon-1"))
	if !ok {
		t.Fatal("expected a classified failure")
	}
	if f.Kind != core.FailureRateLimited {
		t.Fatalf("kind = %v, want rate limited", f.Kind)
	}
	if f.RetryAfter != 75*time.Second {
		t.Fatalf("Failure.RetryAfter = %v, want 75s", f.RetryAfter)
	}
}

// A 429 without a hint leaves RetryAfter at zero, and the ordinary cooldown
// ladder decides on its own (issue #9).
func TestNoHintKeepsRetryAfterZero(t *testing.T) {
	_, err := decodeEnvelope(http.StatusTooManyRequests, []byte(`{"code":1,"message":"too many requests"}`), nil)
	ae := err.(*apiError)
	if ae.RetryAfter != 0 {
		t.Fatalf("RetryAfter = %v, want 0", ae.RetryAfter)
	}
	f, _ := core.AsFailure(classifyFailure(ae, "raccoon-1"))
	if f.RetryAfter != 0 {
		t.Fatalf("Failure.RetryAfter = %v, want 0", f.RetryAfter)
	}
}
