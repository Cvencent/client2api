package panel

import (
	"strings"
	"testing"
)

// balance_ui_test.go pins the accounts page's balance column to the cached,
// paced contract behind it: opening the page must not ask every vendor at once.
//
// 背景：余额列原来每次打开账号页都对整个号池 fan-out 一次，账号一多就是一次
// 能触发风控的突发。现在服务端读缓存、分小批慢刷，界面这边只做两件事：读缓存
// （GET /balances），以及工具栏那个「刷新余额」按钮触发一批新的
// （POST /balances/refresh），然后分几次回读缓存把新数字补上。

func TestBalancesUIReadsTheCacheAndRefreshesInBatches(t *testing.T) {
	src := string(indexHTML)

	load := poolStatsFuncBody(t, src, "loadBalances")
	if !strings.Contains(load, "readBalances(n)") {
		t.Error("loadBalances 不再读服务端的余额缓存")
	}
	if !strings.Contains(load, "pollBalanceCache(n)") {
		t.Error("loadBalances 不再回读缓存，慢刷新后的数字不会显示")
	}
	if !strings.Contains(load, `api(cbase(n) + "/balances/refresh", { method: "POST" })`) {
		t.Error("工具栏的「刷新余额」没有走后端的分批刷新接口")
	}

	bulk := poolStatsFuncBody(t, src, "bulk")
	if !strings.Contains(bulk, "loadBalances(CUR, true)") {
		t.Error(`bulk("balance") 不再触发分批刷新`)
	}
	// 老实现是直接 loadBalances(CUR, true).then(...)，等一次全量结果回来才报数。
	if strings.Contains(bulk, "余额已刷新：") {
		t.Error(`bulk("balance") 还在等一次全量刷新返回`)
	}
}

func TestBalancesUIRendersUnknownCreditsAsDash(t *testing.T) {
	body := poolStatsFuncBody(t, string(indexHTML), "accBalanceCell")
	if !strings.Contains(body, "if (b.credits == null)") {
		t.Error("accBalanceCell 没有把「还没读到」和 0 区分开")
	}
	if !strings.Contains(body, `title="还没有读取过这个账号的余额"`) {
		t.Error("accBalanceCell 的「-」没有说明它为什么是空的")
	}
}

func TestBalancesUIMarksUnverifiedCreditsAsPending(t *testing.T) {
	body := poolStatsFuncBody(t, string(indexHTML), "accBalanceCell")
	for _, want := range []string{"b.unverified", "待确认", "厂商的两套额度数据互相矛盾"} {
		if !strings.Contains(body, want) {
			t.Errorf("accBalanceCell does not mark unverified credit: missing %q", want)
		}
	}
}

func TestBalancesUIPollsTheCacheWithoutCallingTheVendor(t *testing.T) {
	src := string(indexHTML)
	if !strings.Contains(src, "const BAL_SOFT_MS") {
		t.Error("index.html 丢了余额重新进入页面的软窗口")
	}
	ensure := poolStatsFuncBody(t, src, "ensureBalances")
	if !strings.Contains(ensure, "st.loaded && Date.now() - (st.loadedAt || 0) < BAL_SOFT_MS") {
		t.Error("重新进入账号页不再受软窗口节流，会连着要批次")
	}
	body := poolStatsFuncBody(t, src, "pollBalanceCache")
	if !strings.Contains(body, "readBalances(n)") {
		t.Error("pollBalanceCache 不再回读缓存")
	}
	// 回读走的是 GET /balances，绝不是逐账号的厂商查询。
	if strings.Contains(body, "/accounts/") {
		t.Error("轮询余额时打到了逐账号接口")
	}
	read := poolStatsFuncBody(t, src, "readBalances")
	if !strings.Contains(read, `api(cbase(n) + "/balances")`) {
		t.Error("readBalances 没有读余额缓存")
	}
	// GET 是缓存读取；这里出现写方法就说明回读变成了另一次刷新。
	if strings.Contains(read, "method: \"POST\"") {
		t.Error("readBalances 用了 POST，缓存回读不再是副作用读取")
	}
}
