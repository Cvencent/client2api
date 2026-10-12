package doubao

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"client2api/internal/core"
)

// TestBalancePostsCreditProbeAndParsesTotalCredit pins the current vendor
// contract: the credit endpoint rejects GET, expects {"need_tasks":false}, and
// reports the spendable amount as data.credit_info.total_credit_num.
func TestBalancePostsCreditProbeAndParsesTotalCredit(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathCreditNum {
			http.NotFound(w, r)
			return
		}
		gotMethod = r.Method
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"data":{"credit_info":{"total_credit_num":120,"credit_text":"积分","credit_desc_text":"积分可用于视频生成等功能"}}}`))
	}))
	defer srv.Close()
	c := newWebClient(t, srv.URL)
	_, _ = c.store.add(&account{ID: "a1", Cookie: "sessionid=abc", Enabled: true})

	b, err := c.AccountBalance(context.Background(), "a1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != pathCreditNum {
		t.Fatalf("path = %q, want %q", gotPath, pathCreditNum)
	}
	if gotBody["need_tasks"] != false {
		t.Fatalf("body = %#v, want need_tasks=false", gotBody)
	}
	if b.Credits != 120 {
		t.Fatalf("Credits = %d, want 120", b.Credits)
	}
	if b.Unit != "积分" {
		t.Fatalf("Unit = %q, want 积分", b.Unit)
	}
}

// TestBalanceAddsTheMembershipQuotaLine keeps the credit balance as the
// primary number while surfacing the membership window the desktop client
// shows underneath it. The vendor reports percentages, so the gateway must
// relay the percentage and reset time rather than inventing an absolute quota.
func TestBalanceAddsTheMembershipQuotaLine(t *testing.T) {
	const summaryPath = "/alice/commerce/sale/subscription/quota/summary/"
	var summaryHits int
	var summaryBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathCreditNum:
			_, _ = w.Write([]byte(`{"data":{"credit_info":{"total_credit_num":120,"credit_text":"积分"}}}`))
		case summaryPath:
			summaryHits++
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &summaryBody)
			_, _ = w.Write([]byte(`{"data":{"window_limit_section":{"window_limit_groups":[{"window_limits":[{"window_type":1,"used_percent":0,"start_time":0,"end_time":0},{"window_type":2,"used_percent":15,"start_time":1791337226660,"end_time":1791942026660}]}]}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newWebClient(t, srv.URL)
	_, _ = c.store.add(&account{ID: "a1", Cookie: "sessionid=abc", Enabled: true})

	b, err := c.AccountBalance(context.Background(), "a1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if b.Credits != 120 || b.Unit != "积分" {
		t.Fatalf("primary balance = %+v, want 120 积分", b)
	}
	if summaryHits != 1 {
		t.Fatalf("quota summary hits = %d, want 1", summaryHits)
	}
	if summaryBody["product_line"] != "membership" {
		t.Fatalf("quota summary body = %#v, want product_line=membership", summaryBody)
	}
	if len(b.Lines) != 1 {
		t.Fatalf("balance lines = %+v, want one membership line", b.Lines)
	}
	line := b.Lines[0]
	if line.Label != "循环额度" {
		t.Fatalf("line label = %q, want 循环额度", line.Label)
	}
	if line.UsedPercent == nil || *line.UsedPercent != 15 {
		t.Fatalf("used percent = %v, want 15", line.UsedPercent)
	}
	if line.RemainingPercent == nil || *line.RemainingPercent != 85 {
		t.Fatalf("remaining percent = %v, want 85", line.RemainingPercent)
	}
	wantReset := time.UnixMilli(1791942026660).In(time.FixedZone("UTC+8", 8*60*60)).Format(time.RFC3339)
	if line.ResetsAt != wantReset {
		t.Fatalf("resets at = %q, want %q", line.ResetsAt, wantReset)
	}
}

// TestBalanceAddsFixedAndRollingMembershipLines pins the two package meters the
// desktop client can show: a fixed package balance and a rolling usage window.
// Each valid vendor window becomes its own line.
func TestBalanceAddsFixedAndRollingMembershipLines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case pathCreditNum:
			_, _ = w.Write([]byte(`{"data":{"credit_info":{"total_credit_num":120,"credit_text":"积分"}}}`))
		case pathSubscriptionQuotaSummary:
			_, _ = w.Write([]byte(`{"data":{"window_limit_section":{"window_limit_groups":[{"window_limits":[{"window_type":1,"used_percent":40,"start_time":0,"end_time":0},{"window_type":2,"used_percent":15,"start_time":1791337226660,"end_time":1791942026660}]}]}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := newWebClient(t, srv.URL)
	_, _ = c.store.add(&account{ID: "a1", Cookie: "sessionid=abc", Enabled: true})

	b, err := c.AccountBalance(context.Background(), "a1", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if len(b.Lines) != 2 {
		t.Fatalf("balance lines = %+v, want fixed and rolling lines", b.Lines)
	}
	if b.Lines[0].Label != "固定额度" || b.Lines[0].UsedPercent == nil || *b.Lines[0].UsedPercent != 40 {
		t.Fatalf("fixed line = %+v, want 40%% used", b.Lines[0])
	}
	if b.Lines[1].Label != "循环额度" || b.Lines[1].RemainingPercent == nil || *b.Lines[1].RemainingPercent != 85 {
		t.Fatalf("rolling line = %+v, want 85%% remaining", b.Lines[1])
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
	c.store.noteFailure("a1", false, defaultCooldown, nil)
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

// TestFailureCooldownFollowsTheConfiguredPolicy pins the two cooldown classes:
// a rejected session gets the long auth cooldown, a transient failure the
// ordinary one.
func TestFailureCooldownFollowsTheConfiguredPolicy(t *testing.T) {
	c := newTestClient(t)
	c.cfg.Cooldown = durationField("7s")
	c.cfg.AuthCooldown = durationField("9m")
	if got := c.failureCooldown(false); got != 7*time.Second {
		t.Fatalf("ordinary failure cooldown = %v, want 7s", got)
	}
	if got := c.failureCooldown(true); got != 9*time.Minute {
		t.Fatalf("auth failure cooldown = %v, want 9m", got)
	}
}

// TestAuthFailureCooldownExpiresToReady pins the recovery path: an auth
// rejection is an invalid verdict only for the configured cooldown, not a
// permanent dead end that requires a manual revive.
func TestAuthFailureCooldownExpiresToReady(t *testing.T) {
	c := newTestClient(t)
	_, _ = c.store.add(&account{ID: "a1", Cookie: "sessionid=abc", Enabled: true})
	c.store.noteFailure("a1", true, c.failureCooldown(true), nil)

	snap := c.store.snapshot()
	if got := snap[0].State; got != stateInvalid {
		t.Fatalf("state after auth failure = %q, want invalid", got)
	}
	if snap[0].CooldownUntil.IsZero() {
		t.Fatal("auth failure did not set a cooldown deadline")
	}
	recs, _ := c.Accounts(context.Background())
	if len(recs) != 1 || recs[0].State != stateInvalid || recs[0].ExpiresAt == "" {
		t.Fatalf("account record after auth failure = %+v, want invalid with expiry", recs)
	}
	st := c.Status(context.Background())
	if len(st.Accounts) != 1 || st.Accounts[0].State != stateInvalid || st.Accounts[0].ExpiresAt == "" {
		t.Fatalf("status after auth failure = %+v, want invalid with expiry", st.Accounts)
	}
	if _, err := c.store.pick(); err != core.ErrNotConfigured {
		t.Fatalf("pick during auth cooldown = %v, want ErrNotConfigured", err)
	}

	c.store.mu.Lock()
	c.store.accounts[0].CooldownUntil = time.Now().Add(-time.Second)
	c.store.mu.Unlock()

	snap = c.store.snapshot()
	if got := snap[0].State; got != stateReady {
		t.Fatalf("state after auth cooldown = %q, want ready", got)
	}
	picked, err := c.store.pick()
	if err != nil {
		t.Fatalf("pick after auth cooldown: %v", err)
	}
	if picked.ID != "a1" {
		t.Fatalf("picked %q after auth cooldown, want a1", picked.ID)
	}
	if h := c.Health(); !h.Servable || h.Ready != 1 {
		t.Fatalf("health after auth cooldown = %+v, want servable with 1 ready", h)
	}
}

// TestLoginTTLUsesConfigAndHasACap pins the web-login handoff bound.
func TestLoginTTLUsesConfigAndHasACap(t *testing.T) {
	c := newTestClient(t)
	c.cfg.LoginTimeoutSeconds = intPtr(90)
	if got := c.loginTTL(); got != 90*time.Second {
		t.Fatalf("loginTTL = %v, want 90s", got)
	}
	c.cfg.LoginTimeoutSeconds = intPtr(int(maxLoginSessionTTL/time.Second) + 1)
	if got := c.loginTTL(); got != maxLoginSessionTTL {
		t.Fatalf("loginTTL = %v, want capped at %v", got, maxLoginSessionTTL)
	}
}

func intPtr(v int) *int { return &v }

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

// TestRefreshModelsReturnsConfiguredCatalogue pins the catalogue override: the
// operator's `models` list is the catalogue when no account menu can be read.
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
