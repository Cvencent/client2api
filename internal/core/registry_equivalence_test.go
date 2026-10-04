package core

import (
	"context"
	"testing"
)

func TestResolveCandidatesMatchesNamespaceEquivalentBareModel(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "opencode", models: []Model{{ID: "deepseek-v4.1-flash"}}},
		&fakeClient{name: "cline", models: []Model{{ID: "cline-free/deepseek-v4.1-flash", Extra: map[string]any{"free": true}}}},
		&fakeClient{name: "openrouter", models: []Model{{ID: "deepseek/deepseek-v4.1-flash", Extra: map[string]any{"free": true}}}},
		&fakeClient{name: "workbuddy", models: []Model{{ID: "cn:deepseek-v4.1-flash"}}},
		&fakeClient{name: "tabbit", models: []Model{{ID: "DeepSeek-V4.1-Flash"}}},
	)
	r.SetPlatformConfigs(map[string]PlatformConfig{
		"cline":      {Priority: 1},
		"opencode":   {Priority: 1},
		"openrouter": {Priority: 1},
		"tabbit":     {Priority: 1},
		"workbuddy":  {Priority: 2},
	})

	candidates, err := r.ResolveCandidates(context.Background(), "DEEPSEEK-V4.1-FLASH")
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}

	want := map[string]string{
		"cline":      "cline-free/deepseek-v4.1-flash",
		"opencode":   "deepseek-v4.1-flash",
		"openrouter": "deepseek/deepseek-v4.1-flash",
		"tabbit":     "DeepSeek-V4.1-Flash",
		"workbuddy":  "cn:deepseek-v4.1-flash",
	}
	if len(candidates) != len(want) {
		t.Fatalf("got %d candidates, want %d: %+v", len(candidates), len(want), candidates)
	}
	for _, candidate := range candidates {
		name := candidate.Client.Name()
		if got, ok := want[name]; !ok || candidate.Model != got {
			t.Errorf("candidate %s model = %q, want %q", name, candidate.Model, got)
		}
	}
	if got := candidates[0].Client.Name(); got != "cline" {
		t.Fatalf("first candidate = %q, want the free cline entry before the other equal-priority platforms", got)
	}
}

func TestResolveCandidatesKeepsNamespacedRequestsExact(t *testing.T) {
	r := registryWith(t,
		&fakeClient{name: "cline", models: []Model{{ID: "cline-free/deepseek-v4.1-flash", Extra: map[string]any{"free": true}}}},
		&fakeClient{name: "openrouter", models: []Model{{ID: "deepseek/deepseek-v4.1-flash", Extra: map[string]any{"free": true}}}},
	)

	candidates, err := r.ResolveCandidates(context.Background(), "deepseek/deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Client.Name() != "openrouter" {
		t.Fatalf("namespaced request candidates = %+v, want only openrouter", candidates)
	}
}

func TestResolveCandidatesPrefersFreeEquivalentWithinOnePlatform(t *testing.T) {
	r := registryWith(t, &fakeClient{name: "cline", models: []Model{
		{ID: "deepseek/deepseek-v4.1-flash"},
		{ID: "cline-free/deepseek-v4.1-flash", Extra: map[string]any{"free": true}},
	}})

	candidates, err := r.ResolveCandidates(context.Background(), "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("ResolveCandidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("got %d candidates, want one per platform: %+v", len(candidates), candidates)
	}
	if got := candidates[0].Model; got != "cline-free/deepseek-v4.1-flash" {
		t.Fatalf("candidate model = %q, want the free equivalent", got)
	}
}
