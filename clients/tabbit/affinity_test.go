package tabbit

// affinity_test.go — conversation→account stickiness (affinity.go).
//
// The fixtures are this module's own, reused rather than re-invented: newFakeWeb
// and newWebClient for the web transport, addEndpoint for the panel store, and
// roundTripFunc/sseResponse/sseFixture for the sidecar transport.

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// chatWithKey is the shape every test here sends: one user turn, scoped to a
// conversation when key is non-empty so the module has a stickiness key.
func chatWithKey(key string) *core.ChatRequest {
	req := &core.ChatRequest{
		Model:    "Default",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}
	if key != "" {
		req.Options = map[string]any{"conversation_id": key}
	}
	return req
}

// drainChat runs one completion to completion and returns the reply text.
func drainChat(ctx context.Context, c *Client, req *core.ChatRequest) (string, error) {
	st, err := c.Chat(ctx, req)
	if err != nil {
		return "", err
	}
	return drainTabbitStream(st)
}

func webSessionCookie(token string) string { return webTokenCookie + "=" + token }

// TestTabbitAffinityServesTheBoundWebSession is the end-to-end proof that the
// picker is wired: the store's first session is not the bound one, so a
// conversation that comes back served by the bound session can only have got
// there through the affinity table.
func TestTabbitAffinityServesTheBoundWebSession(t *testing.T) {
	ctx := context.Background()
	f := newFakeWeb(t)
	c, first := newWebClient(t, f)

	// A second browser session, added after the first.  Store order means the
	// existing picker would never choose it.
	secondToken := webJWTFor(t, "uid-web-0002", time.Now().Add(time.Hour))
	second := addEndpoint(t, c, map[string]string{"kind": kindWebToken, "token": secondToken})
	if second.ID != webAccountIDTag+"uid-web-0002" {
		t.Fatalf("second session id = %q, want %q", second.ID, webAccountIDTag+"uid-web-0002")
	}
	if ep, ok := c.firstEnabledWeb(); !ok || ep.ID != first.ID {
		t.Fatalf("the picker no longer prefers the first session: %#v", ep)
	}

	// Baseline: an unbound conversation follows store order and lands on the
	// first session, so the assertion below cannot pass for the wrong reason.
	if _, err := drainChat(ctx, c, chatWithKey("conv-other")); err != nil {
		t.Fatalf("unbound chat: %v", err)
	}
	if got := f.header().Get("Cookie"); strings.Contains(got, secondToken) {
		t.Fatalf("an unbound conversation was already served by the second session (Cookie=%q)", got)
	}

	c.BindConversation("conv-bound", second.ID)
	if _, err := drainChat(ctx, c, chatWithKey("conv-bound")); err != nil {
		t.Fatalf("bound chat: %v", err)
	}
	if got, want := f.header().Get("Cookie"), webSessionCookie(secondToken); got != want {
		t.Fatalf("the bound conversation was served with Cookie=%q, want %q (binding %s)", got, want, second.ID)
	}
}

