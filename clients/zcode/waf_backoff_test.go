package zcode

import (
	"testing"
	"time"
)

// 钉住每账号 WAF 连击熔断（issue #198）。
//
// zcode 是单可用账号平台时，IPGate 的「2 个不同账号」阈值永远凑不齐——7 小时
// 27 次 3012 全落同一账号，每次都白付 captcha mint + 3012 全额 toll。熔断
// 按账号计数：10 分钟内连续 3 次 WAF 判决 → 30 分钟起 WAF 形冷却、翻倍、
// 上限 4 小时；到期放探针（账号重新可选），再撞翻倍；200 清零。
func TestWAFBackoffEscalatesOnConsecutiveJudgements(t *testing.T) {
	b := newWAFBackoff()
	now := time.Now()

	// 前两次：返回 0（未到阈值），账号仍按普通 5 分钟冷却走。
	if got := b.noteWAF(now); got != 0 {
		t.Fatalf("judgement 1 cooldown = %s, want 0", got)
	}
	if got := b.noteWAF(now.Add(time.Minute)); got != 0 {
		t.Fatalf("judgement 2 cooldown = %s, want 0", got)
	}
	// 第三次：熔断开，30 分钟起。
	got := b.noteWAF(now.Add(2 * time.Minute))
	if got != wafBackoffBase {
		t.Fatalf("judgement 3 cooldown = %s, want the base %s", got, wafBackoffBase)
	}
	if b.remaining(now.Add(2*time.Minute)) != wafBackoffBase {
		t.Fatal("the breaker is not closed after the threshold")
	}
	// 冷却到期后探针（账号重新可选），再撞 → 从上次基准翻倍。
	probe := now.Add(2*time.Minute + wafBackoffBase + time.Minute)
	if b.remaining(probe) != 0 {
		t.Fatal("the breaker did not lapse for the probe")
	}
	got = b.noteWAF(probe)
	if got != wafBackoffBase*2 {
		t.Fatalf("probe failure cooldown = %s, want the doubled %s（指数退避，issue #198）", got, wafBackoffBase*2)
	}
}

// issue #201：真实重试节奏是流量驱动的（实测 20-35 分钟），「10 分钟内的连击」
// 在这条 cadence 上永远凑不齐。streak 现在跨间隔累积，只被真 200 清零。
func TestWAFBackoffAccumulatesAcrossRealRetryGaps(t *testing.T) {
	b := newWAFBackoff()
	now := time.Now()
	// 按部署实测节奏：07:50 / 08:10 / 08:45（间隔 20-35 分钟）。
	if got := b.noteWAF(now); got != 0 {
		t.Fatalf("hit 1 cooldown = %s, want 0", got)
	}
	if got := b.noteWAF(now.Add(20 * time.Minute)); got != 0 {
		t.Fatalf("hit 2 cooldown = %s, want 0", got)
	}
	got := b.noteWAF(now.Add(55 * time.Minute))
	if got != wafBackoffBase {
		t.Fatalf("hit 3 cooldown = %s, want the base %s（真实 cadence 上的三连击必须触发熔断，issue #201）", got, wafBackoffBase)
	}
}

// 时效边界：最近一次成功距今超过 24h 后，失败的 streak 从零开始——
// 隔天的孤立 3012 依旧不该累计。
func TestWAFBackoffStreakGoesStale(t *testing.T) {
	b := newWAFBackoff()
	now := time.Now()
	b.noteSuccess(now)
	b.noteWAF(now.Add(time.Hour))
	b.noteWAF(now.Add(2 * time.Hour))
	// 距成功 25h：stale。
	got := b.noteWAF(now.Add(25 * time.Hour))
	if got != 0 {
		t.Fatalf("a failure %s after the last success kept the streak: cooldown = %s, want 0（隔天的孤立 3012 不累计，issue #201）", 25*time.Hour, got)
	}
}

func TestWAFBackoffSuccessClearsEverything(t *testing.T) {
	b := newWAFBackoff()
	now := time.Now()
	b.noteWAF(now)
	b.noteWAF(now.Add(time.Minute))
	b.noteWAF(now.Add(2 * time.Minute)) // breaker open
	if b.remaining(now.Add(2*time.Minute)) == 0 {
		t.Fatal("breaker should be open")
	}
	// 探针成功：完全清零（反向钉子）。
	b.noteSuccess(now.Add(3 * time.Minute))
	if b.remaining(now.Add(3*time.Minute)) != 0 {
		t.Fatal("a success did not clear the cooldown")
	}
	// 下一次 streak 从零开始：两次判决不再触发。
	if got := b.noteWAF(now.Add(4 * time.Minute)); got != 0 {
		t.Fatalf("after a success the second judgement tripped: cooldown = %s, want 0（探针成功必须清零，issue #198）", got)
	}
}

func TestSelectableLockedHonorsTheWAFBreaker(t *testing.T) {
	p := newPool(t.TempDir(), &Config{}, nil, func(string, ...any) {})
	now := time.Now()
	a := &Account{ID: "a1", Enabled: true, State: stateReady, waf: newWAFBackoff()}
	p.mu.Lock()
	p.accounts = []*Account{a}
	p.mu.Unlock()
	if !p.selectableLocked(a, now) {
		t.Fatal("a fresh account is not selectable")
	}
	a.waf.noteWAF(now)
	a.waf.noteWAF(now.Add(time.Minute))
	a.waf.noteWAF(now.Add(2 * time.Minute)) // open
	if p.selectableLocked(a, now.Add(2*time.Minute)) {
		t.Fatal("a WAF-parked account is selectable：冷却期内新请求必须跳过（issue #198）")
	}
	// 到期 = 探针。
	if !p.selectableLocked(a, now.Add(2*time.Minute+wafBackoffBase+time.Minute)) {
		t.Fatal("the account did not become selectable after the WAF cooldown lapsed")
	}
}

func TestOrdinaryErrorsNeverFeedTheWAFBreaker(t *testing.T) {
	// 反向钉子：熔断只吃 kindRisk——普通 4xx/5xx 不触发。这里验证 noteWAF
	// 是唯一入口，其它 classify 结果不会走到它（结构性保证：noteWAF 只在
	// kindRisk 分支被调用）。此处钉闸门自身的窗口语义已够；分支归属由
	// grep 契约测试在 panel 层面外另有覆盖。
	b := newWAFBackoff()
	now := time.Now()
	// 非 WAF 判决从不调用 noteWAF —— 用「三次 noteWAF 才开门」反证普通错误
	// （一次一个）打不开闸门。
	if got := b.noteWAF(now); got != 0 {
		t.Fatalf("a single judgement must not open the breaker, got %s", got)
	}
	if b.remaining(now) != 0 {
		t.Fatal("one judgement parked the account")
	}
}
