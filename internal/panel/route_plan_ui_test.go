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

func TestRoutePlanCatalogsCarryTheResolvedModelContext(t *testing.T) {
	src := poolStatsUISource(t)
	render := poolStatsFuncBody(t, src, "renderPlatforms")
	for _, want := range []string{
		`context_length: Number(m.context_length || 0)`,
		`context_source: String(m.context_source || "")`,
	} {
		if !strings.Contains(render, want) {
			t.Errorf("renderPlatforms 没有保留模型上下文 %q", want)
		}
	}
}

func TestRoutePlanUIWiresTheSpeedButton(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "runRouteSpeed")
	for _, want := range []string{
		`fetch("/panel/api/route_speed"`,
		`method: "POST"`,
		`text/event-stream`,
		`getReader()`,
		`ev.result`,
		`PF.models`,
		`catalogs`,
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
		`context_length`,
		`context_source`,
		`上下文 `,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("routeStepHTML 缺少 %q", want)
		}
	}

	edits := poolStatsFuncBody(t, src, "routeOrderEdits")
	for _, want := range []string{
		`platform`,
		`model_id`,
		`priority`,
	} {
		if !strings.Contains(edits, want) {
			t.Errorf("routeOrderEdits 缺少 %q", want)
		}
	}

	// The drag writes the per-model priority on the platform card, which is
	// the one value the chain displays; it must not fall back to the group,
	// schedule or base boxes.
	apply := poolStatsFuncBody(t, src, "applyRouteOrder")
	for _, want := range []string{
		`.pfmPrio`,
		`savePlatforms`,
	} {
		if !strings.Contains(apply, want) {
			t.Errorf("applyRouteOrder 缺少 %q", want)
		}
	}
	for _, unwanted := range []string{`.pfGPrio`, `.pfSchedPrio`} {
		if strings.Contains(apply, unwanted) {
			t.Errorf("applyRouteOrder 仍会写 %s，拖动应当只改模型优先级", unwanted)
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
  {platform:"beta", model_id:"gpt-x", priority:0},
  {platform:"alpha", model_id:"gpt-x", priority:3},
  {platform:"gamma", model_id:"gpt-x", priority:7}
]);
assert.deepStrictEqual(got, [
  {platform:"beta", model_id:"gpt-x", priority:2},
  {platform:"alpha", model_id:"gpt-x", priority:6},
  {platform:"gamma", model_id:"gpt-x", priority:7}
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
    {id:"gpt-x", free:true, context_length:200000, context_source:"manual"},
    {id:"paid", free:false}
  ]}
});
assert.deepStrictEqual(routeCatalogs({alpha:["gpt-x", "paid"]}), {
  alpha:[
    {id:"gpt-x", free:true, context_length:200000, context_source:"manual"},
    {id:"paid"}
  ]
});
`
	if out, err := exec.Command(node, "-e", program).CombinedOutput(); err != nil {
		t.Fatalf("route catalogue behavior: %v\n%s", err, out)
	}
}
