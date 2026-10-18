package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
	"client2api/internal/modelmeta"
)

func TestModelsListIncludesEditableVirtualModels(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	alpha := &fakeClient{name: "alpha", models: []core.Model{{
		ID: "DeepSeek-V4.1-Flash",
		Extra: map[string]any{
			"context_length":    int64(100000),
			"max_output_tokens": int64(12000),
			"supported_efforts": []string{"low", "high"},
			"default_effort":    "high",
		},
	}}}
	beta := &fakeClient{name: "beta", models: []core.Model{{
		ID: "deepseek-v4.1-flash",
		Extra: map[string]any{
			"context_length":    int64(1000000),
			"max_output_tokens": int64(32000),
			"supported_efforts": []string{"high", "max"},
			"default_effort":    "max",
		},
	}}}
	reg := registryOf(alpha, beta)
	reg.SetModelGroups(map[string]core.ModelGroup{
		"flash-group": {Members: []core.ModelGroupMember{
			{Client: "alpha", Model: "DeepSeek-V4.1-Flash"},
			{Client: "beta", Model: "deepseek-v4.1-flash"},
		}},
	})
	if err := store.Set(core.VirtualModelClient, "Auto/DeepSeek-V4.1-Flash", 272000, 0); err != nil {
		t.Fatal(err)
	}

	h := New(Options{Registry: reg, ModelOverrides: store})
	rec := get(t, h, "/panel/api/models")
	var got modelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	virtual := clientModels{}
	for _, c := range got.Clients {
		if c.Name == core.VirtualModelClient {
			virtual = c
			break
		}
	}
	if virtual.Name == "" {
		t.Fatalf("virtual client missing from %#v", got.Clients)
	}
	byID := map[string]modelInfo{}
	for _, m := range virtual.Models {
		byID[m.ID] = m
	}
	auto, ok := byID["Auto/DeepSeek-V4.1-Flash"]
	if !ok {
		t.Fatalf("Auto row missing from virtual models: %#v", virtual.Models)
	}
	if auto.ContextLength != 272000 || auto.ContextSource != modelmeta.SourceManual {
		t.Fatalf("Auto context = %d source=%q, want manual 272000", auto.ContextLength, auto.ContextSource)
	}
	if auto.ContextDefault != 1000000 || auto.MaxOutputDefault != 32000 {
		t.Fatalf("Auto defaults = context %d output %d, want widest member 1000000/32000", auto.ContextDefault, auto.MaxOutputDefault)
	}
	if !auto.ContextEditable || !auto.MaxOutputEditable {
		t.Fatal("virtual Auto row must be editable")
	}
	if !auto.EffortsEditable {
		t.Fatal("virtual Auto row must expose editable thinking efforts")
	}
	if len(auto.SupportedEfforts) == 0 || auto.EffortsSource != modelmeta.SourceVendor {
		t.Fatalf("Auto efforts = %v source=%q, want the union of member efforts", auto.SupportedEfforts, auto.EffortsSource)
	}
	if auto.DefaultEffort != "high" {
		t.Fatalf("Auto default effort = %q, want the first member's high", auto.DefaultEffort)
	}
	if len(auto.SupportedEffortsDefault) == 0 || auto.DefaultEffortDefault != "high" {
		t.Fatalf("Auto effort defaults = %v/%q, want restorable high", auto.SupportedEffortsDefault, auto.DefaultEffortDefault)
	}
	if err := store.SetEfforts(core.VirtualModelClient, "Auto/DeepSeek-V4.1-Flash", []string{"low", "max"}, "max"); err != nil {
		t.Fatal(err)
	}
	rec = get(t, h, "/panel/api/models")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, c := range got.Clients {
		if c.Name != core.VirtualModelClient {
			continue
		}
		for _, m := range c.Models {
			if m.ID == "Auto/DeepSeek-V4.1-Flash" {
				auto = m
			}
		}
	}
	if len(auto.SupportedEfforts) != 2 || auto.DefaultEffort != "max" || auto.EffortsSource != modelmeta.SourceManual {
		t.Fatalf("manual Auto efforts = %v default=%q source=%q", auto.SupportedEfforts, auto.DefaultEffort, auto.EffortsSource)
	}
	group, ok := byID["flash-group"]
	if !ok {
		t.Fatalf("group row missing from virtual models: %#v", virtual.Models)
	}
	if group.ContextLength != 1000000 || group.MaxOutputTokens != 32000 {
		t.Fatalf("group default = %d/%d, want widest member 1000000/32000", group.ContextLength, group.MaxOutputTokens)
	}
}

