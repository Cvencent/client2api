package panel

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 账号表「成功 / 失败」列的几何回归（issue #87）。
//
// col-rate 5% 装不下 nowrap 的「成功 / 失败」标签：sticky 表头按 DOM 序后列
// 压前列，溢出的内容与排序箭头被「在途」列头整块盖住——1440px 及以下的常见
// 窗口看不到该列能排序，点击箭头位置实际排序的是邻列。修复：列宽 5%→6%
//（「在途」4%→4.5%），可排序列头加 overflow 收拢兜底。

func TestRateColumnFitsItsHeader(t *testing.T) {
	src := poolStatsUISource(t)

	// 这里原来逐条钉字面量（`width: 6%` / `4.5%`），#97 换上几何门禁后暴露出
	// 它挡不住真正的缺陷：那条规则在，而 1280px 下这一列只有 79px、表头需要
	// 99px，箭头照样被邻列盖住——**测试绿而缺陷在**，正是本单要消掉的那种
	// 「钉字面量而非行为」。现在结构性要求交给真正的几何断言
	// （cmd/panelsmoke 的 assertLayout 实测 scrollWidth/clientWidth 与
	// elementFromPoint），这里只留两条便宜的结构检查：
	//   1. rate 列必须比 prio 列宽（前者装 5 个汉字 + 箭头，后者只有 3 个汉字）；
	//   2. 窄窗必须让表横滚，而不是把表头裁掉。
	rate := colWidthPct(t, src, "rate")
	prio := colWidthPct(t, src, "prio")
	if rate <= prio {
		t.Errorf("col-rate (%v%%) 不大于 col-prio (%v%%)：装 5 个汉字加箭头的列比 3 个汉字的列还窄（issue #87）", rate, prio)
	}
	if !strings.Contains(src, "#view-accounts table.acc { min-width: 1290px; }") {
		t.Fatal("窄窗没有让账号表横滚：百分比放不下 nowrap 中文表头时会被无声裁掉（issue #87/#97）")
	}
	// 可排序列头的收拢兜底：列宽吃紧时标签收缩而不是溢出到邻列。
	if !strings.Contains(src, ".acc thead th.sortable { max-width: 100%; overflow: hidden; }") {
		t.Fatal("可排序列头没有收拢兜底：窄窗口下溢出内容仍会被 sticky 邻列盖住（issue #87）")
	}
}

// colWidthPct reads one declared account-column width.
func colWidthPct(t *testing.T, src, col string) float64 {
	t.Helper()
	re := regexp.MustCompile(`#view-accounts table\.acc \.col-` + regexp.QuoteMeta(col) + ` \{ width: ([0-9.]+)%; \}`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("no width rule for col-%s", col)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		t.Fatalf("parse col-%s width %q: %v", col, m[1], err)
	}
	return v
}
