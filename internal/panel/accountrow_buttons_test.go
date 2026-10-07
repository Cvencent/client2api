package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 账号行操作列的界面回归测试。
//
// 背景一：操作列里同时摆着「停用/启用」和「恢复」两个按钮，但它们对操作员
// 是同一个意图的两半——core.Reviver 的契约（internal/core/revive.go）写明
// revive 会连同「人工停用的凭据」一起重新启用，zcode / loomy 直接把 Enabled
// 置回 true，qwenwork / raccoon 清 Disabled，trae / workbuddy / kimi 先调
// SetAccountEnabled(id, true)。于是同一行上出现两个都在说"让我能用这个账号"
// 的按钮，点错一个只是白等一次网络往返。它们必须合并成一个：按当前状态显示
// 「停用」还是「恢复」。
//
// 背景二：行内按钮里唯一会露英文的是签到标签——它由各模块自己给（core.
// CheckinAction.Label），模块写英文，面板就照着显示英文。面板必须统一翻成中文。
//
// 这些全是 index.html 里的字符串拼接，Go 编译器管不到，所以静态钉住。
// ---------------------------------------------------------------------------

// accRowWiring 返回行内按钮里读错状态 / 用错语言的地方。返回空表示全对。
//
// 返回列表而不是就地断言，是为了让下面的负控能用同一段代码证明它真的会红。
func accRowWiring(t *testing.T, src string) []string {
	t.Helper()
	var miss []string
	row := poolStatsFuncBody(t, src, "accRowHTML")
	toggle := poolStatsFuncBody(t, src, "accToggleButton")

	// 1. 合并后的按钮由 accToggleButton 独家渲染，accRowHTML 里不该再有第二个
	//    「启用/停用」或独立的「恢复」。
	if !strings.Contains(row, "accToggleButton(a, caps)") {
		miss = append(miss, "accRowHTML 没有调用 accToggleButton 渲染那个开关")
	}
	if strings.Contains(row, `a.enabled ? "停用" : "启用"`) {
		miss = append(miss, "accRowHTML 里还留着旧的「停用/启用」按钮，它应交给 accToggleButton")
	}

	// 2. 文案必须读 a.enabled：只看 state 分不出「人工停用」和「凭据失效」，
	//    而这两种都要能走同一条恢复路径。
	if !strings.Contains(toggle, "a.enabled") {
		miss = append(miss, "accToggleButton 没有读 a.enabled，无法区分该显示「停用」还是「恢复」")
	}
	// 3. 三种文案都要在，且「恢复」只给真能清惩罚的模块（caps.revive）——面板
	//    不能摆一个点了只会 501 的按钮。
	for _, want := range []string{"停用", "恢复", "启用"} {
		if !strings.Contains(toggle, want) {
			miss = append(miss, "accToggleButton 缺少文案 "+want)
		}
	}
	if !strings.Contains(toggle, "caps.revive") {
		miss = append(miss, "accToggleButton 没有按 caps.revive 区分「恢复」和「启用」")
	}
	// 4. 两条路由都要在：启用中翻开关，没启用走 revive（连清惩罚一起做）。
	for _, want := range []string{`data-do="toggle"`, `data-do="revive"`} {
		if !strings.Contains(toggle, want) {
			miss = append(miss, "accToggleButton 缺少路由 "+want)
		}
	}

	// 5. 签到标签必须走翻译层，不能直接把模块给的 label 显示出去。
	if !strings.Contains(row, "checkinActionLabel(") {
		miss = append(miss, "accRowHTML 没有用 checkinActionLabel 翻译签到标签")
	}
	if strings.Contains(row, `esc(acts[0] ? acts[0].label : "签到")`) {
		miss = append(miss, "单一签到动作的按钮仍在直接用模块给的 label")
	}
	if !strings.Contains(src, "function checkinActionLabel(") {
		miss = append(miss, "面板里没有 checkinActionLabel 翻译函数")
	}
	return miss
}

func TestAccountRowButtonsWiring(t *testing.T) {
	if miss := accRowWiring(t, poolStatsUISource(t)); len(miss) > 0 {
		t.Fatalf("账号行按钮接错了：\n  - %s", strings.Join(miss, "\n  - "))
	}
}

