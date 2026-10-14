package openaicompat

import (
	"testing"
)

// The embedded registry is the data source for provider rows the hand-written
// table did not already know.  These ids exist in the upstream catalogue but
// not in builtinProviders, so a base URL here proves the JSON was actually
// merged rather than merely vendored.
func TestEmbeddedRegistrySuppliesNewOpenAIChatProviders(t *testing.T) {
	for id, want := range map[string]string{
		"agnes":         "https://apihub.agnes-ai.com/v1",
		"alibaba_cloud": "https://dashscope-intl.aliyuncs.com/compatible-mode/v1",
		"cohere":        "https://api.cohere.ai/compatibility/v1",
	} {
		if got := builtinBaseURL(id); got != want {
			t.Errorf("builtinBaseURL(%q) = %q, want %q from the embedded registry", id, got, want)
		}
	}
}

func TestEmbeddedRegistrySuppliesKeyPagesAndModels(t *testing.T) {
	if got := builtinKeyPage("agnes"); got != "https://agnes-ai.com/" {
		t.Errorf("agnes key page = %q", got)
	}
	if got := builtinKeyPage("alibaba_cloud"); got != "https://www.alibabacloud.com/help/en/model-studio/get-api-key" {
		t.Errorf("alibaba_cloud key page = %q", got)
	}

	// Keyless rows have no auth.url; the website is still the useful setup link.
	if got := builtinKeyPage("opencode"); got != "https://opencode.ai" {
		t.Errorf("opencode key page = %q, want the website fallback", got)
	}

	for _, id := range []string{"agnes", "alibaba_cloud", "cohere"} {
		p := ProviderConfig{ID: id}
		if models := p.modelsFor(nil); len(models) == 0 {
			t.Errorf("provider %q has no registry-backed cold-start model", id)
		}
	}
	if models := (ProviderConfig{ID: "agnes"}).modelsFor(nil); len(models) == 0 || models[0] != "agnes-2.0-flash" {
		t.Fatalf("agnes models = %v, want the registry's agnes-2.0-flash first", models)
	}
}

// Only openai-chat rows belong in this module.  Native Anthropic Messages and
// the OpenAI Responses wire are different protocols; silently listing them
// here would only produce a runtime 404.
func TestEmbeddedRegistrySkipsOtherWireFormats(t *testing.T) {
	for _, id := range []string{"anthropic", "openai"} {
		if got := builtinBaseURL(id); got != "" {
			t.Errorf("builtinBaseURL(%q) = %q, want the non-openai-chat row excluded", id, got)
		}
	}
}

func TestParseRegistryRejectsMalformedAndKeepsOnlyUsableRows(t *testing.T) {
	if got := parseRegistry([]byte(`{"not":`)); len(got) != 0 {
		t.Fatalf("malformed registry produced %d rows", len(got))
	}
	raw := []byte(`[
		{"id":"good","baseUrl":"https://good.example/v1","format":"openai-chat","auth":{"url":"https://good.example/keys"},"models":[{"id":"m1"},{"id":""}]},
		{"id":"keyless","baseUrl":"https://keyless.example/v1","format":"openai-chat","auth":{"url":""},"website":"https://keyless.example","models":[{"id":"m2"}]},
		{"id":"wrong-format","baseUrl":"https://wrong.example/v1","format":"anthropic-messages"},
		{"id":"no-base","baseUrl":"","format":"openai-chat"},
		{"id":"","baseUrl":"https://empty.example/v1","format":"openai-chat"}
	]`)
	got := parseRegistry(raw)
	if len(got) != 2 || got[0].ID != "good" || got[0].BaseURL != "https://good.example/v1" {
		t.Fatalf("parseRegistry = %+v, want the usable openai-chat rows", got)
	}
	if len(got[0].Models) != 1 || got[0].Models[0] != "m1" {
		t.Fatalf("parsed models = %v, want only the non-empty id", got[0].Models)
	}
	if got[1].ID != "keyless" || got[1].KeyPage != "https://keyless.example" {
		t.Fatalf("keyless row = %+v, want the website fallback", got[1])
	}
}