func TestModelsListIncludesEditableVirtualCapAlignment(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	alpha := &fakeClient{name: "alpha", models: []core.Model{{ID: "m1"}}}
	reg := registryOf(alpha)
	reg.SetModelGroups(map[string]core.ModelGroup{
		"shared": {Members: []core.ModelGroupMember{{Client: "alpha", Model: "m1"}}},
	})
	h := New(Options{Registry: reg, ModelOverrides: store})

	load := func() map[string]modelInfo {
		rec := get(t, h, "/panel/api/models")
		var got modelsReport
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		for _, c := range got.Clients {
			if c.Name != core.VirtualModelClient {
				continue
			}
			byID := map[string]modelInfo{}
			for _, m := range c.Models {
				byID[m.ID] = m
			}
			return byID
		}
		t.Fatalf("virtual client missing from %#v", got.Clients)
		return nil
	}

	byID := load()
	for _, id := range []string{"Auto/m1", "shared"} {
		m, ok := byID[id]
		if !ok {
			t.Fatalf("virtual model %q missing: %#v", id, byID)
		}
		if !m.ElevateTop || !m.ElevateTopDefault || m.ElevateTopManual || !m.ElevateTopEditable {
			t.Fatalf("%s cap alignment = on:%v default:%v manual:%v editable:%v, want on/default and editable",
				id, m.ElevateTop, m.ElevateTopDefault, m.ElevateTopManual, m.ElevateTopEditable)
		}
	}

	if err := store.SetElevateTop(core.VirtualModelClient, "Auto/m1", false); err != nil {
		t.Fatal(err)
	}
	auto := load()["Auto/m1"]
	if auto.ElevateTop || !auto.ElevateTopDefault || !auto.ElevateTopManual || !auto.ElevateTopEditable {
		t.Fatalf("manual Auto cap alignment = on:%v default:%v manual:%v editable:%v, want explicit off",
			auto.ElevateTop, auto.ElevateTopDefault, auto.ElevateTopManual, auto.ElevateTopEditable)
	}
}

func TestModelContextAcceptsVirtualClient(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{ModelOverrides: store})
	body := `{"clients":{"` + core.VirtualModelClient + `":{"Auto/DeepSeek-V4.1-Flash":{"context_length":1048576,"max_output_tokens":65536,"supported_efforts":["low","max"],"default_effort":"max"}}}}`
	req := httptest.NewRequest(http.MethodPost, "/panel/api/model_context", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d body=%s", rec.Code, rec.Body.String())
	}
	got, ok := store.Get(core.VirtualModelClient, "Auto/DeepSeek-V4.1-Flash")
	if !ok || got.ContextLength != 1048576 || got.MaxOutputTokens != 65536 ||
		len(got.SupportedEfforts) != 2 || got.DefaultEffort != "max" {
		t.Fatalf("stored virtual override = %+v ok=%v", got, ok)
	}
}

