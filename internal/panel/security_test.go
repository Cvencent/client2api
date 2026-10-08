package panel

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// securityHeaders are the six headers every panel response must carry.  They are
// listed by name so a rename cannot silently drop one.
var securityHeaders = []string{
	"Content-Security-Policy",
	"X-Content-Type-Options",
	"X-Frame-Options",
	"Referrer-Policy",
	"Cross-Origin-Opener-Policy",
	"Cross-Origin-Resource-Policy",
}

// TestSecurityHeadersAreOnEveryPanelResponse pins the headers on both surfaces:
// the shell (which is unauthenticated, so headers are its only defence) and the
// JSON API (which returns the same secrets the page renders).
func TestSecurityHeadersAreOnEveryPanelResponse(t *testing.T) {
	h := New(Options{Registry: registryOf(), Started: time.Now()})

	cases := []struct {
		name string
		rec  *httptest.ResponseRecorder
	}{
		{"shell", get(t, h, "/panel/")},
		{"api", get(t, h, "/panel/api/status")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", tc.rec.Code, tc.rec.Body.String())
			}
			for _, name := range securityHeaders {
				if got := tc.rec.Header().Get(name); got == "" {
					t.Errorf("%s is missing", name)
				}
			}
			if got := tc.rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := tc.rec.Header().Get("X-Frame-Options"); got != "DENY" {
				t.Errorf("X-Frame-Options = %q, want DENY", got)
			}
			if got := tc.rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("Referrer-Policy = %q, want no-referrer", got)
			}
		})
	}
}

// TestSecurityHeadersAreOnARejectedRequest checks the 401 path too.  A browser
// has to see the policy on the error response, or the page is unprotected
// exactly when something went wrong.
func TestSecurityHeadersAreOnARejectedRequest(t *testing.T) {
	h := New(Options{Registry: registryOf(), AuthEnabled: true, Started: time.Now()})

	req := httptest.NewRequest(http.MethodGet, "/panel/api/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	for _, name := range securityHeaders {
		if got := rec.Header().Get(name); got == "" {
			t.Errorf("%s is missing on the 401", name)
		}
	}
}

// TestCSPNamesTheInlineScriptByHash is the load-bearing test: it recomputes the
// hash of the shell's script block through a different extraction path than
// production uses (string indexing here, bytes.Count/Index there) and demands
// that the policy carry it.  A stale or hand-copied hash fails here instead of
// silently blanking the panel in a browser.
func TestCSPNamesTheInlineScriptByHash(t *testing.T) {
	doc := string(indexHTML)

	const open, close = "<script>", "</script>"
	if strings.Count(doc, open) != 1 || strings.Count(doc, close) != 1 {
		t.Fatalf("shell has %d <%s> and %d <%s>; this test assumes exactly one of each",
			strings.Count(doc, open), open, strings.Count(doc, close), close)
	}
	i := strings.Index(doc, open)
	j := strings.Index(doc, close)
	if i < 0 || j < i+len(open) {
		t.Fatal("could not locate the inline script block")
	}
	body := doc[i+len(open) : j]
	if !strings.HasPrefix(body, "\n\"use strict\";") {
		t.Fatalf("script body starts with %q; the hash would be over the wrong bytes", firstLine(body))
	}
	// Browsers normalize CRLF to LF before hashing inline script text.
	// Mirror that here so the test still checks the real browser rule when
	// the checkout has Windows line endings.
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	sum := sha256.Sum256([]byte(body))
	want := "sha256-" + base64.StdEncoding.EncodeToString(sum[:])

	policy := csp(indexHTML)
	if !strings.Contains(policy, want) {
		t.Fatalf("policy does not carry the shell's own script hash %s\npolicy: %s", want, policy)
	}
	if strings.Contains(policy, "'unsafe-inline'") && !strings.Contains(policy, "style-src") {
		t.Errorf("policy allows inline script: %s", policy)
	}
	// script-src must name the hash and must not also permit inline script.
	scriptSrc := directive(policy, "script-src")
	if !strings.Contains(scriptSrc, want) {
		t.Errorf("script-src = %q, want it to contain %q", scriptSrc, want)
	}
	if strings.Contains(scriptSrc, "'unsafe-inline'") {
		t.Errorf("script-src = %q, must not allow inline script", scriptSrc)
	}
	for _, d := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'none'", "connect-src 'self'"} {
		if !strings.Contains(policy, d) {
			t.Errorf("policy is missing %q\npolicy: %s", d, policy)
		}
	}
}

// TestCSPNormalizesInlineScriptLineEndings pins the browser rule for inline
// script hashes: CRLF and CR are normalized to LF before hashing.
func TestCSPNormalizesInlineScriptLineEndings(t *testing.T) {
	lf := []byte("<html><script>\nlet x = 1;\n</script></html>")
	crlf := []byte(strings.ReplaceAll(string(lf), "\n", "\r\n"))
	if got, want := csp(crlf), csp(lf); got != want {
		t.Fatalf("CRLF and LF documents produced different CSPs:\nCRLF: %s\nLF:   %s", got, want)
	}
}

// TestCSPFallsBackInsteadOfBlankingThePanel covers the safety valve: if the
// shell's shape ever changes so the script block cannot be isolated, the panel
// must still load rather than render nothing.
func TestCSPFallsBackInsteadOfBlankingThePanel(t *testing.T) {
	policy := csp([]byte("<html><body>no script here</body></html>"))
	if !strings.Contains(policy, "script-src 'self' 'unsafe-inline'") {
		t.Fatalf("a document without an isolatable script must fall back to allowing inline script, got %q", policy)
	}
	// The other directives are unaffected by the fallback.
	for _, d := range []string{"default-src 'none'", "frame-ancestors 'none'", "base-uri 'none'"} {
		if !strings.Contains(policy, d) {
			t.Errorf("fallback policy is missing %q", d)
		}
	}
}

// TestScriptBodyHashesExactlyTheBlock checks the extractor against documents
// that are not the real shell, where the expected answer is obvious by hand.
func TestScriptBodyHashesExactlyTheBlock(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
		ok   bool
	}{
		{"plain", "<script>var a = 1;</script>", "var a = 1;", true},
		{"surrounding text", "<h1>x</h1><script>\nlet y=2;\n</script><p>z</p>", "\nlet y=2;\n", true},
		{"no whitespace trimming", "<script>  a  </script>", "  a  ", true},
		{"two blocks", "<script>a</script><script>b</script>", "", false},
		{"no block", "<html></html>", "", false},
		{"empty block", "<script></script>", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := scriptBody([]byte(tc.doc))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if ok && string(got) != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
		})
	}
}

// directive returns one CSP directive's value, or "" when it is absent.
func directive(policy, name string) string {
	for _, part := range strings.Split(policy, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, name+" ") {
			return part
		}
	}
	return ""
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}
