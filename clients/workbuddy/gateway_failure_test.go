package workbuddy

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

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

// The international VLM builder always sets X-Conversation-ID. Its channel
// verifier answers a request without that header with the same HTTP 400 /
// 11-128 body users saw as account authentication failure. The id is also
// scoped to the conversation, not to one turn, so history growth must not
// change it between user sends.
func TestWorkbuddyGlobalChatKeepsConversationFingerprintAcrossTurns(t *testing.T) {
	var conversationIDs, acpIDs []string
	rt := &fakeRT{handler: func(req *http.Request) (*http.Response, error) {
		conversationID := req.Header.Get("X-Conversation-ID")
		if conversationID == "" {
			return jsonResponse(http.StatusBadRequest,
				`{"code":11-128,"msg":"request illegal","displayMsg":{"en":"Request blocked. Please send it again"}}`), nil
		}
		conversationIDs = append(conversationIDs, conversationID)
		acpIDs = append(acpIDs, req.Header.Get("acp-connection-id"))
		return sseResponse(http.StatusOK, realmSSE, nil), nil
	}}
	c, _ := panelClient(t, rt, map[string]string{
		"global.json": credJSON("access-token-global", "refresh-token-global", realmGlobal,
			"www.workbuddy.ai", "7cd135a3-799e-4056-9c12-8c65240eea07", time.Now().Add(24*time.Hour).Unix()),
	})

	requests := [][]core.Message{
		{{Role: "user", Content: "hello"}},
		{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
			{Role: "user", Content: "please continue"},
		},
	}
	for i, messages := range requests {
		stream, err := c.Chat(context.Background(), &core.ChatRequest{
			Model:    "global:glm-5.2",
			Messages: messages,
		})
		if err != nil {
			t.Fatalf("Chat turn %d: %v", i+1, err)
		}
		if err := stream.Close(); err != nil {
			t.Fatalf("close turn %d: %v", i+1, err)
		}
	}

	if len(conversationIDs) != 2 {
		t.Fatalf("upstream calls = %d, want 2", len(conversationIDs))
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(conversationIDs[0]) {
		t.Fatalf("X-Conversation-ID = %q, want a 32-hex conversation id", conversationIDs[0])
	}
	if conversationIDs[1] != conversationIDs[0] {
		t.Fatalf("X-Conversation-ID changed between turns: %q then %q", conversationIDs[0], conversationIDs[1])
	}
	uuidV4 := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !uuidV4.MatchString(acpIDs[0]) {
		t.Fatalf("acp-connection-id = %q, want a lowercase UUIDv4", acpIDs[0])
	}
	if acpIDs[1] != acpIDs[0] {
		t.Fatalf("acp-connection-id changed between turns: %q then %q", acpIDs[0], acpIDs[1])
	}
}

// A genuine 11-128 refusal must stay a request/content rejection. Mapping it to
// FailureAuth made the gateway tell users their account was broken and invite a
// pointless re-login, while the same credential worked once the required client
// fingerprint was present.
func TestWorkbuddyGlobalChannelVerificationIsContentBlockedNotAuth(t *testing.T) {
	rt := &fakeRT{handler: func(*http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest,
			`{"code":11-128,"msg":"request illegal","requestId":"7e3a5d28a0fe660ff82ca080404db374","displayMsg":{"en":"Request blocked. Please send it again"}}`), nil
	}}
	c, _ := panelClient(t, rt, map[string]string{
		"global.json": credJSON("access-token-global", "refresh-token-global", realmGlobal,
			"www.workbuddy.ai", "7cd135a3-799e-4056-9c12-8c65240eea07", time.Now().Add(24*time.Hour).Unix()),
	})

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "global:glm-5.2",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err == nil {
		t.Fatal("expected a channel-verification error")
	}
	f, ok := core.AsFailure(err)
	if !ok {
		t.Fatalf("err = %T %v, want a *core.Failure", err, err)
	}
	if f.Kind != core.FailureContentBlocked {
		t.Fatalf("kind = %q, want %q; 11-128 must not masquerade as account authentication", f.Kind, core.FailureContentBlocked)
	}
	if f.Status != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", f.Status, http.StatusBadRequest)
	}
}
