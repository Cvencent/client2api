package core

import (
	"context"
	"strings"
	"testing"
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

	for _, requested := range []string{"shared", "alpha-model", "beta-model"} {
		gotClient, gotModel := resolveOK(t, r, requested)
		if gotClient != "beta" || gotModel != "beta-model" {
			t.Fatalf("Resolve(%q) = (%q, %q), want (beta, beta-model)", requested, gotClient, gotModel)
		}
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
