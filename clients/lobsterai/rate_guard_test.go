package lobsterai

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"client2api/internal/core"
)

// 钉住每账号速率护栏（issue #179）：
//
//	窗口满的账号只是本轮不可选（请求去兄弟账号 / 全满时 core.ErrBusy），
//	不写 CooldownUntil、不改账号状态——反向钉子要求护栏状态不得被画成
//	「账号异常/冷却」。正常对话流量（每分钟 1-3 次）不得触发。
func TestRateGuardBlocksTheTwentyFirstTick(t *testing.T) {
	g := newRateGuard(3)
	now := time.Now()
	for i := 0; i < 3; i++ {
		now_i := now.Add(time.Duration(i) * time.Second)
		if !g.check("a1", now_i) {
			t.Fatalf("tick %d refused before the cap", i+1)
		}
		g.record("a1", now_i)
	}
	if g.check("a1", now.Add(4*time.Second)) {
		t.Fatal("tick 4 admitted past the cap：护栏没生效（issue #179）")
	}
	// 窗口滑出后恢复。
	if !g.check("a1", now.Add(defaultRateWindow+time.Minute)) {
		t.Fatal("the guard never recovered past the window")
	}
}

func TestRateGuardNeverTouchesAccountState(t *testing.T) {
	f := newRateFixture(t, 1) // cap=1：第二次请求必须换账号
	// 第一发打 a1（占一次窗口 + 一个在途槽，随后归还槽）；第二发必须绕开
	// a1（护栏窗口占满），且 a1 不得被写冷却。
	first, err := f.client.acquireAccount(&core.ChatRequest{Model: "m1"}, "")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	f.client.pool.release(first.ID)
	acct, err := f.client.acquireAccount(&core.ChatRequest{Model: "m1"}, "")
	if err != nil {
		t.Fatalf("second acquire = %v, want a sibling account（护栏降级不是失败，issue #179）", err)
	}
	if acct.ID == f.firstID {
		t.Fatalf("second acquire picked the same rate-capped account %s", acct.ID)
	}
	a := f.client.pool.find(f.firstID)
	if a == nil {
		t.Fatal("account vanished")
	}
	if a.CooldownUntil != "" {
		t.Fatalf("the guard wrote CooldownUntil %q：护栏状态不得写成冷却（issue #179 反向钉子）", a.CooldownUntil)
	}
	if a.LowCredit {
		t.Fatal("the guard wrote LowCredit：护栏不是余额问题")
	}
}

func TestRateGuardExhaustedSurfacesAsBusy(t *testing.T) {
	// 单账号 + cap=1：第二次必须 ErrBusy（429 让客户端稍后再来），不是失败。
	f := newRateFixture(t, 1)
	// 只留一个账号，模拟「没有兄弟可换」的最小形态。
	f.client.pool.mu.Lock()
	f.client.pool.accts = f.client.pool.accts[:1]
	f.client.pool.mu.Unlock()
	if _, err := f.client.acquireAccount(&core.ChatRequest{Model: "m1"}, ""); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	_, err := f.client.acquireAccount(&core.ChatRequest{Model: "m1"}, "")
	if !errors.Is(err, core.ErrBusy) {
		t.Fatalf("exhausted guard = %v, want core.ErrBusy（全满时 429 而不是失败，issue #179）", err)
	}
}

func TestNormalConversationNeverTripsTheGuard(t *testing.T) {
	// 反向钉子：默认 cap=20 下，每分钟 1-3 次、连聊 10 分钟 = 最多 30 发
	// …不，真实对话是 10 分钟 ~10 发。这里模拟 10 分钟 10 发，必须全通过。
	f := newRateFixture(t, 0) // 默认 cap=20
	now := time.Now()
	for i := 0; i < 10; i++ {
		ts := now.Add(time.Duration(i) * time.Minute)
		if !f.client.pool.rate.check("a1", ts) {
			t.Fatalf("normal conversation tick %d tripped the guard（反向钉子：正常流量不得触发，issue #179）", i+1)
		}
		f.client.pool.rate.record("a1", ts)
	}
}

// rateFixture is one client with two ready accounts and a guard of the given
// cap (0 = the default).
type rateFixture struct {
	client  *Client
	firstID string
}

func newRateFixture(t *testing.T, cap int) *rateFixture {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"base_url": "http://127.0.0.1:1", "portal_url": "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := New(core.Deps{DataDir: t.TempDir(), Config: raw, Logf: func(string, ...any) {}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	client, ok := got.(*Client)
	if !ok {
		t.Fatalf("New returned %T, want *Client", got)
	}
	f := &rateFixture{client: client}
	now := time.Now()
	for _, id := range []string{"a1", "a2"} {
		f.client.pool.upsert(accountRecord{
			ID: id, Label: id, AccessToken: "tok-" + id, Enabled: true,
			AddedAt: now.UTC().Format(time.RFC3339),
		})
	}
	f.client.pool.rate = newRateGuard(cap)
	f.firstID = "a1"
	return f
}
