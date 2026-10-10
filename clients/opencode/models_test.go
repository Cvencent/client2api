package opencode

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// The static catalogue.
// ---------------------------------------------------------------------------

// The catalogue is what a fresh install — and an outage — answers with, so it
// must line up with the live list: one entry per id, no duplicates, no blanks.
func TestStaticCatalogueIsWellFormed(t *testing.T) {
	if len(staticCatalogue) != 84 {
		t.Fatalf("staticCatalogue has %d entries, want the 84 ids GET /models served on 2026-10-01",
			len(staticCatalogue))
	}
	seen := make(map[string]bool, len(staticCatalogue))
	for _, m := range staticCatalogue {
		if strings.TrimSpace(m.id) == "" {
			t.Fatal("staticCatalogue carries a blank id")
		}
		if seen[m.id] {
			t.Fatalf("staticCatalogue lists %q twice", m.id)
		}
		seen[m.id] = true
		if strings.TrimSpace(m.name) == "" {
			t.Fatalf("staticCatalogue entry %q has no display name", m.id)
		}
		if m.context < 0 || m.output < 0 {
			t.Fatalf("staticCatalogue entry %q has a negative limit", m.id)
		}
		if m.context > 0 && m.output > m.context {
			t.Fatalf("staticCatalogue entry %q has output %d greater than its context %d",
				m.id, m.output, m.context)
		}
	}
	if len(staticIndex) != len(staticCatalogue) {
		t.Fatalf("staticIndex has %d keys, want %d", len(staticIndex), len(staticCatalogue))
	}
}

// The vendor publishes context/output limits for only 11 of the 84 live ids
// (the models.dev registry entry embedded in the opencode binary).  Every other
// entry must answer "unknown" rather than carry an invented number.
func TestStaticCatalogueCarriesOnlyPublishedLimits(t *testing.T) {
	withOutput := make(map[string]int, len(staticCatalogue))
	for _, m := range staticCatalogue {
		if m.output > 0 {
			withOutput[m.id] = m.output
		}
	}
	want := map[string]int{
		"claude-opus-4-5":        64000,
		"claude-sonnet-4":        64000,
		"gemini-3.5-flash":       65536,
		"gemini-3-flash":         65536,
		"gpt-5":                  128000,
		"deepseek-v4-flash":      384000,
		"glm-5.1":                131072,
		"minimax-m2.5":           131072,
		"deepseek-v4-flash-free": 128000,
		"mimo-v2.5-free":         32000,
		"nemotron-3-ultra-free":  128000,
	}
	if len(withOutput) != len(want) {
		t.Fatalf("%d ids carry an output limit, want exactly %d: %v",
			len(withOutput), len(want), sortedKeys(withOutput))
	}
	for id, n := range want {
		if withOutput[id] != n {
			t.Fatalf("output limit for %q = %d, want %d", id, withOutput[id], n)
		}
	}
}

// Only the ids the docs' "Deprecated models" table lists AND that the live list
// still serves carry the marker: 9 of 84.
func TestStaticCatalogueMarksOnlyLiveDeprecatedIds(t *testing.T) {
	got := make(map[string]bool, len(staticCatalogue))
	for _, m := range staticCatalogue {
		if m.deprecated {
			got[m.id] = true
		}
	}
	want := []string{
		"claude-sonnet-4",
		"gpt-5.2-codex",
		"gpt-5.1-codex-max",
		"gpt-5.1-codex",
		"gpt-5.1-codex-mini",
		"gpt-5-codex",
		"glm-5",
		"minimax-m2.5",
		"kimi-k2.5",
	}
	if len(got) != len(want) {
		t.Fatalf("%d ids are marked deprecated, want %d: %v", len(got), len(want), sortedKeys(got))
	}
	for _, id := range want {
		if !got[id] {
			t.Fatalf("%q is not marked deprecated", id)
		}
	}
}

