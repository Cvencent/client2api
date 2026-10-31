package openaicompat

import (
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
