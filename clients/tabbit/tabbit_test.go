package tabbit

// Offline test suite.  Nothing here needs a network, Node.js, Playwright or a
// real tabbit2api installation: upstream answers are recorded fixtures served
// by httptest.Server or by a fake http.RoundTripper, and every credential or
// state file lives in a t.TempDir().  Live checks belong in tabbit_live_test.go
// behind CLIENT2API_LIVE=1.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func sseResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// closedPortURL returns an http URL on a loopback port that nothing listens on,
// so the offline suite never touches a real sidecar even if one is running.
func closedPortURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return "http://" + addr
}

func withCandidates(t *testing.T, urls ...string) {
	t.Helper()
	old := defaultCandidates
	defaultCandidates = urls
	t.Cleanup(func() { defaultCandidates = old })
}

func clearTabbitEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"CLIENT2API_TABBIT_BASE_URL", "TABBIT_BASE_URL",
		"CLIENT2API_TABBIT_API_KEY", "TABBIT_API_KEY",
		"CLIENT2API_TABBIT_CMD", "TABBIT_SIDECAR_CMD",
	} {
		t.Setenv(k, "")
	}
}

func newTestClient(t *testing.T, cfgJSON string, hc *http.Client) *Client {
	t.Helper()
	deps := core.Deps{
		DataDir:    t.TempDir(),
		HTTPClient: hc,
		Logf:       func(string, ...any) {},
	}
	if strings.TrimSpace(cfgJSON) != "" {
		deps.Config = json.RawMessage(cfgJSON)
	}
	c, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tc, ok := c.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", c)
	}
	return tc
}

func writeStateFile(t *testing.T, c *Client, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(c.deps.DataDir, stateFileName), []byte(body), 0o600); err != nil {
		t.Fatalf("writing state file: %v", err)
	}
}

func collectEvents(t *testing.T, st core.Stream) []core.Event {
	t.Helper()
	var out []core.Event
	for i := 0; i < 200; i++ {
		ev, err := st.Recv()
		if errors.Is(err, io.EOF) {
			if _, err2 := st.Recv(); !errors.Is(err2, io.EOF) {
				t.Fatalf("Recv after io.EOF = %v, want io.EOF", err2)
			}
			return out
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if ev.Type == "" {
			t.Fatalf("event %d has an empty type", i)
		}
		out = append(out, ev)
	}
	t.Fatalf("stream produced 200 events without ending: %+v", out)
	return nil
}

func deltas(events []core.Event) (text []string, reasoning []string) {
	for _, ev := range events {
		if ev.Type != core.EventDelta {
			continue
		}
		if ev.Delta != "" {
			text = append(text, ev.Delta)
		}
		if ev.Reasoning != "" {
			reasoning = append(reasoning, ev.Reasoning)
		}
	}
	return text, reasoning
}

func countType(events []core.Event, typ core.EventType) int {
	n := 0
	for _, ev := range events {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Recorded upstream fixtures
// ---------------------------------------------------------------------------

// sseFixture is a recorded /v1/chat/completions stream in the shape the
// reference sidecar emits: OpenAI chunks, a reasoning delta, a tool call split
// across two frames, a trailing usage-only chunk and a [DONE] sentinel.
const sseFixture = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"tabbit/priority","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"let me think"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":5},"completion_tokens_details":{"reasoning_tokens":3}}}

data: [DONE]

