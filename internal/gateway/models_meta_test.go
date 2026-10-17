package gateway

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	if got, _ := entry["context_length"].(float64); int64(got) != 1000000 {
		t.Errorf("context_length = %v, want the client-published 1000000 preset", entry["context_length"])
	}
	if got, _ := entry["max_output_tokens"].(float64); int64(got) != 128000 {
		t.Errorf("max_output_tokens = %v, want the client-published 128000 preset", entry["max_output_tokens"])
	}
	// The same numbers are mirrored into extra for clients that look there.
	extra, _ := entry["extra"].(map[string]any)
	if extra == nil {
		t.Fatal("the resolved window was not mirrored into extra")
	}
	if got, _ := extra["context_length"].(float64); int64(got) != 1000000 {
		t.Errorf("extra.context_length = %v, want 1000000", extra["context_length"])
	}
}

// TestModelsListPrefersTheClientPresetOverAStaleVendorWindow: a platform's
// built-in catalogue is the preset the panel exposes, so request guarding and
// /v1/models must use that same number instead of a stale Extra value.
func TestModelsListPrefersTheClientPresetOverAStaleVendorWindow(t *testing.T) {
	c := &testClient{name: "zcode", catalogue: []core.Model{
		{ID: "glm-5.3-flash", Extra: map[string]any{"context_length": 4096, "max_output_tokens": 2048}},
	}}
	srv := newLimitsServer(t, c)

	entry := listModels(t, srv)["zcode/glm-5.3-flash"]
	if got, _ := entry["context_length"].(float64); int64(got) != 1000000 {
		t.Errorf("context_length = %v, want the client preset 1000000", entry["context_length"])
	}
	if got, _ := entry["max_output_tokens"].(float64); int64(got) != 128000 {
		t.Errorf("max_output_tokens = %v, want the client preset 128000", entry["max_output_tokens"])
	}
	extra, _ := entry["extra"].(map[string]any)
	if got, _ := extra["context_length"].(float64); int64(got) != 1000000 {
		t.Errorf("extra.context_length = %v, want the resolved preset 1000000", extra["context_length"])
	}
	if got, _ := extra["max_output_tokens"].(float64); int64(got) != 128000 {
		t.Errorf("extra.max_output_tokens = %v, want the resolved preset 128000", extra["max_output_tokens"])
	}
}

// A platform without a client-published preset still trusts its live catalogue.
func TestModelsListUsesVendorWindowWhenNoClientPresetExists(t *testing.T) {
	c := &testClient{name: "unknown-platform", catalogue: []core.Model{
		{ID: "glm-5.3-flash", Extra: map[string]any{"context_length": 4096, "max_output_tokens": 2048}},
	}}
	srv := newLimitsServer(t, c)

	entry := listModels(t, srv)["unknown-platform/glm-5.3-flash"]
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

// A learned vendor refusal is evidence, not an operator decision. It must not
// replace the manual window in either the top-level fields or Extra.
func TestModelsListManualWindowSurvivesALearnedLimit(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("zcode", "glm-5.3-flash", 777, 888); err != nil {
		t.Fatal(err)
	}
	c := &testClient{name: "zcode", catalogue: []core.Model{{ID: "glm-5.3-flash"}}}
	reg := core.NewRegistry()
	reg.Add(c)
	learnedPath := filepath.Join(t.TempDir(), "learned-context-windows.json")
	if err := core.WriteJSONAtomic(learnedPath, learnedWindowDoc{
		Version: learnedWindowVersion,
		Windows: map[string]map[string]int64{"zcode": {"glm-5.3-flash": 333}},
	}); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(Options{
		Registry:           reg,
		ModelOverrides:     store,
		LearnedWindowsPath: learnedPath,
		Version:            "test",
		Logger:             log.New(io.Discard, "", 0),
	})

	entry := listModels(t, srv)["zcode/glm-5.3-flash"]
	if got, _ := entry["context_length"].(float64); int64(got) != 777 {
		t.Errorf("context_length = %v, want the manual 777, not the learned 333", entry["context_length"])
	}
	extra, _ := entry["extra"].(map[string]any)
	if got, _ := extra["context_length"].(float64); int64(got) != 777 {
		t.Errorf("extra.context_length = %v, want the manual 777, not the learned 333", extra["context_length"])
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

func TestModelsListAddsWindowSuffixForCodexPlus(t *testing.T) {
	// Use a platform with no embedded preset: this test is about Codex++'s id
	// suffix, not about which source wins when a preset exists.
	c := &testClient{name: "unknown-platform", catalogue: []core.Model{
		{ID: "glm-5.3-flash", Extra: map[string]any{"context_length": 1048576}},
	}}
	srv := newLimitsServer(t, c)

	plain := listModels(t, srv)["unknown-platform/glm-5.3-flash"]
	if plain == nil {
		t.Fatal("the ordinary catalogue lost the model")
	}
	if plain["id"] != "unknown-platform/glm-5.3-flash" {
		t.Fatalf("ordinary catalogue id = %v, want it unchanged", plain["id"])
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("User-Agent", "CodexPlusPlus/1.0.0")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Codex++ catalogue status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out modelList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode Codex++ catalogue: %v (%s)", err, rec.Body.String())
	}
	var found *modelEntry
	for i := range out.Data {
		if out.Data[i].ID == "unknown-platform/glm-5.3-flash[1048576]" {
			found = &out.Data[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("Codex++ catalogue has no suffixed model: %s", rec.Body.String())
	}
	if found.ContextLength != 1048576 {
		t.Fatalf("Codex++ entry window = %d, want 1048576", found.ContextLength)
	}
}

func TestResolveMaxOutputTokensUsesQualifiedRealm(t *testing.T) {
	c := &testClient{name: "workbuddy", catalogue: []core.Model{{ID: "global:glm-5.3"}}}
	srv := newLimitsServer(t, c)
	body := `{"model":"global:glm-5.3","messages":[{"role":"user","content":"hi"}]}`
	if rec := chat(t, srv, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if c.seen == nil || c.seen.MaxTokens == nil || *c.seen.MaxTokens != 48000 {
		t.Fatalf("global max_tokens = %+v, want 48000", c.seen)
	}
}
