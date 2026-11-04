package panel

import (
	"strings"
	"testing"
)

// 钉住卡片头说明的截断出口（issue #186）。
//
// `.box > header h3 .hint` 曾被 max-width + ellipsis + nowrap 截断，截掉的
// 半句没有任何办法看到（无 title、无 hover、无滚动）——而同一份文件里
// `.savebar > .hint:hover { white-space: normal }` 早就把这类截断的正确
// 出口写过一遍。说明文字讲的是「这页能干什么、保存后要不要重启」，必须
// 读得到全文。
func TestCardHeadHintHasATruncationExit(t *testing.T) {
	src := string(indexHTML)

	// 选择器有三条同名规则（923/4181 只管字号颜色，无截断），权威截断规则
	// 以「带 max-width」的那条为准：截断三件套必须同在这条里。
	var rule string
	for at := -1; ; {
		i := strings.Index(src[at+1:], ".box > header h3 .hint {")
		if i < 0 {
			break
		}
		at = at + 1 + i
		r := src[at : at+strings.Index(src[at:], "}")+1]
		if strings.Contains(r, "max-width") {
			rule = r
			break
		}
	}
	if rule == "" {
		t.Fatal("找不到带 max-width 的 .box > header h3 .hint 权威截断规则")
	}
	for _, want := range []string{"max-width: 54ch", "text-overflow: ellipsis", "white-space: nowrap"} {
		if !strings.Contains(rule, want) {
			t.Errorf("权威规则丢了 %q：%s", want, rule)
		}
	}

	// 出口一：悬停展开（与 .savebar > .hint 同款）。hover 期间必须同时放开
	// max-width，否则换行还是被 54ch 掐着，展开了个寂寞。
	if strings.Index(src, ".box > header h3 .hint:hover {") < 0 {
		t.Fatal(".box > header h3 .hint 没有 :hover 展开出口（issue #186）")
	}
	hover := shellRule(src, ".box > header h3 .hint:hover {")
	if !strings.Contains(hover, "white-space: normal") || !strings.Contains(hover, "max-width: none") {
		t.Errorf(":hover 出口必须同时有 white-space:normal 与 max-width:none：%s", hover)
	}

	// 出口二：截断属性只许在一处说话。曾 52ch/54ch 两条并存（同一属性、
	// 同一选择器、两个值），改宽度的人不知道该改哪条。
	if strings.Contains(src, "max-width: 52ch") {
		t.Error("旧值 52ch 还挂在某条规则里：与 54ch 双值并存的状态没清干净（issue #186）")
	}
	// 同名三条规则里，截断三件套只许权威那条携带（另两条只管字号颜色）。
	blocked := 0
	for at := -1; ; {
		i := strings.Index(src[at+1:], ".box > header h3 .hint {")
		if i < 0 {
			break
		}
		at = at + 1 + i
		r := src[at : at+strings.Index(src[at:], "}")+1]
		if strings.Contains(r, "text-overflow") || strings.Contains(r, "max-width") {
			blocked++
		}
	}
	if blocked != 1 {
		t.Errorf("带截断属性的同名规则有 %d 条：max-width/ellipsis 只该有一个权威来源（issue #186）", blocked)
	}

	// 同文件的 savebar 出口不许在收拾这条时被顺手删掉——它是另一处的出口。
	if strings.Index(src, ".savebar > .hint:hover { white-space: normal; }") < 0 {
		t.Error(".savebar > .hint 的悬停出口不见了：那是本修复的参照实现")
	}
}
