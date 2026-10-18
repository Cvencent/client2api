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
	if m.ContextLength != 1000000 || m.ContextSource != modelmeta.SourceStatic {
		t.Fatalf("context = %d source=%q, want the client-published 1000000/static", m.ContextLength, m.ContextSource)
	}
	if m.MaxOutputTokens != 777 || m.MaxOutputSource != modelmeta.SourceManual {
		t.Fatalf("max_output = %d source=%q", m.MaxOutputTokens, m.MaxOutputSource)
	}
}

func TestModelsListPrefersClientDefaultsOverVendorExtra(t *testing.T) {
	client := &fakeClient{name: "workbuddy", models: []core.Model{{
		ID: "cn:deepseek-v4.1-flash",
		Extra: map[string]any{
			"context_length":    100000,
			"max_output_tokens": 100000,
		},
	}}}
	h := New(Options{Registry: registryOf(client)})
	rec := get(t, h, "/panel/api/models")
	var got modelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	m := got.Clients[0].Models[0]
	if m.ContextLength != 1000000 || m.ContextSource != modelmeta.SourceStatic {
		t.Fatalf("context = %d source=%q, want client default 1000000/static", m.ContextLength, m.ContextSource)
	}
	if m.MaxOutputTokens != 128000 || m.MaxOutputSource != modelmeta.SourceStatic {
		t.Fatalf("max_output = %d source=%q, want client default 128000/static", m.MaxOutputTokens, m.MaxOutputSource)
	}
	if m.ContextDefault != 1000000 || m.MaxOutputDefault != 128000 {
		t.Fatalf("defaults = %d/%d, want 1000000/128000", m.ContextDefault, m.MaxOutputDefault)
	}
}

// Regression: the request path and /v1/models read the same client preset
// through modelmeta.PreferLimits, so the panel's displayed default must match
// both of them when the live Extra is stale.
func TestModelsListClientDefaultsMatchGatewayResolution(t *testing.T) {
	client := &fakeClient{name: "workbuddy", models: []core.Model{{
		ID: "cn:deepseek-v4.1-flash",
		Extra: map[string]any{
			"context_length":    100000,
			"max_output_tokens": 100000,
		},
	}}}
	h := New(Options{Registry: registryOf(client)})
	rec := get(t, h, "/panel/api/models")
	var got modelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	m := got.Clients[0].Models[0]
	if m.ContextLength != 1000000 || m.ContextSource != modelmeta.SourceStatic {
		t.Fatalf("gateway-facing context = %d source=%q, want the client preset 1000000/static", m.ContextLength, m.ContextSource)
	}
	if m.MaxOutputTokens != 128000 || m.MaxOutputSource != modelmeta.SourceStatic {
		t.Fatalf("gateway-facing max_output = %d source=%q, want the client preset 128000/static", m.MaxOutputTokens, m.MaxOutputSource)
	}
}

