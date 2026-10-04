package codearts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// models_test.go covers the catalogue: id normalisation, the benefit-model
// trap, the fallback list, and the per-model output budget the gateway reads
// through core.ModelLimitsProvider.

func TestNormalizeModelID(t *testing.T) {
	cases := []struct {
		in, want string
		why      string
	}{
		{"deepseek-v4-flash-0731", "deepseek-v4-flash", "a four-digit build stamp is stripped"},
		{"deepseek-v4-flash", "deepseek-v4-flash", "an id with no stamp is unchanged"},
		{"GLM-5.2", "GLM-5.2", "a dotted version is not a stamp"},
		{"glm-5.3-flash", "glm-5.3-flash", "unchanged"},
		{"gpt-4o-2024", "gpt-4o", "the rule is deliberately blunt: any trailing four digits go"},
		{"model-123", "model-123", "three digits are not a stamp"},
		{"model-12345", "model-12345", "five digits are not a stamp"},
		{"abcd-1234", "abcd", "exactly six characters, so the length guard passes"},
		{"abc-12", "abc-12", "too short to be a stamped id"},
		{"  deepseek-v4-pro-0731  ", "deepseek-v4-pro", "surrounding whitespace is trimmed first"},
		{"", "", "empty stays empty"},
	}
	for _, c := range cases {
		if got := normalizeModelID(c.in); got != c.want {
			t.Errorf("normalizeModelID(%q) = %q, want %q (%s)", c.in, got, c.want, c.why)
		}
	}
}

func TestIsVisionModel(t *testing.T) {
	for _, id := range []string{"glm-5-VL", "GLM-5-VL-0731", "deepseek-VL-flash"} {
		if !isVisionModel(id) {
			t.Errorf("isVisionModel(%q) = false, want true", id)
		}
	}
	for _, id := range []string{"glm-5.3-flash", "deepseek-v4-flash", "VLM-model", "glm-VLx"} {
		if isVisionModel(id) {
			t.Errorf("isVisionModel(%q) = true, want false", id)
		}
	}
}

