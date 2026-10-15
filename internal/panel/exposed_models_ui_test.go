package panel

import (
	"strings"
	"testing"
)

func TestModelsViewHasExposedModelsSubtab(t *testing.T) {
	src := poolStatsUISource(t)
	nav := subtabNavHTML(t, src, "models")
	for _, want := range []string{
		`data-subtab="capability"`, ">模型能力<",
		`data-subtab="exposed"`, ">对外模型<",
		`role="tab"`, `aria-selected="true"`, `class="chip subtab on"`,
	} {
		if !strings.Contains(nav, want) {
			t.Errorf("模型与档位二级 Tab 缺少 %q", want)
		}
	}
	for _, tc := range []struct {
		panel string
		ids   []string
	}{
		{"models-capability", []string{`id="mdBody"`, `id="btnMdSave"`}},
		{"models-exposed", []string{`id="exBody"`, `id="exSearch"`, `id="exModeAll"`, `id="exModeSelected"`, `id="btnExSave"`}},
	} {
		panel := subtabPanelHTML(t, src, tc.panel)
		for _, id := range tc.ids {
			if !strings.Contains(panel, id) {
				t.Errorf("模型面板 %s 不包含 %s", tc.panel, id)
			}
		}
	}
	if strings.Contains(subtabOpenTag(t, src, "models-capability"), " hidden") {
		t.Error("模型能力应该是默认可见面板")
	}
	if !strings.Contains(subtabOpenTag(t, src, "models-exposed"), " hidden") {
		t.Error("对外模型面板默认应隐藏")
	}
}

func TestExposedModelsUIWiresFetchAndHotSave(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		`setupSubtabs("models"`,
		`api("/panel/api/exposed_models")`,
		`exposed_models: EX_MODE === "all" ? null : selected`,
		`api("/panel/api/config", { method: "PATCH", body: body })`,
		`api("/panel/api/reload", { method: "POST" })`,
		`function exToggleVisible`,
		`function exSave`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("对外模型 UI 接线缺少 %q", want)
		}
	}
}
