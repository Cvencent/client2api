package panel

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"client2api/internal/core"
)

// taskCheckinClient is the shape MiniMax Code has: the check-in is exposed both
// as an account-row action and as a declared task in the task centre.
type taskCheckinClient struct {
	*sweepClient
	actions []core.CheckinAction
}

func (c *taskCheckinClient) CheckinActions(context.Context) []core.CheckinAction {
	return c.actions
}

func (c *taskCheckinClient) Checkin(context.Context, string, string) (core.CheckinResult, error) {
	return core.CheckinResult{}, fmt.Errorf("declared task unexpectedly fell back to CheckinProvider")
}

func newTaskCheckinClient() *taskCheckinClient {
	const code = "daily-signin"
	base := newSweepClient("minimaxcode", core.TaskResult{
		OK:        true,
		Code:      code,
		AccountID: "a1",
		Message:   "already checked in",
		At:        "2026-10-17T11:50:13+08:00",
	}, liveAccount("a1"))
	base.tasks = []core.TaskInfo{{Code: code, Title: "Daily check-in", Auto: true}}
	base.batches = []core.Batch{fastBatch(core.CheckinBatchName, code)}
	return &taskCheckinClient{
		sweepClient: base,
		actions:     []core.CheckinAction{{ID: code, Label: "Daily check-in"}},
	}
}

func requireCheckinDone(t *testing.T, p *panel, account, action string) {
	t.Helper()
	status, ok := p.checkins.Lookup("minimaxcode", account, action)
	if !ok {
		t.Fatalf("check-in state for %s/%s was not recorded", account, action)
	}
	if status.State != core.CheckinStateDone {
		t.Fatalf("check-in state = %q, want %q", status.State, core.CheckinStateDone)
	}
	if status.Message != "already checked in" {
		t.Fatalf("check-in message = %q, want the vendor result", status.Message)
	}
}

func TestTaskRunRecordsDeclaredCheckinInTheAccountState(t *testing.T) {
	c := newTaskCheckinClient()
	p := taskPanel(t, c)
	p.initCheckins()

	rec, out := doTask(t, p, http.MethodPost, "/panel/api/clients/minimaxcode/tasks/daily-signin/run", `{"account":"a1"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("task run = %d (%s), want 202", rec.Code, rec.Body.String())
	}
	run, _ := out["run"].(map[string]any)
	id, _ := run["id"].(string)
	if id == "" {
		t.Fatalf("task run has no id: %s", rec.Body.String())
	}
	waitRun(t, p, "minimaxcode", id)

	requireCheckinDone(t, p, "a1", "daily-signin")
}

func TestTaskQueueRecordsDeclaredCheckinInTheAccountState(t *testing.T) {
	c := newTaskCheckinClient()
	p := taskPanel(t, c)
	p.initCheckins()

	rec, out := doTask(t, p, http.MethodPost, queuePath("minimaxcode", "run_queue"), "")
	if rec.Code != http.StatusOK || out["started"] != true {
		t.Fatalf("run_queue = %d (%s), want a started queue", rec.Code, rec.Body.String())
	}
	waitQueue(t, p, "minimaxcode")

	requireCheckinDone(t, p, "a1", "daily-signin")
}

func TestBatchSweepRecordsDeclaredCheckinInTheAccountState(t *testing.T) {
	c := newTaskCheckinClient()
	p := batchPanel(t, c)
	p.initCheckins()

	run := runSweep(t, p, c, core.CheckinBatchName)
	if run.Ran != 1 {
		t.Fatalf("batch ran = %d, want 1", run.Ran)
	}

	requireCheckinDone(t, p, "a1", "daily-signin")
}
