package panel

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/modelmeta"
)

func TestModelContextOverridesRoundTrip(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore(filepath.Join(t.TempDir(), "model_context.json"))
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{ModelOverrides: store})

	body := `{"clients":{"zcode":{"glm-5.3-flash":{"context_length":1048576,"max_output_tokens":131072}}}}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/model_context", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = get(t, h, "/panel/api/model_context")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "1048576") || !strings.Contains(rec.Body.String(), "131072") {
		t.Fatalf("GET did not return saved values: %s", rec.Body.String())
	}
	got, ok := store.Get("zcode", "glm-5.3-flash")
	if !ok || got.ContextLength != 1048576 || got.MaxOutputTokens != 131072 {
		t.Fatalf("store = %+v ok=%v", got, ok)
	}
}
