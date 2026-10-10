package panel

import (
	"encoding/json"
	"testing"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

func TestModelsListIncludesCurrentAndOfficialPrices(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPrice("openai", "gpt-5.4", 1.5, 6.5, 0.3, true); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{name: "openai", models: []core.Model{{ID: "gpt-5.4"}}}
	h := New(Options{Registry: registryOf(client), ModelOverrides: store})

	rec := get(t, h, "/panel/api/models")
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	model := got["clients"].([]any)[0].(map[string]any)["models"].([]any)[0].(map[string]any)

	if model["has_price"] != true || model["has_cache_read"] != true {
		t.Fatalf("price flags = %#v / %#v", model["has_price"], model["has_cache_read"])
	}
	if model["input_per_million"] != 1.5 || model["output_per_million"] != 6.5 || model["cache_read_per_million"] != 0.3 {
		t.Fatalf("manual price = %#v / %#v / %#v", model["input_per_million"], model["output_per_million"], model["cache_read_per_million"])
	}
	if model["input_price_source"] != modelmeta.SourceManual || model["output_price_source"] != modelmeta.SourceManual || model["cache_read_price_source"] != modelmeta.SourceManual {
		t.Fatalf("price sources = %#v / %#v / %#v", model["input_price_source"], model["output_price_source"], model["cache_read_price_source"])
	}
	if model["default_has_price"] != true || model["default_has_cache_read"] != true {
		t.Fatalf("default flags = %#v / %#v", model["default_has_price"], model["default_has_cache_read"])
	}
	if model["input_price_default"] != 18.0 || model["output_price_default"] != 108.0 || model["cache_read_price_default"] != 1.8 {
		t.Fatalf("official defaults = %#v / %#v / %#v", model["input_price_default"], model["output_price_default"], model["cache_read_price_default"])
	}
	if model["price_editable"] != true {
		t.Fatalf("price_editable = %#v", model["price_editable"])
	}
}
