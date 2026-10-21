package gateway

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// switchRaceClient holds its first Chat call after the request has entered the
// platform.  That gives the test a deterministic window in which an operator
// can force-switch the conversation before the old request has opened its
// stream and written a platform binding.
type switchRaceClient struct {
	name    string
	started chan struct{}
	release chan struct{}
	once    sync.Once
	calls   int
}

func (c *switchRaceClient) Name() string { return c.name }

func (c *switchRaceClient) Models(context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "m1", OwnedBy: c.name}}, nil
}

func (c *switchRaceClient) Chat(context.Context, *core.ChatRequest) (core.Stream, error) {
	c.calls++
	c.once.Do(func() { close(c.started) })
	<-c.release
	return &fakeStream{events: usageEvents(1, 1)}, nil
}

func (c *switchRaceClient) Status(context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

// TestForceSwitchDuringInFlightTurnDoesNotRebindTheOldPlatform reproduces the
// production race: the old request has already selected a platform, the
// operator force-switches to automatic routing, and only then does the old
// request receive its first frame.  That late success must not overwrite the
// decision; the next turn has to rank the remaining platforms normally.
func TestForceSwitchDuringInFlightTurnDoesNotRebindTheOldPlatform(t *testing.T) {
	const key = "conv-force-race"
	platforms := NewSessionPlatforms()
	old := &switchRaceClient{name: "opencode", started: make(chan struct{}), release: make(chan struct{})}
	preferred := &testClient{name: "preferred", events: usageEvents(1, 1)}

	reg := core.NewRegistry()
	reg.Add(old)
	reg.Add(preferred)
	reg.SetPlatformConfigs(map[string]core.PlatformConfig{
		"opencode":  {Priority: 10},
		"preferred": {Priority: -10},
	})
	srv := NewServer(Options{
		Registry:         reg,
		Version:          "test",
		Logger:           log.New(io.Discard, "", 0),
		Stats:            NewStats(),
		Usage:            NewUsageStore(4),
		SessionPlatforms: platforms,
	})

	// Start the conversation on the old platform, but model the switch before
	// that request has reached its first upstream frame.
	if !platforms.Pin(key, "opencode", "m1") {
		t.Fatal("Pin reported no change")
	}
	body := `{"model":"m1","messages":[{"role":"user","content":"hi"}],"metadata":{"conversation_id":"` + key + `"}}`
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		first <- rec
	}()

	select {
	case <-old.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first request never reached the old platform")
	}
	pinned, manual := platforms.pinFor(key)
	if !manual || pinned != "opencode" {
		t.Fatalf("before switch pin = %q/%v, want opencode/true", pinned, manual)
	}
	platforms.Forget(key)
	close(old.release)
	if rec := <-first; rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, body %s", rec.Code, rec.Body.String())
	}

	if rows := platforms.List(); len(rows) != 0 {
		t.Fatalf("the in-flight success recreated the switched platform: %#v", rows)
	}

	second := chat(t, srv, body)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body %s", second.Code, second.Body.String())
	}
	if preferred.chatCalls != 1 {
		t.Fatalf("preferred platform calls = %d, want 1 after re-ranking", preferred.chatCalls)
	}
	if old.calls != 1 {
		t.Fatalf("old platform calls = %d, want only the pre-switch request", old.calls)
	}
}
