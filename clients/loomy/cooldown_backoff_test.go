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

// A credit refusal is an account-funding verdict, not a transient chat
// failure. It must park the account until a funded balance refresh or
// check-in clears lowCredit, rather than rejoining the pool when the
// 30s/1m/2m cooldown ladder expires.
func TestChatCreditExhaustionParksUntilTheBalanceRecovers(t *testing.T) {
	withChatRetryDelay(t, time.Millisecond)
	calls := 0
	refusal := `{"error":{"message":"积分余额不足，请获取积分后再试","type":"insufficient_quota","code":402,` +
		`"metadata":{"provider_name":"loomy"}}}`
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if r.URL.Path != "/api/v1/chat/completions" {
			return jsonResponse(200, okEnvelope("null"))
		}
		calls++
		return jsonResponse(http.StatusPaymentRequired, refusal)
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-1", testToken, 0)

	if _, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("Chat reported success on an HTTP 402 credit refusal")
	}
	if calls != 2 {
		t.Fatalf("chat calls = %d, want the same-account retry before parking", calls)
	}

	acc, _ := c.store.lookup("loomy-1")
	if !acc.lowCredit {
		t.Fatalf("HTTP 402 did not mark lowCredit: %+v", acc)
	}
	if acc.cooldownStage != 0 || acc.failures != 0 {
		t.Fatalf("HTTP 402 advanced the transient cooldown ladder: %+v", acc)
	}
	if acc.selectable(time.Now().UTC().Add(24 * time.Hour)) {
		t.Fatal("a zero-credit account recovered when the transient cooldown expired")
	}
	record := accountRecord(&acc, time.Now().UTC())
	if record.State != "cooling" || record.Fields["low_credit"] != true {
		t.Fatalf("HTTP 402 account record = %+v, want cooling with low_credit", record)
	}
}

// A credit verdict on one account must still allow the same request to fail
// over to another funded account. Parking is account-scoped, not a platform
// circuit breaker.
func TestChatCreditExhaustionStillFailsOverToAFundedAccount(t *testing.T) {
	withChatRetryDelay(t, time.Millisecond)
	refusal := `{"error":{"message":"积分余额不足，请获取积分后再试","type":"insufficient_quota","code":402}}`
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if r.URL.Path != "/api/v1/chat/completions" {
			return jsonResponse(200, okEnvelope("null"))
		}
		if r.Header.Get("token") == testToken {
			return jsonResponse(http.StatusPaymentRequired, refusal)
		}
		return sseResponse("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "empty", testToken, 0)
	seedAccount(t, c, "funded", testTokenTwo, 0)

	var served string
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		ServedBy: &served,
	})
	if err != nil {
		t.Fatalf("Chat did not fail over after a credit refusal: %v", err)
	}
	defer stream.Close()

	if served != "funded" {
		t.Fatalf("ServedBy = %q, want the funded account", served)
	}
	empty, _ := c.store.lookup("empty")
	if !empty.lowCredit {
		t.Fatalf("the credit-refused account was not parked: %+v", empty)
	}
	funded, _ := c.store.lookup("funded")
	if funded.lowCredit || !funded.selectable(time.Now().UTC()) {
		t.Fatalf("the funded account was affected by another account's verdict: %+v", funded)
	}
}

// reserve_credits=-1 disables the low-credit guard, so an HTTP 402 must still
// follow the ordinary retry/cooldown ladder instead of creating a park the
// operator explicitly turned off.
func TestChatCreditRefusalRespectsADisabledBalanceGuard(t *testing.T) {
	withChatRetryDelay(t, time.Millisecond)
	refusal := `{"error":{"message":"积分余额不足，请获取积分后再试","type":"insufficient_quota","code":402}}`
	c := newTestClient(t, "{}", &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if r.URL.Path != "/api/v1/chat/completions" {
			return jsonResponse(200, okEnvelope("null"))
		}
		return jsonResponse(http.StatusPaymentRequired, refusal)
	}})
	seedAccount(t, c, "loomy-1", testToken, 0)
	c.ApplyPlatformPolicy(core.PlatformConfig{ReserveCredits: -1})

	if _, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	}); err == nil {
		t.Fatal("Chat reported success on a refused request")
	}
	acc, _ := c.store.lookup("loomy-1")
	if acc.lowCredit {
		t.Fatalf("reserve_credits=-1 did not disable the chat-path low-credit park: %+v", acc)
	}
	if acc.cooldownStage != 1 {
		t.Fatalf("cooldown stage = %d, want the ordinary first stage", acc.cooldownStage)
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

// A lost stage compare-and-swap must not abort the whole rotation.
//
// Two requests can read the same starting stage for one account and both fail.
// The first to write advances the ladder; the second finds the stage already
// moved, so penaliseChatWithStage reports false. That only means the account is
// already handled -- it is not evidence that the remaining accounts are bad, so
// the request has to keep failing over instead of returning the first error.
func TestChatKeepsFailingOverWhenTheCooldownStageRaceIsLost(t *testing.T) {
	withChatRetryDelay(t, time.Millisecond)
	var c *Client
	var firstCalls int
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		if r.URL.Path != "/api/v1/chat/completions" {
			return jsonResponse(200, okEnvelope("null"))
		}
		if r.Header.Get("token") != testToken {
			return sseResponse("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
		}
		firstCalls++
		if firstCalls == 2 {
			// A concurrent request wins the stage CAS for this same account:
			// the ladder moves 0 -> 1 between Chat's read and its final write.
			c.store.penaliseChatWithStage("first", errSessionExpired("first"), time.Now().UTC(), 0, time.Minute)
		}
		return jsonResponse(200, failureEnvelope("100002", "temporary refusal"))
	}}
	c = newTestClient(t, "{}", rt)
	seedAccount(t, c, "first", testToken, 0)
	seedAccount(t, c, "second", testTokenTwo, 0)

	var served string
	stream, err := c.Chat(context.Background(), &core.ChatRequest{
		Model:    "deepseek-v4-flash-0731",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
		ServedBy: &served,
	})
	if err != nil {
		t.Fatalf("Chat: %v (a lost cooldown-stage CAS aborted the rotation)", err)
	}
	defer stream.Close()

	if served != "second" {
		t.Fatalf("ServedBy = %q, want second (the request should fail over)", served)
	}
	first, _ := c.store.lookup("first")
	if first.cooldownStage != 1 {
		t.Fatalf("first account cooldown stage = %d, want 1", first.cooldownStage)
	}
}
