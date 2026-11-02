package panel

import (
	"strings"
	"testing"
)

// 客户端页的重复下载回归（issue #93）。
//
// renderClients 第一行无条件 loadAllAccounts()——21 个模块各一个 /accounts
// 请求、合计 89 KB，与 overview（full_accounts=true）刚带回的那份逐字节相同。
// 修复沿用 refreshAll 的短路口径：OVER_FULL 为真时不再全量 fan-out。

func TestRenderClientsReusesTheOverviewAccounts(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderClients")

	if !strings.Contains(body, "if (!OVER_FULL) await loadAllAccounts();") {
		t.Fatal("renderClients 仍无条件 loadAllAccounts()：OVER_FULL 时把 overview 已带的账号再下载一遍（issue #93）")
	}
}
