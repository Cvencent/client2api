package loomy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSplitRateBothSpellings(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantRate string
	}{
		// The vendor's own spelling, full-width brackets.
		{"DeepSeek V4 Flash 0731（x3.0）", "DeepSeek V4 Flash 0731", "x3.0"},
		// Half-width brackets.
		{"DeepSeek V4 Flash 0731 (x3.0)", "DeepSeek V4 Flash 0731", "x3.0"},
		// This project's already-normalised form.
		{"DeepSeek V4 Flash 0731 · x3.0", "DeepSeek V4 Flash 0731", "x3.0"},
		// Spaces inside the multiplier are removed, so both spellings compare
		// equal.
		{"Model ( x 1.0 )", "Model", "x1.0"},
		{"Model（ X 1.0 ）", "Model", "x1.0"},
		// A model with no multiplier at all (image generators, mostly).
		{"Some Model", "Some Model", ""},
		// A trailing separator with nothing after it is not a rate, so nothing
		// is invented.
		{"Model ·", "Model ·", ""},
		{"Model ()", "Model ()", ""},
		// A multiplier with no name is malformed; the original string is kept
		// rather than an empty name being invented.
		{"（x3.0）", "（x3.0）", ""},
		// Only a trailing multiplier counts.
		{"x3.0 model", "x3.0 model", ""},
	}
	for _, tc := range cases {
		name, rate := splitRate(tc.in)
		if name != tc.wantName || rate != tc.wantRate {
			t.Errorf("splitRate(%q) = (%q, %q), want (%q, %q)",
				tc.in, name, rate, tc.wantName, tc.wantRate)
		}
	}
}

// TestSplitRateIsIdempotent pins the property that makes it safe to normalise a
// name that may already be normalised.
func TestSplitRateIsIdempotent(t *testing.T) {
	inputs := []string{
		"DeepSeek V4 Flash 0731（x3.0）",
		"Kimi k2.6 (x6.5)",
		"qwen-3.8-max",
		"Model ( x 12.0 )",
		"Spark X2.5 · x0.1",
	}
	for _, in := range inputs {
		first := displayName(in)
		name, rate := splitRate(in)
		if got := displayName(first); got != first {
			t.Errorf("displayName is not idempotent for %q: %q then %q", in, first, got)
		}
		name2, rate2 := splitRate(first)
		if name2 != name || rate2 != rate {
			t.Errorf("splitRate(displayName(%q)) = (%q, %q), want (%q, %q)",
				in, name2, rate2, name, rate)
		}
	}
}

func TestDisplayNameNeverLeavesADanglingSeparator(t *testing.T) {
	if got := displayName("Some Model"); got != "Some Model" {
		t.Errorf("displayName without a rate = %q, want the bare name", got)
	}
	if got := displayName("DeepSeek V4 Flash 0731（x3.0）"); got != "DeepSeek V4 Flash 0731 · x3.0" {
		t.Errorf("displayName = %q", got)
	}
	if got := modelNameWithoutRate("DeepSeek V4 Flash 0731 · x3.0"); got != "DeepSeek V4 Flash 0731" {
		t.Errorf("modelNameWithoutRate = %q", got)
	}
	if got := modelNameWithoutRate("Some Model"); got != "Some Model" {
		t.Errorf("modelNameWithoutRate without a rate = %q", got)
	}
}

// TestParseRemoteModelsAcceptsBothShapes covers the bare array and the `data`
// wrapper, and proves non-chat entries are skipped rather than stringified into
// a bogus model.
func TestParseRemoteModelsAcceptsBothShapes(t *testing.T) {
	entry := `{"id":"m1","name":"Model One（x2.0）","type":"chat","context_length":1000}`

	bare := json.RawMessage(`[` + entry + `,{"id":"img","name":"Image","type":"image"}]`)
	models, err := parseRemoteModels(bare)
	if err != nil {
		t.Fatalf("bare array: %v", err)
	}
	if len(models) != 1 || models[0].ID != "m1" {
		t.Fatalf("bare array: got %+v", models)
	}
	if models[0].Name != "Model One · x2.0" {
		t.Errorf("name was not normalised: %q", models[0].Name)
	}
	if models[0].ContextWindow != 1000 {
		t.Errorf("context window = %d, want 1000", models[0].ContextWindow)
	}

	wrapped := json.RawMessage(`{"code":"000000","data":[` + entry + `]}`)
	models, err = parseRemoteModels(wrapped)
	if err != nil {
		t.Fatalf("data wrapper: %v", err)
	}
	if len(models) != 1 || models[0].ID != "m1" {
		t.Fatalf("data wrapper: got %+v", models)
	}
}

