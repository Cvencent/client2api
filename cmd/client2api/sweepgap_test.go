package main

import (
	"context"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// 余额扫描的账号间隔（issue #142）。
//
// 现象是 16:11:32 boot 之后第一次 sweep，10 个 loomy 账号在 ~1 秒内全部吃
// 502 —— 不是稳态过频（ready 账号 5 个/5 分钟 = 60 probes/h），而是 boot 后
// 所有账号状态全新、软扫一次把整个池子按线速打给厂商。批量任务早在
// scheduler.DefaultAccountGap 里为同一件事设了 45s（「never fire two accounts
// back to back」），余额 sweep 是 0。
//
// 这一组钉两件事，缺一件修复就不成立：
//  1. 循环里**真的有**停顿，而且停顿在两次探测**之间**（第一次不被推迟）；
//  2. 这个停顿是**可配置**的，0 表示关闭（否则测试和其它调用点没法关掉它）。
//
// 第 2 点是 #142 与「直接写死一个 sleep」的区别：写死的间隔在测试里就是死等，
// 而没有看守的话，下一个人把它删掉也没人知道。
// ---------------------------------------------------------------------------

// countingSweepClient 记录每次 AccountBalance 被调用的时刻。
type countingSweepClient struct {
	*renewFakeClient
	calls []time.Time
}

func (f *countingSweepClient) AccountBalance(_ context.Context, id string, _ time.Duration) (core.Balance, error) {
	f.calls = append(f.calls, time.Now())
	return core.Balance{Credits: 1, Total: 10}, nil
}

// TestTheBalanceSweepPausesBetweenAccounts 是 #142 的主判据：A/B 对照。
//
// 同一段代码、同一批账号，只改间隔这一个变量：有间隔时必须能被量到，没有
// 间隔时必须量不到。只断言「有间隔」而不做反向对照，等于没有证明「是间隔
// 造成的」——中间任何一次上游调用变慢都会让它意外通过。
func TestTheBalanceSweepPausesBetweenAccounts(t *testing.T) {
	accounts := []core.AccountRecord{
		{ID: "a1", Enabled: true, State: "ready"},
		{ID: "a2", Enabled: true, State: "ready"},
		{ID: "a3", Enabled: true, State: "ready"},
		{ID: "a4", Enabled: true, State: "ready"},
	}

	run := func(gap time.Duration) time.Duration {
		setSweepAccountGap(gap)
		defer setSweepAccountGap(0) // 这是包级单元，别漏到别的测试里

		f := &countingSweepClient{renewFakeClient: &renewFakeClient{name: "loomy", accounts: accounts}}
		reg := core.NewRegistry()
		reg.Add(f)

		start := time.Now()
		refreshBalances(context.Background(), reg, 0, quietLogger(), nil)
		elapsed := time.Since(start)
		if len(f.calls) != len(accounts) {
			t.Fatalf("gap=%s: 扫了 %d 个账号，期望 %d", gap, len(f.calls), len(accounts))
		}
		// 第一次探测不被推迟：间隔在两次之间，不在开头。boot 首扫本来就慢，
		// 再给第一个账号加一段等待只会让「手动点一下刷新」跟着变慢。
		if firstGap := f.calls[0].Sub(start); firstGap > gap/2+10*time.Millisecond {
			t.Errorf("gap=%s: 第一个账号等了 %s —— 间隔应该在两次探测之间，不该推迟第一个", gap, firstGap)
		}
		return elapsed
	}

	// 每账号 30ms、4 个账号 = 最短 3×30ms = 90ms。给足余量（调度抖动、
	// CI 上的慢机器），但断言的是「有没有停顿」而不是精确时长。
	const gap = 30 * time.Millisecond
	spread := run(gap)
	if spread < 3*gap*2/3 {
		t.Fatalf("gap=%s 时整轮只用了 %s（期望至少 %s）：循环里没有账号间隔，boot 首扫仍然是线速（#142）",
			gap, spread, 3*gap*2/3)
	}

	// 反向：0 = 关闭。四个假账号的探测本身是纯内存调用，不该耗到接近 gap 的量级。
	if fast := run(0); fast >= 3*gap*2/3 {
		t.Fatalf("gap=0 时整轮用了 %s：0 没有真正关掉间隔，调用方关不掉它", fast)
	}
}

// TestTheSweepGapIsConfigurable 钉住配置链：pool.sweep_gap → 实时单元。
//
// 这一层漏掉的后果和本仓其它「死配置」一样：操作员在面板上改数字、保存、
// 看到回显，然后什么都没发生。
func TestTheSweepGapIsConfigurable(t *testing.T) {
	// 文件沉默 → 内置默认（几百 ms 级，而不是批量任务那种 45s：目标只是
	// 别线速，不是给厂商的每账号工作配速）。
	if got := sweepGapFromConfig(&fileConfig{}); got != defaultSweepAccountGap {
		t.Fatalf("空配置的间隔 = %s，期望默认 %s", got, defaultSweepAccountGap)
	}
	if defaultSweepAccountGap <= 0 || defaultSweepAccountGap >= 5*time.Second {
		t.Fatalf("默认间隔 %s 不在「几百 ms 级」这个区间：写死一个够不到的值等于没修", defaultSweepAccountGap)
	}

	// 0 是合法值（关掉间隔），不能被 dur() 当成「缺省」换成默认值——这正是
	// cost_explore_interval 用 durAllowZero 的原因，同一个坑。
	cfg := &fileConfig{}
	cfg.Pool.SweepGap = "0"
	if got := sweepGapFromConfig(cfg); got != 0 {
		t.Fatalf("sweep_gap=\"0\" 解析成 %s，期望 0（关闭）：把 0 当空值会换成默认值，操作员关不掉", got)
	}

	cfg.Pool.SweepGap = "2s"
	if got := sweepGapFromConfig(cfg); got != 2*time.Second {
		t.Fatalf("sweep_gap=\"2s\" 解析成 %s，期望 2s", got)
	}

	// 非法值回落到默认，而不是「静默变成 0」——一个拼错的时长把限速关掉，
	// 是这一层最坏的失败方向。
	cfg.Pool.SweepGap = "4oops"
	if got := sweepGapFromConfig(cfg); got != defaultSweepAccountGap {
		t.Fatalf("非法 sweep_gap 解析成 %s，期望回落到默认 %s", got, defaultSweepAccountGap)
	}

	// 实时单元：run() 用它把配置推给调度钩子里的那两个普通函数。
	setSweepAccountGap(150 * time.Millisecond)
	if got := sweepAccountGap(); got != 150*time.Millisecond {
		t.Fatalf("实时单元读回 %s，期望 150ms", got)
	}
	setSweepAccountGap(-time.Second)
	if got := sweepAccountGap(); got != 0 {
		t.Fatalf("负数间隔读回 %s：负数只能是「不停顿」，不能变成等到进程结束", got)
	}
	setSweepAccountGap(0)
}

// TestTheSweepKeepsBothEntryPointsWired 钉住两个入口都带上间隔：软扫（每
// 5 分钟）与恢复扫（每 4 小时、故意含 cooling 账号）。

// 恢复扫一次要问 40+ 个账号，正是 #142 里「更该限速」的那一路。漏掉它，
// 修复只覆盖了半个问题。
func TestTheSweepKeepsBothEntryPointsWired(t *testing.T) {
	accounts := []core.AccountRecord{
		{ID: "r1", Enabled: true, State: "ready"},
		{ID: "c1", Enabled: true, State: "cooling"},
		{ID: "c2", Enabled: true, State: "cooling"},
	}
	// 恢复扫：3 个账号（ready + 两个 cooling），间隔仍然生效。
	setSweepAccountGap(30 * time.Millisecond)
	defer setSweepAccountGap(0)

	f := &countingSweepClient{renewFakeClient: &renewFakeClient{name: "loomy", accounts: accounts}}
	reg := core.NewRegistry()
	reg.Add(f)

	start := time.Now()
	refreshBalancesForClient(context.Background(), reg, "loomy", 0, quietLogger(), true, nil)
	elapsed := time.Since(start)
	if len(f.calls) != 3 {
		t.Fatalf("恢复扫探了 %d 个账号，期望 3（ready + 两个 cooling）", len(f.calls))
	}
	if min := 2 * 30 * time.Millisecond; elapsed < min*2/3 {
		t.Fatalf("恢复扫整轮只用了 %s（期望至少 %s）：恢复扫没接上间隔，4 小时一次的全池突发照旧（#142）",
			elapsed, min)
	}
}
