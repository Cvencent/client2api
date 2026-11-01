package panel

import (
	"strings"
	"testing"
)

// 运行日志页的粘滞错误回归（issue #84）。
//
// 一次 /panel/api/logs 失败后，页面写下「读取失败」；后端恢复后请求照发照
// 200，但去重早退发生在渲染之前——日志没新增行时 total 不变、stamp 与失败前
// 相同，成功响应被整段丢掉，错误态永远出不去（只能靠切频道或等网关恰好写新
// 行）。修复：把上一轮成败编进去重戳（err 状态戳 + "ok:" 前缀），失败与成功
// 永远不会互相去重。

// TestLogsErrorStateExitsThroughTheDedupeStamp 要求失败分支把 LOGSEEN 置为
// err 戳、成功戳带 "ok:" 前缀——两者永不相等，恢复后的第一次成功必然重画。
func TestLogsErrorStateExitsThroughTheDedupeStamp(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderLogs")

	if !strings.Contains(body, `LOGSEEN = "err"`) {
		t.Fatal("renderLogs 的失败分支没有把 LOGSEEN 置为 err 戳：成功响应会被「内容没变」早退吞掉，错误态粘滞（issue #84）")
	}
	if !strings.Contains(body, `"ok:" + String(d.total || 0)`) {
		t.Fatal("成功戳没有带 ok 前缀：错误态与成功态可能互相去重（issue #84）")
	}
	// 旧的裸戳（无 ok 前缀）不许回来。
	if strings.Contains(body, `stamp = String(d.total || 0) + "|" + LOGCH`) {
		t.Error("renderLogs 退回了无成败标记的旧去重戳（issue #84 回归）")
	}
}

// 配置页/平台页的「读失败后保存」（issue #83）。
//
// /panel/api/config 读取失败时页面上一个可编辑控件都没有，但保存按钮仍可点：
// 配置页点下去 null CFG 走进 Object.keys 抛未捕获 TypeError，被 boot 红条原样
// 钉在页面底部且不可关；平台页点下去弹「已保存，内容没有变化」的假成功，实际
// 一个非 GET 请求都没发。

// TestSaveButtonsGuardAgainstANeverReadConfig 钉住三处守卫：配置页读失败分支
// 禁用保存/重启按钮、两个 save 入口对「从未读到」各有一条明确报错的守卫、
// boot 红条可关闭且堆栈只进 console。
func TestSaveButtonsGuardAgainstANeverReadConfig(t *testing.T) {
	src := poolStatsUISource(t)

	cfg := poolStatsFuncBody(t, src, "renderConfig")
	if !strings.Contains(cfg, `$("#btnCfgSave").disabled = true;`) ||
		!strings.Contains(cfg, `$("#btnCfgRestart").disabled = true;`) {
		t.Fatal("renderConfig 的失败分支没有禁用保存/重启按钮：没读到配置时保存会拿 null CFG 去崩（issue #83）")
	}

	saveCfg := poolStatsFuncBody(t, src, "saveConfig")
	if !strings.Contains(saveCfg, `if (!CFG) { toast(`) {
		t.Fatal("saveConfig 没有 null CFG 守卫：配置没读到时点保存会抛未捕获 TypeError（issue #83）")
	}

	savePf := poolStatsFuncBody(t, src, "savePlatforms")
	if !strings.Contains(savePf, `if (!PF) { toast("平台配置还没读取成功`) {
		t.Fatal("savePlatforms 没有 !PF 守卫：读失败后保存会落到「已保存，内容没有变化」的假成功（issue #83）")
	}

	boot := poolStatsFuncBody(t, src, "panelBootError")
	for _, want := range []string{`close.textContent = "知道了"`, `console.error(full)`} {
		if !strings.Contains(boot, want) {
			t.Errorf("boot 红条缺少 %q：不可关的固定红条 + 原样堆栈会把操作员钉在页面上（issue #83）", want)
		}
	}
}
