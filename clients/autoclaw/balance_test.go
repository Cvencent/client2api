package autoclaw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAccountBalanceListsEachWallet pins the second-balance contract: AutoClaw
// reports a summed credit total plus a list of displayable wallets. The panel
// mirrors the desktop client by showing the total as the primary balance and
// each wallet as a line, so a non-credit wallet (a token grant) is surfaced
// when the vendor actually returns one. Hidden wallets never leak.
func TestAccountBalanceListsEachWallet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != pathWalletsV2 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"total_balance":1500,"wallets":[
		  {"public_wallet_type":"daily","display_name":"每日积分","balance":500,"display":true},
		  {"public_wallet_type":"token","display_name":"Token","balance":1000,"display":true},
		  {"public_wallet_type":"hidden","display_name":"Hidden","balance":9,"display":false}
		]}}`))
	}))
	defer srv.Close()

	p := newPool("", "", 0, cooldownConfig{}, nil)
	p.load([]account{{credential: credential{AccessToken: "tok-a", AccountID: "a1"}}})
	c := &Client{cfg: config{BaseURL: srv.URL}, httpc: srv.Client(), now: time.Now, pool: p}

	bal, err := c.AccountBalance(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 1500 || bal.Total != 1500 || bal.Unit != "积分" {
		t.Fatalf("primary balance = %+v, want total 1500 积分", bal)
	}
	got := map[string]int64{}
	for _, line := range bal.Lines {
		if line.Amount == nil {
			t.Fatalf("line %q has no amount", line.Label)
		}
		got[line.Label] = *line.Amount
	}
	if got["Token"] != 1000 {
		t.Fatalf("balance lines = %+v, want the Token wallet at 1000", bal.Lines)
	}
	if got["每日积分"] != 500 {
		t.Fatalf("balance lines = %+v, want the daily credits wallet at 500", bal.Lines)
	}
	if _, ok := got["Hidden"]; ok {
		t.Fatalf("a hidden wallet leaked into the balance lines: %+v", bal.Lines)
	}
}

// TestAccountBalanceRefreshesRejectedTokenOnce pins the fix for the "user not
// signed in" balance read: a stale access token must be renewed from the
// refresh token and the read retried once, exactly like the chat path does.
// Before the fix the panel only recovered after an unrelated test/chat call
// happened to renew the token as a side effect.
func TestAccountBalanceRefreshesRejectedTokenOnce(t *testing.T) {
	var (
		mu        sync.Mutex
		refreshes int
		v2Header  []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		auth := r.Header.Get("Authorization")
		switch r.URL.Path {
		case pathWalletsV2:
			mu.Lock()
			v2Header = append(v2Header, auth)
			mu.Unlock()
			if auth != "Bearer tok-b" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":400000,"msg":"user not logged in"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"total_balance":1500,"wallets":[
			  {"public_wallet_type":"daily","display_name":"每日积分","balance":1500,"display":true}
			]}}`))
		case pathRefresh:
			mu.Lock()
			refreshes++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"tok-b","refresh_token":"rt-b"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := newPool("", "", 0, cooldownConfig{}, nil)
	p.load([]account{{credential: credential{
		AccessToken:  "tok-a",
		RefreshToken: "rt-a",
		AccountID:    "a1",
	}}})

	c := &Client{cfg: config{BaseURL: srv.URL}, httpc: srv.Client(), now: time.Now, pool: p}

	bal, err := c.AccountBalance(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 1500 {
		t.Fatalf("credits = %d, want 1500 after the renewal retry", bal.Credits)
	}
	mu.Lock()
	defer mu.Unlock()
	if refreshes != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", refreshes)
	}
	if len(v2Header) != 2 {
		t.Fatalf("wallets reads = %d, want 2 (the rejected one plus the retry)", len(v2Header))
	}
	if v2Header[0] != "Bearer tok-a" || v2Header[1] != "Bearer tok-b" {
		t.Fatalf("wallet reads used %v, want the stale token then the refreshed one", v2Header)
	}
}

// TestAccountPackagesRefreshesRejectedTokenOnce covers the second credits
// entry point: the same stale token must not turn the package view into an
// "unauthenticated" error.
func TestAccountPackagesRefreshesRejectedTokenOnce(t *testing.T) {
	var refreshes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case pathWalletsV2:
			if r.Header.Get("Authorization") != "Bearer tok-b" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":400000,"msg":"user not logged in"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"total_balance":700,"wallets":[
			  {"public_wallet_type":"daily","display_name":"每日积分","balance":700,"display":true}
			]}}`))
		case pathRefresh:
			refreshes++
			_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"tok-b"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := newPool("", "", 0, cooldownConfig{}, nil)
	p.load([]account{{credential: credential{
		AccessToken:  "tok-a",
		RefreshToken: "rt-a",
		AccountID:    "a1",
	}}})
	c := &Client{cfg: config{BaseURL: srv.URL}, httpc: srv.Client(), now: time.Now, pool: p}

	rep, err := c.AccountPackages(context.Background(), "")
	if err != nil {
		t.Fatalf("AccountPackages: %v", err)
	}
	if rep.Remain != 700 {
		t.Fatalf("package remaining = %d, want 700 after the renewal retry", rep.Remain)
	}
	if refreshes != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", refreshes)
	}
}

// TestPanelRecordsPublishesTopLevelExpiry pins the renewal-sweep contract:
// cmd/client2api/renew.go only renews an account whose record carries a
// top-level expires_at. Publishing the expiry only inside fields["expires_in"]
// silently excluded every AutoClaw account from proactive renewal.
func TestPanelRecordsPublishesTopLevelExpiry(t *testing.T) {
	fixed := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	exp := fixed.Add(2 * time.Hour)

	p := newPool("", "", 0, cooldownConfig{}, nil)
	p.now = func() time.Time { return fixed }
	p.load([]account{{credential: credential{
		AccessToken: "tok-a",
		AccountID:   "a1",
		ExpiresAt:   flexString(strconv.FormatInt(exp.UnixMilli(), 10)),
	}}})

	recs := p.panelRecords()
	if len(recs) != 1 {
		t.Fatalf("panel records = %d, want 1", len(recs))
	}
	if strings.TrimSpace(recs[0].ExpiresAt) == "" {
		t.Fatal("the record announced no top-level expires_at, so the renewal sweep skips this account")
	}
	got, err := time.Parse(time.RFC3339, recs[0].ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q is not RFC3339: %v", recs[0].ExpiresAt, err)
	}
	if !got.Equal(exp) {
		t.Fatalf("expires_at = %s, want %s", got, exp)
	}
	if in, ok := recs[0].Fields["expires_in"].(int64); !ok || in != 7200 {
		t.Fatalf("expires_in = %v, want 7200 seconds", recs[0].Fields["expires_in"])
	}
}
