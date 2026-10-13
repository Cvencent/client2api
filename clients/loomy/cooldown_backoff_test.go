package loomy

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// A first failed chat call is not authoritative enough to cool the account
// down. The module gives the same credential one follow-up attempt; if that
// succeeds, the account stays selectable with no failure recorded.
func TestChatRetriesTheSameAccountBeforeEnteringCooldown(t *testing.T) {
	withChatRetryDelay(t, time.Millisecond)
	calls := 0
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if r.URL.Path != "/api/v1/chat/completions" {
			return jsonResponse(200, okEnvelope("null"))
		}
		calls++
		if calls == 1 {
			return jsonResponse(200, failureEnvelope("100002", "temporary refusal"))
		}
		return sseResponse("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	if calls != 2 {
		t.Fatalf("chat calls = %d, want 2 (the same account retried once)", calls)
	}
	acc, _ := c.store.lookup("loomy-1")
	if !acc.selectable(time.Now().UTC()) {
		t.Fatalf("the account was parked or cooled down after a successful retry: %+v", acc)
	}
	if !acc.cooldownTill.IsZero() || acc.failures != 0 || acc.dead {
		t.Fatalf("a successful retry left penalties behind: %+v", acc)
	}
}

// Failed rounds escalate 30s -> 1m -> 2m. The next failed round is the fourth
// consecutive failure, so the account is marked abnormal instead of receiving
// another cooldown.
func TestChatEscalatesCooldownThenMarksTheAccountAbnormal(t *testing.T) {
	withChatRetryDelay(t, time.Millisecond)
	calls := 0
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if r.URL.Path != "/api/v1/chat/completions" {
			return jsonResponse(200, okEnvelope("null"))
		}
		calls++
		return jsonResponse(200, failureEnvelope("100002", "still unavailable"))
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	for i, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute} {
		if _, err := c.Chat(context.Background(), &core.ChatRequest{
			Model:    "deepseek-v4-flash-0731",
			Messages: []core.Message{{Role: "user", Content: "hi"}},
		}); err == nil {
			t.Fatalf("round %d succeeded, want a failure", i+1)
		}
		if calls != 2*(i+1) {
			t.Fatalf("round %d made %d calls, want %d", i+1, calls, 2*(i+1))
		}
		acc, _ := c.store.lookup("loomy-1")
		if acc.dead {
			t.Fatalf("round %d marked the account abnormal before three cooldowns elapsed: %+v", i+1, acc)
		}
		if acc.cooldownStage != i+1 {
			t.Fatalf("round %d cooldown stage = %d, want %d", i+1, acc.cooldownStage, i+1)
		}
		remaining := time.Until(acc.cooldownTill)
		if remaining < want-5*time.Second || remaining > want+2*time.Second {
			t.Fatalf("round %d cooldown = %s, want about %s", i+1, remaining, want)
		}
		forceCooldownExpired(t, c, "loomy-1")
	}

	if _, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("the fourth failed round succeeded, want an error")
	}
	acc, _ := c.store.lookup("loomy-1")
	if !acc.abnormal {
		t.Fatalf("after three cooldowns and another failed round, abnormal = false: %+v", acc)
	}
	if acc.dead {
		t.Fatalf("the fourth failed round used the old dead flag instead of abnormal: %+v", acc)
	}
	record := accountRecord(&acc, time.Now().UTC())
	if record.State != "invalid" {
		t.Fatalf("abnormal account state = %q, want invalid", record.State)
	}
	if !strings.Contains(record.Note, "abnormal") {
		t.Fatalf("abnormal account note = %q, want it to explain the repeated failure", record.Note)
	}
	if calls != 8 {
		t.Fatalf("total chat calls = %d, want 8 (four rounds, one retry each)", calls)
	}
}

// A lost race for one account's in-flight slot is not a failed switch. The
// request must skip that account and keep the full upstream attempt budget for
// the next one.
func TestChatSkipsABusyAccountWithoutSpendingAnAttempt(t *testing.T) {
	withChatRetryDelay(t, time.Millisecond)
	var calls int
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if r.URL.Path != "/api/v1/chat/completions" {
			return jsonResponse(200, okEnvelope("null"))
		}
		calls++
		return sseResponse("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	}}
	c := newTestClient(t, `{"max_attempts":1}`, rt)
	seedAccount(t, c, "busy", testToken, 0)
	seedAccount(t, c, "free", testTokenTwo, 0)

	var served string
	req := &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		ServedBy: &served,
	}
	req.SetAccountAcquirer(func(accountID string) (func(), error) {
		if accountID == "busy" {
			return nil, core.ErrBusy
		}
		return func() {}, nil
	})

	stream, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	defer stream.Close()

	if calls != 1 {
		t.Fatalf("upstream calls = %d, want one call on the free account", calls)
	}
	if served != "free" {
		t.Fatalf("ServedBy = %q, want free", served)
	}
	busy, _ := c.store.lookup("busy")
	if busy.failures != 0 || busy.cooldownStage != 0 || !busy.cooldownTill.IsZero() {
		t.Fatalf("the busy account was penalised: %+v", busy)
	}
}

func TestLoomyCooldownStageSurvivesARestart(t *testing.T) {
	withChatRetryDelay(t, time.Millisecond)
	rt := alwaysJSON(200, failureEnvelope("100002", "still unavailable"))
	c := newTestClient(t, "{}", rt)
	dir := filepath.Dir(c.store.path)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if _, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("the first failed round succeeded")
	}

	reloaded := newTestClientInDir(t, dir, "{}", rt)
	forceCooldownExpired(t, reloaded, "loomy-1")
	if _, err := reloaded.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("the failed round after restart succeeded")
	}
	acc, _ := reloaded.store.lookup("loomy-1")
	if acc.cooldownStage != 2 {
		t.Fatalf("cooldown stage after restart = %d, want 2", acc.cooldownStage)
	}
	remaining := time.Until(acc.cooldownTill)
	if remaining < time.Minute-5*time.Second || remaining > time.Minute+2*time.Second {
		t.Fatalf("cooldown after restart = %s, want the second stage (about 1m)", remaining)
	}
}

// Cancelling during the five-second retry wait is still a caller action, not
// evidence about the credential. It must not record a failure or start a
// cooldown.
func TestCancellingDuringTheRetryWaitDoesNotParkTheAccount(t *testing.T) {
	withChatRetryDelay(t, 250*time.Millisecond)
	firstSeen := make(chan struct{})
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if r.URL.Path != "/api/v1/chat/completions" {
			return jsonResponse(200, okEnvelope("null"))
		}
		close(firstSeen)
		return jsonResponse(200, failureEnvelope("100002", "temporary refusal"))
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Chat(ctx, &core.ChatRequest{
			Model:    "deepseek-v4-flash-0731",
			Messages: []core.Message{{Role: "user", Content: "hi"}},
		})
		done <- err
	}()

	<-firstSeen
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Chat error = %v, want context.Canceled", err)
	}
	assertNotParked(t, c, "loomy-1")
}

func withChatRetryDelay(t *testing.T, d time.Duration) {
	t.Helper()
	old := chatRetryDelay
	chatRetryDelay = d
	t.Cleanup(func() { chatRetryDelay = old })
}

func forceCooldownExpired(t *testing.T, c *Client, id string) {
	t.Helper()
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	for _, a := range c.store.all {
		if a.ID == id {
			a.cooldownTill = time.Now().UTC().Add(-time.Second)
			return
		}
	}
	t.Fatalf("account %q vanished while expiring its cooldown", id)
}
