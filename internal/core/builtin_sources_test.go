package core

import (
	"reflect"
	"sync"
	"testing"
)

var registerBuiltinSourceTestOnce sync.Once

func registerBuiltinSourceTest(t *testing.T) string {
	t.Helper()
	const id = "zz-builtin-source-test"
	registerBuiltinSourceTestOnce.Do(func() {
		RegisterBuiltinSource(id, SourceConfig{
			Label:   "shipped test platform",
			BaseURL: "https://builtin.example.test/v1",
		})
	})
	return id
}

func TestBuiltinSourcesStayAvailableWithoutOperatorConfig(t *testing.T) {
	id := registerBuiltinSourceTest(t)
	got := WithBuiltinSources(nil)
	cfg, ok := got[id]
	if !ok {
		t.Fatalf("WithBuiltinSources(nil) omitted %q", id)
	}
	if cfg.Label != "shipped test platform" || cfg.BaseURL != "https://builtin.example.test/v1" {
		t.Fatalf("builtin default = %+v", cfg)
	}
}

func TestOperatorConfigOverridesAndDisablesBuiltinSource(t *testing.T) {
	id := registerBuiltinSourceTest(t)
	override := SourceConfig{Label: "operator label", BaseURL: "https://operator.example.test/v1"}
	got := WithBuiltinSources(map[string]SourceConfig{id: override})
	if !reflect.DeepEqual(got[id], override) {
		t.Fatalf("override = %+v, want %+v", got[id], override)
	}

	disabled := override
	disabled.Disabled = true
	got = WithBuiltinSources(map[string]SourceConfig{id: disabled})
	if !got[id].Disabled || got[id].BaseURL != override.BaseURL {
		t.Fatalf("disabled override = %+v, want disabled copy of %+v", got[id], override)
	}
}
