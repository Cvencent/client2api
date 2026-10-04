package panel

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/gateway"
	"client2api/internal/livecfg"
)

// ---------------------------------------------------------------------------
// fakes
// ---------------------------------------------------------------------------

const testJWT = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk"

// fakeClient is the minimum a module must be: a name, a catalogue, chat and a
// status.  It deliberately does NOT implement any optional capability.
type fakeClient struct {
	name      string
	models    []core.Model
	modelsErr error
	status    core.Status

	modelCalls atomic.Int64
}

func (f *fakeClient) Name() string { return f.name }

func (f *fakeClient) Models(ctx context.Context) ([]core.Model, error) {
	f.modelCalls.Add(1)
	return f.models, f.modelsErr
}

func (f *fakeClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	return nil, core.ErrUnsupported
}

func (f *fakeClient) Status(ctx context.Context) core.Status {
	st := f.status
	st.Name = f.name
	return st
}

// fakeAccountClient adds the AccountManager capability on top of fakeClient.
type fakeAccountClient struct {
	*fakeClient
	accounts    []core.AccountRecord
	accountsErr error
}

func (f *fakeAccountClient) AccountFields(ctx context.Context) []core.FieldSpec { return nil }

func (f *fakeAccountClient) Accounts(ctx context.Context) ([]core.AccountRecord, error) {
	return f.accounts, f.accountsErr
}

func (f *fakeAccountClient) AddAccount(ctx context.Context, spec core.AccountSpec) (core.AccountRecord, error) {
	return core.AccountRecord{}, nil
}
func (f *fakeAccountClient) RemoveAccount(ctx context.Context, id string) error { return nil }
func (f *fakeAccountClient) SetAccountEnabled(ctx context.Context, id string, enabled bool) error {
	return nil
}
func (f *fakeAccountClient) TestAccount(ctx context.Context, id string) (core.TestResult, error) {
	return core.TestResult{}, nil
}
func (f *fakeAccountClient) RefreshAccount(ctx context.Context, id string) ([]core.RefreshResult, error) {
	return nil, nil
}

// fakeRefreshClient adds the ModelRefresher capability.
type fakeRefreshClient struct {
	*fakeClient
	refreshModels []core.Model
	refreshErr    error
	refreshCalls  atomic.Int64
}

func (f *fakeRefreshClient) RefreshModels(ctx context.Context) ([]core.Model, error) {
	f.refreshCalls.Add(1)
	return f.refreshModels, f.refreshErr
}

