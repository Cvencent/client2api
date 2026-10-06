package openaicompat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

func TestQuickConnectTargetsAdvertiseOmniRoute(t *testing.T) {
	c := newTestClient(t, t.TempDir())
	targets := c.QuickConnectTargets(context.Background())
	if len(targets) != 1 {
		t.Fatalf("targets = %d, want 1", len(targets))
	}
	got := targets[0]
	if got.ID != omniRouteID || got.BaseURL != omniRouteBase {
		t.Fatalf("target = %+v", got)
	}
	// OmniRoute answers keyless on loopback, so the card must not force a key.
	if !got.KeyOptional {
		t.Error("OmniRoute's key is optional on loopback; the target says otherwise")
	}
	if got.Console == "" {
		t.Error("the card needs a console link so the operator can mint a key")
	}
	if got.Install == "" {
		t.Error("the card needs an install hint for the probe-failed case")
	}
}

// TestQuickConnectProbeSeparatesUpFromUnreachable pins the contract the panel
// depends on: any HTTP answer means "running" (including a 401 from a
// locked-down install), and only a transport failure means "not running".
func TestQuickConnectProbeSeparatesUpFromUnreachable(t *testing.T) {
	c := newTestClient(t, t.TempDir())

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer up.Close()
	if got := c.probeQuickBase(context.Background(), omniRouteID, up.URL); !got.Running {
		t.Fatalf("probe of a live server = %+v, want running", got)
	}

	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	down := c.probeQuickBase(context.Background(), omniRouteID, deadURL)
	if down.Running {
		t.Fatalf("probe of a closed server = %+v, want not running", down)
	}
	if strings.TrimSpace(down.Detail) == "" {
		t.Error("a failed probe must explain itself")
	}
}

func TestQuickConnectProbeRejectsUnknownTarget(t *testing.T) {
	c := newTestClient(t, t.TempDir())
	if _, err := c.ProbeQuickConnect(context.Background(), "nope"); err == nil {
		t.Fatal("an unknown target id must be an error, not a silent miss")
	}
	if _, err := c.ConnectQuickConnect(context.Background(), "nope", nil); err == nil {
		t.Fatal("connect must refuse an unknown target id")
	}
}

// TestConnectQuickConnectCreatesAKeylessLoopbackProvider covers the whole
// point of the button: one call, no operator-supplied base URL, no API key,
// and the result is an ordinary panel-owned row that survives a reload.
func TestConnectQuickConnectCreatesAKeylessLoopbackProvider(t *testing.T) {
	dir := t.TempDir()
	c := newTestClient(t, dir)

	rec, err := c.ConnectQuickConnect(context.Background(), omniRouteID, nil)
	if err != nil {
		t.Fatalf("ConnectQuickConnect: %v", err)
	}
	if rec.ID != omniRouteID {
		t.Fatalf("record = %+v, want the omniroute row", rec)
	}

	prov, model, ok := c.providerFor(omniRouteID + "/auto")
	if !ok {
		t.Fatal("omniroute/auto did not resolve after connecting")
	}
	if model != "auto" {
		t.Fatalf("model = %q, want auto", model)
	}
	if prov.APIKey != "" {
		t.Fatalf("keyless connect stored a key: %q", prov.APIKey)
	}
	if prov.base() != omniRouteBase {
		t.Fatalf("base URL = %q, want the documented default %q", prov.base(), omniRouteBase)
	}
	if len(prov.Models) != 1 || prov.Models[0] != "auto" {
		t.Fatalf("models = %v, want the seeded [auto]", prov.Models)
	}

	// The row is panel-owned, so a fresh client over the same data dir must
	// reload it without a second write.
	c2 := newTestClient(t, dir)
	if _, _, ok := c2.providerFor(omniRouteID + "/auto"); !ok {
		t.Fatal("the keyless provider was not persisted")
	}
}

// TestKeylessLoopbackProviderSendsNoAuthorization is the end-to-end half of
// the keyless rule: not just "stored without a key" but "reached the upstream
// with no Authorization header at all".  A bare "Bearer " would make some
// servers reject the request outright.
func TestKeylessLoopbackProviderSendsNoAuthorization(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	raw, err := New(core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := raw.(*Client)
	c.ensure()
	if _, err := c.ConnectQuickConnect(context.Background(), omniRouteID,
		map[string]string{"base_url": srv.URL}); err != nil {
		t.Fatalf("ConnectQuickConnect: %v", err)
	}

	res, err := c.TestAccount(context.Background(), omniRouteID)
	if err != nil {
		t.Fatalf("TestAccount: %v", err)
	}
	if !res.OK {
		t.Fatalf("TestAccount = %+v, want a passing probe", res)
	}
	if sawAuth != "" {
		t.Fatalf("keyless provider sent Authorization %q, want none", sawAuth)
	}
}

// TestAddAccountStillRequiresAKeyElsewhere guards the narrow scope of the
// keyless rule: a public provider with no key is still a mistake.
func TestAddAccountStillRequiresAKeyElsewhere(t *testing.T) {
	c := newTestClient(t, t.TempDir())
	_, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{
			"provider": "groq",
			"api_key":  "",
			"base_url": "https://api.groq.com/openai/v1",
		},
	})
	if err == nil {
		t.Fatal("a remote provider with no key must be refused")
	}
}

func TestIsLoopbackBase(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"http://127.0.0.1:20128/v1", true},
		{"http://localhost:20128/v1", true},
		{"http://LocalHost:11434/v1", true},
		{"http://[::1]:8080/v1", true},
		{"https://api.groq.com/openai/v1", false},
		{"http://192.168.1.10:20128/v1", false},
		{"", false},
		{"groq", false},
	} {
		if got := isLoopbackBase(tc.raw); got != tc.want {
			t.Errorf("isLoopbackBase(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func TestApplyHeadersSkipsAuthorizationWhenKeyless(t *testing.T) {
	keyless := ProviderConfig{ID: omniRouteID, BaseURL: omniRouteBase}
	req, err := http.NewRequest(http.MethodGet, keyless.base()+"/models", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	keyless.applyHeaders(req)
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("keyless request sent Authorization %q, want none", got)
	}

	keyed := ProviderConfig{ID: "groq", BaseURL: "https://api.groq.com/openai/v1", APIKey: "sk-test"}
	req2, err := http.NewRequest(http.MethodGet, keyed.base()+"/models", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	keyed.applyHeaders(req2)
	if got := req2.Header.Get("Authorization"); got != "Bearer sk-test" {
		t.Fatalf("Authorization = %q, want the bearer key", got)
	}
}
