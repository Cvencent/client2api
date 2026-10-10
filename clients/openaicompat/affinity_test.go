package openaicompat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// TestSourceConversationReusesTheSameAPIKey is the custom-relay half of the
// requirement: a caller that sends no conversation id must still keep the same
// conversation on one API key so the upstream prompt cache stays warm.
func TestSourceConversationReusesTheSameAPIKey(t *testing.T) {
	var mu sync.Mutex
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	c, err := newSource("custom", core.SourceConfig{BaseURL: srv.URL + "/v1"}, core.Deps{
		DataDir:    t.TempDir(),
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"key-a", "key-b"} {
		if _, err := c.AddAccount(context.Background(), core.AccountSpec{
			Fields: map[string]string{"api_key": key, "models": "vendor/model"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	req := func() *core.ChatRequest {
		return &core.ChatRequest{
			Model: "vendor/model",
			Messages: []core.Message{
				{Role: "system", Content: "you are helpful"},
				{Role: "user", Content: "hello"},
			},
		}
	}
	for turn := 0; turn < 2; turn++ {
		stream, err := c.Chat(context.Background(), req())
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		if _, _, _, _, _, err := drainStream(stream); err != nil {
			t.Fatalf("turn %d: drain: %v", turn, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(auth) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(auth))
	}
	if auth[0] != auth[1] {
		t.Fatalf("one conversation rotated API keys: %q then %q", auth[0], auth[1])
	}
	if strings.TrimSpace(auth[0]) == "" {
		t.Fatal("the upstream call carried no Authorization header")
	}
}

// TestSourceAdvertisesStickyConversationCapabilities pins the capability
// surface the panel discovers through type assertions.
func TestSourceAdvertisesStickyConversationCapabilities(t *testing.T) {
	c, err := newSource("custom", core.SourceConfig{BaseURL: "https://relay.example/v1"}, core.Deps{
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := core.AsConversationBinder(c); !ok {
		t.Fatal("a custom source does not implement ConversationBinder")
	}
	if _, ok := any(c).(core.LiveReloader); !ok {
		t.Fatal("a custom source does not implement LiveReloader")
	}
	caps := core.CapabilitiesOf(context.Background(), c)
	if !caps.Conversations || !caps.Live {
		t.Fatalf("custom source capabilities = %+v, want conversations and live enabled", caps)
	}
}

// TestClientConversationBindingIsWired covers the legacy onmiRoute client:
// the gateway may bind a conversation explicitly, and a successful Chat call
// must record the provider that served it.
func TestClientConversationBindingIsWired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	cfg, err := parseConfig([]byte(`{"providers":[{"id":"relay","base_url":"` + srv.URL + `","api_key":"key-a","models":["vendor/model"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{deps: core.Deps{HTTPClient: srv.Client()}, cfg: cfg}
	c.ensure()
	c.pool.reload(c.buildCredentials())

	binder, ok := core.AsConversationBinder(c)
	if !ok {
		t.Fatal("the legacy onmiRoute client does not implement ConversationBinder")
	}
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model: "relay/vendor/model",
		Messages: []core.Message{
			{Role: "system", Content: "you are helpful"},
			{Role: "user", Content: "hello"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, _, err := drainStream(stream); err != nil {
		t.Fatal(err)
	}
	key := core.DeriveConversationKey([]core.Message{
		{Role: "system", Content: "you are helpful"},
		{Role: "user", Content: "hello"},
	})
	if id, ok := binder.ConversationAccount(key, "relay/vendor/model"); !ok || id != "relay" {
		t.Fatalf("ConversationAccount = %q,%v, want relay,true", id, ok)
	}
	if got := c.affinity.Count(); got != 1 {
		t.Fatalf("sticky bindings = %d, want 1", got)
	}
}

func TestClientAffinityApplyLive(t *testing.T) {
	c := testClient(t)
	c.ApplyLive(core.LiveSettings{
		AffinityTTL:        7 * time.Minute,
		AffinityGCInterval: 45 * time.Second,
	})
	if got := c.affinity.TTL(); got != 7*time.Minute {
		t.Fatalf("affinity TTL = %s, want 7m", got)
	}
	if got := c.affinity.GCInterval(); got != 45*time.Second {
		t.Fatalf("affinity GC interval = %s, want 45s", got)
	}
	off := false
	c.ApplyLive(core.LiveSettings{AffinityEnabled: &off})
	if c.affinity.Enabled() {
		t.Fatal("ApplyLive did not disable conversation stickiness")
	}
}
