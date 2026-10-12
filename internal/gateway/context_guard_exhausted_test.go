package gateway

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"client2api/internal/core"
)

// exhaustedClient fails every account the way workbuddy's pool does for a
// non-retryable refusal: errors.Join(core.ErrPlatformExhausted, classified).
type exhaustedClient struct {
	name    string
	window  int64
	chatErr error
}

func (c *exhaustedClient) Name() string { return c.name }

func (c *exhaustedClient) Models(ctx context.Context) ([]core.Model, error) {
	return []core.Model{{ID: "cn:space-bunny", OwnedBy: c.name, Extra: map[string]any{
		"context_length": c.window,
	}}}, nil
}

func (c *exhaustedClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	return nil, c.chatErr
}

func (c *exhaustedClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: c.name, Ready: true}
}

// workbuddyTooLong is the exact error workbuddy builds for code 11115: the
// classified refusal joined onto core.ErrPlatformExhausted, because the pool
// walked every account before giving up.
func workbuddyTooLong() error {
	inner := core.Fail("workbuddy", "acct", core.FailureContextWindow, 400,
		fmt.Errorf("upstream prompt_too_long (http 400): {\"code\":11115,\"msg\":\"prompt is too long: 100001 tokens > 100000 maximum\"}"))
	return errors.Join(core.ErrPlatformExhausted, inner)
}

// TestGuardLearnsThroughTheExhaustedPoolPath is the regression test for the
// reason the guard never fired in production.
//
// openStream returns on core.ErrPlatformExhausted before it classifies the
// error, so a hook placed after that check never ran for any module whose pool
// joins the marker -- which is every workbuddy 11115.  Six real refusals
// produced no learned window at all.
func TestGuardLearnsThroughTheExhaustedPoolPath(t *testing.T) {
	c := &exhaustedClient{
		name:    "workbuddy",
		window:  1000000, // the inflated figure the catalogue publishes
		chatErr: workbuddyTooLong(),
	}
	s := newGuardServer(t, c, filepath.Join(t.TempDir(), "learned_context_windows.json"))

	req := &core.ChatRequest{
		Model:    "cn:space-bunny",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}
	if _, err := s.openStream(context.Background(), c, req); err == nil {
		t.Fatal("expected the upstream refusal to be returned")
	}

	if got := s.learnedWindows.get("workbuddy", "cn:space-bunny"); got != 100000 {
		t.Fatalf("the guard must learn the vendor's limit through the exhausted-pool path, got %d", got)
	}
	// And it must be what the next request is budgeted against, so the inflated
	// 1M catalogue figure stops being used.
	if got := s.effectiveContextWindow(context.Background(), c, "cn:space-bunny"); got != 100000 {
		t.Fatalf("the effective window must be the vendor's, got %d", got)
	}
}

// TestGuardLearnsFromEveryModuleShape pins that learning does not depend on
// which exit the error took.  A module that returns the bare classified error
// and one that joins the exhausted marker must both teach the guard.
func TestGuardLearnsFromEveryModuleShape(t *testing.T) {
	shapes := map[string]error{
		"bare classified error": core.Fail("workbuddy", "acct", core.FailureContextWindow, 400,
			fmt.Errorf("prompt is too long: 100001 tokens > 100000 maximum")),
		"joined with exhausted": workbuddyTooLong(),
		"wrapped in fmt.Errorf": fmt.Errorf("chat: %w",
			core.Fail("workbuddy", "acct", core.FailureContextWindow, 400,
				fmt.Errorf("prompt is too long: 100001 tokens > 100000 maximum"))),
	}
	for name, err := range shapes {
		t.Run(name, func(t *testing.T) {
			c := &exhaustedClient{name: "workbuddy", window: 1000000, chatErr: err}
			s := newGuardServer(t, c, filepath.Join(t.TempDir(), "learned.json"))
			req := &core.ChatRequest{
				Model:    "cn:space-bunny",
				Messages: []core.Message{{Role: "user", Content: "hi"}},
			}
			s.openStream(context.Background(), c, req)
			if got := s.learnedWindows.get("workbuddy", "cn:space-bunny"); got != 100000 {
				t.Fatalf("expected to learn 100000, got %d", got)
			}
		})
	}
}

// A non-size refusal must not teach the guard anything, even through the
// exhausted path.
func TestGuardIgnoresNonSizeRefusalsThroughTheExhaustedPath(t *testing.T) {
	inner := core.Fail("workbuddy", "acct", core.FailureRateLimited, 429,
		fmt.Errorf("rate limited by upstream"))
	c := &exhaustedClient{
		name:    "workbuddy",
		window:  1000000,
		chatErr: errors.Join(core.ErrPlatformExhausted, inner),
	}
	s := newGuardServer(t, c, filepath.Join(t.TempDir(), "learned.json"))
	req := &core.ChatRequest{
		Model:    "cn:space-bunny",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}
	s.openStream(context.Background(), c, req)
	if got := s.learnedWindows.get("workbuddy", "cn:space-bunny"); got != 0 {
		t.Fatalf("a rate limit must not be read as a window, got %d", got)
	}
}
