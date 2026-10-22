package panel

import (
	"strings"
	"testing"
)

func TestUsageOverviewShowsCacheHitRateAndTotalCost(t *testing.T) {
	src := poolStatsUISource(t)
	tiles := poolStatsFuncBody(t, src, "renderUsageTiles")
	for _, want := range []string{
		`k: "缓存命中率"`,
		`k: "总花费"`,
		"t.cache_hit_tokens",
		"t.cache_prompt_tokens",
		"usageCacheHitRate(cacheHit, cachePrompt, t.has_cached_tokens)",
		"t.has_cost",
		"t.unpriced_requests",
		"未定价",
		"—",
	} {
		if !strings.Contains(tiles, want) {
			t.Errorf("用量总览没有显示 %s", want)
		}
	}
}

func TestRecentCallsShowCacheAndCost(t *testing.T) {
	src := poolStatsUISource(t)
	if cost := poolStatsFuncBody(t, src, "fmtUsageCost"); !strings.Contains(cost, "¥") {
		t.Error("花费格式器没有使用人民币符号")
	}
	for _, want := range []string{"缓存 tokens", "缓存命中率", "花费", "¥/百万 tokens"} {
		if !strings.Contains(src, want) {
			t.Errorf("最近调用缺少 %q", want)
		}
	}
	body := poolStatsFuncBody(t, src, "renderRecentRows")
	for _, want := range []string{
		"x.cached_tokens",
		"x.has_cached_tokens",
		"x.prompt_tokens",
		"x.cost",
		"x.has_cost",
		"—",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("renderRecentRows 没有读取 %s", want)
		}
	}
	if h, r := recentHeaderCells(t, src), recentRowCells(t, src); h != r {
		t.Errorf("最近调用表头 %d 列、行模板 %d 格：新增列后整行会错位", h, r)
	}
	if !strings.Contains(body, `colspan="17"`) {
		t.Error("最近调用空表的 colspan 没有跟着新增列更新到 17")
	}
}

func TestUsageCacheHitRateAlwaysKeepsOneDecimal(t *testing.T) {
	src := poolStatsUISource(t)
	format := poolStatsFuncBody(t, src, "fmtUsageCacheHitRate")
	if !strings.Contains(format, ".toFixed(1)") {
		t.Error("缓存命中率没有固定保留一位小数：99.6% 会被四舍五入成 100%")
	}

	tiles := poolStatsFuncBody(t, src, "renderUsageTiles")
	if !strings.Contains(tiles, "fmtUsageCacheHitRate(hit)") {
		t.Error("总览磁贴没有走统一的一位小数格式化")
	}
	recent := poolStatsFuncBody(t, src, "renderRecentRows")
	if !strings.Contains(recent, "fmtUsageCacheHitRate(cacheHit)") {
		t.Error("最近调用没有走统一的一位小数格式化")
	}
}

// 倍率格也需要一个 xN 格式化器，但它绝不能叫 fmtRate：JS 的函数声明会提升，
// 同名的后一个定义会覆盖前一个，最近调用里的 tokens/s 就会被渲染成 xN。
func TestTokenRateFormatterIsNotShadowedByMultiplierFormatter(t *testing.T) {
	src := poolStatsUISource(t)
	if got := strings.Count(src, "function fmtRate("); got != 1 {
		t.Fatalf("fmtRate 定义了 %d 次；后一个会覆盖前一个，最近调用会把 tok/s 显示成倍率", got)
	}

	rate := poolStatsFuncBody(t, src, "fmtRate")
	if !strings.Contains(rate, ".toFixed(1)") || !strings.Contains(rate, `" tok/s"`) {
		t.Fatalf("fmtRate 不是 tokens/s 格式化器：%s", rate)
	}

	multiplier := poolStatsFuncBody(t, src, "fmtMultiplier")
	if !strings.Contains(multiplier, `return "x"`) {
		t.Fatalf("fmtMultiplier 不是倍率格式化器：%s", multiplier)
	}
	for _, name := range []string{"rateListPrice", "rateEffectivePrice"} {
		body := poolStatsFuncBody(t, src, name)
		if !strings.Contains(body, "fmtMultiplier(") {
			t.Errorf("%s 没有使用 fmtMultiplier：倍率格会失去 xN 显示", name)
		}
	}
}

