package panel

import (
	"encoding/json"
	"testing"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

func TestModelsListIncludesResolvedMetadataSources(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("workbuddy", "glm-5.2", 0, 777); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{name: "workbuddy", models: []core.Model{{ID: "glm-5.2", Extra: map[string]any{"context_length": 123456}}}}
	h := New(Options{Registry: registryOf(client), ModelOverrides: store})
	rec := get(t, h, "/panel/api/models")
	var got modelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	m := got.Clients[0].Models[0]
	if m.ContextLength != 123456 || m.ContextSource != modelmeta.SourceVendor {
		t.Fatalf("context = %d source=%q", m.ContextLength, m.ContextSource)
	}
	if m.MaxOutputTokens != 777 || m.MaxOutputSource != modelmeta.SourceManual {
		t.Fatalf("max_output = %d source=%q", m.MaxOutputTokens, m.MaxOutputSource)
	}
}

// TestModelsListPublishesThePreOverrideDefault pins the field the row's
// "restore default" button writes back: the value that would apply with no
// manual override, i.e. the upstream value or the official preset.
func TestModelsListPublishesThePreOverrideDefault(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	// A manual context override on a model whose vendor reported nothing: the
	// default must fall back to the official preset, not the manual value.
	if err := store.Set("zcode", "glm-5.3-flash", 4242, 0); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{name: "zcode", models: []core.Model{{ID: "glm-5.3-flash"}}}
	h := New(Options{Registry: registryOf(client), ModelOverrides: store})
	rec := get(t, h, "/panel/api/models")
	var got modelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	m := got.Clients[0].Models[0]
	if m.ContextLength != 4242 || m.ContextSource != modelmeta.SourceManual {
		t.Fatalf("context = %d source=%q, want the manual 4242", m.ContextLength, m.ContextSource)
	}
	if m.ContextDefault != 1048576 {
		t.Fatalf("context_default = %d, want the official 1048576 preset", m.ContextDefault)
	}
	if m.MaxOutputDefault != 131072 {
		t.Fatalf("max_output_default = %d, want the official 131072 preset", m.MaxOutputDefault)
	}
	if !m.ContextEditable || !m.MaxOutputEditable {
		t.Fatal("a wired override store must make both fields editable")
	}
}

func TestModelsListPublishesEffortPolicyAndManualProvenance(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEfforts("workbuddy", "cn:glm-5.3", []string{"low", "high", "max"}, "max"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetElevateTop("workbuddy", "cn:glm-5.3", true); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{name: "workbuddy", models: []core.Model{{ID: "cn:glm-5.3"}}}
	h := New(Options{Registry: registryOf(client), ModelOverrides: store})

	rec := get(t, h, "/panel/api/models")
	var got modelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	m := got.Clients[0].Models[0]
	if len(m.SupportedEfforts) != 3 || m.SupportedEfforts[2] != "max" {
		t.Fatalf("supported_efforts = %v, want [low high max]", m.SupportedEfforts)
	}
	if m.DefaultEffort != "max" || m.DefaultEffortSource != modelmeta.SourceManual {
		t.Fatalf("default effort = %q source=%q, want max/manual", m.DefaultEffort, m.DefaultEffortSource)
	}
	if m.EffortsSource != modelmeta.SourceManual {
		t.Fatalf("efforts source = %q, want manual", m.EffortsSource)
	}
	if !m.ElevateTop {
		t.Fatal("elevate_top = false, want true")
	}
	if !m.EffortsEditable || !m.ElevateTopEditable {
		t.Fatal("a wired override store must make the effort policy editable")
	}
}
