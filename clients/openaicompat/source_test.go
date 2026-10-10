package openaicompat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestSourceScanPoolAndPersistence(t *testing.T) {
	var fail atomic.Bool
	var used atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		if r.URL.Path == "/v1/models" {
			if fail.Load() {
				http.Error(w, "temporary", 503)
				return
			}
			if key == "Bearer key-a" {
				fmt.Fprint(w, `{"data":[{"id":"vendor/model"},{"id":"only-a"}]}`)
			} else {
				fmt.Fprint(w, `{"data":[{"id":"vendor/model"},{"id":"only-b"}]}`)
			}
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		used.Store(key)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "vendor/model" && body["model"] != "only-b" {
			t.Errorf("wrong upstream model: %v", body["model"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	deps := core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()}
	cfg := core.SourceConfig{Label: "Custom", BaseURL: srv.URL + "/v1"}
	c, err := newSource("my-relay", cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "key-a"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "key-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatal("two keys replaced the same account")
	}
	models, err := c.RefreshModels(context.Background())
	if err != nil || len(models) != 3 {
		t.Fatalf("scan: %v %v", models, err)
	}
	fail.Store(true)
	models, err = c.RefreshModels(context.Background())
	if err == nil || len(models) != 3 {
		t.Fatalf("lost last-good catalogue: %v %v", models, err)
	}
	restored, err := newSource("my-relay", cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	models, _ = restored.Models(context.Background())
	if len(models) != 3 {
		t.Fatalf("catalogue not persistent: %v", models)
	}
	core.SetAccountPriorities(map[string]map[string]int{"my-relay": {a.ID: 10, b.ID: -5}})
	defer core.SetAccountPriorities(nil)
	for _, model := range []string{"vendor/model", "only-b"} {
		req := &core.ChatRequest{Model: model, Messages: []core.Message{{Role: "user", Content: "Hi"}}, Stream: true}
		stream, err := c.Chat(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		text, _, _, _, _, err := drainStream(stream)
		if err != nil || text != "pong" {
			t.Fatalf("chat: %q %v", text, err)
		}
		if used.Load() != "Bearer key-b" {
			t.Fatalf("priority/eligibility used wrong key: %v", used.Load())
		}
	}
	if err := c.ConfigureSource(core.SourceConfig{BaseURL: srv.URL + "/other"}); err != nil {
		t.Fatal(err)
	}
	models, _ = c.Models(context.Background())
	if len(models) != 0 {
		t.Fatal("URL edit retained old model catalogue")
	}
}

func TestSourceSuccessfulStreamDoesNotRecordEOFAsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	c, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "demo-key", "models": "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := c.Chat(context.Background(), &core.ChatRequest{Model: "demo", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, err := drainStream(stream); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(context.Background()); st.Detail != "" {
		t.Fatalf("normal EOF is a failure: %s", st.Detail)
	}
}

func TestSourceReloadRefusesCorruptAccountStore(t *testing.T) {
	dir := t.TempDir()
	cfg := core.SourceConfig{BaseURL: "https://relay.example/v1"}
	c, err := newSource("relay", cfg, core.Deps{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "demo-key", "models": "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "accounts.json"), []byte(`broken`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.ConfigureSource(cfg); err == nil {
		t.Fatal("corrupt account store silently erased the pool")
	}
	models, _ := c.Models(context.Background())
	if len(models) != 1 {
		t.Fatal("failed reload lost the last working state")
	}
}

func TestSourceModelScansRunConcurrently(t *testing.T) {
	var started atomic.Int32
	gate := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if started.Add(1) == 2 {
			once.Do(func() { close(gate) })
		}
		select {
		case <-gate:
			fmt.Fprint(w, `{"data":[{"id":"demo"}]}`)
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer once.Do(func() { close(gate) })
	c, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"key-a", "key-b"} {
		if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": key}}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	models, err := c.RefreshModels(ctx)
	if err != nil || len(models) != 1 {
		t.Fatalf("scans blocked each other: %v %v", models, err)
	}
}

func TestSourceErrorsNeverExposeOpaqueAPIKeys(t *testing.T) {
	const key = "plainopaqueabc123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "Rejected: "+key, http.StatusUnauthorized) }))
	defer srv.Close()
	c, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": key, "models": "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Chat(context.Background(), &core.ChatRequest{Model: "demo", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected auth failure")
	}
	if strings.Contains(err.Error(), key) || strings.Contains(c.Status(context.Background()).Detail, key) {
		t.Fatal("upstream echo leaked an opaque key")
	}
	if core.ErrorClient(err) != "relay" {
		t.Fatalf("failure attributed to %q", core.ErrorClient(err))
	}
}

func TestRemovedSourceCannotOverwriteRecreatedPool(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, `{"data":[{"id":"old-scan"}]}`)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	cfg := map[string]core.SourceConfig{"relay": {BaseURL: srv.URL}}
	r := core.NewRegistry()
	deps := core.Deps{DataDir: dir, HTTPClient: srv.Client()}
	if err := r.ReconcileSources(cfg, deps); err != nil {
		t.Fatal(err)
	}
	old, _ := r.Get("relay")
	a := old.(core.AccountManager)
	_, err := a.AddAccount(ctx, core.AccountSpec{Fields: map[string]string{"api_key": "old-key", "models": "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	scanned := make(chan error, 1)
	go func() { _, err := old.(core.ModelRefresher).RefreshModels(ctx); scanned <- err }()
	<-started
	if err := r.ReconcileSources(nil, deps); err != nil {
		close(release)
		t.Fatal(err)
	}
	if err := r.ReconcileSources(cfg, deps); err != nil {
		close(release)
		t.Fatal(err)
	}
	next, _ := r.Get("relay")
	if _, err := next.(core.AccountManager).AddAccount(ctx, core.AccountSpec{Fields: map[string]string{"api_key": "new-key", "models": "new-model"}}); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-scanned; err == nil {
		t.Error("removed source's scan was allowed to commit")
	}
	restored, err := newSource("relay", cfg["relay"], core.Deps{DataDir: filepath.Join(dir, "relay")})
	if err != nil {
		t.Fatal(err)
	}
	accounts, _ := restored.Accounts(ctx)
	if len(accounts) != 2 {
		t.Fatalf("old scan overwrote the new key: %v", accounts)
	}
}

func TestSourceRestoreRejectsEarlierModelScan(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, `{"data":[{"id":"stale-model"}]}`)
	}))
	defer srv.Close()
	defer once.Do(func() { close(release) })
	cfg := core.SourceConfig{BaseURL: srv.URL}
	c, err := newSource("relay", cfg, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	account, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "same-key", "models": "old-model"}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := c.RefreshModels(context.Background()); done <- err }()
	<-started
	if err := c.store().saveProviders([]storedProvider{{ID: account.ID, APIKey: "same-key", BaseURL: srv.URL, Models: []string{"restored-model"}}}); err != nil {
		t.Fatal(err)
	}
	if err := c.ConfigureSource(cfg); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	if err := <-done; err == nil {
		t.Error("pre-restore scan was allowed to overwrite restored models")
	}
	models, _ := c.Models(context.Background())
	if len(models) != 1 || models[0].ID != "restored-model" {
		t.Fatalf("restored catalogue was overwritten: %v", models)
	}
}

func TestSourceCatalogueScanPreservesChatCooldown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			fmt.Fprint(w, `{"data":[{"id":"demo"}]}`)
			return
		}
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	c, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "demo-key", "models": "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Chat(context.Background(), &core.ChatRequest{Model: "demo", Messages: []core.Message{{Role: "user", Content: "hi"}}})
	if err == nil {
		t.Fatal("expected chat rate limit")
	}
	if _, err := c.RefreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.Health().Cooling != 1 {
		t.Fatal("successful catalogue request cleared a chat rate limit")
	}
}

func TestSourceOldProbeCannotClearReplacementKeyCooldown(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer old-key" {
			close(started)
			<-release
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\ndata: [DONE]\n\n")
			return
		}
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	defer once.Do(func() { close(release) })
	c, err := newSource("relay", core.SourceConfig{BaseURL: srv.URL}, core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	a, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": "old-key", "models": "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan core.TestResult, 1)
	go func() { res, _ := c.TestAccount(context.Background(), a.ID); done <- res }()
	<-started
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{ID: a.ID, Fields: map[string]string{"api_key": "new-key", "models": "demo"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), &core.ChatRequest{Model: "demo", Messages: []core.Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Fatal("expected rate limit for replacement key")
	}
	once.Do(func() { close(release) })
	if res := <-done; !res.OK {
		t.Fatalf("old probe did not succeed: %v", res)
	}
	if c.Health().Cooling != 1 {
		t.Fatal("old probe erased replacement key cooldown")
	}
}
