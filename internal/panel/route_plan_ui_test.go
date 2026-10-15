package panel

import (
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
	for _, id := range []string{`id="rpModel"`, `id="btnRpTest"`, `id="rpResult"`} {
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
