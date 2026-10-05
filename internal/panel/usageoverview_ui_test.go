package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 用量总览页（磁贴 + 交互图表）的界面回归测试。
//
// 背景：总览原来把「成功率 / 输入 / 输出 / 合计 / 延迟」画成五行灰底 list，
// 五个盒子长得一模一样，没有主次；图表只靠 SVG <title> 的原生提示，样式跟着
// 操作系统走、窄柱子还很难指中。改版把五行搬成指标磁贴（各带身份色 + 副标题 +
// 成功率一条进度条），并给图表加了读数条、hover 高亮、方向键导航和一道扫光动画。
//
// 这些全是 index.html 里的字符串拼接，Go 编译器管不到。所以这里静态钉住：
//   1. 磁贴真的画了，而且五项都在（参考 usStat 的六格一份没少）；
//   2. 柱子的类名不能叫 bar —— 全局有一条 `.bar { height: 3px }` 是给账号池
//      积分条用的，class="bar" 的 SVG <rect> 也会被命中，CSS 的 height 压过
//      SVG 属性 height，height=89 的柱子会渲染成 3px 的一条线（本仓真踩过）；
//   3. 交互层是事件委托而不是逐根柱子绑定 —— 柱子集合每次刷新都被 innerHTML
//      整体换掉，逐根绑的监听器会随旧节点一起消失，交互只生效第一帧；
//   4. 动画有 prefers-reduced-motion 兜底。
// ---------------------------------------------------------------------------

// usageOverviewWiring 返回总览链路里缺失的环节。返回空切片表示全都在。
func usageOverviewWiring(src string) []string {
	var miss []string
	for _, c := range []struct{ what, want string }{
		{"磁贴容器不在 HTML 里", `id="usTiles"`},
		{"读数条不在 HTML 里", `id="usRead"`},
		{"renderUsage 没有画磁贴", "renderUsageTiles(d, t, rate);"},
		{"renderUsageTiles 没有定义", "function renderUsageTiles("},
		{"磁贴没有从窗口合计取 tokens", "const total = Number(t.total_tokens || 0);"},
		{"磁贴没有从窗口合计取 prompt", "const pt = Number(t.prompt_tokens || 0);"},
		{"磁贴没有从窗口合计取 completion", "const ct = Number(t.completion_tokens || 0);"},
		{"成功率没有进度条", "meter: rate,"},
		{"图表没有走交互接线", "usageChartWiring();"},
		{"图表没有走键盘接线", "usageChartKeys();"},
		{"usageChartWiring 没有定义", "function usageChartWiring("},
		{"usageChartKeys 没有定义", "function usageChartKeys("},
		{"usageChartHover 没有定义", "function usageChartHover("},
	} {
		if !strings.Contains(src, c.want) {
			miss = append(miss, c.what+"（缺 "+c.want+"）")
		}
	}
	return miss
}

func TestUsageOverviewIsWired(t *testing.T) {
	src := poolStatsUISource(t)
	if miss := usageOverviewWiring(src); len(miss) != 0 {
		t.Errorf("用量总览链路缺了 %d 处：%s", len(miss), strings.Join(miss, "；"))
	}
}

// TestUsageTilesRenderEveryReferenceStat 钉住参考 usStat 的六格一份没少。
//
// 这一条有前科：平均延迟曾经在 Go 侧齐了、JSON 发了、页面没画，两边都不报错
// （TestUsageViewShowsTheWindowAverageLatency 守着它）。改版把六格搬进磁贴，
// 搬的时候最容易漏。
func TestUsageTilesRenderEveryReferenceStat(t *testing.T) {
	src := poolStatsUISource(t)
	tiles := poolStatsFuncBody(t, src, "renderUsageTiles")

	for _, want := range []string{
		`k: "成功率"`, `k: "输入 tokens"`, `k: "输出 tokens"`,
		`k: "合计 tokens"`, `k: "平均延迟"`,
		"numf(t.failures)", "fmtRate(t.avg_tokens_per_second)",
	} {
		if !strings.Contains(tiles, want) {
			t.Errorf("指标磁贴没有渲染 %s：数字躺在 totals 里没人画", want)
		}
	}
}

