package modelmeta

import (
	"testing"
	"time"
)

func TestDeepSeekOfficialPriceUsesCNYPricing(t *testing.T) {
	cases := []struct {
		name   string
		client string
		model  string
	}{
		{name: "workbuddy cn", client: "workbuddy", model: "cn:deepseek-v4.1-flash"},
		{name: "workbuddy global", client: "workbuddy", model: "global:deepseek-v4.1-flash"},
		{name: "raccoon", client: "raccoon", model: "sn-deepseek-v4-1-flash"},
		{name: "deepseek official id", client: "deepseek", model: "deepseek-flash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			price, ok := DefaultPrice(tc.client, tc.model)
			if !ok {
				t.Fatalf("DefaultPrice(%q, %q) did not resolve", tc.client, tc.model)
			}
			// Official DeepSeek CNY list price, off-peak, per million tokens.
			if price.InputPerMillion != 1 || price.OutputPerMillion != 4 || !price.HasCacheRead || price.CacheReadPerMillion != 0.02 {
				t.Fatalf("DefaultPrice(%q, %q) = %+v, want official CNY off-peak 1/4/0.02", tc.client, tc.model, price)
			}
		})
	}
}

func TestDeepSeekOfficialProPriceUsesCNYPricing(t *testing.T) {
	price, ok := DefaultPrice("workbuddy", "cn:deepseek-v4-pro")
	if !ok {
		t.Fatal("workbuddy deepseek-v4-pro did not resolve")
	}
	// Official DeepSeek CNY list price, off-peak, per million tokens.
	if price.InputPerMillion != 4.5 || price.OutputPerMillion != 13.5 || !price.HasCacheRead || price.CacheReadPerMillion != 0.15 {
		t.Fatalf("DefaultPrice(workbuddy, cn:deepseek-v4-pro) = %+v, want official CNY off-peak 4.5/13.5/0.15", price)
	}
}

func TestDeepSeekOfficialPriceUsesBeijingPeakHours(t *testing.T) {
	beijing := time.FixedZone("CST", 8*60*60)
	cases := []struct {
		name string
		at   time.Time
		want Price
	}{
		{
			name: "weekday before peak",
			at:   time.Date(2026, 10, 12, 8, 59, 0, 0, beijing),
			want: Price{InputPerMillion: 1, OutputPerMillion: 4, CacheReadPerMillion: 0.02, HasPrice: true, HasCacheRead: true},
		},
		{
			name: "weekday morning peak",
			at:   time.Date(2026, 10, 12, 9, 0, 0, 0, beijing),
			want: Price{InputPerMillion: 2, OutputPerMillion: 8, CacheReadPerMillion: 0.04, HasPrice: true, HasCacheRead: true},
		},
		{
			name: "weekday lunch off-peak",
			at:   time.Date(2026, 10, 12, 12, 0, 0, 0, beijing),
			want: Price{InputPerMillion: 1, OutputPerMillion: 4, CacheReadPerMillion: 0.02, HasPrice: true, HasCacheRead: true},
		},
		{
			name: "weekday afternoon peak",
			at:   time.Date(2026, 10, 12, 14, 0, 0, 0, beijing),
			want: Price{InputPerMillion: 2, OutputPerMillion: 8, CacheReadPerMillion: 0.04, HasPrice: true, HasCacheRead: true},
		},
		{
			name: "weekday after peak",
			at:   time.Date(2026, 10, 12, 18, 0, 0, 0, beijing),
			want: Price{InputPerMillion: 1, OutputPerMillion: 4, CacheReadPerMillion: 0.02, HasPrice: true, HasCacheRead: true},
		},
		{
			name: "weekend",
			at:   time.Date(2026, 10, 10, 10, 0, 0, 0, beijing),
			want: Price{InputPerMillion: 1, OutputPerMillion: 4, CacheReadPerMillion: 0.02, HasPrice: true, HasCacheRead: true},
		},
		{
			name: "national day holiday",
			at:   time.Date(2026, 10, 1, 10, 0, 0, 0, beijing),
			want: Price{InputPerMillion: 1, OutputPerMillion: 4, CacheReadPerMillion: 0.02, HasPrice: true, HasCacheRead: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			price, ok := DefaultPriceAt("workbuddy", "cn:deepseek-v4.1-flash", tc.at)
			if !ok {
				t.Fatal("DefaultPriceAt did not resolve")
			}
			if price != tc.want {
				t.Fatalf("DefaultPriceAt(%s) = %+v, want %+v", tc.at.Format(time.RFC3339), price, tc.want)
			}
		})
	}
}

func TestProviderMergeAtKeepsManualPricePrecedence(t *testing.T) {
	beijing := time.FixedZone("CST", 8*60*60)
	at := time.Date(2026, 10, 12, 10, 0, 0, 0, beijing)
	store, err := OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetPrice("workbuddy", "cn:deepseek-v4.1-flash", 7, 8, 0.5, true); err != nil {
		t.Fatal(err)
	}
	p := New(Options{Client: "workbuddy", Overrides: store})
	meta := p.MergeAt("cn:deepseek-v4.1-flash", Meta{}, at)
	if meta.InputPerMillion != 7 || meta.OutputPerMillion != 8 || meta.CacheReadPerMillion != 0.5 {
		t.Fatalf("MergeAt overwrote manual price: %+v", meta)
	}
}

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
