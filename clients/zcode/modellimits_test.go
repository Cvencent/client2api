package zcode

import (
	"context"
	"net/http"
	"testing"
)

// TestModelMaxOutputTokensUsesBuiltinSpecsAndTheCachedCatalogue pins the
// contract the gateway relies on when a caller omits max_tokens: the module
// answers with the budget published for the model it is about to call, or
// says it cannot say.
//
// A known plan model must answer before the first metadata refresh too: a cold
// start must not truncate GLM-5.3-Flash at the module's fallback default just
// because /v1/models has not answered yet.  The cached vendor entry still wins
// when it carries a number.
func TestModelMaxOutputTokensUsesBuiltinSpecsAndTheCachedCatalogue(t *testing.T) {
	srv, _ := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, anthropicList)
	})
	c := newModelTestClient(t, apiKeyConfig(t, srv.URL+"/api/anthropic"))
	ctx := context.Background()

	// The builtin table answers even before the catalogue has ever been read,
	// and a metadata fetch must NOT happen here: this runs inside a chat turn.
	if n, ok := c.ModelMaxOutputTokens(ctx, "glm-4.6"); !ok || n != 131072 {
		t.Fatalf("ModelMaxOutputTokens on a cold cache = %d, %v; want 131072, true", n, ok)
	}
	if n, ok := c.ModelMaxOutputTokens(ctx, "GLM-5.3-Flash"); !ok || n != 131072 {
		t.Fatalf("case-insensitive builtin lookup = %d, %v; want 131072, true", n, ok)
	}

	if _, err := c.Models(ctx); err != nil {
		t.Fatalf("Models: %v", err)
	}

	n, ok := c.ModelMaxOutputTokens(ctx, "glm-4.6")
	if !ok {
		t.Fatal("glm-4.6 advertises max_output_tokens but the module declined to say so")
	}
	if n != 131072 {
		t.Fatalf("max output tokens = %d, want the advertised 131072", n)
	}

	// The vendor published no budget for the flash entry, so it must stay
	// uncapped rather than inherit its sibling's number.
	if n, ok := c.ModelMaxOutputTokens(ctx, "glm-4.6-flash"); ok {
		t.Fatalf("glm-4.6-flash = %d, true; want ok=false", n)
	}
	if n, ok := c.ModelMaxOutputTokens(ctx, "no-such-model"); ok {
		t.Fatalf("an unknown model = %d, true; want ok=false", n)
	}
}
