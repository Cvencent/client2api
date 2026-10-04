package tabbit

import (
	"context"
	"net/http"
	"testing"
)

// limitsFixture carries one id the embedded metadata table can source (GLM-5.1)
// and one it cannot (Default), so the test can pin both halves of the contract.
const limitsFixture = `{"object":"list","data":[
 {"id":"tabbit/GLM-5.1"},
 {"id":"tabbit/Default"}
]}`

// TestModelMaxOutputTokensReadsTheCachedCatalogue pins what the gateway gets
// when a caller omits max_tokens.
//
// tabbit is the module whose number comes from internal/modelmeta rather than
// from the vendor's own payload, so this also pins that the merge actually
// reaches the cache the chat path reads.
func TestModelMaxOutputTokensReadsTheCachedCatalogue(t *testing.T) {
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
	if !ok {
		t.Fatal("GLM-5.1 is sourced by the metadata table but the module declined to say so")
	}
	if n != 131072 {
		t.Fatalf("max output tokens = %d, want the sourced 131072", n)
	}

	// An id no source covers must stay uncapped rather than borrow a number.
	if n, ok := c.ModelMaxOutputTokens(ctx, "Default"); ok {
		t.Fatalf("Default = %d, true; want ok=false", n)
	}
	if n, ok := c.ModelMaxOutputTokens(ctx, "no-such-model"); ok {
		t.Fatalf("an unknown model = %d, true; want ok=false", n)
	}
}