// 大数磁贴必须用紧凑单位显示：23px 等宽字体放不下十亿级 tokens，
// .ustile 的 overflow:hidden 会把真实值截成看似少了一截的数字。
func TestUsageTilesUseCompactValuesForLargeTokenTotals(t *testing.T) {
	tiles := poolStatsFuncBody(t, poolStatsUISource(t), "renderUsageTiles")
	for _, want := range []string{
		"v: fmtTok(pt)",
		"v: fmtTok(ct)",
		"v: fmtTok(total)",
		"title: numf(pt)",
		"title: numf(ct)",
		"title: numf(total)",
	} {
		if !strings.Contains(tiles, want) {
			t.Errorf("用量磁贴没有用紧凑单位并保留完整值提示：缺少 %q", want)
		}
	}
}

// 堆叠柱的总高度必须严格等于 total_tokens 的比例。输出段小于 1px 时
// 不能用 Math.max(..., 1) 抬高总高度，否则相邻数据点会出现“token 更多
// 反而柱子更矮”的假象；可见性用单独的 1px 顶帽解决。
func TestUsageChartKeepsStackedHeightProportional(t *testing.T) {
	body := poolStatsFuncBody(t, poolStatsUISource(t), "usageChartSVG")
	if strings.Contains(body, "Math.max(p.ct ? 1 : 0") {
		t.Error("输出段仍用 1px 最小值抬高总柱高：Token 与柱高可能倒挂")
	}
	for _, want := range []string{
		"const hC = Math.max(0, hTot - hP)",
		"usbar-cap",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("图表没有把输出段与 1px 顶帽分开：缺少 %q", want)
		}
	}
}

// 跨天窗口的 x 轴不能只写 13:00 / 01:00 / 17:00：日期一省略，
// 标签看起来就像时间倒序。超过 24 小时时每个小时刻度都要带月日。
func TestUsageChartLabelsMultiDayTicksWithDate(t *testing.T) {
	body := poolStatsFuncBody(t, poolStatsUISource(t), "usageChartSVG")
	for _, want := range []string{
		"multiDay",
		"span > 24 * 60 * 60 * 1000",
		"String(dt.getMonth() + 1)",
		"String(dt.getDate()).padStart(2, \"0\")",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("跨天 x 轴没有带日期：缺少 %q", want)
		}
	}
}

// 柱高按 total_tokens 计算，悬浮读数也必须给出合计，否则用户拿输入
// token 去和总柱高比较，会把正常的堆叠比例误判成坐标轴错误。
func TestUsageChartHoverShowsTotalTokens(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		`id="usReadA"`,
		`data-us-a="`,
		`$("#usReadA").textContent`,
		`"合计 "`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("用量图悬浮读数没有显示合计 tokens：缺少 %q", want)
		}
	}
}

// 总览不能继续把七项指标画成同一重量。合计 tokens 是窗口的主指标，
// 必须有独立的主卡结构和类名，辅卡才有层级可依附。
func TestUsageOverviewHasPrimaryAndSecondaryTiles(t *testing.T) {
	src := poolStatsUISource(t)
	tiles := poolStatsFuncBody(t, src, "renderUsageTiles")
	for _, want := range []string{
		`x.role === "primary" ? "ustat-primary" : "ustat-mini"`,
		`role: "primary"`,
		`host.classList.add("ustat-grid")`,
	} {
		if !strings.Contains(tiles, want) {
			t.Errorf("用量总览没有主辅卡层级：缺少 %q", want)
		}
	}
	for _, cls := range []string{"ustat-primary", "ustat-mini", "ustat-spark"} {
		if !strings.Contains(src, "."+cls) {
			t.Errorf("样式表没有定义 .%s", cls)
		}
	}
}

// 切换窗口重绘时数字不能瞬间跳变。每个带数值的磁贴都要有统一的过渡标记，
// 并用 requestAnimationFrame 挂上最终可见态；不真实改 DOM 的动画不算。
func TestUsageOverviewAnimatesValueChanges(t *testing.T) {
	src := poolStatsUISource(t)
	tiles := poolStatsFuncBody(t, src, "renderUsageTiles")
	for _, want := range []string{
		"us-num-change",
		"requestAnimationFrame",
		"classList.add",
		"prefers-reduced-motion",
	} {
		if !strings.Contains(tiles, want) {
			t.Errorf("磁贴数字没有窗口切换过渡：缺少 %q", want)
		}
	}
	if !strings.Contains(src, ".ustile .v.us-num-change") {
		t.Error("样式表没有定义数字变化的初始态")
	}
}