// TestTabbitAffinityDropsAnUnusableBindingAndPicksNormally checks the safety
// property: a binding that has gone bad is never served, the request still
// succeeds, and the conversation is re-bound to whoever serves it now.
func TestTabbitAffinityDropsAnUnusableBindingAndPicksNormally(t *testing.T) {
	ctx := context.Background()
	f := newFakeWeb(t)
	c, first := newWebClient(t, f)

	secondToken := webJWTFor(t, "uid-web-0002", time.Now().Add(time.Hour))
	second := addEndpoint(t, c, map[string]string{"kind": kindWebToken, "token": secondToken})

	c.BindConversation("conv-dead", second.ID)
	if got, ok := c.ConversationAccount("conv-dead", "Default"); !ok || got != second.ID {
		t.Fatalf("ConversationAccount = %q,%v before disabling, want %q,true", got, ok, second.ID)
	}

	// The operator disables the session the conversation is pinned to.
	if err := c.SetAccountEnabled(ctx, second.ID, false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	if got, ok := c.ConversationAccount("conv-dead", "Default"); ok {
		t.Fatalf("a disabled session still resolved as a sticky binding (%q)", got)
	}

	if _, err := drainChat(ctx, c, chatWithKey("conv-dead")); err != nil {
		t.Fatalf("a stale binding must not fail the request: %v", err)
	}
	if got := f.header().Get("Cookie"); strings.Contains(got, secondToken) {
		t.Fatalf("the disabled session served the request (Cookie=%q)", got)
	}
	if got, ok := c.ConversationAccount("conv-dead", "Default"); !ok || got != first.ID {
		t.Fatalf("the conversation was not re-bound to %q: got %q,%v", first.ID, got, ok)
	}
}

// TestTabbitAffinityPinsTheSidecarEndpoint is the same proof for the transport
// most requests actually take: tabbit routes to the sidecar whenever no web
// session is stored, so the picker has to be wired there too.
func TestTabbitAffinityPinsTheSidecarEndpoint(t *testing.T) {
	ctx := context.Background()
	clearTabbitEnv(t)
	// webRoute() is consulted before the sidecar path; make sure the ambient
	// environment cannot put this test on the web transport.
	t.Setenv("CLIENT2API_TABBIT_WEB_TOKEN", "")
	t.Setenv("TABBIT_WEB_TOKEN", "")
	withCandidates(t, closedPortURL(t))

	var mu sync.Mutex
	var hosts []string
	hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		hosts = append(hosts, r.URL.Host)
		mu.Unlock()
		return sseResponse(sseFixture), nil
	})}
	c := newTestClient(t, "", hc)

	first := addEndpoint(t, c, map[string]string{"kind": kindSidecar, "base_url": "127.0.0.1:5111"})
	second := addEndpoint(t, c, map[string]string{"kind": kindSidecar, "base_url": "127.0.0.1:5222"})
	if loc := c.locate(); loc.source != epOriginPanel || loc.baseURL != first.ID {
		t.Fatalf("locate() = %q (%s), want the first stored endpoint %q", loc.baseURL, loc.source, first.ID)
	}
	if c.webRoute() {
		t.Fatal("this test needs the sidecar transport, but webRoute() is true")
	}

	lastHost := func() string {
		mu.Lock()
		defer mu.Unlock()
		if len(hosts) == 0 {
			return ""
		}
		return hosts[len(hosts)-1]
	}

	if _, err := drainChat(ctx, c, chatWithKey("conv-sidecar")); err != nil {
		t.Fatalf("unbound chat: %v", err)
	}
	if got := lastHost(); got != "127.0.0.1:5111" {
		t.Fatalf("an unbound conversation went to %q, want the first endpoint", got)
	}

	c.BindConversation("conv-sidecar", second.ID)
	if _, err := drainChat(ctx, c, chatWithKey("conv-sidecar")); err != nil {
		t.Fatalf("bound chat: %v", err)
	}
	if got := lastHost(); got != "127.0.0.1:5222" {
		t.Fatalf("the bound conversation went to %q, want %q", got, "127.0.0.1:5222")
	}
}

// TestTabbitAffinityUsesContentFallbackForAnUnscopedRequest pins the new
// default: a request with no explicit conversation id still derives a stable
// key from the first user turn and is served by the account that warmed it.
func TestTabbitAffinityUsesContentFallbackForAnUnscopedRequest(t *testing.T) {
	ctx := context.Background()
	f := newFakeWeb(t)
	c, _ := newWebClient(t, f)

	secondToken := webJWTFor(t, "uid-web-0002", time.Now().Add(time.Hour))
	second := addEndpoint(t, c, map[string]string{"kind": kindWebToken, "token": secondToken})
	c.BindConversation("conv-bound", second.ID)

	if _, err := drainChat(ctx, c, chatWithKey("")); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if got := f.header().Get("Cookie"); strings.Contains(got, secondToken) {
		t.Fatalf("an unscoped conversation was served by the manually bound second session (Cookie=%q)", got)
	}
	if n := c.affinity.Count(); n != 2 {
		t.Fatalf("the table holds %d bindings, want the explicit binding plus the content-derived one", n)
	}
}

