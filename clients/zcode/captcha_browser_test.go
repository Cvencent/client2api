package zcode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCaptchaCacheTTLAndInvalidate(t *testing.T) {
	var cache captchaCache
	now := time.Unix(1000, 0)

	if _, ok := cache.get(now); ok {
		t.Fatal("a fresh cache should be empty")
	}
	cache.put("  tok  ", " cn ", now)
	got, ok := cache.get(now.Add(captchaParamTTL - time.Millisecond))
	if !ok || got.param != "tok" || got.region != "cn" {
		t.Fatalf("get = (%+v,%v), want the trimmed live entry", got, ok)
	}
	if _, ok := cache.get(now.Add(captchaParamTTL)); ok {
		t.Fatal("an entry at its deadline must not be reused")
	}
	cache.put("again", "cn", now)
	cache.invalidate()
	if _, ok := cache.get(now); ok {
		t.Fatal("invalidate must drop the cached entry")
	}
	cache.put("", "cn", now)
	if _, ok := cache.get(now); ok {
		t.Fatal("an empty parameter must not be cached")
	}
}

func TestCaptchaSessionRoundTrip(t *testing.T) {
	sess, err := newCaptchaSession(regionInfo{SceneID: "scene-7", Region: "cn", Prefix: "pre"})
	if err != nil {
		t.Fatalf("newCaptchaSession: %v", err)
	}
	defer sess.close()

	page, err := http.Get(sess.url())
	if err != nil {
		t.Fatalf("GET page: %v", err)
	}
	defer page.Body.Close()
	if page.StatusCode != http.StatusOK {
		t.Fatalf("page status = %d", page.StatusCode)
	}
	body, _ := io.ReadAll(page.Body)
	if !strings.Contains(string(body), "AliyunCaptcha") {
		t.Fatalf("page did not contain the vendor SDK: %q", string(body))
	}

	reply := captchaReply{OK: true, Param: "param-1"}
	raw, _ := json.Marshal(reply)
	resp, err := http.Get("http://" + sess.ln.Addr().String() + "/reply?d=" + url.QueryEscape(string(raw)))
	if err != nil {
		t.Fatalf("GET reply: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reply status = %d", resp.StatusCode)
	}

	got, err := sess.wait(context.Background())
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !got.OK || got.Param != "param-1" {
		t.Fatalf("wait = %+v", got)
	}
}

func TestCaptchaSessionWaitHonorsContext(t *testing.T) {
	sess, err := newCaptchaSession(regionInfo{SceneID: "scene-7", Region: "cn"})
	if err != nil {
		t.Fatalf("newCaptchaSession: %v", err)
	}
	defer sess.close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := sess.wait(ctx); err != context.DeadlineExceeded {
		t.Fatalf("wait err = %v, want context deadline", err)
	}
}

func TestConfigCaptchaBrowserDefaultsOn(t *testing.T) {
	var cfg Config
	if !cfg.captchaBrowser() {
		t.Fatal("captcha_browser must default on")
	}
	off := false
	cfg.CaptchaBrowser = &off
	if cfg.captchaBrowser() {
		t.Fatal("captcha_browser must honor an explicit false")
	}
}

func TestNewBrowserSolverHonorsConfig(t *testing.T) {
	off := false
	if got := newBrowserSolver(&Config{CaptchaBrowser: &off}, nil); got != nil {
		t.Fatal("captcha_browser=false must disable the browser solver")
	}

	exe := filepath.Join(t.TempDir(), "browser.exe")
	if err := os.WriteFile(exe, []byte("fake"), 0o700); err != nil {
		t.Fatalf("write fake browser: %v", err)
	}
	got := newBrowserSolver(&Config{CaptchaBrowserPath: exe}, nil)
	if got == nil || got.exe != exe {
		t.Fatalf("newBrowserSolver = %+v, want the pinned executable", got)
	}

	bad := newBrowserSolver(&Config{CaptchaBrowserPath: filepath.Join(t.TempDir(), "missing")}, nil)
	if bad != nil {
		t.Fatal("a missing captcha_browser_path must fall back to no solver")
	}
}

func TestBrowserSolverRequiresASceneID(t *testing.T) {
	solver := &browserSolver{exe: "does-not-matter"}
	if _, err := solver.solve(context.Background(), regionInfo{}); err == nil {
		t.Fatal("an empty scene id must be refused before a browser is started")
	}
}

func TestSolveCaptchaFallsBackToTheBrowserAndCachesIt(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false,"captcha_browser":true}`)
	ft := routeTransport(t, captchaRoutes(captchaConfigFixture, nil))
	c := env.client(t, ft)

	calls := 0
	c.mintBrowser = func(context.Context, regionInfo) (string, error) {
		calls++
		return "browser-token", nil
	}

	param, region, err := c.solveCaptcha(context.Background())
	if err != nil {
		t.Fatalf("solveCaptcha: %v", err)
	}
	if param != "browser-token" || region != "cn" || calls != 1 {
		t.Fatalf("solveCaptcha = (%q,%q), calls=%d", param, region, calls)
	}
	if param2, _, err := c.solveCaptcha(context.Background()); err != nil || param2 != "browser-token" {
		t.Fatalf("cached solveCaptcha = (%q,%v), want the cached token", param2, err)
	}
	if calls != 1 {
		t.Fatalf("mint calls = %d, want the cached token to be reused", calls)
	}
	c.captcha.invalidate()
	if _, _, err := c.solveCaptcha(context.Background()); err != nil {
		t.Fatalf("solveCaptcha after invalidate: %v", err)
	}
	if calls != 2 {
		t.Fatalf("mint calls = %d, want a fresh mint after invalidate", calls)
	}
}

func TestCaptchaSceneStandsDownWhenTheBrowserSolverIsReady(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false,"captcha_browser":true}`)
	ft := routeTransport(t, captchaRoutes(captchaConfigFixture, nil))
	c := env.client(t, ft)
	c.pool.browser = &browserSolver{exe: "fake"}

	scene, err := c.CaptchaScene(context.Background(), claimAction)
	if err != nil {
		t.Fatalf("CaptchaScene: %v", err)
	}
	if scene.Required {
		t.Fatal("Required = true while the built-in browser solver is ready")
	}
	if scene.SceneID != "scene-7" {
		t.Fatalf("SceneID = %q, want the vendor's scene", scene.SceneID)
	}
}

func TestCountAccountsGroupsByIdentity(t *testing.T) {
	list := []*Account{
		{ID: "a", UserID: "u-1"},
		{ID: "b", UserID: "u-1"},
		{ID: "c", UserID: "u-2"},
		{ID: "d"},
		{ID: "e"},
	}
	if got := countAccounts(list); got != 4 {
		t.Fatalf("countAccounts = %d, want 4", got)
	}
}
