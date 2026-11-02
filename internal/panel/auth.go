package panel

import (
	"net"
	"net/http"
	"net/url"
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

// authed wraps a management handler with the shared bearer and the write-path
// origin gate.
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
		if isWriteMethod(r.Method) && !p.writeOriginAllowed(r) {
			writeErr(w, http.StatusForbidden, "cross-site write rejected: the request did not originate from this panel")
			return
		}
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

// isWriteMethod reports whether the request mutates state.  Reads stay open to
// same-machine tools; only writes are worth gating.
func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete:
		return true
	}
	return false
}

// writeOriginAllowed is the write-path origin gate (issue #103).  With no
// api_key configured the panel is unauthenticated, and every write route was
// reachable by a cross-site simple POST from any web page the operator opens
// -- a no-cors form/fetch never triggers a preflight, the security headers
// protect reads (what leaves this server) but not writes (what enters it), and
// the change landed on disk with no trace in the panel.
//
// Three checks, cheapest first:
//   - Sec-Fetch-Site names the relationship exactly, and the browser cannot be
//     told to omit it: cross-site is refused outright.
//   - Origin, when present, must be this panel: same-origin, or a loopback /
//     unspecified host on any port (the panel's own port moves in tests and
//     across restarts).
//   - The Host header itself must be local.  A domain pointing at 127.0.0.1
//     (DNS rebinding) arrives with an attacker-chosen Host and no Origin; that
//     path is closed by requiring the Host to be local.
//
// A request with no Origin and no Sec-Fetch-Site is a same-machine tool (curl,
// the panel's own fetch): it passes -- same-machine operators are the trusted
// side of this threat model, and breaking curl would break every documented
// recipe.
func (p *panel) writeOriginAllowed(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin" || site == "same-site" || site == "none"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		return p.originAllowed(origin)
	}
	return p.hostAllowed(r.Host)
}

// originAllowed reports whether an Origin header names this panel: the
// configured listen address, or a loopback/unspecified host on any port.
func (p *panel) originAllowed(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if p.listenHostPort() != "" && strings.EqualFold(u.Host, p.listenHostPort()) {
		return true
	}
	return isLocalHost(u.Hostname())
}

// hostAllowed reports whether the request's Host names this deployment: the
// configured listen address or a loopback/unspecified host on any port.
func (p *panel) hostAllowed(host string) bool {
	if host == "" {
		return false
	}
	if p.listenHostPort() != "" && strings.EqualFold(host, p.listenHostPort()) {
		return true
	}
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	return isLocalHost(h)
}

// listenHostPort renders the configured listen address as a host:port for
// header comparison; empty when the panel was told nothing.
func (p *panel) listenHostPort() string {
	if p.opts.Listen == "" {
		return ""
	}
	return strings.TrimPrefix(p.opts.Listen, "http://")
}

// isLocalHost reports whether h is a loopback or unspecified address (any
// interface of this machine).
func isLocalHost(h string) bool {
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}
