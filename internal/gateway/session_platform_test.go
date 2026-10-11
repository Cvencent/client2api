package gateway

import (
	"errors"
	"io"
	"log"
	"net/http"
	"testing"

	"client2api/internal/core"
)

func stickyTestServer(alpha, beta core.Client) *http.Server {
	return stickyTestServerWithPlatforms(alpha, beta, nil)
}

func stickyTestServerWithPlatforms(alpha, beta core.Client, platforms *SessionPlatforms) *http.Server {
	reg := core.NewRegistry()
	reg.Add(alpha)
	reg.Add(beta)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"alpha": {Priority: 1},
		"beta":  {Priority: 2},
	})
	return NewServer(Options{
		Registry:         reg,
		Version:          "test",
		Logger:           log.New(io.Discard, "", 0),
		Stats:            NewStats(),
		Usage:            NewUsageStore(4),
		SessionPlatforms: platforms,
	})
}

func stickyChat(t *testing.T, srv *http.Server, body string) {
	t.Helper()
	rec := chat(t, srv, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
}

// TestStickyPlatformKeepsTheLastHealthyPlatform pins the operator's rule: after
// a failover succeeded on beta, the next turn must go straight to beta instead
// of trying the higher-priority but broken alpha first.
func TestStickyPlatformKeepsTheLastHealthyPlatform(t *testing.T) {
	alpha := &testClient{name: "alpha", chatErr: core.Fail("alpha", "a1", core.FailureUpstream, http.StatusBadGateway, errors.New("boom"))}
	beta := &testClient{name: "beta", events: usageEvents(1, 1)}
	srv := stickyTestServer(alpha, beta)
	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-keep"}}`

	// First turn: alpha fails, beta succeeds, and beta becomes the binding.
	stickyChat(t, srv, body)
	if beta.seen == nil {
		t.Fatal("failover did not reach beta")
	}

	// Second turn: beta must be tried first.  alpha is still priority 1, so a
	// missing platform binding shows up as alpha being called again.
	alpha.seen = nil
	beta.seen = nil
	stickyChat(t, srv, body)
	if alpha.seen != nil {
		t.Fatal("the broken higher-priority platform was retried before the healthy sticky platform")
	}
	if beta.seen == nil {
		t.Fatal("the conversation did not stay on its last healthy platform")
	}
}

// TestStickyPlatformManagerIsSharedWithThePanel pins the injection point the
// force-switch action depends on: the panel must be able to forget a route and
// the next chat must re-rank the platforms from the beginning.
func TestStickyPlatformManagerIsSharedWithThePanel(t *testing.T) {
	platforms := NewSessionPlatforms()
	alpha := &testClient{name: "alpha", chatErr: core.Fail("alpha", "a1", core.FailureUpstream, http.StatusBadGateway, errors.New("boom"))}
	beta := &testClient{name: "beta", events: usageEvents(1, 1)}
	srv := stickyTestServerWithPlatforms(alpha, beta, platforms)
	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-switch"}}`

	stickyChat(t, srv, body)
	if rows := platforms.List(); len(rows) != 1 || rows[0].Platform != "beta" {
		t.Fatalf("routes = %#v, want the beta binding", rows)
	}
	if !platforms.Forget("conv-switch") {
		t.Fatal("Forget did not clear the injected route")
	}

	alpha.seen = nil
	beta.seen = nil
	stickyChat(t, srv, body)
	if alpha.seen == nil {
		t.Fatal("the next chat skipped alpha instead of re-ranking after the platform binding was cleared")
	}
}
