package modelmeta

import "testing"

func TestModelPriceOverrideRoundTrip(t *testing.T) {
	s, err := OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrice("zcode", "glm-5.3", 1.5, 6.5, 0.3, true); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("zcode", "glm-5.3")
	if !ok {
		t.Fatal("price override was not stored")
	}
	if !got.HasPrice || got.InputPerMillion != 1.5 || got.OutputPerMillion != 6.5 || !got.HasCacheRead || got.CacheReadPerMillion != 0.3 {
		t.Fatalf("price override = %+v", got)
	}
}

func TestDefaultPricingIsCNYPerMillion(t *testing.T) {
	p, ok := DefaultPrice("openai", "gpt-5.4")
	if !ok {
		t.Fatal("gpt-5.4 has no official default price")
	}
	// models.dev publishes USD 2.5 / 15 / 0.25 per million; the embedded
	// directory is converted to CNY by pricingTable using _meta.usd_to_cny.
	if p.InputPerMillion <= 2.5 || p.OutputPerMillion <= 15 || !p.HasCacheRead {
		t.Fatalf("default price is not CNY: %+v", p)
	}
}

func TestProviderMergesPriceWithManualPrecedence(t *testing.T) {
	s, err := OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrice("zcode", "glm-5.3", 9, 20, 0, false); err != nil {
		t.Fatal(err)
	}
	p := New(Options{Client: "zcode", Overrides: s})
	meta := p.Merge("glm-5.3", Meta{})
	if !meta.HasPrice || meta.InputPerMillion != 9 || meta.OutputPerMillion != 20 || meta.HasCacheRead {
		t.Fatalf("merged price = %+v", meta)
	}
}

func TestProviderFillsOnlyMissingOfficialPriceFields(t *testing.T) {
	p := New(Options{Client: "openai"})

	vendorPriced := p.Merge("gpt-5.4", Meta{
		InputPerMillion:  1,
		OutputPerMillion: 2,
		HasPrice:         true,
	})
	if vendorPriced.InputPerMillion != 1 || vendorPriced.OutputPerMillion != 2 {
		t.Fatalf("official default overwrote vendor price: %+v", vendorPriced)
	}
	if !vendorPriced.HasCacheRead || vendorPriced.CacheReadPerMillion != 1.8 {
		t.Fatalf("official cache price was not filled: %+v", vendorPriced)
	}

	vendorCache := p.Merge("gpt-5.4", Meta{
		CacheReadPerMillion: 0.5,
		HasCacheRead:        true,
	})
	if !vendorCache.HasPrice || vendorCache.InputPerMillion != 18 || vendorCache.OutputPerMillion != 108 {
		t.Fatalf("official input/output price was not filled: %+v", vendorCache)
	}
	if vendorCache.CacheReadPerMillion != 0.5 {
		t.Fatalf("official cache price overwrote vendor cache price: %+v", vendorCache)
	}
}

func TestDefaultPriceNormalizesCatalogVariantIDs(t *testing.T) {
	cases := []struct {
		name   string
		client string
		model  string
	}{
		{name: "batch suffix", client: "cline", model: "anthropic/claude-opus-4.5:batch"},
		{name: "dotted model id", client: "cline", model: "anthropic/claude-fable-5.1"},
		{name: "provider namespace", client: "workbuddy", model: "cn:glm-5.3"},
		{name: "reseller prefix", client: "raccoon", model: "sn-kimi-k3"},
		{name: "vendor-prefixed id", client: "cline", model: "openai/gpt-5.1-codex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			price, ok := DefaultPrice(tc.client, tc.model)
			if !ok || !price.HasPrice {
				t.Fatalf("DefaultPrice(%q, %q) = %+v, %v; want a price", tc.client, tc.model, price, ok)
			}
		})
	}
}

func TestExplicitFreeModelIsPricedZero(t *testing.T) {
	for _, tc := range []struct {
		client string
		model  string
	}{
		{client: "openrouter", model: "google/gemma-4-31b-it:free"},
		{client: "opencode", model: "mimo-v2.6-flash-free"},
		{client: "cline", model: "cline-free/mimo-v2.6-flash"},
	} {
		price, ok := DefaultPrice(tc.client, tc.model)
		if !ok || !price.HasPrice || price.HasCacheRead || price.InputPerMillion != 0 || price.OutputPerMillion != 0 {
			t.Fatalf("DefaultPrice(%q, %q) = %+v, %v; want explicit zero", tc.client, tc.model, price, ok)
		}
	}
}

func TestGenericAliasIsNotMistakenForOfficialPrice(t *testing.T) {
	if price, ok := DefaultPrice("workbuddy", "cn:auto"); ok {
		t.Fatalf("generic auto alias resolved to unrelated price: %+v", price)
	}
}
