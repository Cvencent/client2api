package openaicompat

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"client2api/internal/core"
)

// An app key + app secret pair must be sent as HTTP Basic.  Huawei's
// APIG / AgentArts model gateway -- the upstream behind the OfficeAce desktop
// app -- issues that pair and answers a bearer token with APIG.0303.
func TestApplyHeadersUsesBasicForAppSecret(t *testing.T) {
	p := ProviderConfig{ID: "officeace", BaseURL: "https://example.test/v2", APIKey: "app-key", AppSecret: "app-secret"}
	req, err := http.NewRequest(http.MethodGet, p.base()+"/models", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	p.applyHeaders(req)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("app-key:app-secret"))
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}

	orphan := Config{Providers: []ProviderConfig{{ID: "broken", BaseURL: "https://example.test/v1", AppSecret: "only-secret"}}}.normalize()
	if len(orphan.Providers) != 0 {
		t.Fatalf("app_secret without an app key survived normalize: %+v", orphan.Providers)
	}
	kept := Config{Providers: []ProviderConfig{{ID: "officeace", BaseURL: "https://example.test/v2", APIKey: "app-key", AppSecret: "app-secret"}}}.normalize()
	if len(kept.Providers) != 1 || kept.Providers[0].AppSecret != "app-secret" {
		t.Fatalf("basic provider did not survive normalize: %+v", kept.Providers)
	}
}

// The panel-added account, the model scan and the chat call must all carry the
// same Basic credential end to end.
func TestSourceBasicAppAuthReachesUpstream(t *testing.T) {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("app-key:app-secret"))
	var sawModels, sawChat string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != want {
			t.Errorf("%s Authorization = %q, want %q", r.URL.Path, got, want)
		}
		switch r.URL.Path {
		case "/v2/models":
			sawModels = r.Header.Get("Authorization")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"glm-5.2"},{"id":"deepseek-v4.1-flash"}]}`)
		case "/v2/chat/completions":
			sawChat = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"}}]}\n\ndata: [DONE]\n\n")
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	deps := core.Deps{DataDir: t.TempDir(), HTTPClient: srv.Client()}
	c, err := newSource("officeace", core.SourceConfig{BaseURL: srv.URL + "/v2"}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		"api_key":    "app-key",
		"app_secret": "app-secret",
	}}); err != nil {
		t.Fatal(err)
	}
	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v, want both upstream ids", models)
	}
	stream, err := c.Chat(context.Background(), &core.ChatRequest{Model: "glm-5.2", Messages: []core.Message{{Role: "user", Content: "hi"}}, Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	if text, _, _, _, _, err := drainStream(stream); err != nil || text != "pong" {
		t.Fatalf("chat: %q %v", text, err)
	}
	if sawModels != want || sawChat != want {
		t.Fatalf("basic header not applied: models=%q chat=%q", sawModels, sawChat)
	}
}
