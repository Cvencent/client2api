package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 两个「参考有、本仓没有」的界面缺口，各自静态钉住。
//
// 共同点：两边的样式表其实早就搬过来了（.pk-card 一族 / input.invalid），
// 但没有任何代码去点亮它们。这种死样式是最难发现的缺口——CSS 看着齐全，
// 页面却少一块，而且两边都不报错。所以每个缺口都断言三件事：
//   1. 渲染函数存在，且被真正的视图调用（不是只定义了没人用）；
//   2. 输出的类名与样式表里的定义对得上（拼错 = 整块不显示，静默）；
//   3. 参考的字段名/选择器没有被照抄（本仓的账号是 a.id / a.label，
//      字段行是 .cfgrow .k，不是 a.uid / a.nickname 和 .fld .lb）。
// ---------------------------------------------------------------------------

// TestCreditCardsAreMountedAndFed 钉住逐账号积分卡片（参考 renderPackages 的
// #pkSummary）。本仓的积分视图只有 #crBody 一个宿主，所以卡片网格前置在明细表
// 之前，而不是另起挂载点。
func TestCreditCardsAreMountedAndFed(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, "function pkSummaryCards(") {
		t.Fatal("index.html has no pkSummaryCards：.pk-card 那一族样式仍然是死的")
	}
	body := poolStatsFuncBody(t, src, "pkSummaryCards")

	// 卡片必须真的被 renderCredits 用上，而且要排在明细表之前：卡片回答
	// 「谁还剩多少」，表回答「具体是哪些批次」。
	cr := poolStatsFuncBody(t, src, "renderCredits")
	if !strings.Contains(cr, "pkSummaryCards(list, now)") {
		t.Error("renderCredits 没有渲染逐账号积分卡片")
	}
	if i, j := strings.Index(cr, "pkSummaryCards"), strings.Index(cr, "crAcctRow"); i < 0 || j < 0 || i > j {
		t.Error("积分卡片没有排在批次明细表之前")
	}

	// 输出的每个类名都必须在样式表里有定义。.who/.nm/.realm/.big/.sub/.err
	// 的定义挂在 .pk-card 后代选择器上，cssHasClass 只看类名本身，够用。
	for _, cls := range []string{
		"pk-grid", "pk-card", "who", "nm", "realm", "big", "sub",
		"mixbar", "expirybar", "pk-legend", "err",
	} {
		if !strings.Contains(body, `class="`+cls+`"`) {
			t.Errorf("pkSummaryCards 不再输出 class=%q", cls)
		}
		if !cssHasClass(src, cls) {
			t.Errorf("样式表里没有定义 .%s", cls)
		}
	}

	// 账号键必须是 a.id：参考的 uid/nickname 照抄过来只会让颜色映射与图例
	// 整片 undefined，而 JS 不会报错。
	for _, wrong := range []string{".uid", ".nickname"} {
		if strings.Contains(body, wrong) {
			t.Errorf("pkSummaryCards 里读了 %s：本仓的账号对象是 a.id / a.label", wrong)
		}
	}
	if !strings.Contains(body, "String(a.id") {
		t.Error("pkSummaryCards 没有用 a.id 去查账号配色")
	}

	// 行级失败只让那一张卡说失败，其它账号照常显示——与后端 packages 的
	// 行级 error 语义一致（一个账号挂了不该让整页空白）。
	if !strings.Contains(body, "if (a.error)") || !strings.Contains(body, "查询失败") {
		t.Error("pkSummaryCards 没有把行级 error 渲染成单张卡的失败态")
	}

	// 空列表必须返回空串，不能吐一个空 .pk-grid（会留一块带间距的空白）。
	if !strings.Contains(body, `return ""`) {
		t.Error("pkSummaryCards 没有空列表短路")
	}
}

// TestCreditCardCompositionUsesTheSharedHelpers 钉住卡片不是自己另算一套：
// 构成条按来源分组、到期条按账号内递减后的段，两样都必须走已经验证过的助手。
func TestCreditCardCompositionUsesTheSharedHelpers(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "pkSummaryCards")

	for _, fn := range []string{"pkBySource(", "pkAccountSegments(", "pkColor(", "pkAccountColorMap(", "pkCreditOpacity("} {
		if !strings.Contains(body, fn) {
			t.Errorf("pkSummaryCards 没有用 %s：卡片会和到期分布图两套口径", fn)
		}
	}
	// 构成条的宽度必须按该来源面额 / 账号总面额，分母下限 1（全零账号会除以 0）。
	if !strings.Contains(body, "Math.max(1, Number(a.size) || 0)") {
		t.Error("构成条的分母没有下限 1：面额为 0 的账号会画出 NaN 宽度")
	}
	// 到期条每段的最小 flex 是 0.008：段太细也要看得见。
	if !strings.Contains(body, "0.008") {
		t.Error("到期条没有给极细的段留最小宽度")
	}
}