// A sample of the names the docs publish.  The two ids the docs list but the
// live endpoint does not serve must be absent: offering them would advertise a
// model the gateway rejects.
func TestDisplayNamesComeFromTheVendorDocs(t *testing.T) {
	cases := map[string]string{
		"gpt-5.1":                         "GPT 5.1",
		"gpt-5.3-codex-spark":             "GPT 5.3 Codex Spark",
		"claude-opus-4-5":                 "Claude Opus 4.5",
		"claude-fable-5-1":                "Claude Fable 5.1",
		"gemini-3.5-flash-lite":           "Gemini 3.5 Flash Lite",
		"deepseek-v4.1-flash":             "DeepSeek V4.1 Flash",
		"mimo-v2.6-flash-free":            "MiMo-V2.6-Flash Free",
		"longcat-2.5-preview-free":        "LongCat 2.5 Preview Free",
		"nemotron-3.5-lightning-free":     "Nemotron 3.5 Lightning Free",
		"muse-spark-1.3-contributor-free": "Muse Spark 1.3 Contributor Free",
		"qwen3.8-max":                     "Qwen3.8 Max",
		"big-pickle":                      "Big Pickle",
	}
	for id, want := range cases {
		if got := displayNameFor(id); got != want {
			t.Fatalf("displayNameFor(%q) = %q, want %q", id, got, want)
		}
	}
	for _, id := range []string{"qwen3.7-max", "qwen3.7-plus"} {
		if _, ok := staticIndex[id]; ok {
			t.Fatalf("staticCatalogue offers %q, which the live list does not serve", id)
		}
	}
}

// An id no source names still gets a usable label — clearly marked as derived so
// an operator can tell it apart from a published one.
func TestDerivedDisplayNamesAreMarked(t *testing.T) {
	got := deriveDisplayName("muse-spark-9.9-turbo")
	if got != "Muse Spark 9.9 Turbo"+derivedNameSuffix {
		t.Fatalf("deriveDisplayName = %q, want %q", got, "Muse Spark 9.9 Turbo"+derivedNameSuffix)
	}
	if !strings.HasSuffix(got, derivedNameSuffix) {
		t.Fatalf("deriveDisplayName = %q, want the %q marker", got, derivedNameSuffix)
	}
	if got := deriveDisplayName(""); got != "" {
		t.Fatalf("deriveDisplayName(\"\") = %q, want empty", got)
	}
	// A published name must never be marked derived.
	if got := displayNameFor("gpt-5.1"); strings.Contains(got, derivedNameSuffix) {
		t.Fatalf("displayNameFor(known id) = %q, want the published name", got)
	}
	// Every catalogue id must have a label.
	for _, m := range staticCatalogue {
		if strings.TrimSpace(displayNameFor(m.id)) == "" {
			t.Fatalf("displayNameFor(%q) is empty", m.id)
		}
	}
}

func TestNewModelCarriesKnownMetadata(t *testing.T) {
	m := newModel("claude-opus-4-5", clientName)
	if m.ID != "claude-opus-4-5" || m.OwnedBy != clientName {
		t.Fatalf("newModel = %+v", m)
	}
	if got := m.Extra["display_name"]; got != "Claude Opus 4.5" {
		t.Fatalf("display_name = %v", got)
	}
	if got := m.Extra["context_length"]; got != int64(200000) {
		t.Fatalf("context_length = %#v, want int64(200000)", got)
	}
	if got := m.Extra["max_output_tokens"]; got != int64(64000) {
		t.Fatalf("max_output_tokens = %#v, want int64(64000)", got)
	}
	if _, ok := m.Extra["deprecated"]; ok {
		t.Fatal("a current model carries the deprecated marker")
	}

	dep := newModel("kimi-k2.5", clientName)
	if dep.Extra["deprecated"] != true {
		t.Fatalf("deprecated = %#v, want true", dep.Extra["deprecated"])
	}
	if _, ok := dep.Extra["max_output_tokens"]; ok {
		t.Fatal("an id with no published limit carries max_output_tokens")
	}

	// An id the table does not know is still served, with a derived name and no
	// invented limits.
	unk := newModel("brand-new-model-9", clientName)
	if name, _ := unk.Extra["display_name"].(string); !strings.HasSuffix(name, derivedNameSuffix) {
		t.Fatalf("display_name = %v, want a derived name", unk.Extra["display_name"])
	}
	if _, ok := unk.Extra["context_length"]; ok {
		t.Fatal("an unknown id carries a context_length")
	}
	if _, ok := unk.Extra["max_output_tokens"]; ok {
		t.Fatal("an unknown id carries a max_output_tokens")
	}
}

func TestContainsModel(t *testing.T) {
	models := []core.Model{{ID: "a"}, {ID: "b"}}
	if !containsModel(models, "b") {
		t.Fatal("containsModel missed an id that is present")
	}
	if containsModel(models, "c") || containsModel(nil, "a") {
		t.Fatal("containsModel reported an id that is absent")
	}
}

// ---------------------------------------------------------------------------
// The live list.
// ---------------------------------------------------------------------------

