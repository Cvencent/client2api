package kimi

import (
	"context"
	"testing"

	"client2api/internal/core"
)

// TestModelMaxOutputTokensReadsTheCachedCatalogue pins what the gateway gets
// when a caller omits max_tokens.
//
// The cache field is seeded directly: the fetch and merge are covered by
// models_test.go, and what this method has to get right is which list it reads
// -- the upstream cache when there is one, the baseline catalogue otherwise.
func TestModelMaxOutputTokensReadsTheCachedCatalogue(t *testing.T) {
	c, _ := newClient(t, nil)
	ctx := context.Background()

	// An id no source published has no budget, even though the baseline
	// catalogue answers while the upstream cache is cold.
	if n, ok := c.ModelMaxOutputTokens(ctx, "m-rich"); ok {
		t.Fatalf("ModelMaxOutputTokens = %d, true; want ok=false", n)
	}

	c.modelsMu.Lock()
	c.modelsList = []core.Model{
		{ID: "m-rich", Extra: map[string]any{"max_output_tokens": 64000}},
		{ID: "m-bare"},
	}
	c.modelsMu.Unlock()

	n, ok := c.ModelMaxOutputTokens(ctx, "m-rich")
	if !ok {
		t.Fatal("the cached catalogue carries a budget but the module declined to say so")
	}
	if n != 64000 {
		t.Fatalf("max output tokens = %d, want the cached 64000", n)
	}

	if n, ok := c.ModelMaxOutputTokens(ctx, "m-bare"); ok {
		t.Fatalf("an entry without a budget = %d, true; want ok=false", n)
	}
	if n, ok := c.ModelMaxOutputTokens(ctx, "no-such-model"); ok {
		t.Fatalf("an unknown model = %d, true; want ok=false", n)
	}
}