`

// sseErrorFixture is a stream that fails after a partial answer, the way the
// sidecar reports model_unavailable.
const sseErrorFixture = `data: {"id":"c2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}

event: error
data: {"error":{"message":"model_unavailable: welcome banner detected","type":"upstream_error"}}

data: [DONE]

`

const modelsFixture = `{"object":"list","data":[
 {"id":"tabbit/priority","object":"model","owned_by":"tabbit","tabbit_display_name":"Priority","supports_tools":true},
 {"id":"tabbit/GPT-5.5","object":"model","owned_by":"tabbit","supports_images":true},
 {"id":"tabbit/Default","object":"model","owned_by":"tabbit"}
]}`

const completionFixture = `{"id":"c3","object":"chat.completion","model":"tabbit/GPT-5.5","choices":[{"index":0,"message":{"role":"assistant","content":"Hi there","reasoning_content":"because"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr bool
		check   func(t *testing.T, cfg Config)
	}{
		{name: "absent", raw: ""},
		{name: "null", raw: "null"},
		{name: "empty object", raw: "{}"},
		{
			name: "full object",
			raw:  `{"base_url":"http://127.0.0.1:50124","api_key":"k","manage":true,"command":"node","args":["cli.js"],"web_host":"web.tabbit.com","extra_models":["x"],"include_usage":false,"timeouts":{"status_seconds":0.5}}`,
			check: func(t *testing.T, cfg Config) {
				if cfg.BaseURL != "http://127.0.0.1:50124" || cfg.APIKey != "k" || !cfg.Manage {
					t.Fatalf("unexpected config: %+v", cfg)
				}
				if cfg.Command != "node" || len(cfg.Args) != 1 || cfg.WebHost != "web.tabbit.com" {
					t.Fatalf("unexpected command/host: %+v", cfg)
				}
				if cfg.includeUsage() {
					t.Fatal("include_usage=false must disable the usage chunk")
				}
				if got := cfg.Timeouts.statusBudget(); got != 500*time.Millisecond {
					t.Fatalf("statusBudget = %v", got)
				}
			},
		},
		{name: "invalid", raw: `{"base_url":`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseConfig(json.RawMessage(tt.raw))
			if tt.wantErr {
				if err == nil {
					t.Fatal("want an error")
				}
				if !strings.Contains(err.Error(), "clients.tabbit") {
					t.Fatalf("error should name the config key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseConfig: %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

func TestConfigNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   Config
		want Config
	}{
		{
			name: "defaults",
			in:   Config{},
			want: Config{ModelPrefix: "tabbit/", HealthPath: "/health", ModelsPath: "/v1/models", ChatPath: "/v1/chat/completions"},
		},
		{
			name: "trims and repairs",
			in:   Config{BaseURL: " http://127.0.0.1:50124/ ", ModelPrefix: "tabbit", APIKey: " k ", Command: " node "},
			want: Config{BaseURL: "http://127.0.0.1:50124", ModelPrefix: "tabbit/", APIKey: "k", Command: "node", HealthPath: "/health", ModelsPath: "/v1/models", ChatPath: "/v1/chat/completions"},
		},
		{
			name: "keeps explicit paths",
			in:   Config{HealthPath: "/healthz", ModelsPath: "/models", ChatPath: "/chat"},
			want: Config{ModelPrefix: "tabbit/", HealthPath: "/healthz", ModelsPath: "/models", ChatPath: "/chat"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in.normalize()
			if got.BaseURL != tt.want.BaseURL || got.ModelPrefix != tt.want.ModelPrefix ||
				got.HealthPath != tt.want.HealthPath || got.ModelsPath != tt.want.ModelsPath ||
				got.ChatPath != tt.want.ChatPath || got.APIKey != tt.want.APIKey || got.Command != tt.want.Command {
				t.Fatalf("normalize = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestModelPrefixRoundTrip(t *testing.T) {
	tests := []struct {
		id     string
		prefix string
		want   string
	}{
		{"priority", "tabbit/", "tabbit/priority"},
		{"tabbit/priority", "tabbit/", "tabbit/priority"},
		{"GPT-5.5", "", "GPT-5.5"},
		{"", "tabbit/", ""},
		{"  priority  ", "tabbit/", "tabbit/priority"},
	}
	for _, tt := range tests {
		if got := addModelPrefix(tt.id, tt.prefix); got != tt.want {
			t.Errorf("addModelPrefix(%q, %q) = %q, want %q", tt.id, tt.prefix, got, tt.want)
		}
		stripped := stripModelPrefix(tt.want, tt.prefix)
		wantStripped := strings.TrimSpace(tt.id)
		if tt.prefix != "" {
			wantStripped = strings.TrimPrefix(wantStripped, tt.prefix)
		}
		if stripped != wantStripped {
			t.Errorf("stripModelPrefix(%q, %q) = %q, want %q", tt.want, tt.prefix, stripped, wantStripped)
		}
		if tt.id != "" && addModelPrefix(stripModelPrefix(addModelPrefix(tt.id, tt.prefix), tt.prefix), tt.prefix) != tt.want {
			t.Errorf("round trip of %q is not stable", tt.id)
		}
	}
}

// ---------------------------------------------------------------------------
// Discovery
// ---------------------------------------------------------------------------

func TestLocatePrecedence(t *testing.T) {
	t.Run("config wins over env and state", func(t *testing.T) {
		clearTabbitEnv(t)
		t.Setenv("CLIENT2API_TABBIT_BASE_URL", "http://env:1111")
		t.Setenv("CLIENT2API_TABBIT_API_KEY", "envkey")
		c := newTestClient(t, `{"base_url":"http://config:2222","api_key":"cfgkey"}`, nil)
		writeStateFile(t, c, `{"base_url":"http://state:3333","api_key":"statekey"}`)
		loc := c.locate()
		if loc.baseURL != "http://config:2222" || loc.source != "config" {
			t.Fatalf("base url = %q (%s), want the config one", loc.baseURL, loc.source)
		}
		if loc.apiKey != "cfgkey" || loc.keySource != "config" {
			t.Fatalf("api key source = %s", loc.keySource)
		}
		if !loc.explicit() {
			t.Fatal("a configured base_url is explicit")
		}
	})

	t.Run("env wins over state", func(t *testing.T) {
		clearTabbitEnv(t)
		t.Setenv("CLIENT2API_TABBIT_BASE_URL", "http://env:1111")
		c := newTestClient(t, "", nil)
		writeStateFile(t, c, `{"base_url":"http://state:3333"}`)
		loc := c.locate()
		if loc.baseURL != "http://env:1111" || loc.source != "env" {
			t.Fatalf("base url = %q (%s), want the environment one", loc.baseURL, loc.source)
		}
	})

	t.Run("state file wins over the default candidate", func(t *testing.T) {
		clearTabbitEnv(t)
		withCandidates(t, "http://127.0.0.1:50124")
		c := newTestClient(t, "", nil)
		writeStateFile(t, c, `{"base_url":"http://state:3333/","api_key":"statekey","command":"statecmd","args":["--port","1"]}`)
		loc := c.locate()
		if loc.baseURL != "http://state:3333" || loc.source != "state" {
			t.Fatalf("base url = %q (%s)", loc.baseURL, loc.source)
		}
		if loc.apiKey != "statekey" || loc.keySource != "state" {
			t.Fatalf("api key = %q from %s", loc.apiKey, loc.keySource)
		}
		if loc.command != "statecmd" || len(loc.args) != 2 {
			t.Fatalf("command = %q %v", loc.command, loc.args)
		}
	})

	t.Run("defaults when nothing is configured", func(t *testing.T) {
		clearTabbitEnv(t)
		withCandidates(t, "http://127.0.0.1:50124")
		c := newTestClient(t, "", nil)
		loc := c.locate()
		if loc.baseURL != "http://127.0.0.1:50124" || loc.source != "default" {
			t.Fatalf("base url = %q (%s)", loc.baseURL, loc.source)
		}
		if loc.apiKey != defaultAPIKey || loc.keySource != "builtin" {
			t.Fatalf("api key = %q from %s", loc.apiKey, loc.keySource)
		}
		if loc.explicit() {
			t.Fatal("a built-in default must not count as configured")
		}
	})

	t.Run("env api key and command", func(t *testing.T) {
		clearTabbitEnv(t)
		t.Setenv("TABBIT_BASE_URL", "http://env:1111")
		t.Setenv("TABBIT_API_KEY", "envkey")
		t.Setenv("TABBIT_SIDECAR_CMD", "sidecar-from-env")
		c := newTestClient(t, "", nil)
		loc := c.locate()
		if loc.apiKey != "envkey" || loc.keySource != "env" {
			t.Fatalf("api key = %q from %s", loc.apiKey, loc.keySource)
		}
		if loc.command != "sidecar-from-env" {
			t.Fatalf("command = %q", loc.command)
		}
	})

	t.Run("config command and manage flag", func(t *testing.T) {
		clearTabbitEnv(t)
		c := newTestClient(t, `{"command":"node","args":["cli.js"],"workdir":"C:\\tmp","manage":true}`, nil)
		loc := c.locate()
		if loc.command != "node" || len(loc.args) != 1 || loc.workdir != `C:\tmp` {
			t.Fatalf("command = %q %v in %q", loc.command, loc.args, loc.workdir)
		}
		if !loc.manage {
			t.Fatal("manage must come from the config")
		}
	})
}

// ---------------------------------------------------------------------------
// Health and model list decoding
// ---------------------------------------------------------------------------

func TestParseHealth(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		version string
		models  int
		host    string
	}{
		{name: "documented shape", body: `{"status":"ok","version":"0.1.9","models":9,"web_host":"web.tabbit.ai"}`, version: "0.1.9", models: 9, host: "web.tabbit.ai"},
		{name: "ok flag", body: `{"ok":true,"model_count":3}`, models: 3},
		{name: "model array", body: `{"models":["a","b","c","d"]}`, models: 4},
		{name: "unknown shape", body: `{"whatever":1}`},
		{name: "not json", body: "OK"},
		{name: "empty", body: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHealth([]byte(tt.body))
			if got.Version != tt.version || got.Models != tt.models || got.Host != tt.host {
				t.Fatalf("parseHealth(%q) = %+v", tt.body, got)
			}
		})
	}
}

func TestParseModels(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    []string
		wantErr bool
	}{
		{name: "openai data shape", body: modelsFixture, want: []string{"priority", "GPT-5.5", "Default"}},
		{name: "models envelope", body: `{"models":[{"id":"tabbit/GLM-5.1"},{"id":"tabbit/Default"}]}`, want: []string{"GLM-5.1", "Default"}},
		{name: "bare strings", body: `["tabbit/priority","tabbit/GPT-5.4"]`, want: []string{"priority", "GPT-5.4"}},
		{name: "bare objects", body: `[{"id":"tabbit/DeepSeek-V4-Pro","owned_by":"tabbit"}]`, want: []string{"DeepSeek-V4-Pro"}},
		{name: "duplicates and blanks", body: `{"data":[{"id":"tabbit/Default"},{"id":"tabbit/Default"},{"id":""},"tabbit/GPT-5.5"]}`, want: []string{"Default", "GPT-5.5"}},
		{name: "empty list", body: `{"data":[]}`, wantErr: true},
		{name: "blank body", body: "", wantErr: true},
		{name: "invalid json", body: `{`, wantErr: true},
		{name: "no usable ids", body: `{"data":[{"object":"model"}]}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseModels([]byte(tt.body), defaultModelPrefix)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseModels: %v", err)
			}
			var ids []string
			for _, m := range got {
				ids = append(ids, m.ID)
				if strings.HasPrefix(m.ID, defaultModelPrefix) {
					t.Fatalf("id %q kept the prefix", m.ID)
				}
			}
			if strings.Join(ids, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("ids = %v, want %v", ids, tt.want)
			}
		})
	}
}

func TestParseModelsKeepsMetadata(t *testing.T) {
	got, err := parseModels([]byte(modelsFixture), defaultModelPrefix)
	if err != nil {
		t.Fatalf("parseModels: %v", err)
	}
	if got[0].OwnedBy != "tabbit" {
		t.Fatalf("owned_by = %q", got[0].OwnedBy)
	}
	if got[0].Extra["tabbit_display_name"] != "Priority" || got[0].Extra["supports_tools"] != true {
		t.Fatalf("metadata lost: %+v", got[0].Extra)
	}
	if _, ok := got[0].Extra["id"]; ok {
		t.Fatal("the id must not be duplicated into Extra")
	}
}

func TestFallbackModels(t *testing.T) {
	cfg := Config{ModelPrefix: defaultModelPrefix, ExtraModels: []string{"tabbit/Custom-One", "custom-one"}}.normalize()
	got := fallbackModels(cfg)
	if len(got) != len(builtinCatalog)+1 {
		t.Fatalf("fallback has %d models, want %d", len(got), len(builtinCatalog)+1)
	}
	if got[0].ID != builtinCatalog[0].ID {
		t.Fatalf("first fallback id = %q, want %q", got[0].ID, builtinCatalog[0].ID)
	}
	if got[0].Extra["fallback"] != true {
		t.Fatal("fallback entries should be marked")
	}
	for _, m := range got {
		if strings.HasPrefix(m.ID, defaultModelPrefix) {
			t.Fatalf("fallback id %q kept the prefix (the gateway adds it)", m.ID)
		}
	}
	if got[len(got)-1].ID != "Custom-One" {
		t.Fatalf("extra model = %q", got[len(got)-1].ID)
	}
}

// ---------------------------------------------------------------------------
// Models / Chat against a fake or recorded upstream
// ---------------------------------------------------------------------------

func TestModelsFallsBackWhenUnreachable(t *testing.T) {
	clearTabbitEnv(t)
	withCandidates(t, closedPortURL(t))
	c := newTestClient(t, "", nil)

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models must not fail when the sidecar is absent: %v", err)
	}
	if len(models) != len(builtinCatalog) {
		t.Fatalf("got %d models, want the %d-model fallback catalog", len(models), len(builtinCatalog))
	}
	if models[0].ID != builtinCatalog[0].ID {
		t.Fatalf("first model = %q, want the first built-in entry", models[0].ID)
	}
}

func TestModelsFromSidecarAndCache(t *testing.T) {
	var modelCalls int32
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/models":
			atomic.AddInt32(&modelCalls, 1)
			return jsonResponse(200, modelsFixture), nil
		case "/health":
			return jsonResponse(200, `{"status":"ok"}`), nil
		}
		return jsonResponse(404, `{}`), nil
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, hc)

	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 3 || models[0].ID != "priority" {
		t.Fatalf("models = %+v", models)
	}
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatalf("Models (cached): %v", err)
	}
	if n := atomic.LoadInt32(&modelCalls); n != 1 {
		t.Fatalf("the catalog should be cached: %d upstream calls", n)
	}
}

// ---------------------------------------------------------------------------
// RefreshModels (the panel's "re-fetch from upstream" button)
// ---------------------------------------------------------------------------

func TestRefreshModelsUpdatesCatalogueAndBypassesCache(t *testing.T) {
	var modelCalls int32
	body := modelsFixture
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/models" {
			atomic.AddInt32(&modelCalls, 1)
			return jsonResponse(200, body), nil
		}
		return jsonResponse(404, `{}`), nil
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, hc)

	if _, err := c.Models(context.Background()); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if n := atomic.LoadInt32(&modelCalls); n != 1 {
		t.Fatalf("Models should fetch once, saw %d", n)
	}

	// The vendor's catalogue changes while our cache is still fresh.
	body = `{"object":"list","data":[{"id":"tabbit/GPT-5.5"},{"id":"tabbit/GLM-5.1"}]}`
	refreshed, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if n := atomic.LoadInt32(&modelCalls); n != 2 {
		t.Fatalf("RefreshModels must bypass the cache, saw %d upstream calls", n)
	}
	if len(refreshed) != 2 || refreshed[0].ID != "GPT-5.5" {
		t.Fatalf("refreshed = %+v", refreshed)
	}

	// The refreshed list replaced the cached one.
	models, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models after refresh: %v", err)
	}
	if n := atomic.LoadInt32(&modelCalls); n != 2 {
		t.Fatalf("Models should serve the refreshed cache, saw %d upstream calls", n)
	}
	if len(models) != 2 || models[0].ID != "GPT-5.5" {
		t.Fatalf("models = %+v", models)
	}
}

func TestRefreshModelsFailureKeepsLastGoodList(t *testing.T) {
	var fail int32
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/models" {
			if atomic.LoadInt32(&fail) == 1 {
				return jsonResponse(503, `{"error":{"message":"sidecar is restarting"}}`), nil
			}
			return jsonResponse(200, modelsFixture), nil
		}
		return jsonResponse(404, `{}`), nil
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, hc)

	good, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(good) != 3 {
		t.Fatalf("good = %+v", good)
	}

	atomic.StoreInt32(&fail, 1)
	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("a failed refresh must report an error")
	}
	// A failed refresh must never empty the catalogue.
	if len(models) != len(good) || models[0].ID != good[0].ID {
		t.Fatalf("a failed refresh must keep the last good list, got %+v", models)
	}
	if !strings.HasPrefix(err.Error(), "tabbit:") {
		t.Errorf("the error should be prefixed with the module name: %v", err)
	}
}

func TestRefreshModelsWithoutSidecarStillServesFallback(t *testing.T) {
	clearTabbitEnv(t)
	withCandidates(t, closedPortURL(t))
	c := newTestClient(t, "", nil)

	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("refreshing without a reachable sidecar must report an error")
	}
	if len(models) == 0 {
		t.Fatal("a failed refresh must still return a usable catalogue")
	}
	if models[0].ID != builtinCatalog[0].ID {
		t.Fatalf("first model = %q, want the built-in fallback", models[0].ID)
	}
}

func TestRefreshModelsErrorOmitsAPIKey(t *testing.T) {
	// A bare API key carries no label for core.Redact's patterns to key on, so
	// this also pins the explicit replacement in scrubSecret.
	const apiKey = "BARE-TABBIT-KEY-abcdefghijkl"
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/models" {
			return jsonResponse(401,
				`{"error":{"message":"credential `+apiKey+` rejected"}}`), nil
		}
		return jsonResponse(404, `{}`), nil
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test","api_key":"`+apiKey+`"}`, hc)

	models, err := c.RefreshModels(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if len(models) == 0 {
		t.Fatal("a failed refresh must still return a usable catalogue")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("the API key leaked into the error: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "tabbit:") {
		t.Errorf("the error should be prefixed with the module name: %v", err)
	}
}

func TestChatNoSidecarIsNotConfigured(t *testing.T) {
	clearTabbitEnv(t)
	withCandidates(t, closedPortURL(t))
	c := newTestClient(t, "", nil)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{Model: "priority"})
	if stream != nil {
		t.Fatal("no stream expected")
	}
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("Chat error = %v, want core.ErrNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "no tabbit2api sidecar") {
		t.Fatalf("error should explain what to do: %v", err)
	}
}

func TestChatWithNoCandidateAtAll(t *testing.T) {
	clearTabbitEnv(t)
	withCandidates(t)
	c := newTestClient(t, "", nil)
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "priority"}); !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("Chat error = %v, want core.ErrNotConfigured", err)
	}
}

func TestChatUnsupportedRequestNeverTouchesTheNetwork(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("unexpected upstream call to %s", r.URL)
		return nil, errors.New("unreachable")
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, hc)

	if _, err := c.Chat(context.Background(), nil); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("nil request = %v, want core.ErrUnsupported", err)
	}
	if _, err := c.Chat(context.Background(), &core.ChatRequest{}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("empty model = %v, want core.ErrUnsupported", err)
	}
}

func TestChatStreamReframing(t *testing.T) {
	// The recorded fixture is served by a real HTTP server so the SSE framing
	// (chunking, content type, connection reuse) is exercised too.
	var mu sync.Mutex
	var gotBody []byte
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody, gotAuth = body, r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, sseFixture)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)

	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"`+srv.URL+`","api_key":"sk-local-test"}`, nil)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "priority",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	events := collectEvents(t, stream)

	mu.Lock()
	body, auth := gotBody, gotAuth
	mu.Unlock()

	var req oaiChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if req.Model != "tabbit/priority" {
		t.Fatalf("upstream model = %q, want the prefixed id", req.Model)
	}
	if !req.Stream || req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Fatalf("the upstream call must stream with usage: %+v", req)
	}
	if auth != "Bearer sk-local-test" {
		t.Fatalf("authorization = %q", auth)
	}

	text, reasoning := deltas(events)
	if strings.Join(text, "") != "Hello world" {
		t.Fatalf("text deltas = %v", text)
	}
	if strings.Join(reasoning, "") != "let me think" {
		t.Fatalf("reasoning deltas = %v", reasoning)
	}

	var toolCalls []*core.ToolCallDelta
	for _, ev := range events {
		if ev.Type == core.EventToolCall {
			toolCalls = append(toolCalls, ev.ToolCall)
		}
	}
	if len(toolCalls) != 2 {
		t.Fatalf("tool call events = %d, want 2", len(toolCalls))
	}
	if toolCalls[0].Index != 0 || toolCalls[0].ID != "call_1" || toolCalls[0].Name != "get_weather" {
		t.Fatalf("first tool call = %+v", toolCalls[0])
	}
	args := toolCalls[0].Arguments + toolCalls[1].Arguments
	if args != `{"city":"Paris"}` {
		t.Fatalf("tool arguments = %q", args)
	}

	if n := countType(events, core.EventUsage); n != 1 {
		t.Fatalf("usage events = %d, want exactly 1", n)
	}
	if n := countType(events, core.EventDone); n != 1 {
		t.Fatalf("done events = %d, want exactly 1", n)
	}

	// Usage must arrive before done: the gateway stops reading at done.
	var usageIdx, doneIdx int = -1, -1
	for i, ev := range events {
		switch ev.Type {
		case core.EventUsage:
			usageIdx = i
			if ev.Usage == nil || ev.Usage.PromptTokens != 11 || ev.Usage.CompletionTokens != 7 ||
				ev.Usage.TotalTokens != 18 || ev.Usage.ReasoningTokens != 3 || ev.Usage.CachedTokens != 5 {
				t.Fatalf("usage = %+v", ev.Usage)
			}
		case core.EventDone:
			doneIdx = i
			if ev.Finish != "tool_calls" {
				t.Fatalf("finish reason = %q, want tool_calls", ev.Finish)
			}
		}
	}
	if usageIdx < 0 || doneIdx < 0 || usageIdx > doneIdx {
		t.Fatalf("usage must be emitted before done (usage=%d done=%d)", usageIdx, doneIdx)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
}

func TestChatStreamErrorFrame(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return sseResponse(sseErrorFixture), nil
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, hc)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{Model: "priority"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	events := collectEvents(t, stream)
	text, _ := deltas(events)
	if strings.Join(text, "") != "partial" {
		t.Fatalf("deltas = %v", text)
	}
	if n := countType(events, core.EventError); n != 1 {
		t.Fatalf("error events = %d, want 1", n)
	}
	if n := countType(events, core.EventDone); n != 0 {
		t.Fatalf("done events = %d, want none after an error", n)
	}
	for _, ev := range events {
		if ev.Type == core.EventError && !strings.Contains(ev.Err.Error(), "model_unavailable") {
			t.Fatalf("error event = %v", ev.Err)
		}
	}
}

func TestChatNonSSEJSONResponse(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, completionFixture), nil
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, hc)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{Model: "GPT-5.5"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	events := collectEvents(t, stream)
	text, reasoning := deltas(events)
	if strings.Join(text, "") != "Hi there" || strings.Join(reasoning, "") != "because" {
		t.Fatalf("deltas = %v / %v", text, reasoning)
	}
	for _, ev := range events {
		if ev.Type == core.EventDone && ev.Finish != "stop" {
			t.Fatalf("finish = %q", ev.Finish)
		}
		if ev.Type == core.EventUsage && ev.Usage.TotalTokens != 5 {
			t.Fatalf("usage = %+v", ev.Usage)
		}
	}
}

func TestChatNonSSEErrorEnvelope(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, `{"error":{"message":"[492] blocked by the tabbit frontend","type":"upstream_error"}}`), nil
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, hc)
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "priority"}); err == nil ||
		!strings.Contains(err.Error(), "[492]") {
		t.Fatalf("Chat error = %v, want the upstream message", err)
	}
}

func TestUpstreamErrorMapping(t *testing.T) {
	const secret = "sk-do-not-leak-1234567890"
	tests := []struct {
		name        string
		status      int
		body        string
		transport   error
		wantNotConf bool
		wantSubstr  string
	}{
		{name: "401", status: 401, body: `{"error":{"message":"invalid api key"}}`, wantNotConf: true, wantSubstr: "invalid api key"},
		{name: "403", status: 403, body: `{"message":"forbidden"}`, wantNotConf: true, wantSubstr: "forbidden"},
		{name: "429", status: 429, body: `{"error":{"message":"slow down"}}`, wantSubstr: "rate limiting"},
		{name: "500", status: 500, body: `{"error":{"message":"bridge exploded"}}`, wantSubstr: "bridge exploded"},
		{name: "502", status: 502, body: "", wantSubstr: "(empty body)"},
		{name: "transport", transport: errors.New("dial tcp 127.0.0.1:50124: connectex: connection refused"), wantNotConf: true, wantSubstr: "not reachable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				if tt.transport != nil {
					return nil, tt.transport
				}
				return jsonResponse(tt.status, tt.body), nil
			})}
			clearTabbitEnv(t)
			c := newTestClient(t, `{"base_url":"http://sidecar.test","api_key":"`+secret+`"}`, hc)
			stream, err := c.Chat(context.Background(), &core.ChatRequest{Model: "priority"})
			if stream != nil || err == nil {
				t.Fatalf("want an error, got stream=%v err=%v", stream, err)
			}
			if tt.wantNotConf && !errors.Is(err, core.ErrNotConfigured) {
				t.Fatalf("error = %v, want core.ErrNotConfigured", err)
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.wantSubstr)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("the api key leaked into the error: %v", err)
			}
		})
	}
}

func TestChatHonoursContextCancellation(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-release:
			return sseResponse(sseFixture), nil
		}
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, hc)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Chat(ctx, &core.ChatRequest{Model: "priority"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat error = %v, want context.Canceled", err)
	}
}

func TestConcurrentChatIsSafe(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return sseResponse(sseFixture), nil
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, hc)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream, err := c.Chat(context.Background(), &core.ChatRequest{Model: "priority"})
			if err != nil {
				t.Errorf("Chat: %v", err)
				return
			}
			defer stream.Close()
			for {
				if _, err := stream.Recv(); err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if st := c.Status(context.Background()); st.Name != "tabbit" {
		t.Fatalf("Status name = %q", st.Name)
	}
}

// ---------------------------------------------------------------------------
// Status
// ---------------------------------------------------------------------------

func TestStatusDegradedWhenNoSidecar(t *testing.T) {
	clearTabbitEnv(t)
	withCandidates(t, closedPortURL(t))
	c := newTestClient(t, "", nil)

	start := time.Now()
	st := c.Status(context.Background())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Status took %v, it must stay within its budget", elapsed)
	}
	if st.Name != "tabbit" || st.Ready {
		t.Fatalf("status = %+v, want not ready", st)
	}
	if !strings.Contains(st.Detail, "no sidecar configured") {
		t.Fatalf("detail = %q", st.Detail)
	}
	if strings.Count(st.Detail, "\n") != 0 {
		t.Fatalf("detail must be one line: %q", st.Detail)
	}
	if len(st.Accounts) != 1 || st.Accounts[0].State != "unknown" {
		t.Fatalf("accounts = %+v", st.Accounts)
	}
	if len(st.Models) != len(builtinCatalog) {
		t.Fatalf("models = %v", st.Models)
	}
}

func TestStatusNotReachableWhenConfigured(t *testing.T) {
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"`+closedPortURL(t)+`"}`, nil)
	st := c.Status(context.Background())
	if st.Ready {
		t.Fatal("a dead sidecar must not be ready")
	}
	if !strings.Contains(st.Detail, "not reachable") {
		t.Fatalf("detail = %q", st.Detail)
	}
}

func TestStatusReadyWhenHealthy(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/health" {
			return jsonResponse(200, `{"status":"ok","version":"0.1.9","models":9,"web_host":"web.tabbit.com"}`), nil
		}
		return jsonResponse(404, `{}`), nil
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test","web_host":"web.tabbit.ai"}`, hc)

	st := c.Status(context.Background())
	if !st.Ready {
		t.Fatalf("status = %+v, want ready", st)
	}
	for _, want := range []string{"ok", "version 0.1.9", "9 models", "web.tabbit.com"} {
		if !strings.Contains(st.Detail, want) {
			t.Fatalf("detail %q should mention %q", st.Detail, want)
		}
	}
	if len(st.Accounts) != 1 || st.Accounts[0].State != "ready" {
		t.Fatalf("accounts = %+v", st.Accounts)
	}
	if !strings.Contains(st.Accounts[0].Note, "builtin") {
		t.Fatalf("account note should name the key source: %q", st.Accounts[0].Note)
	}
}

func TestStatusReportsHealthFailure(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(500, `{"error":{"message":"bridge not ready"}}`), nil
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, hc)
	st := c.Status(context.Background())
	if st.Ready || !strings.Contains(st.Detail, "HTTP 500") {
		t.Fatalf("status = %+v", st)
	}
}

