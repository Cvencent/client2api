package core

import "testing"

func TestExposedModelPolicyAllowsEverythingWhenUnconfigured(t *testing.T) {
	r := NewRegistry()

	if !r.ExposedModelAllowed("anything") {
		t.Fatal("an unconfigured exposed-model policy must allow every model")
	}
}

func TestExposedModelPolicyEmptyConfiguredDeniesEverything(t *testing.T) {
	r := NewRegistry()
	r.SetExposedModels(nil, true)

	if r.ExposedModelAllowed("anything") {
		t.Fatal("an explicitly empty exposed-model policy must deny every model")
	}
}

func TestExposedModelPolicyMatchesTrimmedCaseInsensitiveIDs(t *testing.T) {
	r := NewRegistry()
	r.SetExposedModels([]string{" Auto/DeepSeek-V4.1-Flash ", "alias"}, true)

	if !r.ExposedModelAllowed("auto/deepseek-v4.1-flash") {
		t.Fatal("exposed-model matching must ignore case and surrounding space")
	}
	if !r.ExposedModelAllowed("ALIAS") {
		t.Fatal("exposed-model matching must ignore case")
	}
	if r.ExposedModelAllowed("other") {
		t.Fatal("an unlisted model must be denied")
	}
}

func TestExposedModelPolicySupportsPlatformWildcards(t *testing.T) {
	r := NewRegistry()
	r.SetExposedModels([]string{"my-relay/*"}, true)

	for _, id := range []string{"my-relay/model-a", "MY-RELAY/sub/model"} {
		if !r.ExposedModelAllowed(id) {
			t.Fatalf("my-relay/* must allow %q", id)
		}
	}
	if r.ExposedModelAllowed("workbuddy/glm-5.3") {
		t.Fatal("a platform wildcard must not allow another platform")
	}
}

func TestExposedModelPolicyReturnsTheConfiguredDisplayIDs(t *testing.T) {
	r := NewRegistry()
	r.SetExposedModels([]string{"Auto/DeepSeek-V4.1-Flash", " auto/deepseek-v4.1-flash ", "alias"}, true)

	got := r.ExposedModelIDs()
	want := []string{"Auto/DeepSeek-V4.1-Flash", "alias"}
	if len(got) != len(want) {
		t.Fatalf("ExposedModelIDs() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ExposedModelIDs() = %#v, want %#v", got, want)
		}
	}
}

func TestExposedModelPolicyAllowsMembersOfASelectedGroup(t *testing.T) {
	r := NewRegistry()
	r.Add(&fakeClient{name: "autoclaw", models: []Model{{ID: "zai_glm-5.3-flash"}}})
	r.SetModelGroups(map[string]ModelGroup{
		"glm-5.3-flash": {Members: []ModelGroupMember{
			{Client: "autoclaw", Model: "zai_glm-5.3-flash"},
		}},
	})
	r.SetExposedModels([]string{"glm-5.3-flash"}, true)

	if !r.ExposedModelCallable("autoclaw/zai_glm-5.3-flash") {
		t.Fatal("a member of a selected model group must remain callable by its qualified id")
	}
	if r.ExposedModelCallable("autoclaw/not-in-a-selected-group") {
		t.Fatal("group membership must not open unrelated models")
	}
}

func TestExposedModelCallableDoesNotWidenAliasesOrBareMembers(t *testing.T) {
	r := NewRegistry()
	r.Add(&fakeClient{name: "autoclaw", models: []Model{{ID: "zai_glm-5.3-flash"}}})
	r.SetModelGroups(map[string]ModelGroup{
		"glm-5.3-flash": {Members: []ModelGroupMember{
			{Client: "autoclaw", Model: "zai_glm-5.3-flash"},
		}},
	})
	r.AddAlias("flash-alias", "autoclaw/zai_glm-5.3-flash")
	r.SetExposedModels([]string{"glm-5.3-flash"}, true)

	for _, id := range []string{
		"flash-alias",
		"Auto/GLM-5.3-Flash",
	} {
		if r.ExposedModelCallable(id) {
			t.Fatalf("selected group must not open unrelated spelling %q", id)
		}
	}
	if r.ExposedModelAllowed("autoclaw/zai_glm-5.3-flash") {
		t.Fatal("a group member must stay callable without becoming a separately advertised id")
	}
}
