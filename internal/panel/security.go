package panel

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"sync"
)

// The panel is a management surface: it holds credentials, can rewrite the
// config file, and can spend every account's quota.  A page that can be framed
// by another site is therefore a clickjacking target ("click here to sign in"
// over a real 重置 button), and a JSON body sniffed as HTML is a scripting
// target.  The headers below close those off.
//
// Ported from the reference internal/panel/index.go.  The reference could use a
// bare script-src 'self' because it keeps its JavaScript in a separate app.js;
// this shell inlines the one script block it has, so the policy names that
// block by SHA-256 hash instead.  The security property is the same -- an
// injected inline script matches neither 'self' nor the hash and does not run --
// without a build step that could drift from the file it is supposed to
// describe.

var (
	cspOnce sync.Once
	cspText string
)

// csp builds the Content-Security-Policy for the shell in doc.
//
// default-src 'none' closes everything and each directive reopens exactly what
// the page needs:
//
//   - script-src 'self' + the inline block's hash: same-origin scripts only, and
//     the one inline block that is actually in the document.
//   - style-src allows 'unsafe-inline' on purpose: the shell has inline
//     style="..." attributes (bar widths, column widths).  Inline CSS cannot
//     execute script, and no external stylesheet origin is opened.
//   - connect-src 'self': the page's fetch() may only call this server, so a
//     compromised page cannot post the API key anywhere else.
//   - img-src 'self' data: for the favicon and the inline SVG chevron.
//   - frame-src 'self': the shell frames exactly one document of ours, the
//     captcha page at /panel/captcha (see captcha.go).  Without this directive
//     the frame would fall back to default-src 'none' and the captcha widget
//     would never appear.  It is not a loosening in the direction that
//     matters: an attacker who can inject markup still cannot frame an
//     off-site origin, so there is no clickjacking surface, and the captcha
//     page's own policy is what names the vendor's origins.
//   - form-action 'none': the config page submits with JS, there is no form
//     target to allow.
//   - frame-ancestors 'none': nothing may frame the panel (the modern
//     equivalent of X-Frame-Options).
//   - base-uri 'none': nothing may inject <base> and rewrite relative URLs.
func csp(doc []byte) string {
	body, ok := scriptBody(doc)
	if !ok {
		// The shell no longer has exactly one inline <script> block.  Rather
		// than emit a policy that blocks the page's own script and leaves a
		// blank screen, fall back to allowing inline script.  The other five
		// headers below are unaffected by this branch, and
		// TestCSPNamesTheInlineScriptFailsIfItCannotHashed keeps this from
		// becoming permanent by accident.
		return "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; " +
			"connect-src 'self'; img-src 'self' data:; frame-src 'self'; form-action 'none'; " +
			"frame-ancestors 'none'; base-uri 'none'"
	}
	sum := sha256.Sum256(body)
	return "default-src 'none'; script-src 'self' 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; " +
		"style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; frame-src 'self'; " +
		"form-action 'none'; frame-ancestors 'none'; base-uri 'none'"
}

// scriptBody returns the exact bytes of the document's single inline <script>
// element.
//
// CSP hashes are computed over the element's text content byte for byte, with
// no trimming, so this deliberately does no whitespace work: the slice between
// the opening tag's ">" and the closing "</script>" is the hashed input.  It
// reports false when the document does not have exactly one such block, because
// in that case there is no single hash that describes the page.
func scriptBody(doc []byte) ([]byte, bool) {
	const open, close = "<script>", "</script>"
	if bytes.Count(doc, []byte(open)) != 1 || bytes.Count(doc, []byte(close)) != 1 {
		return nil, false
	}
	i := bytes.Index(doc, []byte(open))
	j := bytes.Index(doc, []byte(close))
	if i < 0 || j < 0 || j < i+len(open) {
		return nil, false
	}
	body := doc[i+len(open) : j]
	if len(body) == 0 {
		return nil, false
	}
	return body, true
}

// setSecurityHeaders writes the panel's response headers.  It is applied to the
// page and to every /panel/api/* response: the API answers JSON, and those
// responses carry the same data the page renders, so they get the same
// treatment.
func setSecurityHeaders(w http.ResponseWriter) {
	cspOnce.Do(func() { cspText = csp(indexHTML) })
	h := w.Header()
	h.Set("Content-Security-Policy", cspText)
	h.Set("X-Content-Type-Options", "nosniff")           // no MIME sniffing
	h.Set("X-Frame-Options", "DENY")                     // fallback for older browsers
	h.Set("Referrer-Policy", "no-referrer")              // do not leak the panel URL outbound
	h.Set("Cross-Origin-Opener-Policy", "same-origin")   // no window.opener handoff
	h.Set("Cross-Origin-Resource-Policy", "same-origin") // no cross-site embedding
}
