package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 轮询路径不得无谓重建 DOM（issue #144；同型先例是 #90）。
//
// 缺陷形状固定得可怕，本仓已经出现三次：
//
//   - 一个函数挂在轮询上（5 秒或 4 秒）；
//   - 它无条件 `innerHTML = ...` 重建一块 DOM；
//   - 被重建出来的新元素带着 from 值的入场动画（`animation: ... both`），
//     于是「读数没变也把动画放一遍」，而且首采样 opacity 就是动画的 from 值。
//
// 三次实例：
//
//	#90  mountMasthead 每 5 秒重拆页头标题的 span（ttlChar 逐个淡入）
//	#144 rlRenderKpis 每 5 秒重建四格读数（numIn 整排淡入上浮 .35）
//	#144 scRenderRuns 每 4 秒（有活跃批次时）重建运行记录（.runitem 裸的 rowIn）
//
// 所以这一组断言的不是某一个函数，而是**这条规则本身**：任何写进轮询路径的
// 重建，都必须先过 `onlyChanged`（内容签名）。真正的几何/身份判据在
// cmd/panelsmoke 的真浏览器阶段（assertNoPolledReplay），那里用节点身份 + 首采样
// opacity 量的就是报告方用的那套证据——实测回退守卫会红。
// ---------------------------------------------------------------------------

// polledRebuilders 是挂在轮询路径上的重建函数：key 是 onlyChanged 的签名键。
var polledRebuilders = []struct {
	fn    string
	key   string
	why   string
	block string
}{
	{"rlRenderKpis", "rlKpis", "路由实况四格读数（5 秒轮询，renderRouteLive 每拍都调）", ""},
	{"rlRenderEvents", "rlEvents", "路由实况请求流水（同一个 5 秒轮询）", ""},
	{"scRenderRuns", "scRuns", "任务中心运行记录（有活跃批次时 4 秒重拉）", ""},
}

// TestPolledRebuildsAreSignatureGuarded 钉住每一个轮询重建都过了签名守卫。
func TestPolledRebuildsAreSignatureGuarded(t *testing.T) {
	src := poolStatsUISource(t)

	// 守卫本身必须在，而且是「签名没变就整块不动」的语义。
	if !strings.Contains(src, "function onlyChanged(key, sig)") {
		t.Fatal("index.html 没有 onlyChanged 判据：轮询重建只能退回无条件重画（issue #144）")
	}
	guard := poolStatsFuncBody(t, src, "onlyChanged")
	if !strings.Contains(guard, "POLL_SIG[key] === sig") || !strings.Contains(guard, "return false") {
		t.Fatalf("onlyChanged 不再是「签名相同就不重画」：\n%s", guard)
	}

	for _, rb := range polledRebuilders {
		body := poolStatsFuncBody(t, src, rb.fn)
		if !strings.Contains(body, "onlyChanged(\""+rb.key+"\"") {
			t.Errorf("%s 没有过签名守卫（key=%q）：%s 每次都会整段重建并重播入场动画（issue #144）",
				rb.fn, rb.key, rb.why)
			continue
		}
		// 守卫必须排在**要保护的那次**重建之前，否则拦不住——「写了但排错
		// 位置」与本仓 #121/#122 的「读失败被当成成功」是同一类错误。
		// 取最后一次 innerHTML：空态/错误态那些分支是早退的，前期就画的不是
		// 被守卫的那一块（rlRenderEvents 与 scRenderRuns 都有这样的前置分支）。
		g := strings.Index(body, "onlyChanged(\""+rb.key+"\"")
		assign := strings.LastIndex(body, ".innerHTML = ")
		if assign < 0 {
			continue
		}
		if g > assign {
			t.Errorf("%s 的守卫排在它要保护的那次 innerHTML 重建之后：这一拍已经重画完了（issue #144）", rb.fn)
		}
		// 签名必须真的从内容算出来，不能是常量（那等于永远不重画，比缺陷更坏）。
		if !strings.Contains(body, "const sig = ") {
			t.Errorf("%s 的签名不是从这一拍的数据算出来的（issue #144）", rb.fn)
		}
	}
}

// TestTheAnimationLivesOnTheClassNotTheRow 钉住 #144 的第三处修法。
//
// 4 节的注释承诺「列表整体重画时不加类，只有真正新出现的一条才带上 .rowin」，
// 可 .rowin 从没被任何代码加上；而运行记录时间线的 .runitem 是**裸的**动画，
// 于是那条规则在最需要它的那块上失效。修法是把动画只留在 .rowin 上，行要动
// 就自己带类——同一个属性只能有一个说话的人（#86/#109/#111/#123 的教训）。
func TestTheAnimationLivesOnTheClassNotTheRow(t *testing.T) {
	src := poolStatsUISource(t)

	if !cssHasClass(src, "rowin") {
		t.Fatal(".rowin 没了：新行失去唯一的入场动画入口")
	}
	if !strings.Contains(src, ".rowin { animation: rowIn") {
		t.Error(".rowin 不再是那个带 rowIn 动画的类（issue #144）")
	}

	// .runitem 自己的声明块里不能再有 animation——否则整段重建就是每拍重播。
	blocks := cssRulesFor(src, ".runitem")
	if len(blocks) == 0 {
		t.Fatal("找不到 .runitem 的规则")
	}
	for _, b := range blocks {
		if strings.Contains(b, "animation:") {
			t.Errorf(".runitem 又自己挂了动画：\n%s\n"+
				"它会被整段 innerHTML 重建，动画在这里就是「每拍重播」（issue #144）。"+
				"要动的新行带 .rowin。", strings.TrimSpace(b))
		}
	}

	// 反方向：必须真的有人加上 .rowin，否则新行永不入场（另一半的失效）。
	body := poolStatsFuncBody(t, src, "scRenderRuns")
	if !strings.Contains(body, `" rowin"`) && !strings.Contains(body, `' rowin'`) {
		t.Error("没有任何代码给新行加 .rowin：入场动画这一侧又空了（issue #144 的注释兑现了一半）")
	}
	if !strings.Contains(body, "RUN_SEEN") {
		t.Error("新行的判据不是「上一拍没见过它」：整段重建时老行也会跟着重播（issue #144）")
	}
}

// TestRouteLivePollRespectsVisibility 钉住 #144 顺带的那条：这张页自己的
// 5 秒轮询也要过 Page Visibility 门（#95 那次两个全局轮询都换了门控，这是漏项）。
func TestRouteLivePollRespectsVisibility(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "rlStartPolling")

	if !strings.Contains(body, "document.hidden") {
		t.Fatal("rlStartPolling 不过 document.hidden：标签页在后台也每 5 秒发一次 " +
			"/route_live 并重建四格与流水（issue #144 / #95 的漏项）")
	}
	// 补拍：回到前台要能立刻补一拍，否则操作员看到的是切走前的旧读数。
	if !strings.Contains(body, "RL_RESUME") {
		t.Error("rlStartPolling 没有注册回到前台的补拍钩子：切回来仍是旧读数")
	}
	if !strings.Contains(src, `if (typeof RL_RESUME === "function") RL_RESUME();`) {
		t.Error("全局 visibilitychange 没有调用 RL_RESUME：钩子注册了也没人触发")
	}
}
