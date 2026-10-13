package panel

import (
	"strings"
	"testing"
)

func TestModelsViewEditsEffortPolicy(t *testing.T) {
	src := poolStatsUISource(t)
	render := poolStatsFuncBody(t, src, "renderModels")
	for _, want := range []string{
		`mdDefaultEffortCell(m)`,
		`mdEffortsCell(m)`,
		`mdElevateTopCell(m)`,
		"m.supported_efforts",
		"m.default_effort",
		"m.efforts_source",
		"m.default_effort_source",
		"m.elevate_top",
	} {
		if !strings.Contains(render, want) {
			t.Errorf("renderModels missing %q", want)
		}
	}
	if !strings.Contains(src, `>封顶对齐</th>`) {
		t.Error("model table header is missing the cap-alignment column")
	}
	for _, want := range []string{`data-field="efforts"`, `data-field="default_effort"`, `data-field="elevate_top"`, "默认开启", "手动开启"} {
		if !strings.Contains(src, want) {
			t.Errorf("model table is missing %q", want)
		}
	}
}

func TestModelsViewSavesEffortPolicy(t *testing.T) {
	src := poolStatsUISource(t)
	save := poolStatsFuncBody(t, src, "mdSave")
	for _, want := range []string{
		"supported_efforts",
		"default_effort",
		"elevate_top",
		"parseEffortList",
	} {
		if !strings.Contains(save, want) {
			t.Errorf("mdSave missing %q", want)
		}
	}
	inputs := poolStatsFuncBody(t, src, "mdInputs")
	for _, want := range []string{`q("efforts")`, `q("default_effort")`, `q("elevate_top")`} {
		if !strings.Contains(inputs, want) {
			t.Errorf("mdInputs missing %q", want)
		}
	}
}

// mdInputs reads the live values that mdRecord later stores in MD_EDITS. The
// editable td and its input/checkbox both carry data-field, so a generic
// attribute selector returns the td first: .value and .checked are then
// undefined, no edit is recorded, and Save stays disabled.
func TestModelsViewInputsSelectControlsNotTheirCells(t *testing.T) {
	src := poolStatsUISource(t)
	inputs := poolStatsFuncBody(t, src, "mdInputs")
	if !strings.Contains(inputs, `tr.querySelector('input[data-field="' + f + '"]')`) {
		t.Fatal("mdInputs must select input[data-field=...], not the outer td[data-field=...]")
	}
}
