package panel

import (
	"strings"
	"testing"
)

// TestAccountStatusAndIdentityLabelsAreDistinct: 账号池表格里的「状态说明」
// 是模块返回的账号状态；行内原来的「备注」按钮记录的是操作员自己填的手机号/
// 邮箱。两者必须在界面上有完全不同的名字，否则操作员会把它们当成同一项。
func TestAccountStatusAndIdentityLabelsAreDistinct(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		`<th class="c-note" title="`,
		`>状态说明</th>`,
		`<h3>账号身份标识</h3>`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("index.html 少了区分状态说明与账号标识所需的内容：%s", want)
		}
	}

	row := poolStatsFuncBody(t, src, "accRowHTML")
	if !strings.Contains(row, "账号标识") {
		t.Error("行内按钮仍然没有改成「账号标识」")
	}
	if strings.Contains(row, ">备注</button>") || strings.Contains(row, "备注 ✓</button>") {
		t.Error("行内按钮仍然使用「备注」，会和状态说明列混淆")
	}
	if !strings.Contains(row, "状态说明") {
		t.Error("账号标识按钮的提示没有明确说明它和「状态说明」不是同一项")
	}
}

// TestStatusNoteCellAlwaysCarriesFullTip: 状态说明列即使被截断，也必须始终
// 提供完整提示；有中文翻译时提示里同时保留原始值，避免格子里和悬停内容看起来
// 是两套不同的信息。
func TestStatusNoteCellAlwaysCarriesFullTip(t *testing.T) {
	body := poolStatsFuncBody(t, string(indexHTML), "noteCell")
	for _, want := range []string{
		`const tip =`,
		`原始值：`,
		`aria-label="状态说明：`,
		`title="' + esc(tip) + '"`,
		`tabindex="0"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("noteCell 没有完整提示所需的内容：%s", want)
		}
	}
	if strings.Contains(body, `(zh ? ' title="'`) {
		t.Error("noteCell 仍然只在有中文翻译时设置 title，未翻译的备注会看不到完整内容")
	}
}

// TestStatusNoteColumnIsWiderAndWrapsTwoLines: 状态说明列要放宽，并允许最多
// 两行；长内容仍由完整提示兜底，而不是继续单行 100px 截断。
func TestStatusNoteColumnIsWiderAndWrapsTwoLines(t *testing.T) {
	src := string(indexHTML)
	for _, want := range []string{
		`#view-accounts table.acc .col-note { width: 15%; }`,
		`-webkit-line-clamp: 2;`,
		`display: -webkit-box;`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("状态说明列没有准备好宽列或两行展示：%s", want)
		}
	}
	if strings.Contains(src, `#view-accounts table.acc td .note { display: inline-block; max-width: 100px;`) {
		t.Error("状态说明列仍然是单行 100px 截断")
	}
}
