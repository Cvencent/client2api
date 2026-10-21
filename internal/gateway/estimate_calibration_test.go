package gateway

import (
	"context"
	"math"
	"testing"
)

// The estimator's error is not uniform across platforms.  Production shows
// autoclaw's tokenizer reading about 8% FEWER tokens than this gateway's
// heuristic for the same bytes, while workbuddy reads about 2.3x MORE because
// its module injects reasoning_content the gateway never sees.  A single global
// constant is therefore wrong by construction: it fixes one platform and breaks
// the other.  The factor has to be learned per (client, model) from the vendor's
// own reported prompt size, which is the only ground truth a gateway receives.
func TestEstimateCalibrationIsPerModel(t *testing.T) {
	store := newEstimateCalibrationStore("", nil)

	// A model nobody has measured must not be adjusted at all.
	if got := store.factor("autoclaw", "zai_glm-5.3-flash"); got != 1 {
		t.Fatalf("unlearned factor = %v, want exactly 1", got)
	}

	// autoclaw, from the production ledger: the vendor counted 861713 tokens
	// where this gateway estimated 931735 for the request it actually sent.
	store.observe("autoclaw", "zai_glm-5.3-flash", 861713, 931735)
	// workbuddy, from the measured-refusal log: the vendor counted 1477004
	// where the gateway estimated 628884.
	store.observe("workbuddy", "cn:space-bunny", 1477004, 628884)

	ac := store.factor("autoclaw", "zai_glm-5.3-flash")
	wb := store.factor("workbuddy", "cn:space-bunny")
	if math.Abs(ac-0.9248) > 0.01 {
		t.Fatalf("autoclaw factor = %v, want about 0.925", ac)
	}
	if math.Abs(wb-2.3486) > 0.05 {
		t.Fatalf("workbuddy factor = %v, want about 2.35", wb)
	}
	// The whole point: the two directions must not be collapsed into one number.
	if ac == wb {
		t.Fatal("the factor must be per model, not global")
	}
	// A sibling model that was never observed keeps the identity factor.
	if got := store.factor("workbuddy", "cn:glm-5.3"); got != 1 {
		t.Fatalf("unobserved sibling factor = %v, want 1", got)
	}
}

// One observation is noisy and an absurd one is not a tokenizer at all, so the
// stored factor is smoothed towards each new measurement and clamped to a range
// a real tokenizer could plausibly be off by.
func TestEstimateCalibrationSmoothsAndClamps(t *testing.T) {
	store := newEstimateCalibrationStore("", nil)
	store.observe("p", "m", 1000, 1000) // factor 1.0
	store.observe("p", "m", 800, 1000)  // a very different 0.8

	got := store.factor("p", "m")
	if got >= 1.0 || got <= 0.8 {
		t.Fatalf("smoothed factor = %v, want strictly between 0.8 and 1.0", got)
	}

	// A measurement so far off it cannot be a tokenizer.
	store.observe("p", "wild", 1, 1000000)
	if got := store.factor("p", "wild"); got < estimateCalibrationFloor {
		t.Fatalf("factor = %v, want clamped up to at least %v", got, estimateCalibrationFloor)
	}

	// Zero inputs carry no information and must be ignored rather than
	// producing a factor of zero, which would disable the guard for the model.
	before := store.factor("p", "m")
	store.observe("p", "m", 0, 1000)
	store.observe("p", "m", 1000, 0)
	store.observe("p", "m", 1000, -5)
	if after := store.factor("p", "m"); after != before {
		t.Fatalf("a zero observation changed the factor: %v -> %v", before, after)
	}
}

// The factor is applied where it matters: the budget the guard trims against.
// A model whose vendor reads fewer tokens than we estimate can afford a LARGER
// estimate-space budget, because each of our estimate units is worth less.
func TestPromptBudgetAppliesTheLearnedCalibration(t *testing.T) {
	c := &catalogueClient{name: "autoclaw", window: 1048576}
	s := newGuardServer(t, c, "")

	base := s.promptBudget(context.Background(), c, "cn:space-bunny", 0, 0)

	// The vendor counts half of what this gateway estimates: our estimate units
	// are worth half a vendor token each, so twice as many of them fit.
	s.estimates.observe("autoclaw", "cn:space-bunny", 1, 2)
	adjusted := s.promptBudget(context.Background(), c, "cn:space-bunny", 0, 0)

	if base <= 0 || adjusted <= 0 {
		t.Fatalf("budgets must stay positive: base=%d adjusted=%d", base, adjusted)
	}
	if adjusted <= base {
		t.Fatalf("a factor below 1 must raise the estimate-space budget: %d -> %d", base, adjusted)
	}
	// And the other direction must shrink it, not raise it.
	s.estimates.observe("autoclaw", "cn:space-bunny", 2, 1)
	shrunk := s.promptBudget(context.Background(), c, "cn:space-bunny", 0, 0)
	if shrunk >= base {
		t.Fatalf("a factor above 1 must lower the estimate-space budget: %d -> %d", base, shrunk)
	}
}
