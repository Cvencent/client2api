package panel

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestPlatformsViewHasRoutePlanSubtab(t *testing.T) {
	src := poolStatsUISource(t)
	nav := subtabNavHTML(t, src, "platforms")
	for _, want := range []string{
		`data-subtab="groups"`, ">模型路由组<",
		`data-subtab="rules"`, ">平台与模型<",
		`data-subtab="plan"`, ">链路测试<",
		`role="tab"`, `aria-selected="true"`, `class="chip subtab on"`,
	} {
		if !strings.Contains(nav, want) {
			t.Errorf("平台配置二级 Tab 缺少 %q", want)
		}
	}
	if got := strings.Count(nav, `class="chip subtab on"`); got != 1 {
		t.Errorf("平台配置二级 Tab 默认选中项 = %d, want exactly 1", got)
	}

	panel := subtabPanelHTML(t, src, "platforms-plan")
	for _, id := range []string{`id="rpModel"`, `id="btnRpTest"`, `id="btnRpSpeed"`, `id="rpResult"`} {
		if !strings.Contains(panel, id) {
			t.Errorf("链路测试面板缺少 %s", id)
		}
	}
}

func TestRoutePlanUIWiresTheApiCall(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "runRoutePlan")
	for _, want := range []string{
		`api("/panel/api/route_plan"`,
		`method: "POST"`,
		`PF.models`,
		`catalogs`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("runRoutePlan 缺少 %q", want)
		}
	}
	if !strings.Contains(src, `$("#btnRpTest").addEventListener("click", runRoutePlan)`) {
		t.Fatal("runRoutePlan 没有绑定到测试按钮")
	}
}

func TestRoutePlanUIWiresTheSpeedButton(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "runRouteSpeed")
	for _, want := range []string{
		`api("/panel/api/route_speed"`,
		`method: "POST"`,
		`PF.models`,
		`catalogs`,
		`results`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("runRouteSpeed 缺少 %q", want)
		}
	}
	if !strings.Contains(src, `$("#btnRpSpeed").addEventListener("click", runRouteSpeed)`) {
		t.Fatal("runRouteSpeed 没有绑定到测速按钮")
	}

	cell := poolStatsFuncBody(t, src, "routeSpeedCellHTML")
	for _, want := range []string{`completion_tokens`, `tokens_per_second`, `fmtMs`} {
		if !strings.Contains(cell, want) {
			t.Errorf("routeSpeedCellHTML 缺少 %q", want)
		}
	}
}

func TestRoutePlanUIDragOrderPersistsPriority(t *testing.T) {
	src := poolStatsUISource(t)
	row := poolStatsFuncBody(t, src, "routeStepHTML")
	for _, want := range []string{
		`draggable="true"`,
		`data-rp-platform`,
		`data-rp-position`,
		`data-rp-move`,
		`title="拖动调整优先级"`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("routeStepHTML 缺少 %q", want)
		}
	}

	edits := poolStatsFuncBody(t, src, "routeOrderEdits")
	for _, want := range []string{
		`priority_source`,
		`group`,
		`schedule_hit`,
		`priority`,
	} {
		if !strings.Contains(edits, want) {
			t.Errorf("routeOrderEdits 缺少 %q", want)
		}
	}

	apply := poolStatsFuncBody(t, src, "applyRouteOrder")
	for _, want := range []string{
		`.pfPrio`,
		`.pfGPrio`,
		`.pfSchedPrio`,
		`savePlatforms`,
	} {
		if !strings.Contains(apply, want) {
			t.Errorf("applyRouteOrder 缺少 %q", want)
		}
	}

	for _, want := range []string{
		`$("#rpResult").addEventListener("dragstart"`,
		`$("#rpResult").addEventListener("dragover"`,
		`$("#rpResult").addEventListener("drop"`,
		`data-rp-move`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("链路测试拖拽接线缺少 %q", want)
		}
	}
}

func TestRouteOrderEditsUsesTheDisplayedOrder(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the route-order behavior needs it to run the inline function")
	}
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "routeOrderEdits") + "\n}"
	code, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	program := `
const assert = require("assert");
const load = new Function("return " + ` + string(code) + ` + ";");
const routeOrderEdits = load();
const got = routeOrderEdits([
  {platform:"beta", priority_source:"group", group:"shared", model_id:"gpt-x"},
  {platform:"alpha", priority_source:"base", model_id:"gpt-x"},
  {platform:"gamma", priority_source:"schedule", schedule_hit:"窗口2", model_id:"gpt-x"}
]);
assert.deepStrictEqual(got, [
  {platform:"beta", source:"group", group:"shared", schedule_hit:"", priority:0},
  {platform:"alpha", source:"base", group:"", schedule_hit:"", priority:1},
  {platform:"gamma", source:"schedule", group:"", schedule_hit:"窗口2", priority:2}
]);
`
	if out, err := exec.Command(node, "-e", program).CombinedOutput(); err != nil {
		t.Fatalf("route-order behavior: %v\n%s", err, out)
	}
}

func TestRouteCatalogsPreservesFreeModelMetadata(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not on PATH; the catalogue behavior needs it to run the inline function")
	}
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "routeCatalogs") + "\n}"
	code, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	program := `
const assert = require("assert");
const routeCatalogs = new Function("PF", "return " + ` + string(code) + ` + ";")({
  models: {alpha:["gpt-x", "paid"]},
  modelInfos: {alpha:[
    {id:"gpt-x", free:true},
    {id:"paid", free:false}
  ]}
});
assert.deepStrictEqual(routeCatalogs({alpha:["gpt-x", "paid"]}), {
  alpha:[
    {id:"gpt-x", free:true},
    {id:"paid"}
  ]
});
`
	if out, err := exec.Command(node, "-e", program).CombinedOutput(); err != nil {
		t.Fatalf("route catalogue behavior: %v\n%s", err, out)
	}
}