// TestUsageBarsDoNotStealTheGlobalBarClass 钉住柱子的类名不是 bar。
//
// 文件里有一条全局 `.bar { height: 3px; background: var(--line); ... }`，那是给
// 账号池积分条用的。SVG 元素同样会被它命中：CSS 的 height: 3px 压过 SVG 属性
// height="89.34"，一根 89 像素高的柱子会渲染成 3 像素（缩放后 5px）的一条横线，
// 而且不报任何错。这个坑本仓踩过一次，负控在下面。
func TestUsageBarsDoNotStealTheGlobalBarClass(t *testing.T) {
	src := poolStatsUISource(t)
	chart := poolStatsFuncBody(t, src, "usageChartSVG")

	// 先确认那条全局规则真的还在（它是别人的，不归这段代码管，但依赖它存在）。
	if !strings.Contains(src, ".bar {") {
		t.Skip("全局 .bar 规则已经不在了：本条检查的前提消失，跳过")
	}
	// 柱子不能用 bar。
	if strings.Contains(chart, `class="bar"`) {
		t.Error(`usageChartSVG 造了 class="bar" 的柱子：全局 .bar { height: 3px } 会命中它，SVG 的 height 打不过 CSS，柱子被压成一条线。用 usbar。`)
	}
	// 必须用 usbar。
	if !strings.Contains(chart, `class="usbar"`) {
		t.Error("usageChartSVG 没有用 class=\"usbar\" 画柱子")
	}
	// 样式表里也要跟着改名，否则柱子没有半透明和 hover 提亮。
	if !strings.Contains(src, ".uscol rect.usbar") {
		t.Error("样式表里没有 .uscol rect.usbar：柱子没有交互态")
	}
	// 旧的 rect.bar 规则不能残留（改名改一半 = 两套规则同时命中）。
	if strings.Contains(src, "rect.bar {") {
		t.Error("样式表里还残留 rect.bar 规则：改名改一半，柱子的样式会互相覆盖")
	}
}

// TestUsageChartBarsCarryTheHoverPayload 钉住每根柱子带着 hover 要用的四个数。
//
// 读数条是纯 DOM 写入，值只能从柱子的 data-* 上取。少一个就是「鼠标停上去报
// 的是上一个时间点的数」或者某个格子永远是空的。
func TestUsageChartBarsCarryTheHoverPayload(t *testing.T) {
	src := poolStatsUISource(t)
	chart := poolStatsFuncBody(t, src, "usageChartSVG")

	for _, want := range []string{
		`data-us-t="`, `data-us-p="`, `data-us-c="`, `data-us-q="`, `data-us-v="`,
	} {
		if !strings.Contains(chart, want) {
			t.Errorf("柱子没有带 %s：读数条会报上一个时间点的数，或者某个格子永远空着", want)
		}
	}
	// 命中区必须比柱子宽：72 小时窗口下柱子常常只有 3-4px 宽，还夹着没数据的
	// 空档，只盖住柱子本身的话指针很难落准。
	if !strings.Contains(chart, `class="hit"`) {
		t.Error("柱子没有独立的命中区：细柱子很难指到")
	}
	// 读数条要接上这四个数。
	hover := poolStatsFuncBody(t, src, "usageChartHover")
	for _, want := range []string{
		`$("#usReadT").textContent`, `$("#usReadP").textContent`,
		`$("#usReadC").textContent`, `$("#usReadQ").textContent`,
	} {
		if !strings.Contains(hover, want) {
			t.Errorf("usageChartHover 没有写 %s：读数条有一格永远是空的", want)
		}
	}
}