// durationFieldList 截取 const DUR_FIELDS = [...] 的字面量块。
func durationFieldList(t *testing.T, src string) string {
	t.Helper()
	start := strings.Index(src, "const DUR_FIELDS = [")
	if start < 0 {
		t.Fatal("index.html has no const DUR_FIELDS")
	}
	rest := src[start:]
	if end := strings.Index(rest, "];"); end >= 0 {
		return rest[:end]
	}
	t.Fatal("const DUR_FIELDS looks unterminated")
	return ""
}

// TestDurationFieldsAreValidatedWhileTyping 钉住 Go 时长的即时校验（参考 app.js
// 的 durationBad / markDurationFields）。本仓原本只在保存时拦一道，input.invalid
// 那两条样式因此从来没被点亮过。
func TestDurationFieldsAreValidatedWhileTyping(t *testing.T) {
	src := poolStatsUISource(t)

	// 这 9 个字段就是参考 DURATION_FIELDS 的全部内容，一个都不能漏：漏掉的
	// 那个只会等到点保存、被服务端拒了才发现。
	want := []string{
		"cfgCoolSoftRate", "cfgCoolSoftRateMax", "cfgPoolBreakerCooldown",
		"cfgPoolBreakerCooldownMax", "cfgPoolDegradeCooldown", "cfgPoolDegradeCooldownMax",
		"cfgPoolCostExplore", "cfgPoolExpiringSoon", "cfgStickyTTL",
	}
	block := durationFieldList(t, src)
	for _, id := range want {
		if !strings.Contains(block, `"`+id+`"`) {
			t.Errorf("DUR_FIELDS 里没有 %s", id)
		}
	}
	if n := strings.Count(block, `"cfg`); n != len(want) {
		t.Errorf("DUR_FIELDS 有 %d 个字段，参考是 %d 个", n, len(want))
	}
	// 反向：名单里的每个 id 都必须真的被 renderConfig 渲染出来，否则这个
	// 校验守着一个不存在的输入框，永远返回 false。
	// 号池字段在平台配置页的 WorkBuddy 卡片里，全局粘性仍在配置页。
	cfg := poolStatsFuncBody(t, src, "renderConfig") + poolStatsFuncBody(t, src, "pfWorkBuddyPoolHTML")
	for _, id := range want {
		if !strings.Contains(cfg, `"`+id+`"`) {
			t.Errorf("没有渲染 %s，但 DUR_FIELDS 在守它", id)
		}
	}

	bad := poolStatsFuncBody(t, src, "durationBad")
	if !strings.Contains(bad, `v !== ""`) {
		t.Error("durationBad 把空串当脏值了：空串 = 沿用现值，是合法的")
	}
	if !strings.Contains(bad, `v !== "0"`) {
		t.Error("durationBad 把裸 0 当脏值了：本仓的 0 表示关停该机制，是合法的")
	}
	if !strings.Contains(bad, "DUR_RE.test(v)") {
		t.Error("durationBad 没有复用共享的 DUR_RE")
	}

	mark := poolStatsFuncBody(t, src, "markDurationFields")
	if !strings.Contains(mark, `classList.toggle("invalid", bad)`) {
		t.Error("markDurationFields 没有点亮 .invalid：input.invalid 那两条样式仍然是死的")
	}
	if !strings.Contains(mark, "el.title = bad ? DUR_TIP") {
		t.Error("markDurationFields 没有挂 title 提示")
	}

	// 上面这些断言只证明「代码写着 classList.toggle」。它们曾经全绿，而整条即时
	// 校验链是死的：DUR_FIELDS 里是裸 id，$ 却是 querySelector，
	// $("cfgStickyTTL") 是个永远匹配不到的类型选择器 —— durationBad 恒返回 false、
	// markDurationFields 一进 forEach 就 return、durLabel 退化成回显 id。所以这里
	// 必须钉住「id 真的被当成选择器用」这一步：三个函数都得先过 durSel()。
	if !strings.Contains(src, `const durSel = id => (id.charAt(0) === "#" ? id : "#" + id);`) {
		t.Error("没有 durSel 归一：DUR_FIELDS 的裸 id 与 dur() 传的 \"#…\" 会分叉成两套写法")
	}
	for _, fn := range []string{"durationBad", "markDurationFields", "durLabel"} {
		body := poolStatsFuncBody(t, src, fn)
		if !strings.Contains(body, "$(durSel(id))") {
			t.Errorf("%s 把裸 id 直接交给了 $()：querySelector(\"cfgStickyTTL\") 选不中任何元素，即时标红是死的", fn)
		}
		if strings.Contains(body, "$(id)") {
			t.Errorf("%s 里还留着 $(id)：那是类型选择器写法，永远选不中", fn)
		}
	}

	// 边打边校验必须委托在 #cfgForm 上：renderConfig 每次都重建 innerHTML，
	// 挂在具体 input 上的监听会在第一次刷新配置后全部丢失。
	if !strings.Contains(src, `$("#cfgForm").addEventListener("input"`) {
		t.Error("#cfgForm 上没有 input 监听：脏值要等到点保存才被发现")
	}
	if !strings.Contains(src, "DUR_FIELDS.indexOf(ev.target.id)") {
		t.Error("input 监听没有按 DUR_FIELDS 过滤：别名与模块 JSON 也会被标红")
	}

	// 保存路径必须与即时校验同口径，且用可见中文标签点名（参考取 .fld .lb，
	// 本仓的字段行是 .cfgrow .k）。
	// 号池盒与全局粘性共用 readDurField：先钉住这段共用的读值与校验，
	// 再确认两个保存入口都真的走它。
	read := poolStatsFuncBody(t, src, "readDurField")
	if !strings.Contains(read, "DUR_RE.test(raw)") {
		t.Error("时长读值没有复用共享的 DUR_RE：输入时和保存时会分叉成两套口径")
	}
	if strings.Contains(read, "const DUR_RE") {
		t.Error("时长读值又声明了一份局部 DUR_RE：迟早与即时校验分叉")
	}
	if !strings.Contains(read, "durLabel(id)") {
		t.Error("时长报错用的是内部键名，而不是可见中文标签")
	}
	if !strings.Contains(read, "markDurationFields()") {
		t.Error("时长读值拦下脏值时没有同步标红")
	}
	for _, fn := range []string{"savePlatforms", "saveConfig"} {
		body := poolStatsFuncBody(t, src, fn)
		if !strings.Contains(body, "readDurField(") {
			t.Errorf("%s 没有走共享的时长读值 helper", fn)
		}
	}

	label := poolStatsFuncBody(t, src, "durLabel")
	if !strings.Contains(label, `closest(".cfgrow")`) || !strings.Contains(label, `querySelector(".k")`) {
		t.Error("durLabel 没有按本仓的 .cfgrow .k 取标签")
	}
	if strings.Contains(label, ".lb") {
		t.Error("durLabel 照抄了参考的 .lb：本仓的字段行是 .cfgrow .k")
	}
}

