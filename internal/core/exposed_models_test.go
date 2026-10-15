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
