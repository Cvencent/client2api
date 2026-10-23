package raccoon

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

// TestAccountBalanceRefreshesRejectedTokenOnce pins the fix for the "user not
// signed in" balance read: a stale access token must be renewed from the
// refresh token and the read retried once, exactly like the chat/test path
// does. Before the fix the panel only recovered after an unrelated test or
// chat call happened to renew the token as a side effect.
func TestAccountBalanceRefreshesRejectedTokenOnce(t *testing.T) {
	var (
		mu        sync.Mutex
		refreshes int
		headers   []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		auth := r.Header.Get("Authorization")
		switch r.URL.Path {
		case pathBalance:
			mu.Lock()
			headers = append(headers, auth)
			mu.Unlock()
			if auth != "Bearer tok-b" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":200001,"message":"user not logged in"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"available_points":1500}}`))
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
		UserID:       "u-1",
	}}})
	c := &Client{cfg: config{BaseURL: srv.URL}, httpc: srv.Client(), now: time.Now, pool: p}

	bal, err := c.AccountBalance(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if bal.Credits != 1500 || bal.Total != 1500 || bal.Unit != "积分" {
		t.Fatalf("balance = %+v, want 1500 积分 after the renewal retry", bal)
	}
	mu.Lock()
	defer mu.Unlock()
	if refreshes != 1 {
		t.Fatalf("refresh calls = %d, want exactly 1", refreshes)
	}
	if len(headers) != 2 {
		t.Fatalf("balance reads = %d, want 2 (the rejected one plus the retry)", len(headers))
	}
	if headers[0] != "Bearer tok-a" || headers[1] != "Bearer tok-b" {
		t.Fatalf("balance reads used %v, want the stale token then the refreshed one", headers)
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
		case pathBalance:
			if r.Header.Get("Authorization") != "Bearer tok-b" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"code":200001,"message":"user not logged in"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"available_points":700}}`))
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
		UserID:       "u-1",
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
// silently excluded every Raccoon account from proactive renewal, so a token
// expired while idle and the panel reported "user not signed in" until an
// unrelated test/chat renewed it by hand.
func TestPanelRecordsPublishesTopLevelExpiry(t *testing.T) {
	fixed := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	exp := fixed.Add(2 * time.Hour)

	p := newPool("", "", 0, cooldownConfig{}, nil)
	p.now = func() time.Time { return fixed }
	p.load([]account{{credential: credential{
		AccessToken: "tok-a",
		UserID:      "u-1",
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
