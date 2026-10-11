package kilocode

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// These checks talk to the real Kilo gateway and are skipped unless
// KILOCODE_LIVE=1 is set.  They never run in the offline suite.
//
// KILOCODE_LIVE=1 go test ./clients/kilocode/ -run Live -v
//
// A proxy can be supplied through the ordinary HTTPS_PROXY environment
// variable, which the module's default HTTP client honours.
func liveClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("KILOCODE_LIVE") != "1" {
		t.Skip("set KILOCODE_LIVE=1 to run the live gateway checks")
	}
	return newTestClient(t, `{}`, nil)
}

func TestLiveAnonymousModels(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := c.RefreshModels(ctx)
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	free := 0
	for _, m := range list {
		if modelIsFree(m) {
			free++
		}
	}
	if free == 0 {
		t.Fatalf("no free models in the live catalogue (%d total)", len(list))
	}
	t.Logf("live catalogue: %d free models out of %d", free, len(list))
}

func TestLiveAnonymousChat(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stream, err := c.Chat(ctx, &core.ChatRequest{
		Model:    freeFallbackModel,
		Messages: []core.Message{{Role: "user", Content: "Reply with the single word: pong"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	text, _, _, _, _, err := drainStream(stream)
	if err != nil {
		t.Fatalf("drainStream: %v", err)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatalf("empty reply from the live gateway")
	}
	t.Logf("live reply: %q", strings.TrimSpace(text))
}

// TestLiveOAuthImportAndModels imports the machine's own Kilo CLI/Desktop
// sign-in (when one exists) and pulls the authenticated catalogue through it.
// It is the end-to-end proof that a real sign-in token is carried as a bearer
// credential and that the account's free models are visible.
func TestLiveOAuthImportAndModels(t *testing.T) {
	c := liveClient(t)
	var authPath string
	for _, p := range oauthCredentialPaths() {
		if _, err := os.Stat(p); err == nil {
			authPath = p
			break
		}
	}
	if authPath == "" {
		t.Skip("no local Kilo sign-in file found")
	}
	recs, err := c.Import(context.Background(), []string{authPath}, false)
	if err != nil {
		t.Fatalf("Import(%s): %v", authPath, err)
	}
	if len(recs) == 0 {
		t.Fatalf("no account imported from %s", authPath)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := c.RefreshModels(ctx)
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	free := 0
	for _, m := range list {
		if modelIsFree(m) {
			free++
		}
	}
	if free == 0 {
		t.Fatalf("no free models in the authenticated catalogue (%d total)", len(list))
	}
	t.Logf("authenticated catalogue: %d free of %d models via %s", free, len(list), authPath)
}
