package panel

import (
	"strings"
	"testing"
)

// 钉住总览页在闸开时不再整页冻结（issue #207）：
//
//	#98 的两条 guard pills 曾插在 `const bits = []` 声明之前——闸一开
//	renderOverview 每拍抛 ReferenceError，#ovAttn/#ovKpis/#ovRingPct 全部
//	停在打开闸之前那一帧，红条每 5 秒重弹。守护测试钉「声明先于使用」
//	（TDZ 语义在文本层可机械判定）与「refreshAll 的渲染各自兜底」。
func TestOverviewGuardPillsComeAfterTheDeclaration(t *testing.T) {
	src := poolStatsUISource(t)
	overview := poolStatsFuncBody(t, src, "renderOverview")

	decl := strings.Index(overview, "const bits = [];")
	firstUse := strings.Index(overview, "bits.push(")
	if decl < 0 || firstUse < 0 {
		t.Fatal("renderOverview 里找不到 bits 声明或首次使用")
	}
	if firstUse < decl {
		t.Fatalf("bits.push 首次出现在声明之前（offset %d < %d）：闸开启时每拍抛 ReferenceError，整页冻结（issue #207）", firstUse, decl)
	}
	// guard pills（#98 想画的横幅）本身要在声明之后、#ovAttn 写入之前可达。
	if !strings.Contains(overview, `ov-pill err">WAF 拦截`) {
		t.Error("WAF 拦截横幅丢了：#98 的解释文案必须画得出来")
	}
}

// refreshAll 的渲染调用逐个兜底：一格算错只少一格，不把同拍后面的渲染拖垮。
func TestRefreshAllRendersAreIndividuallyGuarded(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "refreshAll")
	if !strings.Contains(body, "safeRender(") {
		t.Fatal("refreshAll 的渲染调用没有逐个兜底：一格算错整页冻结且红条重弹（issue #207）")
	}
	for _, name := range []string{"renderOverview", "renderAlerts", "renderConfig"} {
		call := name + "();"
		if strings.Contains(body, call) {
			t.Errorf("%s 仍在裸调用（应经 safeRender）：一次异常会吞掉同拍后续渲染（issue #207）", name)
		}
	}
}
