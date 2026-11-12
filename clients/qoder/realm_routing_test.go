package qoder

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

// This test answers the operator's question directly: with a healthy domestic
// account AND an international account whose model host is unreachable, does a
// bare-model request ever land on the international account?

// TestBareModelPrefersTheHealthyDomesticAccount: default_realm=cn, one healthy
// CN account, one global account whose transport cannot connect.  The preferred
// realm is domestic and its account works, so the request must be served there
// without touching the international account at all.
func TestBareModelPrefersTheHealthyDomesticAccount(t *testing.T) {
	var cnHits, globalHits int
	cnServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnHits++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(probeSSEFrame))
	}))
	defer cnServer.Close()

	cn := account{storedAccount: storedAccount{ID: "cn-1", Token: "dt-cn", UserID: "u-cn", Name: "cn", Enabled: true, Realm: realmCN}}
	global := account{storedAccount: storedAccount{ID: "g-1", Token: "dt-g", UserID: "u-g", Name: "g", Enabled: true, Realm: realmGlobal}}
	cfg := config{
		GatewayBase:       cnServer.URL,
		GlobalGatewayBase: cnServer.URL,
		// Deliberately unroutable: the international transport cannot connect.
		GlobalModelServer: "http://127.0.0.1:1",
		DefaultRealm:      realmCN,
	}
	c := &Client{
		cfg:      cfg,
		up:       newUpstream(cfg, cnServer.Client(), nil),
		store:    &store{all: []*account{&cn, &global}},
		sessions: map[string]sessionEntry{},
	}
	c.ownHTTP = cnServer.Client()
	c.httpOnce.Do(func() {})

	// A bare model name: the router does not know which realm is meant.
	req := &core.ChatRequest{
		Model:     "auto",
		Messages:  []core.Message{{Role: "user", Content: "ping"}},
		MaxTokens: intPtr(16),
	}
	stream, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat with a healthy cn account failed: %v", err)
	}
	stream.Close()

	if cnHits != 1 {
		t.Fatalf("the domestic account was called %d times, want exactly 1", cnHits)
	}
	if globalHits != 0 {
		t.Fatalf("the unreachable international transport was called %d times, want 0", globalHits)
	}
}

// TestBareModelFallsBackToTheOtherRealmWhenThePreferredOneHasNoAccount: no
// domestic account at all.  A bare name is a preference, so the international
// account is still tried -- and its transport failure must be a classification
// the account survives (cooldown, not a permanent park).
func TestBareModelFallsBackToTheOtherRealmWhenThePreferredOneHasNoAccount(t *testing.T) {
	global := account{storedAccount: storedAccount{ID: "g-1", Token: "dt-g", UserID: "u-g", Name: "g", Enabled: true, Realm: realmGlobal}}
	cfg := config{GlobalModelServer: "http://127.0.0.1:1", DefaultRealm: realmCN}
	c := &Client{
		cfg:      cfg,
		up:       newUpstream(cfg, nil, nil),
		store:    &store{all: []*account{&global}},
		sessions: map[string]sessionEntry{},
	}

	req := &core.ChatRequest{
		Model:     "auto",
		Messages:  []core.Message{{Role: "user", Content: "ping"}},
		MaxTokens: intPtr(16),
	}
	_, err := c.Chat(context.Background(), req)
	if err == nil {
		t.Fatal("Chat against an unreachable transport returned no error")
	}
	if !strings.Contains(err.Error(), "api2-v2.qoder.sh") && !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("error %q does not name the host it failed to reach", err)
	}

	// The failure must NOT have marked the account dead: a transport problem is
	// not a rejection, so the credential stays usable once the host is fixed.
	stored, _ := c.store.lookup("g-1")
	if stored.dead {
		t.Fatalf("a transport failure marked the account dead: %s", stored.lastError)
	}
	if stored.cooldownTill.IsZero() {
		t.Fatal("a transport failure did not start a cooldown; the pool would retry it on every request")
	}
	if failureKind(err) != core.FailureUpstream {
		t.Fatalf("failureKind = %v, want FailureUpstream (so the gateway may fail over, not treat it as auth)", failureKind(err))
	}
}

// TestExplicitDomesticModelNeverTouchesTheInternationalAccount: the hard case.
// The caller pinned "cn:", so the international account must not even be a
// candidate -- regardless of whether its transport works.
func TestExplicitDomesticModelNeverTouchesTheInternationalAccount(t *testing.T) {
	global := account{storedAccount: storedAccount{ID: "g-1", Token: "dt-g", UserID: "u-g", Enabled: true, Realm: realmGlobal}}
	c := &Client{
		cfg:      config{},
		up:       newUpstream(config{}, nil, nil),
		store:    &store{all: []*account{&global}},
		sessions: map[string]sessionEntry{},
	}
	got := c.orderedCandidatesFor(nil, "cn:auto", routeModel("cn:auto", realmCN))
	if len(got) != 0 {
		t.Fatalf("cn:-pinned request produced candidates %v, want none (the international account must not serve it)", ids(got))
	}
}

// TestDisablingTheInternationalAccountRemovesItFromThePool: setting an account
// off (the panel's 停用 switch) must take it out of the candidate set entirely,
// which is the most direct way to stop a request landing on it.
func TestDisablingTheInternationalAccountRemovesItFromThePool(t *testing.T) {
	global := account{storedAccount: storedAccount{ID: "g-1", Token: "dt-g", Enabled: true, Realm: realmGlobal}}
	c := &Client{
		cfg:      config{GlobalModelServer: "http://127.0.0.1:1"},
		up:       newUpstream(config{}, nil, nil),
		store:    &store{all: []*account{&global}},
		sessions: map[string]sessionEntry{},
	}

	// Enabled: an explicit global route finds it.
	if got := c.orderedCandidatesFor(nil, "global:auto", routeModel("global:auto", realmCN)); len(got) != 1 {
		t.Fatalf("enabled global account not offered: %v", ids(got))
	}

	if err := c.store.setEnabled("g-1", false); err != nil {
		t.Fatalf("setEnabled: %v", err)
	}

	// Disabled: gone from every candidate list, explicit or bare.
	if got := c.orderedCandidatesFor(nil, "global:auto", routeModel("global:auto", realmCN)); len(got) != 0 {
		t.Fatalf("a disabled account was still offered for an explicit global route: %v", ids(got))
	}
	if got := c.orderedCandidatesFor(nil, "auto", routeModel("auto", realmCN)); len(got) != 0 {
		t.Fatalf("a disabled account was still offered for a bare route: %v", ids(got))
	}
}
