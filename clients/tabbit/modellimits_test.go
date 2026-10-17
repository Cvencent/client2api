package tabbit

import (
	"context"
	"net/http"
	"testing"
)

// limitsFixture carries two ids for which the installed Tabbit client publishes
// no per-model output limit.
const limitsFixture = `{"object":"list","data":[
 {"id":"tabbit/GLM-5.1"},
 {"id":"tabbit/Default"}
]}`

// TestModelMaxOutputTokensReadsTheCachedCatalogue pins what the gateway gets
// when a caller omits max_tokens.
//
// The installed Tabbit client does not publish output caps, so the catalogue
// must report unknown both before and after the cache is populated rather than
// borrowing a number from public documentation.
func TestModelMaxOutputTokensStaysUnknownWithoutClientLimit(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/models" {
			return jsonResponse(200, limitsFixture), nil
		}
		return jsonResponse(404, `{}`), nil
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, hc)
	ctx := context.Background()

	if n, ok := c.ModelMaxOutputTokens(ctx, "GLM-5.1"); ok {
		t.Fatalf("ModelMaxOutputTokens on a cold cache = %d, true; want ok=false", n)
	}

	if _, err := c.Models(ctx); err != nil {
		t.Fatalf("Models: %v", err)
	}

	n, ok := c.ModelMaxOutputTokens(ctx, "GLM-5.1")
	if ok {
		t.Fatalf("GLM-5.1 = %d, true; the installed client publishes no output cap", n)
	}

	// An unsourced id must stay uncapped rather than borrow a number.
	if n, ok := c.ModelMaxOutputTokens(ctx, "Default"); ok {
		t.Fatalf("Default = %d, true; want ok=false", n)
	}
	if n, ok := c.ModelMaxOutputTokens(ctx, "no-such-model"); ok {
		t.Fatalf("an unknown model = %d, true; want ok=false", n)
	}
}
