package gateway

import (
	"errors"
	"fmt"
	"testing"

	"client2api/internal/core"
)

// The sentence OpenCode Zen answers an oversized prompt with.  It names no token
// count, so it teaches the guard nothing -- but it is the refusal that must reach
// the caller as context_window_exceeded, because that is the code a Responses
// client compacts on.
const zenContextRefusal = "opencode: chat: HTTP 400: invalid_request_error: Your request " +
	"resulted in more tokens than this model's maximum context length allows. " +
	"Please reduce the length of the messages or run compaction."

func TestLooksLikeContextRefusalRecognisesTheZenSentence(t *testing.T) {
	limit, ok := looksLikeContextRefusal(zenContextRefusal)
	if !ok {
		t.Fatalf("the Zen refusal was not recognised: %q", zenContextRefusal)
	}
	if limit != 0 {
		t.Fatalf("limit = %d, want 0: the sentence names no number", limit)
	}
}

func TestLooksLikeContextRefusalWordings(t *testing.T) {
	positive := []string{
		zenContextRefusal,
		"prompt is too long: 100001 tokens > 100000 maximum",
		"maximum context length is 200000 tokens",
		"this model's maximum context length allows fewer tokens",
		"context length exceeded",
		"input is too long for this model",
		"too many tokens for this model",
		"\u5185\u5bb9\u592a\u957f\u4e86\uff0c\u8bf7\u7cbe\u7b80\u8f93\u5165", // escaped Chinese
	}
	for _, msg := range positive {
		if _, ok := looksLikeContextRefusal(msg); !ok {
			t.Errorf("looksLikeContextRefusal(%q) = false, want true", msg)
		}
	}
}

// The recogniser must not promote an unrelated rejection just because it
// mentions a context field or a length.
func TestLooksLikeContextRefusalRejectsUnrelatedRejections(t *testing.T) {
	negative := []string{
		"no such field: context",
		"invalid api key",
		"rate limit exceeded",
		"insufficient credit",
		"model is not supported",
		"",
	}
	for _, msg := range negative {
		if _, ok := looksLikeContextRefusal(msg); ok {
			t.Errorf("looksLikeContextRefusal(%q) = true, want false", msg)
		}
	}
}

func TestLooksLikeContextRefusalReadsTheLimitWhenNamed(t *testing.T) {
	limit, ok := looksLikeContextRefusal("prompt is too long: 400001 tokens > 200000 maximum")
	if !ok || limit != 200000 {
		t.Fatalf("limit, ok = %d, %v; want 200000, true", limit, ok)
	}
}

// The gateway owns the wire vocabulary.  A module that only saw an unremarkable
// 400 must still produce the code clients compact on.
func TestNormalizeContextRefusalPromotesAnUnclassifiedRefusal(t *testing.T) {
	raw := core.Fail("opencode", "acct", core.FailureOther, 400, errors.New(zenContextRefusal))
	f, ok := core.AsFailure(raw)
	if !ok || f.Kind != core.FailureOther {
		t.Fatalf("precondition failed: %+v", raw)
	}

	got := normalizeContextRefusal(raw)
	f, ok = core.AsFailure(got)
	if !ok {
		t.Fatalf("normalizeContextRefusal returned %T, want a *core.Failure", got)
	}
	if f.Kind != core.FailureContextWindow {
		t.Fatalf("kind = %q, want %q", f.Kind, core.FailureContextWindow)
	}
	if f.Status != 400 {
		t.Fatalf("status = %d, want 400 preserved", f.Status)
	}
	if f.Client != "opencode" || f.Account != "acct" {
		t.Fatalf("attribution lost: client=%q account=%q", f.Client, f.Account)
	}
	if !errors.Is(got, raw) && !errors.Is(got, core.ErrPlatformExhausted) {
		// The wrapper must keep the vendor's own words reachable.
		if f.Err == nil {
			t.Fatalf("the promoted failure dropped its cause")
		}
	}
}

// A classified verdict is never rewritten: a module that said "auth" knows
// something the prose does not.
func TestNormalizeContextRefusalLeavesClassifiedVerdictsAlone(t *testing.T) {
	already := core.Fail("opencode", "acct", core.FailureRateLimited, 429, errors.New("rate limit exceeded"))
	got := normalizeContextRefusal(already)
	f, _ := core.AsFailure(got)
	if f.Kind != core.FailureRateLimited {
		t.Fatalf("kind = %q, want the original %q", f.Kind, core.FailureRateLimited)
	}
}

func TestNormalizeContextRefusalPassesThroughOrdinaryErrors(t *testing.T) {
	plain := errors.New("something unrelated")
	if got := normalizeContextRefusal(plain); got != plain {
		t.Fatalf("an unrelated error was rewritten: %v", got)
	}
	if got := normalizeContextRefusal(nil); got != nil {
		t.Fatalf("nil was rewritten: %v", got)
	}
}

// A numbered refusal that a module failed to classify still gets the right wire
// code and, through the existing learn hook, still teaches the guard the window.
func TestNormalizeContextRefusalKeepsTheNumberedFormLearnable(t *testing.T) {
	raw := errors.New(fmt.Sprintf("upstream chat (http 400): prompt is too long: %d tokens > %d maximum", 400001, 200000))
	got := normalizeContextRefusal(raw)
	f, ok := core.AsFailure(got)
	if !ok || f.Kind != core.FailureContextWindow {
		t.Fatalf("got %v, want a context-window failure", got)
	}
	if limit := parseLimitFromMessage(got.Error()); limit != 200000 {
		t.Fatalf("limit = %d, want 200000 still readable", limit)
	}
}
