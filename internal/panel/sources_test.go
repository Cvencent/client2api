package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSourceConfigValidationAndHotSave(t *testing.T) {
	p := configPanel(configFile(t, `{"listen":"127.0.0.1:0"}`))
	for _, body := range []string{
		`{"sources":{"../evil":{"base_url":"https://relay.example/v1"}}}`,
		`{"sources":{"relay":{"base_url":"https://secret@relay.example/v1"}}}`,
		`{"sources":{"relay":{"base_url":"https://relay.example/v1","api_key":"should-not-be-in-config"}}}`,
	} {
		w, _ := doConfig(t, p, http.MethodPatch, body)
		if w.Code != 400 {
			t.Fatalf("accepted %s: %d %s", body, w.Code, w.Body.String())
		}
	}
	out := mustSave(t, p, `{"sources":{"my-auth":{"label":"自定义","base_url":"https://relay.example/v1","max_tokens_field":"max_completion_tokens"}}}`)
	if out["restart_required"] != false {
		t.Fatal("source edit requested restart")
	}
	cfg := out["config"].(map[string]any)
	source := cfg["sources"].(map[string]any)["my-auth"].(map[string]any)
	if source["max_tokens_field"] != "max_completion_tokens" || source["label"] != "自定义" {
		t.Fatalf("source definition incorrectly redacted: %v", source)
	}
}

func TestSourceHandlerRequiresHotReloadBeforeWriting(t *testing.T) {
	p := configPanel(configFile(t, `{}`))
	w := httptest.NewRecorder()
	p.handleSource(w, httptest.NewRequest(http.MethodPut, "/panel/api/sources/my-relay", strings.NewReader(`{"base_url":"https://relay.example/v1"}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no reload: %d %s", w.Code, w.Body.String())
	}
	if _, ok := onDisk(t, p.opts.ConfigPath)["sources"]; ok {
		t.Fatal("wrote source with no live runtime")
	}
}

func TestSourceRedactionMasksUnexpectedCredentials(t *testing.T) {
	cfg := map[string]any{"sources": map[string]any{"my-auth": map[string]any{"api_key": "plainopaqueabc123", "max_tokens_field": "max_tokens", "base_url": "https://relay.example/v1"}}}
	out := redactConfig(cfg).(map[string]any)["sources"].(map[string]any)["my-auth"].(map[string]any)
	if out["api_key"] != "<redacted>" {
		t.Fatal("source definition leaked an unexpected credential")
	}
	if out["max_tokens_field"] != "max_tokens" {
		t.Fatal("non-secret token field was masked")
	}
}
