package panel

import (
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

	if !strings.Contains(src, "#view-accounts table.acc .col-rate { width: 6%; }") {
		t.Fatal("col-rate 仍是 5%：装不下 nowrap 的「成功 / 失败」标签，箭头被邻列盖住（issue #87）")
	}
	if !strings.Contains(src, "#view-accounts table.acc .col-inflight { width: 4.5%; }") {
		t.Fatal("col-inflight 没有同步到 4.5%：加宽 rate 列的代价无处可付（issue #87）")
	}
	if strings.Contains(src, "#view-accounts table.acc .col-rate { width: 5%; }") {
		t.Error("col-rate 退回了 5%（issue #87 回归）")
	}
	// 可排序列头的收拢兜底：列宽吃紧时标签收缩而不是溢出到邻列。
	if !strings.Contains(src, ".acc thead th.sortable { max-width: 100%; overflow: hidden; }") {
		t.Fatal("可排序列头没有收拢兜底：窄窗口下溢出内容仍会被 sticky 邻列盖住（issue #87）")
	}
}
