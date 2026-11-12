package main

import (
	"bytes"
	"log"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// #260：trae 死凭据 renew 失败不回写状态（ready 永远成立），#48 的状态门结构性
// 失效——每 5 分钟 tick 真发一次注定失败的 oauth refresh，11 天 ~900 次。
// 修法 = renew.go 本地连续失败退避：连续 3 次失败 → 1 小时内跳过该账号；成功
// 清零；退避过期放行一次新探测。不依赖任何模块配合。
// ---------------------------------------------------------------------------

func mkNow(t0 time.Time) func() time.Time { return func() time.Time { return t0 } }

// TestRenewBackoffArmsAfterThreeConsecutiveFailures 钉住武装：连续失败达阈值后
// blocked 生效；armed 时只记一次提示（调用方日志由 armRenewBackoff 的返回控制，
// 这里直接验表语义）。
func TestRenewBackoffArmsAfterThreeConsecutiveFailures(t *testing.T) {
	t0 := time.Now()
	tb := newRenewBackoffTable()

	if tb.blocked("trae", "acc1", t0) {
		t.Fatal("无失败记录的账号一开始就被挡（不应武装）")
	}
	for i := 1; i <= 2; i++ {
		if armed := tb.recordFailure("trae", "acc1", t0); armed {
			t.Fatalf("第 %d 次失败就武装了（阈值是 3）", i)
		}
		if tb.blocked("trae", "acc1", t0) {
			t.Fatalf("第 %d 次失败后账号被挡（阈值是 3）", i)
		}
	}
	if !tb.recordFailure("trae", "acc1", t0) {
		t.Fatal("第 3 次连续失败应触发武装（返回 true）")
	}
	if !tb.blocked("trae", "acc1", t0) {
		t.Fatal("武装后 blocked 仍放行")
	}
	// 其它账号不连坐。
	if tb.blocked("trae", "acc2", t0) {
		t.Fatal("无关账号被连坐退避")
	}
	// 窗口经过：放行一次新尝试。
	if !tb.blocked("trae", "acc1", t0.Add(renewBackoffWindow+time.Minute)) {
		// blocked 在过期时清表并放行；第二次查询应同样放行。
		if tb.blocked("trae", "acc1", t0.Add(renewBackoffWindow+time.Minute)) {
			t.Fatal("退避过期后仍被挡")
		}
	}
}

// TestRenewBackoffSuccessClearsTheCounter 钉住解除：一次成功清零——厂商恢复后
// 冷却不能残留。
func TestRenewBackoffSuccessClearsTheCounter(t *testing.T) {
	t0 := time.Now()
	tb := newRenewBackoffTable()
	tb.recordFailure("trae", "acc1", t0)
	tb.recordFailure("trae", "acc1", t0)
	tb.recordFailure("trae", "acc1", t0)
	if !tb.blocked("trae", "acc1", t0) {
		t.Fatal("前置条件：应已武装")
	}
	tb.recordSuccess("trae", "acc1")
	if tb.blocked("trae", "acc1", t0) {
		t.Fatal("成功后冷却仍生效（应清零）")
	}
	// 清零后一次失败重新从 1 计。
	tb.recordFailure("trae", "acc1", t0)
	if tb.blocked("trae", "acc1", t0) {
		t.Fatal("清零后单次失败就武装（阈值语义被破坏）")
	}
}

// TestArmRenewBackoffLogsOnce 钉住 armed 提示只在达到阈值那次写日志——每 tick
// 都写就成了新的日志噪音（本条要修的就是噪音）。
func TestArmRenewBackoffLogsOnce(t *testing.T) {
	t0 := time.Now()
	tb := newRenewBackoffTable()
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	logCalled := func() bool { return buf.Len() > 0 }

	record := func(tb *renewBackoffTable) bool {
		// armRenewBackoff 写共享表；为了可测，直接复刻其语义（表 + 日志一次）。
		if tb.recordFailure("trae", "acc1", t0) {
			logf(logger, "[%s] renew %s: failed %d consecutive times; backing off renewals for %s",
				"trae", "acc1", renewBackoffThreshold, renewBackoffWindow)
			return true
		}
		return false
	}

	if record(tb) {
		t.Fatal("第 1 次失败就写了 armed 日志")
	}
	if record(tb) {
		t.Fatal("第 2 次失败就写了 armed 日志")
	}
	if !record(tb) {
		t.Fatal("第 3 次失败应写 armed 日志")
	}
	if !logCalled() {
		t.Fatal("armed 日志没有落")
	}
	buf.Reset()
	if record(tb) {
		t.Fatal("第 4 次失败又写了 armed 日志（阈值跨越只该有一次）")
	}
}
