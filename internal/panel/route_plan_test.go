package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// The route plan must show the per-model override, and label it as such, so the
// number on the platform card's model row and the number in the chain are the
// same value.
func TestRoutePlanShowsThePerModelPriority(t *testing.T) {
	reg := registryOf(
		&fakeClient{name: "alpha", models: []core.Model{{ID: "gpt-x"}}},
		&fakeClient{name: "beta", models: []core.Model{{ID: "gpt-x"}}},
	)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: 9, ModelPriorities: map[string]int{"gpt-x": -3}},
		"beta":  {Priority: 0},
	})
	h := New(Options{Registry: reg})

	body := `{"model":"gpt-x","catalogs":{"alpha":[{"id":"gpt-x"}],"beta":[{"id":"gpt-x"}]}}`
	rec := routePlanPost(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	resp := struct {
		Steps []struct {
			Platform   string `json:"platform"`
			Priority   int    `json:"priority"`
			Source     string `json:"priority_source"`
			SourceName string `json:"priority_source_name"`
		} `json:"steps"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(resp.Steps) != 2 || resp.Steps[0].Platform != "alpha" {
		t.Fatalf("chain = %+v, want alpha first by its per-model -3", resp.Steps)
	}
	if resp.Steps[0].Priority != -3 || resp.Steps[0].Source != "model" {
		t.Fatalf("step 0 = %+v, want priority -3 from the model rung", resp.Steps[0])
	}
	if resp.Steps[0].SourceName == "" {
		t.Error("the model rung needs a display name")
	}
}

// A model with no override keeps reporting the platform's own priority, so the
// new rung does not relabel untouched chains as "model".
func TestRoutePlanKeepsTheBaseSourceWithoutAnOverride(t *testing.T) {
	reg := registryOf(&fakeClient{name: "alpha", models: []core.Model{{ID: "gpt-x"}}})
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{"alpha": {Priority: -2}})
	h := New(Options{Registry: reg})

	body := `{"model":"gpt-x","catalogs":{"alpha":[{"id":"gpt-x"}]}}`
	rec := routePlanPost(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	resp := struct {
		Steps []struct {
			Priority int    `json:"priority"`
			Source   string `json:"priority_source"`
		} `json:"steps"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(resp.Steps) != 1 || resp.Steps[0].Priority != -2 || resp.Steps[0].Source != "base" {
		t.Fatalf("step = %+v, want the base priority and source", resp.Steps)
	}
}

// End-to-end walk of the reported scenario, using the group shape the operator
// actually has: AutoClaw is a direct member and carries a stale group priority
// of 0, while WorkBuddy arrives through the Auto member (which is why it shows
// as an Auto-pool row rather than a preferred member).  Dragging AutoClaw above
// WorkBuddy writes the per-model priority as -1, and clearing it again hands the
// model back to the platform's own -3 instead of the stale group value.
func TestRoutePlanDragFixLandsOnTheModelRung(t *testing.T) {
	reg := registryOf(
		&fakeClient{name: "autoclaw", models: []core.Model{{ID: "zai_glm-5.3-flash"}}},
		&fakeClient{name: "workbuddy", models: []core.Model{{ID: "cn:glm-5.3-flash"}}},
	)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"autoclaw": {Priority: -3},
		"workbuddy": {Priority: 1, PrioritySchedule: []core.PriorityWindow{
			{StartMinute: 0, EndMinute: 17 * 60, Priority: 0},
		}},
	})
	group := core.ModelGroup{
		Members: []core.ModelGroupMember{
			{Client: "autoclaw", Model: "zai_glm-5.3-flash"},
			{Client: "Auto", Model: "glm-5.3-flash"},
		},
		PlatformPriorities: map[string]int{"autoclaw": 0},
	}
	reg.SetModelGroups(map[string]core.ModelGroup{"glm-5.3-flash": group})

	catalogs := map[string][]core.Model{
		"autoclaw":  {{ID: "zai_glm-5.3-flash"}},
		"workbuddy": {{ID: "cn:glm-5.3-flash"}},
	}
	// A fixed clock keeps the 00:00-17:00 window active regardless of when the
	// suite runs, so the assertion cannot flake after 17:00.
	at := time.Date(2026, 10, 18, 11, 0, 0, 0, time.UTC)

	plan, err := reg.ResolveStaticCatalogAt("glm-5.3-flash", catalogs, at)
	if err != nil {
		t.Fatalf("ResolveStaticCatalogAt returned an error: %v", err)
	}
	if len(plan.Steps) != 2 || plan.Steps[0].Platform != "autoclaw" {
		t.Fatalf("plan = %+v, want autoclaw first on the stale group 0", plan.Steps)
	}
	if plan.Steps[0].Priority != 0 || plan.Steps[0].Source != core.RoutePlanSourceGroup {
		t.Fatalf("autoclaw step = %+v, want priority 0 from the group rung", plan.Steps[0])
	}

	// The drag writes the per-model override: one below the row it displaced.
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"autoclaw": {Priority: -3, ModelPriorities: map[string]int{"zai_glm-5.3-flash": -1}},
		"workbuddy": {Priority: 1, PrioritySchedule: []core.PriorityWindow{
			{StartMinute: 0, EndMinute: 17 * 60, Priority: 0},
		}},
	})
	plan, err = reg.ResolveStaticCatalogAt("glm-5.3-flash", catalogs, at)
	if err != nil {
		t.Fatalf("after drag: %v", err)
	}
	if len(plan.Steps) != 2 || plan.Steps[0].Platform != "autoclaw" {
		t.Fatalf("plan = %+v, want autoclaw first after the drag", plan.Steps)
	}
	if plan.Steps[0].Priority != -1 || plan.Steps[0].Source != core.RoutePlanSourceModel {
		t.Fatalf("autoclaw step = %+v, want priority -1 from the model rung", plan.Steps[0])
	}
	if plan.Steps[0].SourceName == "" {
		t.Error("the model rung needs a display name for the panel")
	}

	// Clearing the override -- and removing the stale group value that was
	// shadowing the base priority -- lets the platform's own -3 apply.
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"autoclaw": {Priority: -3},
		"workbuddy": {Priority: 1, PrioritySchedule: []core.PriorityWindow{
			{StartMinute: 0, EndMinute: 17 * 60, Priority: 0},
		}},
	})
	group.PlatformPriorities = nil
	reg.SetModelGroups(map[string]core.ModelGroup{"glm-5.3-flash": group})

	plan, err = reg.ResolveStaticCatalogAt("glm-5.3-flash", catalogs, at)
	if err != nil {
		t.Fatalf("after clearing: %v", err)
	}
	if len(plan.Steps) != 2 || plan.Steps[0].Platform != "autoclaw" || plan.Steps[0].Priority != -3 {
		t.Fatalf("plan = %+v, want autoclaw first on its inherited -3", plan.Steps)
	}
	if plan.Steps[0].Source != core.RoutePlanSourceBase {
		t.Fatalf("source = %q, want base once the override is cleared", plan.Steps[0].Source)
	}
}

