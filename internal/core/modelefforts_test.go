package core

import (
	"context"
	"slices"
	"testing"
)

type effortsClient struct {
	plainClient
	efforts       []string
	defaultEffort string
	ok            bool
}

func (c *effortsClient) ModelEfforts(context.Context, string) ([]string, string, bool) {
	return append([]string(nil), c.efforts...), c.defaultEffort, c.ok
}

func TestModelEffortsForReadsCanonicalAndLegacyKeys(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]any
	}{
		{
			name: "canonical keys",
			extra: map[string]any{
				"supported_efforts": []string{"low", "high", "max"},
				"default_effort":    "max",
			},
		},
		{
			name: "workbuddy keys",
			extra: map[string]any{
				"reasoning_supported_efforts": []string{"low", "high", "max"},
				"reasoning_default_effort":    "max",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			efforts, def, ok := ModelEffortsFor([]Model{{ID: "m1", Extra: tc.extra}}, "m1")
			if !ok {
				t.Fatal("ModelEffortsFor reported no vocabulary")
			}
			if !slices.Equal(efforts, []string{"low", "high", "max"}) {
				t.Fatalf("efforts = %v, want [low high max]", efforts)
			}
			if def != "max" {
				t.Fatalf("default effort = %q, want max", def)
			}
		})
	}
}

func TestModelEffortsForDropsAnUnsupportedDefault(t *testing.T) {
	extra := map[string]any{
		"supported_efforts": []any{"low", "high"},
		"default_effort":    "max",
	}
	efforts, def, ok := ModelEffortsFor([]Model{{ID: "m1", Extra: extra}}, "m1")
	if !ok || !slices.Equal(efforts, []string{"low", "high"}) {
		t.Fatalf("efforts = %v, %v; want [low high], true", efforts, ok)
	}
	if def != "" {
		t.Fatalf("default effort = %q, want it dropped because max is not supported", def)
	}
}

func TestModelEffortsForScansPastAnEntryWithoutAVocabulary(t *testing.T) {
	rich := Model{ID: "m1", Extra: map[string]any{"supported_efforts": []string{"high", "max"}}}
	models := []Model{{ID: "m1"}, rich}
	efforts, _, ok := ModelEffortsFor(models, "m1")
	if !ok || !slices.Equal(efforts, []string{"high", "max"}) {
		t.Fatalf("ModelEffortsFor = %v, %v; want [high max], true", efforts, ok)
	}
}

func TestAsModelEffortsNarrowsOnlyAnImplementer(t *testing.T) {
	var c Client = &plainClient{name: "plain"}
	if _, ok := AsModelEfforts(c); ok {
		t.Fatal("a module without the method narrowed to ModelEffortProvider")
	}

	c = &effortsClient{
		plainClient:   plainClient{name: "efforts"},
		efforts:       []string{"high", "max"},
		defaultEffort: "high",
		ok:            true,
	}
	p, ok := AsModelEfforts(c)
	if !ok {
		t.Fatal("a module with the method did not narrow to ModelEffortProvider")
	}
	efforts, def, ok := p.ModelEfforts(context.Background(), "m1")
	if !ok || !slices.Equal(efforts, []string{"high", "max"}) || def != "high" {
		t.Fatalf("ModelEfforts = %v, %q, %v; want [high max], high, true", efforts, def, ok)
	}
}
