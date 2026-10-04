package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 一键重启，以及「这个键到底要不要重启」的如实报告
// ---------------------------------------------------------------------------

// A save that only touched hot keys must not ask for a restart.  The panel now
// offers a restart button for the keys that really need one, and crying wolf for
// a one-line alias edit is what made an operator ask "restart what, exactly?".
func TestConfigPatchSeparatesHotKeysFromRestartKeys(t *testing.T) {
	path := configFile(t, `{"aliases":{}}`)
	p := configPanel(path)

	hot := mustSave(t, p, `{"aliases":{"glm-5.3":"zcode/GLM-5.3"}}`)
	if hot["restart_required"] != false {
		t.Errorf("restart_required = %v for an aliases-only save, want false: the alias table is read per request",
			hot["restart_required"])
	}
	if hot["can_restart"] != false {
		t.Errorf("can_restart = %v with no restart hook wired, want false", hot["can_restart"])
	}

	// The cold case still says so, and the page can tell it has a button to offer.
	p.opts.Restart = func() error { return nil }
	cold := mustSave(t, p, `{"data_dir":"other"}`)
	if cold["restart_required"] != true {
		t.Errorf("restart_required = %v for a data_dir save, want true", cold["restart_required"])
	}
	if cold["can_restart"] != true {
		t.Errorf("can_restart = %v once the host wired a restart hook, want true", cold["can_restart"])
	}
}

func TestRestartRouteOnlyAnswersToPostAndOnlyWhenWired(t *testing.T) {
	p := configPanel(configFile(t, `{}`))

	w := httptest.NewRecorder()
	p.handleRestart(w, httptest.NewRequest(http.MethodGet, "/panel/api/restart", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /panel/api/restart = HTTP %d, want 405", w.Code)
	}

	// No hook: say so instead of reporting a restart that never happened.
	w = httptest.NewRecorder()
	p.handleRestart(w, httptest.NewRequest(http.MethodPost, "/panel/api/restart", nil))
	if w.Code != http.StatusNotImplemented {
		t.Errorf("POST /panel/api/restart without a hook = HTTP %d, want 501", w.Code)
	}

	calls := 0
	p.opts.Restart = func() error { calls++; return nil }
	w = httptest.NewRecorder()
	p.handleRestart(w, httptest.NewRequest(http.MethodPost, "/panel/api/restart", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("POST /panel/api/restart = HTTP %d (%s), want 200", w.Code, w.Body.String())
	}
	if calls != 1 {
		t.Errorf("the restart hook ran %d times, want exactly 1", calls)
	}

	// A hook that refuses (bad config, already restarting) has to surface its
	// reason: the operator is one click from having no idea what went wrong.
	p.opts.Restart = func() error { return errRestartRefused }
	w = httptest.NewRecorder()
	p.handleRestart(w, httptest.NewRequest(http.MethodPost, "/panel/api/restart", nil))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("POST with a failing hook = HTTP %d, want 500", w.Code)
	}
}

type restartRefusedError struct{}

func (restartRefusedError) Error() string { return "a restart is already in progress" }

var errRestartRefused = restartRefusedError{}

// The one-click restart has to actually be wired end to end: a button that only
// exists in the markup, or a POST to a route nobody serves, is worse than the
// "go restart it yourself" text it replaced.
func TestConfigPageOffersOneClickRestart(t *testing.T) {
	src := poolStatsUISource(t)
	if !strings.Contains(src, `id="btnCfgRestart"`) {
		t.Fatal("配置页没有「重启进程」按钮")
	}
	render := poolStatsFuncBody(t, src, "renderConfig")
	if !strings.Contains(render, "rbtn.hidden = !d.can_restart;") {
		t.Error("renderConfig 没有根据 can_restart 决定是否显示重启按钮：宿主没接钩子时它会一直只能报错")
	}
	if !strings.Contains(render, `rbtn.classList.toggle("primary", CFG_PENDING_RESTART.length > 0)`) {
		t.Error("有未生效改动时重启按钮没有点亮")
	}
	restart := poolStatsFuncBody(t, src, "restartProcess")
	if !strings.Contains(restart, `api("/panel/api/restart", { method: "POST" })`) {
		t.Error("restartProcess 没有 POST /panel/api/restart")
	}
	// 重启会切断连接，所以结果判定必须靠「新进程重新应答」，不能靠那一次 POST。
	wait := poolStatsFuncBody(t, src, "waitForRestart")
	if !strings.Contains(wait, `api("/panel/api/overview")`) {
		t.Error("waitForRestart 没有轮询到新进程应答")
	}
	if !strings.Contains(wait, "if (!r.ok) continue;") {
		t.Error("waitForRestart 把一次断线当成了重启失败")
	}
}
