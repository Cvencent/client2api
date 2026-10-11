package cline

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

func TestFreeOnlyFiltersModels(t *testing.T) {
	f := newFakeServer(t)
	f.json(modelsPath, http.StatusOK, `{"data":[{"id":"deepseek/deepseek-v4.1-flash"},{"id":"anthropic/claude-sonnet-4.5"}]}`)
	f.json(recommendedPath, http.StatusOK, `{"data":[],"free":["cline-free/deepseek-v4.1-flash"]}`)
	c := newTestClientWithConfig(t, f, `"access_token":"workos:t","free_only":true`)

	models, err := c.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	for _, m := range models {
		if m.ID == "deepseek/deepseek-v4.1-flash" || m.ID == "anthropic/claude-sonnet-4.5" {
			t.Fatalf("paid model %q survived free_only: %v", m.ID, modelIDs(models))
		}
	}
	if !containsID(models, "cline-free/deepseek-v4.1-flash") {
		t.Fatalf("models = %v, want the free model", modelIDs(models))
	}
}

func TestFreeOnlyRejectsPaidChatBeforeUpstream(t *testing.T) {
	f := newFakeServer(t)
	c := newTestClientWithConfig(t, f, `"access_token":"workos:t","free_only":true`)
	c.models.finishRefresh([]core.Model{{ID: "deepseek/deepseek-v4.1-flash", Extra: map[string]any{"free": false}}}, nil, time.Now())

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek/deepseek-v4.1-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("Chat = %v, want ErrUnsupported", err)
	}
	if f.count(chatPath) != 0 {
		t.Fatalf("chat endpoint was called %d times, want 0", f.count(chatPath))
	}
}

func TestFreeOnlyFiltersStatusModels(t *testing.T) {
	f := newFakeServer(t)
	c := newTestClientWithConfig(t, f, `"access_token":"workos:t","free_only":true`)
	c.models.finishRefresh([]core.Model{
		{ID: "deepseek/deepseek-v4.1-flash", Extra: map[string]any{"free": false}},
		{ID: "cline-free/deepseek-v4.1-flash", Extra: map[string]any{"free": true}},
	}, nil, time.Now())

	st := c.Status(context.Background())
	for _, id := range st.Models {
		if id == "deepseek/deepseek-v4.1-flash" {
			t.Fatalf("Status.Models = %v, want the paid model filtered under free_only", st.Models)
		}
	}
	if !containsString(st.Models, "cline-free/deepseek-v4.1-flash") {
		t.Fatalf("Status.Models = %v, want the free model", st.Models)
	}
}
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsID(models []core.Model, id string) bool {
	for _, m := range models {
		if m.ID == id {
			return true
		}
	}
	return false
}
