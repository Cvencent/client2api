package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The shell used to ship 1.2 MB raw with Cache-Control: no-store -- every
// reload re-downloaded everything and a 304 was impossible (issue #14).
func TestPanelShellCompressesAndRevalidates(t *testing.T) {
	h := New(Options{Version: "test"})

	// A gzip-capable client gets the compressed body, and it is smaller.
	req := httptest.NewRequest(http.MethodGet, "/panel/", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}
	if rec.Body.Len() >= len(indexHTML) {
		t.Fatalf("compressed body %d >= raw %d: nothing was saved", rec.Body.Len(), len(indexHTML))
	}
	if rec.Header().Get("Vary") == "" {
		t.Error("missing Vary: caches would serve gzip to a client that cannot read it")
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag: a reload cannot revalidate")
	}

	// A client without gzip still gets the full body, uncompressed.
	req2 := httptest.NewRequest(http.MethodGet, "/panel/", nil)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Header().Get("Content-Encoding") != "" {
		t.Fatalf("Content-Encoding = %q, want none without Accept-Encoding", rec2.Header().Get("Content-Encoding"))
	}
	if rec2.Body.Len() != len(indexHTML) {
		t.Fatalf("raw body = %d, want %d", rec2.Body.Len(), len(indexHTML))
	}

	// A reload with If-None-Match gets a bodyless 304.
	req3 := httptest.NewRequest(http.MethodGet, "/panel/", nil)
	req3.Header.Set("If-None-Match", etag)
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNotModified {
		t.Fatalf("revalidation status = %d, want 304", rec3.Code)
	}
	if rec3.Body.Len() != 0 {
		t.Fatalf("304 carried %d bytes, want none", rec3.Body.Len())
	}
}

// The vendored libraries are immutable per build: long cache, gzip, and never
// a body on a revalidation the browser will not even send for a year.
func TestVendorsAreCachedAndCompressed(t *testing.T) {
	h := New(Options{Version: "test"})

	req := httptest.NewRequest(http.MethodGet, "/panel/vendors/three.min.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q, want immutable", got)
	}
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Error("vendor not gzipped despite Accept-Encoding: gzip")
	}
	if rec.Body.Len() >= len(threeJS) {
		t.Fatalf("gzipped vendor %d >= raw %d", rec.Body.Len(), len(threeJS))
	}
	if !strings.Contains(string(rec.Body.Bytes()), "\x1f\x8b") {
		t.Error("body does not start with the gzip magic number")
	}
}
