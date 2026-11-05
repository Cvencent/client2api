package core

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// retry_after.go is the shared Retry-After / X-Ratelimit-Reset parser
// (issue #210).  Five modules carried copies that read X-Ratelimit-Reset as
// "seconds from now" while their own comments called it an epoch second
// count -- the vendor's 90-second window then parked an account for two
// hours (the caller's cap), and an already-expired window (a reset in the
// past, meaning "you may proceed") parked it too.
//
// The spelling rule this helper implements is the one codearts and
// workbuddy already had and verified against live headers: a value greater
// than 1e6 is a unix timestamp (no vendor sends a 12-day delta), anything
// smaller is a relative second count.  A computed wait of zero or less
// (the reset already happened) returns 0 -- "no wait", never a fallback.
// The caller keeps its own sanity cap.

// retryAfterEpochThreshold is where a reset value stops being a relative
// second count and becomes a unix timestamp.  1e6 seconds is ~11.6 days of
// delta -- no rate-limit window is that long -- while epoch seconds have been
// above 1.7e9 since 2023.
const retryAfterEpochThreshold = 1e6

// RetryAfterReset parses the Retry-After family from header h and returns how
// long the caller should wait, capped at cap.  Zero means "no usable header"
// or "the window already reset".
//
//   - Retry-After: relative seconds or an HTTP date;
//   - X-Ratelimit-Reset (either casing): epoch seconds when > 1e6, else a
//     relative second count;
//   - Retry-After-Ms: milliseconds.
func RetryAfterReset(h http.Header, cap time.Duration) time.Duration {
	if h == nil {
		return 0
	}
	for _, name := range []string{"Retry-After", "X-RateLimit-Reset", "X-Ratelimit-Reset", "Retry-After-Ms"} {
		v := strings.TrimSpace(h.Get(name))
		if v == "" {
			continue
		}
		var d time.Duration
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			if strings.HasSuffix(strings.ToLower(name), "-ms") {
				d = time.Duration(n) * time.Millisecond
			} else if n > retryAfterEpochThreshold {
				// An epoch second count: wait until that moment.
				d = time.Until(time.Unix(int64(n), 0))
			} else {
				d = time.Duration(n * float64(time.Second))
			}
		} else if t, err := http.ParseTime(v); err == nil {
			d = time.Until(t)
		}
		if d <= 0 {
			// Already reset (or an unusable spelling): try the next header,
			// never fall back to a default -- the vendor said "you may go".
			continue
		}
		if d > cap {
			d = cap
		}
		return d
	}
	return 0
}
