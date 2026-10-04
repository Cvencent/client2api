package qwenwork

import (
	"context"
	"testing"

	"client2api/internal/core"
)

// TestModelMaxOutputTokensReadsTheCachedCatalogue pins what the gateway gets
// when a caller omits max_tokens.
//
// The cache field is seeded directly: the fetch is covered elsewhere, and what
// this method has to get right is that it reads the catalogue Models serves
// from, under the same lock, without a metadata round trip inside a chat turn.
func TestModelMaxOutputTokensReadsTheCachedCatalogue(t *testing.T) {
	raw, err := New(core.Deps{DataDir: t.TempDir(), Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := raw.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", raw)
	}
	ctx := context.Background()

	if n, ok := c.ModelMaxOutputTokens(ctx, "m-rich"); ok {
		t.Fatalf("ModelMaxOutputTokens on a cold cache = %d, true; want ok=false", n)
	}

	c.modelsMu.Lock()
	c.models = []core.Model{
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
