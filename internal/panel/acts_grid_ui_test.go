package panel

import (
	"strings"
	"testing"
)

// 操作列网格的密度回归（issue #88）。
//
// #44 把操作列的宽度还回来之后（1440px 下约 300px），写死的两列网格把每颗
// 按钮拉到 144px、行高撑到 137~198px——一屏只能看全 1 行，而同行其余内容只
// 需要 51~60px。修复：列数按内容定（auto-fit，96px 一格下限），窄屏兜底同
// 口径（84px）；.test-split 长标签仍整行豁免。

func TestActsGridFollowsTheColumnWidth(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, "grid-template-columns: repeat(auto-fit, minmax(96px, 1fr));") {
		t.Fatal("操作列网格仍是写死列数：#44 还宽后按钮被拉到 144px、行高 137~198px，一屏 1 行（issue #88）")
	}
	if !strings.Contains(src, "repeat(auto-fit, minmax(84px, 1fr))") {
		t.Fatal("窄屏兜底没有跟随 auto-fit 口径（issue #88）")
	}
	if strings.Contains(src, "grid-template-columns: repeat(2, minmax(46px, 1fr))") ||
		strings.Contains(src, "grid-template-columns: repeat(2, minmax(54px, 1fr))") {
		t.Error("写死两列的旧网格回来了（issue #88 回归）")
	}
	// 长标签整行豁免必须保留：auto-fit 的 96px 格子装不下「领取每日签到积分」。
	if !strings.Contains(src, ".c-acts .actwrap > .test-split { grid-column: 1 / -1; }") {
		t.Fatal(".test-split 的整行豁免丢了：长标签会被 96px 格子截断（issue #88）")
	}
}
