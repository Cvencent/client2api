package loomy

import (
	"context"
	"net/http"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// abnormal / expired 的低频自愈旁路（issues #168/#169）。
//
// #153 把「有余额读数即清 abnormal」写进了 applyBalance，#169 的 clearExpiry
// 也现成放在 reset 里——但五条自动路径全部跳过 abnormal/expired，自愈出口只剩
// 操作员的手。修法是给 backgroundProbeDue 开一条低频侧门（abnormal/expired 且
// 非 dead 的账号按 6..12h 的独立时钟放行探测），读数进来后现成的自愈逻辑即通。
//
// 反向钉子与正向判据同权重：
//   - dead（厂商自己拒绝的 100002）不得借道；
//   - 侧门时钟独立于普通恢复时钟（6..12h，不是 3..6h）；
//   - applyBalance 的 funded 读数清 expired 钟并落盘 accounts.json；
//   - 探测失败不清钟（失败走的是读数错误路径，状态原样）。
// ---------------------------------------------------------------------------

// newSelfhealClient 建一个单账号客户端，seedAccount 后由测试自选标记形态。
func newSelfhealClient(t *testing.T) *Client {
	t.Helper()
	rt := &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		return jsonResponse(200, okEnvelope(`{"balance":500}`))
	}}
	c := newTestClient(t, "{}", rt)
	seedAccount(t, c, "loomy-selfheal-1", testToken, 0)
	return c
}

// markAbnormal / markExpired / markDead 把种子账号摆成对应形状。
func markAbnormal(t *testing.T, c *Client) {
	t.Helper()
	c.store.mu.Lock()
	a := c.store.all[0]
	a.abnormal = true
	a.failures = 3
	a.cooldownStage = 3
	a.lastError = "upstream HTTP 502"
	c.store.mu.Unlock()
}

func markExpired(t *testing.T, c *Client, at time.Time) {
	t.Helper()
	c.store.mu.Lock()
	a := c.store.all[0]
	a.ExpiresAtMS = at.UnixMilli()
	c.store.mu.Unlock()
}

func markDead(t *testing.T, c *Client) {
	t.Helper()
	c.store.mu.Lock()
	a := c.store.all[0]
	a.dead = true
	a.lastError = "the vendor rejected this session"
	c.store.mu.Unlock()
}

func TestAbnormalAccountGetsTheLowFrequencySideDoor(t *testing.T) {
	c := newSelfhealClient(t)
	markAbnormal(t, c)

	now := time.Now().UTC()
	// 主判据（#168）：abnormal 不再被一刀切挡掉——侧门在，第一次询问就放行。
	if !c.store.backgroundProbeDue("loomy-selfheal-1", now, backgroundProbeDelay) {
		t.Fatal("abnormal 账号没有探测旁路：自愈出口仍然只有手动刷新（issue #168）")
	}
	// 时钟形状：侧门是 6..12h 的低频，不是普通恢复的 3..6h。
	acc, _ := c.store.lookup("loomy-selfheal-1")
	next := acc.probeAfter.Sub(now)
	if next < 6*time.Hour || next > 12*time.Hour {
		t.Fatalf("abnormal 侧门的第一拍在 %s， want 6h..12h 的低频时钟", next)
	}
	// 时钟生效：第一拍之后、下一拍之前，闸保持关。
	if c.store.backgroundProbeDue("loomy-selfheal-1", now.Add(6*time.Hour-time.Minute), backgroundProbeDelay) {
		t.Fatal("侧门时钟没有生效：第一拍后立刻又放行")
	}
}

func TestExpiredAccountGetsTheLowFrequencySideDoor(t *testing.T) {
	c := newSelfhealClient(t)
	markExpired(t, c, time.Now().UTC().Add(-time.Minute))

	now := time.Now().UTC()
	if !c.store.backgroundProbeDue("loomy-selfheal-1", now, backgroundProbeDelay) {
		t.Fatal("expired 账号没有探测旁路：本地钟错杀的活号永远拿不到自证机会（issue #169）")
	}
	acc, _ := c.store.lookup("loomy-selfheal-1")
	next := acc.probeAfter.Sub(now)
	if next < 6*time.Hour || next > 12*time.Hour {
		t.Fatalf("expired 侧门的第一拍在 %s， want 6h..12h 的低频时钟", next)
	}
}

func TestDeadAccountNeverTakesTheSideDoor(t *testing.T) {
	// 反向钉子：dead 是厂商自己的 100002 拒绝，探测不得把它洗回轮换。
	c := newSelfhealClient(t)
	markDead(t, c)

	now := time.Now().UTC()
	if c.store.backgroundProbeDue("loomy-selfheal-1", now, backgroundProbeDelay) {
		t.Fatal("dead 账号借道了侧门：厂商明确拒绝的会话不得被探测复活（issues #168/#169）")
	}
	// 同样钉 disabled：操作员的显式停车不受影响。
	c.store.mu.Lock()
	c.store.all[0].dead = false
	c.store.all[0].abnormal = true
	c.store.all[0].Enabled = false
	c.store.mu.Unlock()
	if c.store.backgroundProbeDue("loomy-selfheal-1", now, backgroundProbeDelay) {
		t.Fatal("disabled 账号借道了侧门：操作员显式停车优先")
	}
}

