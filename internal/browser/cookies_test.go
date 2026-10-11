package browser

import (
	"context"
	"testing"
)

// The cookie reader is the one shared addition the Doubao login needs: the
// session lives in HttpOnly cookies, so only a CDP client attached to the
// browser can read it back.  These tests drive the method over an in-process
// fake CDP endpoint and assert both the command and the decoded reply.
func TestPageCookiesReadsNetworkGetCookies(t *testing.T) {
	var gotURLs []any
	f := startCookieFake(t, func(method string, params map[string]any) any {
		if method != "Network.getCookies" {
			t.Errorf("method = %q, want Network.getCookies", method)
		}
		if urls, ok := params["urls"].([]any); ok {
			gotURLs = urls
		}
		return map[string]any{"cookies": []map[string]any{
			{"name": "sessionid", "value": "abc", "domain": ".doubao.com", "path": "/", "httpOnly": true, "secure": true},
			{"name": "ttwid", "value": "xyz", "domain": ".doubao.com", "path": "/"},
		}}
	})
	defer f.ln.Close()

	p := newCookiePage(t, f.addr)
	p.sessionID = "test-session"
	defer p.shutdown()

	cookies, err := p.Cookies(context.Background(), []string{"https://www.doubao.com/"})
	if err != nil {
		t.Fatalf("Cookies: %v", err)
	}
	if len(gotURLs) != 1 || gotURLs[0] != "https://www.doubao.com/" {
		t.Fatalf("urls = %v, want the doubao origin", gotURLs)
	}
	if len(cookies) != 2 {
		t.Fatalf("got %d cookies, want 2", len(cookies))
	}
	if cookies[0].Name != "sessionid" || cookies[0].Value != "abc" || !cookies[0].HTTPOnly {
		t.Fatalf("cookie[0] = %+v, want an httpOnly sessionid=abc", cookies[0])
	}
	if cookies[1].Name != "ttwid" || cookies[1].Value != "xyz" {
		t.Fatalf("cookie[1] = %+v, want ttwid=xyz", cookies[1])
	}
}

// TestPageCookiesEmptyURLsFallsBackToStorage pins the documented fallback: an
// empty url list asks Storage.getCookies for the whole jar.
func TestPageCookiesEmptyURLsFallsBackToStorage(t *testing.T) {
	var method string
	f := startCookieFake(t, func(m string, params map[string]any) any {
		method = m
		return map[string]any{"cookies": []map[string]any{{"name": "a", "value": "b"}}}
	})
	defer f.ln.Close()

	p := newCookiePage(t, f.addr)
	p.sessionID = "test-session"
	defer p.shutdown()

	if _, err := p.Cookies(context.Background(), nil); err != nil {
		t.Fatalf("Cookies: %v", err)
	}
	if method != "Storage.getCookies" {
		t.Fatalf("method = %q, want Storage.getCookies", method)
	}
}
