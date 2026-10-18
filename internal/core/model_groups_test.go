package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestModelGroupRoutesGroupNameByPerModelPriority(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "alpha-model"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "beta-model"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: 1},
		"beta":  {Priority: 2},
	})
	r.SetModelGroups(map[string]ModelGroup{
		"shared": {
			Members: []ModelGroupMember{
				{Client: "alpha", Model: "alpha-model"},
				{Client: "beta", Model: "beta-model"},
			},
			PlatformPriorities: map[string]int{"alpha": 5, "beta": -5},
		},
	})

	gotClient, gotModel := resolveOK(t, r, "shared")
	if gotClient != "beta" || gotModel != "beta-model" {
		t.Fatalf("Resolve(shared) = (%q, %q), want (beta, beta-model)", gotClient, gotModel)
	}
}

func TestModelGroupMemberNameRoutesWholeGroup(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "alpha-model"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "beta-model"}}},
	)
	r.SetModelGroups(map[string]ModelGroup{
		"shared": {
			Members: []ModelGroupMember{
				{Client: "alpha", Model: "alpha-model"},
				{Client: "beta", Model: "beta-model"},
			},
			PlatformPriorities: map[string]int{"alpha": 5, "beta": 1},
		},
	})

	// The bare group name follows the configured priority.
	if gotClient, gotModel := resolveOK(t, r, "shared"); gotClient != "beta" || gotModel != "beta-model" {
		t.Fatalf("Resolve(shared) = (%q, %q), want (beta, beta-model)", gotClient, gotModel)
	}
	// Naming a member routes through the whole group but prefers that member,
	// even when the operator ranked a sibling higher.
	if gotClient, gotModel := resolveOK(t, r, "alpha-model"); gotClient != "alpha" || gotModel != "alpha-model" {
		t.Fatalf("Resolve(alpha-model) = (%q, %q), want (alpha, alpha-model)", gotClient, gotModel)
	}
	if gotClient, gotModel := resolveOK(t, r, "beta-model"); gotClient != "beta" || gotModel != "beta-model" {
		t.Fatalf("Resolve(beta-model) = (%q, %q), want (beta, beta-model)", gotClient, gotModel)
	}
}

func TestModelGroupExplicitPlatformStillLocksToOneMember(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "alpha-model"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "beta-model"}}},
	)
	r.SetModelGroups(map[string]ModelGroup{
		"shared": {
			Members: []ModelGroupMember{
				{Client: "alpha", Model: "alpha-model"},
				{Client: "beta", Model: "beta-model"},
			},
			PlatformPriorities: map[string]int{"alpha": 5, "beta": -5},
		},
	})

	gotClient, gotModel := resolveOK(t, r, "alpha/alpha-model")
	if gotClient != "alpha" || gotModel != "alpha-model" {
		t.Fatalf("Resolve(alpha/alpha-model) = (%q, %q), want the explicit alpha member", gotClient, gotModel)
	}
}

func TestModelGroupPriorityFallsBackToPlatformPriority(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "alpha-model"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "beta-model"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: 20},
		"beta":  {Priority: 10},
	})
	r.SetModelGroups(map[string]ModelGroup{
		"shared": {Members: []ModelGroupMember{
			{Client: "alpha", Model: "alpha-model"},
			{Client: "beta", Model: "beta-model"},
		}},
	})

	gotClient, gotModel := resolveOK(t, r, "shared")
	if gotClient != "beta" || gotModel != "beta-model" {
		t.Fatalf("Resolve(shared) = (%q, %q), want global priority to select beta", gotClient, gotModel)
	}
}

func TestModelGroupSkipsMissingMembersAndDisabledModels(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "alpha-model"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "beta-model"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: -100, DisabledModels: []string{"alpha-model"}},
		"beta":  {Priority: 10},
	})
	r.SetModelGroups(map[string]ModelGroup{
		"shared": {Members: []ModelGroupMember{
			{Client: "missing", Model: "gone"},
			{Client: "alpha", Model: "alpha-model"},
			{Client: "beta", Model: "beta-model"},
		}},
	})

	gotClient, gotModel := resolveOK(t, r, "shared")
	if gotClient != "beta" || gotModel != "beta-model" {
		t.Fatalf("Resolve(shared) = (%q, %q), want the only allowed member", gotClient, gotModel)
	}
}

