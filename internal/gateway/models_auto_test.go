package gateway

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

func autoTestServer(t *testing.T, clients ...*testClient) *http.Server {
	t.Helper()
	reg := core.NewRegistry()
	for _, c := range clients {
		reg.Add(c)
	}
	return NewServer(Options{
		Registry: reg,
		Version:  "test",
		Logger:   log.New(io.Discard, "", 0),
		Stats:    NewStats(),
		Usage:    NewUsageStore(10),
	})
}

func modelIDs(t *testing.T, srv *http.Server) map[string]modelEntry {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out modelList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	byID := make(map[string]modelEntry, len(out.Data))
	for _, m := range out.Data {
		byID[m.ID] = m
	}
	return byID
}

func TestModelsAddsAggregatedAutoEntriesWithoutReplacingPlatformModels(t *testing.T) {
	srv := autoTestServer(t,
		&testClient{name: "alpha", catalogue: []core.Model{{ID: "DeepSeek-V4.1-Flash"}}},
		&testClient{name: "beta", catalogue: []core.Model{{ID: "deepseek-v4.1-flash"}}},
		&testClient{name: "gamma", catalogue: []core.Model{{ID: "openrouter/auto"}}},
	)

	got := modelIDs(t, srv)
	for _, id := range []string{
		"alpha/DeepSeek-V4.1-Flash",
		"beta/deepseek-v4.1-flash",
		"gamma/openrouter/auto",
		"Auto/DeepSeek-V4.1-Flash",
		"Auto/auto",
	} {
		if _, ok := got[id]; !ok {
			t.Errorf("catalogue is missing %q: %v", id, got)
		}
	}
	auto := got["Auto/DeepSeek-V4.1-Flash"]
	if auto.OwnedBy != "auto" || auto.Extra["target"] != "deepseek-v4.1-flash" {
		t.Errorf("Auto entry = %+v, want owned_by=auto and canonical target", auto)
	}
}

func TestModelsHidesAutoEntriesWhenEveryPlatformDisabledTheModel(t *testing.T) {
	reg := core.NewRegistry()
	reg.Add(&testClient{name: "alpha", catalogue: []core.Model{{ID: "GLM-5.3"}}})
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {DisabledModels: []string{"GLM-5.3"}},
	})
	srv := NewServer(Options{
		Registry: reg, Version: "test", Logger: log.New(io.Discard, "", 0),
		Stats: NewStats(), Usage: NewUsageStore(10),
	})

	got := modelIDs(t, srv)
	if _, ok := got["Auto/GLM-5.3"]; ok {
		t.Fatalf("Auto/GLM-5.3 is advertised although every platform disabled it: %v", got)
	}
}

func TestAutoRequestRoutesThroughPlatformPriority(t *testing.T) {
	low := &testClient{
		name: "alpha", catalogue: []core.Model{{ID: "DeepSeek-V4.1-Flash"}},
		events: usageEvents(1, 1),
	}
	high := &testClient{
		name: "beta", catalogue: []core.Model{{ID: "deepseek-v4.1-flash"}},
		events: usageEvents(1, 1),
	}
	reg := core.NewRegistry()
	reg.Add(low)
	reg.Add(high)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: 20},
		"beta":  {Priority: 10},
	})
	srv := NewServer(Options{
		Registry: reg, Version: "test", Logger: log.New(io.Discard, "", 0),
		Stats: NewStats(), Usage: NewUsageStore(10),
	})

	rec := chat(t, srv, `{"model":"Auto/DeepSeek-V4.1-Flash","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("Auto request failed: %d %s", rec.Code, rec.Body.String())
	}
	if high.seen == nil || high.seen.Model != "deepseek-v4.1-flash" {
		t.Fatalf("high-priority platform did not receive the request: %+v", high.seen)
	}
	if low.seen != nil {
		t.Fatalf("low-priority platform received a request it should not have: %+v", low.seen)
	}
	if !strings.Contains(rec.Body.String(), `"model":"Auto/DeepSeek-V4.1-Flash"`) {
		t.Errorf("response model = %s, want the requested Auto/ name echoed back", rec.Body.String())
	}
}
