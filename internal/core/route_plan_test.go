package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

// countingClient counts every Models call; the static route plan must never
// touch a live catalogue and must never consult Status.
type countingClient struct {
	name        string
	models      []Model
	modelCalls  int
	statusCalls int
}

func (c *countingClient) Name() string { return c.name }

func (c *countingClient) Models(_ context.Context) ([]Model, error) {
	c.modelCalls++
	return c.models, nil
}

func (c *countingClient) Chat(_ context.Context, _ *ChatRequest) (Stream, error) {
	return nil, errFakeNoChat
}

func (c *countingClient) Status(_ context.Context) Status {
	c.statusCalls++
	return Status{Name: c.name}
}

func planPlatforms(t *testing.T, r *Registry, model string) ([]RoutePlanStep, RoutePlan) {
	t.Helper()
	catalogs := map[string][]Model{}
	for _, name := range r.Names() {
		c, ok := r.Get(name)
		if !ok {
			continue
		}
		models, err := c.Models(context.Background())
		if err != nil {
			continue
		}
		catalogs[name] = models
	}
	plan, err := r.ResolveStaticCatalog(model, catalogs)
	if err != nil {
		t.Fatalf("ResolveStaticCatalog(%q) failed: %v", model, err)
	}
	return plan.Steps, plan
}

