package panel

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 任务看板的「缓存 + 绿色完成态 + 手动刷新」三件事的回归。
//
// 后端把每个账号最近一次任务状态写进 data/panel/task_board_cache.json，老账号
// 直接吃缓存、只有新账号才去问厂商；前端据此把"全部自动化任务已做完"的账号
// 卡片染绿，并给一个手动「刷新任务状态」按钮强制走 refresh=1 绕过缓存。
//
// 这些全是字符串拼接，Go 编译不管它们，所以这里静态钉住四件事：
//   1. 平台层的 taskBoardTasks 在非 force 时先读缓存、force 时打上游；
//   2. 「刷新任务状态」按钮存在，并把 renderTaskBoard(true) 接上；
//   3. 账号矩阵的 allDone 判定与 done 类名都在；
//   4. 强制刷新链路（renderTaskBoard / waitForTask / tbSchedule）都带 refresh=1，
//      否则轮询会把刚执行完的状态又读回旧缓存。
// ---------------------------------------------------------------------------

func TestTaskBoardShowsTheRefreshTasksButton(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, `id="btnTbRefreshTasks"`) {
		t.Fatal("任务看板没有「刷新任务状态」按钮 #btnTbRefreshTasks")
	}
	if !strings.Contains(src, `$("#btnTbRefreshTasks").addEventListener("click", tbRefreshTasks)`) {
		t.Fatal("「刷新任务状态」按钮没有绑定 tbRefreshTasks")
	}
}

func TestTaskBoardMarksFullyDoneAccountsGreen(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "tbAccountMatrix")

	if !strings.Contains(body, "const allDone = pending === 0 && !g.error && tasks.length > 0;") {
		t.Fatalf("tbAccountMatrix 的完成判定不对；只剩人工/未解锁行也算完成：\n%s", body)
	}
	want := `'<div class="tb-acct' + (allDone ? " done" : "")`
	if !strings.Contains(body, want) {
		t.Fatalf("完成的账号卡片没有加上 done 类名：\n%s", body)
	}
	if !strings.Contains(src, ".tb-acct.done {") {
		t.Fatal("index.html 没有 .tb-acct.done 的绿色样式")
	}
}

func TestTaskBoardRefreshTasksUsesForcedRefetch(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "tbRefreshTasks")

	if !strings.Contains(body, "renderTaskBoard(true)") {
		t.Fatalf("tbRefreshTasks 没有强制刷新 renderTaskBoard(true)：\n%s", body)
	}
	if !strings.Contains(body, "刷新中") {
		t.Fatalf("tbRefreshTasks 没有把按钮切到「刷新中」：\n%s", body)
	}
	if !strings.Contains(body, "#btnTbRefreshTasks") {
		t.Fatalf("tbRefreshTasks 没有操作 #btnTbRefreshTasks：\n%s", body)
	}
}

func TestTaskBoardRenderPushesRefreshParamWhenForced(t *testing.T) {
	src := poolStatsUISource(t)
	body := poolStatsFuncBody(t, src, "renderTaskBoard")

	if !strings.Contains(body, `params.push("refresh=1")`) {
		t.Fatalf("renderTaskBoard 在 force 时没有追加 refresh=1：\n%s", body)
	}
	if !strings.Contains(body, "if (force) params.push") {
		t.Fatalf("refresh=1 没有受 force 控制，普通轮询也会强制打上游：\n%s", body)
	}
}

func TestTaskBoardPollingAlwaysBypassesCache(t *testing.T) {
	src := poolStatsUISource(t)

	waitBody := poolStatsFuncBody(t, src, "waitForTask")
	if !strings.Contains(waitBody, `"refresh=1"`) {
		t.Fatalf("waitForTask 轮询没有带 refresh=1，执行完仍会看到旧缓存：\n%s", waitBody)
	}

	schedBody := poolStatsFuncBody(t, src, "tbSchedule")
	if !strings.Contains(schedBody, "renderTaskBoard(true).catch") {
		t.Fatalf("tbSchedule 有运行中的任务时没有强制刷新：\n%s", schedBody)
	}
}
