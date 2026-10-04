package workbuddy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// --- shared helpers for the ported vendor-domain endpoints -------------------
//
// The endpoint tests drive the new read/claim functions directly (rather than
// through a chore runner) so they can pin the exact wire shape: method, path,
// headers and body fields.  fakeRT records every request; these helpers keep the
// fixtures readable and the assertions short.

// wbEnvelope wraps a data payload in the vendor's {code,msg,data} envelope,
// which is what doJSON/taskJSON expect on a successful answer.
func wbEnvelope(data string) string {
	if strings.TrimSpace(data) == "" {
		return `{"code":0,"msg":""}`
	}
	return `{"code":0,"msg":"","data":` + data + `}`
}

// wbRefusal is the HTTP-200 answer with a non-zero business code, i.e. the
// shape upstream uses for "already claimed" / "locked" style refusals.
func wbRefusal(code int, msg string) string {
	b, err := json.Marshal(map[string]any{"code": code, "msg": msg})
	if err != nil {
		panic(err)
	}
	return string(b)
}

// wbAuth returns the one live credential the fixture set up.
func wbAuth(t *testing.T, c *Client, uid string) *Auth {
	t.Helper()
	if a := c.findAuthByUID(uid); a != nil {
		return a
	}
	t.Fatalf("no live account with uid %q", uid)
	return nil
}

// wbCNAuth is the CN fixture account (uid-cn-0001).
func wbCNAuth(t *testing.T, c *Client) *Auth {
	t.Helper()
	return wbAuth(t, c, "uid-cn-0001")
}

// wbIntlAuth is the international fixture account (uid-intl-0001).
func wbIntlAuth(t *testing.T, c *Client) *Auth {
	t.Helper()
	return wbAuth(t, c, "uid-intl-0001")
}

// wbIdentity asserts the billing identity headers every growth/billing call must
// carry, and returns the request for further checks.
func wbIdentity(t *testing.T, req *http.Request, a *Auth) *http.Request {
	t.Helper()
	if req == nil {
		t.Fatal("no request was sent")
	}
	if got, want := req.Header.Get("Authorization"), "Bearer "+a.AccessTokenValue(); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
	if got, want := req.Header.Get("X-User-Id"), a.UIDValue(); got != want {
		t.Fatalf("X-User-Id = %q, want %q", got, want)
	}
	if req.Header.Get("User-Agent") == "" {
		t.Fatal("no User-Agent was set")
	}
	return req
}

// wbBody decodes a request body into a map.
func wbBody(t *testing.T, req *http.Request) map[string]any {
	t.Helper()
	if req == nil || req.Body == nil {
		t.Fatal("no request body to decode")
	}
	var got map[string]any
	if err := json.NewDecoder(req.Body).Decode(&got); err != nil {
		t.Fatalf("decode %s %s body: %v", req.Method, req.URL.Path, err)
	}
	return got
}

// wbPaths lists the paths of every request the transport saw, in order.
func wbPaths(rt *fakeRT) []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	out := make([]string, 0, len(rt.calls))
	for _, r := range rt.calls {
		out = append(out, r.URL.Path)
	}
	return out
}

// wbCalls counts requests whose path is exactly p.
func wbCalls(rt *fakeRT, p string) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	n := 0
	for _, r := range rt.calls {
		if r.URL.Path == p {
			n++
		}
	}
	return n
}

// wbHostOf returns the host of the single request the transport saw.
func wbHostOf(t *testing.T, rt *fakeRT) string {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.calls) == 0 {
		t.Fatal("no request was sent")
	}
	return rt.calls[0].URL.Host
}
