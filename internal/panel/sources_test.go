package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
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

func TestSourceHandlerPersistsRelayLimitsAndCooldowns(t *testing.T) {
	path := configFile(t, `{}`)
	p := configPanel(path)
	p.opts.Registry = core.NewRegistry()
	p.opts.Reload = func() error { return nil }
	w := httptest.NewRecorder()
	body := `{
		"base_url":"https://relay.example/v1",
		"requests_per_minute":120,
		"requests_per_minute_per_account":30,
		"max_in_flight":0,
		"max_in_flight_per_account":1,
		"cooldown":"15s",
		"rate_limit_cooldown":"0s",
		"quota_cooldown":"5m",
		"auth_cooldown":"6m",
		"upstream_cooldown":"10s"
	}`
	p.handleSource(w, httptest.NewRequest(http.MethodPut, "/panel/api/sources/relay", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	src := onDisk(t, path)["sources"].(map[string]any)["relay"].(map[string]any)
	for key, want := range map[string]any{
		"requests_per_minute":             float64(120),
		"requests_per_minute_per_account": float64(30),
		"max_in_flight":                   float64(0),
		"max_in_flight_per_account":       float64(1),
		"cooldown":                        "15s",
		"rate_limit_cooldown":             "0s",
		"quota_cooldown":                  "5m",
		"auth_cooldown":                   "6m",
		"upstream_cooldown":               "10s",
	} {
		if got, ok := src[key]; !ok || got != want {
			t.Errorf("persisted %s = %#v (present=%v), want %#v", key, got, ok, want)
		}
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

func TestSourceLimitsEditorIsWired(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		`id="sourceRequestLimits"`,
		`id="sourceRequestLimitsEmpty"`,
		`id="btnAddSourceLimit"`,
		`function sourceLimitConfig(cfg)`,
		`function sourceLimitWindowParts(window)`,
		`function sourceLimitWindow(every, unit)`,
		`function addSourceLimitRow(rule)`,
		`function renderSourceLimits(rules)`,
		`function readSourceLimits()`,
		`renderSourceLimits(sourceLimitConfig(cfg));`,
		`cfg.request_limits = readSourceLimits();`,
		`id="sourceMaxInFlight"`,
		`id="sourceAccountMaxInFlight"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("source limits editor is missing %q", want)
		}
	}
	if strings.Contains(src, `id="sourceRPM"`) || strings.Contains(src, `id="sourceAccountRPM"`) {
		t.Error("legacy RPM inputs are still rendered; rules should be the only request-count editor")
	}
	start := strings.Index(src, `<form class="dlg" id="sourceForm" role="dialog" aria-modal="true" aria-labelledby="sourceTitle">`)
	if start < 0 {
		t.Fatal("source form not found")
	}
	form := src[start:]
	if end := strings.Index(form, "</form>"); end >= 0 {
		form = form[:end]
	}
	if strings.Contains(form, "<textarea") || strings.Contains(form, "sourceLimitJSON") {
		t.Error("source form exposes a JSON/textarea editor; it must be form controls only")
	}
}

func TestSourceHandlerPersistsRequestLimits(t *testing.T) {
	path := configFile(t, `{}`)
	p := configPanel(path)
	p.opts.Registry = core.NewRegistry()
	p.opts.Reload = func() error { return nil }
	w := httptest.NewRecorder()
	body := `{
		"base_url":"https://relay.example/v1",
		"request_limits":[
			{"scope":"account","limit":100,"window":"24h","reset":"rolling"},
			{"scope":"source","limit":500,"window":"24h","reset":"calendar","timezone":"Asia/Shanghai","reset_at":"08:00"}
		]
	}`
	p.handleSource(w, httptest.NewRequest(http.MethodPut, "/panel/api/sources/relay", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	src := onDisk(t, path)["sources"].(map[string]any)["relay"].(map[string]any)
	limits, ok := src["request_limits"].([]any)
	if !ok || len(limits) != 2 {
		t.Fatalf("request_limits was not persisted: %#v", src["request_limits"])
	}
	second := limits[1].(map[string]any)
	if second["timezone"] != "Asia/Shanghai" || second["reset_at"] != "08:00" {
		t.Fatalf("calendar rule was not persisted: %#v", second)
	}
}

func TestSourceLimitsMigrateLegacyRPM(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "sourceLimitConfig")
	for _, want := range []string{
		`cfg.request_limits`,
		`cfg.requests_per_minute`,
		`cfg.requests_per_minute_per_account`,
		`scope: "source"`,
		`scope: "account"`,
		`reset: "first"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("legacy RPM migration is missing %q", want)
		}
	}
}

func TestSourceLimitsWindowAndMigration(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH")
	}
	src := poolStatsUISource(t)
	code, _ := json.Marshal(
		poolStatsFuncBody(t, src, "sourceLimitWindowParts") + "\n}" +
			poolStatsFuncBody(t, src, "sourceLimitWindow") + "\n}" +
			poolStatsFuncBody(t, src, "sourceLimitConfig") + "\n}")
	program := `
const assert=require("assert");
const vm=require("vm");
const ctx={}; vm.createContext(ctx);
vm.runInContext(` + string(code) + `+"\nthis.config=sourceLimitConfig;this.window=sourceLimitWindow;", ctx);
const legacy=ctx.config({requests_per_minute:120, requests_per_minute_per_account:30});
assert.strictEqual(legacy.length, 2);
assert.deepStrictEqual(JSON.parse(JSON.stringify(legacy[0])), {scope:"source", limit:120, every:1, unit:"m", reset:"first", timezone:"Asia/Shanghai", reset_at:"00:00"});
assert.deepStrictEqual(JSON.parse(JSON.stringify(legacy[1])), {scope:"account", limit:30, every:1, unit:"m", reset:"first", timezone:"Asia/Shanghai", reset_at:"00:00"});
const current=ctx.config({request_limits:[{scope:"account", limit:100, window:"24h", reset:"rolling"}]});
assert.strictEqual(current.length, 1);
assert.strictEqual(current[0].every, 24);
assert.strictEqual(current[0].unit, "h");
assert.strictEqual(ctx.window(2, "d"), "48h");
assert.strictEqual(ctx.window(90, "m"), "90m");
const compound=ctx.config({request_limits:[{scope:"account", limit:1, window:"1h30m"}]})[0];
assert.strictEqual(compound.every, 90);
assert.strictEqual(compound.unit, "m");
`
	if out, err := exec.Command(node, "-e", program).CombinedOutput(); err != nil {
		t.Fatalf("source limit JS behavior: %v\n%s", err, out)
	}
}

func TestSourceLimitsEditorPreservesExplicitZeroInFlight(t *testing.T) {
	path := configFile(t, `{}`)
	p := configPanel(path)
	p.opts.Registry = core.NewRegistry()
	p.opts.Reload = func() error { return nil }
	w := httptest.NewRecorder()
	body := `{"base_url":"https://relay.example/v1","max_in_flight":0,"request_limits":[{"scope":"account","limit":1,"window":"1h"}]}`
	p.handleSource(w, httptest.NewRequest(http.MethodPut, "/panel/api/sources/relay", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	src := onDisk(t, path)["sources"].(map[string]any)["relay"].(map[string]any)
	if got, ok := src["max_in_flight"]; !ok || got != float64(0) {
		t.Fatalf("max_in_flight = %#v (present=%v), want explicit zero", got, ok)
	}
}

func TestSourceHandlerPersistsThePerSourceProxy(t *testing.T) {
	path := configFile(t, `{}`)
	p := configPanel(path)
	p.opts.Registry = core.NewRegistry()
	p.opts.Reload = func() error { return nil }
	w := httptest.NewRecorder()
	body := `{"base_url":"https://relay.example/v1","proxy":"socks5://user:pass@127.0.0.1:1080"}`
	p.handleSource(w, httptest.NewRequest(http.MethodPut, "/panel/api/sources/relay", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	src := onDisk(t, path)["sources"].(map[string]any)["relay"].(map[string]any)
	if src["proxy"] != "socks5://user:pass@127.0.0.1:1080" {
		t.Fatalf("proxy was not persisted: %v", src)
	}
}

func TestSourceProxyEditorIsWired(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		`id="sourceProxy"`,
		`$("#sourceProxy").value = cfg.proxy || "";`,
		`proxy: $("#sourceProxy").value.trim(),`,
		`function sourceProxyHost(raw) {`,
		`SOURCE_DEFS[n].proxy ? ' · 代理 '`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("source proxy editor is missing %q", want)
		}
	}
}
func TestSourceHandlerPersistsTheUserAgent(t *testing.T) {
	path := configFile(t, `{}`)
	p := configPanel(path)
	p.opts.Registry = core.NewRegistry()
	p.opts.Reload = func() error { return nil }
	w := httptest.NewRecorder()
	body := `{"base_url":"https://relay.example/v1","user_agent":"codex_cli_rs/0.0.1 (client2api)"}`
	p.handleSource(w, httptest.NewRequest(http.MethodPut, "/panel/api/sources/relay", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	src := onDisk(t, path)["sources"].(map[string]any)["relay"].(map[string]any)
	if src["user_agent"] != "codex_cli_rs/0.0.1 (client2api)" {
		t.Fatalf("user_agent was not persisted: %v", src)
	}
}

func TestSourceUserAgentEditorIsWired(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		`id="sourceUserAgent"`,
		`$("#sourceUserAgent").value = cfg.user_agent || "";`,
		`user_agent: $("#sourceUserAgent").value.trim(),`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("source user-agent editor is missing %q", want)
		}
	}
}

func TestSourceHandlerPersistsTheAPIFormat(t *testing.T) {
	path := configFile(t, `{}`)
	p := configPanel(path)
	p.opts.Registry = core.NewRegistry()
	p.opts.Reload = func() error { return nil }
	w := httptest.NewRecorder()
	body := `{"base_url":"https://relay.example/v1","api_format":"responses"}`
	p.handleSource(w, httptest.NewRequest(http.MethodPut, "/panel/api/sources/relay", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	src := onDisk(t, path)["sources"].(map[string]any)["relay"].(map[string]any)
	if src["api_format"] != "responses" {
		t.Fatalf("api_format was not persisted: %v", src)
	}
	w = httptest.NewRecorder()
	body = `{"base_url":"https://relay.example/v1","api_format":"bogus"}`
	p.handleSource(w, httptest.NewRequest(http.MethodPut, "/panel/api/sources/relay", strings.NewReader(body)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bogus api_format: %d %s", w.Code, w.Body.String())
	}
}

func TestSourceAPIFormatEditorIsWired(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		`id="sourceAPIFormat"`,
		`$("#sourceAPIFormat").value = cfg.api_format || "";`,
		`api_format: $("#sourceAPIFormat").value,`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("source api-format editor is missing %q", want)
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
