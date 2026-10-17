package tabbit

import "testing"

// fallbackModels is what /v1/models serves when the sidecar is absent - the cold
// start this package's metadata fallback exists for. The installed Tabbit client
// publishes no per-model context/output limits, so every entry stays bare rather
// than borrowing numbers from public documentation.
func TestFallbackModelsCarriesOnlyClientPublishedMetadata(t *testing.T) {
	byID := map[string]map[string]any{}
	for _, m := range fallbackModels(Config{}) {
		byID[m.ID] = m.Extra
	}

	for _, id := range []string{"DeepSeek-V4-Pro", "GLM-5.1", "MiMo-V2.6-Pro", "Kimi-K3", "LongCat-2.0", "Default", "Qwen3.5-Plus"} {
		extra, ok := byID[id]
		if !ok {
			t.Fatalf("%s is missing from the fallback catalogue", id)
		}
		if v, ok := extra["context_length"]; ok {
			t.Errorf("%s grew a context_length of %v; the client publishes none", id, v)
		}
		if v, ok := extra["max_output_tokens"]; ok {
			t.Errorf("%s grew a max_output_tokens of %v; the client publishes none", id, v)
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

// Extra models configured by the operator are not given numbers when the client
// publishes none for them.
func TestFallbackModelsDoesNotFillExtraModelsWithoutClientSource(t *testing.T) {
	models := fallbackModels(Config{ExtraModels: []string{"GPT-5.5", "totally-made-up"}})
	byID := map[string]map[string]any{}
	for _, m := range models {
		byID[m.ID] = m.Extra
	}
	if v, ok := byID["GPT-5.5"]["context_length"]; ok {
		t.Errorf("configured GPT-5.5 grew a context_length of %v", v)
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
