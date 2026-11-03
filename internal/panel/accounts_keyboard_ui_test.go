package panel

import (
	"regexp"
	"strings"
	"testing"
)

// 账号池行内控件的键盘契约（issue #139 + #140）。
//
// 两单都在同一片委托代码里，而且都是**文本断言拦不住**的那一类：
//   - #139：`accPaneKey` 把板卡内任何 Enter/空格都当「展开/收起」，连同按钮自己
//     的激活一起 `preventDefault` 掉 —— 按钮的 click 根本不派发。
//   - #140：每 5 秒重建把焦点所在的按钮整个换掉，焦点掉回 body，用户刚要按的
//     那一下回车就没了；而 #118 的教训是**不能**再把豁免放宽成「焦点在容器内」。
//
// 行为判据在 cmd/panelsmoke 的真浏览器阶段（assertAccountsKeyboard）；这里钉的是
// **会漂移的那几条静态契约**，尤其是「豁免集合里有什么、没有什么」——它正是这两
// 单之间那条窄缝。

// stripLineComments removes JS `// ...` comments line by line, so an assertion
// about the ORDER of two statements cannot be fooled by prose that mentions
// them (which is exactly how the first version of this test failed).
func stripLineComments(src string) string {
	lines := strings.Split(src, "\n")
	for i, l := range lines {
		if j := strings.Index(l, "//"); j >= 0 {
			lines[i] = l[:j]
		}
	}
	return strings.Join(lines, "\n")
}

// acctFuncBody 截取一个顶层 function 的函数体（到行首 "}" 为止）。
func acctFuncBody(t *testing.T, src, name string) string {
	t.Helper()
	i := strings.Index(src, "function "+name+"(")
	if i < 0 {
		t.Fatalf("index.html has no function %s", name)
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n}"); j >= 0 {
		return rest[:j]
	}
	t.Fatalf("function %s looks unterminated", name)
	return ""
}

// TestThePaneKeyHandlerYieldsToControls 钉住 #139。
func TestThePaneKeyHandlerYieldsToControls(t *testing.T) {
	src := poolStatsUISource(t)
	body := acctFuncBody(t, src, "accPaneKey")

	// 注释里也会出现 preventDefault / closest 这些词，所以只在**去掉注释后**的
	// 代码上比位置（决策顺序才是契约）。stripPanelComments 只处理 /* */ 与
	// <!-- -->，而这是 JS，还有行注释要去。
	code := stripLineComments(stripPanelComments(body))

	// 必须在 preventDefault 之前先判断事件目标是控件，并把按键让回去。
	i := strings.Index(code, "preventDefault")
	if i < 0 {
		t.Fatal("accPaneKey 不再 preventDefault：板卡头部的 Enter/空格展开会失效（#118 的另一半）")
	}
	head := code[:i]
	if !strings.Contains(head, "ev.target.closest") {
		t.Fatal("accPaneKey 在 preventDefault 之前没有先看事件目标：板卡里任何按钮上的 Enter/空格都会被当成「展开板卡」并吃掉按钮的激活（#139）")
	}
	if !strings.Contains(head, "button") {
		t.Error("accPaneKey 的控件豁免没有列 button：行内按钮的键盘激活仍然拿不回来（#139）")
	}
	// 反向钉子：不能把 [role=button] 收进来，否则板卡头部（div.ac-head[role=button]）
	// 自己也被豁免，展开/收起的键盘路径就没了 —— #118 会被带回来。
	sel := regexp.MustCompile(`ev\.target\.closest\("([^"]*)"\)`).FindStringSubmatch(head)
	if sel == nil {
		t.Fatal("没有读到 accPaneKey 的控件选择器")
	}
	if strings.Contains(sel[1], "role=button") {
		t.Errorf("accPaneKey 的豁免把 [role=button] 收进来了（%q）：板卡头部就是 role=button，"+
			"它会被一起豁免、Enter 不再展开（#118 回归）", sel[1])
	}
}

// TestARebuildRestoresFocusByIdentity 钉住 #140。
func TestARebuildRestoresFocusByIdentity(t *testing.T) {
	src := poolStatsUISource(t)

	// renderAccounts 的重建路径必须调用还原函数（否则焦点还是掉回 body）。
	render := acctFuncBody(t, src, "renderAccounts")
	if !strings.Contains(render, "restoreAcctFocus(") {
		t.Fatal("renderAccounts 重建后没有还原焦点：焦点所在的按钮被换掉、掉回 body，用户刚要按的回车丢失（#140）")
	}
	if !strings.Contains(render, "acctFocusKey(") {
		t.Error("renderAccounts 没有先取焦点的稳定身份：无法在重建后找回等价元素（#140）")
	}

	// 身份判据：按钮按 data-do（+data-id/data-act）定位；板卡头部必须被排除。
	key := acctFuncBody(t, src, "acctFocusKey")
	if !strings.Contains(key, "data-do") {
		t.Error("acctFocusKey 没有用 data-do 作为身份：按钮在重建后换的是元素、不是身份（#140）")
	}
	if !strings.Contains(key, "ac-head") {
		t.Error("acctFocusKey 没有排除 .ac-head：板卡头部必须继续被重画，豁免它会把 #118 带回来")
	}

	// 还原必须在**可见**容器里找：表格与卡片墙同时存在 DOM，另一个是隐藏的。
	restore := acctFuncBody(t, src, "restoreAcctFocus")
	if !strings.Contains(restore, "acct-mode-board") {
		t.Error("restoreAcctFocus 没有按当前呈现选择容器：会在隐藏的那份里找到元素，" +
			"焦点落到不可见按钮上等于没还原（#140 的实现陷阱）")
	}
}

// TestTheCardHeadKeepsItsToggleKeys 是 #118 的反向钉子：豁免放宽时最容易踩的就是它。
func TestTheCardHeadKeepsItsToggleKeys(t *testing.T) {
	src := poolStatsUISource(t)
	// 板卡头部仍是 role=button + tabindex=0 + data-grp，且由 accPaneKey 承担键盘展开。
	if !strings.Contains(src, `class="ac-head"`) || !strings.Contains(src, `role="button" tabindex="0" data-grp=`) {
		t.Fatal("板卡头部的 role/tabindex/data-grp 组合变了：键盘展开的判据依赖它（#118/#139）")
	}
	// accPaneClick 里的**表单**豁免必须仍是「只豁免表单控件」：它同时承担把
	// button[data-do] / button[data-goto] 交给 act()/跳转的职责，所以判据是那条
	// 豁免的选择器，不是「这个函数里出现过 button」。
	click := stripPanelComments(acctFuncBody(t, src, "accPaneClick"))
	sel := regexp.MustCompile(`closest\("([^"]*input[^"]*)"\)`).FindStringSubmatch(click)
	if sel == nil {
		t.Fatal("accPaneClick 里的表单豁免不见了：拖滑杆时会误触发展开/收起（#23 的一半）")
	}
	if strings.Contains(sel[1], "button") {
		t.Errorf("accPaneClick 的表单豁免把 button 也收进来了（%q）：行内按钮的鼠标点击不会再走委托（#139 的鼠标侧）", sel[1])
	}
}