func TestParseModelListAcceptsBothSpellings(t *testing.T) {
	raw := []byte(`{"object":"list","data":[
		{"id":"gpt-5.1","object":"model","owned_by":"opencode"},
		{"modelId":"claude-opus-4-5","object":"model"},
		{"id":"   "},
		{"id":"gpt-5.1"}
	]}`)
	ids, err := parseModelList(raw)
	if err != nil {
		t.Fatalf("parseModelList = %v", err)
	}
	want := []string{"gpt-5.1", "claude-opus-4-5", "gpt-5.1"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
}

func TestParseModelListRejectsAnEmptyOrBrokenList(t *testing.T) {
	_, err := parseModelList([]byte(`{"object":"list","data":[]}`))
	if err == nil {
		t.Fatal("parseModelList accepted an empty list")
	}
	if !strings.Contains(err.Error(), "carried no ids") {
		t.Fatalf("error = %v, want it to name the empty list", err)
	}
	if _, err := parseModelList([]byte(`not json`)); err == nil {
		t.Fatal("parseModelList accepted a non-JSON body")
	}
}

// Membership comes from the live list: an id the vendor retired must disappear
// from the picker, and an id the static table has never seen must still be
// served.
func TestMergeCatalogueTakesMembershipFromLive(t *testing.T) {
	got := mergeCatalogue([]string{"gpt-5.1", "brand-new-model-9", "gpt-5.1", "  "}, Config{})
	ids := make([]string, 0, len(got))
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "gpt-5.1,brand-new-model-9" {
		t.Fatalf("mergeCatalogue = %v, want the live ids deduped and in order", ids)
	}
	if got[0].Extra["display_name"] != "GPT 5.1" {
		t.Fatalf("display_name = %v, want the published name", got[0].Extra["display_name"])
	}
	if name, _ := got[1].Extra["display_name"].(string); !strings.HasSuffix(name, derivedNameSuffix) {
		t.Fatalf("display_name = %v, want a derived name for an unknown id", got[1].Extra["display_name"])
	}
	if containsModel(got, "gpt-5.4") {
		t.Fatal("mergeCatalogue served gpt-5.4, which the live list did not return")
	}
}

func TestFallbackModelsIncludesConfiguredExtras(t *testing.T) {
	cfg := Config{ExtraModels: []string{"my-private-mirror-model", "gpt-5.1"}}.normalize()
	models := fallbackModels(cfg)
	if len(models) != len(staticCatalogue)+1 {
		t.Fatalf("fallbackModels = %d models, want %d", len(models), len(staticCatalogue)+1)
	}
	if models[0].ID != staticCatalogue[0].id {
		t.Fatalf("fallbackModels[0] = %q, want the catalogue order preserved", models[0].ID)
	}
	if last := models[len(models)-1]; last.ID != "my-private-mirror-model" {
		t.Fatalf("last model = %q, want the configured extra", last.ID)
	}
	n := 0
	for _, m := range models {
		if m.ID == "gpt-5.1" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("gpt-5.1 appears %d times, want once", n)
	}
}

func TestModelIDsIsSorted(t *testing.T) {
	c := newTestClient(t, Config{})
	ids := c.modelIDs()
	if len(ids) != len(staticCatalogue) {
		t.Fatalf("modelIDs = %d ids, want %d", len(ids), len(staticCatalogue))
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("modelIDs is not sorted: %v", ids[:5])
	}
}

func TestCachedModelsHonoursTheTTL(t *testing.T) {
	c := newTestClient(t, Config{})
	if _, ok := c.cachedModels(); ok {
		t.Fatal("a cold cache reported a hit")
	}
	c.storeModels(fallbackModels(c.cfg))
	if _, ok := c.cachedModels(); !ok {
		t.Fatal("a freshly stored list is not a cache hit")
	}

	// Age the cache past the TTL: cachedModels must miss, lastModels must not.
	c.modelsAt = testNow.Add(-2 * c.cfg.modelsTTL())
	if _, ok := c.cachedModels(); ok {
		t.Fatal("an expired list is still a cache hit")
	}
	if _, ok := c.lastModels(); !ok {
		t.Fatal("lastModels must serve a list at any age so an outage cannot empty the picker")
	}
}

// ---------------------------------------------------------------------------
// Fetching.
// ---------------------------------------------------------------------------

// GET /models is public, so even an unconfigured module gets the API catalogue.
func TestModelsWithoutACredentialFetchesThePublicCatalogue(t *testing.T) {
	called := false
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		called = true
		return jsonResponse(200, `{"object":"list","data":[{"id":"gpt-5.1"}]}`), nil
	})

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models = %v, want nil", err)
	}
	if !called {
		t.Fatal("Models did not call the public catalogue endpoint")
	}
	if len(models) != 1 || models[0].ID != "gpt-5.1" {
		t.Fatalf("Models = %v, want the API catalogue", modelIDsOf(models))
	}
	if _, ok := c.cachedModels(); !ok {
		t.Fatal("the public catalogue was not cached")
	}

	called = false
	models, err = c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels = %v, want nil", err)
	}
	if !called {
		t.Fatal("RefreshModels did not call the public catalogue endpoint")
	}
	if len(models) != 1 || models[0].ID != "gpt-5.1" {
		t.Fatalf("RefreshModels = %v, want the API catalogue", modelIDsOf(models))
	}
}

