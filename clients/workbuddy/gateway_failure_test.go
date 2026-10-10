package workbuddy

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// TestChatPromptTooLongCarriesTheGatewayFailureStatus is the regression for a
// 11115 refusal reaching the gateway as an unclassified local error: the wire
// answer became 502 even though WorkBuddy had already said HTTP 400.
func TestChatPromptTooLongCarriesTheGatewayFailureStatus(t *testing.T) {
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest,
			`{"code":11115,"msg":"prompt is too long: 1060027 tokens > 1048576 maximum"}`), nil
	}}
	c, _ := newTestClient(t, rt)

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "claude-sonnet-4",
		Messages: []core.Message{{Role: "user", Content: strings.Repeat("x", 100)}},
	})
	if err == nil {
		t.Fatal("expected an error")
	}

	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %T %v, want a *core.Failure so the gateway can preserve HTTP 400", err, err)
	}
	if f.Kind != core.FailureContextWindow {
		t.Fatalf("kind = %q, want %q for a non-retryable context-window refusal", f.Kind, core.FailureContextWindow)
	}
	if f.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", f.Status, http.StatusBadRequest)
	}
	if !strings.Contains(err.Error(), "prompt_too_long") || !strings.Contains(err.Error(), "11115") {
		t.Fatalf("error %q lost the vendor classification", err)
	}
}
