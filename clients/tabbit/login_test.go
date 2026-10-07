package tabbit

// Offline tests for the browser-login hand-off (login.go).  Nothing here needs
// a browser, Node.js, Playwright or a real sidecar: the transport is a fake
// RoundTripper and the "operator" is the test itself advancing the state.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

const wantsLoginURL = "https://web.tabbit.com/login?callback=close&flow=history_opt_in&theme=mn"

// downTransport refuses every connection, like a machine where tabbit2api has
// never been started.
func downTransport() roundTripFunc {
	return func(*http.Request) (*http.Response, error) {
		return nil, errors.New("dial tcp 127.0.0.1:50124: connectex: No connection could be made because the target machine actively refused it")
	}
}

// sidecarTransport answers /health and /v1/models while up is true.  A closed
// port and an empty catalogue are different failures on purpose: the first is
// "not running", the second is "running but not signed in".
func sidecarTransport(up *atomic.Bool, healthBody, modelsBody string) roundTripFunc {
	return func(r *http.Request) (*http.Response, error) {
		if !up.Load() {
			return nil, errors.New("dial tcp 127.0.0.1:50124: connectex: No connection could be made")
		}
		switch r.URL.Path {
		case "/health":
			return jsonResponse(http.StatusOK, healthBody), nil
		case "/v1/models":
			return jsonResponse(http.StatusOK, modelsBody), nil
		}
		return jsonResponse(http.StatusNotFound, `{"error":"not found"}`), nil
	}
}

const (
	healthyHealth = `{"status":"ok","version":"0.1.9","models":9,"web_host":"web.tabbit.com"}`
	twoModels     = `{"object":"list","data":[{"id":"tabbit/priority"},{"id":"tabbit/GPT-5.5"}]}`
)

func loginClient(t *testing.T, cfg string, tp roundTripFunc) *Client {
	t.Helper()
	clearTabbitEnv(t)
	// 启动器默认指向一个不存在的路径：这套测试必须在任何一台机器上都不去
	// 真启动 Tabbit 浏览器。交接那条路自己指定一个存在的桩文件。
	missingTabbitCLI(t)
	withCandidates(t, closedPortURL(t))
	return newTestClient(t, cfg, &http.Client{Transport: tp})
}

// missingTabbitCLI 把 tabbit_cli 指到一个不存在的路径，并返回它。测试用
// 它表达「这台机器上没有 Tabbit 浏览器」。
func missingTabbitCLI(t *testing.T) string {
	t.Helper()
	p := filepath.ToSlash(filepath.Join(t.TempDir(), "no-such-tabbit-cli.exe"))
	t.Setenv("TABBIT_CLI", p)
	return p
}

// stubTabbitCLI 把启动器指到一个真实存在的文件，让「交给客户端」那条路被
// 走到。真正执行的东西由 fakeLauncher 换掉，所以这里不会拉起任何进程。
func stubTabbitCLI(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tabbit-cli.exe")
	if err := os.WriteFile(p, []byte("stub"), 0o600); err != nil {
		t.Fatalf("writing the stub launcher: %v", err)
	}
	t.Setenv("TABBIT_CLI", filepath.ToSlash(p))
	return p
}

// launcherCalls 记下交接路径交给启动器的东西。
type launcherCalls struct {
	tasks    []string
	programs []string
}

// fakeLauncher 替换调起浏览器那一步。生产代码只会调 runTabbitProgram；
// 这里换掉它的调用点，让「模块把登录页交给 Tabbit 浏览器」这件事可以在
// 没有浏览器、没有 Node、没有 Playwright 的机器上被断言。
func fakeLauncher(t *testing.T, reply json.RawMessage, err error) *launcherCalls {
	t.Helper()
	calls := &launcherCalls{}
	old := runTabbitLauncher
	runTabbitLauncher = func(_ *Client, _ context.Context, task, _ string, program string) (json.RawMessage, error) {
		calls.tasks = append(calls.tasks, task)
		calls.programs = append(calls.programs, program)
		return reply, err
	}
	t.Cleanup(func() { runTabbitLauncher = old })
	return calls
}