// 参考面板每 5s 静默刷新「当前可见」的视图，所以在途数/健康注记/积分自己会走。
// 本仓原本只刷日志，账号视图要手点刷新 —— 补齐这一路，并锁死三条跳过规则。
func TestVisibleViewIsPolledLikeTheReference(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, "function refreshVisible()") {
		t.Fatal("壳里没有 refreshVisible()：账号视图不会像原版那样自己刷新")
	}
	if !strings.Contains(src, "setInterval(refreshVisible, 5000)") {
		t.Error("refreshVisible 没有按原版的 5s 节奏挂上定时器")
	}
	// 日志那路的 4s 是本仓既有的更高频刷新，不能被顶掉。
	if !strings.Contains(src, "if (VIEW === \"logs\") renderLogs(); }, 4000)") {
		t.Error("日志 4s 刷新被改掉了：本仓日志比原版更勤，是有意的")
	}

	body := poolStatsFuncBody(t, src, "refreshVisible")
	if !strings.Contains(body, `refreshAll(false)`) {
		t.Error("refreshVisible 没有用静默档 refreshAll(false)：会每 5s 弹一次「已刷新」")
	}
	if !strings.Contains(body, "visBusy") {
		t.Error("refreshVisible 没有重入保护：上一轮没回来就会叠请求")
	}
	// 用户正在操作的界面不能被重建：弹层、登录轮询、对话流三条都要跳过。
	for _, guard := range []string{
		`$("#addVeil").classList.contains("on")`,
		`$("#keyVeil").classList.contains("on")`,
		"ADD.timer",
		"CHAT.busy",
	} {
		if !strings.Contains(body, guard) {
			t.Errorf("refreshVisible 缺少跳过规则 %s：会把用户正在操作的界面重建掉", guard)
		}
	}
	// 只刷账号视图：日志有自己的定时器，任务中心有自己的队列/任务板定时器。
	if !strings.Contains(body, `VIEW !== "accounts"`) {
		t.Error("refreshVisible 没有限定在账号视图：会与日志/任务中心的既有定时器打架")
	}
	if strings.Contains(body, "renderLogs()") {
		t.Error("refreshVisible 又去刷日志：日志已由 4s 定时器负责，两处节奏会互相顶")
	}
}

