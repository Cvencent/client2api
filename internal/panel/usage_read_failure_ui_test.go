package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 用量页读失败必须清掉「所有承载数字的地方」（issue #156）。
//
// 失败分支原先只做三件事：写 #usNote、把图换成错误块、清磁贴收读数条——
// 四张明细表、#usStats、最近调用子表与「N / N 条」全留上一帧，而唯一的
// 「获取失败」提示挂在「总览」子页的 header 里，一换子页就不可见。
// 判据是「失败时任何一格都不许继续显示上一帧的数字」，照抄同一函数已经
// 做对的那三行的口径。行为判据（真浏览器 + fetch 注入）在
// cmd/panelsmoke 的 assertUsageReadFailure；这里钉会静默漂移的标记。
// ---------------------------------------------------------------------------

// TestUsageErrorBranchClearsEveryNumberSurface 钉失败分支要处理的每一个
// 承载面：读数带、四张明细表、最近调用子表、各子页的 note、以及错误位本身。
func TestUsageErrorBranchClearsEveryNumberSurface(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderUsage")

	// 错误文案本体不钉死（含中文），钉的是「写进 #usNote 的同时写进两个子页
	// 错误位」——判据是三个承载位拿到同一句 msg（issue #156）。
	if !strings.Contains(body, `$("#usNote").textContent = msg`) {
		t.Fatal("renderUsage 的失败分支不见了：错误提示没有任何承载位（issue #156）")
	}
	for _, id := range []string{
		"usStats",      // 「请求 / 失败」读数带
		"usClientBody", // 四张明细表
		"usAccBody", "usModelBody", "usRealmBody",
		"usRecentBody", // 最近调用子表
		"usRecentNote", // 「N / N 条」
		"usClientNote", "usAccNote", "usModelNote", "usRealmNote",
		"usStatsErr", "usRecentErr", // 两个子页自己的错误位
	} {
		if !strings.Contains(body, id) {
			t.Errorf("失败分支没有处理 %s：读失败时它仍显示上一帧的数字（issue #156）", id)
		}
	}
	// 错误位必须出现在「各自子页的 header」里，否则换子页提示又不可见
	// ——这正是 #156 报的「#usNote 挂在别人子页里」的形状。
	for _, pair := range [][2]string{
		{"usRecentErr", "usRecentNote"},
		{"usStatsErr", "usClientNote"},
	} {
		if !strings.Contains(src, `id="`+pair[0]+`" hidden`) {
			t.Errorf("错误位 %s 没有 hidden 起始态：成功路径会显示一个空错误（issue #156）", pair[0])
		}
		if i, j := strings.Index(src, `id="`+pair[0]+`"`), strings.Index(src, `id="`+pair[1]+`"`); i < 0 || j < 0 {
			t.Errorf("错误位 %s / note %s 不在同一子页 header（issue #156）", pair[0], pair[1])
		}
	}
}

// TestUsageErrorBranchResetsRecentCache 钉住内存侧：US_RECENT（最近调用子表
// 的数据源，renderRecentRows 从它取行数）在失败时也必须清掉——否则下一次
// 任何触发 renderRecentRows 的路径会把旧明细再画回来。
func TestUsageErrorBranchResetsRecentCache(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderUsage")

	if !strings.Contains(body, "US_RECENT = []") {
		t.Error("失败分支没有清 US_RECENT：任何后续 renderRecentRows 会把上一帧的明细再画回来（issue #156）")
	}
	// 成功路径必须收起两个子页的错误位：失败态不能黏住（恢复后还是错误样式）。
	if !strings.Contains(body, "usStatsErr") || !strings.Contains(body, "usRecentErr") {
		t.Error("成功路径没有收起子页错误位：失败态会黏住（issue #156）")
	}
}