func TestTabbitLoginHandsOffTheBrowserFlow(t *testing.T) {
	c := loginClient(t, "", downTransport())

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if st.URL != wantsLoginURL {
		t.Errorf("login url = %q, want %q", st.URL, wantsLoginURL)
	}
	if st.State != core.LoginPending {
		t.Errorf("state = %q, want pending", st.State)
	}
	if !strings.HasPrefix(st.SessionID, loginSessionPrefix) {
		t.Errorf("session id = %q, want the %q namespace", st.SessionID, loginSessionPrefix)
	}
	if st.Code != "" {
		t.Errorf("a browser hand-off must not invent a code, got %q", st.Code)
	}
	if !strings.Contains(st.Message, "导入凭据") {
		t.Errorf("the message must point at the import action: %q", st.Message)
	}
	if !strings.Contains(st.Message, "web-token") {
		t.Errorf("the message must name what the import stores: %q", st.Message)
	}
	if !strings.Contains(st.Message, "not answering yet") {
		t.Errorf("the message must report that the sidecar is down: %q", st.Message)
	}
	if caps := core.CapabilitiesOf(context.Background(), c); !caps.Login {
		t.Fatal("tabbit implements LoginProvider but CapabilitiesOf does not report login")
	}
	// 没有启动器时不能假装已经把登录页交给了客户端：面板只能把链接给操作员，
	// 让他自己去 Tabbit 浏览器里打开。
	if st.LocalApp {
		t.Error("本机没有 Tabbit 启动器，却报告已经把登录页交给了客户端")
	}
	if st.HandoffPath != "" {
		t.Error("没有交接就不该给出要导入的凭据路径")
	}
}

// Tabbit 的登录只能在 Tabbit 浏览器里完成，普通浏览器打开只会被弹回厂商官网。
// 所以模块自己把登录页送进那个浏览器，并告诉面板「别再给我一个在浏览器打开的
// 按钮，改成回来读凭据」。
func TestTabbitLoginOpensThePageInTheTabbitBrowser(t *testing.T) {
	clearTabbitEnv(t)
	stubTabbitCLI(t)
	withCandidates(t, closedPortURL(t))
	calls := fakeLauncher(t, json.RawMessage(`{"url":"`+wantsLoginURL+`"}`), nil)
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, &http.Client{Transport: downTransport()})
	ctx := context.Background()

	st, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if !st.LocalApp {
		t.Error("模块已经把登录页交给 Tabbit 浏览器，却没有告诉面板（local_app）")
	}
	if st.HandoffPath != browserCookiePath {
		t.Errorf("交接后要导入的凭据路径 = %q，want %q", st.HandoffPath, browserCookiePath)
	}
	if !strings.Contains(st.Message, "客户端") {
		t.Errorf("交接的消息必须说清楚登录要在客户端里完成：%q", st.Message)
	}
	if !strings.Contains(st.Message, "读取凭据") {
		t.Errorf("交接的消息必须告诉操作员下一步按哪个按钮：%q", st.Message)
	}
	if len(calls.programs) != 1 {
		t.Fatalf("启动器被调用了 %d 次，want 1", len(calls.programs))
	}
	prog := calls.programs[0]
	if !strings.Contains(prog, wantsLoginURL) {
		t.Errorf("交给浏览器的程序没有导航到登录页：%q", prog)
	}
	if !strings.Contains(prog, "page.goto") || !strings.Contains(prog, "bringToFront") {
		t.Errorf("交给浏览器的程序必须导航并把窗口置前：%q", prog)
	}
	if calls.tasks[0] == cliTaskName {
		t.Errorf("登录页和读 cookie 用同一个任务名 %q，收尾时会把登录页的所有权一起放掉", cliTaskName)
	}

	// 轮询一个交接会话不能再拉一次浏览器：操作员正在那边输密码，面板的收尾
	// 动作是「读凭据」，不是「再开一次」。
	again, err := c.PollLogin(ctx, st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if again.State != core.LoginPending {
		t.Errorf("交接会话在客户端登录完成前应保持 pending，得到 %q", again.State)
	}
	if len(calls.programs) != 1 {
		t.Errorf("轮询又拉起了 %d 次浏览器，want 仍然 1 次", len(calls.programs))
	}
}

