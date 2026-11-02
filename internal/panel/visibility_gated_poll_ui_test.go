package panel

import (
	"strings"
	"testing"
)

// 轮询的后台标签门控回归（issue #95）。
//
// 面板的 9 处 setInterval 全部无条件运转：挂在后台标签页仍每 5 秒打
// /panel/api/status + /pool，一小时 ~1440 次没人看的 GET。修复：两个全局
// 轮询换用 setIntervalVisible 包装（hidden 时跳过 tick，恢复可见且已满一个
// 周期时补跑一次 refreshVisible）；登录/接码/录音等短生命周期的用户主动
// 轮询不在本单范围。

func TestGlobalPollsAreVisibilityGated(t *testing.T) {
	src := poolStatsUISource(t)

	if !strings.Contains(src, `setIntervalVisible(() => { if (VIEW === "logs") renderLogs(); }, 4000);`) {
		t.Fatal("日志页轮询没有换用可见性门控：后台标签仍每 4 秒下载 500 行（issue #95）")
	}
	if !strings.Contains(src, `setIntervalVisible(refreshVisible, 5000);`) {
		t.Fatal("状态轮询没有换用可见性门控：后台标签仍每 5 秒打 /status（issue #95）")
	}
	// 旧的无条件写法不许回来。
	if strings.Contains(src, `setInterval(() => { if (VIEW === "logs") renderLogs(); }, 4000);`) ||
		strings.Contains(src, "setInterval(refreshVisible, 5000);") {
		t.Error("全局轮询退回了无条件 setInterval（issue #95 回归）")
	}
	// 门控 helper 本体：hidden 跳过 + 补跑判据。
	helper := poolStatsFuncBody(t, src, "setIntervalVisible")
	if !strings.Contains(helper, "if (document.hidden) return;") {
		t.Fatal("setIntervalVisible 没有 hidden 早退：后台标签照常打接口（issue #95）")
	}
	if !strings.Contains(helper, "lastVisibleTick = Date.now()") {
		t.Fatal("setIntervalVisible 没有记录上次执行时间：补跑判据无从谈起（issue #95）")
	}
	// visibilitychange 的补跑：只在真正隔了一个周期时才跑。
	script := src[strings.Index(src, "document.addEventListener(\"visibilitychange\""):]
	script = script[:strings.Index(script, "(async function boot()")]
	if !strings.Contains(script, "if (document.hidden) return;") {
		t.Fatal("visibilitychange 没有忽略「进入后台」方向：hidden 时也会触发补跑（issue #95）")
	}
	if !strings.Contains(script, "Date.now() - lastVisibleTick >= 5000") {
		t.Fatal("visibilitychange 补跑没有周期阈值：切一次标签就会跑一轮刷新（issue #95）")
	}
}
