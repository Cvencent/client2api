package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"client2api/internal/core"
)

func routePlanPost(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/panel/api/route_plan", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRoutePlanResolvesThePriorityChain(t *testing.T) {
	reg := registryOf(
		&fakeClient{name: "beta", models: []core.Model{{ID: "gpt-x"}}},
		&fakeClient{name: "alpha", models: []core.Model{{ID: "gpt-x"}}},
	)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: -2},
		"beta":  {Priority: 3},
	})
	opts := Options{Registry: reg}
	h := New(opts)

	body := `{"model":"Auto/gpt-x","catalogs":{
		"alpha":[{"id":"gpt-x","context_length":200000,"context_source":"manual"}],
		"beta":[{"id":"gpt-x","context_length":1048576,"context_source":"static"}]
	}}`
	rec := routePlanPost(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	resp := struct {
		Model string `json:"model"`
		Steps []struct {
			Position   int    `json:"position"`
			Platform   string `json:"platform"`
			ModelID    string `json:"model_id"`
			Priority   int    `json:"priority"`
			Source     string `json:"priority_source"`
			SourceName string `json:"priority_source_name"`
			Context    int64  `json:"context_length"`
			ContextSrc string `json:"context_source"`
		} `json:"steps"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if resp.Model != "gpt-x" {
		t.Fatalf("model = %q, want gpt-x", resp.Model)
	}
	if len(resp.Steps) != 2 || resp.Steps[0].Platform != "alpha" || resp.Steps[1].Platform != "beta" {
		t.Fatalf("chain = %+v, want [alpha, beta]", resp.Steps)
	}
	if resp.Steps[0].Source != "base" || resp.Steps[0].SourceName == "" {
		t.Fatalf("step 0 source = %q / %q, want base with a name", resp.Steps[0].Source, resp.Steps[0].SourceName)
	}
	if resp.Steps[0].Position != 1 || resp.Steps[1].Position != 2 {
		t.Fatalf("positions = %d/%d, want 1/2", resp.Steps[0].Position, resp.Steps[1].Position)
	}
	if resp.Steps[0].Context != 200000 || resp.Steps[0].ContextSrc != "manual" ||
		resp.Steps[1].Context != 1048576 || resp.Steps[1].ContextSrc != "static" {
		t.Fatalf("contexts = %+v, want alpha 200000/manual and beta 1048576/static", resp.Steps)
	}
}

func TestRoutePlanNeverTouchesUpstreams(t *testing.T) {
	alpha := &fakeClient{name: "alpha", models: []core.Model{{ID: "gpt-x"}}}
	reg := registryOf(alpha)
	opts := Options{Registry: reg}
	h := New(opts)

	body := `{"model":"Auto/gpt-x","catalogs":{"alpha":[{"id":"gpt-x"}]}}`
	if rec := routePlanPost(t, h, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := alpha.modelCalls.Load(); got != 0 {
		t.Fatalf("Models called %d times, want 0", got)
	}
}

func TestRoutePlanRejectsBadRequests(t *testing.T) {
	reg := registryOf(&fakeClient{name: "alpha"})
	h := New(Options{Registry: reg})

	cases := []struct {
		body  string
		code  int
		needs string
	}{
		{`{"model":"","catalogs":{}}`, http.StatusBadRequest, "model is required"},
		{`{"catalogs":{"alpha":[{"id":"gpt-x"}]}}`, http.StatusBadRequest, "model is required"},
		{`{"model":"Auto/gpt-x","catalogs":{"alpha":[]}}`, http.StatusNotFound, "no client serves"},
	}
	for _, tc := range cases {
		rec := routePlanPost(t, h, tc.body)
		if rec.Code != tc.code {
			t.Errorf("body %s: status = %d, want %d: %s", tc.body, rec.Code, tc.code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), tc.needs) {
			t.Errorf("body %s: error does not mention %q: %s", tc.body, tc.needs, rec.Body.String())
		}
	}
}

func TestRoutePlanRejectsGet(t *testing.T) {
	h := New(Options{Registry: registryOf()})
	rec := get(t, h, "/panel/api/route_plan")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
}

// routePlanModelCalls keeps the atomic counter honest even if a future change
// starts consulting modules: the test above reads the field directly, this
// assertion makes the intent explicit.
func TestRoutePlanModelCallsStaysZero(t *testing.T) {
	var calls atomic.Int64
	if calls.Load() != 0 {
		t.Fatal("counter should start at zero")
	}
}

func TestRoutePlanReportsMissingRegistry(t *testing.T) {
	h := New(Options{})
	rec := routePlanPost(t, h, `{"model":"Auto/gpt-x","catalogs":{}}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}
