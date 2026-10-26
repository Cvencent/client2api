package openaicompat

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"client2api/internal/core"
)

// TestSourceRequestsGoThroughItsOwnProxy is the whole point of
// source.proxy: every request a relay makes -- model scan and chat here --
// must leave through the configured proxy, while the rest of the process keeps
// using the shared client.
func TestSourceRequestsGoThroughItsOwnProxy(t *testing.T) {
	var upstreamHits, proxyHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if strings.HasSuffix(r.URL.Path, "/models") {
			fmt.Fprint(w, `{"data":[{"id":"relay/model"}]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	// A plain-HTTP forward proxy: the client sends an absolute URI, which the
	// proxy hands to the upstream with a direct round trip.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		if r.URL.Scheme == "" || r.URL.Host == "" {
			http.Error(w, "expected an absolute proxy URI", http.StatusBadRequest)
			return
		}
		out := r.Clone(r.Context())
		out.RequestURI = ""
		resp, err := (&http.Transport{}).RoundTrip(out)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	c, err := newSource("relay", core.SourceConfig{BaseURL: upstream.URL + "/v1", Proxy: proxy.URL},
		core.Deps{DataDir: t.TempDir(), HTTPClient: upstream.Client()})
	if err != nil {
		t.Fatalf("newSource: %v", err)
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "k1", "models": "relay/model"}}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatalf("model scan through the proxy: %v", err)
	}
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "relay/model",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("chat through the proxy: %v", err)
	}
	if text, _, _, _, _, err := drainStream(stream); err != nil || text != "pong" {
		t.Fatalf("chat through the proxy: %q %v", text, err)
	}
	if proxyHits.Load() == 0 {
		t.Fatal("the relay's requests bypassed its configured proxy")
	}
	if upstreamHits.Load() == 0 {
		t.Fatal("the proxy never reached the upstream")
	}
	if got := c.httpClient(); got == nil || got == c.deps.HTTPClient {
		t.Fatal("httpClient() still hands out the shared client after a proxy was configured")
	}
}

// TestSourceProxyClearingRestoresTheSharedClient keeps the operator able to
// switch a relay back to the gateway-wide egress without restarting.
func TestSourceProxyClearingRestoresTheSharedClient(t *testing.T) {
	shared := &http.Client{}
	c, err := newSource("relay", core.SourceConfig{BaseURL: "https://relay.example/v1", Proxy: "http://127.0.0.1:7890"},
		core.Deps{DataDir: t.TempDir(), HTTPClient: shared})
	if err != nil {
		t.Fatalf("newSource: %v", err)
	}
	if c.httpClient() == shared {
		t.Fatal("configured proxy was ignored")
	}
	if err := c.ConfigureSource(core.SourceConfig{BaseURL: "https://relay.example/v1"}); err != nil {
		t.Fatalf("ConfigureSource: %v", err)
	}
	if c.httpClient() != shared {
		t.Fatal("clearing the proxy did not restore the shared client")
	}
}

// TestSourceProxyIsRevalidatedOnReload refuses a malformed proxy at reload
// time instead of silently falling back to a direct connection.
func TestSourceProxyIsRevalidatedOnReload(t *testing.T) {
	c, err := newSource("relay", core.SourceConfig{BaseURL: "https://relay.example/v1"},
		core.Deps{DataDir: t.TempDir(), HTTPClient: &http.Client{}})
	if err != nil {
		t.Fatalf("newSource: %v", err)
	}
	if err := c.ConfigureSource(core.SourceConfig{BaseURL: "https://relay.example/v1", Proxy: "socks4://127.0.0.1:1080"}); err == nil {
		t.Fatal("a malformed proxy was accepted on reload")
	}
}

// TestSourceUserAgentReachesTheWire pins the per-source UA: a relay that gates
// by client allowlist (agentrouter answers a generic UA with 401 "unauthorized
// client detected") needs the operator's UA on every request the module makes,
// and a source-level value must win over the module default.
func TestSourceUserAgentReachesTheWire(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("User-Agent"))
		mu.Unlock()
		fmt.Fprint(w, `{"data":[{"id":"demo"}]}`)
	}))
	defer srv.Close()

	c, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL, UserAgent: "codex_cli_rs/0.0.1 (client2api)"}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "demo-key", "models": "demo"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 {
		t.Fatal("no request reached the test server")
	}
	for _, ua := range seen {
		if ua != "codex_cli_rs/0.0.1 (client2api)" {
			t.Errorf("request went out with UA %q, want the configured source UA", ua)
		}
	}
}

// Without a configured UA the module keeps its own identity.
func TestSourceDefaultUserAgentUnchanged(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("User-Agent"))
		mu.Unlock()
		fmt.Fprint(w, `{"data":[{"id":"demo"}]}`)
	}))
	defer srv.Close()

	c, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "demo-key", "models": "demo"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, ua := range seen {
		if ua != "client2api/1.0 (+openai-compat)" {
			t.Errorf("request went out with UA %q, want the module default", ua)
		}
	}
}
