package openaicompat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestSourceRPMRejectsBeforeUpstreamAndRecoversAfterWindow(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	now := time.Unix(1_700_000_000, 0)
	c, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL, RequestsPerMinute: 1}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return now }
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "demo-key", "models": "demo"}}); err != nil {
		t.Fatal(err)
	}
	req := func() error {
		_, err := c.Chat(context.Background(), &core.ChatRequest{Model: "demo", Messages: []core.Message{{Role: "user", Content: "hi"}}})
		return err
	}
	if err := req(); err != nil {
		t.Fatal(err)
	}
	if err := req(); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("second request error = %v, want ErrBusy", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	now = now.Add(time.Minute + time.Nanosecond)
	if err := req(); err != nil {
		t.Fatalf("request after window: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls after window = %d, want 2", got)
	}
}

func TestSourceAccountRPMRotatesToAnotherKey(t *testing.T) {
	var keyA, keyB atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer key-a":
			keyA.Add(1)
		case "Bearer key-b":
			keyB.Add(1)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	now := time.Unix(1_700_000_000, 0)
	c, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL, RequestsPerMinutePerAccount: 1}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return now }
	for _, key := range []string{"key-a", "key-b"} {
		if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": key, "models": "demo"}}); err != nil {
			t.Fatal(err)
		}
	}
	req := func() error {
		_, err := c.Chat(context.Background(), &core.ChatRequest{Model: "demo", Messages: []core.Message{{Role: "user", Content: "hi"}}})
		return err
	}
	if err := req(); err != nil {
		t.Fatal(err)
	}
	if err := req(); err != nil {
		t.Fatalf("second request: %v", err)
	}
	if keyA.Load() != 1 || keyB.Load() != 1 {
		t.Fatalf("key usage = (%d, %d), want (1, 1)", keyA.Load(), keyB.Load())
	}
	if err := req(); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("third request error = %v, want ErrBusy", err)
	}
}

func TestSourceRPMIgnoresAccountSlotRefusal(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	now := time.Unix(1_700_000_000, 0)
	c, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL, RequestsPerMinute: 1}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return now }
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "demo-key", "models": "demo"}}); err != nil {
		t.Fatal(err)
	}
	refused := false
	req := &core.ChatRequest{Model: "demo", Messages: []core.Message{{Role: "user", Content: "hi"}}}
	req.SetAccountAcquirer(func(string) (func(), error) {
		if !refused {
			refused = true
			return nil, core.ErrBusy
		}
		return func() {}, nil
	})
	if _, err := c.Chat(context.Background(), req); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("refused account slot error = %v, want ErrBusy", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("upstream calls after local slot refusal = %d, want 0", got)
	}

	req = &core.ChatRequest{Model: "demo", Messages: []core.Message{{Role: "user", Content: "hi"}}}
	req.SetAccountAcquirer(func(string) (func(), error) { return func() {}, nil })
	stream, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("request after local slot refusal: %v", err)
	}
	_ = stream.Close()
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls after successful slot = %d, want 1", got)
	}
}

func TestSourceCooldownUsesConfiguredDuration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	now := time.Unix(1_700_000_000, 0)
	c, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL, RateLimitCooldown: "45s"}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return now }
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "demo-key", "models": "demo"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "demo", Messages: []core.Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Fatal("expected rate limit")
	}
	c.mu.Lock()
	until := c.cooling[c.rows[0].ID]
	c.mu.Unlock()
	if !until.Equal(now.Add(45 * time.Second)) {
		t.Fatalf("cooling until %s, want %s", until, now.Add(45*time.Second))
	}
}

