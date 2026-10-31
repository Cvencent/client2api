package loomy

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"client2api/internal/core"
)

// A 429 that carries a Retry-After hint must reach the gateway as a classified
// Failure with that hint, so the park honours the vendor's own window instead
// of doubling its own ladder (issue #9).
func TestClassifyFailureCarriesRetryAfterOnRateLimit(t *testing.T) {
	api := &apiError{Status: http.StatusTooManyRequests, Message: "slow down", RetryAfter: 90 * time.Second}
	f, ok := core.AsFailure(classifyFailure(api, "loomy-1"))
	if !ok {
		t.Fatal("expected a classified failure")
	}
	if f.Kind != core.FailureRateLimited {
		t.Fatalf("kind = %v, want rate limited", f.Kind)
	}
	if f.RetryAfter != 90*time.Second {
		t.Fatalf("RetryAfter = %v, want 90s", f.RetryAfter)
	}
}

// parseRetryAfter reads the three header forms the other modules already do,
// capped at two hours (issue #9).
func TestParseRetryAfterForms(t *testing.T) {
	if got := parseRetryAfter(http.Header{}); got != 0 {
		t.Fatalf("empty header = %v, want 0", got)
	}
	if got := parseRetryAfter(http.Header{"Retry-After": {"60"}}); got != 60*time.Second {
		t.Fatalf("Retry-After: 60 -> %v, want 60s", got)
	}
	if got := parseRetryAfter(http.Header{"Retry-After-Ms": {"45000"}}); got != 45*time.Second {
		t.Fatalf("Retry-After-Ms: 45000 -> %v, want 45s", got)
	}
	if got := parseRetryAfter(http.Header{"Retry-After": {strconv.FormatInt(3*3600, 10)}}); got != 2*time.Hour {
		t.Fatalf("Retry-After: 3h -> %v, want the 2h cap", got)
	}
}

// The module's own cooldown takes the vendor's hint when it is longer than the
// ladder stage, and the ladder still advances (issue #9).
func TestPenaliseChatWithStageHonoursRetryAfter(t *testing.T) {
	rt := alwaysJSON(200, okEnvelope(`{"balance":5}`))
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)
	now := time.Now().UTC()
	err := &apiError{Status: http.StatusTooManyRequests, RetryAfter: 90 * time.Second}

	if !c.store.penaliseChatWithStage("loomy-1", err, now, 0, time.Minute) {
		t.Fatal("stage CAS lost on a fresh account")
	}
	a, ok := c.store.lookup("loomy-1")
	if !ok {
		t.Fatal("the penalised account vanished")
	}
	if a.cooldownStage != 1 {
		t.Fatalf("stage = %d, want 1", a.cooldownStage)
	}
	if d := a.cooldownTill.Sub(now); d < 89*time.Second || d > 91*time.Second {
		t.Fatalf("cooldown = %v, want the vendor's 90s over the 30s stage", d)
	}
}

func TestRetryAfterOf(t *testing.T) {
	if got := retryAfterOf(&apiError{RetryAfter: 5 * time.Second}); got != 5*time.Second {
		t.Fatalf("apiError hint = %v, want 5s", got)
	}
	if got := retryAfterOf(errors.New("bare")); got != 0 {
		t.Fatalf("bare error = %v, want 0", got)
	}
}
