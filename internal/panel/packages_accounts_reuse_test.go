package panel

import (
	"strings"
	"testing"
)

// 客户端页的重复下载回归（issue #93 + #146）。
//
// renderClients 第一行无条件 loadAllAccounts()——21 个模块各一个 /accounts
// 请求、合计 89 KB，与 overview（full_accounts=true）刚带回的那份逐字节相同。
// 修复沿用 refreshAll 的短路口径：OVER_FULL 为真时不再全量 fan-out。
//
// #146 的教训：那笔修复后来被 #107 又原样追加了一次（注释 + 那句短路逐字
// 两份），回退路径下每个模块的账号被读两轮——而本测试原先只断言
// strings.Contains（「出现过」），**拦不住这种重复**：两份时它照样是绿的。
// 所以现在的判据是**恰好一次**（按去掉注释后的代码数）。

func TestRenderClientsReusesTheOverviewAccounts(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderClients")

	if !strings.Contains(body, "if (!OVER_FULL) await loadAllAccounts();") {
		t.Fatal("renderClients 仍无条件 loadAllAccounts()：OVER_FULL 时把 overview 已带的账号再下载一遍（issue #93）")
	}
	// 恰好一次（issue #146）：注释里提它不算，只数代码行。重复的那次是两笔
	// 修复各自追加造成的——只断言「存在」等于给第三次开绿灯。
	code := stripLineComments(stripPanelComments(body))
	if n := strings.Count(code, "if (!OVER_FULL) await loadAllAccounts();"); n != 1 {
		t.Fatalf("renderClients 里 OVER_FULL 短路出现 %d 次，必须恰好 1 次："+
			"回退路径下每个模块的账号会被读 %d 轮（issue #146）", n, n)
	}
}
