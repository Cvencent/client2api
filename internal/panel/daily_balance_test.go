package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/scheduler"
)

// The daily balance refresh shows up in the tasks centre exactly like recovery:
// one synthetic row per registered platform, whether or not the module plans any
// daily batch of its own (a module with only an account pool still has balances
// to top up after midnight).

func TestScheduleViewIncludesDailyBalanceForEveryRegisteredClient(t *testing.T) {
	wb := newSweepClient("wb", core.TaskResult{OK: true}, liveAccount("a1"))
	loomy := newSweepClient("loomy", core.TaskResult{OK: true}, liveAccount("l1"))
	loomy.batches = nil // the daily balance sweep must not depend on a batch plan
	nextWB := time.Date(2026, 3, 5, 0, 12, 0, 0, scheduler.CST)
	nextLoomy := nextWB.Add(7 * time.Minute)
	p := statusPanel(t, &fakeScheduler{
		st: scheduler.Status{Enabled: true, NextByClient: map[string]time.Time{
			"wb/daily_balance":    nextWB,
			"loomy/daily_balance": nextLoomy,
		}},
		cfg: scheduler.Config{
			Enabled:      true,
			DailyBalance: scheduler.Group{Enabled: true, Hours: []int{0}},
		},
	})
	p.opts.Registry = registryOf(wb, loomy)

	rows := p.scheduleRows(p.opts.Scheduler.Config(), p.opts.Scheduler.Status())
	got := map[string]scheduleRow{}
	for _, row := range rows {
		if row.Batch == scheduler.DailyBalanceTaskName {
			got[row.Client] = row
		}
	}
	for _, client := range []string{"wb", "loomy"} {
		row, ok := got[client]
		if !ok {
			t.Fatalf("%s has no daily_balance row: %+v", client, rows)
		}
		if got, want := row.Hours, []int{0}; len(got) != 1 || got[0] != want[0] {
			t.Fatalf("%s daily_balance hours = %v, want [0]", client, row.Hours)
		}
		if !row.GroupEnabled {
			t.Fatalf("%s daily_balance row is not enabled", client)
		}
		if row.Next == "" {
			t.Fatalf("%s daily_balance next fire is missing", client)
		}
	}
}

func TestDailyBalanceRunRouteUsesSchedulerHook(t *testing.T) {
	c := newSweepClient("wb", core.TaskResult{OK: true}, liveAccount("a1"))
	s := &fakeScheduler{}
	p := statusPanel(t, s)
	p.opts.Registry = registryOf(c)

	rec := httptest.NewRecorder()
	p.handleClientScoped(rec, httptest.NewRequest(http.MethodPost, "/panel/api/clients/wb/batches/daily_balance/run", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("daily_balance run = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if s.dailyCall != "wb" {
		t.Fatalf("daily balance hook client = %q, want wb", s.dailyCall)
	}
}

func TestDailyBalanceRunRouteWithoutAHookAnswers501(t *testing.T) {
	c := newSweepClient("wb", core.TaskResult{OK: true}, liveAccount("a1"))
	s := &fakeScheduler{noDaily: true}
	p := statusPanel(t, s)
	p.opts.Registry = registryOf(c)

	rec := httptest.NewRecorder()
	p.handleClientScoped(rec, httptest.NewRequest(http.MethodPost, "/panel/api/clients/wb/batches/daily_balance/run", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("daily_balance run without hook = %d (%s), want 501", rec.Code, rec.Body.String())
	}
}
