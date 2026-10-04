package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 轮询行为的回归测试：这五处都不是文案问题，而是「每 5 秒折腾一次」的放大
// 器 —— 一个多余的往返、一句重复的 toast、一次整表重画，都会在长时间开着
// 页面的操作员那里变成噪音。Go 编译看不见 index.html，所以在这里静态钉住：
//   1. /overview 带了 full_accounts 时，轮询跳过逐平台的 /accounts；
//   2. 静默轮询读失败只记在行上，不弹（会重复的）toast；
//   3. 保存配置按热加载 / 需重启分两档说，只有热键才 POST /reload；
//   4. toast 容器是 live region，错误还额外带 role=alert；
//   5. 日志累计计数没变时整段跳过重画；
//   6. 首帧空表画「正在加载」而不是「还没有账号」。
// ---------------------------------------------------------------------------

func TestAccountPollSkipsPerClientRoundTripsWhenOverviewIsComplete(t *testing.T) {
	src := poolStatsUISource(t)

	// overview 分支必须按 full_accounts 记下「这份响应就是完整答案」。
	if !strings.Contains(src, "OVER_FULL = d.full_accounts === true;") {
		t.Error("loadStatus 没有把 full_accounts 记进 OVER_FULL：轮询会重新变成逐平台重拉")
	}
	// 老服务没有这个字段，回退路径必须显式关掉，否则会拿一次 /status 当成完整答案。
	if !strings.Contains(src, "OVER_FULL = false;") {
		t.Error("loadStatus 的回退分支没有把 OVER_FULL 关掉")
	}
	refresh := poolStatsFuncBody(t, src, "refreshAll")
	if !strings.Contains(refresh, "if (!OVER_FULL || loud) await loadAllAccounts(!loud);") {
		t.Error("refreshAll 没有在 overview 完整时跳过 loadAllAccounts：每个轮询仍会多打 14 次 /accounts")
	}
}

func TestQuietPollRecordsReadFailureInsteadOfToasting(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, "async function loadAccounts(n, quiet)") {
		t.Fatal("loadAccounts 没有 quiet 参数：轮询无法静默")
	}
	body := poolStatsFuncBody(t, src, "loadAccounts")
	// 静默分支必须写回 error 字段，而不是无声地丢掉失败。
	if !strings.Contains(body, "if (quiet)") {
		t.Error("loadAccounts 没有 quiet 分支：读失败仍会每 5 秒弹一次 toast")
	}
	if !strings.Contains(body, "error: r.err || (\"HTTP \" + r.status)") {
		t.Error("loadAccounts 的静默分支没有把失败原因记进记录的 error 字段：操作员看不到为什么空着")
	}
	if !strings.Contains(body, `toast("读取 " + n + " 账号失败：`) {
		t.Error("loadAccounts 的手动刷新分支不再弹 toast：静默和显式刷新变得无法区分")
	}
	if !strings.Contains(src, `if (rec.error) accText += " · 读取失败：" + rec.error;`) {
		t.Error("renderAccounts 没有把静默轮询记下的 error 画出来：静默变成了消失")
	}
	// 轮询入口要把 quiet 一路传下去。
	if !strings.Contains(src, "await Promise.all(NAMES.map(n => loadAccounts(n, quiet)));") {
		t.Error("loadAllAccounts 没有把 quiet 传给 loadAccounts")
	}
}

func TestSaveConfigSplitsHotAndColdKeys(t *testing.T) {
	src := poolStatsUISource(t)

	if n := strings.Count(src, "const CFG_RESTART_KEYS ="); n != 1 {
		t.Fatalf("CFG_RESTART_KEYS 定义了 %d 次，想要恰好一次", n)
	}
	save := poolStatsFuncBody(t, src, "saveConfig")
	if !strings.Contains(save, "const hot = changed.filter(k => !CFG_RESTART_KEYS[k]);") ||
		!strings.Contains(save, "const cold = changed.filter(k => CFG_RESTART_KEYS[k]);") {
		t.Error("saveConfig 没有把改动按「热加载 / 需重启」分开")
	}
	// 只有热键才值得 POST reload；纯冷改动去 reload 只会白写日志。
	if !strings.Contains(save, "if (hot.length) {") {
		t.Error("saveConfig 无条件 POST /reload：一次纯需重启的保存也会说「已生效」")
	}
	if !strings.Contains(save, `bits.push("需重启进程：" + cold.join("、") + "（点右上角「重启进程」立即生效）")`) {
		t.Error("saveConfig 不再把需重启的键单独列出来，或没告诉操作员下一步点哪里")
	}
	// 渲染时的默认说明要按实际规则说：哪些键热加载、哪些键要重启进程。
	render := poolStatsFuncBody(t, src, "renderConfig")
	if !strings.Contains(render, "需要重启进程。") {
		t.Error("renderConfig 的说明没有指出哪些键需要重启进程")
	}
	if !strings.Contains(render, "保存后立即生效") {
		t.Error("renderConfig 的说明没有指出哪些键保存后立即生效")
	}
}

func TestToastsAreALiveRegion(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, `<div id="toasts" role="status" aria-live="polite"`) {
		t.Error("toast 容器不是 live region：读屏用户听不到任何提示")
	}
	body := poolStatsFuncBody(t, src, "toast")
	if !strings.Contains(body, `if (kind === "err") el.setAttribute("role", "alert");`) {
		t.Error("错误 toast 没有升级成 role=alert：读屏不会打断着念")
	}
}

func TestLogViewSkipsRedrawWhenNothingChanged(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, "let LOGSEEN = null;") {
		t.Fatal("没有 LOGSEEN 记账，日志页每 4 秒整段重画 500 行")
	}
	body := poolStatsFuncBody(t, src, "renderLogs")
	if !strings.Contains(body, `const stamp = String(d.total || 0) + "|" + LOGCH;`) {
		t.Error("renderLogs 没有把「累计行数 | 频道」当重画判据")
	}
	if !strings.Contains(body, "if (stamp === LOGSEEN) return;") {
		t.Error("renderLogs 在计数没变时仍会重画")
	}
}

func TestFirstPaintShowsLoadingNotEmpty(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, "let OVER_FULL = false, BOOTING = true;") {
		t.Fatal("没有 BOOTING 标志：首帧会先闪一句「还没有账号」")
	}
	render := poolStatsFuncBody(t, src, "renderAccounts")
	if !strings.Contains(render, `(rec.unsupported ? empty : (BOOTING ? "正在加载账号…" : empty))`) {
		t.Error("账号表空态没有区分「首帧加载中」与「真的没有账号」")
	}
	refresh := poolStatsFuncBody(t, src, "refreshAll")
	if !strings.Contains(refresh, "await loadStatus();") || !strings.Contains(refresh, "BOOTING = false;") {
		t.Error("refreshAll 没有在第一次往返结束后翻掉 BOOTING")
	}
}
