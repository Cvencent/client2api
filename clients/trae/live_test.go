package trae

// live_test.go — the only test that touches the network.  It is skipped unless
// CLIENT2API_LIVE=1, so `go test ./clients/trae/...` stays offline by default.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestLive(t *testing.T) {
	if os.Getenv("CLIENT2API_LIVE") != "1" {
		t.Skip("set CLIENT2API_LIVE=1 to run the live test (needs a real Trae credential)")
	}

	deps := core.Deps{
		DataDir:    t.TempDir(),
		HTTPClient: &http.Client{Timeout: 60 * time.Second},
		Logf:       func(format string, args ...any) { t.Logf(format, args...) },
	}
	cl, err := New(deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	st := cl.Status(ctx)
	t.Logf("status: ready=%v detail=%s accounts=%d", st.Ready, st.Detail, len(st.Accounts))
	for _, a := range st.Accounts {
		t.Logf("  account %s state=%s expires=%s note=%s extra=%v", a.ID, a.State, a.ExpiresAt, a.Note, a.Extra)
	}
	if !st.Ready {
		t.Fatalf("not ready: %s", st.Detail)
	}

	models, err := cl.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	t.Logf("catalogue: %d models (first %s)", len(models), models[0].ID)

	model := st.Models[0]
	for _, m := range models {
		if m.ID == "glm-5.2" {
			model = m.ID
		}
	}

	stream, err := cl.Chat(ctx, &core.ChatRequest{
		Model:    model,
		Messages: []core.Message{{Role: "user", Content: "say hi in three words"}},
		Stream:   true,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	var usage *core.Usage
	var finish string
	for {
		ev, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch ev.Type {
		case core.EventDelta:
			text.WriteString(ev.Delta)
		case core.EventUsage:
			usage = ev.Usage
		case core.EventDone:
			finish = ev.Finish
		case core.EventError:
			t.Fatalf("upstream error: %v", ev.Err)
		}
	}
	t.Logf("model=%s finish=%s text=%q usage=%+v", model, finish, text.String(), usage)
	if strings.TrimSpace(text.String()) == "" {
		t.Error("the model returned no text")
	}
	if finish == "" {
		t.Error("no finish reason was emitted")
	}
}
