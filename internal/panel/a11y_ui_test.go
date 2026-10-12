package panel

import (
	"strings"
	"testing"
)

// index.html 的可访问性回归。
//
// 这一组测试钉的都是「浏览器能渲染、Go 能编译，但辅助技术读不出来」这类退化：
// 面板的界面全是字符串拼出来的，一个标签或属性漏掉不会有任何一边报错，只会在
// 真实使用里变成「这里有个控件，但不知道它是干什么的」。

// TestScriptBuiltCheckboxesCarryAnAccessibleName 覆盖由 JS 拼出来的复选框。
//
// 面板里绝大多数 input 手写在 HTML 里，天然带着上下文；定时任务页那一列「启用」
// 开关是 JS 逐行拼出来的，每一行除了行首的任务名再没有任何标识，所以拼的时候
// 必须自带名字。少了它，整列在无障碍树里就是一串匿名复选框。
func TestScriptBuiltCheckboxesCarryAnAccessibleName(t *testing.T) {
	src := poolStatsUISource(t)

	for _, want := range []string{
		`class="scOn" aria-label=`,
		`启用定时任务：`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the scheduled-task toggle is missing %s", want)
		}
	}
}

// TestHandWrittenCheckboxesAreWrappedInALabel 覆盖手写 HTML 里的复选框。
//
// 这些控件的标签文字直接跟在 input 后面，可访问名由 <label> 隐式给出。把它们
// 从 <label> 里挪出来，页面看着一模一样，只是没了名字。
func TestHandWrittenCheckboxesAreWrappedInALabel(t *testing.T) {
	src := poolStatsUISource(t)

	for _, id := range []string{"scMaster", "scOnlyOwn"} {
		i := strings.Index(src, `id="`+id+`"`)
		if i < 0 {
			t.Errorf("index.html has no #%s checkbox", id)
			continue
		}
		before := src[:i]
		labelOpen := strings.LastIndex(before, "<label")
		labelClose := strings.LastIndex(before, "</label>")
		if labelOpen < 0 || labelOpen < labelClose {
			t.Errorf("#%s is not inside a <label>, so it has no accessible name", id)
			continue
		}
		// 标签里还得真的有文字，否则名字是空的。
		rest := src[i:]
		end := strings.Index(rest, "</label>")
		if end < 0 {
			t.Errorf("#%s has no closing </label>", id)
			continue
		}
		if text := stripTags(rest[:end]); text == "" {
			t.Errorf("#%s sits in an empty <label>, so its accessible name is blank", id)
		}
	}
}

// TestCredentialInputsDeclareAutocomplete pins the attribute on every secret
// field the panel renders.
//
// These are vendor API keys and one-time tokens the operator pastes by hand,
// not a login the browser should ever remember or offer to fill. Without an
// explicit autocomplete the browser treats them as login fields: Chrome logs a
// "password field is not contained in a form" hint for each one and may offer
// to autofill credentials the operator never typed here.
func TestCredentialInputsDeclareAutocomplete(t *testing.T) {
	src := poolStatsUISource(t)

	for _, id := range []string{"sourceKey", "ownKeyValue", "smsToken", "keyInput"} {
		i := strings.Index(src, `id="`+id+`"`)
		if i < 0 {
			t.Errorf("index.html has no #%s input", id)
			continue
		}
		// Take the whole opening tag, up to the first ">".
		rest := src[i:]
		end := strings.Index(rest, ">")
		if end < 0 {
			t.Errorf("#%s has no closing >", id)
			continue
		}
		tag := rest[:end]
		if !strings.Contains(tag, `autocomplete=`) {
			t.Errorf("#%s carries a credential but declares no autocomplete attribute: %s", id, tag)
		}
	}
}

// stripTags removes element tags and collapses whitespace, which is all the
// label-text check above needs.
func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch r {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				b.WriteRune(r)
			}
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
