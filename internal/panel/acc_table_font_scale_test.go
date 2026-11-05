package panel

import (
	"strings"
	"testing"
)

// 钉住账号表 200% 字号下的列宽契约（issue #204）：
//
//	列宽是百分比 + px 下限 + table-layout:fixed，全是绝对长度——字号翻倍后
//	格宽不变而墨迹翻倍，「在途」格的内容直接画到「用量」列上。正解=下限
//	用 em（格子随字号长大，.tbl-wrap 横滚兜底）+ td.num 裁剪兜底 + title 全文。
func TestAccountTableGrowsWithTheFont(t *testing.T) {
	src := poolStatsUISource(t)

	// min-width 在常规层只许一处权威声明（媒体查询里的降级 65em 是另一档，
	// 合法）；单位必须是 em；旧 px 值清零。
	rules := strings.Count(src, "\n#view-accounts table.acc { min-width")
	if rules != 1 {
		t.Errorf("#view-accounts table.acc 常规层的 min-width 出现 %d 次：必须只有一处权威声明（issue #204，曾有 1290/1180/1040px 三条并存）", rules)
	}
	if strings.Contains(src, "min-width: 1180px") || strings.Contains(src, "min-width: 1290px") || strings.Contains(src, "min-width: 1040px") {
		t.Error("旧 px 下限还挂在源码里：字号放大时格宽不变，墨迹画到邻列（issue #204）")
	}
	authoritative := shellRule(src, "#view-accounts table.acc { min-width")
	if !strings.Contains(authoritative, "em;") {
		t.Errorf("权威下限不是 em：%s（issue #204）", authoritative)
	}
	// 媒体查询里的降级下限同样要 em。
	if strings.Contains(src, "min-width: 1040px") || !strings.Contains(src, "min-width: 65em") {
		t.Error("窄屏降级下限没有换成 em（issue #204）")
	}

	// td.num 有裁剪兜底：字号再放大也不把墨迹画到邻列格子里。
	numRule := shellRule(src, "#view-accounts table.acc td.num {")
	if !strings.Contains(numRule, "overflow: hidden") || !strings.Contains(numRule, "text-overflow: ellipsis") {
		t.Errorf("td.num 缺少 overflow/ellipsis 裁剪：%s（墨迹会画到「用量」列上，issue #204）", numRule)
	}

	// 在途格带 title 全文：截断后读数不丢（#45 先例）。
	if !strings.Contains(src, `'<span title="' + esc(text) + '">'`) {
		t.Error("在途格没有 title 全文：截断后回到「看不到全文」的老问题（issue #204/#45）")
	}
}
