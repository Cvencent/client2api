package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
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

func TestSourceHandlerPersistsBalanceProtocol(t *testing.T) {
	path := configFile(t, `{}`)
	p := configPanel(path)
	p.opts.Registry = core.NewRegistry()
	p.opts.Reload = func() error { return nil }
	w := httptest.NewRecorder()
	body := `{"base_url":"https://relay.example/v1","balance_protocol":"newapi","balance_quota_per_usd":1000000}`
	p.handleSource(w, httptest.NewRequest(http.MethodPut, "/panel/api/sources/relay", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	src := onDisk(t, path)["sources"].(map[string]any)["relay"].(map[string]any)
	if src["balance_protocol"] != "newapi" || src["balance_quota_per_usd"] != float64(1000000) {
		t.Fatalf("balance config was not persisted: %v", src)
	}
}

func TestSourceBalanceEditorIsWired(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		`id="sourceBalanceProtocol"`,
		`<option value="auto">自动识别</option>`,
		`<option value="newapi">New API</option>`,
		`<option value="sub2api">Sub2API</option>`,
		`<option value="openai_billing">OpenAI Billing</option>`,
		`<option value="none">不查询</option>`,
		`id="sourceBalanceQuota"`,
		`balance_protocol: $("#sourceBalanceProtocol").value`,
		`balance_quota_per_usd: Number($("#sourceBalanceQuota").value)`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("source balance editor is missing %q", want)
		}
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
