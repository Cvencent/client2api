package panel

import (
	"strings"
	"testing"
)

// 冷启动占位的分支顺序回归（issue #82）。
//
// renderAccounts 先判 !CUR（CUR 要等 /overview 回来才赋值，首帧必为空）并提前
// return，后面的 BOOTING 占位永远不可达——中间几秒页面用肯定语气说「没有已注册
// 的客户端」而实际有几十个账号。修复：先判 BOOTING；总览的「全部正常」与
// 「还没有接入账号」也只在数据落地后才说。

// TestBootingPlaceholderBeatsTheEmptyState 要求 renderAccounts 先判 BOOTING：
// 首帧的表格正文与卡片墙都是「正在加载账号…」，而不是「没有已注册的客户端」。
func TestBootingPlaceholderBeatsTheEmptyState(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderAccounts")

	bootIdx := strings.Index(body, "if (BOOTING) {")
	if bootIdx < 0 {
		t.Fatal("renderAccounts 没有 BOOTING 早退分支：冷启动会用肯定语气说「没有已注册的客户端」（issue #82）")
	}
	curIdx := strings.Index(body, `if (!CUR) { tb.innerHTML`)
	if curIdx < 0 {
		t.Fatal("renderAccounts 丢了 !CUR 空态分支")
	}
	if curIdx < bootIdx {
		t.Fatal("!CUR 分支排在 BOOTING 之前：首帧 CUR 必为空，占位永远不可达（issue #82 回归）")
	}
	if !strings.Contains(body[:curIdx], "正在加载账号…") {
		t.Fatal("BOOTING 分支没有写「正在加载账号…」占位")
	}
	// 统计格在 BOOTING 下是「-」，不是 0。
	stats := poolStatsFuncBody(t, src, "renderStats")
	if !strings.Contains(stats, "if (BOOTING) {") || !strings.Contains(stats, `el.textContent = "-"`) {
		t.Fatal("renderStats 在冷启动仍写 0：0 是「一个账号都没有」的肯定结论，首帧什么都还不知道（issue #82）")
	}
}

// 总览侧同一套口吻：「全部正常」只在数据落地后说；读取中给中性占位。
func TestOverviewKeepsConfidentVerdictsUntilDataLands(t *testing.T) {
	src := poolStatsUISource(t)
	ov := poolStatsFuncBody(t, src, "renderOverview")

	if !strings.Contains(ov, `bits.push(BOOTING`) {
		t.Fatal("总览的「全部正常」没有按 BOOTING 门控：读取中会挂着绿色肯定结论（issue #82）")
	}
	if !strings.Contains(ov, `BOOTING ? '<div class="ov-empty">正在加载账号…</div>'`) {
		t.Fatal("总览的「还没有接入账号」没有按 BOOTING 门控：冷启动会写「还没有接入账号」而实际有几十个（issue #82）")
	}
}
