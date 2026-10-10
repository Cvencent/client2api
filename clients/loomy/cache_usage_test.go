package loomy

import "testing"

func TestUsageCachePresence(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		known bool
	}{
		{name: "cache field omitted", raw: `{"usage":{"prompt_tokens":10}}`, known: false},
		{name: "explicit zero", raw: `{"usage":{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":0}}}`, known: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := &chatStream{}
			if err := stream.decode(tc.raw); err != nil {
				t.Fatal(err)
			}
			if stream.usage == nil {
				t.Fatal("usage missing")
			}
			if got := stream.usage.CachedTokensKnown; got != tc.known {
				t.Fatalf("CachedTokensKnown = %v, want %v", got, tc.known)
			}
		})
	}
}
