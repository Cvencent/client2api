package loomy

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// abnormal 标记的「有余额读数即自愈」（issue #153）。
//
// 现场形状：chat 失败阶梯走完把账号标成 abnormal（状态 invalid、note
// 「repeated chat failures…」）；选择器跳过 abnormal → 证明会话活着的 chat 200
// 永远打不进来；而操作员实测**点余额能拿到数、点测试能通过**——凭据明明是好的，
// 行却不恢复。与 officeace #126 完全同型的自锁家族：标记的成因（昨夜上游 502）
// 过去了，标记却没有出路。
//
// 修法与 lowCredit 的提升完全对称：applyBalance 在 `credits > 0` 时顺带清
// abnormal（余额读数是模块刚拿到的「这个键没死」的最强证据，#104/#126 同款判据）。
// note 不需要单独清——status 的 note 是从 a.abnormal 派生的（accounts.go:1233），
// 标记清了话术自然消失。限时冷却/会话冷却照旧不归它管（各自的钟不能被误清）。
// ---------------------------------------------------------------------------

// newAbnormalClient 建一个带单个 abnormal 账号的客户端，供两条测试共用
// （走 helpers_test 的 seedAccount，字段沿用仓内既有形状）。
func newAbnormalClient(t *testing.T) *Client {
	t.Helper()
	c := newTestClient(t, "{}", &stubTransport{handler: func(r *http.Request, _ string) *http.Response {
		return jsonResponse(200, okEnvelope("null"))
	}})
	seedAccount(t, c, "loomy-abnormal-1", testToken, 0)
	c.store.mu.Lock()
	a := c.store.all[0]
	a.abnormal = true
	a.failures = 3
	a.cooldownStage = 3
	a.lastError = "upstream 502"
	a.cooldownTill = time.Now().Add(time.Hour)
	a.probeAfter = time.Now().Add(time.Hour)
	c.store.mu.Unlock()
	return c
}

// TestAFundedBalanceReadClearsAbnormal 是 #153 的主判据：读数成功（credits > 0）
// 时 abnormal 被清、惩罚计数与冷却钟一并复位——与 lowCredit 的提升同形。
func TestAFundedBalanceReadClearsAbnormal(t *testing.T) {
	c := newAbnormalClient(t)

	if !c.store.applyBalance("loomy-abnormal-1", 500, c.reserveCreditLimit()) {
		t.Fatal("applyBalance reported no change：abnormal 的自愈路径没有生效（issue #153）")
	}

	c.store.mu.Lock()
	a := c.store.all[0]
	bad := a.abnormal || a.failures != 0 || a.sessionFailures != 0 ||
		a.cooldownStage != 0 || a.lastError != "" ||
		!a.cooldownTill.IsZero() || !a.probeAfter.IsZero()
	snap := accountRecord(a, time.Now())
	c.store.mu.Unlock()
	if bad {
		t.Fatalf("abnormal 清了但惩罚状态没复位：%+v", a)
	}

	// 状态行必须跟着恢复：note 的「repeated chat failures」是从 a.abnormal
	// 派生的，标记清了就不该再出现（报告的建议 3 自动满足）。
	if snap.State == "invalid" {
		t.Errorf("标记清了但状态行仍是 invalid：%q / %q", snap.State, snap.Note)
	}
	if strings.Contains(snap.Note, "repeated chat failures") {
		t.Errorf("note 仍带着 abnormal 的话术：%q（note 应从标记派生，标记清了话术就该消失）", snap.Note)
	}
}

// TestAZeroOrFailedReadDoesNotReviveAbnormal 反向钉子：**零余额读数**（比如
// 恰好在扣费窗口读到 0）不是「这个键没死」的证据，不能清 abnormal——否则一次
// 空钱包读数会把一个真出问题的账号放回轮换。
func TestAZeroOrFailedReadDoesNotReviveAbnormal(t *testing.T) {
	c := newAbnormalClient(t)

	// 返回值只说「lowCredit 有变化」（credits=0 ≤ reserve 会把账号标记为
	// lowCredit——那是另一条路径的正当行为），abnormal 是否被清要看标记本身：
	// 零余额不是「键活着」的证据，不许自愈（issue #153）。
	c.store.applyBalance("loomy-abnormal-1", 0, c.reserveCreditLimit())

	c.store.mu.Lock()
	still := c.store.all[0].abnormal
	c.store.mu.Unlock()
	if !still {
		t.Fatal("abnormal 被零余额读数清掉了：零余额不是「键活着」的证据（issue #153）")
	}
}

// TestBalanceStillLiftsLowCreditOnAbnormalAccount 钉住两条路径**共存**：
// abnormal 账号同时挂着 lowCredit 时，一次有余量的读数应该把两者都清掉
// （自愈路径 + 原有的 lowCredit 提升路径互不遮挡）。
func TestBalanceStillLiftsLowCreditOnAbnormalAccount(t *testing.T) {
	c := newAbnormalClient(t)
	c.store.mu.Lock()
	c.store.all[0].lowCredit = true
	c.store.mu.Unlock()

	if !c.store.applyBalance("loomy-abnormal-1", 500, c.reserveCreditLimit()) {
		t.Fatal("applyBalance reported no change：双标记应该同时被清")
	}
	c.store.mu.Lock()
	a := c.store.all[0]
	bad := a.abnormal || a.lowCredit
	c.store.mu.Unlock()
	if bad {
		t.Fatalf("还有残留标记：%+v", a)
	}
}
