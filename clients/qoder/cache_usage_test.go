package qoder

import "testing"

func TestUsageCachePresence(t *testing.T) {
	cases := []struct {
		name  string
		in    map[string]any
		known bool
	}{
		{name: "cache field omitted", in: map[string]any{"prompt_tokens": 10}, known: false},
		{name: "details explicit zero", in: map[string]any{"prompt_tokens": 10, "prompt_tokens_details": map[string]any{"cached_tokens": 0}}, known: true},
		{name: "top-level explicit zero", in: map[string]any{"prompt_tokens": 10, "cached_tokens": 0}, known: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageFromAny(tc.in).CachedTokensKnown; got != tc.known {
				t.Fatalf("CachedTokensKnown = %v, want %v", got, tc.known)
			}
		})
	}
}
