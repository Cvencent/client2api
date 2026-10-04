package zcode

import (
	"context"
	"strings"

	"client2api/internal/core"
)

// CaptchaScene implements core.CaptchaProvider: it tells the panel what a real
// browser would have to run before this module can claim a promotion.
//
// Why this module needs one at all.  The claim endpoint wants a token in
// X-Aliyun-Captcha-Verify-Param, and that token is minted by Aliyun's own
// JavaScript against the caller's real browser fingerprint.  A Go process
// cannot mint one; the reference implementation gets away with it only because
// it is a desktop app with an embedded WebView.  The panel is a browser, so it
// can run the same SDK -- this method is what tells it which scene to run.
//
// Only the claim action is gated.  Reading the plan preview needs no token (the
// module does that with a plain GET), and nothing else here touches the billing
// endpoints.  Any other action answers Required=false, so a future action
// cannot silently inherit a popup.
//
// Required is keyed on the SCENE ID, and on there being no local solver:
//
//   - No scene id means the widget cannot start at all.  Saying "required"
//     would only produce a dead popup, so the module keeps its old behaviour
//     and reports the gap in Note instead.
//   - A configured captcha_command means the server can already mint the token
//     itself, unattended, including from the scheduler.  Interrupting the
//     operator with a browser window in that case would be a regression, so the
//     browser step stays out of the way.  (The token is still preferred over
//     the solver if the panel ever does send one -- see solveCaptcha.)
//
// The vendor's own enabled flag is reported, not obeyed.  It says whether the
// vendor currently thinks a captcha is needed; it is not a statement that the
// claim endpoint will accept a request without a token, and getting that wrong
// in the permissive direction would mean a silent failure at claim time.
func (c *Client) CaptchaScene(ctx context.Context, action string) (core.CaptchaScene, error) {
	var scene core.CaptchaScene
	if action != "" && action != claimAction {
		return scene, nil
	}

	// sceneFor, not regionFor: the region override must not suppress this read,
	// because the scene id the widget needs only ever comes from the endpoint.
	// See the comment on sceneFor.
	info := c.pool.sceneFor(ctx)
	scene.Enabled = info.Enabled
	scene.SceneID = info.SceneID
	scene.Region = info.Region
	scene.Prefix = info.Prefix
	scene.Required = info.SceneID != "" && strings.TrimSpace(c.cfg.CaptchaCommand) == ""

	switch {
	case !info.Known:
		scene.Note = "没能读到厂商的验证配置（client/configs）。如果领取时提示缺少验证参数，先检查到 z.ai 的网络。"
	case info.SceneID == "":
		scene.Note = "厂商没有下发验证场景号（sceneId），没法在浏览器里跑它的验证码；请配置 captcha_command。"
	case !info.Enabled:
		scene.Note = "厂商当前把验证码开关关掉了，但领取接口仍然要验证参数，所以照常验证一次。"
	}
	return scene, nil
}