func TestModelsFetchesAndCachesWithACredential(t *testing.T) {
	hits := 0
	c := newFakeClient(t, Config{}, func(r *http.Request) (*http.Response, error) {
		hits++
		if r.URL.Path != "/zen/v1/models" {
			t.Fatalf("path = %q, want /zen/v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want none: GET /models is public", got)
		}
		return jsonResponse(200, `{"object":"list","data":[{"id":"gpt-5.1"},{"id":"claude-opus-4-5"}]}`), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models = %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("Models = %d models, want the 2 the server returned", len(models))
	}
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatalf("second Models = %v", err)
	}
	if hits != 1 {
		t.Fatalf("the server was hit %d times, want 1: the TTL cache must serve the second call", hits)
	}
	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels = %v", err)
	}
	if hits != 2 {
		t.Fatalf("RefreshModels did not bypass the TTL cache: hits = %d", hits)
	}
}

// A failed refresh must not empty the model picker: the last good list comes
// back alongside the error.
func TestRefreshFailureKeepsTheLastGoodList(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(500, `{"type":"error","error":{"type":"ServerError","message":"boom"}}`), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")
	c.storeModels([]core.Model{{ID: "gpt-5.1"}, {ID: "claude-opus-4-5"}})

	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("RefreshModels hid the failure")
	}
	if len(models) != 2 || models[0].ID != "gpt-5.1" {
		t.Fatalf("RefreshModels = %v, want the last good list alongside the error", models)
	}
	got, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Models = %d models, want the last good list", len(got))
	}
}

func TestFetchModelsRejectsANon200(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(404, `<html>Not Found</html>`), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")

	_, err := c.fetchModels(context.Background())
	if err == nil {
		t.Fatal("fetchModels accepted a 404")
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("error = %v, want it to name the status", err)
	}
}

// ---------------------------------------------------------------------------
// core.ModelLimitsProvider.
// ---------------------------------------------------------------------------

// The contract is unusually strict: consulted on every request that omitted
// max_tokens, so it must be cheap and must never fetch.  A cold cache declines.
func TestModelMaxOutputTokensNeverFetches(t *testing.T) {
	called := false
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		called = true
		return jsonResponse(200, `{"object":"list","data":[{"id":"gpt-5.1"}]}`), nil
	})
	addAccount(t, c, "opencode:a", "sk-a")

	for _, id := range []string{"", "   ", "gpt-5.1", "gemini-3-flash", "no-such-model"} {
		c.ModelMaxOutputTokens(context.Background(), id)
	}
	if called {
		t.Fatal("ModelMaxOutputTokens made a network call: the contract forbids it")
	}
}

func TestModelMaxOutputTokensAnswersFromTheStaticTable(t *testing.T) {
	c := newTestClient(t, Config{})

	if n, ok := c.ModelMaxOutputTokens(context.Background(), "gemini-3-flash"); !ok || n != 65536 {
		t.Fatalf("ModelMaxOutputTokens(gemini-3-flash) = %d, %v; want 65536, true", n, ok)
	}
	if n, ok := c.ModelMaxOutputTokens(context.Background(), "  claude-opus-4-5  "); !ok || n != 64000 {
		t.Fatalf("ModelMaxOutputTokens(trimmed) = %d, %v; want 64000, true", n, ok)
	}
	// The 73 ids with no published limit must decline rather than guess.
	for _, id := range []string{"", "   ", "gpt-5.1", "claude-opus-5", "kimi-k2.5", "brand-new"} {
		if n, ok := c.ModelMaxOutputTokens(context.Background(), id); ok {
			t.Fatalf("ModelMaxOutputTokens(%q) = %d, true; want a decline, not an invented cap", id, n)
		}
	}
}

// A limit that arrives with the live list is honoured from the cache.
func TestModelMaxOutputTokensHonoursACachedLiveLimit(t *testing.T) {
	c := newTestClient(t, Config{})
	c.storeModels([]core.Model{{
		ID:    "gpt-5.1",
		Extra: map[string]any{"max_output_tokens": int64(12345)},
	}})
	if n, ok := c.ModelMaxOutputTokens(context.Background(), "gpt-5.1"); !ok || n != 12345 {
		t.Fatalf("ModelMaxOutputTokens = %d, %v; want 12345, true from the cache", n, ok)
	}
}

