package gateway

import (
	"testing"

	"client2api/internal/core"
)

// TestChatNamesOneCataloguePassForAKnownModel pins the hot path: routing and
// context-budget resolution should share the catalogue result for a model the
// router already found.
func TestChatNamesOneCataloguePassForAKnownModel(t *testing.T) {
	c := &testClient{
		name: "alpha",
		catalogue: []core.Model{{
			ID: "m1",
			Extra: map[string]any{
				"context_length": 200000,
			},
		}},
	}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(4))
	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`

	if rec := chat(t, srv, body); rec.Code != 200 {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if c.modelsCalls != 1 {
		t.Fatalf("successful chat called Models %d times, want 1", c.modelsCalls)
	}
}

// TestMissingModelDoesNotWalkTheCatalogueTwice pins the pathological shape:
// when the router cannot resolve a model, the context guard must not repeat the
// same catalogue walk while building the 404.
func TestMissingModelDoesNotWalkTheCatalogueTwice(t *testing.T) {
	c := &testClient{name: "alpha", catalogue: []core.Model{{ID: "other"}}}
	srv := newTestServer(t, c, NewStats(), NewUsageStore(4))
	body := `{"model":"missing","messages":[{"role":"user","content":"hi"}]}`

	_ = chat(t, srv, body)
	if c.modelsCalls > 1 {
		t.Fatalf("missing-model chat called Models %d times, want one catalogue pass", c.modelsCalls)
	}
}
