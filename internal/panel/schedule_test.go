package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"client2api/internal/core"
	"client2api/internal/scheduler"
)

// fakeScheduler is the timetable's read side, reduced to what the panel asks of
// it.  The panel never starts a batch itself -- that is the scheduler's job --
// so Status is the whole surface, plus the one write it does own: the balance
// refresh behind balance_all, which is not a batch and has nowhere else to go.
type fakeScheduler struct {
	st scheduler.Status
	// cfg and history are what Config()/History() answer; the tasks-centre
	// schedule view is the only reader.
	cfg     scheduler.Config
	history []scheduler.RunRecord
	// noBalance makes RunBalanceRefreshNow report that the host never wired a
	// balance hook, which is the 501 case.  The zero value is the wired case,
	// because that is what a real process has.
	noBalance bool
	// balanceCalls counts presses, so a test can prove the route reached the
	// scheduler rather than merely answering 200.
	balanceCalls int
}

func (f *fakeScheduler) Status() scheduler.Status { return f.st }

func (f *fakeScheduler) Config() scheduler.Config { return f.cfg }

func (f *fakeScheduler) History() []scheduler.RunRecord { return f.history }

func (f *fakeScheduler) RunBalanceRefreshNow(ctx context.Context) bool {
	f.balanceCalls++
	return !f.noBalance
}

func statusPanel(t *testing.T, s Scheduler) *panel {
	t.Helper()
	return &panel{
		opts: Options{
			Version:     "test",
			Listen:      "127.0.0.1:0",
			Started:     time.Now().Add(-time.Minute),
			AuthEnabled: false,
			Scheduler:   s,
		},
		chores: newTaskQueues(),
	}
}

func getStatus(t *testing.T, p *panel) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	p.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/panel/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	return out
}

// TestStatusReportsTheTimetable is what makes the unattended work visible.  A
// schedule that runs but cannot be seen is indistinguishable from a schedule
// that was never wired up -- which is exactly how the shared automation went
// unnoticed in the first place.
func TestStatusReportsTheTimetable(t *testing.T) {
	next := time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
	p := statusPanel(t, &fakeScheduler{st: scheduler.Status{
		Enabled: true,
		Next:    map[string]time.Time{"checkin": next},
	}})

	out := getStatus(t, p)
	raw, ok := out["schedule"]
	if !ok {
		t.Fatalf("status carried no schedule: %v", out)
	}
	sched, _ := raw.(map[string]any)
	if sched["enabled"] != true {
		t.Errorf("enabled = %v, want true", sched["enabled"])
	}
	got, _ := sched["next"].(map[string]any)
	if _, ok := got["checkin"]; !ok {
		t.Errorf("the next check-in fire is missing: %v", sched["next"])
	}
}

// TestStatusOmitsTheScheduleWhenNoneIsWired keeps "automation is off" distinct
// from "this build has no automation": a caller must be able to tell them
// apart, and an empty object would hide the difference.
func TestStatusOmitsTheScheduleWhenNoneIsWired(t *testing.T) {
	p := statusPanel(t, nil)
	out := getStatus(t, p)
	if _, present := out["schedule"]; present {
		t.Fatalf("an unwired panel invented a schedule: %v", out["schedule"])
	}
}

// TestStatusSurvivesASchedulerThatIsOff checks that a configured-but-disabled
// timetable is still reported, carrying Enabled:false.  The operator turned it
// off deliberately, so the panel should say so rather than say nothing.
func TestStatusSurvivesASchedulerThatIsOff(t *testing.T) {
	p := statusPanel(t, &fakeScheduler{st: scheduler.Status{Enabled: false}})
	out := getStatus(t, p)
	raw, ok := out["schedule"]
	if !ok {
		t.Fatalf("a disabled timetable was hidden: %v", out)
	}
	sched, _ := raw.(map[string]any)
	if sched["enabled"] != false {
		t.Errorf("enabled = %v, want false", sched["enabled"])
	}
}

// ---------------------------------------------------------------------------
// 任务中心的「定时任务」盒：GET /panel/api/schedule。
//
// 这一块的关键契约是“每个平台各自一行”：同一批 checkin，两个平台可以有不
// 同的时点与不同的下一跳，而共享时间表只是没写自定义时的回退值。
// ---------------------------------------------------------------------------

