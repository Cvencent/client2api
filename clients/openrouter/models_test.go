package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"client2api/internal/core"
)

// --- free pricing ---------------------------------------------------------

func TestFreePricingMarksAModelFree(t *testing.T) {
	body := `{"data":[
		{"id":"vendor/free-string","pricing":{"prompt":"0","completion":"0.000000"}},
		{"id":"vendor/free-number","pricing":{"prompt":0,"completion":0}},
		{"id":"vendor/paid","pricing":{"prompt":"0.000001","completion":"0.000002"}},
		{"id":"vendor/half-free","pricing":{"prompt":"0","completion":"0.000002"}},
		{"id":"vendor/no-price"}
	]}`
	list, err := parseModelsBody([]byte(body))
	if err != nil {
		t.Fatalf("parseModelsBody: %v", err)
	}
	byID := make(map[string]core.Model, len(list))
	for _, m := range list {
		byID[m.ID] = m
	}
	for _, id := range []string{"vendor/free-string", "vendor/free-number"} {
		m, ok := byID[id]
		if !ok {
			t.Fatalf("%s is missing from the catalogue", id)
		}
		if m.Extra["free"] != true {
			t.Fatalf("%s Extra[free] = %v, want true", id, m.Extra["free"])
		}
	}
	for _, id := range []string{"vendor/paid", "vendor/half-free", "vendor/no-price"} {
		m, ok := byID[id]
		if !ok {
			t.Fatalf("%s is missing from the catalogue", id)
		}
		if _, marked := m.Extra["free"]; marked {
			t.Fatalf("%s is marked free, want it left alone", id)
		}
	}
}

func TestFallbackMarksTheKnownFreeModels(t *testing.T) {
	list := fallbackModels(Config{})
	byID := make(map[string]core.Model, len(list))
	for _, m := range list {
		byID[m.ID] = m
	}
	for _, id := range []string{"openrouter/free", "cohere/north-mini-code:free"} {
		m, ok := byID[id]
		if !ok {
			t.Fatalf("%s is missing from the built-in catalogue", id)
		}
		if m.Extra["free"] != true {
			t.Fatalf("%s Extra[free] = %v, want true", id, m.Extra["free"])
		}
	}
	if m, ok := byID["openai/gpt-4o"]; ok {
		if _, marked := m.Extra["free"]; marked {
			t.Fatalf("openai/gpt-4o is marked free, want it left alone")
		}
	}
}

// --- probe selection ------------------------------------------------------

func TestProbeModelPrefersTheConfiguredModel(t *testing.T) {
	// free_only off: the operator's explicit test_model is the whole point of
	// the knob.  TestFreeOnlyProbesFreeEvenWhenTheConfiguredModelIsPaid pins
	// the free-only exception.
	c := newTestClient(t, `{"api_key":"`+testKey+`","test_model":"vendor/configured","free_only":false}`, nil)
	c.models.store([]core.Model{
		{ID: "vendor/free", Extra: map[string]any{"free": true}},
	}, testBase)
	if got := c.probeModel(); got != "vendor/configured" {
		t.Fatalf("probeModel() = %q, want the configured test_model", got)
	}
}

func TestProbeModelFallsBackToTheFirstFreeModel(t *testing.T) {
	c := newTestClient(t, cfgWithKey(testKey), nil)
	c.models.store([]core.Model{
		{ID: "vendor/paid", Extra: map[string]any{"pricing_prompt": "1"}},
		{ID: "vendor/free-one", Extra: map[string]any{"free": true}},
		{ID: "vendor/free-two", Extra: map[string]any{"free": true}},
	}, testBase)
	if got := c.probeModel(); got != "vendor/free-one" {
		t.Fatalf("probeModel() = %q, want the first free model", got)
	}
}

func TestTestAccountProbesAFreeModelWhenNoneIsConfigured(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: sseBasic} }}
	c := newTestClient(t, cfgWithKey(testKey), up)
	c.models.store([]core.Model{
		{ID: "vendor/paid", Extra: map[string]any{"pricing_prompt": "1"}},
		{ID: "vendor/free", Extra: map[string]any{"free": true}},
	}, testBase)

	res, err := c.TestAccount(context.Background(), accountIDFor(testKey))
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("result = %+v, want the probe to succeed", res)
	}
	if res.Model != "vendor/free" {
		t.Fatalf("probe model = %q, want the cached free model", res.Model)
	}
	body := up.jsonAt(t, 0)
	if body["model"] != "vendor/free" {
		t.Fatalf("request model = %v, want vendor/free", body["model"])
	}
}

// --- free-only policy -----------------------------------------------------
//
// free_only is the deployment's promise that OpenRouter never spends paid
// credit.  It has two halves: the served catalogue is narrowed to the
// zero-cost ids, and a chat that names a known paid id is refused before it
// can leave the process.  Both default ON.