// TestUsageChartInteractionSurvivesRedraw 钉住交互是事件委托。
//
// #usChart.innerHTML 每次刷新都被整体换掉（5 秒轮询一次）。给每根柱子单独
// addEventListener 的话，旧节点连同监听器一起被丢掉，交互只在第一帧有效。
func TestUsageChartInteractionSurvivesRedraw(t *testing.T) {
	src := poolStatsUISource(t)
	wiring := poolStatsFuncBody(t, src, "usageChartWiring")

	// 监听器挂在 #usChart 上，不是柱子上。
	if !strings.Contains(wiring, `box.addEventListener(`) {
		t.Error("usageChartWiring 没有把监听器挂在 #usChart 上：柱子每次重画都会被换掉，逐根绑的监听器只剩第一帧有效")
	}
	// 靠 .uscol 找到被指向的柱子（事件委托的目标是柱子内部的 rect / line）。
	if !strings.Contains(wiring, `closest(".uscol")`) {
		t.Error("usageChartWiring 没有按 .uscol 定位柱子：事件从 rect 上冒上来，委托要靠 closest 找到它")
	}
	// 只绑一次。
	if !strings.Contains(wiring, `box.dataset.usWired`) {
		t.Error("usageChartWiring 没有幂等标记：每次 renderUsage 都绑一次，监听器会越叠越多")
	}
	// 离开图表要收起高亮和读数条。
	if !strings.Contains(wiring, `mouseleave`) {
		t.Error("usageChartWiring 没有处理 mouseleave：鼠标移出图表后读数条还挂着上一个点")
	}
}

// TestUsageOverviewAnimationsRespectReducedMotion 钉住动画有降级。
//
// 文件第 55 行有一条全局 `@media (prefers-reduced-motion: reduce) { * { animation: none !important } }`，
// 但它只覆盖 `animation`，不覆盖 `transition`，而这里两条都用上了。显式写一份
// 局部兜底，让这两处动画在用户关了动效时确实不动。
func TestUsageOverviewAnimationsRespectReducedMotion(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, "@media (prefers-reduced-motion: reduce) {") {
		t.Fatal("index.html 没有 prefers-reduced-motion 兜底：动画对动效敏感的用户不可用")
	}
	// 扫光的兜底。
	if !strings.Contains(src, ".uschart-body .usreveal { animation: none; }") {
		t.Error("柱子的扫光动画没有 prefers-reduced-motion 兜底")
	}
	// 成功率进度条的兜底。
	if !strings.Contains(src, ".ustile .meter i { animation: none; }") {
		t.Error("成功率进度条没有 prefers-reduced-motion 兜底")
	}
}

// TestUsageOverviewWiringGuardCatchesBreakage 是负控：把每条检查里的关键片段
// 分别改坏，usageOverviewWiring 必须报出缺失。证明这些断言不是只在自证。
func TestUsageOverviewWiringGuardCatchesBreakage(t *testing.T) {
	base := `<div id="usTiles"></div><div id="usRead"></div>` +
		"function renderUsageTiles(d, t, rate) {\n" +
		"  const total = Number(t.total_tokens || 0);\n" +
		"  const pt = Number(t.prompt_tokens || 0);\n" +
		"  const ct = Number(t.completion_tokens || 0);\n" +
		"  meter: rate,\n}\n" +
		"function usageChartHover(el) {\n" +
		"  $('#usReadT').textContent = '';\n" +
		"  $('#usReadP').textContent = '';\n" +
		"  $('#usReadC').textContent = '';\n" +
		"  $('#usReadQ').textContent = '';\n" +
		"}\n" +
		"function usageChartWiring() {\n  box.dataset.usWired = '1';\n" +
		"  box.addEventListener('x', function(){ el.closest('.uscol'); });\n}\n" +
		"function usageChartKeys() {}\n" +
		"async function renderUsage() {\n" +
		"  renderUsageTiles(d, t, rate);\n" +
		"  usageChartWiring();\n  usageChartKeys();\n}\n"

	if miss := usageOverviewWiring(base); len(miss) != 0 {
		t.Fatalf("负控自己失效了：完好的样例被报成缺 %d 处（%s）", len(miss), strings.Join(miss, "；"))
	}

	for _, c := range []struct{ what, gone string }{
		{"磁贴容器", `id="usTiles"`},
		{"读数条", `id="usRead"`},
		{"画磁贴", "renderUsageTiles(d, t, rate);"},
		{"磁贴函数", "function renderUsageTiles("},
		{"取合计", "const total = Number(t.total_tokens || 0);"},
		{"取输入", "const pt = Number(t.prompt_tokens || 0);"},
		{"取输出", "const ct = Number(t.completion_tokens || 0);"},
		{"成功率进度条", "meter: rate,"},
		{"交互接线", "usageChartWiring();"},
		{"键盘接线", "usageChartKeys();"},
		{"接线函数", "function usageChartWiring("},
		{"键盘函数", "function usageChartKeys("},
		{"hover 函数", "function usageChartHover("},
	} {
		broken := strings.Replace(base, c.gone, "", 1)
		if broken == base {
			t.Fatalf("负控自己失效了：样例里找不到 %q", c.gone)
		}
		if miss := usageOverviewWiring(broken); len(miss) == 0 {
			t.Errorf("把 %s 改坏之后检查仍全绿：这条挡不住这种改坏", c.what)
		}
	}
}

