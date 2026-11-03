package openaicompat

import (
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// coolingError 必须把真实剩余冷却时间交给网关（issue #155）。
//
// 现场形状：30 分钟配额冷却，响应头写「1 秒后重试」（网关的 ErrBusy 常数），
// 紧接着 503 + 「30 秒」（内部跳过窗口的常数）——同一份响应里头与正文互相
// 矛盾，任何遵守 Retry-After 的客户端都会被骗进 30 分钟的重试风暴。
//
// 修法：coolingError 带上 until 的时刻（coolingUntilInstant），实现
// core.RetryAfterHint——网关读到正值就写真实剩余时间，读不到就干脆不写头。
// ---------------------------------------------------------------------------

func TestCoolingErrorCarriesTheRealRemainingWait(t *testing.T) {
	now := time.Now()
	until := now.Add(30 * time.Minute)
	e := &coolingError{
		source:  "qa",
		model:   "foo",
		until:   until.Format(time.RFC3339),
		untilAt: until,
	}

	got := e.RetryAfterHint()
	if got <= 0 {
		t.Fatalf("RetryAfterHint = %s，期望接近 30m 的正值（issue #155）", got)
	}
	// 真实剩余时间在 29m30s 与 30m 之间（测试自身耗一点时间）。
	if got < 29*time.Minute+30*time.Second || got > 30*time.Minute {
		t.Fatalf("RetryAfterHint = %s，期望落在 [29m30s, 30m]（issue #155）", got)
	}

	// 已过期的 until：冷却已结束，0 =「没有提示」——头会被省掉而不是写 0。
	expired := &coolingError{untilAt: now.Add(-time.Minute)}
	if got := expired.RetryAfterHint(); got != 0 {
		t.Fatalf("过期冷却的 RetryAfterHint = %s，期望 0（冷却已结束，不该再让调用方等）", got)
	}

	// 没有时刻可解析（ coolingUntilInstant 的零值回退）：0，头省掉。
	if got := (&coolingError{}).RetryAfterHint(); got != 0 {
		t.Fatalf("无时刻的 RetryAfterHint = %s，期望 0：宁缺勿错（issue #155）", got)
	}

	// nil 安全。
	var e2 *coolingError
	if got := e2.RetryAfterHint(); got != 0 {
		t.Fatalf("nil receiver 的 RetryAfterHint = %s，期望 0", got)
	}
}

// TestCoolingUntilInstantPicksTheSoonestParkedKey 钉住时刻的来源：同一模型
// 多把钥匙都在冷却时，取**最早**解封的那把——调用方等的是「第一个能用」，
// 不是「全部解封」。
func TestCoolingUntilInstantPicksTheSoonestParkedKey(t *testing.T) {
	now := time.Now()
	s := &source{
		name:    "qa",
		now:     func() time.Time { return now },
		cooling: map[string]time.Time{},
	}
	s.mu.Lock()
	s.rows = []storedProvider{
		{ID: "k1", Models: []string{"foo"}},
		{ID: "k2", Models: []string{"foo"}},
	}
	s.cooling = map[string]time.Time{
		"k1": now.Add(10 * time.Minute),
		"k2": now.Add(30 * time.Minute),
	}
	s.mu.Unlock()

	got := s.coolingUntilInstant("foo", now, s.cooling)
	if want := now.Add(10 * time.Minute); !got.Equal(want) {
		t.Fatalf("coolingUntilInstant = %s，期望最早解封的 %s（issue #155）", got, want)
	}

	// 该模型没有候选在冷却：零时刻 → 头会被省掉。
	if got := s.coolingUntilInstant("bar", now, s.cooling); !got.IsZero() {
		t.Fatalf("无冷却候选的 instant = %s，期望零值", got)
	}
}
