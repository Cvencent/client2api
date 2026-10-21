package gateway

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"client2api/internal/core"
)

// A completed call is where the vendor's count and this gateway's estimate of
// the same bytes finally coexist.  This pins that the gateway actually feeds
// them into the calibration, instead of collecting the numbers and doing
// nothing with them.
func TestCompletedCallFeedsTheCalibration(t *testing.T) {
	c := &catalogueClient{name: "autoclaw", window: 1048576}
	usage := NewUsageStore(0)
	s := newGuardServer(t, c, "", func(o *Options) {
		o.Usage = usage
		o.EstimateCalibrationPath = filepath.Join(t.TempDir(), "estimate_calibration.json")
	})
	// newGuardServer does not wire the path itself (it builds the store before
	// callers can set the option in some tests), so build it explicitly.
	s.estimates = newEstimateCalibrationStore(s.opts.EstimateCalibrationPath, nil)

	if got := s.estimates.factor("autoclaw", "cn:space-bunny"); got != 1 {
		t.Fatalf("precondition: factor = %v, want 1 before any call", got)
	}

	// The vendor counts 800 where this gateway estimated 1000: a factor of 0.8.
	s.recordUsage(UsageRecord{
		Client:                "autoclaw",
		Model:                 "autoclaw/cn:space-bunny",
		PromptTokens:          800,
		CompletionTokens:      10,
		EstimatedPromptTokens: 1000,
	})

	got := s.estimates.factor("autoclaw", "cn:space-bunny")
	if math.Abs(got-0.8) > 1e-9 {
		t.Fatalf("factor = %v, want 0.8 after one measurement", got)
	}
}

// A failed row never reached the vendor's tokenizer, so its prompt_tokens is
// either absent or a refusal's own number -- not a measurement of our bytes.
func TestFailedCallDoesNotFeedTheCalibration(t *testing.T) {
	c := &catalogueClient{name: "autoclaw", window: 1048576}
	s := newGuardServer(t, c, "")
	s.recordUsage(UsageRecord{
		Client:                "autoclaw",
		Model:                 "autoclaw/cn:space-bunny",
		PromptTokens:          800,
		EstimatedPromptTokens: 1000,
		Failed:                true,
	})
	if got := s.estimates.factor("autoclaw", "cn:space-bunny"); got != 1 {
		t.Fatalf("a failed call must not calibrate, factor = %v", got)
	}
}

// The estimate recorded on the row must be this gateway's estimate of the
// request actually sent -- not the raw size of the caller's history, which the
// guard may have trimmed -- or every measurement would be biased.
func TestGuardRecordsTheEstimateOfTheRequestItSent(t *testing.T) {
	c := &catalogueClient{name: "autoclaw", window: 6000}
	s := newGuardServer(t, c, "")
	rec := &UsageRecord{}
	base := &core.ChatRequest{Messages: []core.Message{
		{Role: "user", Content: textOf(4000)},
		{Role: "assistant", Content: textOf(4000)},
		{Role: "user", Content: textOf(4000)},
		{Role: "assistant", Content: textOf(4000)},
		{Role: "user", Content: textOf(4000)},
	}}
	req, err := s.prepareCandidateAttempt(context.Background(), core.Candidate{Client: c, Model: "cn:space-bunny"}, "autoclaw/cn:space-bunny", base, rec, false)
	if err != nil {
		t.Fatalf("prepareCandidateAttempt: %v", err)
	}
	if !rec.Trimmed {
		t.Fatal("precondition: the request should have been trimmed to fit 6000")
	}
	if want := int64(estimateRequestTokens(req)); rec.EstimatedPromptTokens != want {
		t.Fatalf("recorded estimate = %d, want the estimate of the trimmed request %d",
			rec.EstimatedPromptTokens, want)
	}
	if rec.EstimatedPromptTokens >= int64(estimateRequestTokens(base)) {
		t.Fatalf("recorded estimate %d must be smaller than the untrimmed history %d",
			rec.EstimatedPromptTokens, estimateRequestTokens(base))
	}
}

func textOf(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
