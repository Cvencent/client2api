package openaicompat

import (
	"context"
	"os"
	"strings"
	"testing"

	"client2api/internal/core"
)

// TestLiveAgentArtsBasicAuth runs only when the operator exports a real
// AgentArts / Huawei MaaS app credential.  It is the live half of the Basic
// auth support: the scan below fails with a 401 if the header is wrong.
func TestLiveAgentArtsBasicAuth(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("CLIENT2API_AGENTARTS_BASE_URL"))
	key := strings.TrimSpace(os.Getenv("CLIENT2API_AGENTARTS_APP_KEY"))
	secret := strings.TrimSpace(os.Getenv("CLIENT2API_AGENTARTS_APP_SECRET"))
	if base == "" || key == "" || secret == "" {
		t.Skip("set CLIENT2API_AGENTARTS_BASE_URL / _APP_KEY / _APP_SECRET to run the live probe")
	}
	c, err := newSource("agentarts", core.SourceConfig{BaseURL: base}, core.Deps{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddAccount(context.Background(), core.AccountSpec{Fields: map[string]string{
		"api_key":    key,
		"app_secret": secret,
	}}); err != nil {
		t.Fatal(err)
	}
	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("live scan: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range models {
		seen[m.ID] = true
	}
	if !seen["glm-5.2"] || !seen["deepseek-v4.1-flash"] {
		t.Fatalf("live catalogue is missing the expected ids: %d models", len(models))
	}
	t.Logf("live scan returned %d models", len(models))
}