func TestStatusMarksRejectedKeyInvalid(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return jsonResponse(401, `{"error":{"message":"bad key"}}`), nil
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, hc)
	st := c.Status(context.Background())
	if len(st.Accounts) != 1 || st.Accounts[0].State != "invalid" {
		t.Fatalf("accounts = %+v", st.Accounts)
	}
}

func TestStatusInvalidConfig(t *testing.T) {
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":`, nil)
	st := c.Status(context.Background())
	if st.Ready || !strings.Contains(st.Detail, "invalid config") {
		t.Fatalf("status = %+v", st)
	}
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "priority"}); !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("Chat with a broken config = %v, want core.ErrNotConfigured", err)
	}
	models, err := c.Models(context.Background())
	if err != nil || len(models) != len(builtinCatalog) {
		t.Fatalf("Models = %v, %v", models, err)
	}
}

// ---------------------------------------------------------------------------
// Optional supervision ("manage": true)
// ---------------------------------------------------------------------------

func childCommand() (string, []string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/c", "exit 0"}
	}
	return "sh", []string{"-c", "exit 0"}
}

// managedConfig renders a clients.tabbit object with a real command, using
// json.Marshal so the argument list is always valid JSON.
func managedConfig(t *testing.T, cmd string, args []string, extra map[string]any) string {
	t.Helper()
	m := map[string]any{"manage": true, "command": cmd, "args": args}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshalling the test config: %v", err)
	}
	return string(b)
}

