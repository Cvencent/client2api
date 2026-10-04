package panel

import (
	_ "embed"
	"net/http"
	"strings"
	"time"

	"client2api/internal/core"
)

// captchaHTML is the document that runs the vendor's captcha SDK.  It is served
// as its own page rather than inlined into the shell because it needs a policy
// the shell must not have.
//
//go:embed captcha.html
var captchaHTML []byte

// captchaPolicy is the Content-Security-Policy for the captcha document.
//
// It is deliberately NOT csp(indexHTML).  This document exists to run one
// third-party script, so it has to name that script's origins; the shell, which
// holds the API key and can rewrite the config file, does not.  Keeping the two
// policies apart is the entire reason the captcha lives in its own document.
//
// The origins are the ones Aliyun's own CSP FAQ lists for captcha 2.0: the SDK
// comes from *.alicdn.com, and the verification calls go to *.aliyuncs.com.
//
// Two deliberate loosenings, both scoped to this page only:
//
//   - script-src carries 'unsafe-inline'.  The SDK injects inline bootstrap
//     blocks of its own, and a hash (or a nonce, which we cannot mint for it)
//     would not describe them -- it would only break the widget.  Note that a
//     hash would not help even if we added one: per CSP, 'unsafe-inline' is
//     ignored once a hash is present, so the choice is one or the other.
//   - connect-src does NOT include 'self'.  Nothing on this page may call the
//     panel API, so the loosest directive is still confined to the vendor.
//
// frame-ancestors is 'self' rather than 'none' so the panel can frame it, which
// is why the page also has to override X-Frame-Options (the shell uses DENY).
func captchaPolicy() string {
	return "default-src 'none'; " +
		"script-src 'self' 'unsafe-inline' https://*.alicdn.com; " +
		"style-src 'self' 'unsafe-inline' https://*.alicdn.com; " +
		"connect-src https://*.aliyuncs.com https://*.alicdn.com; " +
		"img-src 'self' data: https://*.aliyuncs.com https://*.alicdn.com; " +
		"frame-src https://*.aliyuncs.com https://*.alicdn.com; " +
		"font-src https://*.alicdn.com; " +
		"form-action 'none'; frame-ancestors 'self'; base-uri 'none'"
}

// captchaPage serves the captcha document.
//
// It is a document, not JSON, so it is mounted at /panel/captcha rather than
// under /panel/api/.  It is unauthenticated on purpose and carries no secret:
// everything it needs arrives in its query string, and every value there
// (scene id, region, prefix) is something the vendor hands to any browser.
// Authentication would not add anything either -- the shell that frames it is
// itself open, because a browser has to load the page before it can be asked
// for a key.
func (p *panel) captchaPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET")
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", captchaPolicy())
	h.Set("X-Content-Type-Options", "nosniff")
	// SAMEORIGIN, not DENY: the panel frames this page on purpose.
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(captchaHTML)
}

// captchaScene answers GET <base>/captcha?action=…: what this module would need
// from a real browser before it can run that action.
//
// It is a GET because it is a description, not a spend.  A module is expected
// to answer it from what it already knows (zcode reads the scene the vendor
// publishes alongside the rest of its client config), and the panel only asks
// when the operator has actually clicked the button -- never on page load.
//
// A module without the capability answers 501, the usual "no button for an
// unimplemented mechanism" rule, so the panel can hide the browser step
// entirely instead of offering something that cannot work.
func (p *panel) captchaScene(w http.ResponseWriter, r *http.Request, c core.Client) {
	cp, ok := core.AsCaptchaProvider(c)
	if !ok {
		writeErr(w, http.StatusNotImplemented, c.Name()+" has no captcha scene: it does not implement core.CaptchaProvider")
		return
	}
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	ctx, cancel := p.ctx(r, 30*time.Second)
	defer cancel()
	scene, err := cp.CaptchaScene(ctx, strings.TrimSpace(r.URL.Query().Get("action")))
	if err != nil {
		// Unlike Checkin, there is nothing useful to show without an answer:
		// the panel would not know whether to run the SDK.  So this is an
		// error, the same way a balance read is.
		writeErr(w, http.StatusBadGateway, core.Redact(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"captcha": scene})
}
