package zcode

import (
	"context"
	"net/http"
	"testing"
)

// TestModelMaxOutputTokensReadsTheCachedCatalogue pins the contract the gateway
// relies on when a caller omits max_tokens: the module answers with the budget
// the vendor published for the model it is about to call, or says it cannot say.
//
// It drives the real path -- the catalogue is fetched and cached by Models() --
// because the interesting failure is not the lookup itself (core.OutputLimitFor
// has its own tests) but whether this method reads the same cache Models serves
// from.  A method wired to the wrong accessor answers "cannot say" forever,
// which looks exactly like a module that has no numbers at all.
func TestModelMaxOutputTokensReadsTheCachedCatalogue(t *testing.T) {
	srv, _ := modelServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, anthropicList)
	})
	c := newModelTestClient(t, apiKeyConfig(t, srv.URL+"/api/anthropic"))
	ctx := context.Background()

	// Before the catalogue has ever been read there is nothing to answer from,
	// and a metadata fetch must NOT happen here: this runs inside a chat turn.
	if n, ok := c.ModelMaxOutputTokens(ctx, "glm-4.6"); ok {
		t.Fatalf("ModelMaxOutputTokens on a cold cache = %d, true; want ok=false", n)
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
