package panel

import (
	"strings"
	"testing"
)

// TestAutoReloginDialogHidesAddAccountControls pins the simplified WorkBuddy
// re-login flow.  Re-login is already one pinned account and one SMS platform
// token; showing the add-account batch knobs and manual number controls made
// the operator configure things that the restore path never reads.
func TestAutoReloginDialogHidesAddAccountControls(t *testing.T) {
	src := poolStatsUISource(t)
	for _, want := range []string{
		`id="addClientField"`,
		`id="autoBatchRow"`,
		`id="smsPick"`,
		`id="smsPick2"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("重登弹窗缺少可隐藏的控件标记：%s", want)
		}
	}

	sync := poolStatsFuncBody(t, src, "syncAddMode")
	for _, want := range []string{
		`const restore = ADD.mode === "restore";`,
		`$("#addTabs").hidden = restore;`,
		`$("#addClientField").hidden = restore;`,
		`$("#autoBatchRow").hidden = restore;`,
		`$("#smsPick").hidden = restore;`,
		`$("#smsPick2").hidden = restore || autoOn(n);`,
		`$("#smsHint").hidden = restore && autoOn(n);`,
		`$("#btnAutoStart").textContent = restore ? "一键重新登录" : "一键自动添加";`,
	} {
		if !strings.Contains(sync, want) {
			t.Errorf("重登模式没有隐藏添加账号专用控件：%s", want)
		}
	}

	loginButton := poolStatsFuncBody(t, src, "syncAddLoginButton")
	if !strings.Contains(loginButton, `const autoRestore = ADD.mode === "restore" && autoOn($("#addClient").value);`) ||
		!strings.Contains(loginButton, `$("#btnStartLogin").hidden = autoRestore || !(ADD.tab === "login" && loginOn());`) {
		t.Error("自动重登仍会显示「获取授权链接」，让操作员误以为还要手动走一遍")
	}

	for _, fn := range []string{"openAdd", "openReloginTarget"} {
		if body := poolStatsFuncBody(t, src, fn); !strings.Contains(body, "syncAddMode();") {
			t.Errorf("%s 没有在打开时同步弹窗模式", fn)
		}
	}
}
