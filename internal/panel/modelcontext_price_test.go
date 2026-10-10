package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/modelmeta"
)

func TestModelContextPriceRoundTripAndPartialUpdates(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{ModelOverrides: store})

	post := func(body string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/panel/api/model_context", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}

	post(`{"clients":{"zcode":{"glm-5.3":{"has_price":true,"input_per_million":1.5,"output_per_million":6.5,"has_cache_read":true,"cache_read_per_million":0.3}}}}`)
	got, ok := store.Get("zcode", "glm-5.3")
	if !ok || !got.HasPrice || got.InputPerMillion != 1.5 || got.OutputPerMillion != 6.5 || !got.HasCacheRead || got.CacheReadPerMillion != 0.3 {
		t.Fatalf("price after first POST = %+v ok=%v", got, ok)
	}

	// Saving context/output must not wipe the independently edited price.
	post(`{"clients":{"zcode":{"glm-5.3":{"context_length":1048576,"max_output_tokens":131072}}}}`)
	got, ok = store.Get("zcode", "glm-5.3")
	if !ok || got.ContextLength != 1048576 || got.MaxOutputTokens != 131072 || !got.HasPrice || got.InputPerMillion != 1.5 {
		t.Fatalf("price was not preserved: %+v ok=%v", got, ok)
	}

	// Clearing only the price must leave context/output intact.
	post(`{"clients":{"zcode":{"glm-5.3":{"has_price":false}}}}`)
	got, ok = store.Get("zcode", "glm-5.3")
	if !ok || got.HasPrice || got.ContextLength != 1048576 || got.MaxOutputTokens != 131072 {
		t.Fatalf("price clear damaged context: %+v ok=%v", got, ok)
	}
}
