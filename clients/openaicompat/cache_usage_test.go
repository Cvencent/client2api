package openaicompat

import (
	"encoding/json"
	"testing"
)

func TestUsageCachePresence(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		known bool
	}{
		{name: "cache field omitted", raw: `{"prompt_tokens":10}`, known: false},
		{name: "explicit zero", raw: `{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":0}}`, known: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var wire oaiUsage
			if err := json.Unmarshal([]byte(tc.raw), &wire); err != nil {
				t.Fatal(err)
			}
			if got := wire.toCore().CachedTokensKnown; got != tc.known {
				t.Fatalf("CachedTokensKnown = %v, want %v", got, tc.known)
			}
		})
	}
}
