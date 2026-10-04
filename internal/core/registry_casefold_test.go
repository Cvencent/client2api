package core

import (
	"context"
	"testing"
)

func TestResolveMatchesAliasesAndQualifiedNamesCaseInsensitively(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "WorkBuddy", models: []Model{{ID: "DeepSeek-V4.1-Flash"}}})
	r.AddAlias("MyFlash", "workbuddy/deepseek-v4.1-flash")

	for _, request := range []string{"myflash", "WORKBUDDY/deepseek-v4.1-flash"} {
		gotClient, gotModel := resolveOK(t, r, request)
		if gotClient != "WorkBuddy" || gotModel != "DeepSeek-V4.1-Flash" {
			t.Fatalf("Resolve(%q) = (%q, %q), want (WorkBuddy, DeepSeek-V4.1-Flash)", request, gotClient, gotModel)
		}
	}
}

func TestResolveCandidatesMatchesBareNamesCaseInsensitivelyForEveryOwner(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "tabbit", models: []Model{{ID: "DeepSeek-V4.1-Flash"}}},
		&fakeClient{name: "cline", models: []Model{{ID: "deepseek-v4.1-flash", Extra: map[string]any{"free": true}}}},
	)

	candidates, err := r.ResolveCandidates(context.Background(), "DEEPSEEK-V4.1-FLASH")
	if err != nil {
		t.Fatalf("ResolveCandidates returned an error: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("ResolveCandidates returned %d owners, want both platforms: %#v", len(candidates), candidates)
	}
	if got := candidates[0].Client.Name(); got != "cline" {
		t.Fatalf("first candidate = %q, want the free cline model before tabbit", got)
	}
	wantModels := map[string]string{"tabbit": "DeepSeek-V4.1-Flash", "cline": "deepseek-v4.1-flash"}
	for _, candidate := range candidates {
		if got := candidate.Model; got != wantModels[candidate.Client.Name()] {
			t.Errorf("candidate %s model = %q, want the catalogue spelling %q", candidate.Client.Name(), got, wantModels[candidate.Client.Name()])
		}
	}
}