func TestStatusNeverSpawnsTheSidecar(t *testing.T) {
	clearTabbitEnv(t)
	cmd, args := childCommand()
	cfg := managedConfig(t, cmd, args, map[string]any{"timeouts": map[string]any{"start_seconds": 0.3}})
	withCandidates(t, closedPortURL(t))
	c := newTestClient(t, cfg, nil)

	if st := c.Status(context.Background()); st.Ready {
		t.Fatalf("status = %+v, want not ready", st)
	}
	if _, err := os.Stat(filepath.Join(c.deps.DataDir, "sidecar.log")); !os.IsNotExist(err) {
		t.Fatalf("Status must not start the sidecar (sidecar.log exists: %v)", err)
	}
}

func TestManagedStartWritesState(t *testing.T) {
	var healthCalls int32
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/health":
			if atomic.AddInt32(&healthCalls, 1) == 1 {
				return jsonResponse(500, `{"error":{"message":"booting"}}`), nil
			}
			return jsonResponse(200, `{"status":"ok","version":"0.1.9","models":9}`), nil
		case "/v1/chat/completions":
			return sseResponse(sseFixture), nil
		}
		return jsonResponse(404, `{}`), nil
	})}
	clearTabbitEnv(t)
	cmd, args := childCommand()
	cfg := managedConfig(t, cmd, args, map[string]any{"base_url": "http://sidecar.test"})
	c := newTestClient(t, cfg, hc)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{Model: "priority"})
	if err != nil {
		t.Fatalf("Chat with manage=true: %v", err)
	}
	events := collectEvents(t, stream)
	if countType(events, core.EventDone) != 1 {
		t.Fatalf("events = %+v", events)
	}

	raw, err := os.ReadFile(filepath.Join(c.deps.DataDir, stateFileName))
	if err != nil {
		t.Fatalf("the managed start should be remembered in DataDir: %v", err)
	}
	var st stateFile
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatalf("state file: %v", err)
	}
	if st.BaseURL != "http://sidecar.test" || st.Command != cmd {
		t.Fatalf("state = %+v", st)
	}
	if st.APIKey != "" {
		t.Fatal("the api key must not be persisted")
	}
	if _, err := os.Stat(filepath.Join(c.deps.DataDir, "sidecar.log")); err != nil {
		t.Fatalf("the child's output should go to DataDir/sidecar.log: %v", err)
	}
	if c.sup != nil {
		c.sup.stop()
	}
}

