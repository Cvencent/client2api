package panel

import (
	"strings"
	"testing"
)

// accountnote_ui_test.go pins the per-account 「备注」 control: the panel's own
// place to record which phone number or e-mail an account signs in with.
//
// 背景：账号池里一条凭据到底对应哪个手机号/邮箱，模块自己说不出来（trae 的
// userInfo 只回一个昵称，凭据坏掉之后从账号表上看不出该用哪一个重登）。备注
// 存在网关配置里（platforms.<平台>.account_notes），跟账号优先级同一套写入路径。

// TestAccountNoteButtonIsWiredOnEveryRow: 备注是面板自己的元数据，跟模块能力
// 无关，所以按钮要在 caps.manage 之前渲染，每一行都有。
func TestAccountNoteButtonIsWiredOnEveryRow(t *testing.T) {
	src := string(indexHTML)
	body := poolStatsFuncBody(t, src, "accRowHTML")

	note := strings.Index(body, `data-do="note"`)
	if note < 0 {
		t.Fatal("accRowHTML 没有渲染「备注」按钮")
	}
	if manage := strings.Index(body, "if (caps.manage) {"); manage >= 0 && note > manage {
		t.Error("「备注」按钮被放进了 caps.manage 分支：只读模块的账号就标不了身份")
	}

	act := poolStatsFuncBody(t, src, "act")
	if !strings.Contains(act, `doIt === "note"`) || !strings.Contains(act, "openAccountNote(id)") {
		t.Error("act() 没有把 data-do=\"note\" 接到 openAccountNote")
	}
}

// TestAccountIdentityPrefersTheOperatorNote: 账号列认人的那行字优先用备注，
// 模块自己的 label 退到 hint 行；没有备注时两者都保持原样。
func TestAccountIdentityPrefersTheOperatorNote(t *testing.T) {
	src := string(indexHTML)
	ident := poolStatsFuncBody(t, src, "accountIdentity")
	for _, want := range []string{
		`const own = String((a && a.operator_note) || "").trim();`,
		"if (own) return own;",
		`return String((a && a.label) || "").trim() || String((a && a.id) || "");`,
	} {
		if !strings.Contains(ident, want) {
			t.Errorf("accountIdentity 少了：%s", want)
		}
	}

	row := poolStatsFuncBody(t, src, "accRowHTML")
	if !strings.Contains(row, "esc(accountIdentity(a))") {
		t.Error("账号列没有用 accountIdentity，备注不会显示出来")
	}
	if !strings.Contains(row, "esc(accountHint(a))") {
		t.Error("hint 行没有走 accountHint，模块自己的 label 会丢")
	}
	// 重登弹层也要认备注：认不出人就没法重登。
	for _, fn := range []string{"openRelogin", "openBrowserRelogin", "openReloginTarget"} {
		body := poolStatsFuncBody(t, src, fn)
		if !strings.Contains(body, "accountIdentity(acc)") && !strings.Contains(body, "acc.operator_note") {
			t.Errorf("%s 没有认操作员记的备注", fn)
		}
	}
}

// TestAccountNoteModalSavesThroughTheConfig: 保存必须走账号优先级那条
// PATCH /panel/api/config → 热加载的路径，写进 account_notes，而不是只改
// 前端的对象（那样刷新一次就没了）。
func TestAccountNoteModalSavesThroughTheConfig(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		`<div class="veil" id="noteVeil">`,
		`id="noteInput"`,
		`id="btnNoteSave"`,
		`id="btnNoteClear"`,
		`$("#noteVeil").addEventListener("click", ev => { if (ev.target === $("#noteVeil")) closeAccountNote(); });`,
		`$("#btnNoteSave").addEventListener("click", saveAccountNote);`,
		`if (v.id === "noteVeil") { closeAccountNote(); return true; }`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("index.html 少了备注弹层需要的接线：%s", want)
		}
	}

	save := poolStatsFuncBody(t, src, "saveAccountNote")
	for _, want := range []string{
		`body.platforms[CUR] = { account_notes: {} };`,
		`body.platforms[CUR].account_notes[id] = value === "" ? null : value;`,
		`api("/panel/api/config", { method: "PATCH", body })`,
		`api("/panel/api/reload", { method: "POST" })`,
	} {
		if !strings.Contains(save, want) {
			t.Errorf("saveAccountNote 少了这一步：%s", want)
		}
	}
	// 清除走 null（与优先级一致）；直接把空字符串写进配置会留一个没用的条目。
	if strings.Contains(save, "account_notes[id] = value;") {
		t.Error("saveAccountNote 把空字符串直接写进了配置")
	}
}

// TestCreditsViewGroupsByIdentity 钉住积分构成视图的合并逻辑：它和账号池
// 用同一套 identity 分组，卡片、到期分布和明细表都渲染合并后的行。这全是
// 字符串拼接，Go 编译器管不到，所以静态钉住调用链；分组函数被改名或没接
// 上时这里会红。
func TestCreditsViewGroupsByIdentity(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		"function crGroups(list)",
		"function crMerge(g)",
		"function crIdentityName(rows)",
		"const merged = crGroups(list).map(crMerge);",
		"renderExpiryDistribution(merged, now)",
		"pkSummaryCards(merged, now)",
		"merged.map(a => crAcctRow(a, limit))",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("credits view missing %q; grouping was likely unwired", want)
		}
	}
}
