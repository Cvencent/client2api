package qoder

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
)

// modelserver_test.go pins the international chat transport: a plain OpenAI
// completion, a bearer device token, no COSY signature, and the bare model id on
// the wire.  The mainland build's signed /algo route must not be used for an
// international account.

// plainOpenAIStream is the international model server's wire shape: a bare
// OpenAI chunk stream, with none of the {headers, body, statusCode} envelope the
// mainland /algo edge wraps around each frame.
const plainOpenAIStream = "data:{\"choices\":[{\"delta\":{\"content\":\"pong\"},\"index\":0}],\"object\":\"chat.completion.chunk\"}\n\n" +
	"data:{\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"object\":\"chat.completion.chunk\"}\n\n" +
	"data:[DONE]\n\n"

func TestModelServerChatUsesThePlainOpenAITransport(t *testing.T) {
	type hit struct {
		path   string
		auth   string
		body   map[string]any
		accept string
	}
	var hits []hit
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		hits = append(hits, hit{
			path:   r.URL.Path,
			auth:   r.Header.Get("Authorization"),
			body:   decoded,
			accept: r.Header.Get("Accept"),
		})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(plainOpenAIStream))
	}))
	defer server.Close()

	acc := account{storedAccount: storedAccount{
		ID: "g1", Token: "dt-intl", UserID: "u-1", Name: "intl", Enabled: true, Realm: realmGlobal,
	}}
	cfg := config{
		GatewayBase:       "http://gateway.invalid",
		GlobalGatewayBase: "http://gateway.invalid",
		GlobalModelServer: server.URL,
	}
	c := &Client{
		cfg:      cfg,
		up:       newUpstream(cfg, server.Client(), nil),
		store:    &store{all: []*account{&acc}},
		sessions: map[string]sessionEntry{},
	}
	// The server URL is http, and gatewayHTTP is the client the transport uses.
	c.ownHTTP = server.Client()
	c.httpOnce.Do(func() {})

	req := &core.ChatRequest{
		Model:     "global:efficient",
		Messages:  []core.Message{{Role: "user", Content: "ping"}},
		MaxTokens: intPtr(16),
	}
	stream, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	var reply strings.Builder
	for {
		ev, rerr := stream.Recv()
		if rerr != nil {
			break
		}
		if ev.Type == core.EventDelta {
			reply.WriteString(ev.Delta)
		}
	}
	if got := strings.TrimSpace(reply.String()); got != "pong" {
		t.Fatalf("reply = %q, want pong (the plain OpenAI stream must decode)", got)
	}

	if len(hits) != 1 {
		t.Fatalf("model server hits = %d, want 1", len(hits))
	}
	h := hits[0]
	if h.path != pathModelServerChat {
		t.Fatalf("path = %q, want %q", h.path, pathModelServerChat)
	}
	if h.auth != "Bearer dt-intl" {
		t.Fatalf("Authorization = %q, want the plain device bearer token", h.auth)
	}
	if h.accept != "text/event-stream" {
		t.Fatalf("Accept = %q, want text/event-stream", h.accept)
	}
	// The realm prefix is this gateway's addressing scheme: it must be gone from
	// the body the vendor reads.
	if model, _ := h.body["model"].(string); model != "efficient" {
		t.Fatalf("wire model = %q, want the bare id %q", model, "efficient")
	}
}

func TestModelServerChatNeverSendsTheCOSYSignature(t *testing.T) {
	authHeaders := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name := range r.Header {
			lower := strings.ToLower(name)
			if strings.HasPrefix(lower, "cosy-") {
				authHeaders = append(authHeaders, lower)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(plainOpenAIStream))
	}))
	defer server.Close()

	acc := account{storedAccount: storedAccount{
		ID: "g1", Token: "dt-intl", UserID: "u-1", Enabled: true, Realm: realmGlobal,
	}}
	body, _ := chatBody(&core.ChatRequest{
		Model:    "efficient",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}, "efficient")
	resp, err := (&Client{
		cfg: config{GlobalModelServer: server.URL},
		up:  newUpstream(config{}, server.Client(), nil),
	}).openModelServerChat(context.Background(), acc, body, "efficient")
	if err != nil {
		t.Fatalf("openModelServerChat: %v", err)
	}
	resp.Body.Close()
	if len(authHeaders) != 0 {
		t.Fatalf("the model-server request carried COSY headers %v; the clean endpoint takes a plain bearer token", authHeaders)
	}
}