func TestOrdinaryRecoveryClockIsUnchangedForOtherStates(t *testing.T) {
	// 不含 abnormal/expired 的普通惩罚账号，走的是原有 3..6h 时钟——侧门
	// 不得改变既有节奏（#52 的排除与其依赖的 quiet window 都原样）。
	c := newSelfhealClient(t)
	now := time.Now().UTC()
	c.store.penalise("loomy-selfheal-1", &apiError{Status: 429, Message: "rate limited"}, now, time.Minute)

	acc, _ := c.store.lookup("loomy-selfheal-1")
	first := acc.probeAfter.Sub(now)
	if first < 3*time.Hour || first > 6*time.Hour {
		t.Fatalf("普通惩罚账号的第一拍在 %s， want 原有的 3h..6h（侧门不得改变既有节奏）", first)
	}
}

func TestAFundedBalanceReadClearsTheLocalExpiry(t *testing.T) {
	// 主判据（#169）：funded 读数证明厂商侧会话活着 → 本地 14 天估算被证伪 →
	// 清钟（ExpiresAtMS=0，落 accounts.json）+ 复位惩罚，账号回轮换。
	c := newSelfhealClient(t)
	expiredAt := time.Now().UTC().Add(-time.Minute)
	markExpired(t, c, expiredAt)

	if !c.store.applyBalance("loomy-selfheal-1", 500, c.reserveCreditLimit()) {
		t.Fatal("applyBalance 报告无变化：expired 的自愈出口没接上（issue #169）")
	}
	c.store.mu.Lock()
	a := c.store.all[0]
	bad := a.ExpiresAtMS != 0 || a.abnormal || a.failures != 0 ||
		!a.cooldownTill.IsZero() || !a.probeAfter.IsZero()
	c.store.mu.Unlock()
	if bad {
		t.Fatal("funded 读数没有清掉本地到期钟：账号仍在 invalid（issue #169）")
	}
	// 钟的清除要落盘（ExpiresAtMS 在 accounts.json，不在 state.json）。
	if loaded, err := loadStore(c.accountsPath, c.statePath); err == nil {
		loaded.mu.Lock()
		var got int64
		for _, a := range loaded.all {
			if a.ID == "loomy-selfheal-1" {
				got = a.ExpiresAtMS
			}
		}
		loaded.mu.Unlock()
		if got != 0 {
			t.Fatalf("清钟没有落盘：磁盘上 ExpiresAtMS=%d，重启后假死复活（issue #169）", got)
		}
	}
}

func TestAnUnfundedReadDoesNotClearTheExpiry(t *testing.T) {
	// 反向钉子：读数为 0（积分耗尽）不得清钟——账号的经济态走 lowCredit，
	// 到期钟只能被「证明会话活着」的 funded 读数清。
	c := newSelfhealClient(t)
	expiredAt := time.Now().UTC().Add(-time.Minute)
	markExpired(t, c, expiredAt)

	c.store.applyBalance("loomy-selfheal-1", 0, c.reserveCreditLimit())
	c.store.mu.Lock()
	got := c.store.all[0].ExpiresAtMS
	c.store.mu.Unlock()
	if got != expiredAt.UnixMilli() {
		t.Fatalf("非 funded 读数清了到期钟（ExpiresAtMS=%d）：只有 funded 读数证明会话活着（issue #169）", got)
	}
}

func TestSideDoorReachesApplyBalanceThroughTheRealBalancePath(t *testing.T) {
	// 端到端（模块内）：abnormal 账号过闸 → AccountBalance 的读数 →
	// applyBalanceGuard → 标记清掉。这是 #153 的修复在 #168 旁路接通后
	// 第一次能被自动路径触达的完整链条。
	c := newSelfhealClient(t)
	markAbnormal(t, c)

	now := time.Now().UTC()
	if !c.store.backgroundProbeDue("loomy-selfheal-1", now, backgroundProbeDelay) {
		t.Fatal("闸没放行，链条第一步就断（issue #168）")
	}
	bal, err := c.AccountBalance(context.Background(), "loomy-selfheal-1", 0)
	if err != nil {
		t.Fatalf("余额读数: %v", err)
	}
	if bal.Credits <= 0 {
		t.Fatalf("stub 应答 funded 读数，拿到 %d", bal.Credits)
	}
	c.store.mu.Lock()
	a := c.store.all[0]
	stillAbnormal := a.abnormal
	c.store.mu.Unlock()
	if stillAbnormal {
		t.Fatal("读数落地后 abnormal 仍在：#153 的自愈逻辑没有被旁路触达（issue #168）")
	}
}

func TestLoomyDeclaresSelfHealableInvalid(t *testing.T) {
	// 网关侧的闸（cmd/client2api balanceProbeEligible 的 invalid 豁免）读的是
	// 这个标记接口：loomy 必须声明，否则 main.go 的软/恢复 sweep 仍会在
	// eligibility 一层把 invalid 行滤掉，旁路只在 panel sweep 生效一半。
	var c core.Client = newSelfhealClient(t)
	if !core.SelfHealsFromBalance(c) {
		t.Fatal("loomy 没有实现 core.SelfHealableInvalid：main.go 的 sweep 仍在 invalid 处截断（issues #168/#169）")
	}
}
