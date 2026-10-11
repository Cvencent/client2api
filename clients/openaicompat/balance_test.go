package openaicompat

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestSourceAccountBalanceReadsNewAPITokenUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/usage/token/" {
			t.Fatalf("request = %s %s, want GET /api/usage/token/", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key-a" {
			t.Fatalf("Authorization = %q, want the account key", got)
		}
		fmt.Fprint(w, `{"code":true,"message":"ok","data":{"object":"token_usage","total_granted":6000000,"total_used":1000000,"total_available":5000000,"unlimited_quota":false}}`)
	}))
	defer srv.Close()

	c, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL + "/v1"}, core.Deps{
		DataDir:    t.TempDir(),
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	acct, err := c.AddAccount(context.Background(), core.AccountSpec{
		Fields: map[string]string{"api_key": "key-a"},
	})
	if err != nil {
		t.Fatal(err)
	}

	bal, err := c.AccountBalance(context.Background(), acct.ID, 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 5_000_000 || bal.Total != 6_000_000 {
		t.Fatalf("balance = %+v, want raw New API quota units", bal)
	}
	if math.Abs(bal.Scale-1.0/500000.0) > 1e-12 {
		t.Fatalf("scale = %v, want one USD per 500000 quota units", bal.Scale)
	}
	if math.Abs(bal.Used-2) > 1e-9 {
		t.Fatalf("used = %v, want 2 USD", bal.Used)
	}
	if bal.Unit != "USD" {
		t.Fatalf("unit = %q, want USD", bal.Unit)
	}
}

func TestSourceBalanceUsesConfiguredQuotaPerUSD(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/usage/token/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":{"total_granted":6000000,"total_used":1000000,"total_available":5000000,"unlimited_quota":false}}`)
	}))
	defer srv.Close()

	c, err := newSource("relay", core.SourceConfig{
		BaseURL:            srv.URL + "/v1",
		BalanceProtocol:    "newapi",
		BalanceQuotaPerUSD: 1000000,
	}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	acct, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "key-a"}})
	if err != nil {
		t.Fatal(err)
	}
	bal, err := c.AccountBalance(context.Background(), acct.ID, 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if math.Abs(bal.Scale-1.0/1000000.0) > 1e-12 || math.Abs(bal.Used-1) > 1e-9 {
		t.Fatalf("balance = %+v, want the configured 1000000 quota/USD", bal)
	}
}

func TestSourceAccountFieldsExposeAdvancedBalanceCredentials(t *testing.T) {
	c, err := newSource("relay", core.SourceConfig{BaseURL: "https://relay.example/v1"}, core.Deps{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]core.FieldSpec{}
	for _, f := range c.AccountFields(context.Background()) {
		byKey[f.Key] = f
	}
	for _, key := range []string{"balance_access_token", "balance_user_id"} {
		f, ok := byKey[key]
		if !ok {
			t.Fatalf("source AccountFields missing %q", key)
		}
		if !f.Advanced {
			t.Errorf("source field %q is not advanced", key)
		}
		if strings.TrimSpace(f.Label) == "" {
			t.Errorf("source field %q has no useful label", key)
		}
	}
}

func TestSourceBalanceTreatsNegativeAvailableAsUnlimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/usage/token/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":{"total_granted":6000000,"total_used":1000000,"total_available":-1,"unlimited_quota":false}}`)
	}))
	defer srv.Close()

	c, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL + "/v1"}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	acct, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "key-a"}})
	if err != nil {
		t.Fatal(err)
	}
	bal, err := c.AccountBalance(context.Background(), acct.ID, 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if !bal.Unlimited || bal.Credits != 0 {
		t.Fatalf("balance = %+v, want negative available reported as unlimited", bal)
	}
}

