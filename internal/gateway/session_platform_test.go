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
	if rows := platforms.List(); len(rows) != 1 || rows[0].Platform != "beta" || rows[0].Model != "m1" {
		t.Fatalf("routes = %#v, want the beta binding carrying m1", rows)
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

// TestManualPinDoesNotFailOverToAnotherPlatform pins the force-switch
// contract.  The panel's targeted switch writes a manual binding, and the
// dialog promises the conversation then stays on that platform.  A failure on
// the chosen platform must therefore be reported, not silently answered by the
// platform the operator was switching away from.
func TestManualPinDoesNotFailOverToAnotherPlatform(t *testing.T) {
	platforms := NewSessionPlatforms()
	alpha := &testClient{name: "alpha", servedBy: "a1", events: usageEvents(1, 1)}
	beta := &testClient{
		name:    "beta",
		chatErr: core.Fail("beta", "b1", core.FailureUpstream, http.StatusBadGateway, errors.New("boom")),
	}
	srv := stickyTestServerWithPlatforms(alpha, beta, platforms)
	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-force"}}`

	if !platforms.Pin("conv-force", "beta", "m1") {
		t.Fatal("Pin reported no change")
	}

	rec := chat(t, srv, body)
	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, body %s: the old platform answered a force-switched conversation",
			rec.Code, rec.Body.String())
	}
	if beta.seen == nil {
		t.Fatal("the pinned platform was never tried")
	}
	if alpha.seen != nil {
		t.Fatal("the conversation fell back to the platform the operator switched away from")
	}
}

// TestManualPinKeepsTheConversationOnTheChosenPlatform is the happy half of the
// same contract: while both platforms are healthy, the operator's choice wins
// over platform priority and the sibling is never called.
func TestManualPinKeepsTheConversationOnTheChosenPlatform(t *testing.T) {
	platforms := NewSessionPlatforms()
	alpha := &testClient{name: "alpha", servedBy: "a1", events: usageEvents(1, 1)}
	beta := &testClient{name: "beta", servedBy: "b1", events: usageEvents(1, 1)}
	srv := stickyTestServerWithPlatforms(alpha, beta, platforms)
	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"conv-force-ok"}}`

	if !platforms.Pin("conv-force-ok", "beta", "m1") {
		t.Fatal("Pin reported no change")
	}
	stickyChat(t, srv, body)

	if beta.seen == nil {
		t.Fatal("the pinned platform did not serve the conversation")
	}
	if alpha.seen != nil {
		t.Fatal("a platform the operator switched away from served the conversation")
	}
}
