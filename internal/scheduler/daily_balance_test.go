package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The daily balance refresh is a per-platform synthetic task, like recovery but
// pinned to a fixed hour: the vendors that grant a daily quota top accounts up
// shortly after midnight, so one sweep per day inside the 00:00-00:30 window
// picks up the new credits without the ten-times-an-hour churn a periodic tick
// would cause.

func TestDailyBalancePlanFiresInsideTheMidnightWindow(t *testing.T) {
	rec := &recorder{}
	clk := newFakeClock(time.Date(2026, 3, 4, 12, 0, 0, 0, CST), rec)
	var calls atomic.Int32
	var mu sync.Mutex
	seen := ""
	d := deps(registryOf(&fakeClient{name: "fake"}), clk, rec)
	d.OnDailyBalanceProbe = func(_ context.Context, client string) {
		calls.Add(1)
		mu.Lock()
		seen = client
		mu.Unlock()
	}
	r := New(d)
	r.Reconfigure(Config{Enabled: true, DailyBalance: Group{Enabled: true, Hours: []int{0}}})

	st := r.Status()
	at, ok := st.NextByClient["fake/"+DailyBalanceTaskName]
	if !ok || at.IsZero() {
		t.Fatalf("Status().NextByClient = %v, want fake/%s", st.NextByClient, DailyBalanceTaskName)
	}
	if !at.After(clk.Now()) {
		t.Fatalf("daily balance fire %s is not after now %s", at, clk.Now())
	}
	if loc := at.In(CST); loc.Hour() != 0 || loc.Minute() >= 30 {
		t.Fatalf("daily balance fire at %s, want 00:00..00:30 CST", at)
	}
	// plan and Status must agree on the stored window instead of drawing two
	// independent random offsets that disagree every time they are read.
	for _, f := range r.plan(clk.Now(), r.Config()) {
		if f.client == "fake" && f.batch == DailyBalanceTaskName && !f.at.Equal(at) {
			t.Fatalf("plan daily balance at %s, Status reported %s", f.at, at)
		}
	}
	if !r.RunDailyBalanceNow(context.Background(), "fake") {
		t.Fatal("RunDailyBalanceNow = false, want true when the hook is wired")
	}
	if calls.Load() != 1 {
		t.Fatalf("daily balance hook calls = %d, want 1", calls.Load())
	}
	mu.Lock()
	got := seen
	mu.Unlock()
	if got != "fake" {
		t.Fatalf("daily balance hook client = %q, want fake", got)
	}
	// A manual press is the operator asking for one now; it must not reschedule
	// the automatic fire.
	if after := r.Status().NextByClient["fake/"+DailyBalanceTaskName]; !after.Equal(at) {
		t.Fatalf("manual daily balance moved next from %s to %s", at, after)
	}
}

func TestDailyBalanceDisabledPlansNothing(t *testing.T) {
	rec := &recorder{}
	clk := newFakeClock(cstMidnight, rec)
	d := deps(registryOf(&fakeClient{name: "fake"}), clk, rec)
	d.OnDailyBalanceProbe = func(context.Context, string) {}
	r := New(d)
	r.Reconfigure(Config{Enabled: true, DailyBalance: Group{Enabled: false, Hours: []int{0}}})
	if _, ok := r.Status().NextByClient["fake/"+DailyBalanceTaskName]; ok {
		t.Fatal("disabled daily balance was still planned")
	}
}

func TestRunDailyBalanceNowWithoutAHookSaysSo(t *testing.T) {
	rec := &recorder{}
	clk := newFakeClock(cstMidnight, rec)
	r := New(deps(registryOf(&fakeClient{name: "fake"}), clk, rec))
	if r.RunDailyBalanceNow(context.Background(), "fake") {
		t.Fatal("RunDailyBalanceNow = true without a hook, want false")
	}
}

func TestDailyBalanceSupportsIntervalAndJitter(t *testing.T) {
	rec := &recorder{}
	now := time.Date(2026, 3, 4, 12, 0, 0, 0, CST)
	clk := newFakeClock(now, rec)
	r := New(deps(registryOf(&fakeClient{name: "fake"}), clk, rec))
	r.Reconfigure(Config{Enabled: true, DailyBalance: Group{
		Enabled: true,
		Every:   20 * time.Minute,
		Jitter:  3 * time.Minute,
	}})

	at, ok := r.Status().NextByClient["fake/"+DailyBalanceTaskName]
	if !ok || at.IsZero() {
		t.Fatalf("interval daily balance was not planned: %v", r.Status().NextByClient)
	}
	delay := at.Sub(now)
	if delay < 17*time.Minute || delay > 23*time.Minute {
		t.Fatalf("daily balance delay = %s, want 17m..23m", delay)
	}
}

func TestDailyBalanceSupportsCronExpressions(t *testing.T) {
	rec := &recorder{}
	now := time.Date(2026, 3, 4, 12, 1, 0, 0, CST)
	clk := newFakeClock(now, rec)
	r := New(deps(registryOf(&fakeClient{name: "fake"}), clk, rec))
	r.Reconfigure(Config{Enabled: true, DailyBalance: Group{
		Enabled: true,
		Cron:    "*/20 * * * *",
	}})

	at, ok := r.Status().NextByClient["fake/"+DailyBalanceTaskName]
	if !ok || at.IsZero() {
		t.Fatalf("cron daily balance was not planned: %v", r.Status().NextByClient)
	}
	want := time.Date(2026, 3, 4, 12, 20, 0, 0, CST)
	if !at.Equal(want) {
		t.Fatalf("daily balance cron fire = %s, want %s", at, want)
	}
}

func TestValidateAcceptsCronDailyBalanceAndRejectsMalformedCron(t *testing.T) {
	cfg := Config{Enabled: true, DailyBalance: Group{Enabled: true, Cron: "0 */20 * * * *"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid cron daily balance was rejected: %v", err)
	}
	cfg.DailyBalance.Cron = "not a cron"
	if err := cfg.Validate(); err == nil {
		t.Fatal("malformed cron daily balance was accepted")
	}
	cfg.DailyBalance = Group{Enabled: true, Every: 20 * time.Minute, Jitter: 20 * time.Minute}
	if err := cfg.Validate(); err == nil {
		t.Fatal("jitter equal to the interval was accepted")
	}
	cfg.DailyBalance.Jitter = 3 * time.Minute
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid interval daily balance was rejected: %v", err)
	}
}