func TestStartSidecarWithoutCommand(t *testing.T) {
	clearTabbitEnv(t)
	c := newTestClient(t, `{"manage":true}`, nil)
	err := c.startSidecar(context.Background(), location{baseURL: "http://sidecar.test"})
	if err == nil || !strings.Contains(err.Error(), "no sidecar command") {
		t.Fatalf("error = %v", err)
	}
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestBuildChatBody(t *testing.T) {
	cfg := Config{ModelPrefix: defaultModelPrefix}.normalize()

	t.Run("minimal", func(t *testing.T) {
		body, err := buildChatBody(cfg, &core.ChatRequest{
			Model:    "priority",
			Messages: []core.Message{{Role: "user", Content: "hi"}},
		})
		if err != nil {
			t.Fatalf("buildChatBody: %v", err)
		}
		var req oaiChatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if req.Model != "tabbit/priority" || !req.Stream || req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
			t.Fatalf("body = %s", body)
		}
		if len(req.Messages) != 1 || req.Messages[0].Content != "hi" {
			t.Fatalf("messages = %+v", req.Messages)
		}
	})

	t.Run("already prefixed model", func(t *testing.T) {
		body, err := buildChatBody(cfg, &core.ChatRequest{Model: "tabbit/GPT-5.5"})
		if err != nil {
			t.Fatalf("buildChatBody: %v", err)
		}
		if !strings.Contains(string(body), `"model":"tabbit/GPT-5.5"`) {
			t.Fatalf("body = %s", body)
		}
	})

	t.Run("usage can be disabled", func(t *testing.T) {
		no := false
		off := Config{ModelPrefix: defaultModelPrefix, IncludeUsage: &no}.normalize()
		body, err := buildChatBody(off, &core.ChatRequest{Model: "priority"})
		if err != nil {
			t.Fatalf("buildChatBody: %v", err)
		}
		if strings.Contains(string(body), "stream_options") {
			t.Fatalf("body = %s", body)
		}
	})

	t.Run("parts, tools and sampling", func(t *testing.T) {
		temp := 0.2
		maxTok := 64
		body, err := buildChatBody(cfg, &core.ChatRequest{
			Model: "priority",
			Messages: []core.Message{{
				Role: "user",
				Parts: []core.ContentPart{
					{Type: "text", Text: "look"},
					{Type: "image_url", ImageURL: "http://img/1.png", Detail: "low"},
				},
			}},
			Tools: []core.Tool{
				{Type: "function", Name: "get_weather", Description: "d", Parameters: json.RawMessage(`{"type":"object"}`)},
				{Type: "function", Name: ""},
				{Type: "function", Name: "no_params"},
			},
			ToolChoice:  json.RawMessage(`"auto"`),
			Temperature: &temp,
			MaxTokens:   &maxTok,
			Stop:        []string{"END"},
			User:        "u1",
		})
		if err != nil {
			t.Fatalf("buildChatBody: %v", err)
		}
		var req oaiChatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(req.Tools) != 2 {
			t.Fatalf("the nameless tool must be dropped: %+v", req.Tools)
		}
		if string(req.Tools[1].Function.Parameters) != `{"type":"object","properties":{}}` {
			t.Fatalf("default parameters = %s", req.Tools[1].Function.Parameters)
		}
		if string(req.ToolChoice) != `"auto"` || req.Temperature == nil || *req.Temperature != temp ||
			req.MaxTokens == nil || *req.MaxTokens != maxTok || req.User != "u1" {
			t.Fatalf("request = %+v", req)
		}
		var parts []oaiContentPart
		rawParts, _ := json.Marshal(req.Messages[0].Content)
		if err := json.Unmarshal(rawParts, &parts); err != nil {
			t.Fatalf("content parts: %v (%s)", err, rawParts)
		}
		if len(parts) != 2 || parts[0].Type != "text" || parts[1].Type != "image_url" ||
			parts[1].ImageURL == nil || parts[1].ImageURL.URL != "http://img/1.png" {
			t.Fatalf("parts = %+v", parts)
		}
	})

	t.Run("tool choice is dropped without tools", func(t *testing.T) {
		body, err := buildChatBody(cfg, &core.ChatRequest{Model: "priority", ToolChoice: json.RawMessage(`"auto"`)})
		if err != nil {
			t.Fatalf("buildChatBody: %v", err)
		}
		if strings.Contains(string(body), "tool_choice") {
			t.Fatalf("body = %s", body)
		}
	})
}

