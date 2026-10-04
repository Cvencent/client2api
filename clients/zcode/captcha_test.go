package zcode

// The browser path.  This module's claim endpoint wants an Aliyun token that
// only a real browser can mint, so the panel runs the vendor's SDK and hands the
// result back.  These tests pin the two halves of that contract: what the module
// tells the panel to run (CaptchaScene), and that the token the panel returns is
// actually used in place of a local solver (solveCaptcha + the claim request).
//
// The scene id, region and prefix are read from the vendor's unauthenticated
// client-config endpoint, so every test here has to script that route too --
// which is itself the point of TestCaptchaSceneReadsTheSceneEvenWhenTheRegionIsPinned.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// captchaConfigPath is the fragment routeTransport matches on.
const captchaConfigPath = "/api/v1/client/configs"

// captchaConfigFixture is the vendor document with the captcha switch on.
const captchaConfigFixture = `{"code":0,"msg":"","data":{"configs":{"captcha":` +
	`{"enabled":true,"prefix":"1r7e","region":"cn","sceneId":"scene-7"}}}}`

// captchaConfigOff is the same document with no scene id, which is what a vendor
// that has not turned the feature on sends.
const captchaConfigOff = `{"code":0,"msg":"","data":{"configs":{"captcha":` +
	`{"enabled":false,"prefix":"","region":"cn","sceneId":""}}}}`

// captchaRoutes returns the config route plus whatever else a test needs.
func captchaRoutes(body string, extra map[string]func(*http.Request) (*http.Response, error)) map[string]func(*http.Request) (*http.Response, error) {
	routes := map[string]func(*http.Request) (*http.Response, error){
		captchaConfigPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusOK, body), nil
		},
	}
	for k, v := range extra {
		routes[k] = v
	}
	return routes
}

// captchaEnv builds a client with no solver and no pinned region: exactly the
// state this deployment ships in, so the scene has to come from the vendor.
func captchaEnv(t *testing.T, body string) (*Client, *fakeTransport) {
	t.Helper()
	env := newPanelEnv(t, `{"auto_discover":false}`)
	ft := routeTransport(t, captchaRoutes(body, nil))
	return env.client(t, ft), ft
}

// findRequest returns the recorded request whose path contains fragment.
func findRequest(t *testing.T, ft *fakeTransport, fragment string) *http.Request {
	t.Helper()
	for i := 0; i < ft.count(); i++ {
		if req := ft.requestAt(i); req != nil && strings.Contains(req.URL.Path, fragment) {
			return req
		}
	}
	t.Fatalf("no request to %s among %d recorded", fragment, ft.count())
	return nil
}

// TestCaptchaSceneIsRequiredWithoutASolver is the state a fresh deployment is
// in, and the one the panel's browser step exists for.
func TestCaptchaSceneIsRequiredWithoutASolver(t *testing.T) {
	c, _ := captchaEnv(t, captchaConfigFixture)

	scene, err := c.CaptchaScene(context.Background(), claimAction)
	if err != nil {
		t.Fatalf("CaptchaScene: %v", err)
	}
	if !scene.Required {
		t.Fatal("Required = false; the panel would skip the browser and the claim would go out tokenless")
	}
	if scene.SceneID != "scene-7" || scene.Region != "cn" || scene.Prefix != "1r7e" {
		t.Errorf("scene = %+v, want the vendor's parameters", scene)
	}
	if !scene.Enabled {
		t.Error("Enabled = false, want the vendor's switch echoed")
	}
	// No note: there is nothing surprising to tell the operator.
	if scene.Note != "" {
		t.Errorf("Note = %q, want empty when the scene is complete", scene.Note)
	}
}

// TestCaptchaSceneStandsDownWhenASolverIsConfigured pins the regression guard:
// a deployment with captcha_command already mints tokens unattended, including
// from the scheduler, so interrupting the operator with a popup would be worse
// than doing nothing.  The scene is still reported, so the panel can show what
// the vendor asked for.
func TestCaptchaSceneStandsDownWhenASolverIsConfigured(t *testing.T) {
	c, _ := captchaEnv(t, captchaConfigFixture)
	installSolver(t, c, "token-from-solver")

	scene, err := c.CaptchaScene(context.Background(), claimAction)
	if err != nil {
		t.Fatalf("CaptchaScene: %v", err)
	}
	if scene.Required {
		t.Fatal("Required = true with a solver configured; the scheduler would open a dialog nobody is watching")
	}
	if scene.SceneID != "scene-7" {
		t.Errorf("SceneID = %q, want the scene still reported", scene.SceneID)
	}
}

// TestCaptchaSceneIsNotRequiredWithoutASceneID covers the other half of the
// Required rule.  A missing scene id means the widget cannot start at all, so
// "required" would only produce a dialog that loads and hangs.
func TestCaptchaSceneIsNotRequiredWithoutASceneID(t *testing.T) {
	c, _ := captchaEnv(t, captchaConfigOff)

	scene, err := c.CaptchaScene(context.Background(), claimAction)
	if err != nil {
		t.Fatalf("CaptchaScene: %v", err)
	}
	if scene.Required {
		t.Fatal("Required = true without a scene id; the popup could never start")
	}
	if !strings.Contains(scene.Note, "sceneId") {
		t.Errorf("Note = %q, want the missing scene id named", scene.Note)
	}
}