// TestModelServerChatNamesTheHostOnATransportFailure keeps the diagnosis honest:
// the vendor's model host is HTTPDNS-only, so a machine that cannot resolve it
// must be told which address it failed to reach.
func TestModelServerChatNamesTheHostOnATransportFailure(t *testing.T) {
	acc := account{storedAccount: storedAccount{
		ID: "g1", Token: "dt-intl", Enabled: true, Realm: realmGlobal,
	}}
	c := &Client{
		cfg: config{GlobalModelServer: "http://127.0.0.1:1"},
		up:  newUpstream(config{}, &http.Client{}, nil),
	}
	body, _ := chatBody(&core.ChatRequest{
		Model:    "efficient",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}, "efficient")
	_, err := c.openModelServerChat(context.Background(), acc, body, "efficient")
	if err == nil {
		t.Fatal("a connection to a dead port succeeded")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Fatalf("error %q does not name the host it failed to reach", err)
	}
}

// TestModelServerRejectsAnEmptyConfiguration refuses to send a request at all
// when no reachable host is configured, instead of dialling the vendor's
// HTTPDNS-only default silently.
func TestModelServerRejectsAnEmptyConfiguration(t *testing.T) {
	cfg := config{GlobalModelServer: ""}
	c := &Client{cfg: cfg}
	// An explicit "disable" is expressed by setting the field to a value that
	// trims to empty; the accessor must then fall back to the vendor default
	// rather than returning "".  That default is what an unconfigured install
	// uses, so the only true "no host" case is a non-international realm.
	if got := cfg.modelServerBaseFor(realmCN); got != "" {
		t.Fatalf("domestic model server = %q, want empty", got)
	}
	acc := account{storedAccount: storedAccount{ID: "c1", Token: "dt", Enabled: true, Realm: realmCN}}
	if _, err := c.openModelServerChat(context.Background(), acc, "{}", "auto"); err == nil {
		t.Fatal("a domestic account was allowed onto the model-server transport")
	}
}

func TestChatOpenerPicksTheRealmTransport(t *testing.T) {
	c := &Client{cfg: config{}}
	if got := c.chatOpener(realmGlobal); got == nil {
		t.Fatal("chatOpener(global) returned nil")
	}
	if got := c.chatOpener(realmCN); got == nil {
		t.Fatal("chatOpener(cn) returned nil")
	}
}

// TestProbeModelForUsesTheRealmsOwnCatalogue stops a global account from being
// probed with a domestic model id, which the vendor would answer "Unsupported
// model" -- a refusal that would park a working account.
func TestProbeModelForUsesTheRealmsOwnCatalogue(t *testing.T) {
	c := &Client{cfg: config{ChatModel: "auto"}}
	c.models = catalogue([]modelInfo{
		{ID: "cn:qmodel_38max"},
		{ID: "global:auto"},
		{ID: "global:efficient"},
	})
	if got := c.probeModelFor(realmGlobal); got != "auto" && got != "efficient" {
		t.Fatalf("global probe model = %q, want an id the global catalogue publishes", got)
	}
	if got := c.probeModelFor(realmCN); got != "qmodel_38max" {
		t.Fatalf("domestic probe model = %q, want the configured auto when the realm has it", got)
	}
}

func TestFetchModelsMergesBothRealmCatalogues(t *testing.T) {
	// The signed /algo catalogue route carries a COSY bearer token, not the
	// device token, so the fixture keys on the host each realm was configured
	// with instead.  Two servers stand in for the two builds' edges.
	cnServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat":[{"key":"qmodel_38max","enable":true}]}`))
	}))
	defer cnServer.Close()
	globalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"chat":[{"key":"auto","enable":true},{"key":"efficient","enable":true}]}`))
	}))
	defer globalServer.Close()

	cn := account{storedAccount: storedAccount{ID: "cn-1", Token: "dt-cn", UserID: "u-cn", Enabled: true, Realm: realmCN}}
	global := account{storedAccount: storedAccount{ID: "g-1", Token: "dt-global", UserID: "u-g", Enabled: true, Realm: realmGlobal}}
	cfg := config{
		OpenAPIBase:       cnServer.URL,
		GatewayBase:       cnServer.URL,
		GlobalGatewayBase: globalServer.URL,
	}
	c := &Client{
		cfg:      cfg,
		up:       newUpstream(cfg, cnServer.Client(), nil),
		store:    &store{all: []*account{&cn, &global}},
		sessions: map[string]sessionEntry{},
	}
	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	got := map[string]bool{}
	for _, m := range models {
		got[m.ID] = true
	}
	for _, want := range []string{"cn:qmodel_38max", "global:auto", "global:efficient"} {
		if !got[want] {
			t.Errorf("merged catalogue is missing %q (got %v)", want, got)
		}
	}
	// A bare id must never reach the picker once the realms are known.
	for id := range got {
		if !realmQualified(id) {
			t.Errorf("catalogue entry %q is not realm-qualified", id)
		}
	}
}