func TestParseRemoteModelsErrors(t *testing.T) {
	cases := map[string]json.RawMessage{
		"empty":           json.RawMessage(``),
		"null":            json.RawMessage(`null`),
		"empty array":     json.RawMessage(`[]`),
		"only non-chat":   json.RawMessage(`[{"id":"a","type":"image"}]`),
		"no data field":   json.RawMessage(`{"code":"000000"}`),
		"chat without id": json.RawMessage(`[{"name":"x","type":"chat"}]`),
		"not json at all": json.RawMessage(`<html>nope</html>`),
	}
	for name, raw := range cases {
		if _, err := parseRemoteModels(raw); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

// TestRawModelEntryCoercion covers the defensive decoding: the vendor has been
// seen to send `context_length` as a string, and `input_modalities` says nothing
// about whether a model generates images.
func TestRawModelEntryCoercion(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "m1",
		"name": "Model（x1.5）",
		"type": "chat",
		"context_length": "262144",
		"reasoning_efforts": ["low", "high", "high", 7, "  ", "medium"],
		"default_reasoning_effort": "low",
		"capabilities": {"reasoning": true, "input_modalities": ["text", "IMAGE"]},
		"output_modalities": ["image"]
	}`)
	var row rawModelEntry
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !row.isChat() {
		t.Fatalf("entry should be a chat model")
	}
	m, ok := row.model()
	if !ok {
		t.Fatalf("model() refused a chat entry")
	}
	if m.ContextWindow != 262144 {
		t.Errorf("context window = %d, want 262144 from a string", m.ContextWindow)
	}
	if !m.SupportsImage {
		t.Errorf("image input modality was not detected")
	}
	if !m.SupportsThinking {
		t.Errorf("capabilities.reasoning was not honoured")
	}
	// De-duplicated in first-seen order, with the non-string and blank entries
	// dropped rather than stringified.
	if got, want := strings.Join(m.Efforts, ","), "low,high,medium"; got != want {
		t.Errorf("efforts = %q, want %q", got, want)
	}
	if m.DefaultEffort != "low" {
		t.Errorf("default effort = %q, want low", m.DefaultEffort)
	}
}

// TestIsChatIgnoresOutputModalities is the specific trap the reference
// documents: several chat models accept `image` as an INPUT modality.
func TestIsChatIgnoresOutputModalities(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "vision-chat",
		"name": "Vision Chat（x1.0）",
		"type": "chat",
		"output_modalities": ["text"],
		"capabilities": {"input_modalities": ["image", "text"]}
	}`)
	var row rawModelEntry
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !row.isChat() {
		t.Fatalf("a chat model with image INPUT must still be a chat model")
	}
}

func TestCoreModelsCarriesDisplayNameAndMultiplier(t *testing.T) {
	models := coreModels([]remoteModel{{
		ID:            "m1",
		Name:          "Model One · x2.0",
		ContextWindow: 4096,
		SupportsImage: true,
		Efforts:       []string{"low", "high"},
		DefaultEffort: "high",
	}}, false)

	if len(models) != 1 {
		t.Fatalf("got %d models", len(models))
	}
	m := models[0]
	if m.ID != "m1" {
		t.Errorf("ID = %q, want the vendor's raw id", m.ID)
	}
	if m.OwnedBy != "loomy" {
		t.Errorf("OwnedBy = %q", m.OwnedBy)
	}
	if got := m.Extra["display_name"]; got != "Model One · x2.0" {
		t.Errorf("display_name = %v", got)
	}
	if got := m.Extra["context_length"]; got != 4096 {
		t.Errorf("context_length = %v", got)
	}
	if got := m.Extra["vision"]; got != true {
		t.Errorf("vision = %v", got)
	}
	if _, ok := m.Extra["fallback"]; ok {
		t.Errorf("fallback must not be set on a live catalogue entry")
	}
	if got := m.Extra["default_effort"]; got != "high" {
		t.Errorf("default_effort = %v", got)
	}
}

func TestFallbackCatalogue(t *testing.T) {
	models := fallbackCatalogue()
	if len(models) != len(fallbackModels) {
		t.Fatalf("fallback catalogue has %d entries, want %d", len(models), len(fallbackModels))
	}
	seen := map[string]bool{}
	for _, m := range models {
		if m.ID == "" {
			t.Fatalf("a fallback entry has no id")
		}
		if seen[m.ID] {
			t.Fatalf("duplicate fallback id %q", m.ID)
		}
		seen[m.ID] = true
		if m.Extra["fallback"] != true {
			t.Errorf("%s: fallback flag not set", m.ID)
		}
		name, _ := m.Extra["display_name"].(string)
		if name == "" {
			t.Errorf("%s: no display_name", m.ID)
		}
		// Vision is under-reported on purpose: offering an image the server may
		// reject is worse than waiting for the live catalogue.
		if m.Extra["vision"] != false {
			t.Errorf("%s: fallback vision must be false", m.ID)
		}
	}
	if !seen["deepseek-v4-flash-0731"] || !seen["spark-x"] {
		t.Errorf("the fallback table lost a known model: %v", seen)
	}
}

// TestFallbackCatalogueKeepsTheVendorsSparkXWindow records the deliberate
// disagreement with the Loomy desktop client, which forces 262144 locally.
func TestFallbackCatalogueKeepsTheVendorsSparkXWindow(t *testing.T) {
	for _, m := range fallbackCatalogue() {
		if m.ID != "spark-x" {
			continue
		}
		if got := m.Extra["context_length"]; got != 1048576 {
			t.Fatalf("spark-x context_length = %v, want the vendor's own 1048576", got)
		}
		return
	}
	t.Fatalf("spark-x is missing from the fallback catalogue")
}

func TestResolveModelID(t *testing.T) {
	models := fallbackCatalogue()

	cases := []struct {
		in   string
		want string
	}{
		{"deepseek-v4-flash-0731", "deepseek-v4-flash-0731"},
		{"DeepSeek V4 Flash 0731", "deepseek-v4-flash-0731"},
		{"DeepSeek V4 Flash 0731 · x3.0", "deepseek-v4-flash-0731"},
		{"DEEPSEEK-V4-FLASH-0731", "deepseek-v4-flash-0731"},
		{"spark-x", "spark-x"},
		{"Spark X2.5 · x0.1", "spark-x"},
		{"something-else", "something-else"},
		{"  Kimi-k2.6  ", "Kimi-k2.6"},
	}
	for _, tc := range cases {
		if got := resolveModelID(models, tc.in); got != tc.want {
			t.Errorf("resolveModelID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := resolveModelID(models, ""); got != "" {
		t.Errorf("resolveModelID(\"\") = %q, want empty", got)
	}
}