func TestModelsListManualOverrideBeatsClientDefaults(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set("workbuddy", "cn:deepseek-v4.1-flash", 777, 888); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{name: "workbuddy", models: []core.Model{{
		ID: "cn:deepseek-v4.1-flash",
		Extra: map[string]any{
			"context_length":    100000,
			"max_output_tokens": 100000,
		},
	}}}
	h := New(Options{Registry: registryOf(client), ModelOverrides: store})
	rec := get(t, h, "/panel/api/models")
	var got modelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	m := got.Clients[0].Models[0]
	if m.ContextLength != 777 || m.ContextSource != modelmeta.SourceManual {
		t.Fatalf("context = %d source=%q, want manual 777", m.ContextLength, m.ContextSource)
	}
	if m.MaxOutputTokens != 888 || m.MaxOutputSource != modelmeta.SourceManual {
		t.Fatalf("max_output = %d source=%q, want manual 888", m.MaxOutputTokens, m.MaxOutputSource)
	}
	if m.ContextDefault != 1000000 || m.MaxOutputDefault != 128000 {
		t.Fatalf("defaults = %d/%d, want client defaults 1000000/128000", m.ContextDefault, m.MaxOutputDefault)
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
	if m.ContextDefault != 1000000 {
		t.Fatalf("context_default = %d, want the client-published 1000000 preset", m.ContextDefault)
	}
	if m.MaxOutputDefault != 128000 {
		t.Fatalf("max_output_default = %d, want the client-published 128000 preset", m.MaxOutputDefault)
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
	if !m.ElevateTopManual {
		t.Fatal("a stored elevate_top choice must be reported as manual")
	}
}

// TestModelsListDefaultsCapAlignmentOnForNonV41Models pins the all-model
// default: any ordinary catalogue row is shown with the box checked before the
// operator touches it, and unchecking it becomes an explicit off that still lets
// the row restore the default.
func TestModelsListDefaultsCapAlignmentOnForNonV41Models(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{name: "zcode", models: []core.Model{{ID: "glm-5.3"}}}
	h := New(Options{Registry: registryOf(client), ModelOverrides: store})
	rec := get(t, h, "/panel/api/models")
	var got modelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	m := got.Clients[0].Models[0]
	if !m.ElevateTop || !m.ElevateTopDefault {
		t.Fatalf("elevate = %v default = %v, want both true for a non-V4.1 row", m.ElevateTop, m.ElevateTopDefault)
	}
	if m.ElevateTopManual {
		t.Fatal("an untouched row was reported as a manual elevate choice")
	}

	if err := store.SetElevateTop("zcode", "glm-5.3", false); err != nil {
		t.Fatal(err)
	}
	rec = get(t, h, "/panel/api/models")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	m = got.Clients[0].Models[0]
	if m.ElevateTop || !m.ElevateTopDefault || !m.ElevateTopManual {
		t.Fatalf("after explicit off: elevate=%v default=%v manual=%v, want false/true/true",
			m.ElevateTop, m.ElevateTopDefault, m.ElevateTopManual)
	}
}

// TestModelsListPublishesV41CapAlignmentDefaults pins what the row checkbox
// shows before the operator touches it: the embedded V4.1 Flash default is on,
// but it is not a manual value, so the panel can still say "restore default".
func TestModelsListPublishesV41CapAlignmentDefaults(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{name: "cline", models: []core.Model{{ID: "cline-free/deepseek-v4.1-flash"}}}
	h := New(Options{Registry: registryOf(client), ModelOverrides: store})
	rec := get(t, h, "/panel/api/models")
	var got modelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	m := got.Clients[0].Models[0]
	if !m.ElevateTop || !m.ElevateTopDefault {
		t.Fatalf("elevate = %v default = %v, want both true", m.ElevateTop, m.ElevateTopDefault)
	}
	if m.ElevateTopManual {
		t.Fatal("an untouched row was reported as a manual elevate choice")
	}

	// Unchecking the box is an explicit off, and the row must say so while the
	// embedded default stays visible for "restore default".
	if err := store.SetElevateTop("cline", "cline-free/deepseek-v4.1-flash", false); err != nil {
		t.Fatal(err)
	}
	rec = get(t, h, "/panel/api/models")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	m = got.Clients[0].Models[0]
	if m.ElevateTop || !m.ElevateTopDefault || !m.ElevateTopManual {
		t.Fatalf("after explicit off: elevate=%v default=%v manual=%v, want false/true/true",
			m.ElevateTop, m.ElevateTopDefault, m.ElevateTopManual)
	}
}

func TestModelsListPublishesMultipleEffortMappings(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEffortMappings("workbuddy", "cn:glm-5.3", []modelmeta.EffortMapping{
		{From: "high", To: "max"},
		{From: "xhigh", To: "max"},
	}); err != nil {
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
	if len(m.EffortMappings) != 2 ||
		m.EffortMappings[0] != (modelmeta.EffortMapping{From: "high", To: "max"}) ||
		m.EffortMappings[1] != (modelmeta.EffortMapping{From: "xhigh", To: "max"}) {
		t.Fatalf("effort_mappings = %+v, want high->max and xhigh->max", m.EffortMappings)
	}
	if len(m.EffortMappingsDefault) != 1 || m.EffortMappingsDefault[0] != (modelmeta.EffortMapping{From: "xhigh", To: "top"}) {
		t.Fatalf("effort_mappings_default = %+v, want xhigh->top", m.EffortMappingsDefault)
	}
	if !m.EffortMappingsManual || !m.EffortMappingsEditable {
		t.Fatalf("mapping flags = manual:%v editable:%v, want true/true", m.EffortMappingsManual, m.EffortMappingsEditable)
	}
}
