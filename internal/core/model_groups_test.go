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