func scheduleView(t *testing.T, p *panel) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	p.handleSchedule(rec, httptest.NewRequest(http.MethodGet, "/panel/api/schedule", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("schedule = %d (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	return out
}

func TestScheduleViewReportsEachPlatformsTimetable(t *testing.T) {
	next := time.Date(2026, 3, 4, 7, 0, 0, 0, scheduler.CST)
	c := newSweepClient("wb", core.TaskResult{OK: true}, liveAccount("a1"), parkedAccount("a2"))
	p := statusPanel(t, &fakeScheduler{
		st: scheduler.Status{Enabled: true, NextByClient: map[string]time.Time{"wb/checkin": next}},
		cfg: scheduler.Config{
			Enabled: true,
			Checkin: scheduler.Group{Enabled: true, Hours: []int{9}},
			Clients: map[string]map[string]scheduler.Group{
				"wb": {"checkin": {Enabled: true, Hours: []int{7, 19}}},
			},
		},
	})
	p.opts.Registry = registryOf(c)

	out := scheduleView(t, p)
	if out["wired"] != true {
		t.Fatalf("wired = %v, want true", out["wired"])
	}
	rows, _ := out["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want one (wb, checkin) row", out["rows"])
	}
	row, _ := rows[0].(map[string]any)
	if row["client"] != "wb" || row["batch"] != "checkin" {
		t.Errorf("row = %v, want wb/checkin", row)
	}
	if row["override"] != true {
		t.Errorf("override = %v, want true: the platform has its own hours", row["override"])
	}
	if got, _ := row["hours"].([]any); len(got) != 2 || got[0] != float64(7) || got[1] != float64(19) {
		t.Errorf("hours = %v, want [7 19] (the override, not the shared 9)", row["hours"])
	}
	if row["next"] == "" || row["next"] == nil {
		t.Error("next fire is missing: the page cannot say when this platform runs")
	}
	if row["accounts"] != float64(2) || row["ready"] != float64(1) {
		t.Errorf("accounts/ready = %v/%v, want 2/1", row["accounts"], row["ready"])
	}
}

func TestScheduleViewFallsBackToTheSharedHours(t *testing.T) {
	c := newSweepClient("wb", core.TaskResult{OK: true}, liveAccount("a1"))
	p := statusPanel(t, &fakeScheduler{
		st:  scheduler.Status{Enabled: true},
		cfg: scheduler.Config{Enabled: true, Checkin: scheduler.Group{Enabled: true, Hours: []int{8, 20}}},
	})
	p.opts.Registry = registryOf(c)

	// 全局默认区需要的余额刷新字段必须随 payload 下发，否则迁移后任务中心会丢掉
	// 这一项（以前它在配置页，现在唯一入口在这里）。
	out := scheduleView(t, p)
	if out["balance_refresh_enabled"] != false {
		t.Errorf("balance_refresh_enabled = %v, want the config's false", out["balance_refresh_enabled"])
	}
	if out["balance_refresh_minutes"] != float64(0) {
		t.Errorf("balance_refresh_minutes = %v, want 0 when disabled", out["balance_refresh_minutes"])
	}
	rows, _ := scheduleView(t, p)["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %v, want one", rows)
	}
	row, _ := rows[0].(map[string]any)
	if row["override"] != false {
		t.Errorf("override = %v, want false", row["override"])
	}
	if got, _ := row["hours"].([]any); len(got) != 2 || got[0] != float64(8) || got[1] != float64(20) {
		t.Errorf("hours = %v, want the shared [8 20]", row["hours"])
	}
}

// TestRunOutcomeDistinguishesSuccessFromRefusal pins the fix for a real
// display bug: the scheduled journal hard-coded state="done" for every
// finished run, so a sweep in which every account was refused looked
// exactly like a clean success, and the UI showed no status chip at all.
func TestRunOutcomeDistinguishesSuccessFromRefusal(t *testing.T) {
	cases := []struct {
		name    string
		ran     int
		refused int
		failed  int
		errText string
		want    string
	}{
		{name: "clean success", ran: 3, want: "ok"},
		{name: "all refused", ran: 3, refused: 3, want: "refused"},
		{name: "mixed", ran: 3, refused: 1, want: "partial"},
		{name: "failure", ran: 3, refused: 3, failed: 1, want: "failed"},
		{name: "journal error with one success", ran: 1, errText: "boom", want: "partial"},
		{name: "journal error with nothing run", errText: "boom", want: "failed"},
		{name: "nothing to do", want: "done"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runOutcome(tc.ran, tc.refused, tc.failed, tc.errText); got != tc.want {
				t.Errorf("runOutcome(%d,%d,%d,%q) = %q, want %q", tc.ran, tc.refused, tc.failed, tc.errText, got, tc.want)
			}
		})
	}
}

func TestScheduleViewWithoutASchedulerSaysSo(t *testing.T) {
	out := scheduleView(t, statusPanel(t, nil))
	if out["wired"] != false {
		t.Errorf("wired = %v, want false when no scheduler is wired", out["wired"])
	}
}

// TestScheduleViewMergesTheRunJournal 钉住「运行记录」的两个来源：调度器自己的
// 历史（定时触发）与面板的一键批量（手动），合并成一张按时间倒序的表。
func TestScheduleViewMergesTheRunJournal(t *testing.T) {
	old := time.Date(2026, 3, 4, 7, 0, 0, 0, scheduler.CST)
	newer := old.Add(time.Hour)
	p := statusPanel(t, &fakeScheduler{
		st:  scheduler.Status{Enabled: true},
		cfg: scheduler.Config{Enabled: true},
		history: []scheduler.RunRecord{{
			Client: "wb", Batch: "checkin", Trigger: scheduler.TriggerSchedule,
			Started: old, Duration: time.Second, Accounts: 2, Ran: 3,
		}},
	})
	p.sweeps = newBatchRuns()
	p.sweeps.add(&batchRun{
		Client: "wb", Batch: "travel", State: batchDone,
		StartedAt: newer.Format(time.RFC3339Nano), FinishedAt: newer.Format(time.RFC3339Nano),
	})

	runs, _ := scheduleView(t, p)["runs"].([]any)
	if len(runs) != 2 {
		t.Fatalf("runs = %v, want the scheduled fire and the manual sweep", runs)
	}
	first, _ := runs[0].(map[string]any)
	if first["trigger"] != scheduler.TriggerManual {
		t.Errorf("first run trigger = %v, want the newest (manual) one", first["trigger"])
	}
	second, _ := runs[1].(map[string]any)
	if second["trigger"] != scheduler.TriggerSchedule {
		t.Errorf("second run trigger = %v, want the scheduled one", second["trigger"])
	}
}
