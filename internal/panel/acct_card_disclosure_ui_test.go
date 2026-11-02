package panel

import (
	"strings"
	"testing"
)

// 账号池卡片墙的两条（issue #118 + #119）。
//
// #118 是交互缺陷：重建豁免的判据是「焦点在这个容器内」，而板卡自己
// tabindex="0"，于是点一下（焦点落在板卡上）之后每一拍都被豁免，ACC_OPEN 翻了
// 而界面永远不画（实测 9 秒不动，blur 一下立刻正常）。
//
// #119 是 a11y 缺陷：aria-expanded 写在 <article> 上，而 article 不支持这个
// 属性，AX 树里连 expanded 都没有；这一层也没有任何名字，Tab 过去只念「article」。
//
// 两条都在同一个渲染函数里，且 #119 的修法顺手把 #118 的「点哪」变成一个有语义的
// 控件，所以放一个文件。

// TestRebuildExemptionCoversFormControlsOnly 钉住 #118：豁免必须只看**表单控件**。
func TestRebuildExemptionCoversFormControlsOnly(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, "const editingInTable = (isFormControl && (tb.contains(active) || board.contains(active)))") {
		t.Fatal("重建豁免不再只覆盖表单控件：板卡/组头行自己 tabindex=0，点一下就会把后续每一拍都豁免掉（issue #118）")
	}
	// 焦点在板卡上时不算「正在编辑」——这一条是缺陷本身。
	if strings.Contains(src, "const editingInTable = tb.contains(document.activeElement) ||\n    board.contains(document.activeElement)") {
		t.Error("豁免判据退回「焦点在容器内」：点击板卡后展开永不渲染（issue #118 回归）")
	}
	// 表单控件那一档必须真的判 input/select/textarea（滑杆、下拉、输入框）。
	if !strings.Contains(src, `active.matches("input, select, textarea, [contenteditable=true]")`) {
		t.Error("表单控件判据缺失：编辑豁免（#23/#25）要保护的就是这三类控件")
	}
	// 按住中的滑杆/下拉仍要拦：那是拖到一半不能被重建抽走的那一类。
	for _, want := range []string{".accPrio:active", "select.test-model:active"} {
		if !strings.Contains(src, want) {
			t.Errorf("豁免丢了 %q：正在拖动的滑杆/打开的下拉会被重建打断（#25 回归）", want)
		}
	}
}

// TestCardDisclosureLivesOnASupportedRole 钉住 #119：披露语义 + 可访问名。
func TestCardDisclosureLivesOnASupportedRole(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderAcctBoard")

	// aria-expanded 不许再写在 <article> 上（article 不支持它，AX 树会丢掉）。
	if strings.Contains(body, `'<article class="acct-card '`) && strings.Contains(body, `' tabindex="0" aria-expanded="'`) {
		t.Fatal("aria-expanded 又写回 <article> 上了：article 不支持这个属性，无障碍树里两态一样（issue #119）")
	}
	// 卡片头是披露控件：role=button + aria-expanded + aria-controls + 名字。
	for _, want := range []string{
		`'<div class="ac-head" id="' + headId + '" role="button" tabindex="0"`,
		`' aria-expanded="' + (open ? "true" : "false") + '" aria-controls="' + detailId + '"'`,
		`' aria-labelledby="' + headId + '-n ' + headId + '-s">'`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("卡片头缺少披露语义片段 %q（issue #119）", want)
		}
	}
	// 名字指向的两段文字必须真的存在且带对应 id。
	if !strings.Contains(body, `'<span class="ac-name" id="' + headId + '-n"`) {
		t.Error("账号名没有 id：aria-labelledby 指不到它，读屏仍然念不出这块卡片是什么")
	}
	if !strings.Contains(body, `'<span class="ac-state" id="' + headId + '-s">'`) {
		t.Error("状态词没有 id：可访问名里缺了「可用/冷却中」这一档")
	}
	// aria-controls 的目标必须真的画出那个 id。
	if !strings.Contains(body, `acctDetailHTML(g, a, caps, acts, showBal, prio, detailId)`) {
		t.Error("详情块没有拿到 detailId：aria-controls 指向一个不存在的 id（issue #119）")
	}
	detail := poolStatsFuncBody(t, src, "acctDetailHTML")
	if !strings.Contains(detail, `(detailID ? ' id="' + esc(detailID) + '"' : "")`) {
		t.Error("acctDetailHTML 没有把 detailID 落到 .ac-detail 上")
	}
	// 3D 检视复用同一个渲染器且不带披露控件：它必须仍然能不给 id 地调用。
	if !strings.Contains(src, "acctDetailHTML(g, a, caps, acts, showBal, prio);") {
		t.Error("3D 检视的 acctDetailHTML 调用被改了：那条路径没有披露控件，不该要求 id")
	}
}