// 启动器装了但起不来（常见的 BROWSER_LAUNCH_FAILED）时不能把会话说成交接成功：
// 面板会因此藏掉「在浏览器打开」，操作员就一个入口都没有了。
func TestTabbitLoginFallsBackWhenTheLauncherFails(t *testing.T) {
	clearTabbitEnv(t)
	stubTabbitCLI(t)
	withCandidates(t, closedPortURL(t))
	fakeLauncher(t, nil, errors.New("BROWSER_LAUNCH_FAILED: the launcher refused to start"))
	c := newTestClient(t, `{"base_url":"http://sidecar.test"}`, &http.Client{Transport: downTransport()})

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if st.LocalApp {
		t.Error("启动器失败了，却报告登录页已经交给客户端")
	}
	if st.URL != wantsLoginURL {
		t.Errorf("失败时仍要把链接交给操作员，got %q", st.URL)
	}
	if !strings.Contains(st.Message, "BROWSER_LAUNCH_FAILED") {
		t.Errorf("失败原因必须原样告诉操作员：%q", st.Message)
	}
	if !strings.Contains(st.Message, "导入凭据") {
		t.Errorf("失败后要指出手动那条路：%q", st.Message)
	}
}

func TestTabbitLoginURLFollowsTheConfiguredWebHost(t *testing.T) {
	cases := []struct {
		name    string
		webHost string
		want    string
	}{
		{"unset falls back to the live host", "", wantsLoginURL},
		{"the upstream default is still reachable", "web.tabbit.ai",
			"https://web.tabbit.ai/login?callback=close&flow=history_opt_in&theme=mn"},
		{"a pasted url is reduced to its host", "https://web.tabbit.com/", wantsLoginURL},
		{"surrounding space is ignored", "  web.tabbit.com  ", wantsLoginURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := `{"base_url":"http://sidecar.test"}`
			if tc.webHost != "" {
				cfg = `{"base_url":"http://sidecar.test","web_host":"` + tc.webHost + `"}`
			}
			c := loginClient(t, cfg, downTransport())
			if got := loginURL(c.locate().webHost); got != tc.want {
				t.Errorf("loginURL() for web_host %q = %q, want %q", tc.webHost, got, tc.want)
			}
		})
	}
}

// The panel writes `web_host` onto the endpoint it stores, while the module
// config can only be edited by the operator.  A field that is stored and then
// ignored would be exactly the "looks done but is not" defect, so the stored
// value has to reach the URL the operator is told to open.
func TestTabbitLoginUsesTheHostStoredThroughThePanel(t *testing.T) {
	c := loginClient(t, `{"base_url":"http://sidecar.test"}`, downTransport())
	if got := loginURL(c.locate().webHost); got != wantsLoginURL {
		t.Fatalf("with nothing configured the URL must use the documented host: %q", got)
	}
	addEndpoint(t, c, map[string]string{
		"base_url": "http://sidecar.test",
		"web_host": "https://web.tabbit.ai/",
	})
	want := "https://web.tabbit.ai/login?callback=close&flow=history_opt_in&theme=mn"
	if got := loginURL(c.locate().webHost); got != want {
		t.Errorf("a host stored through the panel must reach the login URL: got %q, want %q", got, want)
	}
	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if !strings.Contains(st.URL, "web.tabbit.ai") {
		t.Errorf("StartLogin returned %q, want the host stored through the panel", st.URL)
	}
}

