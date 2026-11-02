package panel

import (
	"strings"
	"testing"
)

// 表单字段级错误的程序化出口（issue #120）。
//
// 出错的字段原来只有「红边 + 旁边一段红字」：没有 aria-invalid、没有
// aria-describedby、没有焦点移动，弹窗顶部那条兜底错误也没有 role。读屏用户提交
// 一个缺必填项的表单，听到的与没出错时完全一样（AX 里 invalid=false、无 description）。
//
// 面板其余部分是做过无障碍的（aria-live 12 处、role=dialog 13 个、aria-expanded
// 29 处），所以这一格是漏掉而不是不打算做。下面按两处落点各钉一条。

// TestManualAddFieldErrorsReachTheAccessibilityTree 钉住手动添加账号弹窗。
func TestManualAddFieldErrorsReachTheAccessibilityTree(t *testing.T) {
	src := poolStatsUISource(t)

	// 错误行必须有稳定 id，否则 aria-describedby 无处可指。
	if !strings.Contains(src, `'<div class="fldErr" id="' + esc(fldErrId(f.key)) + '" hidden></div></div>'`) {
		t.Fatal("字段错误行没有稳定 id：input 的 aria-describedby 指不到它，读屏永远念不出错误文字（issue #120）")
	}
	body := poolStatsFuncBody(t, src, "manualFieldError")
	for _, want := range []string{
		`inp.setAttribute("aria-invalid", "true")`,
		`inp.setAttribute("aria-describedby", err.id)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("manualFieldError 缺少 %q：红边只对视觉用户存在（issue #120）", want)
		}
	}
	// 清空错误时必须把程序化状态一起清掉，否则填好的字段仍被报成「无效」。
	clear := poolStatsFuncBody(t, src, "clearManualErrors")
	for _, want := range []string{
		`el.removeAttribute("aria-invalid")`,
		`el.removeAttribute("aria-describedby")`,
	} {
		if !strings.Contains(clear, want) {
			t.Errorf("clearManualErrors 缺少 %q：字段填对了但无障碍树里仍是无效（issue #120）", want)
		}
	}
	// 提交被本地拦下时把焦点送到第一个出错字段。
	if !strings.Contains(src, "if ($$(\"#manFields .fldErr\").some(el => !el.hidden)) { focusFirstManualError(); return; }") {
		t.Error("提交被拦下时没有把焦点送到第一个出错字段：读屏用户得自己逐个 Tab 去找（issue #120）")
	}
	if !strings.Contains(src, "function focusFirstManualError()") {
		t.Error("缺少 focusFirstManualError")
	}
	// 弹窗级的兜底错误要能被播报。
	if !strings.Contains(src, `id="manErr" class="state err" role="alert" hidden`) {
		t.Error("#manErr 没有 role=alert：说不清哪一栏时的兜底错误不会被播报（issue #120）")
	}
}

// TestDurationFieldsReportInvalidAndDescribe 钉住配置页时长字段那一处。
func TestDurationFieldsReportInvalidAndDescribe(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "markDurationFields")

	if !strings.Contains(body, `el.setAttribute("aria-invalid", "true")`) {
		t.Fatal("时长字段出错时没有 aria-invalid：AX 里 invalid 恒为 false（issue #120）")
	}
	// 说明元素由标记提供（shell_test 的「脚本只许引用标记里有的 id」那条守着），
	// 所以这里只要求 aria-describedby 指向它，而元素本身在标记里。
	if !strings.Contains(body, `el.setAttribute("aria-describedby", tip.id)`) ||
		!strings.Contains(body, "const tip = durTipEl();") {
		t.Error("时长字段没有指向说明文字的 aria-describedby：title 里的那句话读屏通常不念")
	}
	if !strings.Contains(src, `<span class="sr-only" id="durTip"></span>`) {
		t.Error("说明元素不在标记里：运行时 createElement 会让「脚本只许引用标记里有的 id」那条守卫失效")
	}
	// 合法值必须把两个属性摘掉，否则一个填对的字段仍被报成无效。
	for _, want := range []string{
		`el.removeAttribute("aria-invalid")`,
		`el.removeAttribute("aria-describedby")`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("markDurationFields 缺少 %q：填对之后状态不复位（issue #120）", want)
		}
	}
	// 说明元素必须真的能进无障碍树：sr-only 是视觉隐藏，hidden/display:none 不是。
	if !strings.Contains(src, `class="sr-only" id="durTip"`) {
		t.Error("说明元素不是 sr-only：用 hidden 或 display:none 的话读屏也读不到（issue #120）")
	}
	// 文案只有一个来源：标记里留空，由 DUR_TIP 写入。
	if !strings.Contains(src, "if (el && !el.textContent) el.textContent = DUR_TIP;") {
		t.Error("说明文案不是从 DUR_TIP 写进去的：两份文案会漂移")
	}
	if !strings.Contains(src, ".sr-only {") {
		t.Error("缺少 .sr-only 工具类")
	}
	if strings.Contains(src, `.sr-only { display: none`) {
		t.Error(".sr-only 用了 display:none：那样无障碍树里也没有它")
	}
}