func TestModelsListPublishesVirtualEffortMappings(t *testing.T) {
	store, err := modelmeta.OpenOverrideStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetEffortMappings(core.VirtualModelClient, "shared", []modelmeta.EffortMapping{{From: "high", To: "max"}}); err != nil {
		t.Fatal(err)
	}
	alpha := &fakeClient{name: "alpha", models: []core.Model{{ID: "m1"}}}
	reg := registryOf(alpha)
	reg.SetModelGroups(map[string]core.ModelGroup{
		"shared": {Members: []core.ModelGroupMember{{Client: "alpha", Model: "m1"}}},
	})
	h := New(Options{Registry: reg, ModelOverrides: store})

	rec := get(t, h, "/panel/api/models")
	var got modelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, c := range got.Clients {
		if c.Name != core.VirtualModelClient {
			continue
		}
		for _, m := range c.Models {
			if m.ID != "shared" {
				continue
			}
			if len(m.EffortMappings) != 1 || m.EffortMappings[0] != (modelmeta.EffortMapping{From: "high", To: "max"}) {
				t.Fatalf("shared effort_mappings = %+v", m.EffortMappings)
			}
			if !m.EffortMappingsManual || !m.EffortMappingsEditable {
				t.Fatalf("shared mapping flags = manual:%v editable:%v", m.EffortMappingsManual, m.EffortMappingsEditable)
			}
			return
		}
	}
	t.Fatal("shared virtual model not found")
}

// The platform card shows the per-model box's placeholder from the value the
// registry resolves, not from a JS re-derivation.  This pins that contract:
// the number published on the model row is the number the route chain reports
// for the same (platform, model) pair.
func TestModelsListPublishesTheEffectivePriority(t *testing.T) {
	reg := registryOf(
		&fakeClient{name: "autoclaw", models: []core.Model{{ID: "zai_glm-5.3-flash"}}},
		&fakeClient{name: "workbuddy", models: []core.Model{{ID: "cn:glm-5.3-flash"}}},
	)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"autoclaw":  {Priority: -3},
		"workbuddy": {Priority: 1},
	})
	reg.SetModelGroups(map[string]core.ModelGroup{
		"glm-5.3-flash": {
			Members:            []core.ModelGroupMember{{Client: "autoclaw", Model: "zai_glm-5.3-flash"}},
			PlatformPriorities: map[string]int{"autoclaw": 0},
		},
	})

	h := New(Options{Registry: reg})
	rec := get(t, h, "/panel/api/models")
	var got modelsReport
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	var row modelInfo
	for _, c := range got.Clients {
		if c.Name != "autoclaw" {
			continue
		}
		for _, m := range c.Models {
			if m.ID == "zai_glm-5.3-flash" {
				row = m
			}
		}
	}
	if row.ID == "" {
		t.Fatal("the autoclaw model row was not published")
	}
	// The group's 0 shadows the base -3, and the row has to say so.
	if row.EffectivePriority != 0 || row.PrioritySource != "group" {
		t.Fatalf("row = (%d, %q), want the inherited group 0", row.EffectivePriority, row.PrioritySource)
	}
	if row.ModelPriority != nil {
		t.Fatalf("model_priority = %v, want nil while the model inherits", *row.ModelPriority)
	}

	// Once the operator writes a per-model value, the row reports it as both
	// the effective value and the explicit override.
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"autoclaw":  {Priority: -3, ModelPriorities: map[string]int{"zai_glm-5.3-flash": -1}},
		"workbuddy": {Priority: 1},
	})
	rec = get(t, h, "/panel/api/models")
	got = modelsReport{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	row = modelInfo{}
	for _, c := range got.Clients {
		if c.Name != "autoclaw" {
			continue
		}
		for _, m := range c.Models {
			if m.ID == "zai_glm-5.3-flash" {
				row = m
			}
		}
	}
	if row.EffectivePriority != -1 || row.PrioritySource != "model" {
		t.Fatalf("row = (%d, %q), want the written model -1", row.EffectivePriority, row.PrioritySource)
	}
	if row.ModelPriority == nil || *row.ModelPriority != -1 {
		t.Fatalf("model_priority = %v, want the explicit -1", row.ModelPriority)
	}
}
