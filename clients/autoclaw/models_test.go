package autoclaw

import (
	"encoding/json"
	"strings"
	"testing"

	"client2api/internal/core"
)

func TestDeveloperRoleMapsToSystem(t *testing.T) {
	req := &core.ChatRequest{Messages: []core.Message{{Role: "developer", Content: "be terse"}, {Role: "user", Content: "hi"}}}
	body := buildChatBody(req, "zai_auto", true)
	// The desktop marker is prepended because the caller supplied no system
	// prompt of its own, so the developer message follows it.
	if len(body.Messages) != 3 {
		t.Fatalf("messages = %d", len(body.Messages))
	}
	if body.Messages[0].Role != "system" || !contentHasMarker(body.Messages[0].Content) {
		t.Errorf("desktop system marker missing: %+v", body.Messages[0])
	}
	if body.Messages[1].Role != "system" {
		t.Errorf("developer should map to system, got %q", body.Messages[1].Role)
	}
	if body.Messages[2].Role != "user" {
		t.Errorf("user role changed: %q", body.Messages[2].Role)
	}
}

func TestDesktopSystemPromptInjected(t *testing.T) {
	// A plain OpenAI request carrying no system prompt must be padded with the
	// managed proxy marker, otherwise AutoClaw answers 406 Not Acceptable.
	req := &core.ChatRequest{Messages: []core.Message{{Role: "user", Content: "hi"}}}
	body := buildChatBody(req, "zai_auto", true)
	if len(body.Messages) != 2 {
		t.Fatalf("messages = %d", len(body.Messages))
	}
	if body.Messages[0].Role != "system" || !contentHasMarker(body.Messages[0].Content) {
		t.Fatalf("marker not prepended: %+v", body.Messages[0])
	}
	if body.Messages[1].Role != "user" {
		t.Fatalf("user message lost: %+v", body.Messages[1])
	}
}

func TestDesktopSystemPromptNotDuplicated(t *testing.T) {
	req := &core.ChatRequest{Messages: []core.Message{
		{Role: "system", Content: desktopSystemPromptMarker + " Be brief."},
		{Role: "user", Content: "hi"},
	}}
	body := buildChatBody(req, "zai_auto", true)
	if len(body.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(body.Messages))
	}
	if body.Messages[0].Role != "system" {
		t.Fatalf("first role = %q", body.Messages[0].Role)
	}
}

func TestChatBodyUsesCompletionTokenField(t *testing.T) {
	// AutoClaw's managed proxy rejects any body carrying `max_tokens`, and for
	// values below 10000 it answers an opaque 406 before authentication. The
	// accepted spelling is `max_completion_tokens`, which the desktop client
	// uses for every request. A small caller limit must therefore still arrive
	// as max_completion_tokens, never as max_tokens.
	maxTok := 512
	req := &core.ChatRequest{
		Messages:  []core.Message{{Role: "user", Content: "hi"}},
		MaxTokens: &maxTok,
	}
	body := buildChatBody(req, "zai_auto", true)
	raw, _ := json.Marshal(body)
	got := string(raw)
	if strings.Contains(got, `"max_tokens"`) {
		t.Fatalf("body must not carry max_tokens: %s", got)
	}
	if !strings.Contains(got, `"max_completion_tokens":512`) {
		t.Fatalf("max_completion_tokens missing: %s", got)
	}
}

func TestParseCatalogEnvelope(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"models": []any{
		map[string]any{"id": "zai_auto", "name": "Auto", "api": "openai-completions", "reasoning": true, "input": []any{"text", "image"}, "contextWindow": 1048576, "maxTokens": 393216},
		map[string]any{"id": "skip", "api": "embeddings"},
	}}})
	models := parseCatalog(raw)
	if len(models) != 1 {
		t.Fatalf("models = %d, want 1", len(models))
	}
	m := models[0]
	if m.ID != "zai_auto" || m.OwnedBy != "autoclaw" {
		t.Errorf("unexpected model: %+v", m)
	}
	if m.Extra["max_output_tokens"] != 393216 {
		t.Errorf("max_output_tokens = %v", m.Extra["max_output_tokens"])
	}
	if m.Extra["vision"] != true {
		t.Error("vision flag missing")
	}
}

func TestParseCatalogBare(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"models": []any{map[string]any{"id": "zai_auto-fast", "name": "Auto-Fast", "api": "openai-completions"}}})
	models := parseCatalog(raw)
	if len(models) != 1 || models[0].ID != "zai_auto-fast" {
		t.Fatalf("unexpected models: %+v", models)
	}
}

func TestFallbackModels(t *testing.T) {
	models := fallbackModels()
	if len(models) != len(builtinModels) {
		t.Fatalf("fallback models = %d", len(models))
	}
	for _, m := range models {
		if m.ID == "" || m.Extra["fallback"] != true {
			t.Errorf("unexpected fallback entry: %+v", m)
		}
	}
}

func TestFlexString(t *testing.T) {
	var s flexString
	if err := json.Unmarshal([]byte(`123456`), &s); err != nil || s.String() != "123456" {
		t.Fatalf("flexString number: %v %q", err, s)
	}
	if err := json.Unmarshal([]byte(`"abc"`), &s); err != nil || s.String() != "abc" {
		t.Fatalf("flexString string: %v %q", err, s)
	}
}
