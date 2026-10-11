package doubao

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"client2api/internal/core"
)

// TestBalanceParsesCreditsReply pins the credit -> Balance mapping.
func TestBalanceParsesCreditsReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"credit_num":120,"credit_total":200,"credit_usage_num":80}}`))
	}))
	defer srv.Close()
	c := newWebClient(t, srv.URL)
	_, _ = c.store.add(&account{ID: "a1", Cookie: "sessionid=abc", Enabled: true})

	b, err := c.AccountBalance(context.Background(), "a1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if b.Credits != 120 {
		t.Fatalf("Credits = %d, want 120", b.Credits)
	}
	if b.Total != 200 {
		t.Fatalf("Total = %d, want 200", b.Total)
	}
	if b.Used != 80 {
		t.Fatalf("Used = %v, want 80", b.Used)
	}
	if b.Unit == "" {
		t.Fatalf("Unit is empty; the panel would show a bare number")
	}
}

// TestHealthEmptyPoolIsNotServable pins the empty-pool report.
func TestHealthEmptyPoolIsNotServable(t *testing.T) {
	c := newTestClient(t)
	h := c.Health()
	if h.Servable {
		t.Fatalf("empty pool reported servable")
	}
	if h.Note == "" {
		t.Fatalf("empty pool reported no reason")
	}
	if s := c.PoolStats(); s.InFlight != 0 {
		t.Fatalf("InFlight = %d, want 0", s.InFlight)
	}
}

// TestHealthReadyPoolIsServable pins the ready report.
func TestHealthReadyPoolIsServable(t *testing.T) {
	c := newTestClient(t)
	_, _ = c.store.add(&account{ID: "a1", Cookie: "sessionid=abc", Enabled: true})
	h := c.Health()
	if !h.Servable || h.Ready != 1 {
		t.Fatalf("health = %+v, want servable with 1 ready", h)
	}
}

// TestReviveClearsCooldown pins the operator's "try it again" hook.
func TestReviveClearsCooldown(t *testing.T) {
	c := newTestClient(t)
	_, _ = c.store.add(&account{ID: "a1", Cookie: "sessionid=abc", Enabled: true})
	c.store.noteFailure("a1", false, nil)
	if got := c.store.snapshot()[0].State; got != stateCooling {
		t.Fatalf("state after failure = %q, want cooling", got)
	}
	if err := c.ReviveAccount(context.Background(), "a1"); err != nil {
		t.Fatalf("ReviveAccount: %v", err)
	}
	if got := c.store.snapshot()[0].State; got != stateReady {
		t.Fatalf("state after revive = %q, want ready", got)
	}
}

// TestStatusNeverLeaksCookie pins the redaction rule: Status carries a label,
// never the credential.
func TestStatusNeverLeaksCookie(t *testing.T) {
	c := newTestClient(t)
	_, _ = c.store.add(&account{ID: "a1", Label: "mine", Cookie: "sessionid=supersecret", Enabled: true})
	st := c.Status(context.Background())
	for _, a := range st.Accounts {
		if a.State == "" {
			t.Fatalf("account %q has no state", a.ID)
		}
	}
	if st.Ready != true {
		t.Fatalf("Status.Ready = %v, want true with one enabled account", st.Ready)
	}
	if contains(st.Detail, "supersecret") {
		t.Fatalf("Status.Detail leaked the cookie: %q", st.Detail)
	}
}

// TestConversationAccountReportsOnlyUsable pins stickiness: a binding to a
// disabled account reports absent.
func TestConversationAccountReportsOnlyUsable(t *testing.T) {
	c := newTestClient(t)
	_, _ = c.store.add(&account{ID: "a1", Cookie: "sessionid=abc", Enabled: true})
	c.BindConversation("conv", "a1")
	if id, ok := c.ConversationAccount("conv", "doubao-seed-2.1-pro"); !ok || id != "a1" {
		t.Fatalf("bound account = %q,%v, want a1,true", id, ok)
	}
	_ = c.SetAccountEnabled(context.Background(), "a1", false)
	if _, ok := c.ConversationAccount("conv", "doubao-seed-2.1-pro"); ok {
		t.Fatalf("a disabled account must not be reported as a usable binding")
	}
}

// TestRefreshModelsReturnsConfiguredCatalogue pins the catalogue override.
func TestRefreshModelsReturnsConfiguredCatalogue(t *testing.T) {
	raw, err := New(core.Deps{DataDir: t.TempDir(), Config: []byte(`{"models":["doubao-seed-2.1-pro","my-model","my-model"]}`)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := raw.(*Client)
	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2 (duplicates removed)", len(models))
	}
	if models[1].ID != "my-model" {
		t.Fatalf("models[1] = %q, want my-model", models[1].ID)
	}
}