// TestAccRowWiringCatchesTheOldTwoButtonLayout 是负控：把界面改回老布局
// （accRowHTML 里自己按 a.enabled 渲染「停用/启用」，不再走 accToggleButton），
// 检查器必须报出来——否则上面那条断言可能只是在自证。
func TestAccRowWiringCatchesTheOldTwoButtonLayout(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, "btns.push(accToggleButton(a, caps));") {
		t.Fatalf("测试自己失效了：accRowHTML 里找不到 accToggleButton 的调用")
	}
	broken := strings.Replace(src, "btns.push(accToggleButton(a, caps));",
		`btns.push('<button class="xs" data-do="toggle" data-id="' + esc(a.id) + '" data-on="' + (a.enabled ? "1" : "0") + '">' + (a.enabled ? "停用" : "启用") + '</button>');`, 1)
	if miss := accRowWiring(t, broken); len(miss) == 0 {
		t.Fatal("把行内按钮改回「停用/启用 + 独立恢复」的老布局之后，检查器还是绿的")
	}
}

// TestAccountRowOwnButtonsAreChinese 钉住面板自己写的行内按钮文案。签到按钮走
// 翻译层，由 TestAccountRowButtonsWiring 验。
func TestAccountRowOwnButtonsAreChinese(t *testing.T) {
	row := poolStatsFuncBody(t, poolStatsUISource(t), "accRowHTML")

	// data-do -> 该行按钮上必须出现的中文标签（逐字出现在渲染代码里）。
	wants := []struct{ do, label string }{
		{`data-do="note"`, "账号标识"},
		{`data-do="test"`, "测试"},
		{`data-do="del"`, "删除"},
		{`data-do="relogin"`, "重登"},
		{`data-do="balance"`, "余额"},
		{`data-do="tasks"`, "任务"},
	}
	for _, w := range wants {
		i := strings.Index(row, w.do)
		if i < 0 {
			t.Errorf("accRowHTML 没有渲染 %s 按钮", w.do)
			continue
		}
		// 按钮标签在这一行 '</button>' 之前。
		rest := row[i:]
		end := strings.Index(rest, "</button>")
		if end < 0 {
			t.Errorf("%s 按钮的渲染代码没有闭合", w.do)
			continue
		}
		if !strings.Contains(rest[:end], `"`+w.label+`"`) && !strings.Contains(rest[:end], w.label) {
			t.Errorf("%s 按钮上没有中文标签 %q，渲染代码：%s", w.do, w.label, rest[:end])
		}
	}
}

// TestCheckinActionLabelTranslatesEveryModule 钉住翻译层：所有模块现在真实给出的
// 签到标签都得能翻成中文，不许有英文漏到按钮上。
func TestCheckinActionLabelTranslatesEveryModule(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, "function checkinActionLabel(") {
		t.Fatal("面板里没有 checkinActionLabel 翻译函数")
	}
	// 这些是各模块 CheckinActions 现在真实给出的 label（见 clients/*/*.go）：
	// codearts "Daily sign-in"、lobsterai "Daily check-in"、loomy "Claim the
	// daily gift allowance"、workbuddy "Daily check-in (CN)" 与 "Daily activity
	// (international)"、zcode "领取活动套餐 (claim a promotion)"。英文原文出现在
	// 翻译表里是允许的（那是查表用的键），但每一个都必须有对应的中文。
	for _, en := range []string{
		"Daily sign-in",
		"Daily check-in",
		"Daily check-in (CN)",
		"Claim the daily gift allowance",
		"Daily activity (international)",
		"claim a promotion",
	} {
		if !strings.Contains(src, en) {
			t.Errorf("翻译表里没有 %q，模块的这个签到标签会原样显示成英文", en)
		}
	}
	// 翻译结果必须是中文：表里每个英文键都得映射到含中文字符的值。
	for _, cn := range []string{"每日签到", "每日活跃", "领取每日赠送额度", "领取活动套餐"} {
		if !strings.Contains(src, cn) {
			t.Errorf("翻译表里没有中文译文 %q", cn)
		}
	}
	// 已经写成中文的标签要能原样透传，不能被翻译层改坏：面板不查表不等于不能
	// 显示，checkinActionLabel 查不到就返回原文。
	if !strings.Contains(src, "CHECKIN_LABELS[raw] || raw") {
		t.Error("checkinActionLabel 查不到就返回原文的兜底没了，模块新加的中文标签会被吞掉")
	}
}
