package scheduler

import (
	"context"
	"errors"
	"testing"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// #261：accountRecordsOfForBatch 把「无账号管理」「列举失败」「空池」三条语义
// 靠同一个占位 {ID: ""} 表达——合成 checkin 批次消费空池占位后调
// Checkin(ctx, "", "") 必败（doubao 空池实测「unknown account ""」）且
// rep.Accounts=1 假计数。修法 = 占位保留但**checkin 模式**的空池占位带
// poolEmpty 标记区分来源；合成 checkin 执行器识别标记后按 skip 落账。任务
// 路径不动（RunTask("") 的「模块自己挑」合同是真语义）。四条分支全部钉住。
// ---------------------------------------------------------------------------

// errTestListBroken stands in for a real listing failure.
var errTestListBroken = errors.New("listing broken for test")

// bareClient implements only the core.Client surface, so
// core.AsAccountManager reports "no account manager": the placeholder path.
type bareClient struct {
	name string
}

func (b *bareClient) Name() string { return b.name }

func (b *bareClient) Models(ctx context.Context) ([]core.Model, error) { return nil, nil }

func (b *bareClient) Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error) {
	return nil, core.ErrUnsupported
}

func (b *bareClient) Status(ctx context.Context) core.Status {
	return core.Status{Name: b.name, Ready: true}
}

func (b *bareClient) Batches() []core.Batch { return nil }

// TestEmptyPoolPlaceholderIsMarkedOnCheckin 钉住标记的作用域：有 AccountManager
// 的空池在 checkin 模式拿到的占位带 poolEmpty 标记；任务模式保持无标记占位
// （RunTask("") 的「模块自己挑」合同是真语义，fixture 与 doubao.RunTask 都依赖）。
func TestEmptyPoolPlaceholderIsMarkedOnCheckin(t *testing.T) {
	c := &fakeClient{name: "empty-pool"} // accounts nil → Accounts() 返回空
	recs, err := accountRecordsOfForBatch(context.Background(), c, true)
	if err != nil {
		t.Fatalf("list (checkin): %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "" {
		t.Fatalf("checkin 空池应有一个占位，got %+v", recs)
	}
	if !poolEmptyPlaceholder(recs[0]) {
		t.Fatalf("checkin 空池占位没有 poolEmpty 标记：%+v（issue #261）", recs[0])
	}
	// 任务模式：无标记（合同语义不动）。
	recs2, err := accountRecordsOfForBatch(context.Background(), c, false)
	if err != nil {
		t.Fatalf("list (task): %v", err)
	}
	if len(recs2) != 1 || recs2[0].ID != "" || poolEmptyPlaceholder(recs2[0]) {
		t.Fatalf("任务模式空池应保持无标记占位，got %+v（#261 反向钉）", recs2)
	}
}

// TestPlainPlaceholderIsNotPoolEmpty 钉住两条非空池分支不带标记：
// 1. 无账号管理的模块占位（"" 的本来含义——模块自己解析默认凭据）；
// 2. 列举失败的 fallback 占位（列举失败≠空池）。
func TestPlainPlaceholderIsNotPoolEmpty(t *testing.T) {
	bare := &bareClient{name: "bare"}
	recs, err := accountRecordsOfForBatch(context.Background(), bare, false)
	if err != nil {
		t.Fatalf("bare list: %v", err)
	}
	if len(recs) != 1 || recs[0].ID != "" || poolEmptyPlaceholder(recs[0]) {
		t.Fatalf("无账号管理模块应拿到无标记占位，got %+v（#261 反向钉）", recs)
	}

	broken := &fakeClient{name: "listing-broken", accountsErr: errTestListBroken}
	recs2, err := accountRecordsOfForBatch(context.Background(), broken, false)
	if err == nil {
		t.Fatal("列举失败必须把错误带出来")
	}
	if len(recs2) != 1 || recs2[0].ID != "" || poolEmptyPlaceholder(recs2[0]) {
		t.Fatalf("列举失败应保持无标记占位 fallback，got %+v（#261 反向钉）", recs2)
	}
}

// TestSyntheticCheckinSkipsTheEmptyPoolPlaceholder 钉住执行器：空池占位在合成
// checkin 批次里落成 skip（原因写明「模块管理 0 个账号」），不花上游调用、
// 不再假计数 accounts=1 failed=1。
func TestSyntheticCheckinSkipsTheEmptyPoolPlaceholder(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	c := &checkinOnly{name: "empty-checkin", rec: logs} // accounts nil → 空池
	d := deps(registryOf(c), clk, logs)
	r := New(d)
	r.Reconfigure(Config{Enabled: true, Checkin: Group{Enabled: true, Hours: []int{8}}})

	rep, ok := r.RunBatchNow(context.Background(), "empty-checkin", batchCheckin)
	if !ok {
		t.Fatal("RunBatchNow: ok = false")
	}
	if rep.Skipped != 1 || rep.Ran != 0 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want skipped=1 ran=0 failed=0（issue #261：空池是 skip 不是失败）", rep)
	}
	reasons := ""
	for _, s := range rep.SkipReasons {
		reasons += s + ";"
	}
	if !contains(reasons, "manages no accounts") {
		t.Fatalf("skip reason 未写明空池：%q", rep.SkipReasons)
	}
	for _, e := range logs.all() {
		if contains(e, "unknown account") {
			t.Fatalf("空池占位仍被送进了 Checkin：%v（issue #261）", logs.all())
		}
	}
}

// TestTaskPathDefaultAccountStillRuns 钉住任务路径的反向钉：RunTask("") 的
// 「模块自己挑」合同不动——空池的占位照旧无标记地跑（fixture 与 doubao.RunTask
// 的 accountFor 都走这条语义）。
func TestTaskPathDefaultAccountStillRuns(t *testing.T) {
	logs := &recorder{}
	clk := newFakeClock(cstMidnight, logs)
	f := &fakeClient{name: "fake", batches: []core.Batch{oneBatch(batchCheckin, "c1")}, rec: logs}
	d := deps(registryOf(f), clk, logs)
	r := New(d)
	r.Reconfigure(Config{})

	rep, ok := r.RunBatchNow(context.Background(), "fake", batchCheckin)
	if !ok {
		t.Fatal("RunBatchNow: ok = false")
	}
	if rep.Ran != 1 {
		t.Fatalf("report = %+v, want ran=1：RunTask(\"\") 默认账号语义不得回退（#261 反向钉）", rep)
	}
	if got := logs.all(); len(got) < 1 || got[0] != "/c1" {
		t.Fatalf("events = %v, want the default-account run first", got)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
