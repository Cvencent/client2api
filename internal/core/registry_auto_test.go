package core

import (
	"context"
	"testing"
)

func TestResolveAutoUsesTheSameBareModelCandidates(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "GLM-5.3"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "glm-5.3"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: 20},
		"beta":  {Priority: 10},
	})

	gotClient, gotModel := resolveOK(t, r, "Auto/GLM-5.3")
	if gotClient != "beta" || gotModel != "glm-5.3" {
		t.Fatalf("Resolve = (%q, %q), want (beta, glm-5.3)", gotClient, gotModel)
	}
}

func TestResolveAutoCandidatesKeepPriorityAndFailoverOrder(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "alpha", models: []Model{{ID: "deepseek-v4.1-flash"}}},
		&fakeClient{name: "beta", models: []Model{{ID: "DeepSeek-V4.1-Flash"}}},
		&fakeClient{name: "gamma", models: []Model{{ID: "other"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"alpha": {Priority: 30},
		"beta":  {Priority: 10},
	})

	candidates, err := r.ResolveCandidates(context.Background(), "Auto/DeepSeek-V4.1-Flash")
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want 2: %+v", len(candidates), candidates)
	}
	if candidates[0].Client.Name() != "beta" || candidates[1].Client.Name() != "alpha" {
		t.Fatalf("candidate order = [%s, %s], want [beta, alpha]",
			candidates[0].Client.Name(), candidates[1].Client.Name())
	}
}

// WorkBuddy publishes one bare model per realm.  Auto must keep every
// equivalent realm id on that platform: the cn entry can be unusable while the
// global entry is healthy, and the caller never asked for a realm.
func TestResolveAutoKeepsEveryEquivalentRealmCandidate(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "workbuddy", models: []Model{
		{ID: "cn:deepseek-v4.1-flash"},
		{ID: "global:deepseek-v4.1-flash"},
	}})

	candidates, err := r.ResolveCandidates(context.Background(), "Auto/DeepSeek-V4.1-Flash")
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("got %d candidates, want both realm entries: %+v", len(candidates), candidates)
	}
	want := map[string]bool{
		"cn:deepseek-v4.1-flash":     true,
		"global:deepseek-v4.1-flash": true,
	}
	for _, candidate := range candidates {
		if candidate.Client.Name() != "workbuddy" {
			t.Fatalf("candidate client = %q, want workbuddy", candidate.Client.Name())
		}
		if !want[candidate.Model] {
			t.Fatalf("unexpected model candidate %q", candidate.Model)
		}
		delete(want, candidate.Model)
	}
	if len(want) != 0 {
		t.Fatalf("missing realm candidates: %v", want)
	}
}
func TestResolveAutoIsCaseInsensitive(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "alpha", models: []Model{{ID: "GLM-5.3"}}})

	gotClient, gotModel := resolveOK(t, r, "aUtO/glm-5.3")
	if gotClient != "alpha" || gotModel != "GLM-5.3" {
		t.Fatalf("Resolve = (%q, %q), want (alpha, GLM-5.3)", gotClient, gotModel)
	}
}

func TestResolveAutoWithNoModelIsRejected(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "alpha", models: []Model{{ID: "GLM-5.3"}}})

	for _, model := range []string{"Auto", "Auto/", "Auto/   "} {
		if _, _, err := r.Resolve(context.Background(), model); err == nil {
			t.Fatalf("Resolve(%q) accepted an empty Auto model", model)
		}
	}
}
