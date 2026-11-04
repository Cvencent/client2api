package openaicompat

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// 零积分账号不再被 chat pick 选中（issue #172）。
//
// #57 让零读数停车（5 分钟 quota 冷却），#24 让它显示「积分不足」——但 chat
// 候选过滤只看冷却钟：每 5 分钟一次「冷却到期 → 真实 401 → 再停 5 分钟」，对
// 空钱包重问永远得到同一个答案。修法：pick 消费 stateHook 的判决，zero 读数
// 的账号直接不进 candidates。反向钉子与正向判据同权重：
//   - -1（not drained）与无读数的账号 hook 答空，照旧可被选中；
//   - 正数读数走 reviveOfficeAceAccount 立即清判决，下一次 pick 即可见。
// ---------------------------------------------------------------------------

// officeAcePickingSource 与生产 wrapOfficeAceSource 的接线一致（含
// bindStateHook），供 pick 测试共用。
func officeAcePickingSource(t *testing.T, srv *httptest.Server) *officeAceSource {
	t.Helper()
	o := officeAcePremiumSource(t, srv)
	o.bindStateHook()
	return o
}

func TestZeroCreditAccountIsNotPickedForChat(t *testing.T) {
	// 主判据（#172）：zero 读数之后，chat 不再向该账号发任何请求——不只是
	// 停 5 分钟，是根本不进候选。
	var chatCalls atomic.Int32
	srv := officeAceChatServer(t, officeAceDrainedSubscriptionJSON, &chatCalls)
	defer srv.Close()

	o := officeAcePickingSource(t, srv)
	id := officeAceAddChatAccount(t, o, srv)

	if _, err := o.AccountBalance(context.Background(), id, 0); err != nil {
		t.Fatalf("balance: %v", err)
	}
	// 无冷却钟帮忙（park 的 5 分钟若已到，旧逻辑会重新打上游）；直接把钟拨走，
	// 剩下唯一的守门人是 pick 的 stateHook 询问。
	o.source.mu.Lock()
	delete(o.source.cooling, id)
	o.source.mu.Unlock()

	err := officeAceChatOnce(t, o.source)
	// 报错形状：zero 账号在 model 匹配之前就被 continue，不置 cooling flag，
	// 落到 ErrNotConfigured（"has no ready API key"）——网关侧它与 ErrBusy 同为
	// candidateUnavailable，照样切平台；语义上「这个平台没有可服务的账号」也更准。
	if !errors.Is(err, core.ErrNotConfigured) {
		t.Fatalf("zero-credit 账号上的 chat = %v, want not-configured（没有账号可服务该模型）", err)
	}
	if got := chatCalls.Load(); got != 0 {
		t.Fatalf("chat calls = %d, want 0：空钱包不该再挨真实 401（issue #172）", got)
	}
}

func TestNotDrainedAccountsRemainPickable(t *testing.T) {
	// 反向钉子：-1（not drained）不得被跳过。无读数路径由下面的子测试覆盖
	// （chat 401 会触发 vendor invalid 判决，与本单无关，所以 -1 用 200 应答）。
	var chatCalls atomic.Int32
	// 与 officeAceChatServer 相同形状，但 chat 应答 200（账号不被打成
	// invalid，pick 每次都能到达厂商）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			chatCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
		case r.Method == http.MethodGet && r.URL.Path == officeAcePremiumPathSubscription:
			_, _ = io.WriteString(w, officeAceDrainedSubscriptionJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o := officeAcePickingSource(t, srv)
	id := officeAceAddChatAccount(t, o, srv)

	// -1（not drained）：显式写进 points 表，hook 仍须答空。maps 可能还没被
	// 读数初始化过，先建再写（与 loadOfficeAcePoints 的懒初始化一致）。
	o.pointsMu.Lock()
	if o.points == nil {
		o.points = map[string]int64{}
	}
	if o.pointsAt == nil {
		o.pointsAt = map[string]time.Time{}
	}
	o.points[id] = -1
	o.pointsAt[id] = time.Now().UTC()
	o.pointsMu.Unlock()

	for i := 1; i <= 2; i++ {
		_ = officeAceChatOnce(t, o.source)
		if got := chatCalls.Load(); got != int32(i) {
			t.Fatalf("not-drained 账号第 %d 次 chat 的 vendor calls = %d：-1 是「没有判决」，不得被 pick 跳过（issue #172）", i, got)
		}
	}
}

func TestUnreadAccountsRemainPickable(t *testing.T) {
	// 反向钉子第二半：无读数的账号不得被跳过（hook 对未知账号答空）。
	var chatCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			chatCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
		case r.Method == http.MethodGet && r.URL.Path == officeAcePremiumPathSubscription:
			_, _ = io.WriteString(w, officeAceDrainedSubscriptionJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	o := officeAcePickingSource(t, srv)
	officeAceAddChatAccount(t, o, srv)

	_ = officeAceChatOnce(t, o.source)
	if got := chatCalls.Load(); got != 1 {
		t.Fatalf("无读数账号的 chat calls = %d, want 1：pick 不得跳过没读过积分的账号（issue #172）", got)
	}
}

func TestPositiveReadingReopensThePickImmediately(t *testing.T) {
	// 正读数路径：zero 停车 → 充值后的读数（>0）走 reviveOfficeAceAccount →
	// hook 答空 → 下一次 pick 立即可见，无需等任何钟。
	var chatCalls atomic.Int32
	srv := officeAceChatServer(t, officeAceDrainedSubscriptionJSON, &chatCalls)
	defer srv.Close()

	o := officeAcePickingSource(t, srv)
	id := officeAceAddChatAccount(t, o, srv)

	if _, err := o.AccountBalance(context.Background(), id, 0); err != nil {
		t.Fatalf("zero balance: %v", err)
	}
	// 充值：直接走 noteOfficeAcePoints 记一个正数读数，语义与厂商刷新后的
	// 读数完全一致（reviveOfficeAceAccount 就是在 note 的 >0 分支被调用的）。
	o.noteOfficeAcePoints(id, 784, time.Now().UTC())

	o.source.mu.Lock()
	delete(o.source.cooling, id)
	o.source.mu.Unlock()
	_ = officeAceChatOnce(t, o.source)
	if got := chatCalls.Load(); got != 1 {
		t.Fatalf("positive-reading 账号的 chat calls = %d, want 1：正读数应立即回到可选中（issue #172）", got)
	}
}
