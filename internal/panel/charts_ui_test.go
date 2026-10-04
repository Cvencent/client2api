package panel

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 用量时间序列图（renderUsageChart 族）与积分到期分布图（pk 族）的回归测试。
//
// 这两块界面此前是「死 CSS」：样式表里画好了 .uschart-body .ax/.gl/.tk 与
// .pk-expiry-* 一整族，JS 里却从来没有元素带上这些类，于是图表位置只有一排
// 纯色柱条、积分页只有批次表。移植完成后，本文件钉住四件事：
//   1. 界面读的字段名 == gateway.UsagePoint 的 json tag（错一个字母 = 图的每个
//      坐标都是 NaN，而 Go 侧与浏览器都不报错）；
//   2. JS 造出来的类名 == 样式表真的定义了的类名（拼错 = 图还在，但没颜色、
//      没网格线、柱子挤在一起）；
//   3. 账号字段名是 a.id / a.label，不是参考的 a.uid / a.nickname（参考是
//      workbuddy-only 项目，字段名照抄会让颜色映射与图例整片 undefined）；
//   4. 旧实现（.bars 柱条带）已经彻底退场，不再是「两套画法并存」。
// ---------------------------------------------------------------------------

// cssHasClass 判断样式表里有没有定义这个类（要求后面紧跟分隔符，避免
// .pk-expiry 命中 .pk-expiry-row 这种子串自证）。
func cssHasClass(src, name string) bool {
	needle := "." + name
	for i := 0; ; {
		j := strings.Index(src[i:], needle)
		if j < 0 {
			return false
		}
		j += i
		after := j + len(needle)
		if after >= len(src) {
			return false
		}
		switch src[after] {
		case ' ', '\t', '\r', '\n', '{', ',', ':', '>', '.', '+', '~':
			return true
		}
		i = after
	}
}

// usagePointJSONKeys 从 gateway.UsagePoint 的字段上抽出 json tag。
func usagePointJSONKeys(t *testing.T) map[string]string {
	t.Helper()
	b, err := os.ReadFile("../gateway/usage.go")
	if err != nil {
		t.Skipf("cannot read ../gateway/usage.go: %v", err)
	}
	src := string(b)
	start := strings.Index(src, "type UsagePoint struct {")
	if start < 0 {
		t.Fatal("gateway.UsagePoint is gone; the shell reads its fields")
	}
	end := strings.Index(src[start:], "\n}")
	if end < 0 {
		t.Fatal("UsagePoint looks unterminated")
	}
	body := src[start : start+end]

	tagRe := regexp.MustCompile("`json:\"([a-z_]+)")
	out := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if m := tagRe.FindStringSubmatch(line); m != nil {
			out[fields[0]] = m[1]
		}
	}
	return out
}

// TestUsageChartReadsTheKeysTheGoStructSends 是这一块的跨包一致性检查：图读的
// 每个字段名必须逐字等于 gateway.UsagePoint 的 json tag，而且真的被用起来
// （不是只出现在注释里）。
func TestUsageChartReadsTheKeysTheGoStructSends(t *testing.T) {
	src := poolStatsUISource(t)
	tags := usagePointJSONKeys(t)
	if len(tags) == 0 {
		t.Fatal("UsagePoint 里一个 json tag 都没解析到")
	}

	// 字段名 -> 界面必须出现的一次读取。错一个字母，那段数据永远是 undefined。
	for field, want := range map[string]string{
		"T":                `p.t`,
		"Scope":            `p.scope`,
		"PromptTokens":     `p.prompt_tokens`,
		"CompletionTokens": `p.completion_tokens`,
		"TotalTokens":      `p.total_tokens`,
		"Requests":         `p.requests`,
	} {
		tag, ok := tags[field]
		if !ok {
			t.Errorf("gateway.UsagePoint 没有字段 %s（json tag 应为 %q）", field, want)
			continue
		}
		// 界面用 p.<tag> 读它。参考把 json tag 写成 total_tokens 这类蛇形，
		// 界面若照着 Go 字段名写 p.TotalTokens 会静默拿到 undefined。
		if !strings.Contains(src, "p."+tag) {
			t.Errorf("界面里找不到 p.%s（UsagePoint.%s 的 json tag %q）：这一项永远是空的", tag, field, tag)
		}
	}
}

// TestUsageChartReplacesTheBarBandInsteadOfAddingToIt 钉住旧画法已经退场：
// .bars 的 CSS、JS 与类名都不该再存在，否则就是两套图表并存、改一个漏一个。
func TestUsageChartReplacesTheBarBandInsteadOfAddingToIt(t *testing.T) {
	src := poolStatsUISource(t)

	for _, gone := range []string{`class="bars"`, ".bars {"} {
		if strings.Contains(src, gone) {
			t.Errorf("旧的柱条带还在：%q（.bars 已被 usageChartSVG 取代）", gone)
		}
	}
	if body := poolStatsFuncBody(t, src, "renderUsage"); !strings.Contains(body, "usageChartSVG(d.series)") {
		t.Error("renderUsage 没有把 series 交给 usageChartSVG")
	}
	// 图是「追加」，因为 #usChart 上面还有成功率与三行合计：整体赋值会把它们冲掉。
	if body := poolStatsFuncBody(t, src, "renderUsage"); !strings.Contains(body, `$("#usChart").innerHTML +=`) {
		t.Error("renderUsage 覆盖了 #usChart：成功率与三行合计会被图冲掉")
	}
}

