package smscap

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewRoutesRequestsThroughThePerCallProxy proves the proxy is not merely
// stored in Options: the platform request really goes through it.
func TestNewRoutesRequestsThroughThePerCallProxy(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("code"); got != "leftAmount" {
			t.Errorf("target code = %q, want leftAmount", got)
		}
		_, _ = io.WriteString(w, "28.55")
	}))
	defer target.Close()

	var proxyHits int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() {
			t.Errorf("proxy request URL = %q, want an absolute URL", r.URL.String())
		}
		proxyHits++
		out := r.Clone(r.Context())
		out.RequestURI = ""
		resp, err := (&http.Transport{}).RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	c, err := New(Options{Base: target.URL, Token: "tok-abc", HTTP: &http.Client{}, Proxy: proxy.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	balance, err := c.Balance(context.Background())
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != "28.55" {
		t.Fatalf("balance = %q, want 28.55", balance)
	}
	if proxyHits != 1 {
		t.Fatalf("proxy hits = %d, want 1", proxyHits)
	}
}

func TestNewRejectsAMalformedProxy(t *testing.T) {
	_, err := New(Options{Base: "http://example.invalid", Token: "tok-abc", Proxy: "http://[::1"})
	if err == nil {
		t.Fatal("New accepted a malformed proxy")
	}
	if !strings.Contains(err.Error(), "proxy") {
		t.Fatalf("error = %q, want it to name the proxy", err)
	}
}
