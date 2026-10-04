package workbuddy

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// Live, credentialed checks.  These talk to the real WorkBuddy endpoints and
// are therefore skipped unless CLIENT2API_LIVE=1 is set AND a loadable
// plaintext credential can be found.  Nothing here runs in the default
// offline test run.
//
//	CLIENT2API_LIVE=1 go test ./clients/workbuddy/... -run Live -v
//
// WORKBUDDY_LIVE_ACCOUNTS overrides the credential directory (default:
// ../../data/workbuddy, this module's real data dir, read-only here);
// WORKBUDDY_LIVE_MODEL overrides the model id used for the chat checks.

func liveClient(t *testing.T) *Client {
	t.Helper()
	if os.Getenv("CLIENT2API_LIVE") != "1" {
		t.Skip("set CLIENT2API_LIVE=1 to run live WorkBuddy checks")
	}
	dir := strings.TrimSpace(os.Getenv("WORKBUDDY_LIVE_ACCOUNTS"))
	if dir == "" {
		dir = filepath.Join("..", "..", "data", "workbuddy")
	}
	accounts, err := LoadAccounts(dir)
	if err != nil {
		t.Skipf("cannot read credentials from %s: %v", dir, err)
	}
	if len(accounts) == 0 {
		t.Skipf("no plaintext credential in %s (the vendor's own stores keep their tokens encrypted)", dir)
	}
	cfg, err := json.Marshal(map[string]any{"accounts_dir": dir})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	// The data dir is a temp dir so a live run never writes into the module's
	// real state; only the credential directory is read from disk.
	deps := core.Deps{DataDir: t.TempDir(), Config: cfg}
	deps.Logf = func(format string, args ...any) { t.Logf(format, args...) }
	c, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	wb, ok := c.(*Client)
	if !ok {
		t.Fatalf("New returned %T", c)
	}
	t.Logf("loaded %d account(s) from %s", len(accounts), dir)
	return wb
}

func TestLiveStatusAndModels(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	st := c.Status(ctx)
	t.Logf("status: ready=%v detail=%q accounts=%d", st.Ready, st.Detail, len(st.Accounts))
	for _, a := range st.Accounts {
		t.Logf("  account %s state=%s expires=%s note=%s", a.ID, a.State, a.ExpiresAt, a.Note)
	}
	if !st.Ready {
		t.Fatalf("no usable account: %s", st.Detail)
	}

	models, err := c.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("Models returned nothing")
	}
	t.Logf("models: %d (first: %s)", len(models), models[0].ID)
}

func TestLiveChatStream(t *testing.T) {
	c := liveClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if st := c.Status(ctx); !st.Ready {
		t.Skipf("no usable account: %s", st.Detail)
	}
	model := strings.TrimSpace(os.Getenv("WORKBUDDY_LIVE_MODEL"))
	if model == "" {
		if models, err := c.Models(ctx); err == nil && len(models) > 0 {
			model = models[0].ID
		}
	}
	if model == "" {
		t.Skip("no model id available")
	}

	stream, err := c.Chat(ctx, &core.ChatRequest{
		Model:  model,
		Stream: true,
		Messages: []core.Message{{
			Role:    "user",
			Content: "Reply with exactly one word: pong",
		}},
	})
	if err != nil {
		t.Fatalf("Chat(%s): %v", model, err)
	}
	defer func() { _ = stream.Close() }()

	var text, reasoning strings.Builder
	var toolCalls int
	var usage *core.Usage
	var done bool
	for {
		ev, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch ev.Type {
		case core.EventDelta:
			text.WriteString(ev.Delta)
			reasoning.WriteString(ev.Reasoning)
		case core.EventToolCall:
			toolCalls++
		case core.EventUsage:
			usage = ev.Usage
		case core.EventDone:
			done = true
			t.Logf("finish reason: %q", ev.Finish)
		case core.EventError:
			t.Fatalf("stream error: %v", ev.Err)
		}
	}
	t.Logf("model=%s text=%q reasoning=%q tool_calls=%d usage=%+v", model, text.String(), reasoning.String(), toolCalls, usage)
	if !done {
		t.Error("stream ended without EventDone")
	}
	if strings.TrimSpace(text.String()) == "" && toolCalls == 0 {
		t.Error("empty completion")
	}
}