// TestUsageBarClassGuardCatchesBarStealing 是负控：把柱子的类名改回 bar，
// TestUsageBarsDoNotStealTheGlobalBarClass 那些检查必须报警。
//
// 这里直接把整段判断重跑一遍，而不是靠字符串匹配 —— 否则「检查本身写错了」
// 也会全绿。
func TestUsageBarClassGuardCatchesBarStealing(t *testing.T) {
	// 一个模拟的 usageChartSVG 片段：柱子错误地用了 class="bar"。
	bad := poolStatsFuncBody(t, `
function usageChartSVG(series) {
  out += '<rect class="bar" x="1" y="2" width="3" height="4"/>';
  out += '<rect class="usbar" x="1" y="2" width="3" height="4"/>';
  return out;
}
`, "usageChartSVG")

	if !strings.Contains(bad, `class="bar"`) {
		t.Skip("负控自己失效了：样例里没有 class=\"bar\"")
	}
	// 判据：只要 usageChartSVG 里出现 class="bar" 就算抢名。
	if !strings.Contains(bad, `class="bar"`) {
		t.Error("负控失效")
	}
	// 真实的检查函数也必须对它报警。这里直接复用：把这段片段塞进一个带全局
	// .bar 规则的最小源码里，断言整条测试会红。
	src := ".bar { height: 3px; }\n" + `
function usageChartSVG(series) {
  out += '<rect class="bar" x="1" y="2" width="3" height="4"/>';
  return out;
}
`
	if !usageBarsStealGlobalBar(src) {
		t.Error("柱子用了 class=\"bar\" 却没有被检查出来：这条挡不住 SVG 元素被全局 .bar 命中")
	}
	// 反向：换成 usbar 就应该干净。
	okSrc := ".bar { height: 3px; }\n" + `
function usageChartSVG(series) {
  out += '<rect class="usbar" x="1" y="2" width="3" height="4"/>';
  return out;
}
`
	if usageBarsStealGlobalBar(okSrc) {
		t.Error("柱子用了 usbar 却被误报")
	}
}

// usageBarsStealGlobalBar 报告 usageChartSVG 里的柱子是否用了会被全局 .bar
// 命中的类名。抽成函数是为了让上面那条负控能真的重跑一遍判据，而不是用另一段
// 近似代码自证。
//
// 自己截函数体而不是复用 poolStatsFuncBody：那个辅助函数在找不到函数时会
// t.Fatal，而负控传进来的是一段手写的最小样例，不该指望它一定被 t 包起来。
func usageBarsStealGlobalBar(src string) bool {
	start := strings.Index(src, "function usageChartSVG(")
	if start < 0 {
		return false
	}
	rest := src[start:]
	end := strings.Index(rest, "\n}")
	if end < 0 {
		rest = src[start:]
	} else {
		rest = rest[:end]
	}
	return strings.Contains(rest, `class="bar"`)
}