func TestSourceGeneralCooldownFallsBackAndZeroDisables(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  core.SourceConfig
		want time.Duration
	}{
		{"general fallback", core.SourceConfig{Cooldown: "7s"}, 7 * time.Second},
		{"specific overrides general", core.SourceConfig{Cooldown: "7s", RateLimitCooldown: "9s"}, 9 * time.Second},
		{"zero disables", core.SourceConfig{Cooldown: "7s", RateLimitCooldown: "0s"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.CooldownFor(core.FailureRateLimited, time.Minute); got != tc.want {
				t.Fatalf("cooldown = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestSourceDailyAccountLimitRotatesAndReportsBusy(t *testing.T) {
	var keyA, keyB atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer key-a":
			keyA.Add(1)
		case "Bearer key-b":
			keyB.Add(1)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	now := time.Unix(1_700_000_000, 0)
	c, err := newSource("relay", core.SourceConfig{
		BaseURL: srv.URL,
		RequestLimits: []core.SourceRequestLimit{{
			Scope: "account", Limit: 1, Window: "24h", Reset: "rolling",
		}},
	}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return now }
	for _, key := range []string{"key-a", "key-b"} {
		if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": key, "models": "demo"}}); err != nil {
			t.Fatal(err)
		}
	}
	req := func() error {
		stream, err := c.Chat(context.Background(), &core.ChatRequest{Model: "demo", Messages: []core.Message{{Role: "user", Content: "hi"}}})
		if stream != nil {
			_ = stream.Close()
		}
		return err
	}
	if err := req(); err != nil {
		t.Fatal(err)
	}
	if err := req(); err != nil {
		t.Fatalf("second request should rotate to the other key: %v", err)
	}
	if keyA.Load() != 1 || keyB.Load() != 1 {
		t.Fatalf("key usage = (%d, %d), want (1, 1)", keyA.Load(), keyB.Load())
	}
	if err := req(); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("third request error = %v, want ErrBusy", err)
	}
}

func TestSourceSourceLimitReturnsBusy(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	now := time.Unix(1_700_000_000, 0)
	c, err := newSource("relay", core.SourceConfig{
		BaseURL: srv.URL,
		RequestLimits: []core.SourceRequestLimit{{
			Scope: "source", Limit: 1, Window: "24h", Reset: "rolling",
		}},
	}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return now }
	for _, key := range []string{"key-a", "key-b"} {
		if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": key, "models": "demo"}}); err != nil {
			t.Fatal(err)
		}
	}
	stream, err := c.Chat(context.Background(), &core.ChatRequest{Model: "demo", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "demo", Messages: []core.Message{{Role: "user", Content: "hi"}}}); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("second request error = %v, want ErrBusy", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestSourceRequestLimitStatusShowsRecovery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	now := time.Unix(1_700_000_000, 0)
	c, err := newSource("relay", core.SourceConfig{
		BaseURL: srv.URL,
		RequestLimits: []core.SourceRequestLimit{{
			Scope: "account", Limit: 1, Window: "24h", Reset: "rolling",
		}},
	}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return now }
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "key-a", "models": "demo"}}); err != nil {
		t.Fatal(err)
	}
	stream, err := c.Chat(context.Background(), &core.ChatRequest{Model: "demo", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()

	st := c.Status(context.Background())
	if len(st.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(st.Accounts))
	}
	acct := st.Accounts[0]
	if acct.State != "cooling" || acct.Note != "request limit reached" || acct.ExpiresAt == "" {
		t.Fatalf("account status = %+v", acct)
	}
	parks, _ := acct.Extra["model_cooldowns"].([]map[string]any)
	if len(parks) != 1 || parks[0]["kind"] != "request_limit" {
		t.Fatalf("model cooldowns = %#v", acct.Extra["model_cooldowns"])
	}
}

func TestSourceRequestLimitPersistsAcrossReload(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	cfg := core.SourceConfig{
		BaseURL: srv.URL,
		RequestLimits: []core.SourceRequestLimit{{
			Scope: "account", Limit: 1, Window: "24h", Reset: "rolling",
		}},
	}
	first, err := newSource("relay", cfg, core.Deps{DataDir: dir, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	first.now = func() time.Time { return now }
	if _, err := first.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "key-a", "models": "demo"}}); err != nil {
		t.Fatal(err)
	}
	stream, err := first.Chat(context.Background(), &core.ChatRequest{Model: "demo", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()

	second, err := newSource("relay", cfg, core.Deps{DataDir: dir, HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	second.now = func() time.Time { return now.Add(time.Second) }
	if _, err := second.Chat(context.Background(), &core.ChatRequest{Model: "demo", Messages: []core.Message{{Role: "user", Content: "hi"}}}); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("second source request error = %v, want ErrBusy", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}