// ---------------------------------------------------------------------------
// Free-model metadata and anonymous filtering.
// ---------------------------------------------------------------------------

// TestFreeModelIsMarkedInTheCatalogue proves the built-in catalogue advertises
// which ids the anonymous credential can actually serve.
func TestFreeModelIsMarkedInTheCatalogue(t *testing.T) {
	models := fallbackModels(Config{})
	var found bool
	for _, m := range models {
		if m.ID != "space-bunny-free" {
			continue
		}
		found = true
		if m.Extra["free"] != true {
			t.Fatalf("Extra = %+v, want free:true", m.Extra)
		}
	}
	if !found {
		t.Fatal("the built-in catalogue lost space-bunny-free")
	}
}

func TestFreeSuffixIsMarkedWithoutAnAllowlistEntry(t *testing.T) {
	m := markFree(core.Model{ID: "step-5-preview-free"}, freeModelSet(Config{}))
	if m.Extra["free"] != true {
		t.Fatalf("Extra = %+v, want free:true from the vendor id", m.Extra)
	}
}

func TestStep5PreviewFreeIsServedToAnAnonymousAccount(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"object":"list","data":[`+
			`{"id":"step-5-preview-free"},`+
			`{"id":"gpt-5.1"}]}`), nil
	})
	c.pool.upsert(accountRecord{
		ID: "opencode:anonymous", AuthMode: "anonymous", APIKey: "public",
		Enabled: true, Source: sourcePanel,
	})

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	var found bool
	for _, m := range models {
		if m.ID != "step-5-preview-free" {
			continue
		}
		found = true
		if m.Extra["free"] != true {
			t.Fatalf("Extra = %+v, want free:true", m.Extra)
		}
	}
	if !found {
		t.Fatalf("models = %v, want step-5-preview-free", modelIDsOf(models))
	}
}

// TestAnonymousOnlyPoolServesOnlyFreeModels proves the catalogue is narrowed
// to vendor free ids and the configured additions for an anonymous-only pool.
func TestAnonymousOnlyPoolServesOnlyFreeModels(t *testing.T) {
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"object":"list","data":[{"id":"space-bunny-free"},{"id":"gpt-5.1"}]}`), nil
	})
	c.pool.upsert(accountRecord{
		ID: "opencode:anonymous", AuthMode: "anonymous", APIKey: "public",
		Enabled: true, Source: sourcePanel,
	})

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 1 || models[0].ID != "space-bunny-free" {
		t.Fatalf("models = %+v, want only space-bunny-free", models)
	}
	if models[0].Extra["free"] != true {
		t.Fatalf("Extra = %+v, want free:true", models[0].Extra)
	}
}

// TestAnonymousChatRejectsAPaidModelLocally proves a model the anonymous
// credential cannot serve never reaches the network.
func TestAnonymousChatRejectsAPaidModelLocally(t *testing.T) {
	var calls int
	c := newFakeClient(t, Config{}, func(*http.Request) (*http.Response, error) {
		calls++
		return sseResponse(happySSE), nil
	})
	c.pool.upsert(accountRecord{
		ID: "opencode:anonymous", AuthMode: "anonymous", APIKey: "public",
		Enabled: true, Source: sourcePanel,
	})

	_, err := c.Chat(context.Background(), chatRequest("gpt-5.1"))
	wantErrIs(t, err, core.ErrUnsupported)
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
}

// TestOAuthWorkspaceModelsAreUnioned proves a signed-in workspace's allowed
// models are added to the catalogue the panel serves.
func TestOAuthWorkspaceModelsAreUnioned(t *testing.T) {
	c := newTestClient(t, Config{})
	c.pool.upsert(accountRecord{
		ID: "opencode:oauth", AuthMode: "oauth", AccessToken: "t", OrgID: "o",
		AllowedModels: []string{"my-workspace-model"},
		Enabled:       true, Source: sourcePanel,
	})

	models := c.servedModels(fallbackModels(c.cfg))
	var found bool
	for _, m := range models {
		if m.ID == "my-workspace-model" {
			found = true
		}
	}
	if !found {
		t.Fatalf("models = %v, want the workspace model unioned in", modelIDsOf(models))
	}
	if !containsModel(models, "space-bunny-free") {
		t.Fatal("the public catalogue was dropped for an OAuth account")
	}
}

func modelIDsOf(models []core.Model) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}
