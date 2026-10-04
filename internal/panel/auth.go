package panel

import (
	"net/http"
	"strings"

	"client2api/internal/core"
)

// apiKey is the bearer currently protecting the management surface.
//
// The live holder wins when present, so an operator who rotates the key from
// the config page is enforced against the new value on the next request rather
// than after a restart.  An empty key means the panel is unauthenticated, which
// the reference also allows for a single-operator deployment.
func (p *panel) apiKey() string {
	if p.opts.Live != nil {
		return p.opts.Live.Load().APIKey
	}
	if p.opts.AuthEnabled {
		// The panel was told auth is on but not given the key: that can only
		// happen in a test, where the caller is asserting "protected".  Deny
		// rather than silently opening the surface.
		return "\x00missing-key"
	}
	return ""
}

// authed wraps a management handler with the shared bearer.
//
// A cross-origin preflight (OPTIONS) is answered before the check, because a
// browser never attaches credentials to the preflight itself and would
// otherwise see the API as broken.
func (p *panel) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Every management response carries the panel's security headers,
		// including the preflight and the 401: a browser must see the policy on
		// the error response too, or the page that triggered it is unprotected
		// exactly when something went wrong.
		setSecurityHeaders(w)
		w.Header().Set("Vary", "Authorization, X-Api-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if key := p.apiKey(); key != "" {
			if !core.VerifyBearer(r.Header.Get("Authorization"), r.Header.Get("x-api-key"), key) {
				// 401 + WWW-Authenticate is what makes a browser prompt
				// instead of showing the dashboard's own error string.
				w.Header().Set("WWW-Authenticate", strings.TrimSpace(core.BearerPrefix)+" realm=\"client2api\"")
				writeErr(w, http.StatusUnauthorized, "invalid API key")
				return
			}
		}
		next(w, r)
	}
}
