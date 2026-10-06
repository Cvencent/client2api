package panel

import (
	"strings"
	"testing"
)

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
