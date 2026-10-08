package panel

import (
	"strings"
	"testing"
)

// TestAutoAddBatchAvoidsNumbersItAlreadyTried pins the fix for a batch looping
// on one number: every phone the module reports joins this run's avoid list, the
// next start sends that list, and it is only ever built for a real batch -- a
// re-login pins the account's own number and must not avoid it.
func TestAutoAddBatchAvoidsNumbersItAlreadyTried(t *testing.T) {
	src := poolStatsUISource(t)

	start := poolStatsFuncBody(t, src, "autoStart")
	if !strings.Contains(start, "seen: []") {
		t.Error("autoStart 新建批次时没有清空避让表（seen），上一轮的号码会渗进来")
	}

	job := poolStatsFuncBody(t, src, "renderAutoJob")
	if !strings.Contains(job, "ADD.autoBatch.seen.push(job.phone)") {
		t.Error("renderAutoJob 没有把本轮的号码记进避让表，平台会把同一个号反复发回来")
	}

	body := poolStatsFuncBody(t, src, "autoBody")
	if !strings.Contains(body, "b.avoid = ADD.autoBatch.seen.slice()") {
		t.Error("autoBody 没有把本轮已试号码作为 avoid 发给模块")
	}
	// avoid 只能在普通添加时发；重登用 phone 钉住号码，二者互斥。
	if !strings.Contains(body, `if (ADD.mode === "restore" && ADD.restore) b.phone = ADD.restore.phone;`) ||
		!strings.Contains(body, "else if (ADD.autoBatch && ADD.autoBatch.seen.length)") {
		t.Error("autoBody 的 avoid 分支不是重登的 else，重登会被自己的号码挡掉")
	}
}

// TestAutoAddDialogStaysOpenWhileRunning pins the accidental-backdrop-close
// regression: the auto-login job runs on the server, but the panel's progress
// view lives in this dialog.  Closing the veil while the job is in flight used
// to drop that view and orphan the job id from the operator's perspective.
func TestAutoAddDialogStaysOpenWhileRunning(t *testing.T) {
	src := poolStatsUISource(t)

	closeAdd := poolStatsFuncBody(t, src, "closeAdd")
	for _, want := range []string{
		"if (autoBusy()) {",
		`toast("自动添加运行中，请先点「停止」再关闭", "err")`,
		"return false;",
	} {
		if !strings.Contains(closeAdd, want) {
			t.Errorf("closeAdd 在自动任务运行期间仍会关闭弹层：缺少 %q", want)
		}
	}

	lock := poolStatsFuncBody(t, src, "setAutoBusy")
	for _, want := range []string{
		"ADD.autoBusy = !!on;",
		"close.disabled = !!on;",
		`close.textContent = on ? "运行中…" : "关闭";`,
		"picker.disabled = !!on;",
		`tabs.querySelectorAll("button").forEach(b => { b.disabled = !!on; });`,
	} {
		if !strings.Contains(lock, want) {
			t.Errorf("自动任务的运行态没有锁住会丢失进度的控件：缺少 %q", want)
		}
	}

	if start := poolStatsFuncBody(t, src, "autoStart"); !strings.Contains(start, "setAutoBusy(true);") {
		t.Error("autoStart 没有进入运行态，关闭保护不会生效")
	}
	if finish := poolStatsFuncBody(t, src, "autoBatchFinish"); !strings.Contains(finish, "setAutoBusy(false);") {
		t.Error("autoBatchFinish 没有退出运行态，关闭按钮会一直禁用")
	}
}