func TestFlattenContent(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", ``, ""},
		{"null", `null`, ""},
		{"string", `"hello"`, "hello"},
		{"parts", `[{"type":"text","text":"a"},{"type":"input_text","text":"b"},{"type":"image_url"}]`, "ab"},
		{"unknown", `42`, ""},
		{"broken", `[`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := flattenContent(json.RawMessage(tt.in)); got != tt.want {
				t.Fatalf("flattenContent(%s) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeFinish(t *testing.T) {
	tests := map[string]string{
		"":                     "",
		"stop":                 "stop",
		"end_turn":             "stop",
		"stop_sequence":        "stop",
		"complete":             "stop",
		"length":               "length",
		"max_tokens":           "length",
		"max_output_tokens":    "length",
		"tool_calls":           "tool_calls",
		"tool_use":             "tool_calls",
		"function_call":        "tool_calls",
		"content_filter":       "content_filter",
		"safety":               "content_filter",
		"something_unexpected": "stop",
	}
	for in, want := range tests {
		if got := normalizeFinish(in); got != want {
			t.Errorf("normalizeFinish(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSSEReader(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		frames []sseFrame
	}{
		{
			name:   "crlf and comments",
			in:     ": keep-alive\r\nevent: message\r\ndata: {\"a\":1}\r\n\r\n",
			frames: []sseFrame{{Event: "message", Data: `{"a":1}`}},
		},
		{
			name:   "multi line data",
			in:     "data: line1\ndata: line2\n\n",
			frames: []sseFrame{{Data: "line1\nline2"}},
		},
		{
			name:   "no trailing newline",
			in:     "data: [DONE]",
			frames: []sseFrame{{Data: "[DONE]"}},
		},
		{
			name:   "several frames",
			in:     "data: one\n\ndata: two\n\n",
			frames: []sseFrame{{Data: "one"}, {Data: "two"}},
		},
		{
			name:   "blank lines between frames are ignored",
			in:     "\n\ndata: one\n\n\n",
			frames: []sseFrame{{Data: "one"}},
		},
		{name: "empty stream", in: "", frames: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newSSEReader(strings.NewReader(tt.in))
			var got []sseFrame
			for {
				fr, err := r.next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("next: %v", err)
				}
				got = append(got, fr)
			}
			if len(got) != len(tt.frames) {
				t.Fatalf("frames = %+v, want %+v", got, tt.frames)
			}
			for i := range got {
				if got[i] != tt.frames[i] {
					t.Fatalf("frame %d = %+v, want %+v", i, got[i], tt.frames[i])
				}
			}
		})
	}
}

func TestJoinURL(t *testing.T) {
	tests := []struct {
		base    string
		path    string
		want    string
		wantErr bool
	}{
		{base: "http://127.0.0.1:50124", path: "/health", want: "http://127.0.0.1:50124/health"},
		{base: "http://127.0.0.1:50124/", path: "v1/models", want: "http://127.0.0.1:50124/v1/models"},
		{base: "http://127.0.0.1:50124/api", path: "/v1/chat/completions", want: "http://127.0.0.1:50124/api/v1/chat/completions"},
		{base: "127.0.0.1:50124", path: "/health", wantErr: true},
		{base: "", path: "/health", wantErr: true},
	}
	for _, tt := range tests {
		got, err := joinURL(tt.base, tt.path)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("joinURL(%q, %q) = %q, want an error", tt.base, tt.path, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Fatalf("joinURL(%q, %q) = %q, %v; want %q", tt.base, tt.path, got, err, tt.want)
		}
	}
}

func TestStreamFromJSON(t *testing.T) {
	t.Run("completion", func(t *testing.T) {
		st, err := streamFromJSON([]byte(completionFixture), nil)
		if err != nil {
			t.Fatalf("streamFromJSON: %v", err)
		}
		events := collectEvents(t, st)
		if n := countType(events, core.EventDone); n != 1 {
			t.Fatalf("events = %+v", events)
		}
	})
	t.Run("error envelope", func(t *testing.T) {
		if _, err := streamFromJSON([]byte(`{"error":{"message":"nope"}}`), nil); err == nil || !strings.Contains(err.Error(), "nope") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("not json", func(t *testing.T) {
		if _, err := streamFromJSON([]byte("<html>oops</html>"), nil); err == nil || !strings.Contains(err.Error(), "non-JSON") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, err := streamFromJSON(nil, nil); err == nil {
			t.Fatal("want an error for an empty body")
		}
	})
}

func TestChatStreamCloseIsIdempotent(t *testing.T) {
	st := newChatStream(io.NopCloser(strings.NewReader(sseFixture)), func() {}, nil)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close twice: %v", err)
	}
	if _, err := st.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv after Close = %v, want io.EOF", err)
	}
}

func TestRegistration(t *testing.T) {
	c, err := core.Build("tabbit", core.Deps{DataDir: t.TempDir(), Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("core.Build(tabbit): %v", err)
	}
	if c.Name() != "tabbit" {
		t.Fatalf("Name() = %q", c.Name())
	}
}

func TestHelperFunctions(t *testing.T) {
	if got := firstNonEmpty("", "  ", "x", "y"); got != "x" {
		t.Fatalf("firstNonEmpty = %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("firstNonEmpty = %q", got)
	}
	if got := truncate("abcdef", 3); got != "abc…" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("ab", 5); got != "ab" {
		t.Fatalf("truncate = %q", got)
	}
	if got := asString(json.Number("42")); got != "42" {
		t.Fatalf("asString = %q", got)
	}
	if got := asString(float64(3)); got != "3" {
		t.Fatalf("asString = %q", got)
	}
	if got := firstString(map[string]any{"b": "2"}, "a", "b"); got != "2" {
		t.Fatalf("firstString = %q", got)
	}
	if n, ok := toInt("7"); !ok || n != 7 {
		t.Fatalf("toInt = %d, %v", n, ok)
	}
	if _, ok := toInt(struct{}{}); ok {
		t.Fatal("toInt should reject an unsupported type")
	}
	if got := readLimited(strings.NewReader("hello"), 3); string(got) != "hel" {
		t.Fatalf("readLimited = %q", got)
	}
	if got := readLimited(nil, 3); got != nil {
		t.Fatalf("readLimited(nil) = %v", got)
	}
	if got := isDonePayload(" [done] "); !got {
		t.Fatal("[done] should terminate the stream")
	}
	if got := isDonePayload("nope"); got {
		t.Fatal("nope is not a terminator")
	}
}

func TestExampleConfigParses(t *testing.T) {
	// config.example.json is a deliverable: keep it honest.
	raw, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatalf("reading config.example.json: %v", err)
	}
	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("parseConfig(config.example.json): %v", err)
	}
	cfg = cfg.normalize()
	if cfg.BaseURL != "http://127.0.0.1:50124" {
		t.Fatalf("base_url = %q", cfg.BaseURL)
	}
	if cfg.ModelPrefix != "tabbit/" {
		t.Fatalf("model_prefix = %q", cfg.ModelPrefix)
	}
	if cfg.ChatPath != "/v1/chat/completions" || cfg.HealthPath != "/health" {
		t.Fatalf("paths = %q %q", cfg.HealthPath, cfg.ChatPath)
	}
	if !cfg.includeUsage() {
		t.Fatal("include_usage should be true")
	}
	if cfg.Timeouts.statusBudget() <= 0 || cfg.Timeouts.requestBudget() <= 0 {
		t.Fatal("budgets must be positive")
	}
}

func TestStatusModelsComeFromTheCache(t *testing.T) {
	var modelCalls int32
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/v1/models":
			atomic.AddInt32(&modelCalls, 1)
			return jsonResponse(200, modelsFixture), nil
		case "/health":
			return jsonResponse(200, `{"status":"ok"}`), nil
		}
		return jsonResponse(404, `{}`), nil
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, hc)

	if _, err := c.Models(context.Background()); err != nil {
		t.Fatalf("Models: %v", err)
	}
	st := c.Status(context.Background())
	if strings.Join(st.Models, ",") != "priority,GPT-5.5,Default" {
		t.Fatalf("status models = %v", st.Models)
	}
	if n := atomic.LoadInt32(&modelCalls); n != 1 {
		t.Fatalf("Status must not refetch the catalog: %d calls", n)
	}
}

// TestTabbitChatNamesTheServedAccount pins the gateway-facing attribution for
// the sidecar transport.  On this transport the endpoint *is* the account: it
// is the id the account table reports and the value resolve() settled on.
// Before the ServedBy slot existed every success was filed under "(unrouted)".
func TestTabbitChatNamesTheServedAccount(t *testing.T) {
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return sseResponse(sseFixture), nil
	})}
	clearTabbitEnv(t)
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, hc)

	var served string
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "priority",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		ServedBy: &served,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	collectEvents(t, stream)

	if served != "http://sidecar.test" {
		t.Errorf("ServedBy = %q, want %q", served, "http://sidecar.test")
	}
}