func registryOf(clients ...core.Client) *core.Registry {
	reg := core.NewRegistry()
	for _, c := range clients {
		reg.Add(c)
	}
	return reg
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func post(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// getWithKey issues an authenticated GET.  Every /panel/api/* route is behind
// the shared bearer, so a test that exercises one has to present the key the
// way a browser does — otherwise it is testing the 401 path by accident.
func getWithKey(t *testing.T, h http.Handler, target, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response %q: %v", rec.Body.String(), err)
	}
	return got
}

// failingChat sends one chat request for a model nobody serves.  It always
// fails, which is exactly what makes it a cheap way to move the gateway's
// counters from a test that does not own them.
func failingChat(t *testing.T, stats *gateway.Stats, reg *core.Registry) {
	t.Helper()
	srv := gateway.NewServer(gateway.Options{
		Registry: reg,
		Stats:    stats,
		Logger:   log.New(io.Discard, "", 0),
	})
	body := strings.NewReader(`{"model":"nobody-serves-this","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected the unknown-model request to fail, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// overview
// ---------------------------------------------------------------------------

func TestOverviewShape(t *testing.T) {
	plain := &fakeClient{
		name:   "plain",
		models: []core.Model{{ID: "plain-model", OwnedBy: "plain"}},
		status: core.Status{Ready: true, Detail: "ready", Models: []string{"plain-model"}},
	}
	// A module that manages accounts but currently holds none: the panel must
	// see [] and not null.
	empty := &fakeAccountClient{
		fakeClient: &fakeClient{name: "empty", status: core.Status{Ready: false, Detail: "no account"}},
		accounts:   nil,
	}

	reg := registryOf(plain, empty)
	stats := gateway.NewStats()
	// Drive two failing chat requests through the gateway itself: this proves
	// the panel and the gateway are looking at the SAME counters, which is the
	// whole point of creating them in cmd and sharing them by reference.
	failingChat(t, stats, reg)
	failingChat(t, stats, reg)

	logs := gateway.NewLogRing(8)
	logs.Add("hello")

	h := New(Options{
		Registry: reg,
		Version:  "0.1.0-dev",
		Listen:   "127.0.0.1:8788",
		Started:  time.Now().Add(-2 * time.Minute),
		Stats:    stats,
		Logs:     logs,
	})

	rec := get(t, h, "/panel/api/overview")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	got := decodeMap(t, rec)

	for _, key := range []string{"version", "uptime", "started_at", "requests", "failures", "log_lines", "clients"} {
		if _, ok := got[key]; !ok {
			t.Errorf("overview is missing %q: %v", key, got)
		}
	}
	if got["version"] != "0.1.0-dev" {
		t.Errorf("version = %v", got["version"])
	}
	if got["requests"] != float64(2) || got["failures"] != float64(2) {
		t.Errorf("counters = %v/%v, want 2/2 from the two failing gateway requests", got["requests"], got["failures"])
	}
	if got["log_lines"] != float64(1) {
		t.Errorf("log_lines = %v, want 1", got["log_lines"])
	}
	if !strings.Contains(got["uptime"].(string), "2m") {
		t.Errorf("uptime = %v, want something like 2m0s", got["uptime"])
	}

	clients, ok := got["clients"].([]any)
	if !ok || len(clients) != 2 {
		t.Fatalf("clients = %v, want 2 entries", got["clients"])
	}
	byName := map[string]map[string]any{}
	for _, c := range clients {
		row := c.(map[string]any)
		byName[row["name"].(string)] = row
	}

	emptyRow, ok := byName["empty"]
	if !ok {
		t.Fatalf("no row for the empty client: %v", byName)
	}
	accounts, ok := emptyRow["accounts"].([]any)
	if !ok || accounts == nil {
		t.Errorf("accounts for a client with none = %v (%T), want an empty array", emptyRow["accounts"], emptyRow["accounts"])
	}
	if _, ok := emptyRow["capabilities"].(map[string]any); !ok {
		t.Errorf("capabilities missing on %v", emptyRow)
	}
	if emptyRow["models"] == nil {
		t.Errorf("models must be an array, got %v", emptyRow["models"])
	}

	plainRow := byName["plain"]
	if plainRow["ready"] != true {
		t.Errorf("plain.ready = %v, want true", plainRow["ready"])
	}
	if models, _ := plainRow["models"].([]any); len(models) != 1 || models[0] != "plain-model" {
		t.Errorf("plain.models = %v, want [plain-model]", plainRow["models"])
	}
}

// TestOverviewReportsWhetherAKeyIsNeeded pins the shell's one chance to know it
// must ask for a key before its first request fails, and to keep the two states
// apart: "no key configured" and "a key is configured" must not look alike,
// because the second one is what puts the prompt in front of the operator.
func TestOverviewReportsWhetherAKeyIsNeeded(t *testing.T) {
	reg := registryOf(&fakeClient{name: "plain"})

	t.Run("open", func(t *testing.T) {
		h := New(Options{Registry: reg, Started: time.Now()})
		got := decodeMap(t, get(t, h, "/panel/api/overview"))
		if v, ok := got["auth_required"]; !ok || v != false {
			t.Fatalf("auth_required = %v (present=%v), want false", v, ok)
		}
	})

	t.Run("protected", func(t *testing.T) {
		h := New(Options{
			Registry:    reg,
			Started:     time.Now(),
			AuthEnabled: true,
			Live:        livecfg.New(livecfg.Snapshot{APIKey: "deadbeefdeadbeef"}),
		})
		// The route itself is protected, so the honest way to read the flag is
		// with the key.  Asserting the 401 as well keeps the two facts tied: a
		// panel that reported auth_required=false while returning 401 would be
		// lying to the shell that asked.
		if rec := get(t, h, "/panel/api/overview"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
		}
		got := decodeMap(t, getWithKey(t, h, "/panel/api/overview", "deadbeefdeadbeef"))
		if v, ok := got["auth_required"]; !ok || v != true {
			t.Fatalf("auth_required = %v (present=%v), want true", v, ok)
		}
	})
}

func TestOverviewRedactsModelIDs(t *testing.T) {
	c := &fakeClient{
		name:   "leaky",
		status: core.Status{Ready: true, Models: []string{testJWT}},
	}
	h := New(Options{Registry: registryOf(c), Started: time.Now()})

	rec := get(t, h, "/panel/api/overview")
	if strings.Contains(rec.Body.String(), testJWT) {
		t.Errorf("overview leaks a model id that looks like a credential: %s", rec.Body.String())
	}
}

func TestOverviewWithoutRegistry(t *testing.T) {
	h := New(Options{Version: "x", Started: time.Now()})
	rec := get(t, h, "/panel/api/overview")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	got := decodeMap(t, rec)
	if clients, ok := got["clients"].([]any); !ok || clients == nil {
		t.Errorf("clients = %v, want []", got["clients"])
	}
}

// ---------------------------------------------------------------------------
// logs
// ---------------------------------------------------------------------------

func TestLogsEndpoint(t *testing.T) {
	ring := gateway.NewLogRing(5)
	for i := 0; i < 10; i++ {
		ring.Add(strings.Repeat("x", 1) + " line-" + string(rune('a'+i)))
	}
	h := New(Options{Registry: registryOf(), Logs: ring, Started: time.Now()})

	rec := get(t, h, "/panel/api/logs?lines=3")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var got logsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Entries) != 3 {
		t.Errorf("entries = %d, want 3", len(got.Entries))
	}
	if got.Total != 10 || got.Capacity != 5 {
		t.Errorf("total/capacity = %d/%d, want 10/5", got.Total, got.Capacity)
	}
	if !strings.Contains(got.Entries[2].Text, "line-j") {
		t.Errorf("newest entry missing: %q", got.Entries)
	}

	// Asking for more than the capacity is clamped, not rejected.
	rec = get(t, h, "/panel/api/logs?lines=999")
	got = logsResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Entries) != 5 {
		t.Errorf("clamped entries = %d, want 5", len(got.Entries))
	}

	// Default is 500 lines; with a 5-line ring that is still 5.
	rec = get(t, h, "/panel/api/logs")
	got = logsResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Entries) != 5 {
		t.Errorf("default entries = %d, want 5", len(got.Entries))
	}

	rec = get(t, h, "/panel/api/logs?lines=abc")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("lines=abc status = %d, want 400", rec.Code)
	}
}

func TestLogsEndpointWithoutRing(t *testing.T) {
	h := New(Options{Started: time.Now()})
	rec := get(t, h, "/panel/api/logs")
	got := logsResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Entries == nil {
		t.Error("entries must be [] and not null")
	}
}

// ---------------------------------------------------------------------------
// models
// ---------------------------------------------------------------------------

func TestModelsListGroupsByClient(t *testing.T) {
	plain := &fakeClient{
		name:   "plain",
		models: []core.Model{{ID: "plain-model"}},
		status: core.Status{},
	}
	refresher := &fakeRefreshClient{
		fakeClient:    &fakeClient{name: "refresh", models: []core.Model{{ID: "stale"}}},
		refreshModels: []core.Model{{ID: "fresh", OwnedBy: "vendor", Extra: map[string]any{"context_length": 200000}}},
	}

	h := New(Options{Registry: registryOf(plain, refresher), Started: time.Now()})
	rec := get(t, h, "/panel/api/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var got modelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RefreshedAt == "" {
		t.Error("refreshed_at is empty")
	}
	if len(got.Clients) != 2 {
		t.Fatalf("clients = %d, want 2", len(got.Clients))
	}
	byName := map[string]clientModels{}
	for _, c := range got.Clients {
		byName[c.Name] = c
	}

	if byName["plain"].CanRefresh {
		t.Error("plain.can_refresh = true, want false")
	}
	if !byName["refresh"].CanRefresh {
		t.Error("refresh.can_refresh = false, want true")
	}
	// GET must never refresh, even for a capable client.
	if refresher.refreshCalls.Load() != 0 {
		t.Errorf("GET called RefreshModels %d times, want 0", refresher.refreshCalls.Load())
	}
	if len(byName["refresh"].Models) != 1 || byName["refresh"].Models[0].ID != "stale" {
		t.Errorf("GET should report the plain Models output: %+v", byName["refresh"].Models)
	}
	if byName["plain"].Models[0].OwnedBy != "plain" {
		t.Errorf("owned_by = %q, want the client name as fallback", byName["plain"].Models[0].OwnedBy)
	}
}

func TestModelsRefreshOnlyCallsCapableClients(t *testing.T) {
	plain := &fakeClient{name: "plain", models: []core.Model{{ID: "plain-model"}}}
	refresher := &fakeRefreshClient{
		fakeClient:    &fakeClient{name: "refresh", models: []core.Model{{ID: "stale"}}},
		refreshModels: []core.Model{{ID: "fresh"}},
	}

	h := New(Options{Registry: registryOf(plain, refresher), Started: time.Now()})
	rec := post(t, h, "/panel/api/models/refresh")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	if got := refresher.refreshCalls.Load(); got != 1 {
		t.Errorf("RefreshModels calls = %d, want exactly 1", got)
	}
	// The incapable client is served from plain Models, never refreshed.
	if got := plain.modelCalls.Load(); got != 1 {
		t.Errorf("plain Models calls = %d, want 1", got)
	}

	var report modelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byName := map[string]clientModels{}
	for _, c := range report.Clients {
		byName[c.Name] = c
	}
	if byName["refresh"].Models[0].ID != "fresh" {
		t.Errorf("refresh.models = %+v, want the refreshed list", byName["refresh"].Models)
	}
	if byName["plain"].CanRefresh {
		t.Error("plain.can_refresh = true, want false")
	}
	if byName["refresh"].Error != "" {
		t.Errorf("refresh.error = %q, want empty", byName["refresh"].Error)
	}
}

// TestModelsRefreshErrorKeepsLastGoodModels is the important one: a refresh
// failure must not blank the catalogue.
func TestModelsRefreshErrorKeepsLastGoodModels(t *testing.T) {
	refresher := &fakeRefreshClient{
		fakeClient:    &fakeClient{name: "flaky"},
		refreshModels: []core.Model{{ID: "last-good"}},
		refreshErr:    errFake("upstream 502: token " + testJWT + " rejected"),
	}

	h := New(Options{Registry: registryOf(refresher), Started: time.Now()})
	rec := post(t, h, "/panel/api/models/refresh")

	var report modelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	row := report.Clients[0]
	if len(row.Models) != 1 || row.Models[0].ID != "last-good" {
		t.Errorf("models were blanked by the error: %+v", row.Models)
	}
	if row.Error == "" {
		t.Error("error is empty, want the redacted failure")
	}
	if strings.Contains(row.Error, testJWT) {
		t.Errorf("error leaks a credential: %q", row.Error)
	}
}

func TestModelsRefreshErrorWithoutModelsSaysSo(t *testing.T) {
	refresher := &fakeRefreshClient{
		fakeClient: &fakeClient{name: "broken"},
		refreshErr: errFake("no credential"),
	}
	h := New(Options{Registry: registryOf(refresher), Started: time.Now()})
	rec := post(t, h, "/panel/api/models/refresh")

	var report modelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("decode: %v", err)
	}
	row := report.Clients[0]
	if len(row.Models) != 0 {
		t.Errorf("models = %+v, want empty", row.Models)
	}
	if row.Models == nil {
		t.Error("models must be [] and not null")
	}
	if !strings.Contains(row.Error, "no models returned") {
		t.Errorf("error = %q, want it to say the list is empty", row.Error)
	}
}

func TestModelsMethodGuards(t *testing.T) {
	h := New(Options{Registry: registryOf(), Started: time.Now()})
	if rec := post(t, h, "/panel/api/models"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /models status = %d, want 405", rec.Code)
	}
	if rec := get(t, h, "/panel/api/models/refresh"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /models/refresh status = %d, want 405", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// usage
// ---------------------------------------------------------------------------

func TestUsageEndpoint(t *testing.T) {
	store := gateway.NewUsageStore(100)
	now := time.Now()
	store.Record(gateway.UsageRecord{At: now.Add(-time.Hour), Client: "trae", Model: "trae/m", PromptTokens: 900, CompletionTokens: 400, TotalTokens: 1300})
	store.Record(gateway.UsageRecord{At: now.Add(-30 * time.Minute), Client: "trae", Model: "trae/m", Failed: true})

	h := New(Options{Registry: registryOf(), Usage: store, Started: time.Now()})
	rec := get(t, h, "/panel/api/usage?window=72")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}

	var got gateway.UsageReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.WindowHours != 72 {
		t.Errorf("window_hours = %d, want 72", got.WindowHours)
	}
	if got.Totals.Requests != 2 || got.Totals.Failures != 1 {
		t.Errorf("totals = %+v, want 2 requests / 1 failure", got.Totals)
	}
	if got.Totals.TotalTokens != 1300 {
		t.Errorf("total_tokens = %d, want 1300", got.Totals.TotalTokens)
	}
	if len(got.ByClient) != 1 || got.ByClient[0].Name != "trae" {
		t.Errorf("by_client = %+v", got.ByClient)
	}
	if len(got.Series) == 0 {
		t.Error("series is empty, want at least one hourly bucket")
	}
}

func TestUsageWindowValidation(t *testing.T) {
	h := New(Options{Registry: registryOf(), Usage: gateway.NewUsageStore(10), Started: time.Now()})

	if rec := get(t, h, "/panel/api/usage?window=abc"); rec.Code != http.StatusBadRequest {
		t.Errorf("window=abc status = %d, want 400", rec.Code)
	}

	var got gateway.UsageReport
	rec := get(t, h, "/panel/api/usage?window=999999")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.WindowHours != maxUsageWindowHours {
		t.Errorf("window_hours = %d, want the cap %d", got.WindowHours, maxUsageWindowHours)
	}

	// No window parameter falls back to the default.
	rec = get(t, h, "/panel/api/usage")
	got = gateway.UsageReport{}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.WindowHours != defaultUsageWindowHours {
		t.Errorf("default window_hours = %d, want %d", got.WindowHours, defaultUsageWindowHours)
	}
	if got.ByClient == nil || got.ByModel == nil || got.Series == nil {
		t.Error("empty usage report must serialise its lists as []")
	}
}

// ---------------------------------------------------------------------------
// config
// ---------------------------------------------------------------------------

func TestConfigRedactsEverySecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "client2api.json")
	fixture := `{
  "listen": "127.0.0.1:8788",
  "api_key": "deadbeefdeadbeef",
  "data_dir": "data",
  "clients": {
    "kimi": {"access_token": "` + testJWT + `", "refresh_token": "refresh-secret-value", "note": "keep me"},
    "zcode": {"password": "hunter2hunter2", "keys": ["first-key-value"], "model": "GLM-5.3"}
  }
}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	h := New(Options{
		Registry:    registryOf(&fakeClient{name: "kimi"}),
		Version:     "0.1.0-dev",
		Listen:      "127.0.0.1:8788",
		Started:     time.Now(),
		ConfigPath:  path,
		AuthEnabled: true,
		Live:        livecfg.New(livecfg.Snapshot{APIKey: "deadbeefdeadbeef"}),
	})

	rec := getWithKey(t, h, "/panel/api/config", "deadbeefdeadbeef")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, secret := range []string{"deadbeefdeadbeef", testJWT, "refresh-secret-value", "hunter2hunter2", "first-key-value"} {
		if strings.Contains(body, secret) {
			t.Errorf("config response leaks %q: %s", secret, body)
		}
	}

	var got configResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Path != path {
		t.Errorf("path = %q, want %q", got.Path, path)
	}
	if !got.APIKeySet {
		t.Error("api_key_set = false, want true")
	}
	if got.Addr != "127.0.0.1:8788" {
		t.Errorf("addr = %q", got.Addr)
	}
	if len(got.Clients) != 1 || got.Clients[0] != "kimi" {
		t.Errorf("clients = %v", got.Clients)
	}
	if got.Config == nil {
		t.Fatal("config is null, want the redacted document")
	}

	cfg := got.Config.(map[string]any)
	// A non-secret value survives, so the panel can actually show something.
	if cfg["data_dir"] != "data" {
		t.Errorf("data_dir = %v, want data", cfg["data_dir"])
	}
	if cfg["api_key"] != "<redacted>" {
		t.Errorf("api_key = %v, want <redacted>", cfg["api_key"])
	}
	if cfg["listen"] != "127.0.0.1:8788" {
		t.Errorf("listen = %v", cfg["listen"])
	}

	clients := cfg["clients"].(map[string]any)
	kimi := clients["kimi"].(map[string]any)
	if kimi["access_token"] != "<redacted>" || kimi["refresh_token"] != "<redacted>" {
		t.Errorf("kimi tokens not masked: %v", kimi)
	}
	if kimi["note"] != "keep me" {
		t.Errorf("kimi.note = %v, want keep me", kimi["note"])
	}
	zcode := clients["zcode"].(map[string]any)
	if zcode["password"] != "<redacted>" {
		t.Errorf("zcode.password = %v, want <redacted>", zcode["password"])
	}
	if zcode["model"] != "GLM-5.3" {
		t.Errorf("zcode.model = %v, want GLM-5.3", zcode["model"])
	}
}

func TestConfigUnknownPathReportsNull(t *testing.T) {
	h := New(Options{Registry: registryOf(), Version: "x", Started: time.Now()})
	rec := get(t, h, "/panel/api/config")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got configResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Path != "" {
		t.Errorf("path = %q, want empty", got.Path)
	}
	if got.Config != nil {
		t.Errorf("config = %v, want null when the path is unknown", got.Config)
	}
	if got.APIKeySet {
		t.Error("api_key_set = true, want false")
	}
}

func TestConfigMissingFileIsAnError(t *testing.T) {
	h := New(Options{
		Registry:   registryOf(),
		Started:    time.Now(),
		ConfigPath: filepath.Join(t.TempDir(), "absent.json"),
	})
	rec := get(t, h, "/panel/api/config")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestMaskSecretKeepsShape(t *testing.T) {
	if got := maskSecret(""); got != "" {
		t.Errorf("maskSecret(\"\") = %v, want an empty string so 'not set' stays visible", got)
	}
	if got := maskSecret(nil); got != nil {
		t.Errorf("maskSecret(nil) = %v, want nil", got)
	}
	if got := maskSecret("secret-value"); got != "<redacted>" {
		t.Errorf("maskSecret(string) = %v", got)
	}
	if got := maskSecret(map[string]any{"a": "b"}); got.(map[string]any)["a"] != "<redacted>" {
		t.Errorf("maskSecret(map) = %v", got)
	}
	if got := maskSecret([]any{"a", "b"}); len(got.([]any)) != 2 {
		t.Errorf("maskSecret(slice) = %v", got)
	}
	if got := maskSecret(42); got != "<redacted>" {
		t.Errorf("maskSecret(number) = %v", got)
	}
}

// errFake is a tiny error type so tests can build failures with odd payloads.
type errFake string

func (e errFake) Error() string { return string(e) }