func TestParseModelCatalogue(t *testing.T) {
	entries := []modelInfo{
		{ModelID: "deepseek-v4-flash", ModelName: "DeepSeek V4 Flash"},
		{ModelID: "deepseek-v4-flash-0731", ModelName: "deepseek-v4-flash-0731", ContextLength: 1048576},
		{ModelID: "glm-5-VL", ModelName: "GLM-5 Vision"},
		{ModelID: "", ModelName: "nameless"},
		{ModelID: "GLM-5.2", ModelName: "GLM-5.2", ContextLength: 202752},
	}
	models := parseModelCatalogue(entries)

	var ids []string
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	want := []string{"deepseek-v4-flash", "GLM-5.2"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v (the vision variant, the nameless entry and the duplicate must all be gone)", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
	if models[0].Extra["display_name"] != "DeepSeek V4 Flash" {
		t.Errorf("display_name = %v, want the model_name", models[0].Extra["display_name"])
	}
	if models[0].Extra["context_length"] != 1048576 {
		t.Errorf("context_length = %v, want the catalogue value", models[0].Extra["context_length"])
	}
}

// Every model must carry an output budget, because the gateway fills
// max_tokens from it when the caller did not ask for one, and a missing budget
// means a silently truncated answer.
func TestParseModelCatalogueCarriesAnOutputBudget(t *testing.T) {
	models := parseModelCatalogue([]modelInfo{{ModelID: "deepseek-v4-pro"}})
	if len(models) != 1 {
		t.Fatalf("got %d models, want 1", len(models))
	}
	if limit, ok := core.ModelOutputLimit(models[0]); !ok || limit != maxOutputTokens {
		t.Errorf("output limit = %d,%v, want %d,true", limit, ok, maxOutputTokens)
	}
	if models[0].Extra["context_length"] == nil {
		t.Error("the model carries no context_length")
	}
}

// An absurd vendor budget is clamped rather than passed through: a cap above
// what the module's own default can justify would be a vendor-side rejection.
func TestNewModelClampsAnAbsurdBudget(t *testing.T) {
	m := newModel("m", "", 0, maxOutputTokens*8, "")
	if got, _ := core.ModelOutputLimit(m); got != maxOutputTokens {
		t.Errorf("output limit = %d, want it clamped to %d", got, maxOutputTokens)
	}
}

func TestNewModelUsesTheMeasuredContextWindow(t *testing.T) {
	m := newModel("deepseek-v4-flash", "", 0, 0, "")
	if got := m.Extra["context_length"]; got != 1048576 {
		t.Errorf("context_length = %v, want the measured 1048576", got)
	}
}

// THE BENEFIT TRAP.  A dated id and its undated form are different backends
// with opposite benefit-ness, so only ids the normaliser leaves alone may be
// recorded as free.  Recording the rewritten form would mark the paid model as
// free and make every request for it carry `maas_type: benefit`.
func TestParseBenefitIDsSkipsRewrittenIDs(t *testing.T) {
	got := parseBenefitIDs([]string{
		"deepseek-v4-flash-0731",
		"deepseek-v4.1-flash",
		"glm-5.3-flash",
		"glm-5.3-flash",
		"  ",
		"",
	})
	want := []string{"deepseek-v4.1-flash", "glm-5.3-flash"}
	if len(got) != len(want) {
		t.Fatalf("benefit ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("benefit[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIsBenefitModelFallsBackWhenNothingIsKnown(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	if !c.isBenefitModel("glm-5.3-flash") {
		t.Error("glm-5.3-flash was not recognised as a free-quota model")
	}
	if !c.isBenefitModel("deepseek-v4.1-flash") {
		t.Error("deepseek-v4.1-flash was not recognised as a free-quota model")
	}
	// A model no list mentions is NOT free: guessing "free" would send the
	// benefit header for a paid model and get it refused.
	if c.isBenefitModel("deepseek-v4-pro") {
		t.Error("a model no list mentions was treated as free")
	}
	if c.isBenefitModel("") {
		t.Error("an empty model id was treated as free")
	}
}

func TestIsBenefitModelPrefersTheLiveList(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	c.benefit = []string{"deepseek-v4-pro"}
	if !c.isBenefitModel("deepseek-v4-pro") {
		t.Error("the live list was not honoured")
	}
	if c.isBenefitModel("glm-5.3-flash") {
		t.Error("the built-in fallback overrode the live list")
	}
}

func TestFallbackCataloguePrefersTheConfiguredList(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1","models":["only-this-one"]}`)
	models := c.fallbackCatalogue()
	if len(models) != 1 || models[0].ID != "only-this-one" {
		t.Fatalf("got %+v, want the configured list", models)
	}
}

func TestFallbackCatalogueIsTheBuiltInList(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	models := c.fallbackCatalogue()
	if len(models) != len(builtinModels) {
		t.Fatalf("got %d models, want the %d built-ins", len(models), len(builtinModels))
	}
	if models[0].ID != builtinModels[0] {
		t.Errorf("models[0] = %q, want %q", models[0].ID, builtinModels[0])
	}
}

// The catalogue response has been seen in three shapes; all three must parse.
func TestBuiltinModelsResponseShapes(t *testing.T) {
	cases := map[string]string{
		"builtinModels": `{"builtinModels":[{"model_id":"a"}]}`,
		"models":        `{"models":[{"model_id":"a"}]}`,
		"nested":        `{"result":{"builtinModels":[{"model_id":"a"}]}}`,
		"nestedModels":  `{"result":{"models":[{"model_id":"a"}]}}`,
	}
	for name, body := range cases {
		var resp builtinModelsResponse
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		entries := resp.entries()
		if len(entries) != 1 || entries[0].ModelID != "a" {
			t.Errorf("%s: entries = %+v, want one entry with model_id a", name, entries)
		}
	}
}

func TestGatewayConfigResponseShapes(t *testing.T) {
	var nested gatewayConfigResponse
	if err := json.Unmarshal([]byte(`{"result":{"models":[{"model_id":"x"}]}}`), &nested); err != nil {
		t.Fatalf("nested: %v", err)
	}
	if got := nested.ids(); len(got) != 1 || got[0] != "x" {
		t.Errorf("nested ids = %v, want [x]", got)
	}
	var flat gatewayConfigResponse
	if err := json.Unmarshal([]byte(`{"models":[{"model_id":"y"}]}`), &flat); err != nil {
		t.Fatalf("flat: %v", err)
	}
	if got := flat.ids(); len(got) != 1 || got[0] != "y" {
		t.Errorf("flat ids = %v, want [y]", got)
	}
}

// ---------------------------------------------------------------------------
// the catalogue over the wire
// ---------------------------------------------------------------------------

func TestFetchCatalogueAndModelLimits(t *testing.T) {
	var sawCatalogue, sawBenefit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case builtinModelsPath:
			sawCatalogue = true
			if r.Header.Get("Authorization") == "" {
				t.Error("the catalogue request was not signed")
			}
			// These two are the unsigned pair the reference appends.
			if r.Header.Get("Agent-Type") != "PromptCenter" || r.Header.Get("X-Language") != "zh-cn" {
				t.Errorf("catalogue headers = %q/%q, want PromptCenter/zh-cn",
					r.Header.Get("Agent-Type"), r.Header.Get("X-Language"))
			}
			if strings.Contains(r.Header.Get("Authorization"), "agent-type") {
				t.Error("the unsigned Agent-Type header was included in the signature")
			}
			_, _ = w.Write([]byte(`{"builtinModels":[{"model_id":"deepseek-v4-flash-0731","model_name":"Flash","context_length":1048576}]}`))
		case "/gateway/config":
			sawBenefit = true
			if r.Header.Get("Authorization") != "" {
				t.Error("the free-quota list is unauthenticated but carried an Authorization header")
			}
			_, _ = w.Write([]byte(`{"result":{"models":[{"model_id":"deepseek-v4.1-flash"}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, `{
		"base_url": `+jsonString(srv.URL)+`,
		"gateway_url": `+jsonString(srv.URL+"/gateway/config")+`,
		"access_key_id":"AKIDEXAMPLE","secret_access_key":"SKEXAMPLE","security_token":"TOKEXAMPLE"
	}`)

	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if !sawCatalogue || !sawBenefit {
		t.Fatalf("catalogue=%v benefit=%v, want both endpoints called", sawCatalogue, sawBenefit)
	}
	if len(models) != 1 || models[0].ID != "deepseek-v4-flash" {
		t.Fatalf("models = %+v, want the normalised deepseek-v4-flash", models)
	}
	// The budget is what the gateway reads when the caller set no max_tokens.
	if got, ok := c.ModelMaxOutputTokens(context.Background(), "deepseek-v4-flash"); !ok || got != maxOutputTokens {
		t.Errorf("ModelMaxOutputTokens = %d,%v, want %d,true", got, ok, maxOutputTokens)
	}
	if _, ok := c.ModelMaxOutputTokens(context.Background(), "not-a-model"); ok {
		t.Error("an unknown model was given an output budget instead of 'cannot say'")
	}
	// The benefit list came from the live endpoint, so the dated id is not
	// recorded and the undated one is.
	if !c.isBenefitModel("deepseek-v4.1-flash") {
		t.Error("the live free-quota list was not stored")
	}
	if c.isBenefitModel("deepseek-v4-flash-0731") {
		t.Error("a rewritten (dated) id was treated as free")
	}
	// And the catalogue survives a reload from the data dir.
	if !c.modelsFresh(time.Now()) {
		t.Error("the catalogue was not marked fresh after a successful refresh")
	}
}

// A catalogue refresh that fails must hand back the last known list alongside
// the error, so one flaky refresh never empties the model picker.
func TestRefreshModelsKeepsTheLastGoodListOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newTestClient(t, `{
		"base_url": `+jsonString(srv.URL)+`,
		"gateway_url": `+jsonString(srv.URL)+`,
		"access_key_id":"AKIDEXAMPLE","secret_access_key":"SKEXAMPLE","security_token":"TOKEXAMPLE"
	}`)

	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("a failing catalogue endpoint produced no error")
	}
	if !strings.Contains(err.Error(), "refresh models") {
		t.Errorf("err = %q, want it to name the operation", err.Error())
	}
	if len(models) == 0 {
		t.Fatal("the last known list was discarded on a failed refresh")
	}
}

func TestRefreshModelsRejectsAnEmptyCatalogue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == builtinModelsPath {
			_, _ = w.Write([]byte(`{"builtinModels":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"result":{"models":[]}}`))
	}))
	defer srv.Close()

	c := newTestClient(t, `{
		"base_url": `+jsonString(srv.URL)+`,
		"gateway_url": `+jsonString(srv.URL)+`,
		"access_key_id":"AKIDEXAMPLE","secret_access_key":"SKEXAMPLE","security_token":"TOKEXAMPLE"
	}`)
	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("an empty catalogue was accepted")
	}
	if !strings.Contains(err.Error(), "no usable models") {
		t.Errorf("err = %q, want the empty-catalogue message", err.Error())
	}
	if len(models) == 0 {
		t.Error("the fallback list was not returned alongside the error")
	}
}

// A free-quota-list failure must not cost the operator the catalogue.
func TestRefreshModelsSurvivesAFailingBenefitList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == builtinModelsPath {
			_, _ = w.Write([]byte(`{"builtinModels":[{"model_id":"glm-5.3-flash"}]}`))
			return
		}
		http.Error(w, "no", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := newTestClient(t, `{
		"base_url": `+jsonString(srv.URL)+`,
		"gateway_url": `+jsonString(srv.URL+"/gateway/config")+`,
		"access_key_id":"AKIDEXAMPLE","secret_access_key":"SKEXAMPLE","security_token":"TOKEXAMPLE"
	}`)
	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "glm-5.3-flash" {
		t.Fatalf("models = %+v, want the catalogue despite the benefit failure", models)
	}
	// With no live benefit list the built-in fallback still answers.
	if !c.isBenefitModel("glm-5.3-flash") {
		t.Error("the built-in free-quota fallback was not used")
	}
}

func TestRefreshModelsWithoutAnAccountIsNotConfigured(t *testing.T) {
	c := newTestClient(t, `{"base_url":"http://127.0.0.1:1"}`)
	_, err := c.RefreshModels(context.Background())
	if err != core.ErrNotConfigured {
		t.Fatalf("err = %v, want core.ErrNotConfigured", err)
	}
}

// The catalogue is persisted so a restart does not need the network.
func TestModelsCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	deps := core.Deps{DataDir: dir, Config: json.RawMessage(`{"base_url":"http://127.0.0.1:1"}`)}
	first, err := newClient(deps)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	c1 := first.(*Client)
	models := []core.Model{{ID: "cached-model", OwnedBy: "codearts", Extra: map[string]any{"max_output_tokens": 4096}}}
	c1.saveModelsCache(models, []string{"cached-model"}, time.Now())

	second, err := newClient(deps)
	if err != nil {
		t.Fatalf("second newClient: %v", err)
	}
	c2 := second.(*Client)
	got := c2.cachedModels()
	if len(got) != 1 || got[0].ID != "cached-model" {
		t.Fatalf("cachedModels = %+v, want the persisted catalogue", got)
	}
	if !c2.isBenefitModel("cached-model") {
		t.Error("the persisted free-quota list was not restored")
	}
	if limit, ok := c2.ModelMaxOutputTokens(context.Background(), "cached-model"); !ok || limit != 4096 {
		t.Errorf("ModelMaxOutputTokens = %d,%v, want 4096,true", limit, ok)
	}
}

// A corrupt cache file must not stop the module from starting.
func TestModelsCacheCorruptFileIsIgnored(t *testing.T) {
	dir := t.TempDir()
	deps := core.Deps{DataDir: dir, Config: json.RawMessage(`{"base_url":"http://127.0.0.1:1"}`)}
	first, _ := newClient(deps)
	c1 := first.(*Client)
	if c1.modelsPath == "" {
		t.Fatal("the module has no catalogue cache path")
	}
	if err := writeFile(c1.modelsPath, "{not json"); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	second, err := newClient(deps)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	if got := second.(*Client).cachedModels(); len(got) != len(builtinModels) {
		t.Fatalf("got %d models, want the built-in fallback", len(got))
	}
}

// ---------------------------------------------------------------------------
// helpers shared by the module's tests
// ---------------------------------------------------------------------------

// jsonString renders a Go string as a JSON string literal for a config body.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// writeFile writes a fixture file, for the tests that need a corrupt store.
func writeFile(path, content string) error {
	return core.WriteFileAtomic(path, []byte(content))
}

// newTestClient builds a Client the way the host does, from a config body.
func newTestClient(t *testing.T, cfg string) *Client {
	t.Helper()
	deps := core.Deps{DataDir: t.TempDir(), Config: json.RawMessage(cfg)}
	c, err := newClient(deps)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	client, ok := c.(*Client)
	if !ok {
		t.Fatalf("newClient returned %T, want *Client", c)
	}
	return client
}