// TestCaptchaSceneReportsADisabledCaptchaButStillRequiresIt pins the deliberate
// refusal to obey the vendor's own switch.  The flag says whether the vendor
// currently thinks a captcha is needed; it is not a promise that the claim
// endpoint will accept a request without a token, and getting that wrong in the
// permissive direction is a silent failure at claim time.
func TestCaptchaSceneReportsADisabledCaptchaButStillRequiresIt(t *testing.T) {
	c, _ := captchaEnv(t, `{"code":0,"data":{"configs":{"captcha":`+
		`{"enabled":false,"prefix":"1r7e","region":"cn","sceneId":"scene-7"}}}}`)

	scene, err := c.CaptchaScene(context.Background(), claimAction)
	if err != nil {
		t.Fatalf("CaptchaScene: %v", err)
	}
	if scene.Enabled {
		t.Error("Enabled = true, want the vendor's switch echoed as off")
	}
	if !scene.Required {
		t.Fatal("Required = false because the vendor's flag is off; a claim without a token would be rejected")
	}
	if !strings.Contains(scene.Note, "开关") {
		t.Errorf("Note = %q, want the switch explained", scene.Note)
	}
}

// TestCaptchaSceneReportsAnUnreachableVendor: a failed config read and a vendor
// that turned its captcha off both leave Enabled false, so Known is what keeps
// them apart for the operator.
func TestCaptchaSceneReportsAnUnreachableVendor(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	ft := routeTransport(t, map[string]func(*http.Request) (*http.Response, error){
		captchaConfigPath: func(*http.Request) (*http.Response, error) {
			return jsonResponse(http.StatusServiceUnavailable, `{"code":503,"msg":"busy"}`), nil
		},
	})
	c := env.client(t, ft)

	scene, err := c.CaptchaScene(context.Background(), claimAction)
	if err != nil {
		t.Fatalf("CaptchaScene: %v", err)
	}
	if scene.Required {
		t.Fatal("Required = true with no scene id; the popup could never start")
	}
	if !strings.Contains(scene.Note, "网络") {
		t.Errorf("Note = %q, want the network named", scene.Note)
	}
}

// TestCaptchaSceneIgnoresOtherActions: only the claim is gated.  Reading the
// plan preview needs no token, and a future action must not silently inherit a
// popup -- so an unknown action answers the zero scene and costs no request.
func TestCaptchaSceneIgnoresOtherActions(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	ft := &fakeTransport{}
	c := env.client(t, ft)

	for _, action := range []string{"daily-signin", "refresh_models", "claim-now"} {
		scene, err := c.CaptchaScene(context.Background(), action)
		if err != nil {
			t.Fatalf("CaptchaScene(%q): %v", action, err)
		}
		if scene != (core.CaptchaScene{}) {
			t.Errorf("CaptchaScene(%q) = %+v, want the zero scene", action, scene)
		}
	}
	if ft.count() != 0 {
		t.Errorf("made %d requests for actions that are not gated", ft.count())
	}
}

