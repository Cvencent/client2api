package codearts

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// TestLivePluginStatistics reports only the credit-shaped fields from a real
// CodeArts statistics response. It is opt-in because it spends a network call
// and requires the operator's stored credential.
func TestLivePluginStatistics(t *testing.T) {
	if strings.TrimSpace(os.Getenv("C2A_LIVE_CODEARTS")) != "1" {
		t.Skip("set C2A_LIVE_CODEARTS=1 to query the live CodeArts statistics endpoint")
	}
	client, err := newClient(core.Deps{
		DataDir:    `..\..\data\codearts`,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
		Logf:       func(format string, args ...any) { t.Logf(format, args...) },
	})
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	c := client.(*Client)
	entries := c.pool.all()
	if len(entries) == 0 {
		t.Fatal("no stored CodeArts credential is available")
	}

	var raw map[string]any
	if err := c.signedSnap(context.Background(), entries[0].account(), http.MethodGet, statisticsPath, nil, &raw); err != nil {
		t.Fatalf("signedSnap: %v", err)
	}

	pkg, _ := raw["package"].(map[string]any)
	t.Logf("package=%v", pkg)
	if metrics, ok := raw["metrics"].([]any); ok {
		for _, item := range metrics {
			m, _ := item.(map[string]any)
			name, _ := m["name"].(string)
			if strings.Contains(strings.ToLower(name), "credit") {
				blob, _ := json.Marshal(m)
				t.Logf("metric=%s", blob)
			}
		}
	}
	for key, value := range raw {
		if strings.Contains(strings.ToLower(key), "credit") {
			t.Logf("top-level %s=%v", key, value)
		}
	}
}
