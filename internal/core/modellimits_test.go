package core

import (
	"context"
	"encoding/json"
	"testing"
)

// limitsClient is a plainClient that opted into ModelLimitsProvider, so the
// narrowing test has both halves: an implementer and a module that stayed out.
type limitsClient struct {
	plainClient
	limit int
	ok    bool
}

func (c *limitsClient) ModelMaxOutputTokens(context.Context, string) (int, bool) {
	return c.limit, c.ok
}

// TestModelOutputLimitReadsEveryNumericType pins why this helper exists at all:
// the modules already publish the advertised budget in the same place, but as
// whatever Go type their own decode produced.  A type switch that missed one of
// these would make exactly one module silently stop filling max_tokens -- the
// kind of failure that only shows up as a truncated answer months later.
func TestModelOutputLimitReadsEveryNumericType(t *testing.T) {
	const want = 8192
	cases := []struct {
		name string
		v    any
	}{
		{"an int from a hand-built table", int(want)},
		{"an int32", int32(want)},
		{"an int64 from a struct-decoded vendor list", int64(want)},
		{"a float32", float32(want)},
		{"a float64 from a map[string]any round trip", float64(want)},
		{"a json.Number", json.Number("8192")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ModelOutputLimit(Model{ID: "m", Extra: map[string]any{"max_output_tokens": tc.v}})
			if !ok {
				t.Fatalf("ModelOutputLimit(%T) reported no budget, want %d", tc.v, want)
			}
			if got != want {
				t.Fatalf("ModelOutputLimit(%T) = %d, want %d", tc.v, got, want)
			}
		})
	}
}

// TestModelOutputLimitRefusesWhatIsNotABudget is the other half: every shape
// that must answer "this catalogue does not say".
//
// Zero and a negative number are the load-bearing ones.  A vendor that
// publishes 0 is publishing "unknown", and forwarding that as max_tokens would
// turn a missing default into a vendor-side rejection -- strictly worse than
// sending no cap at all.
func TestModelOutputLimitRefusesWhatIsNotABudget(t *testing.T) {
	cases := []struct {
		name string
		m    Model
	}{
		{"no Extra at all", Model{ID: "m"}},
		{"an Extra map without the key", Model{ID: "m", Extra: map[string]any{"other": 1}}},
		{"the key explicitly nil", Model{ID: "m", Extra: map[string]any{"max_output_tokens": nil}}},
		{"a string", Model{ID: "m", Extra: map[string]any{"max_output_tokens": "8192"}}},
		{"a bool", Model{ID: "m", Extra: map[string]any{"max_output_tokens": true}}},
		{"zero", Model{ID: "m", Extra: map[string]any{"max_output_tokens": 0}}},
		{"a negative number", Model{ID: "m", Extra: map[string]any{"max_output_tokens": -1}}},
		{"an absurd number that cannot survive the int conversion", Model{ID: "m", Extra: map[string]any{"max_output_tokens": int64(1) << 40}}},
		{"a json.Number that is not an integer", Model{ID: "m", Extra: map[string]any{"max_output_tokens": json.Number("8192.5")}}},
		{"a json.Number that is not a number", Model{ID: "m", Extra: map[string]any{"max_output_tokens": json.Number("lots")}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := ModelOutputLimit(tc.m); ok {
				t.Fatalf("ModelOutputLimit = %d, true; want ok=false", got)
			}
		})
	}
}

// TestOutputLimitForMatchesTheResolvedIDExactly pins the matching rule the
// gateway depends on: it passes the module's own upstream id, and every module
// keys its catalogue on that same string.
func TestOutputLimitForMatchesTheResolvedIDExactly(t *testing.T) {
	models := []Model{{ID: "GLM-5.3", Extra: map[string]any{"max_output_tokens": int64(128000)}}}

	got, ok := OutputLimitFor(models, "GLM-5.3")
	if !ok || got != 128000 {
		t.Fatalf("OutputLimitFor(GLm-5.3) = %d, %v; want 128000, true", got, ok)
	}
	// Surrounding space is the caller's formatting, not a different model.
	if got, ok := OutputLimitFor(models, "  GLM-5.3  "); !ok || got != 128000 {
		t.Fatalf("OutputLimitFor with padding = %d, %v; want 128000, true", got, ok)
	}
	// Case is NOT folded: two vendors can publish ids that differ only in case,
	// and answering for the wrong one is worse than answering nothing.
	if got, ok := OutputLimitFor(models, "glm-5.3"); ok {
		t.Fatalf("OutputLimitFor(glm-5.3) = %d, true; want a miss", got)
	}
}

func TestOutputLimitForRefusesAnEmptyLookup(t *testing.T) {
	models := []Model{{ID: "", Extra: map[string]any{"max_output_tokens": int64(128000)}}}
	if got, ok := OutputLimitFor(models, "   "); ok {
		t.Fatalf("OutputLimitFor(\"\") = %d, true; want a miss", got)
	}
	if got, ok := OutputLimitFor(nil, "m1"); ok {
		t.Fatalf("OutputLimitFor over a nil catalogue = %d, true; want a miss", got)
	}
}

// TestOutputLimitForScansPastAnEntryWithoutABudget pins the search order: a
// catalogue that holds the same id twice -- which happens when one entry is the
// stripped id and another is namespaced -- must not answer "no budget" just
// because the first spelling carries none.
func TestOutputLimitForScansPastAnEntryWithoutABudget(t *testing.T) {
	rich := Model{ID: "m1", Extra: map[string]any{"max_output_tokens": int64(64000)}}
	bare := Model{ID: "m1"}

	for _, tc := range []struct {
		name   string
		models []Model
	}{
		{"the bare entry first", []Model{bare, rich}},
		{"the rich entry first", []Model{rich, bare}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := OutputLimitFor(tc.models, "m1")
			if !ok || got != 64000 {
				t.Fatalf("OutputLimitFor = %d, %v; want 64000, true", got, ok)
			}
		})
	}
}

// TestAsModelLimitsNarrowsOnlyAnImplementer is the opt-in contract: a module
// that stayed out of the interface is indistinguishable from one that answered
// "cannot say", which is what keeps this change additive.
func TestAsModelLimitsNarrowsOnlyAnImplementer(t *testing.T) {
	var c Client = &plainClient{name: "plain"}
	if _, ok := AsModelLimits(c); ok {
		t.Fatal("a module without the method narrowed to ModelLimitsProvider")
	}

	c = &limitsClient{plainClient: plainClient{name: "lim"}, limit: 4096, ok: true}
	ml, ok := AsModelLimits(c)
	if !ok {
		t.Fatal("a module with the method did not narrow to ModelLimitsProvider")
	}
	if n, ok := ml.ModelMaxOutputTokens(context.Background(), "m1"); !ok || n != 4096 {
		t.Fatalf("ModelMaxOutputTokens = %d, %v; want 4096, true", n, ok)
	}
}