// TestCaptchaSceneReadsTheSceneEvenWhenTheRegionIsPinned is why regionFor and
// sceneFor are two methods.  The override exists so a deployment can pin the
// REGION without talking to the endpoint -- the solver path needs nothing else,
// and TestRegionConfigOverrideWins pins that no request is made.  But the panel
// cannot run the widget without a scene id, and a scene id only ever comes from
// the endpoint, so the browser path must still fetch.
func TestCaptchaSceneReadsTheSceneEvenWhenTheRegionIsPinned(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false,"captcha_region":"sgp"}`)
	ft := routeTransport(t, captchaRoutes(captchaConfigFixture, nil))
	c := env.client(t, ft)

	scene, err := c.CaptchaScene(context.Background(), claimAction)
	if err != nil {
		t.Fatalf("CaptchaScene: %v", err)
	}
	if scene.Region != "sgp" {
		t.Errorf("Region = %q, want the pinned override", scene.Region)
	}
	if scene.SceneID != "scene-7" || scene.Prefix != "1r7e" {
		t.Errorf("scene = %+v, want the scene id and prefix from the endpoint", scene)
	}
	if !scene.Required {
		t.Error("Required = false; the override suppressed the browser step")
	}
}

// TestCaptchaSceneAnswersTheDefaultAction pins that the empty action means the
// same thing as the claim: the panel always names one, but a hand-written curl
// should not have to guess a magic value.
func TestCaptchaSceneAnswersTheDefaultAction(t *testing.T) {
	c, _ := captchaEnv(t, captchaConfigFixture)

	scene, err := c.CaptchaScene(context.Background(), "")
	if err != nil {
		t.Fatalf("CaptchaScene: %v", err)
	}
	if !scene.Required || scene.SceneID != "scene-7" {
		t.Errorf("scene = %+v, want the same answer as the claim action", scene)
	}
}

// TestSolveCaptchaPrefersTheBrowserToken is the precedence rule.  A token the
// operator watched succeed, in their own browser, against the real risk engine
// on this exact network, beats anything this process can arrange -- and the
// solver must not even run.
func TestSolveCaptchaPrefersTheBrowserToken(t *testing.T) {
	c, _ := captchaEnv(t, captchaConfigFixture)
	// A solver that would produce a different token, to prove which one won.
	installSolver(t, c, "token-from-solver")

	ctx := core.WithCaptchaSolution(context.Background(), core.CaptchaSolution{Param: "token-from-browser", Region: "cn"})
	got, err := c.solveCaptcha(ctx, regionInfo{Region: "cn"})
	if err != nil {
		t.Fatalf("solveCaptcha: %v", err)
	}
	if got != "token-from-browser" {
		t.Fatalf("param = %q, want the browser's token", got)
	}
}

// TestSolveCaptchaWithoutASolverPointsAtThePanel: the error has to be a
// configuration fact, not a vendor failure, and it has to name both ways out.
func TestSolveCaptchaWithoutASolverPointsAtThePanel(t *testing.T) {
	c, _ := captchaEnv(t, captchaConfigFixture)

	_, err := c.solveCaptcha(context.Background(), regionInfo{Region: "cn"})
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("err = %v, want core.ErrNotConfigured", err)
	}
	if !strings.Contains(err.Error(), "captcha_command") || !strings.Contains(err.Error(), "panel") {
		t.Errorf("err = %q, want both the solver and the panel named", err)
	}
}

// TestCaptchaRegionPrefersTheBrowserToken: the region describes where the token
// was actually minted, so it travels with the token rather than being resolved
// again from config.
func TestCaptchaRegionPrefersTheBrowserToken(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false,"captcha_region":"sgp"}`)
	ft := routeTransport(t, captchaRoutes(captchaConfigFixture, nil))
	c := env.client(t, ft)

	ctx := core.WithCaptchaSolution(context.Background(), core.CaptchaSolution{Param: "tok", Region: "cn"})
	if got := c.captchaRegion(ctx); got != "cn" {
		t.Fatalf("region = %q, want the token's own region", got)
	}
	// No token: fall back to the resolved config, which is the pinned override.
	if got := c.captchaRegion(context.Background()); got != "sgp" {
		t.Fatalf("region = %q, want the pinned override", got)
	}
}

// TestCheckinUsesTheBrowserTokenEndToEnd is the payoff.  No solver is
// configured, and the claim still goes out carrying the token and region the
// panel produced -- which is the whole reason the browser path exists.
func TestCheckinUsesTheBrowserTokenEndToEnd(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	ft := routeTransport(t, captchaRoutes(captchaConfigFixture, previewRoutes()))
	c := env.client(t, ft)
	id := addJWTAccount(t, c)

	ctx := core.WithCaptchaSolution(context.Background(), core.CaptchaSolution{Param: "token-from-browser", Region: "cn"})
	res, err := c.Checkin(ctx, id, claimAction)
	if err != nil {
		t.Fatalf("Checkin: %v", err)
	}
	if !res.OK {
		t.Fatalf("res = %+v, want the claim to succeed", res)
	}

	claim := findRequest(t, ft, planClaimPath)
	if got := claim.Header.Get("X-Aliyun-Captcha-Verify-Param"); got != "token-from-browser" {
		t.Errorf("Verify-Param = %q, want the browser's token", got)
	}
	if got := claim.Header.Get("X-Aliyun-Captcha-Verify-Region"); got != "cn" {
		t.Errorf("Verify-Region = %q, want the token's region", got)
	}
	// The token is a per-call bearer credential: it must not have been written
	// anywhere the operator can read it back.
	if strings.Contains(res.Message, "token-from-browser") {
		t.Errorf("message = %q, must not echo the token", res.Message)
	}
}

// TestCheckinWithoutASolverAndWithoutABrowserReportsNotConfigured is the state
// the scheduler runs in when no browser is attached.  It has to be a refusal
// (a nil error), not a failure, or an unattended sweep would report a storm of
// transport failures for what is really a missing configuration.
func TestCheckinWithoutASolverAndWithoutABrowserReportsNotConfigured(t *testing.T) {
	env := newPanelEnv(t, `{"auto_discover":false}`)
	ft := routeTransport(t, captchaRoutes(captchaConfigFixture, previewRoutes()))
	c := env.client(t, ft)
	id := addJWTAccount(t, c)

	res, err := c.Checkin(context.Background(), id, claimAction)
	if err != nil {
		t.Fatalf("Checkin returned a Go error (%v); the scheduler would count this as a failure", err)
	}
	if res.OK {
		t.Fatalf("res = %+v, want a refusal", res)
	}
	if !strings.Contains(res.Error, "captcha_command") {
		t.Errorf("error = %q, want the missing solver named", res.Error)
	}
	// And nothing was sent to the claim endpoint.
	for i := 0; i < ft.count(); i++ {
		if strings.Contains(ft.requestAt(i).URL.Path, planClaimPath) {
			t.Fatal("a claim request went out without a token")
		}
	}
}