// TestUsageChartClassesMatchTheStylesheet 钉住 JS 造出的类名与样式表对得上。
// 类名拼错不会报错，只会让图变成一堆没有颜色、没有网格线的方块。
func TestUsageChartClassesMatchTheStylesheet(t *testing.T) {
	src := poolStatsUISource(t)
	chart := poolStatsFuncBody(t, src, "usageChartSVG")

	// JS 里出现的类名 -> 样式表里必须有定义。
	for _, cls := range []string{"gl", "ax", "tk", "us-empty"} {
		if !strings.Contains(chart, `class="`+cls+`"`) {
			t.Errorf("usageChartSVG 不再输出 class=%q", cls)
		}
		if !cssHasClass(src, cls) {
			t.Errorf("样式表里没有定义 .%s：图会画出来但没颜色 / 没网格线", cls)
		}
	}
	// 堆叠的两段颜色必须分开（输入 vs 输出），否则整根柱子一个色，图例也就没意义。
	for _, want := range []string{"var(--accent)", "var(--ok)"} {
		if !strings.Contains(chart, want) {
			t.Errorf("usageChartSVG 缺少 %q：两段堆叠分不出输入与输出", want)
		}
	}
	// 图例本身：hd 里必须真的有三个元素（两个色块 + 文案），否则那三条 CSS 又变回死的。
	hd := src[strings.Index(src, `class="uschart-hd"`):]
	hd = hd[:strings.Index(hd, "\n")]
	for _, want := range []string{`class="legend"`, `class="sw sw-p"`, `class="sw sw-c"`} {
		if !strings.Contains(hd, want) {
			t.Errorf("uschart-hd 里缺少 %q：图例仍然是死的", want)
		}
	}
}

// TestUsageChartDropsUnparsablePoints 钉住两处「别让一个坏点毁掉整张图」：
// 时间解析失败的点整点丢掉；y 轴上限至少为 1（全零窗口不能除以 0）。
func TestUsageChartDropsUnparsablePoints(t *testing.T) {
	src := poolStatsUISource(t)
	parse := poolStatsFuncBody(t, src, "parsePointTime")
	if !strings.Contains(parse, "isNaN(ms) ? null : ms") {
		t.Error("parsePointTime 对解析不出的时间没有返回 null")
	}
	if !strings.Contains(parse, "s.length === 13") {
		t.Error("parsePointTime 没有按 t 的长度区分小时点与日期点")
	}
	// gateway.seriesPoints 发的是完整 RFC3339（25 字符），解析器必须先尝试整串，
	// 否则每个点都解析成 NaN、整张图静默变空。
	if !strings.Contains(parse, "Date.parse(s)") {
		t.Error("parsePointTime 没有先按完整 RFC3339 解析：gateway 发的是 25 字符时间戳")
	}
	chart := poolStatsFuncBody(t, src, "usageChartSVG")
	if !strings.Contains(chart, "if (t === null) return;") {
		t.Error("usageChartSVG 没有丢掉解析不出时间的点：NaN 会传染整张图")
	}
	if !strings.Contains(chart, "const max = Math.max(1,") {
		t.Error("usageChartSVG 的 y 轴上限没有下限 1：全零窗口会除以 0")
	}
	if !strings.Contains(chart, `p.scope === "day"`) {
		t.Error("x 轴标签没有按 scope 逐点取格式：day 与 hour 混在一张图里会标错")
	}
}

// TestExpiryDistributionIsMountedAndFed 钉住积分到期分布图真的接上了：
// markup 里有挂载点，renderCredits 真的往里写，且空态会清空它（否则切换到一个
// 没有账号的客户端时，上一个客户端的图会留在页面上）。
func TestExpiryDistributionIsMountedAndFed(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, `<div class="pad" id="pkExpiry"></div>`) {
		t.Error("markup 里没有 #pkExpiry 挂载点：那一整族 .pk-expiry-* 样式仍然是死的")
	}
	if !strings.Contains(src, `$("#pkExpiry").innerHTML`) {
		t.Error("没有任何代码往 #pkExpiry 写内容")
	}
	body := poolStatsFuncBody(t, src, "renderCredits")
	// 图和卡片共用同一个 now：各调各的 Date.now() 会让「N 天」在同一次渲染里
	// 出现两种口径。
	if !strings.Contains(body, "const now = Date.now()") {
		t.Error("renderCredits 没有取一次 now 给图和卡片共用")
	}
	if !strings.Contains(body, "renderExpiryDistribution(list, now)") {
		t.Error("renderCredits 没有把账号列表交给 renderExpiryDistribution")
	}
	// 空态必须清空：只 return 会把上一个客户端的图留在原地。
	if !strings.Contains(body, `$("#pkExpiry").innerHTML = ""`) {
		t.Error("renderCredits 的空态没有清空 #pkExpiry")
	}
	// 图必须在批次表之前：图回答「还能用多久」，表回答「有哪些批次」。
	if i, j := strings.Index(body, "renderExpiryDistribution"), strings.Index(body, "crAcctRow"); i < 0 || j < 0 || i > j {
		t.Error("到期分布图没有排在批次表之前")
	}
}

