package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// 出站 Retry-After 必须是真实剩余冷却时间，而不是写死的常数（issue #155）。
//
// 现场：30 分钟配额冷却 → 429 头写「1 秒后重试」+ 正文写「cooling down until
// 30 分钟后」；按头重试 → 503 头写「30 秒」（内部跳过窗口被当成调用方该等的
// 时长）。同一份响应自相矛盾，机器读的那个（头）恰好是错的那个，任何遵守
// Retry-After 的客户端都会被骗进 30 分钟的重试风暴。
//
// 规则（`writeUpstreamError`）：
//   - 错误携带 core.RetryAfterHint → 头写真实剩余秒数（向上取整）；
//   - 普通背压（在途上限，已知且短）保留历史上的 1 秒；
//   - 平台耗尽（30s 内部跳过窗口，不是调用方该等的时长）→ 无头，缺省比
//     一个错的短值安全。
// ---------------------------------------------------------------------------

// TestCoolingErrorSurfacesTheRealRemainingWait 是 #155 的主判据：带冷却时刻的
// 错误，响应头必须反映真实剩余时间而不是常数。
func TestCoolingErrorSurfacesTheRealRemainingWait(t *testing.T) {
	until := time.Now().Add(30 * time.Minute)
	// coolingError lives in the openaicompat package; the gateway only sees
	// core.RetryAfterHint.  Build the error through that interface so this
	// test pins the GATEWAY half of the contract.
	busy := &hintedBusy{
		msg:  `[qa] qa has a key for model "foo" but it is cooling down until ` + until.Format(time.RFC3339),
		hint: time.Until(until),
	}
	stats := NewStats()
	usage := NewUsageStore(10)
	srv := newTestServer(t, &testClient{name: "t", chatErr: busy}, stats, usage)

	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	got := rec.Header().Get("Retry-After")
	secs, atoiErr := strconv.Atoi(got)
	if atoiErr != nil {
		t.Fatalf("Retry-After = %q，不是秒数（issue #155）", got)
	}
	// 真实剩余 ≈ 30 分钟：头必须在 [30m, 30m+5s] 区间（向上取整到整秒再加
	// 测试自身耗时），而不是 1 或 30。
	if secs < 30*60 || secs > 30*60+5 {
		t.Fatalf("Retry-After = %q 秒，期望反映 ~30 分钟的真实冷却（issue #155：30 分钟被说成 1 秒/30 秒）", got)
	}
	if !strings.Contains(rec.Body.String(), "cooling down until") {
		t.Errorf("body = %q，期望与头一致的 until 时刻", rec.Body.String())
	}
}

// hintedBusy is a stand-in for openaicompat's coolingError: an ErrBusy that
// knows its own cooldown instant through core.RetryAfterHint.  The gateway
// must not care which module produced it.
type hintedBusy struct {
	msg  string
	hint time.Duration
}

func (e *hintedBusy) Error() string                 { return e.msg }
func (e *hintedBusy) Is(target error) bool          { return target == core.ErrBusy }
func (e *hintedBusy) RetryAfterHint() time.Duration { return e.hint }

// TestPlatformExhaustedCarriesNoRetryAfter 钉住 503 那一半：30 秒的内部跳过
// 窗口不是调用方该等的时长，无提示时宁缺勿错（issue #155）。
func TestPlatformExhaustedCarriesNoRetryAfter(t *testing.T) {
	srv := newTestServer(t, &testClient{name: "t", chatErr: core.ErrPlatformExhausted}, NewStats(), NewUsageStore(10))
	rec := chat(t, srv, bufferedBody)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q，期望无头：30 秒的内部跳过窗口不是调用方该等的时长，"+
			"一个错的短值会把遵守 Retry-After 的客户端骗进整个冷却的重试风暴（issue #155）", got)
	}
}
