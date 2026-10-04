package panel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// The panel knows nothing about Aliyun.  It asks the module what a browser would
// have to run, frames a document that runs it, and hands the answer back in the
// context of the action the operator clicked.  Every test below is driven by
// whether the module implements core.CaptchaProvider, so the whole mechanism can
// appear or disappear without the panel changing.
// ---------------------------------------------------------------------------

// fakeCaptchaClient implements core.Client + core.CaptchaProvider and nothing
// else.
type fakeCaptchaClient struct {
	*fakeBareClient
	scene core.CaptchaScene
	err   error

	// asked records the action each call was made for, so a test can prove the
	// panel forwards the operator's choice instead of asking for a default.
	asked []string
}

func (f *fakeCaptchaClient) CaptchaScene(_ context.Context, action string) (core.CaptchaScene, error) {
	f.asked = append(f.asked, action)
	return f.scene, f.err
}

func newCaptchaClient(name string, scene core.CaptchaScene) *fakeCaptchaClient {
	return &fakeCaptchaClient{fakeBareClient: &fakeBareClient{name: name}, scene: scene}
}

func TestCaptchaSceneWithoutTheCapabilityIs501(t *testing.T) {
	// The panel hides the browser step from this answer alone, so a module that
	// never hits a captcha must produce it rather than a zero-valued scene the
	// shell would try to run.
	p := taskPanel(t, &fakeBareClient{name: "plain"})

	rec, _ := doTask(t, p, http.MethodGet, "/panel/api/clients/plain/captcha?action=claim", "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "CaptchaProvider") {
		t.Errorf("body = %s, want the missing interface named", rec.Body.String())
	}
}

func TestCaptchaSceneForwardsTheActionAndReturnsTheScene(t *testing.T) {
	c := newCaptchaClient("zcode", core.CaptchaScene{
		Required: true,
		Enabled:  true,
		SceneID:  "scene-7",
		Region:   "cn",
		Prefix:   "1r7e",
	})
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodGet, "/panel/api/clients/zcode/captcha?action=claim", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	scene, _ := out["captcha"].(map[string]any)
	if scene == nil {
		t.Fatalf("no captcha object in %s", rec.Body.String())
	}
	// Every field the shell needs to build the iframe URL has to survive the
	// round trip; a dropped scene id would render a popup that can never open.
	if scene["required"] != true || scene["enabled"] != true {
		t.Errorf("flags lost: %v", scene)
	}
	if scene["scene_id"] != "scene-7" || scene["region"] != "cn" || scene["prefix"] != "1r7e" {
		t.Errorf("scene parameters lost: %v", scene)
	}
	if len(c.asked) != 1 || c.asked[0] != "claim" {
		t.Fatalf("module was asked for %v, want [claim]", c.asked)
	}
}

