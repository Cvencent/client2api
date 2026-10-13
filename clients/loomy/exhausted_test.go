package loomy

import (
	"errors"
	"testing"

	"context"
	"time"

	"client2api/internal/core"
)

// The gateway skips a platform whose accounts are all parked or cooling down
// only when the module returns core.ErrPlatformExhausted.  A plain error reads
// as an upstream refusal, so a bare model that loomy also serves would fail
// outright instead of failing over to a sibling platform.
func TestChatWhenEveryAccountIsCoolingReturnsPlatformExhausted(t *testing.T) {
	c := newTestClient(t, "{}", failingTransport{err: errors.New("unreachable")})
	seedAccount(t, c, "loomy-1", testToken, 0)

	// Park the account: a short cooldown is enough.
	c.store.penaliseChat("loomy-1", &apiError{Code: "6004", Message: "rate limited"}, time.Now().UTC(), 10*time.Minute)

	acc, _ := c.store.lookup("loomy-1")
	if acc.selectable(time.Now()) {
		t.Fatal("the account should be cooling down")
	}

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat succeeded although every account is cooling down")
	}
	if !errors.Is(err, core.ErrPlatformExhausted) {
		t.Fatalf("Chat = %v, want an error wrapping core.ErrPlatformExhausted", err)
	}
}

func TestChatWhenEveryAccountIsDeadReturnsPlatformExhausted(t *testing.T) {
	c := newTestClient(t, "{}", failingTransport{err: errors.New("unreachable")})
	seedAccount(t, c, "loomy-1", testToken, 0)

	// Mark the account dead.
	c.store.penaliseWith("loomy-1", &apiError{Code: "100002"}, time.Now().UTC(), 0, true)

	acc, _ := c.store.lookup("loomy-1")
	if acc.selectable(time.Now()) {
		t.Fatal("the account should be dead")
	}

	_, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("Chat succeeded although every account is dead")
	}
	if !errors.Is(err, core.ErrPlatformExhausted) {
		t.Fatalf("Chat = %v, want an error wrapping core.ErrPlatformExhausted", err)
	}
}