// 走势图除了柱，还要有一条弱化的合计趋势线；它让尖峰和低谷一眼可扫，
// 同时不能抢掉输入/输出堆叠柱。
func TestUsageChartHasAReadableTrendLine(t *testing.T) {
	chart := poolStatsFuncBody(t, poolStatsUISource(t), "usageChartSVG")
	for _, want := range []string{
		"ustrend",
		"polyline",
		"points=",
	} {
		if !strings.Contains(chart, want) {
			t.Errorf("用量图缺少合计趋势线：%q", want)
		}
	}
}

// hover 提示条不能瞬间出现；进入用短过渡，离开立即收起。样式必须
// 只动 opacity/transform，不动布局，避免 5 秒轮询时抖动。
func TestUsageReadoutUsesNonLayoutMotion(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		".usread.is-in",
		"opacity",
		"transform",
		"transition:",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("读数条没有非布局动画：缺少 %q", want)
		}
	}
	hover := poolStatsFuncBody(t, src, "usageChartHover")
	if !strings.Contains(hover, `classList.add("is-in")`) {
		t.Error("usageChartHover 没有给读数条挂显示态")
	}
}

// 迷你趋势线和合计趋势线不能把缺失的小时用直连“补”出来：
// 断档处必须断开，否则视觉上会凭空出现一个不存在的尖峰/深谷。
func TestUsageTrendLinesBreakAcrossMissingBuckets(t *testing.T) {
	body := poolStatsFuncBody(t, poolStatsUISource(t), "usageChartSVG")
	for _, want := range []string{
		"maxTrendGap",
		"trendRuns",
		"run.push",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("趋势线没有按缺桶断开：缺少 %q", want)
		}
	}
}

// 迷你趋势线的面积必须用主题色，不能因为选择器少了一层而退化成黑色。
func TestUsageSparkAreaUsesThemeFill(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		".ustile .ustat-spark path.usspark-area",
		"fill: color-mix(in srgb, var(--accent) 30%, transparent)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("迷你趋势线面积没有使用主题色：缺少 %q", want)
		}
	}
}

// 总览网格必须在窄屏降级：12 列在 390px 会把主卡压成不到 100px，
// 卡片文字全部竖排。小屏必须回到单列/双列。
func TestUsageOverviewGridDegradesOnNarrowScreens(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		"@media (max-width: 980px)",
		"grid-template-columns: repeat(6, minmax(0, 1fr))",
		"grid-column: span 3",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("总览网格没有窄屏降级：缺少 %q", want)
		}
	}
	for _, want := range []string{
		"@media (max-width: 560px)",
		"grid-template-columns: repeat(2, minmax(0, 1fr))",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("手机宽度没有降成两列：缺少 %q", want)
		}
	}
}

// SVG 面积色必须是显式的，且特异性要盖过全局 path { fill: currentColor }：
// 没有这一条时，迷你趋势线的面积会渲染成纯黑。
func TestUsageSparkAreaBeatsGlobalPathFill(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, ".ustile .ustat-spark path.usspark-area { fill: color-mix(in srgb, var(--accent) 30%, transparent); stroke: none; }") {
		t.Error("迷你趋势线面积没有用足够特异的选择器指定主题色：会被全局 path 规则染成黑色")
	}
}

// 迷你趋势线的 SVG 只有 ustat-spark 类，是 .ustile 的后代，不是同一个元素。
// 写成 .ustile.ustat-spark（复合选择器）永远匹配不到，polyline 的 fill:none
// 不生效，而 SVG polyline 默认 fill 是黑色 —— 于是主卡上出现一块黑色面积。
func TestUsageSparkSelectorsMatchTheRenderedMarkup(t *testing.T) {
	src := poolStatsUISource(t)
	if strings.Contains(src, ".ustile.ustat-spark") {
		t.Error("迷你趋势线用了复合选择器 .ustile.ustat-spark：SVG 上没有 ustile 类，规则匹配不到，polyline 会以黑色填充渲染")
	}
	for _, want := range []string{
		".ustile .ustat-spark polyline",
		"fill: none",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("迷你趋势线没有正确断开填充：缺少 %q", want)
		}
	}
}

// 主卡的迷你趋势线应该是一条横跨卡底的完整小图，而不是缩在右下角、
// 让主卡中部留一大块空白。宽度铺满、底部对齐、按非等比拉伸才有仪表盘观感。
func TestUsagePrimarySparkSpansTheCardFooter(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		".ustile .ustat-spark {",
		"left: 0; right: 0; bottom: 0;",
		`preserveAspectRatio="none"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("主卡迷你趋势线没有铺满卡底：缺少 %q", want)
		}
	}
}
