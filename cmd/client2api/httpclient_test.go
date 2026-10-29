package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewHTTPClientEmptyProxyUsesDirectEgress(t *testing.T) {
	targetHits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	proxyHits := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()

	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	client, err := newHTTPClient("")
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("direct request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("direct request status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if targetHits != 1 {
		t.Fatalf("target hits = %d, want 1", targetHits)
	}
	if proxyHits != 0 {
		t.Fatalf("proxy hits = %d, want 0", proxyHits)
	}
}

func TestNewHTTPClientExplicitProxyOverridesEnvironment(t *testing.T) {
	target := httptest.NewServer(http.NotFoundHandler())
	defer target.Close()

	proxyHits := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer proxy.Close()

	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	client, err := newHTTPClient(proxy.URL)
	if err != nil {
		t.Fatalf("newHTTPClient: %v", err)
	}
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("proxied request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("proxied request status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if proxyHits != 1 {
		t.Fatalf("proxy hits = %d, want 1", proxyHits)
	}
}

func TestNewHTTPClientRejectsInvalidExplicitProxy(t *testing.T) {
	if _, err := newHTTPClient("://invalid"); err == nil {
		t.Fatal("newHTTPClient accepted an invalid explicit proxy")
	}
}