func TestModelGroupReturnsClearErrorWhenNoMemberIsAvailable(t *testing.T) {
	r := registryWith(t)

	r.SetModelGroups(map[string]ModelGroup{
		"shared": {Members: []ModelGroupMember{{Client: "missing", Model: "gone"}}},
	})

	_, _, err := r.Resolve(context.Background(), "shared")
	if err == nil {
		t.Fatal("Resolve accepted a group with no live members")
	}
	if !strings.Contains(err.Error(), "model group") || !strings.Contains(err.Error(), "shared") {
		t.Fatalf("group error = %q, want it to name the model group", err)
	}
}

func TestModelGroupAutoMemberExpandsEveryLivePlatform(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "gpt-6.1-sol"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "vendor/gpt-6.1-sol"}}},
		&fakeClient{name: "gamma", models: []Model{{ID: "other-model"}}},
	)
	r.SetModelGroups(map[string]ModelGroup{
		"gpt-6.1-sol": {
			Members: []ModelGroupMember{{Client: "Auto", Model: "gpt-6.1-sol"}},
		},
	})

	got, err := r.ResolveCandidates(context.Background(), "gpt-6.1-sol")
	if err != nil {
		t.Fatalf("ResolveCandidates returned an error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ResolveCandidates returned %d candidates, want alpha and beta: %+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, candidate := range got {
		seen[candidate.Client.Name()] = true
	}
	if !seen["alpha"] || !seen["beta"] {
		t.Fatalf("dynamic Auto member did not expand every platform: %+v", got)
	}
}

func TestModelGroupAutoMemberKeepsAutoPlatformPriority(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "shared-model"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "shared-model"}}},
		&fakeClient{name: "gamma", models: []Model{{ID: "gamma-model"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: 20},
		"beta":  {Priority: 10},
		"gamma": {Priority: 0},
	})
	r.SetModelGroups(map[string]ModelGroup{
		"shared": {
			Members: []ModelGroupMember{
				{Client: "Auto", Model: "shared-model"},
				{Client: "gamma", Model: "gamma-model"},
			},
			// This moves the whole Auto pool ahead of gamma, but must not
			// flatten the platform priority used inside that pool.
			PlatformPriorities: map[string]int{"Auto": -100},
		},
	})

	got, err := r.ResolveCandidates(context.Background(), "shared")
	if err != nil {
		t.Fatalf("ResolveCandidates returned an error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ResolveCandidates returned %d candidates, want three: %+v", len(got), got)
	}
	want := []string{"beta", "alpha", "gamma"}
	for i, name := range want {
		if got[i].Client.Name() != name {
			t.Fatalf("candidate %d = %q, want %q (all candidates: %+v)", i, got[i].Client.Name(), name, got)
		}
	}
}

func TestAutoPrefixBypassesModelGroupLookup(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "gpt-6.1-sol"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "vendor/gpt-6.1-sol"}}},
	)
	r.SetModelGroups(map[string]ModelGroup{
		"gpt-6.1-sol": {
			Members: []ModelGroupMember{{Client: "Auto", Model: "gpt-6.1-sol"}},
		},
	})

	got, err := r.ResolveCandidates(context.Background(), "Auto/gpt-6.1-sol")
	if err != nil {
		t.Fatalf("ResolveCandidates(Auto/gpt-6.1-sol) returned an error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Auto/ returned %d candidates, want alpha and beta: %+v", len(got), got)
	}
}

// A per-model priority is the most specific rung.  It must win over the
// model group's platform priority, so the number the operator typed on the
// platform card's model row is the one the request actually follows.
func TestModelPriorityOverridesGroupAndBasePriority(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "shared-model"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "shared-model"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: 50, ModelPriorities: map[string]int{"shared-model": -4}},
		"beta":  {Priority: 10},
	})
	r.SetModelGroups(map[string]ModelGroup{
		"shared": {
			Members: []ModelGroupMember{
				{Client: "alpha", Model: "shared-model"},
				{Client: "beta", Model: "shared-model"},
			},
			PlatformPriorities: map[string]int{"alpha": 5},
		},
	})

	got, err := r.ResolveCandidates(context.Background(), "shared")
	if err != nil {
		t.Fatalf("ResolveCandidates(shared) returned an error: %v", err)
	}
	if len(got) != 2 || got[0].Client.Name() != "alpha" {
		t.Fatalf("candidates = %+v, want alpha first via its per-model -4", got)
	}
}

