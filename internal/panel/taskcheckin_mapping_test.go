package panel

import (
	"encoding/json"
	"net/http"
	"testing"

	"client2api/internal/core"
)

func TestTaskRunUsesExplicitCheckinActionMapping(t *testing.T) {
	const (
		taskCode = "checkin"
		actionID = "daily-checkin"
	)
	var mapped core.TaskResult
	if err := json.Unmarshal([]byte(`{
		"ok": true,
		"code": "checkin",
		"account_id": "a1",
		"message": "credited",
		"at": "2026-10-17T11:50:13+08:00",
		"checkin_action": "daily-checkin"
	}`), &mapped); err != nil {
		t.Fatalf("unmarshal mapped task result: %v", err)
	}
	base := newSweepClient("workbuddy", mapped, liveAccount("a1"))
	base.tasks = []core.TaskInfo{{Code: taskCode, Title: "Daily check-in", Auto: true}}
	base.batches = []core.Batch{fastBatch(core.CheckinBatchName, taskCode)}
	c := &taskCheckinClient{
		sweepClient: base,
		actions:     []core.CheckinAction{{ID: actionID, Label: "Daily check-in"}},
	}
	p := taskPanel(t, c)
	p.initCheckins()

	rec, out := doTask(t, p, http.MethodPost, "/panel/api/clients/workbuddy/tasks/checkin/run", `{"account":"a1"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("task run = %d (%s), want 202", rec.Code, rec.Body.String())
	}
	run, _ := out["run"].(map[string]any)
	id, _ := run["id"].(string)
	waitRun(t, p, "workbuddy", id)

	status, ok := p.checkins.Lookup("workbuddy", "a1", actionID)
	if !ok || status.State != core.CheckinStateDone || status.Message != "credited" {
		t.Fatalf("mapped check-in state = %+v, found=%v", status, ok)
	}
	if _, ok := p.checkins.Lookup("workbuddy", "a1", taskCode); ok {
		t.Fatalf("task code %q leaked into the account action state", taskCode)
	}
}
