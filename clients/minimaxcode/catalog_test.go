package minimaxcode

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"client2api/internal/core"
)

// vendorCatalogueReply is the desktop client's config endpoint's own answer,
// captured on 2026-10-20.  It is the authority for which ids exist: `models`
// holds the definitions, `model_order` their display order, and `whitelist` an
// allowlist that still names two ids the vendor no longer defines.  A
// whitelisted id with no definition must NOT be invented into the catalogue.
const vendorCatalogueReply = `{"version":"1.0","ttlSeconds":300,"providers":[{"providerId":"minimax","config":{"name":"MiniMax","npm":"@ai-sdk/anthropic","models":{"MiniMax-M3":{"name":"M3","attachment":true,"reasoning":true,"temperature":true,"tool_call":true,"limit":{"context":512000,"output":128000},"modalities":{"input":["text","image","video"],"output":["text"]},"options":{"reasoningSummary":"auto"},"variants":{"none-thinking":{"thinking":{"type":"disabled"}},"thinking":{"thinking":{"type":"adaptive"}}},"thinking_config":{"mode":"switchable","default_value":"true"},"capabilities":{"files_api_upload_endpoint":"/v1/files/upload","max_attachments_count":9,"max_image_bytes_inline":10485760,"max_request_body_bytes":67108864,"max_video_bytes_inline":52428800,"support_files_api":true},"context_window_options":[512000,1000000],"context_window_option_hints":{"1000000":"higher_usage"}},"MiniMax-M3.1-Flash":{"name":"M3.1-Flash\uff08\u8ba2\u9605\u9650\u514d\uff09","attachment":true,"reasoning":true,"temperature":true,"tool_call":true,"limit":{"context":512000,"output":128000},"modalities":{"input":["text","image","video"],"output":["text"]},"options":{"reasoningSummary":"auto"},"variants":{"none-thinking":{"thinking":{"type":"disabled"}},"thinking":{"thinking":{"type":"adaptive"}}},"thinking_config":{"mode":"forced_on"},"capabilities":{"files_api_upload_endpoint":"/v1/files/upload","max_attachments_count":4,"max_image_bytes_inline":10485760,"max_request_body_bytes":67108864,"max_video_bytes_inline":52428800,"support_files_api":true},"context_window_options":[512000,1000000],"context_window_option_hints":{"1000000":"higher_usage"},"effort_options":["default","low","medium","high","xhigh","max"],"default_effort":"default"}},"model_order":["MiniMax-M3.1-Flash","MiniMax-M3"],"whitelist":["MiniMax-M2.7-highspeed","MiniMax-M2.7","MiniMax-M3","MiniMax-M3.1-Flash"],"options":{"apiKey":"sk-xxx","baseURL":"https://agent.minimaxi.com/mavis/api/v1/llm/v1"}}}]}`

func idsOf(models []core.Model) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		out = append(out, m.ID)
	}
	return out
}

func TestRefreshModelsUsesOnlyWhatTheVendorReturns(t *testing.T) {
	c, ft := newTestClient(t, map[string]any{
		"config_yaml": filepath.Join(t.TempDir(), "missing.yaml"),
	}, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/models") {
			t.Errorf("catalogue request = %s %s, want GET .../models", r.Method, r.URL.Path)
		}
		return jsonResponse(http.StatusOK, vendorCatalogueReply), nil
	})

	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if ft.count() != 1 {
		t.Fatalf("upstream requests = %d, want 1", ft.count())
	}
	got := idsOf(models)
	want := []string{"MiniMax-M3.1-Flash", "MiniMax-M3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("catalogue = %v, want %v (the vendor only defines these two)", got, want)
	}
	for _, m := range models {
		if m.Extra["source"] != "upstream" {
			t.Errorf("%s source = %v, want upstream", m.ID, m.Extra["source"])
		}
	}
}

func TestTheWhitelistDoesNotResurrectIdsTheVendorNoLongerDefines(t *testing.T) {
	c, _ := newTestClient(t, map[string]any{
		"config_yaml": filepath.Join(t.TempDir(), "missing.yaml"),
	}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, vendorCatalogueReply), nil
	})
	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	for _, m := range models {
		if strings.Contains(m.ID, "M2.7") {
			t.Fatalf("%s is only in the whitelist, not in models; it must not be served", m.ID)
		}
	}
}

func TestRefreshModelsFallsBackToTheLocalTableWhenTheVendorIsUnreachable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, path, configYAMLFixture)
	c, _ := newTestClient(t, map[string]any{"config_yaml": path}, func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusServiceUnavailable, `{"error":"unavailable"}`), nil
	})

	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("a failed refresh must report the error alongside the fallback")
	}
	if len(models) != 4 {
		t.Fatalf("fallback = %v, want the 4 ids from config.yaml", idsOf(models))
	}
}

func TestParseUpstreamCatalogueRejectsAMissingProvider(t *testing.T) {
	_, err := parseUpstreamCatalogue([]byte(`{"providers":[]}`))
	if err == nil {
		t.Fatal("a catalogue with no providers must fail instead of being indexed blindly")
	}
}

func TestModelsURLFollowsTheConfiguredBase(t *testing.T) {
	cases := map[string]string{
		"https://agent.minimax.cn/mavis/api/v1/llm":    "https://agent.minimax.cn/mavis/api/v1/models",
		"https://agent.minimax.cn/mavis/api/v1/llm/v1": "https://agent.minimax.cn/mavis/api/v1/models",
		"https://agent.minimaxi.com/mavis/api/v1/llm":  "https://agent.minimaxi.com/mavis/api/v1/models",
	}
	for base, want := range cases {
		if got := modelsURL(base); got != want {
			t.Errorf("modelsURL(%q) = %q, want %q", base, got, want)
		}
	}
}