// A model with no override still inherits the platform's effective priority,
// so the feature is additive: an empty map changes nothing.
func TestModelPriorityAbsentInheritsThePlatformPriority(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "shared-model"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "shared-model"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: 50, ModelPriorities: map[string]int{"other-model": -99}},
		"beta":  {Priority: 10},
	})

	got, err := r.ResolveCandidates(context.Background(), "shared-model")
	if err != nil {
		t.Fatalf("ResolveCandidates(shared-model) returned an error: %v", err)
	}
	if len(got) != 2 || got[0].Client.Name() != "beta" {
		t.Fatalf("candidates = %+v, want beta by the platform priority", got)
	}
}

// The override is keyed by the model id as written, and matching ignores case
// and surrounding space the same way the blacklist does.
func TestModelPriorityMatchingIgnoresCaseAndSpace(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "GLM-5.3-Flash"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "GLM-5.3-Flash"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: 50, ModelPriorities: map[string]int{"  glm-5.3-flash ": -1}},
		"beta":  {Priority: 10},
	})

	got, err := r.ResolveCandidates(context.Background(), "GLM-5.3-Flash")
	if err != nil {
		t.Fatalf("ResolveCandidates returned an error: %v", err)
	}
	if len(got) != 2 || got[0].Client.Name() != "alpha" {
		t.Fatalf("candidates = %+v, want alpha first despite the spelling", got)
	}
}

// EffectiveModelPriority is the single exported answer the panel uses to label
// a model row.  It must report the same number and the same rung the request
// path resolves, for each rung of the ladder.
func TestEffectiveModelPriorityReportsTheRungThatDecides(t *testing.T) {
	// The windows are matched in Beijing time, so the probe clock has to be in
	// that zone or the assertion would drift with the host's offset.
	at := time.Date(2026, 10, 18, 11, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "shared-model"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "shared-model"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: -3, PrioritySchedule: []PriorityWindow{
			{StartMinute: 0, EndMinute: 12 * 60, Priority: -7},
		}},
		"beta": {Priority: 1},
	})

	// Base rung, once no more specific value applies.
	if got, rung, ok := r.EffectiveModelPriority("beta", "shared-model", at); !ok || got != 1 || rung != "base" {
		t.Fatalf("beta = (%d, %q, %v), want the base 1", got, rung, ok)
	}
	// The schedule window wins over the base priority.
	if got, rung, ok := r.EffectiveModelPriority("alpha", "shared-model", at); !ok || got != -7 || rung != "schedule" {
		t.Fatalf("alpha = (%d, %q, %v), want the window -7", got, rung, ok)
	}
	// A group value wins over the schedule window.
	r.SetModelGroups(map[string]ModelGroup{
		"shared": {
			Members:            []ModelGroupMember{{Client: "alpha", Model: "shared-model"}},
			PlatformPriorities: map[string]int{"alpha": 5},
		},
	})
	if got, rung, ok := r.EffectiveModelPriority("alpha", "shared-model", at); !ok || got != 5 || rung != "group" {
		t.Fatalf("alpha = (%d, %q, %v), want the group 5", got, rung, ok)
	}
	// The per-model override wins over everything.
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: -3, ModelPriorities: map[string]int{"shared-model": -9}, PrioritySchedule: []PriorityWindow{
			{StartMinute: 0, EndMinute: 12 * 60, Priority: -7},
		}},
		"beta": {Priority: 1},
	})
	if got, rung, ok := r.EffectiveModelPriority("alpha", "shared-model", at); !ok || got != -9 || rung != "model" {
		t.Fatalf("alpha = (%d, %q, %v), want the model -9", got, rung, ok)
	}
	// An unknown platform is reported as such rather than as a real zero.
	if _, _, ok := r.EffectiveModelPriority("ghost", "shared-model", at); ok {
		t.Error("an unknown platform should report ok=false")
	}
}
