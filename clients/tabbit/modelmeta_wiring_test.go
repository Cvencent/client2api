package tabbit

import "testing"

// fallbackModels is what /v1/models serves when the sidecar is absent — the cold
// start this package's metadata fallback exists for. The two halves of the wiring
// contract are pinned here: an id our static table can prove gains the numbers, and
// an id it cannot prove gains nothing at all.
func TestFallbackModelsCarriesOnlySourcedMetadata(t *testing.T) {
	byID := map[string]map[string]any{}
	for _, m := range fallbackModels(Config{}) {
		byID[m.ID] = m.Extra
	}

	sourced := []struct {
		id      string
		context int64
		output  int64
	}{
		{"DeepSeek-V4-Pro", 1000000, 384000},
		{"GLM-5.1", 200000, 131072},
	}
	for _, tc := range sourced {
		extra, ok := byID[tc.id]
		if !ok {
			t.Fatalf("%s is missing from the fallback catalogue", tc.id)
		}
		if got := extra["context_length"]; got != tc.context {
			t.Errorf("%s context_length = %v, want %d", tc.id, got, tc.context)
		}
		if got := extra["max_output_tokens"]; got != tc.output {
			t.Errorf("%s max_output_tokens = %v, want %d", tc.id, got, tc.output)
		}
	}

	// Ids the static table cannot prove must stay bare.  These four are real
	// vendor display names, so they are in the catalogue without numbers.
	unsourced := []string{"Default", "MiMo-V2.6-Pro", "Kimi-K3", "Qwen3.5-Plus", "LongCat-2.0"}
	for _, id := range unsourced {
		extra, ok := byID[id]
		if !ok {
			t.Fatalf("%s is missing from the fallback catalogue", id)
		}
		if v, ok := extra["context_length"]; ok {
			t.Errorf("%s grew a context_length of %v; no source covers it", id, v)
		}
		if v, ok := extra["max_output_tokens"]; ok {
			t.Errorf("%s grew a max_output_tokens of %v; no source covers it", id, v)
		}
	}

	// The keys this module already emitted must survive untouched.
	for id, extra := range byID {
		if extra["fallback"] != true {
			t.Errorf("%s lost its fallback marker: %v", id, extra["fallback"])
		}
		if _, ok := extra["tabbit_display_name"].(string); !ok {
			t.Errorf("%s lost its display name: %v", id, extra["tabbit_display_name"])
		}
	}
}

// Extra models configured by the operator are filled from the same table, and an
// operator-supplied id with no sourced numbers still gets no numbers.
func TestFallbackModelsFillsExtraModelsFromTheSameTable(t *testing.T) {
	models := fallbackModels(Config{ExtraModels: []string{"GPT-5.5", "totally-made-up"}})
	byID := map[string]map[string]any{}
	for _, m := range models {
		byID[m.ID] = m.Extra
	}
	if got := byID["GPT-5.5"]["context_length"]; got != int64(1050000) {
		t.Errorf("configured GPT-5.5 context_length = %v, want 1050000", got)
	}
	if v, ok := byID["totally-made-up"]["context_length"]; ok {
		t.Errorf("an unknown configured id grew a context_length of %v", v)
	}
}

// The fallback path must stay offline and side-effect free: no cache directory is
// touched, because the provider behind it is built without one.
func TestFallbackMetadataNeverTouchesTheDisk(t *testing.T) {
	if got := metaProvider.Store(); got != nil {
		t.Fatalf("metaProvider has a cache store (%v); the fallback path must not touch the disk", got)
	}
	if metaProvider.RemoteEnabled() {
		t.Fatal("metaProvider has models.dev enabled; the fallback path must stay offline")
	}
}
