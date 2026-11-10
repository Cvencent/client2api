package scheduler

import (
	"context"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// #254：checkin 循环的 gap 睡眠在本地跳过判定之前——34 个全部 already-settled
// 的号白等 25m11s（每号 45s），串行 batch 链被整体推迟。修法=先判后睡：纯本地
// 判定（action scope、already settled）挪到 gap 之前，只有真打上游的号才消耗
// gap。反向钉：真跑的号之间的 gap 间隔保持（#142 线速防护不回退）。
// ---------------------------------------------------------------------------

// settledReader answers "done" for every lookup: the all-settled shape from
// the issue (34 accounts, zero upstream calls, 25 minutes of pure sleep).
type settledReader struct{}

func (settledReader) Lookup(platform, account, action string) (core.CheckinStatus, bool) {
	return core.CheckinStatus{State: core.CheckinStateDone}, true
}

func TestAllSettledCheckinSkipsWithoutPacingGaps(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	// checkinOnly 是真走 runCheckinBatchAs 的 fixture（fakeClient 不是
	// CheckinProvider，会早退）。AccountGap 用 issue 的 45s 复现值。
	c := &checkinOnly{
		name: "fake",
		accounts: []core.AccountRecord{
			{ID: "acc1", Enabled: true},
			{ID: "acc2", Enabled: true},
			{ID: "acc3", Enabled: true},
		},
		rec: logs,
	}
	// 通过 planner 批口（checkinOnly 无 BatchPlanner——桥接层会给合成 checkin
	// batch；直接用 batchOf 不行，改走 RunBatchNow 的 planner 路径前先给它一个
	// planner 壳）。
	d := deps(registryOf(c), clk, logs)
	d.CheckinStateOf = settledReader{}
	r := New(d)
	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{8}}})

	rep, ok := r.RunBatchNow(context.Background(), "fake", batchCheckin)
	if !ok {
		t.Fatal("RunBatchNow: ok = false")
	}
	// 全跳过轮：零 gap 睡眠。batch 尾的 settle 是既有行为（fixture 未设
	// Settle → DefaultSettle=5s），不得与 gap 混淆——要断言的是「无 45s 级
	// 的 gap 睡眠」（issue #254）。
	sleeps := clk.durations()
	for _, d := range sleeps {
		if d > 10*time.Second {
			t.Fatalf("sleeps = %v: an all-settled round must not pace a 45s gap per account (issue #254)", sleeps)
		}
	}
	if rep.Skipped != 3 || rep.Ran != 0 {
		t.Fatalf("report = %+v, want skipped=3 ran=0", rep)
	}
}

// 反向钉：真跑的号之间 gap 保持——两号都真打上游时，间隔 ≥ AccountGap。
func TestRealUpstreamCallsKeepTheirGap(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	c := &checkinOnly{
		name: "fake",
		accounts: []core.AccountRecord{
			{ID: "acc1", Enabled: true},
			{ID: "acc2", Enabled: true},
		},
		rec: logs,
	}
	d := deps(registryOf(c), clk, logs)
	r := New(d)
	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{8}}})

	rep, ok := r.RunBatchNow(context.Background(), "fake", batchCheckin)
	if !ok {
		t.Fatal("RunBatchNow: ok = false")
	}
	sleeps := clk.durations()
	if len(sleeps) < 2 {
		t.Fatalf("sleeps = %v, want a gap and a settle", sleeps)
	}
	// checkinOnly 无合成 batch 的 AccountGap 字段 → DefaultAccountGap=45s。
	// 断言 45s ±20%：真跑号之间的 gap 保持（#142 线速防护不得回退）。
	if sleeps[0] < 36*time.Second || sleeps[0] > 54*time.Second {
		t.Fatalf("account gap = %v, want 45s +/-20%% (#142 线速防护不得回退)", sleeps[0])
	}
	if rep.Ran != 2 {
		t.Fatalf("report = %+v, want ran=2", rep)
	}
}