// TestExpiryDistributionClassesMatchTheStylesheet 钉住 pk 族的每个类名都在
// 样式表里有定义。这族 CSS 此前一个引用都没有，正是「拼错也看不出来」的重灾区。
func TestExpiryDistributionClassesMatchTheStylesheet(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderExpiryDistribution")

	for _, cls := range []string{
		"pk-expiry-chart", "pk-expiry-hdr", "pk-expiry-row",
		"pk-expiry-track", "pk-expiry-seg", "pk-expiry-legend",
		"pk-expiry-foot", "pk-expiry-empty",
	} {
		if !strings.Contains(body, `class="`+cls+`"`) {
			t.Errorf("renderExpiryDistribution 不再输出 class=%q", cls)
		}
		if !cssHasClass(src, cls) {
			t.Errorf("样式表里没有定义 .%s", cls)
		}
	}
	// 段的颜色 / 透明度 / 宽度都走内联自定义属性，CSS 只负责画。
	for _, want := range []string{"--seg-color:", "opacity:", "flex:"} {
		if !strings.Contains(body, want) {
			t.Errorf("renderExpiryDistribution 没有给段设置 %q", want)
		}
	}
	// 每个函数都必须在（缺一个，图的某一段就是空的，而且不会有任何报错）。
	for _, fn := range []string{
		"pkColor", "pkAccountColorMap", "pkBySource", "pkCreditOpacity",
		"pkExpiryText", "pkExpiryDateTime", "pkAccountSegments",
		"summarizeCreditDays", "renderExpiryDistribution",
	} {
		if !strings.Contains(src, "function "+fn+"(") {
			t.Errorf("缺少函数 %s", fn)
		}
	}
}

// TestExpiryDistributionUsesThisReposAccountFields 钉住参考的字段名没有被照抄。
// 参考是 workbuddy-only 项目，账号对象是 {uid, nickname}；本仓是 {id, label}。
// 照抄不会报错，只会让颜色映射与图例整片 undefined。
func TestExpiryDistributionUsesThisReposAccountFields(t *testing.T) {
	src := poolStatsUISource(t)

	for _, fn := range []string{"pkAccountColorMap", "pkAccountSegments", "summarizeCreditDays"} {
		body := poolStatsFuncBody(t, src, fn)
		for _, wrong := range []string{".uid", ".nickname"} {
			if strings.Contains(body, wrong) {
				t.Errorf("%s 里读了 %s：本仓的账号对象是 a.id / a.label", fn, wrong)
			}
		}
	}
	for _, fn := range []string{"pkAccountColorMap", "summarizeCreditDays"} {
		if body := poolStatsFuncBody(t, src, fn); !strings.Contains(body, "a.id") {
			t.Errorf("%s 没有用 a.id 做账号键", fn)
		}
	}
	// 余额必须按账号总额约束后递减：上游会把同一笔积分重复记在多个批次里，
	// 直接把各批 remain 相加会把余额算大。
	if seg := poolStatsFuncBody(t, src, "pkAccountSegments"); !strings.Contains(seg, "budget -= take") {
		t.Error("pkAccountSegments 没有从账号总余额里递减：重复记录的批次会把余额算大")
	}
	// 没有到期时间的余额不进图表，也不猜一个到期日。
	if sum := poolStatsFuncBody(t, src, "summarizeCreditDays"); !strings.Contains(sum, "if (!seg.expiresAt || seg.days === null)") {
		t.Error("summarizeCreditDays 没有把「无到期时间」的余额排除在图表之外")
	}
}

// TestExpiryChartIDGuardCatchesARename 是负控：把挂载点改名后，shell 的 id 守卫
// 必须报出来——否则上面那些 strings.Contains 可能只是在自证。
func TestExpiryChartIDGuardCatchesARename(t *testing.T) {
	src := poolStatsUISource(t)
	broken := strings.Replace(src, `$("#pkExpiry")`, `$("#pkExpiryRenamed")`, -1)
	if broken == src {
		t.Fatal(`测试自己失效了：源码里没有 $(" #pkExpiry")`)
	}
	for _, id := range shellDanglingIDs(broken) {
		if id == "pkExpiryRenamed" {
			return
		}
	}
	t.Error("shell id 守卫对改名后的 #pkExpiry 没意见：它守不住这张图")
}