func catalogueIDs(list []core.Model) []string {
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, m.ID)
	}
	return out
}

func catalogueHas(list []core.Model, id string) bool {
	for _, m := range list {
		if m.ID == id {
			return true
		}
	}
	return false
}

func TestFreeOnlyDefaultsOn(t *testing.T) {
	cfg, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if !cfg.freeOnly() {
		t.Fatal("free_only must default to on")
	}
	off, err := parseConfig(json.RawMessage(`{"free_only":false}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	if off.freeOnly() {
		t.Fatal(`"free_only":false must turn the policy off`)
	}
}

func TestFreeOnlyNarrowsTheColdStartCatalogue(t *testing.T) {
	cfg, _ := parseConfig(json.RawMessage(`{"extra_models":["vendor/paid-extra","vendor/added:free"]}`))
	list := fallbackModels(cfg)
	if len(list) == 0 {
		t.Fatal("the free-only cold-start catalogue must still carry the known free ids")
	}
	for _, m := range list {
		if !modelIsFree(m) {
			t.Fatalf("%s reached a free-only catalogue without a zero price", m.ID)
		}
	}
	for _, id := range []string{"openai/gpt-4o", "vendor/paid-extra"} {
		if catalogueHas(list, id) {
			t.Fatalf("%s is paid and must not be advertised: %v", id, catalogueIDs(list))
		}
	}
	if !catalogueHas(list, "openrouter/free") {
		t.Fatalf("the known free ids must survive: %v", catalogueIDs(list))
	}
}

func TestFreeOnlyKeepsTheWholeCatalogueWhenOff(t *testing.T) {
	cfg, _ := parseConfig(json.RawMessage(`{"free_only":false}`))
	list := fallbackModels(cfg)
	if !catalogueHas(list, "openai/gpt-4o") {
		t.Fatalf("free_only:false must keep the paid curated ids: %v", catalogueIDs(list))
	}
}

func TestFreeOnlyDropsPaidModelsFromALiveFetch(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply {
		return reply{body: `{"data":[{"id":"vendor/free","pricing":{"prompt":"0","completion":"0"}},{"id":"vendor/paid","pricing":{"prompt":"0.1","completion":"0.2"}}]}`}
	}}
	c := newTestClient(t, fmt.Sprintf(`{"api_key":%q}`, testKey), up)

	list, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(list) != 1 || list[0].ID != "vendor/free" {
		t.Fatalf("live catalogue = %v, want only vendor/free", catalogueIDs(list))
	}

	served, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if catalogueHas(served, "vendor/paid") {
		t.Fatalf("the paid id is still served: %v", catalogueIDs(served))
	}
}

func TestFreeOnlyRefusesAKnownPaidModelBeforeTheVendor(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: sseBasic} }}
	c := newTestClient(t, fmt.Sprintf(`{"api_key":%q}`, testKey), up)
	c.models.store([]core.Model{
		{ID: "vendor/free", Extra: map[string]any{"free": true}},
		{ID: "vendor/paid", Extra: map[string]any{"pricing_prompt": "0.1"}},
	}, testBase)

	if _, err := c.Chat(context.Background(), simpleReq("vendor/paid")); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if up.count() != 0 {
		t.Fatal("a paid model was refused only after the vendor had been called")
	}
}

func TestFreeOnlyProbesFreeEvenWhenTheConfiguredModelIsPaid(t *testing.T) {
	// The built-in test_model is paid, so a free-only deployment must not let
	// the Test button spend credit just because nobody overrode the default.
	c := newTestClient(t, fmt.Sprintf(`{"api_key":%q,"test_model":"vendor/paid"}`, testKey), nil)
	c.models.store([]core.Model{
		{ID: "vendor/free", Extra: map[string]any{"free": true}},
		{ID: "vendor/paid", Extra: map[string]any{"pricing_prompt": "0.1"}},
	}, testBase)

	if got := c.probeModel(); got != "vendor/free" {
		t.Fatalf("probeModel() = %q, want the free id under free_only", got)
	}
}

func TestFreeOnlyLetsAFreeModelThrough(t *testing.T) {
	up := &fakeUpstream{handle: func(*http.Request, string) reply { return reply{body: sseBasic} }}
	c := newTestClient(t, fmt.Sprintf(`{"api_key":%q}`, testKey), up)
	c.models.store([]core.Model{
		{ID: "vendor/free", Extra: map[string]any{"free": true}},
	}, testBase)

	st, err := c.Chat(context.Background(), simpleReq("vendor/free"))
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	_ = st.Close()
	if up.count() != 1 {
		t.Fatalf("requests = %d, want the free model to reach the vendor", up.count())
	}
}