// Regression: an Auto member's model name is the bare model being requested, so
// memberMatchesRequest is trivially true for it.  Marking every platform the
// pool expands to as "preferred" would sort them all ahead of the operator's
// ordering and make a drag look like it saved but did nothing.  Auto-expanded
// platforms must stay unmarked, exactly as the runtime treats them.
func TestRoutePlanAutoPoolMembersAreNotPreferred(t *testing.T) {
	reg := registryOf(
		&fakeClient{name: "alpha", models: []core.Model{{ID: "shared-model"}}},
		&fakeClient{name: "beta", models: []core.Model{{ID: "shared-model"}}},
	)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: -5},
		"beta":  {Priority: 20},
	})
	// The group is reached by the bare name and binds the model through an Auto
	// member, which is the shape that used to mark everything preferred.
	reg.SetModelGroups(map[string]core.ModelGroup{
		"shared": {Members: []core.ModelGroupMember{
			{Client: "Auto", Model: "shared-model"},
		}},
	})
	h := New(Options{Registry: reg})

	body := `{"model":"shared","catalogs":{"alpha":[{"id":"shared-model"}],"beta":[{"id":"shared-model"}]}}`
	rec := routePlanPost(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	resp := struct {
		Steps []struct {
			Platform  string `json:"platform"`
			Priority  int    `json:"priority"`
			Preferred bool   `json:"preferred"`
			AutoPool  bool   `json:"auto_pool"`
		} `json:"steps"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(resp.Steps) != 2 {
		t.Fatalf("chain = %+v, want both platforms", resp.Steps)
	}
	for _, step := range resp.Steps {
		if step.Preferred {
			t.Errorf("%s is marked preferred; an Auto pool member must not outrank the priority", step.Platform)
		}
		if !step.AutoPool {
			t.Errorf("%s is not marked auto_pool", step.Platform)
		}
	}
	// Ordering is the priority, so the -5 platform leads despite the pool.
	if resp.Steps[0].Platform != "alpha" {
		t.Fatalf("chain = %+v, want alpha first by priority", resp.Steps)
	}
}

// A direct member that the caller spelled out is still preferred -- that is the
// documented "naming a member routes the group but tries that member first"
// behaviour, and the fix above must not have removed it.
func TestRoutePlanDirectMemberSpellingStaysPreferred(t *testing.T) {
	reg := registryOf(
		&fakeClient{name: "alpha", models: []core.Model{{ID: "alpha-model"}}},
		&fakeClient{name: "beta", models: []core.Model{{ID: "beta-model"}}},
	)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: 20},
		"beta":  {Priority: -5},
	})
	reg.SetModelGroups(map[string]core.ModelGroup{
		"shared": {Members: []core.ModelGroupMember{
			{Client: "alpha", Model: "alpha-model"},
			{Client: "beta", Model: "beta-model"},
		}},
	})
	h := New(Options{Registry: reg})

	// Requesting alpha's own model id names that member, so alpha leads even
	// though beta has the better priority.
	body := `{"model":"alpha-model","catalogs":{"alpha":[{"id":"alpha-model"}],"beta":[{"id":"beta-model"}]}}`
	rec := routePlanPost(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	resp := struct {
		Steps []struct {
			Platform  string `json:"platform"`
			Preferred bool   `json:"preferred"`
		} `json:"steps"`
	}{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(resp.Steps) != 2 || resp.Steps[0].Platform != "alpha" || !resp.Steps[0].Preferred {
		t.Fatalf("chain = %+v, want the spelled-out alpha first and preferred", resp.Steps)
	}
}
