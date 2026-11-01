package openaicompat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"client2api/internal/core"
)

// A 429 that carries a Retry-After hint must hand that hint to the gateway:
// the park then honours the vendor's own window instead of doubling its own
// ladder (issue #9).  The three header forms the other modules already parse.
func TestClassifyHTTPCarriesRetryAfter(t *testing.T) {
	c := &Client{}
	cases := []struct {
		name   string
		header map[string][]string
		want   time.Duration
	}{
		{"seconds", map[string][]string{"Retry-After": {"60"}}, 60 * time.Second},
		{"http date", map[string][]string{"Retry-After": {time.Now().UTC().Add(90 * time.Second).Format(http.TimeFormat)}}, 0},
		// A large epoch value reads as "seconds", which the 2h cap clamps -- the
		// same coarse behaviour cline's reference parser has.  A small value is
		// the relative spelling some relays send and parses exactly.
		{"epoch reset capped", map[string][]string{"X-Ratelimit-Reset": {strconv.FormatInt(time.Now().Add(45*time.Second).Unix(), 10)}}, 2 * time.Hour},
		{"epoch reset relative", map[string][]string{"X-Ratelimit-Reset": {"45"}}, 45 * time.Second},
		{"milliseconds", map[string][]string{"Retry-After-Ms": {"45000"}}, 45 * time.Second},
		{"absent", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := core.AsFailure(c.classifyHTTP("chat", "acct-1", 429, []byte(`{"error":{"message":"slow down"}}`), http.Header(tc.header)))
			if !ok {
				t.Fatal("expected a classified failure")
			}
			if f.Kind != core.FailureRateLimited {
				t.Fatalf("kind = %v, want rate limited", f.Kind)
			}
			got := f.RetryAfter
			switch tc.name {
			case "http date":
				// The date is now+90s; the parse goes through time.Until, so
				// accept a window rather than an exact value.
				if got < 60*time.Second || got > 90*time.Second {
					t.Fatalf("RetryAfter = %v, want about 90s", got)
				}
			case "epoch reset capped", "epoch reset relative":
				if got != tc.want {
					t.Fatalf("RetryAfter = %v, want %v", got, tc.want)
				}
			default:
				if got != tc.want {
					t.Fatalf("RetryAfter = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestClassifyUpstreamKeepsTheCallerCancellationAlive pins issue #69: a
// context.Canceled (or DeadlineExceeded) that bubbles out of the HTTP round
// trip used to be recognised and then re-wrapped as an upstreamError whose
// Unwrap chain drops the original error -- so the gateway's failureReason
// could no longer probe errors.Is(err, context.Canceled) and the same caller
// cancellation wore the 上游拒绝 label or the 调用方已取消 one depending on
// which layer happened to notice it first.  The identity must survive.
func TestClassifyUpstreamKeepsTheCallerCancellationAlive(t *testing.T) {
	c := &Client{}

	canceled := fmt.Errorf(`Post "https://relay.example/v1/chat/completions": %w`, context.Canceled)
	if got := c.classifyUpstream("acct-1", canceled); !errors.Is(got, context.Canceled) {
		t.Fatalf("classifyUpstream severed the cancellation identity: %v", got)
	}
	// It is also not a *core.Failure: a bare context.Canceled is what the
	// routing loop reads as "nobody's failure".
	if _, ok := core.AsFailure(c.classifyUpstream("acct-1", canceled)); ok {
		t.Fatal("a cancellation was wrapped as a classified failure")
	}

	deadline := fmt.Errorf(`Post "https://relay.example/v1/chat/completions": %w`, context.DeadlineExceeded)
	if got := c.classifyUpstream("acct-1", deadline); !errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("classifyUpstream severed the deadline identity: %v", got)
	}

	// A genuine transport fault keeps its classified envelope.
	plain := errors.New(`Post "https://relay.example/v1/chat/completions": connection reset by peer`)
	f, ok := core.AsFailure(c.classifyUpstream("acct-1", plain))
	if !ok || f.Kind != core.FailureUpstream && f.Kind != core.FailureOther {
		t.Fatalf("a real transport fault lost its envelope: %v", plain)
	}
}
