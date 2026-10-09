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