// 参考的 CFG_MAP 把 session_sticky_enabled / prompt_mode / prompt_file /
// sanitize_blacklist_fingerprints 列成独立一组；本仓此前只暴露了 session_sticky.ttl，
// 另外三项在文件里能写、页面却看不见，等于「只能手改 JSON 才用得上」。这个测试把
// 渲染端和回写端一起钉住：只加输入框不加回写，等于改了也存不下去。
func TestConfigPageExposesTheSessionAndPromptKnobs(t *testing.T) {
	src := poolStatsUISource(t)
	render := poolStatsFuncBody(t, src, "renderConfig")
	save := poolStatsFuncBody(t, src, "saveConfig")

	for _, id := range []string{"cfgStickyEnabled", "cfgPromptMode", "cfgPromptFile", "cfgSanitizeFingerprints"} {
		if !strings.Contains(render, `"`+id+`"`) {
			t.Errorf("renderConfig 不渲染 %s：这一项只能手改 JSON 才用得上", id)
		}
		if !strings.Contains(save, `$("#`+id+`")`) {
			t.Errorf("saveConfig 不回写 %s：输入框改了也存不下去", id)
		}
	}
	// 两个开关必须走页面的 cfgCheck，否则样式与其它开关不一致。
	for _, call := range []string{`cfgCheck("cfgStickyEnabled"`, `cfgCheck("cfgSanitizeFingerprints"`} {
		if !strings.Contains(render, call) {
			t.Errorf("renderConfig 没有用 %s 渲染开关", call)
		}
	}
	// 文件里没有这个键时页面必须显示「开」，与 Go 侧 applyDefaults 的默认一致；
	// 显示成「关」会让操作员一保存就把功能关掉。
	if !strings.Contains(render, "sticky.enabled == null ? true") {
		t.Error("会话粘性开关在键缺失时没有按「开」显示，与 applyDefaults 的默认不一致")
	}
	if !strings.Contains(render, "sanitize_blacklist_fingerprints == null ? true") {
		t.Error("指纹清洗开关在键缺失时没有按「开」显示，与 Go 侧 boolOr(..., true) 不一致")
	}
	// 两个 *bool 必须显式回写：Go 侧缺省是「开」，不回写就等于关不掉。
	if !strings.Contains(save, `sticky.enabled = $("#cfgStickyEnabled").checked`) {
		t.Error("session_sticky.enabled 没有显式回写：勾掉之后关不掉")
	}
	if !strings.Contains(save, `features.sanitize_blacklist_fingerprints = $("#cfgSanitizeFingerprints").checked`) {
		t.Error("features.sanitize_blacklist_fingerprints 没有显式回写：勾掉之后关不掉")
	}
	// 顶层键名必须是参考的那三个，写错了服务端会当未知键原样保留（静默失效）。
	for _, want := range []string{"patch.session_sticky", "patch.prompt", "patch.features"} {
		if !strings.Contains(save, want) {
			t.Errorf("saveConfig 没有写 %s", want)
		}
	}
}

// TestUsageViewShowsTheWindowAverageLatency 钉住用量视图的窗口「平均延迟」。
//
// 参考 renderUsage（app.js:1274-1279）把六个窗口合计交给 usStat 渲染：请求数、
// 总 token、prompt、completion、失败尝试、平均延迟。本仓把前五个分别落在
// #usStats 的「请求 / 失败」与 #usChart 的 tokens 行上，只有平均延迟一度谁都没
// 显示——而数字一直躺在 totals.avg_latency_ms 里（internal/panel/usage.go:148，
// usage_test.go 也断言过它等于 2000）。所以这是纯渲染缺口：Go 侧齐了、JSON 发了、
// 页面没画，两边都不报错。
func TestUsageViewShowsTheWindowAverageLatency(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, "function fmtMs(") {
		t.Fatal("index.html has no fmtMs：这一行会渲染成 undefined")
	}
	body := poolStatsFuncBody(t, src, "renderUsage")
	if !strings.Contains(body, `row("平均延迟", fmtMs(t.avg_latency_ms))`) {
		t.Error("renderUsage 没有渲染窗口平均延迟：参考 usStat 的六格里少一格")
	}
	// 读的必须是窗口合计 t.*，不是某一行的 b.*：这六格的口径是「所选窗口的合计」，
	// 用错对象会在表里取到 undefined 而不报错。
	if !strings.Contains(body, "d.totals") {
		t.Error("renderUsage 没有取 d.totals")
	}
}
