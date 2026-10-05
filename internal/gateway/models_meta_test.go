package gateway

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

// listModels fetches /v1/models and indexes the entries by id.
func listModels(t *testing.T, srv *http.Server) map[string]map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	byID := map[string]map[string]any{}
	for _, e := range out.Data {
		id, _ := e["id"].(string)
		byID[id] = e
	}
	return byID
}

// TestModelsListPublishesTheResolvedContextWindow is the fix for the caller that
// sized its context from a 4K default: a module that reports no window still
// gets the official preset on /v1/models, so Codex and friends can size
// themselves correctly.
func TestModelsListPublishesTheResolvedContextWindow(t *testing.T) {
	c := &testClient{name: "zcode", catalogue: []core.Model{{ID: "glm-5.3-flash"}}}
	srv := newLimitsServer(t, c)

	entry := listModels(t, srv)["zcode/glm-5.3-flash"]
	if entry == nil {
		t.Fatal("zcode/glm-5.3-flash missing from /v1/models")
	}
	if got, _ := entry["context_length"].(float64); int64(got) != 1048576 {
		t.Errorf("context_length = %v, want the official 1048576 preset", entry["context_length"])
	}
	if got, _ := entry["max_output_tokens"].(float64); int64(got) != 131072 {
		t.Errorf("max_output_tokens = %v, want the official 131072 preset", entry["max_output_tokens"])
	}
	// The same numbers are mirrored into extra for clients that look there.
	extra, _ := entry["extra"].(map[string]any)
	if extra == nil {
		t.Fatal("the resolved window was not mirrored into extra")
	}
	if got, _ := extra["context_length"].(float64); int64(got) != 1048576 {
		t.Errorf("extra.context_length = %v, want 1048576", extra["context_length"])
	}
}

// TestModelsListKeepsTheVendorWindowOverThePreset: a module that did report a
// window must not be overwritten by the table.
func TestModelsListKeepsTheVendorWindowOverThePreset(t *testing.T) {
	c := &testClient{name: "zcode", catalogue: []core.Model{
		{ID: "glm-5.3-flash", Extra: map[string]any{"context_length": 4096, "max_output_tokens": 2048}},
	}}
	srv := newLimitsServer(t, c)

	entry := listModels(t, srv)["zcode/glm-5.3-flash"]
	if got, _ := entry["context_length"].(float64); int64(got) != 4096 {
		t.Errorf("context_length = %v, want the vendor's 4096", entry["context_length"])
	}
	if got, _ := entry["max_output_tokens"].(float64); int64(got) != 2048 {
		t.Errorf("max_output_tokens = %v, want the vendor's 2048", entry["max_output_tokens"])
	}
}

// TestModelsListPrefersTheManualOverride pins the top of the precedence chain on
// the catalogue path too, not only on the request path.
func TestModelsListPrefersTheManualOverride(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("zcode", "glm-5.3-flash", 777, 888); err != nil {
		t.Fatal(err)
	}
	c := &testClient{name: "zcode", catalogue: []core.Model{
		{ID: "glm-5.3-flash", Extra: map[string]any{"context_length": 4096, "max_output_tokens": 2048}},
	}}
	reg := core.NewRegistry()
	reg.Add(c)
	srv := NewServer(Options{
		Registry:       reg,
		ModelOverrides: store,
		Version:        "test",
		Logger:         log.New(io.Discard, "", 0),
	})

	entry := listModels(t, srv)["zcode/glm-5.3-flash"]
	if got, _ := entry["context_length"].(float64); int64(got) != 777 {
		t.Errorf("context_length = %v, want the manual 777", entry["context_length"])
	}
	if got, _ := entry["max_output_tokens"].(float64); int64(got) != 888 {
		t.Errorf("max_output_tokens = %v, want the manual 888", entry["max_output_tokens"])
	}
}

// TestModelsListDoesNotMutateTheModuleCatalogue: resolving the window must copy
// the Extra map rather than write into the map the module handed out.
func TestModelsListDoesNotMutateTheModuleCatalogue(t *testing.T) {
	extra := map[string]any{"vendor": "x"}
	c := &testClient{name: "zcode", catalogue: []core.Model{{ID: "glm-5.3-flash", Extra: extra}}}
	srv := newLimitsServer(t, c)
	listModels(t, srv)
	if _, ok := extra["context_length"]; ok {
		t.Fatalf("the module's own Extra map was mutated: %v", extra)
	}
}