func TestSourceBalanceReadsSub2APIUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/usage" {
			t.Fatalf("request = %s %s, want GET /v1/usage", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key-a" {
			t.Fatalf("Authorization = %q, want the account key", got)
		}
		fmt.Fprint(w, `{"remaining":5.50961497,"used":1.25,"total":6.75961497}`)
	}))
	defer srv.Close()

	c, err := newSource("relay", core.SourceConfig{
		BaseURL:         srv.URL + "/v1",
		BalanceProtocol: "sub2api",
	}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	acct, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "key-a"}})
	if err != nil {
		t.Fatal(err)
	}
	bal, err := c.AccountBalance(context.Background(), acct.ID, 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 5_509_615 || bal.Total != 6_759_615 || math.Abs(bal.Scale-1e-6) > 1e-15 {
		t.Fatalf("balance = %+v, want micro-USD Sub2API values", bal)
	}
	if math.Abs(bal.Used-1.25) > 1e-9 || bal.Unit != "USD" {
		t.Fatalf("used/unit = %v/%q, want 1.25/USD", bal.Used, bal.Unit)
	}
}

func TestSourceBalanceAutoFallsBackToConfiguredConsole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/usage/token/", "/v1/usage":
			http.NotFound(w, r)
		case "/api/user/self":
			if got := r.Header.Get("Authorization"); got != "Bearer console-token" {
				t.Fatalf("Authorization = %q, want the configured access token", got)
			}
			if got := r.Header.Get("New-API-User"); got != "user-42" {
				t.Fatalf("New-API-User = %q, want the configured user id", got)
			}
			fmt.Fprint(w, `{"data":{"quota":7500000,"used_quota":2500000}}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfg := core.SourceConfig{BaseURL: srv.URL + "/v1"}
	deps := core.Deps{DataDir: dir, HTTPClient: srv.Client()}
	c, err := newSource("relay", cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		"api_key":              "key-a",
		"balance_access_token": "console-token",
		"balance_user_id":      "user-42",
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Reopen the source to prove the console credentials survive the account
	// store round trip, not just the in-memory row that AddAccount just made.
	restored, err := newSource("relay", cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	bal, err := restored.AccountBalance(context.Background(), acct.ID, 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 7_500_000 || bal.Total != 10_000_000 || math.Abs(bal.Used-5) > 1e-9 {
		t.Fatalf("balance = %+v, want the New API console quota", bal)
	}
	if math.Abs(bal.Scale-1.0/500000.0) > 1e-12 || bal.Unit != "USD" {
		t.Fatalf("scale/unit = %v/%q, want 1/500000 USD", bal.Scale, bal.Unit)
	}
}

func TestSourceBalanceAutoFallsBackToOpenAIBillingAndIgnoresSentinel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dashboard/billing/credit_grants":
			fmt.Fprint(w, `{"total_granted":20,"total_used":5,"total_available":15}`)
		case "/dashboard/billing/subscription":
			fmt.Fprint(w, `{"hard_limit_usd":100000000}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := newSource("relay", core.SourceConfig{
		BaseURL: srv.URL + "/v1",
	}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	acct, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "key-a"}})
	if err != nil {
		t.Fatal(err)
	}
	bal, err := c.AccountBalance(context.Background(), acct.ID, 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 15_000_000 || bal.Total != 20_000_000 || math.Abs(bal.Scale-1e-6) > 1e-15 {
		t.Fatalf("balance = %+v, want OpenAI billing USD values without the sentinel", bal)
	}
	if math.Abs(bal.Used-5) > 1e-9 || bal.Unit != "USD" {
		t.Fatalf("used/unit = %v/%q, want 5/USD", bal.Used, bal.Unit)
	}
}

func TestSourceBalanceNoneDoesNotProbe(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c, err := newSource("relay", core.SourceConfig{
		BaseURL:         srv.URL + "/v1",
		BalanceProtocol: "none",
	}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	acct, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "key-a"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AccountBalance(context.Background(), acct.ID, 0); err == nil {
		t.Fatal("expected balance_protocol=none to reject the query")
	}
	if calls != 0 {
		t.Fatalf("balance_protocol=none made %d vendor call(s)", calls)
	}
}

func TestAccountBalanceHonorsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL + "/v1"}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	acct, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "key-a"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.AccountBalance(ctx, acct.ID, 0); err == nil {
		t.Fatal("a cancelled balance query returned success")
	}
}
