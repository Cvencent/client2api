package panel

import (
	"net/http"
	"strings"
	"testing"

	"client2api/internal/core"
)

// balancePanel is the smallest panel that can reach the *_all dispatcher: a
// registry, so the client in the path resolves, plus whichever scheduler the
// case is about.
func balancePanel(t *testing.T, s Scheduler, clients ...core.Client) *panel {
	t.Helper()
	return &panel{
		opts: Options{
			Registry:  registryOf(clients...),
			Version:   "test",
			Listen:    "127.0.0.1:0",
			Scheduler: s,
		},
		chores: newTaskQueues(),
	}
}

// TestBalanceAllReachesTheScheduler pins the fix for a real bug: the panel drew
// a 刷新余额 button whose route fell through to the generic batch path, where
// core.BatchOf could only ever say no -- no module plans a batch named
// "balance" -- so the button was a guaranteed 501.  The refresh is not a batch;
// it is the scheduler's own fleet-wide sweep.
func TestBalanceAllReachesTheScheduler(t *testing.T) {
	sched := &fakeScheduler{}
	p := balancePanel(t, sched, &fakeBareClient{name: "workbuddy"})

	rec, out := doTask(t, p, http.MethodPost, "/panel/api/clients/workbuddy/balance_all", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("balance_all = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if sched.balanceCalls != 1 {
		t.Fatalf("RunBalanceRefreshNow calls = %d, want 1", sched.balanceCalls)
	}
	if out["ok"] != true {
		t.Fatalf("body = %v, want ok:true", out)
	}
}

// TestBalanceAllWithoutASchedulerIs501 covers the build that has no timetable
// at all: the verb must say "not implemented" rather than report a refresh that
// never ran.
func TestBalanceAllWithoutASchedulerIs501(t *testing.T) {
	p := balancePanel(t, nil, &fakeBareClient{name: "workbuddy"})

	rec, _ := doTask(t, p, http.MethodPost, "/panel/api/clients/workbuddy/balance_all", "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("balance_all = %d (%s), want 501", rec.Code, rec.Body.String())
	}
}

// TestBalanceAllWithNoHookIs501 covers the build that has a scheduler whose
// host never wired Deps.OnBalanceRefresh.  RunBalanceRefreshNow reports false
// and the route must pass that verdict on instead of answering 200.
func TestBalanceAllWithNoHookIs501(t *testing.T) {
	sched := &fakeScheduler{noBalance: true}
	p := balancePanel(t, sched, &fakeBareClient{name: "workbuddy"})

	rec, _ := doTask(t, p, http.MethodPost, "/panel/api/clients/workbuddy/balance_all", "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("balance_all = %d (%s), want 501", rec.Code, rec.Body.String())
	}
	if sched.balanceCalls != 1 {
		t.Fatalf("RunBalanceRefreshNow calls = %d, want 1", sched.balanceCalls)
	}
}

// TestTheOtherAllVerbsStillTakeTheBatchPath guards the interception: only
// balance_all is diverted, and everything else still resolves to a batch.  The
// bare client plans no batches at all, so a 501 that names the batch is exactly
// the batch path's answer -- and the scheduler must not have been touched.
func TestTheOtherAllVerbsStillTakeTheBatchPath(t *testing.T) {
	sched := &fakeScheduler{}
	p := balancePanel(t, sched, &fakeBareClient{name: "workbuddy"})

	rec, out := doTask(t, p, http.MethodPost, "/panel/api/clients/workbuddy/checkin_all", "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("checkin_all = %d (%s), want 501", rec.Code, rec.Body.String())
	}
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "has no batch named checkin") {
		t.Fatalf("checkin_all message = %q, want the batch path's verdict", msg)
	}
	if sched.balanceCalls != 0 {
		t.Fatalf("checkin_all reached the balance refresh %d time(s)", sched.balanceCalls)
	}
}
