package panel

import (
	"strings"
	"testing"
)

// 面板「名字/占位承诺了行为、实际零动作或对不上」一类（issue #107 + #110）。
//
// 两单同型：界面上写着的那句话与代码真正干的事分叉了，而且两边都不报错——
//   - #107：按钮叫「重新探测」，实现接的是 renderClients，而后者在 OVER_FULL
//     下第一步就短路（#93），整颗按钮只剩「把内存里的表重排一遍」；
//   - #110：按钮首屏叫「预演顺序」，空态文案却说「点『测试链路』」，而那个名字
//     只在第一次预演跑完后的 finally 复位里出现过。
//
// 判据不是「这段代码长什么样」，而是「页面上那个名字指向的东西真的存在」：
// 文案里的按钮名必须等于静态 HTML 的名字，静态名字必须等于运行时复位值。

// TestProbeButtonActuallyProbes 钉住 #107：那颗按钮必须有真动作与在途反馈。
func TestProbeButtonActuallyProbes(t *testing.T) {
	src := poolStatsUISource(t)

	// 接的必须是 probeClients，不能退回 renderClients（后者在 OVER_FULL 下零动作）。
	if !strings.Contains(src, `$("#btnPk").addEventListener("click", probeClients);`) {
		t.Fatal("#btnPk 没有接 probeClients：OVER_FULL 时点下去零请求零反馈（issue #107）")
	}
	if strings.Contains(src, `$("#btnPk").addEventListener("click", renderClients);`) {
		t.Error("#btnPk 退回了 renderClients：那是纯 DOM 重排，不是探测（issue #107 回归）")
	}

	body := poolStatsFuncBody(t, src, "probeClients")
	// 能力块只来自 /overview 的一次重读，所以必须重读它。
	if !strings.Contains(body, "await loadStatus()") {
		t.Error("probeClients 没有重读 /overview：能力清单读不到，等于只重排了一遍（issue #107）")
	}
	// 号池也要一并重读（模块可能中途换了上游/加了号）。
	if !strings.Contains(body, "await loadAllAccounts(true)") {
		t.Error("probeClients 没有重读各模块账号：号池变化看不到（issue #107）")
	}
	if !strings.Contains(body, "renderClients()") {
		t.Error("probeClients 没有重画本页：探测回来的能力不会显示（issue #107）")
	}
	// 在途反馈：置灰 + 文案，且要能恢复（与账号页「刷新凭据」同一套口径）。
	for _, want := range []string{`btn.disabled = true`, `btn.textContent = "探测中…"`, "btn.disabled = false", "probeClientsBusy"} {
		if !strings.Contains(body, want) {
			t.Errorf("probeClients 缺少在途/收尾语句 %q：按钮点了看不出在跑（issue #107）", want)
		}
	}
	// 失败不许静默：网关不可达时要说出来。
	if !strings.Contains(body, "toast(") {
		t.Error("probeClients 没有任何 toast：成功与失败都不可见（issue #107）")
	}
}

// TestRenderClientsKeepsItsShortcut 反向钉住 #107 的第 3 条边界：修复不该把
// #93 的短路一起拿掉——「进入页面」和「用户点按钮」是两条路径。
func TestRenderClientsKeepsItsShortcut(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderClients")
	if !strings.Contains(body, "if (!OVER_FULL) await loadAllAccounts();") {
		t.Fatal("renderClients 的 OVER_FULL 短路被拿掉了：进页面又把 overview 已带的账号下载一遍（#93 回归）")
	}
	if strings.Contains(body, "探测中…") {
		t.Error("在途文案跑进了 renderClients：进入页面的那次渲染不是一次探测（issue #107）")
	}
}

// TestRoutePlanButtonsHaveOneName 钉住 #110：按钮的静态名字、两个动作函数的
// 复位值、空态文案指的按钮名，必须全一致。「测试链路」这个只在 finally 里出现过
// 的名字不许留在任何文案里——首屏页面上没有它。
func TestRoutePlanButtonsHaveOneName(t *testing.T) {
	src := poolStatsUISource(t)

	// 静态 HTML 的两个按钮。
	for _, want := range []string{
		`>预演顺序</button>`,
		`>实测速率</button>`,
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("静态 HTML 里找不到 %q（issue #110）", want)
		}
	}
	// 空态文案必须点的是「预演顺序」，不是页面上不存在的名字。
	if !strings.Contains(src, "点「预演顺序」") {
		t.Error("链路测试的空态文案没有指向「预演顺序」：首屏找不到它说的那颗按钮（issue #110）")
	}
	// 旧名字不许留在任何会显示出来的文本里（注释里提到它是允许的——说明历史
	// 正是这类回归最需要的上下文）。剥掉注释再查。
	if strings.Contains(stripPanelComments(src), "测试链路") {
		t.Error("界面文本里还有「测试链路」：首屏没有叫这个名字的按钮（issue #110 回归）")
	}
	// 运行时复位值也必须等于各自按钮的静态名字。
	plan := poolStatsFuncBody(t, src, "runRoutePlan")
	if !strings.Contains(plan, `btn.textContent = "预演顺序"`) {
		t.Error("runRoutePlan 的复位值不是「预演顺序」：跑完一次后按钮改名（issue #110）")
	}
	if strings.Contains(plan, `btn.textContent = "测试链路"`) {
		t.Error("runRoutePlan 仍把按钮复位成「测试链路」：那就是与静态名字分叉的那个来源（issue #110）")
	}
	speed := poolStatsFuncBody(t, src, "runRouteSpeed")
	if !strings.Contains(speed, `btn.textContent = "实测速率"`) {
		t.Error("runRouteSpeed 的复位值不是「实测速率」：测速按钮跑完改名（issue #110）")
	}
	if strings.Contains(speed, `btn.textContent = "测速"`) {
		t.Error("runRouteSpeed 仍把按钮复位成「测速」：静态 HTML 叫「实测速率」（issue #110）")
	}
}

// stripPanelComments 去掉 /* ... */ 与 <!-- ... --> 里的内容，保留代码与可见文本。
// 断言「某个名字不许再出现」时，注释里解释历史不算违规。
func stripPanelComments(src string) string {
	for {
		i := strings.Index(src, "/*")
		if i < 0 {
			break
		}
		j := strings.Index(src[i:], "*/")
		if j < 0 {
			src = src[:i]
			break
		}
		src = src[:i] + src[i+j+2:]
	}
	for {
		i := strings.Index(src, "<!--")
		if i < 0 {
			break
		}
		j := strings.Index(src[i:], "-->")
		if j < 0 {
			src = src[:i]
			break
		}
		src = src[:i] + src[i+j+3:]
	}
	return src
}
