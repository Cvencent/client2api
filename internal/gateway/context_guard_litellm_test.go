package gateway

import (
	"strings"
	"testing"

	"client2api/internal/core"
)

// litellm fronts several of the platforms this gateway routes to, and its
// refusal puts the LIMIT first and the measured size second -- the opposite of
// the "N tokens > M maximum" shape the parser was written for:
//
//	This model's maximum context length is 1048576 tokens. However, you
//	requested 1263369 tokens (1163369 in the messages, 100000 in the completion).
//
// The old parser matched "maximum context length is N tokens" and stopped there,
// so it reported a limit and no observed size. The measured-refusal retry is
// gated on having BOTH numbers, so it never fired and the caller got a dead 400
// it could only answer by hand. Replayed verbatim.
const litellmContextRefusal = "raccoon: HTTP 400: litellm.ContextWindowExceededError: " +
	"litellm.BadRequestError: ContextWindowExceededError: Custom_raccoonException - " +
	"This model's maximum context length is 1048576 tokens. However, you requested " +
	"1263369 tokens (1163369 in the messages, 100000 in the completion). " +
	"Please reduce the length of the messages or completion. " +
	"(request_id: b95e8de0-7749-47e8-ab3c-ec221fe9e160)"

func TestParseRefusalSizesReadsLimitBeforeObserved(t *testing.T) {
	sizes := parseRefusalSizes(litellmContextRefusal)
	if sizes.Limit != 1048576 {
		t.Fatalf("Limit = %d, want 1048576", sizes.Limit)
	}
	if sizes.Observed != 1263369 {
		t.Fatalf("Observed = %d, want 1263369 -- without it the measured retry cannot fire", sizes.Observed)
	}
}

// The parser must keep reporting both numbers whichever order the vendor writes
// them in, and must never swap them: the observed size is the larger one.
func TestParseRefusalSizesBothOrders(t *testing.T) {
	cases := []struct {
		name string
		msg  string
	}{
		{"observed-then-limit", "prompt is too long: 100001 tokens > 100000 maximum"},
		{"limit-then-observed", "maximum context length is 100000 tokens. However, you requested 100001 tokens"},
		{"with-separators", "maximum context length is 1,048,576 tokens. However, you requested 1,263,369 tokens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sizes := parseRefusalSizes(tc.msg)
			if sizes.Observed <= sizes.Limit {
				t.Fatalf("%s: Observed=%d must exceed Limit=%d", tc.name, sizes.Observed, sizes.Limit)
			}
		})
	}
}

// A refusal that names only its window still reports no observed size, so
// nothing invents a measurement the vendor never made.
func TestParseRefusalSizesLimitOnlyStaysUnmeasured(t *testing.T) {
	sizes := parseRefusalSizes("context_length_exceeded: maximum context length is 1048576 tokens")
	if sizes.Limit != 1048576 || sizes.Observed != 0 {
		t.Fatalf("sizes = %+v, want limit 1048576 and no observed", sizes)
	}
}

// The point of reading the measured size: a refusal in this shape must produce
// a retry budget the guard can trim to, instead of a dead 400 the caller can
// only answer by hand. The numbers are the real ones from the bug report.
func TestLitellmRefusalProducesARetryBudget(t *testing.T) {
	sizes := parseRefusalSizes(litellmContextRefusal)
	// The refusal measured 1263369 against a 1048576 window; the gateway's own
	// estimate for the same bytes reads lower, which is why the proactive guard
	// let it through in the first place.
	req := &core.ChatRequest{Model: "sn-deepseek-v4-1-flash-1017"}
	bulk := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 40000)
	req.Messages = []core.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 22; i++ {
		req.Messages = append(req.Messages, core.Message{Role: "user", Content: bulk})
	}
	estimated := int64(estimateRequestTokens(req))
	budget := retryBudgetForMeasuredRefusal(req, sizes.Observed, sizes.Limit, 100000)
	if budget <= 0 {
		t.Fatalf("no retry budget from %+v (estimate %d); the caller gets a dead 400", sizes, estimated)
	}
	if budget >= estimated {
		t.Fatalf("retry budget %d must be smaller than the estimate %d", budget, estimated)
	}
	report, trimmed := applyContextBudget(req, budget)
	if !trimmed {
		t.Fatalf("budget %d did not trim a %d-token request", budget, estimated)
	}
	if report.AfterTokens > budget {
		t.Fatalf("after %d still above budget %d", report.AfterTokens, budget)
	}
	t.Logf("estimate=%d budget=%d dropped=%d after=%d", estimated, budget, report.DroppedMessages, report.AfterTokens)
}
