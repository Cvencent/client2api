package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := parseConfig(json.RawMessage(`{
		"providers": [
			{"id": "groq", "api_key": "gsk_test"},
			{"id": "cerebras", "api_key": "csk_test"}
		]
	}`))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	return cfg
}

func testClient(t *testing.T) *Client {
	t.Helper()
	raw, _ := New(core.Deps{})
	c := raw.(*Client)
	c.ensure()
	c.cfg = testConfig(t)
	c.pool.reload(c.buildCredentials())
	return c
}

// providerAndModel splits a qualified model the way Client.Chat does, for
// tests that call buildChatBody directly.
func providerAndModel(t *testing.T, c *Client, model string) (ProviderConfig, string) {
	t.Helper()
	prov, m, ok := c.providerFor(model)
	if !ok {
		t.Fatalf("providerFor(%q) failed", model)
	}
	return prov, m
}

func TestChatRoutesToCorrectProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/groq/chat/completions":
			if auth != "Bearer gsk_test" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		case "/cerebras/chat/completions":
			if auth != "Bearer csk_test" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	cfg := testConfig(t)
	for i := range cfg.Providers {
		cfg.Providers[i].BaseURL = srv.URL + "/" + cfg.Providers[i].ID
	}
	raw, _ := New(core.Deps{HTTPClient: srv.Client()})
	c := raw.(*Client)
	c.ensure()
	c.cfg = cfg
	c.pool.reload(c.buildCredentials())

	ctx := context.Background()
	for _, provider := range []string{"groq", "cerebras"} {
		model := provider + "/openai/gpt-oss-120b"
		req := &core.ChatRequest{
			Model:    model,
			Messages: []core.Message{{Role: "user", Content: "ping"}},
			Stream:   true,
		}
		req.SetAccountAcquirer(func(string) (func(), error) { return func() {}, nil })
		stream, err := c.Chat(ctx, req)
		if err != nil {
			t.Fatalf("Chat(%s): %v", model, err)
		}
		text, _, _, _, _, derr := drainStream(stream)
		if derr != nil {
			t.Fatalf("drainStream(%s): %v", model, derr)
		}
		if strings.TrimSpace(text) != "ok" {
			t.Errorf("Chat(%s) text = %q, want %q", model, text, "ok")
		}
	}
}

func TestChatReportsUnexpectedEOFWhenUpstreamCutsStreamBeforeDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
	}))
	defer srv.Close()
	c := testClient(t)
	c.cfg.Providers[0].BaseURL = srv.URL
	c.cfg.Providers = c.cfg.Providers[:1]
	c.pool.reload(c.buildCredentials())
	req := &core.ChatRequest{Model: "groq/llama-3.3-70b-versatile", Messages: []core.Message{{Role: "user", Content: "ping"}}, Stream: true}
	req.SetAccountAcquirer(func(string) (func(), error) { return func() {}, nil })
	stream, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()
	_, _, _, _, _, err = drainStream(stream)
	if err == nil {
		t.Fatal("cut stream was reported as a successful completion")
	}
	if !strings.Contains(err.Error(), "ended before a terminal frame") {
		t.Fatalf("error = %v", err)
	}
}
func TestChatUnknownProvider(t *testing.T) {
	c := testClient(t)
	req := &core.ChatRequest{
		Model:    "nosuch/model",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}
	_, err := c.Chat(context.Background(), req)
	if !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("expected ErrUnsupported, got %v", err)
	}
}

func TestChatEmptyMessages(t *testing.T) {
	c := testClient(t)
	req := &core.ChatRequest{Model: "groq/llama-3.3-70b-versatile"}
	_, err := c.Chat(context.Background(), req)
	if !errors.Is(err, core.ErrUnsupported) {
		t.Errorf("expected ErrUnsupported for empty messages, got %v", err)
	}
}

func TestBuildChatBodyMaxTokens(t *testing.T) {
	c := testClient(t)
	cfg := c.cfg
	prov, model := providerAndModel(t, c, "groq/llama-3.3-70b-versatile")
	limit := 256
	req := &core.ChatRequest{
		Model:     "groq/llama-3.3-70b-versatile",
		Messages:  []core.Message{{Role: "user", Content: "hi"}},
		MaxTokens: &limit,
	}
	body, err := buildChatBody(cfg, prov, model, req)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if m["max_tokens"] != float64(256) {
		t.Errorf("max_tokens = %v, want 256", m["max_tokens"])
	}
	if _, has := m["max_completion_tokens"]; has {
		t.Errorf("max_completion_tokens should not be present by default")
	}
}

func TestBuildChatBodyModelStripped(t *testing.T) {
	c := testClient(t)
	prov, model := providerAndModel(t, c, "groq/openai/gpt-oss-120b")
	req := &core.ChatRequest{
		Model:    "groq/openai/gpt-oss-120b",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}
	body, err := buildChatBody(c.cfg, prov, model, req)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	if m["model"] != "openai/gpt-oss-120b" {
		t.Errorf("model = %v, want openai/gpt-oss-120b", m["model"])
	}
}

func TestParseModelsBody(t *testing.T) {
	body := []byte(`{"data":[{"id":"m1","object":"model"},{"id":"m2"},{"id":"m1"}]}`)
	list, err := parseModelsBody(body)
	if err != nil {
		t.Fatalf("parseModelsBody: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 models, got %d", len(list))
	}
	if list[0].ID != "m1" || list[1].ID != "m2" {
		t.Errorf("models = [%s, %s]", list[0].ID, list[1].ID)
	}
}

func TestModelsAggregatesProviders(t *testing.T) {
	c := testClient(t)
	list, err := c.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("expected a non-empty aggregated catalogue")
	}
	seenGroq, seenCerebras := false, false
	for _, m := range list {
		if strings.HasPrefix(m.ID, "groq/") {
			seenGroq = true
		}
		if strings.HasPrefix(m.ID, "cerebras/") {
			seenCerebras = true
		}
	}
	if !seenGroq || !seenCerebras {
		t.Errorf("catalogue missing provider prefix: groq=%v cerebras=%v", seenGroq, seenCerebras)
	}
}