func TestTabbitAffinityReportsAbsentForAnUnknownKey(t *testing.T) {
	c := tabbitPanelClient(t)
	if id, ok := c.ConversationAccount("never-seen", "Default"); ok || id != "" {
		t.Fatalf("ConversationAccount for an unknown key = %q,%v, want \"\",false", id, ok)
	}
}

func TestTabbitAffinityUnbindForgetsTheBinding(t *testing.T) {
	c := tabbitPanelClient(t)
	rec := addEndpoint(t, c, map[string]string{"base_url": "127.0.0.1:50124"})

	c.BindConversation("conv-unbind", rec.ID)
	if got, ok := c.ConversationAccount("conv-unbind", "Default"); !ok || got != rec.ID {
		t.Fatalf("ConversationAccount = %q,%v, want %q,true", got, ok, rec.ID)
	}
	if !c.UnbindConversation("conv-unbind") {
		t.Fatal("UnbindConversation reported nothing to forget")
	}
	if c.UnbindConversation("conv-unbind") {
		t.Fatal("UnbindConversation reported forgetting the same key twice")
	}
	if id, ok := c.ConversationAccount("conv-unbind", "Default"); ok || id != "" {
		t.Fatalf("after Unbind, ConversationAccount = %q,%v, want \"\",false", id, ok)
	}
}

func TestTabbitAffinityAdvertisesTheCapability(t *testing.T) {
	c := tabbitPanelClient(t)
	if caps := core.CapabilitiesOf(context.Background(), c); !caps.Conversations {
		t.Fatal("core.CapabilitiesOf does not report tabbit as a conversation binder")
	}
	var binder core.ConversationBinder = c
	if _, ok := binder.ConversationAccount("x", "Default"); ok {
		t.Fatal("an empty table must not resolve anything")
	}
}

// TestTabbitAffinityApplyLiveMovesTheStickinessWindow covers the live-reload
// half: tabbit has no other ApplyLive, so this method carries exactly the two
// stickiness settings.
func TestTabbitAffinityApplyLiveMovesTheStickinessWindow(t *testing.T) {
	c := newTestClient(t, "", nil)
	if got := c.affinity.TTL(); got != core.DefaultAffinityTTL {
		t.Fatalf("window = %s by default, want %s", got, core.DefaultAffinityTTL)
	}

	c.ApplyLive(core.LiveSettings{AffinityTTL: 3 * time.Hour, AffinityGCInterval: 20 * time.Minute})
	if got := c.affinity.TTL(); got != 3*time.Hour {
		t.Fatalf("window = %s after ApplyLive, want 3h", got)
	}
	if got := c.affinity.GCInterval(); got != 20*time.Minute {
		t.Fatalf("sweep interval = %s after ApplyLive, want 20m", got)
	}

	// A zero value means "the file said nothing" and must not clobber what is
	// already there, which would quietly disable the feature.
	c.ApplyLive(core.LiveSettings{})
	if got := c.affinity.TTL(); got != 3*time.Hour {
		t.Fatalf("an empty LiveSettings reset the window to %s", got)
	}
	if got := c.affinity.GCInterval(); got != 20*time.Minute {
		t.Fatalf("an empty LiveSettings reset the sweep interval to %s", got)
	}

	// Neither a Client built without New nor a nil one may panic.
	bare := &Client{}
	bare.ApplyLive(core.LiveSettings{AffinityTTL: time.Minute})
	bare.BindConversation("k", "a")
	if bare.UnbindConversation("k") {
		t.Error("a Client without New must not claim to have forgotten a binding")
	}

	var nilClient *Client
	nilClient.ApplyLive(core.LiveSettings{AffinityTTL: time.Minute})
	nilClient.BindConversation("k", "a")
	if nilClient.UnbindConversation("k") {
		t.Error("a nil Client must not claim to have forgotten a binding")
	}
	if _, ok := nilClient.ConversationAccount("k", "m"); ok {
		t.Error("a nil Client must not resolve a binding")
	}
}
