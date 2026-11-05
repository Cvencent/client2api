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
		// An epoch value is a unix timestamp (issue #210): "the vendor said 45s"
		// must read as about 45s, not as the 2h cap.  The window form is used
		// because the parse goes through time.Until.
		{"epoch reset reads as the remaining window", map[string][]string{"X-Ratelimit-Reset": {strconv.FormatInt(time.Now().Add(45*time.Second).Unix(), 10)}}, 45 * time.Second},
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
			case "epoch reset reads as the remaining window":
				// Epoch goes through time.Until: accept a window around 45s
				// (issue #210 -- the old assertion pinned the 2h cap as
				// "correct", protecting the bug instead of the behaviour).
				if got < 30*time.Second || got > 45*time.Second {
					t.Fatalf("RetryAfter = %v, want about 45s (the vendor's own window)", got)
				}
			case "epoch reset relative":
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

// TestClassifyFailureReadsThePreConsumeRefusalAsCredit pins issue #77: the
// pre-consume vendors estimate each request's cost and answer 403 "Not enough
// quota to pre-consume for this request: ¥0.000002 remaining, ¥1.09 required"
// when the estimate exceeds the balance.  The same account answers 200 to
// small requests, so the row must park as quota (and revive when the balance
// sweep sees money) rather than stay ready through FailureModelRefused.
func TestClassifyFailureReadsThePreConsumeRefusalAsCredit(t *testing.T) {
	msg := "Not enough quota to pre-consume for this request: ¥0.000002 remaining, ¥1.092788 required"
	if got := classifyFailure(403, msg); got != kindCredit {
		t.Fatalf("classifyFailure(403, pre-consume refusal) = %v, want kindCredit -- an empty wallet is the account's economy, not a forbidden route", got)
	}
	// The short marker form alone matches too.
	if got := classifyFailure(403, "not enough quota"); got != kindCredit {
		t.Fatalf("classifyFailure(403, %q) = %v, want kindCredit", "not enough quota", got)
	}
	// A genuine route-forbidden 403 keeps its verdict.
	if got := classifyFailure(403, "permission denied for this resource"); got != kindForbidden {
		t.Fatalf("classifyFailure(403, %q) = %v, want kindForbidden", "permission denied for this resource", got)
	}
}

// TestQuotaCooldownBacksOffOnAnUnchangedWallet pins issue #81: the pre-consume
// vendors print the wallet balance in every refusal, and an unchanged amount
// means nobody topped up -- re-asking every 30-minute cycle only buys a
// guaranteed 403 round trip.  The cooldown doubles per identical refusal up to
// a two-hour ceiling; a changed amount (a top-up landed) resets the streak so
// the next cycle is a prompt re-check.
func TestQuotaCooldownBacksOffOnAnUnchangedWallet(t *testing.T) {
	dir := t.TempDir()
	s, err := newSource("rzol", core.SourceConfig{BaseURL: "https://relay.example/v1"}, core.Deps{DataDir: dir})
	if err != nil {
		t.Fatalf("new source: %v", err)
	}
	refusal := "Not enough quota to pre-consume for this request: ¥0.000002 remaining, ¥1.092788 required"

	base := 30 * time.Minute
	// First refusal parks for the base.
	if got := s.quotaCooldown("acct", refusal, base); got != base {
		t.Fatalf("first refusal: cooldown = %v, want the base %v", got, base)
	}
	// The same amount again: nobody topped up, back off.
	if got := s.quotaCooldown("acct", refusal, base); got != 60*time.Minute {
		t.Fatalf("second identical refusal: cooldown = %v, want 1h", got)
	}
	if got := s.quotaCooldown("acct", refusal, base); got != 120*time.Minute {
		t.Fatalf("third identical refusal: cooldown = %v, want 2h", got)
	}
	// The ceiling holds from here on.
	if got := s.quotaCooldown("acct", refusal, base); got != 120*time.Minute {
		t.Fatalf("fourth identical refusal: cooldown = %v, want the 2h ceiling", got)
	}

	// A top-up landed: the vendor's number changed, so the streak resets and
	// the next cycle re-checks promptly.
	topped := "Not enough quota to pre-consume for this request: ¥12.500000 remaining, ¥1.092788 required"
	if got := s.quotaCooldown("acct", topped, base); got != base {
		t.Fatalf("changed amount: cooldown = %v, want the base %v (streak reset)", got, base)
	}

	// A refusal that carries no amount leaves the base alone.
	if got := s.quotaCooldown("acct", "insufficient balance", base); got != base {
		t.Fatalf("unmeasured refusal: cooldown = %v, want the base", got)
	}

	// A different account is tracked independently.
	if got := s.quotaCooldown("other", refusal, base); got != base {
		t.Fatalf("a second account's first refusal = %v, want the base", got)
	}
}