func TestTabbitLoginPollsUntilTheSidecarListsModels(t *testing.T) {
	var up atomic.Bool
	c := loginClient(t, `{"base_url":"http://sidecar.test"}`, sidecarTransport(&up, healthyHealth, twoModels))
	ctx := context.Background()

	st, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	waiting, err := c.PollLogin(ctx, st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if waiting.State != core.LoginPending {
		t.Fatalf("state while the sidecar is down = %q, want pending", waiting.State)
	}
	if !strings.Contains(waiting.Message, "not usable yet") {
		t.Errorf("the pending message must name the failure: %q", waiting.Message)
	}

	up.Store(true)
	done, err := c.PollLogin(ctx, st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if done.State != core.LoginSuccess {
		t.Fatalf("state = %q (message %q), want success", done.State, done.Message)
	}
	if done.AccountID != "http://sidecar.test" {
		t.Errorf("account id = %q, want the sidecar endpoint", done.AccountID)
	}
	if !strings.Contains(done.Message, "2 model") {
		t.Errorf("the success message must name the evidence: %q", done.Message)
	}
	if !strings.Contains(done.Message, "Nothing was stored") {
		t.Errorf("the success message must not imply a credential landed here: %q", done.Message)
	}

	// The catalogue the login proved is now the cached one, so the panel and
	// the gateway see the ids without another upstream call.
	models, err := c.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 || models[0].ID != "priority" {
		t.Errorf("catalogue after login = %v, want the two bare ids", ids(models))
	}

	again, err := c.PollLogin(ctx, st.SessionID)
	if err != nil || again.State != core.LoginSuccess {
		t.Errorf("a finished session must stay finished: state %q, err %v", again.State, err)
	}
}

func TestTabbitLoginStaysPendingWhileTheSidecarListsNothing(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	c := loginClient(t, `{"base_url":"http://sidecar.test"}`, sidecarTransport(&up, healthyHealth, `{"object":"list","data":[]}`))
	ctx := context.Background()

	st, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	got, err := c.PollLogin(ctx, st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if got.State != core.LoginPending {
		t.Fatalf("state = %q (message %q), want pending: a sidecar that lists nothing is running but not signed in", got.State, got.Message)
	}
	if !strings.Contains(got.Message, "listed no models") || !strings.Contains(got.Message, "does not look signed in") {
		t.Errorf("the message must distinguish empty from unreachable: %q", got.Message)
	}
}

func TestTabbitLoginWarnsAboutHostDrift(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	// The sidecar reports the host it drives; here it disagrees with the host
	// the login URL uses, which is the documented drift hazard.
	drift := sidecarTransport(&up, `{"status":"ok","version":"0.1.9","models":9,"web_host":"web.tabbit.ai"}`, twoModels)
	c := loginClient(t, `{"base_url":"http://sidecar.test"}`, drift)

	st, err := c.StartLogin(context.Background())
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if !strings.Contains(st.Message, "web.tabbit.ai") || !strings.Contains(st.Message, "clients.tabbit.web_host") {
		t.Errorf("the message must surface host drift and how to fix it: %q", st.Message)
	}
}

func TestTabbitLoginRefusesAnUnknownSession(t *testing.T) {
	c := loginClient(t, `{"base_url":"http://sidecar.test"}`, downTransport())
	ctx := context.Background()

	_, err := c.PollLogin(ctx, "tabbit-login-nope")
	if err == nil {
		t.Fatal("PollLogin must refuse a session it never handed out")
	}
	if !strings.Contains(err.Error(), "tabbit-login-nope") {
		t.Errorf("the error must name the session: %v", err)
	}
	if _, err := c.PollLogin(ctx, "   "); err == nil {
		t.Fatal("PollLogin must refuse an empty session id")
	}
	if err := c.CancelLogin(ctx, ""); err == nil {
		t.Fatal("CancelLogin must refuse an empty session id")
	}
}

func TestTabbitLoginCancelIsIdempotent(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	c := loginClient(t, `{"base_url":"http://sidecar.test"}`, sidecarTransport(&up, healthyHealth, twoModels))
	ctx := context.Background()

	st, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if err := c.CancelLogin(ctx, st.SessionID); err != nil {
		t.Fatalf("CancelLogin: %v", err)
	}
	if err := c.CancelLogin(ctx, st.SessionID); err != nil {
		t.Errorf("cancelling twice must be a no-op, got %v", err)
	}
	if _, err := c.PollLogin(ctx, st.SessionID); err == nil {
		t.Fatal("a cancelled session must not be pollable")
	}
	if _, ok := c.getLogin(st.SessionID); ok {
		t.Fatal("the cancelled session is still in the store")
	}
}

func TestTabbitLoginExpiresAForgottenSession(t *testing.T) {
	c := loginClient(t, `{"base_url":"http://sidecar.test"}`, downTransport())
	ctx := context.Background()

	st, err := c.StartLogin(ctx)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	c.loginMu.Lock()
	c.logins[st.SessionID].startedAt = time.Now().Add(-loginSessionTTL - time.Minute)
	c.loginMu.Unlock()

	expired, err := c.PollLogin(ctx, st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if expired.State != core.LoginFailed {
		t.Fatalf("state = %q, want failed after the window", expired.State)
	}
	if !strings.Contains(expired.Message, "start a new login") {
		t.Errorf("the expired message must say what to do: %q", expired.Message)
	}
	again, err := c.PollLogin(ctx, st.SessionID)
	if err != nil {
		t.Fatalf("PollLogin: %v", err)
	}
	if again.State != core.LoginFailed {
		t.Errorf("an expired session must stay failed, got %q", again.State)
	}
}
