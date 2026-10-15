package cline

import (
	"context"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// TestChatReportsARateLimitToTheGatewayHook pins the module half of the
// gateway's per-model park: when the only account answers 429, the module must
// report (account, error) through the request hook, carrying the vendor's
// Retry-After so the gateway can honour it.
func TestChatReportsARateLimitToTheGatewayHook(t *testing.T) {
	f := newFakeServer(t)
	f.handle(chatPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	})
	c := newTestClient(t, f, `"access_token":"workos:tok","account_id":"usr-1"`)

	var gotAccount string
	var gotErr error
	req := &core.ChatRequest{
		Model:    "deepseek-v4.1-flash",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}
	req.SetAccountFailureReporter(func(accountID string, err error) {
		gotAccount, gotErr = accountID, err
	})

	if _, err := c.Chat(context.Background(), req); err == nil {
		t.Fatal("Chat succeeded against a 429")
	}
	if gotAccount == "" {
		t.Fatal("the 429 was not reported to the gateway hook")
	}
	failure, ok := core.AsFailure(gotErr)
	if !ok {
		t.Fatalf("reported error = %v, want a *core.Failure", gotErr)
	}
	if failure.Kind != core.FailureRateLimited {
		t.Fatalf("reported kind = %q, want %q", failure.Kind, core.FailureRateLimited)
	}
	if failure.RetryAfter != 7*time.Second {
		t.Fatalf("reported Retry-After = %v, want 7s", failure.RetryAfter)
	}
}