func TestResolveStaticCatalogOrdersByBasePriority(t *testing.T) {
	r := registryWith(t,
		&countingClient{name: "beta", models: []Model{{ID: "gpt-x"}}},
		&countingClient{name: "alpha", models: []Model{{ID: "gpt-x"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: -2},
		"beta":  {Priority: 3},
	})

	steps, plan := planPlatforms(t, r, "Auto/gpt-x")
	if len(steps) != 2 {
		t.Fatalf("got %d steps, want 2: %+v", len(steps), steps)
	}
	if steps[0].Platform != "alpha" || steps[1].Platform != "beta" {
		t.Fatalf("chain = [%s, %s], want [alpha, beta]", steps[0].Platform, steps[1].Platform)
	}
	for i, step := range steps {
		if step.Source != RoutePlanSourceBase {
			t.Errorf("step %d source = %q, want base", i, step.Source)
		}
		if step.Position != i+1 {
			t.Errorf("step %d position = %d, want %d", i, step.Position, i+1)
		}
		if step.SourceName == "" {
			t.Errorf("step %d is missing a priority_source name", i)
		}
	}
	if plan.Model != "gpt-x" {
		t.Fatalf("plan.Model = %q, want gpt-x", plan.Model)
	}
}

func TestResolveStaticCatalogUsesScheduleWindow(t *testing.T) {
	r := registryWith(t,
		&countingClient{name: "beta", models: []Model{{ID: "gpt-x"}}},
		&countingClient{name: "alpha", models: []Model{{ID: "gpt-x"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: 9},
		"beta":  {Priority: 9, PrioritySchedule: []PriorityWindow{{StartMinute: 0, EndMinute: 1440, Priority: 1}}},
	})

	cst := time.FixedZone("CST", 8*60*60)
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, cst)

	catalogs := map[string][]Model{
		"beta":  {{ID: "gpt-x"}},
		"alpha": {{ID: "gpt-x"}},
	}
	plan, err := r.ResolveStaticCatalogAt("Auto/gpt-x", catalogs, now)
	if err != nil {
		t.Fatalf("ResolveStaticCatalogAt failed: %v", err)
	}
	if len(plan.Steps) != 2 || plan.Steps[0].Platform != "beta" {
		t.Fatalf("chain = %v, want beta first", plan.Steps)
	}
	if plan.Steps[0].Source != RoutePlanSourceSchedule {
		t.Fatalf("step source = %q, want schedule", plan.Steps[0].Source)
	}
	if plan.Steps[0].ScheduleHit == "" {
		t.Fatalf("schedule step should name the matched window")
	}
}

func TestResolveStaticCatalogUsesGroupOverrides(t *testing.T) {
	r := registryWith(t,
		&countingClient{name: "beta", models: []Model{{ID: "gpt-x"}}},
		&countingClient{name: "alpha", models: []Model{{ID: "gpt-x"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: 9},
		"beta":  {Priority: 9},
	})
	r.SetModelGroups(map[string]ModelGroup{
		"gpt-x": {
			Members: []ModelGroupMember{
				{Client: "alpha", Model: "gpt-x"},
				{Client: "beta", Model: "gpt-x"},
			},
			PlatformPriorities: map[string]int{"beta": -1},
		},
	})

	steps, _ := planPlatforms(t, r, "gpt-x")
	if len(steps) != 2 || steps[0].Platform != "beta" {
		t.Fatalf("chain = %v, want beta first", steps)
	}
	if steps[0].Source != RoutePlanSourceGroup {
		t.Fatalf("step source = %q, want group", steps[0].Source)
	}
	if steps[0].Group != "gpt-x" {
		t.Fatalf("step group = %q, want gpt-x", steps[0].Group)
	}
}

func TestResolveStaticCatalogAutoSkipsGroups(t *testing.T) {
	r := registryWith(t,
		&countingClient{name: "beta", models: []Model{{ID: "other"}}},
		&countingClient{name: "alpha", models: []Model{{ID: "gpt-x"}}},
	)
	r.SetModelGroups(map[string]ModelGroup{
		"gpt-x": {Members: []ModelGroupMember{{Client: "beta", Model: "other"}}},
	})

	steps, plan := planPlatforms(t, r, "Auto/gpt-x")
	if len(steps) != 1 || steps[0].Platform != "alpha" {
		t.Fatalf("chain = %v, want only alpha", steps)
	}
	if steps[0].Source != RoutePlanSourceBase {
		t.Fatalf("step source = %q, want base (Auto/ skips groups)", steps[0].Source)
	}
	if plan.Model != "gpt-x" {
		t.Fatalf("plan.Model = %q, want gpt-x", plan.Model)
	}
}

func TestResolveStaticCatalogGroupAutoMemberExpandsPool(t *testing.T) {
	r := registryWith(t,
		&countingClient{name: "beta", models: []Model{{ID: "gpt-x"}}},
		&countingClient{name: "alpha", models: []Model{{ID: "gpt-x"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: -2},
		"beta":  {Priority: 3},
	})
	r.SetModelGroups(map[string]ModelGroup{
		"gpt-x": {
			Members:            []ModelGroupMember{{Client: "Auto", Model: "gpt-x"}},
			PlatformPriorities: map[string]int{"Auto": 0},
		},
	})

	steps, _ := planPlatforms(t, r, "gpt-x")
	if len(steps) != 2 {
		t.Fatalf("got %d steps, want 2: %+v", len(steps), steps)
	}
	if steps[0].Platform != "alpha" || steps[1].Platform != "beta" {
		t.Fatalf("chain = [%s, %s], want [alpha, beta] (platform priority inside the pool)", steps[0].Platform, steps[1].Platform)
	}
	for i, step := range steps {
		if step.Source != RoutePlanSourceAutoPool {
			t.Errorf("step %d source = %q, want auto_pool", i, step.Source)
		}
		if !step.AutoPool {
			t.Errorf("step %d should be flagged auto_pool", i)
		}
		if step.Priority != step.Priority {
			t.Errorf("step %d priority missing", i)
		}
	}
}

func TestResolveStaticCatalogPrefersFreeOnTie(t *testing.T) {
	r := registryWith(t,
		&countingClient{name: "beta", models: []Model{{ID: "gpt-x"}}},
		&countingClient{name: "alpha", models: []Model{{ID: "gpt-x", Extra: map[string]any{"free": true}}}},
	)

	steps, _ := planPlatforms(t, r, "Auto/gpt-x")
	if len(steps) != 2 || steps[0].Platform != "alpha" {
		t.Fatalf("chain = %v, want alpha first", steps)
	}
	if !steps[0].Free {
		t.Fatalf("first step should carry the free marker")
	}
}

func TestResolveStaticCatalogSkipsBlacklistedPlatforms(t *testing.T) {
	r := registryWith(t,
		&countingClient{name: "beta", models: []Model{{ID: "gpt-x"}}},
		&countingClient{name: "alpha", models: []Model{{ID: "gpt-x"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: -5, DisabledModels: []string{"gpt-x"}},
	})

	steps, _ := planPlatforms(t, r, "Auto/gpt-x")
	if len(steps) != 1 || steps[0].Platform != "beta" {
		t.Fatalf("chain = %v, want only beta", steps)
	}
}

func TestResolveStaticCatalogRejectsEverywhereBlocked(t *testing.T) {
	r := registryWith(t,
		&countingClient{name: "beta", models: []Model{{ID: "gpt-x"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"beta": {DisabledModels: []string{"gpt-x"}},
	})

	_, err := r.ResolveStaticCatalog("Auto/gpt-x", map[string][]Model{
		"beta": {{ID: "gpt-x"}},
	})
	if err == nil {
		t.Fatal("ResolveStaticCatalog should fail when every listing platform blacklists the model")
	}
	if !strings.Contains(err.Error(), "beta") {
		t.Fatalf("error should name the blocking platform: %v", err)
	}
}

func TestResolveStaticCatalogNeverCallsUpstream(t *testing.T) {
	alpha := &countingClient{name: "alpha", models: []Model{{ID: "gpt-x"}}}
	r := registryWith(t, alpha)

	catalogs := map[string][]Model{"alpha": {{ID: "gpt-x"}}}
	if _, err := r.ResolveStaticCatalog("Auto/gpt-x", catalogs); err != nil {
		t.Fatalf("ResolveStaticCatalog failed: %v", err)
	}
	if alpha.modelCalls != 0 {
		t.Fatalf("Models called %d times, want 0", alpha.modelCalls)
	}
	if alpha.statusCalls != 0 {
		t.Fatalf("Status called %d times, want 0", alpha.statusCalls)
	}
}

func TestResolveStaticCatalogRequiresModel(t *testing.T) {
	r := registryWith(t)
	if _, err := r.ResolveStaticCatalog("  ", map[string][]Model{}); err == nil {
		t.Fatal("empty model should be rejected")
	}
}

// Writing client/model selects one platform even when the same model id is
// also a model-group member. A bare group name still falls back to siblings;
// the explicit platform choice must not.
func TestQualifiedGroupMemberIsAHardPlatformPin(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "loomy", models: []Model{{ID: "deepseek-v4.1-flash"}}},
		&fakeClient{name: "other", models: []Model{{ID: "deepseek-v4.1-flash"}}},
	)
	r.SetModelGroups(map[string]ModelGroup{
		"deepseek-v4.1-flash": {
			Members: []ModelGroupMember{
				{Client: "loomy", Model: "deepseek-v4.1-flash"},
				{Client: "other", Model: "deepseek-v4.1-flash"},
			},
		},
	})

	cands, err := r.ResolveCandidates(context.Background(), "loomy/deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("ResolveCandidates failed: %v", err)
	}
	if len(cands) != 1 || cands[0].Client.Name() != "loomy" {
		t.Fatalf("candidates = %v, want only loomy", cands)
	}
}

// The static preview must agree with the runtime about a hard pin: a
// qualified member of a group shows one step on the named platform, not the
// group's whole chain.
func TestResolveStaticCatalogHardPinBeatsGroupMembership(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "loomy", models: []Model{{ID: "deepseek-v4.1-flash"}}},
		&fakeClient{name: "other", models: []Model{{ID: "deepseek-v4.1-flash"}}},
	)
	r.SetModelGroups(map[string]ModelGroup{
		"deepseek-v4.1-flash": {
			Members: []ModelGroupMember{
				{Client: "loomy", Model: "deepseek-v4.1-flash"},
				{Client: "other", Model: "deepseek-v4.1-flash"},
			},
		},
	})

	catalogs := map[string][]Model{
		"loomy": {{ID: "deepseek-v4.1-flash"}},
		"other": {{ID: "deepseek-v4.1-flash"}},
	}

	pinned, err := r.ResolveStaticCatalog("loomy/deepseek-v4.1-flash", catalogs)
	if err != nil {
		t.Fatalf("ResolveStaticCatalog failed: %v", err)
	}
	if len(pinned.Steps) != 1 {
		t.Fatalf("pinned steps = %d, want 1 (the group chain must not apply)", len(pinned.Steps))
	}
	if pinned.Steps[0].Platform != "loomy" {
		t.Fatalf("pinned step platform = %q, want loomy", pinned.Steps[0].Platform)
	}
	if pinned.Steps[0].Preferred {
		t.Fatal("a hard pin is the whole chain, not a preferred member of one")
	}

	grouped, err := r.ResolveStaticCatalog("deepseek-v4.1-flash", catalogs)
	if err != nil {
		t.Fatalf("ResolveStaticCatalog(group) failed: %v", err)
	}
	if len(grouped.Steps) != 2 {
		t.Fatalf("group steps = %d, want 2", len(grouped.Steps))
	}
}
