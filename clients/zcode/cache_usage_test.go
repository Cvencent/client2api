package zcode

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
		{name: "cache field omitted", raw: `{"input_tokens":10}`, known: false},
		{name: "explicit zero", raw: `{"input_tokens":10,"cache_read_input_tokens":0}`, known: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var wire anthropicUsage
			if err := json.Unmarshal([]byte(tc.raw), &wire); err != nil {
				t.Fatal(err)
			}
			if got := usageToCore(wire).CachedTokensKnown; got != tc.known {
				t.Fatalf("CachedTokensKnown = %v, want %v", got, tc.known)
			}
		})
	}
}
