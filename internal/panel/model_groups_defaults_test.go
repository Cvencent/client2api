package panel

import (
	"net/http"
	"testing"

	"client2api/internal/core"
)

func TestConfigReadShowsDefaultGPT6ModelGroups(t *testing.T) {
	path := configFile(t, baseConfig)
	p := configPanel(path)
	registry := core.NewRegistry()
	registry.SetModelGroups(map[string]core.ModelGroup{
		"gpt-6-astra":     {Members: []core.ModelGroupMember{{Client: "opencode", Model: "gpt-6-astra"}}, Builtin: true},
		"gpt-6.1-sol":     {Members: []core.ModelGroupMember{{Client: "opencode", Model: "gpt-6.1-sol"}}, Builtin: true},
		"gpt-6-sol":       {Members: []core.ModelGroupMember{{Client: "opencode", Model: "gpt-6-sol"}, {Client: "openrouter", Model: "openai/gpt-6-sol"}}, Builtin: true},
		"gpt-6-luna":      {Members: []core.ModelGroupMember{{Client: "opencode", Model: "gpt-6-luna"}, {Client: "openrouter", Model: "openai/gpt-6-luna"}}, Builtin: true},
		"gpt-6.1-sol-pro": {Members: []core.ModelGroupMember{{Client: "openrouter", Model: "openai/gpt-6.1-sol-pro"}}, Builtin: true},
	})
	p.opts.Registry = registry
	_, out := doConfig(t, p, http.MethodGet, "")

	cfg, ok := out["config"].(map[string]any)
	if !ok {
		t.Fatalf("GET did not return a config object: %v", out)
	}
	groups, ok := cfg["model_groups"].(map[string]any)
	if !ok {
		t.Fatalf("GET did not expose model_groups: %v", cfg["model_groups"])
	}
	for _, name := range []string{
		"gpt-6-astra",
		"gpt-6.1-sol",
		"gpt-6-sol",
		"gpt-6-luna",
		"gpt-6.1-sol-pro",
	} {
		group, ok := groups[name].(map[string]any)
		if !ok {
			t.Errorf("config editor cannot see default model group %q: %v", name, groups)
			continue
		}
		members, _ := group["members"].([]any)
		if len(members) == 0 {
			t.Errorf("default model group %q has no visible members: %v", name, group)
		}
	}

	defaults, _ := out["model_group_defaults"].([]any)
	if len(defaults) != 5 {
		t.Fatalf("model_group_defaults = %v, want the five built-in GPT-6 groups", out["model_group_defaults"])
	}
}

func TestConfigPatchResponseShowsDefaultGPT6ModelGroups(t *testing.T) {
	path := configFile(t, baseConfig)
	p := configPanel(path)
	registry := core.NewRegistry()
	registry.SetModelGroups(map[string]core.ModelGroup{
		"gpt-6-sol": {Members: []core.ModelGroupMember{{Client: "opencode", Model: "gpt-6-sol"}}, Builtin: true},
	})
	p.opts.Registry = registry

	out := mustSave(t, p, `{"proxy":"http://127.0.0.1:9999"}`)
	cfg, ok := out["config"].(map[string]any)
	if !ok {
		t.Fatalf("PATCH did not return a config object: %v", out)
	}
	groups, ok := cfg["model_groups"].(map[string]any)
	if !ok || groups["gpt-6-sol"] == nil {
		t.Fatalf("PATCH did not return the effective default group: %v", cfg["model_groups"])
	}
}
