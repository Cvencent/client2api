package openaicompat

import (
	"context"
	"os"
	"testing"

	"client2api/internal/core"
)

// TestLiveAgentRouterResponses verifies the Responses wire against the real
// agentrouter.org endpoint.  Skipped unless CLIENT2API_LIVE_AGENTROUTER=1,
// so `go test ./...` never depends on a third-party service.
func TestLiveAgentRouterResponses(t *testing.T) {
	if os.Getenv("CLIENT2API_LIVE_AGENTROUTER") != "1" {
		t.Skip("set CLIENT2API_LIVE_AGENTROUTER=1 to run against agentrouter.org")
	}
	key := os.Getenv("AGENTROUTER_KEY")
	if key == "" {
		t.Fatal("AGENTROUTER_KEY is required for the live check")
	}
	c, err := newSource("agentrouter", core.SourceConfig{
		BaseURL:   "https://agentrouter.org/v1",
		UserAgent: "codex_cli_rs/0.0.1 (client2api)",
		APIFormat: "responses",
	}, core.Deps{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{"api_key": key}}); err != nil {
		t.Fatal(err)
	}
	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("scan returned no models")
	}
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    models[0].ID,
		Messages: []core.Message{{Role: "user", Content: "Reply with exactly: OK"}},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	text, _, _, usage, finish, err := drainStream(stream)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	t.Logf("model=%s text=%q finish=%s usage=%+v", models[0].ID, text, finish, usage)
	if text == "" {
		t.Fatal("empty completion")
	}
}