func TestCaptchaSceneTreatsAMissingActionAsTheDefault(t *testing.T) {
	// The query parameter is optional by design: the panel always sends one, but
	// a hand-written curl should not have to guess a magic value, and the module
	// decides what "no action" means.
	c := newCaptchaClient("zcode", core.CaptchaScene{})
	p := taskPanel(t, c)

	rec, _ := doTask(t, p, http.MethodGet, "/panel/api/clients/zcode/captcha", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(c.asked) != 1 || c.asked[0] != "" {
		t.Fatalf("module was asked for %q, want the empty default", c.asked)
	}
}

func TestCaptchaSceneRejectsNonGet(t *testing.T) {
	// A scene read is a description, not a spend.  Allowing a POST here would
	// invite a module to treat it as "run the action", which is what the checkin
	// route is for.
	c := newCaptchaClient("zcode", core.CaptchaScene{SceneID: "s"})
	p := taskPanel(t, c)

	rec, _ := doTask(t, p, http.MethodPost, "/panel/api/clients/zcode/captcha?action=claim", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405 (body=%s)", rec.Code, rec.Body.String())
	}
	if len(c.asked) != 0 {
		t.Errorf("the module was asked %v despite the rejected method", c.asked)
	}
}

func TestCaptchaSceneReportsAFailureAsABadGateway(t *testing.T) {
	// Unlike Checkin, there is nothing useful to show without an answer: the
	// panel would not know whether to run the SDK.  So a module error is an
	// error here, and the panel must not silently skip the browser step and let
	// the action run tokenless.
	c := newCaptchaClient("zcode", core.CaptchaScene{})
	c.err = errors.New("zcode: the config endpoint said 503")
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodGet, "/panel/api/clients/zcode/captcha?action=claim", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	if msg, _ := out["error"].(string); !strings.Contains(msg, "503") {
		t.Errorf("error = %q, want the module's reason", msg)
	}
}

func TestCaptchaSceneRedactsAFailure(t *testing.T) {
	// The route is unauthenticated, and a module error is free-form text that
	// may quote an upstream body.  core.Redact is what keeps a token out of it.
	c := newCaptchaClient("zcode", core.CaptchaScene{})
	c.err = errors.New("upstream rejected Bearer sk-live-0123456789abcdefghijklmnop")
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodGet, "/panel/api/clients/zcode/captcha?action=claim", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	msg, _ := out["error"].(string)
	if strings.Contains(msg, "sk-live-0123456789abcdefghijklmnop") {
		t.Fatalf("error = %q, want the credential redacted", msg)
	}
}

func TestCaptchaPageServesTheDocumentWithItsOwnPolicy(t *testing.T) {
	p := taskPanel(t)

	rec := httptest.NewRecorder()
	p.captchaPage(rec, httptest.NewRequest(http.MethodGet, "/panel/captcha?scene_id=s", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	// SAMEORIGIN, not the shell's DENY: the panel frames this page on purpose,
	// and DENY would render a permanently blank dialog.
	if got := rec.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Errorf("X-Frame-Options = %q, want SAMEORIGIN", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != captchaPolicy() {
		t.Errorf("CSP = %q, want the captcha policy", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	// The document must be the real one, not an empty body that happens to have
	// the right headers.
	if !strings.Contains(rec.Body.String(), "AliyunCaptcha.js") {
		t.Error("the served document does not load the vendor SDK")
	}
}

func TestCaptchaPageRejectsNonGet(t *testing.T) {
	p := taskPanel(t)

	rec := httptest.NewRecorder()
	p.captchaPage(rec, httptest.NewRequest(http.MethodPost, "/panel/captcha", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET" {
		t.Errorf("Allow = %q, want GET", got)
	}
}

// TestCaptchaPolicyConfinesThePageToTheVendor pins the two properties that make
// a page running third-party script safe to serve next to a panel that holds an
// API key and can rewrite the config file.
func TestCaptchaPolicyConfinesThePageToTheVendor(t *testing.T) {
	policy := captchaPolicy()

	if !strings.Contains(policy, "default-src 'none'") {
		t.Error("the policy does not start from nothing")
	}
	// connect-src is the one that matters: this page must not be able to reach
	// the panel API, which is same-origin and unauthenticated by default.
	connect := directive(policy, "connect-src")
	if strings.Contains(connect, "'self'") {
		t.Errorf("connect-src = %q, must not include 'self'", connect)
	}
	if !strings.Contains(connect, "https://*.aliyuncs.com") {
		t.Errorf("connect-src = %q, want the vendor's verification host", connect)
	}
	if !strings.Contains(policy, "frame-ancestors 'self'") {
		t.Errorf("policy = %q, want frame-ancestors 'self' so the panel can frame it", policy)
	}
	if !strings.Contains(policy, "form-action 'none'") {
		t.Errorf("policy = %q, want form-action 'none'", policy)
	}
	// The SDK injects its own inline bootstrap, so this is required rather than
	// sloppy -- see the comment on captchaPolicy.
	if !strings.Contains(directive(policy, "script-src"), "'unsafe-inline'") {
		t.Error("script-src lost 'unsafe-inline'; the vendor SDK cannot boot without it")
	}
}

// TestShellCSPAllowsFramingTheCaptchaPage is the other half of the pair: the
// shell's own policy has to permit a same-origin frame, or the dialog renders
// blank while every other header looks correct.
func TestShellCSPAllowsFramingTheCaptchaPage(t *testing.T) {
	policy := csp(indexHTML)

	if !strings.Contains(directive(policy, "frame-src"), "'self'") {
		t.Fatalf("frame-src = %q, want 'self' so the panel can frame /panel/captcha", directive(policy, "frame-src"))
	}
	// frame-src must not become a blanket loosening: the panel has no reason to
	// frame anything off-origin.
	if strings.Contains(directive(policy, "frame-src"), "https://") {
		t.Errorf("frame-src = %q, want same-origin only", directive(policy, "frame-src"))
	}
}

// TestTheShellAndTheCaptchaPageAgreeOnTheirHandshake is a contract test across
// two files that no compiler checks.  A rename on one side would produce a
// dialog that loads, verifies, and then hangs forever with no error anywhere.
func TestTheShellAndTheCaptchaPageAgreeOnTheirHandshake(t *testing.T) {
	shell := string(indexHTML)
	page := string(captchaHTML)

	for _, token := range []string{"c2a-captcha"} {
		if !strings.Contains(shell, token) {
			t.Errorf("the shell does not mention %q", token)
		}
		if !strings.Contains(page, token) {
			t.Errorf("the captcha page does not mention %q", token)
		}
	}
	// Every query parameter the page reads has to be one the shell writes.
	for _, param := range []string{"action", "scene_id", "region", "prefix"} {
		if !strings.Contains(page, param) {
			t.Errorf("the captcha page does not read %q", param)
		}
		if !strings.Contains(shell, param) {
			t.Errorf("the shell does not send %q", param)
		}
	}
	// And the frame has to point at the route the panel actually mounts.
	if !strings.Contains(shell, "/panel/captcha") {
		t.Error("the shell does not point its iframe at /panel/captcha")
	}
}
