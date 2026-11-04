package panel

import (
	"strings"
	"testing"
)

// 钉住「可排序列头必须跟着表头一起粘住」（issue #170）。
//
// 粘性表头靠 .tbl-wrap > table > thead th 的 position:sticky；两条后置规则
// （.acc thead th.sortable 与 #view-models .acc.md thead th.sortable）为画
// 下缘刻度线曾把它改写成 position:relative——权重更高，于是带 .sortable 的
// 列头全部不再粘：账号池 13 列滚下去 12 列表头消失，模型页丢「上下文」列。
// sticky 元素本身就是绝对定位的包含块，::after 刻度线不需要 relative。
func TestSortableColumnHeadsStaySticky(t *testing.T) {
	src := string(indexHTML)

	base := shellRule(src, ".tbl-wrap > table > thead th {")
	if !strings.Contains(base, "position: sticky") {
		t.Fatalf(".tbl-wrap 粘性表头规则丢了 sticky：%s", base)
	}

	for _, prefix := range []string{
		".acc thead th.sortable {",
		"#view-models .acc.md thead th.sortable {",
	} {
		// 同名选择器可能有多条规则（如 1541 行的 max-width 剪裁），逐条扫：
		// 任何一条敢写 relative 都会以更高权重顶掉 sticky。
		for at, rule := -1, ""; ; {
			i := strings.Index(src[at+1:], prefix)
			if i < 0 {
				break
			}
			at = at + 1 + i
			rule = src[at : at+strings.Index(src[at:], "}")+1]
			if strings.Contains(rule, "position: relative") {
				t.Errorf("%s 又把 position 写回 relative：可排序列头会滚出视野（issue #170）：%s", prefix, rule)
			}
		}
		// 覆盖者本体必须显式 sticky：它是后置规则，不写就是让 sticky 靠
		// 上游规则「恰好活着」，下次谁在前置块里改一行就又断了。
		if !strings.Contains(shellRule(src, prefix), "position: sticky") &&
			prefix == ".acc thead th.sortable {" && !strings.Contains(src, prefix+" ") {
			t.Errorf("%s 丢了 position: sticky：粘性表头对可排序列失效（issue #170）", prefix)
		}
	}
	// 两条覆盖者规则的正文里都必须有 sticky（同名首条规则不算数）。
	if !strings.Contains(src, ".acc thead th.sortable { position: sticky") {
		t.Error(".acc thead th.sortable 丢了 position: sticky：粘性表头对可排序列失效（issue #170）")
	}
	if !strings.Contains(src, "#view-models .acc.md thead th.sortable { position: sticky") {
		t.Error("#view-models .acc.md thead th.sortable 丢了 position: sticky（issue #170）")
	}

	// 刻度线的两个 ::after 仍然存在：sticky 替换不许把它们弄丢。
	for _, prefix := range []string{
		".acc thead th.sortable::after {",
		"#view-models .acc.md thead th.sortable::after {",
	} {
		if strings.Index(src, prefix) < 0 {
			t.Errorf("找不到 %s 规则：下缘刻度线丢了", prefix)
		}
	}
}
