package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"client2api/internal/core"
	"client2api/internal/scheduler"
)

// checkinOnlyClient is the shape the task centre used to drop on the floor: a
// module with accounts and a check-in action, but no BatchPlanner and no
// TaskProvider.  The shared bridge must give it one synthetic "checkin" batch
// and run that batch through CheckinProvider.
type checkinOnlyClient struct {
	*fakeBareClient
	accounts []core.AccountRecord
	actions  []core.CheckinAction
	calls    []string
	res      core.CheckinResult
	err      error
}

func (c *checkinOnlyClient) AccountFields(context.Context) []core.FieldSpec { return nil }

func (c *checkinOnlyClient) Accounts(context.Context) ([]core.AccountRecord, error) {
	return c.accounts, nil
}

func (c *checkinOnlyClient) AddAccount(context.Context, core.AccountSpec) (core.AccountRecord, error) {
	return core.AccountRecord{}, core.ErrUnsupported
}

func (c *checkinOnlyClient) RemoveAccount(context.Context, string) error { return core.ErrUnsupported }

func (c *checkinOnlyClient) SetAccountEnabled(context.Context, string, bool) error {
	return core.ErrUnsupported
}

func (c *checkinOnlyClient) TestAccount(context.Context, string) (core.TestResult, error) {
	return core.TestResult{}, core.ErrUnsupported
}

func (c *checkinOnlyClient) RefreshAccount(context.Context, string) ([]core.RefreshResult, error) {
	return nil, core.ErrUnsupported
}

func (c *checkinOnlyClient) CheckinActions(context.Context) []core.CheckinAction {
	if c.actions != nil {
		return c.actions
	}
	return []core.CheckinAction{{ID: "daily", Label: "每日签到"}}
}

func (c *checkinOnlyClient) Checkin(_ context.Context, id, action string) (core.CheckinResult, error) {
	c.calls = append(c.calls, id)
	if c.err != nil {
		return core.CheckinResult{}, c.err
	}
	res := c.res
	res.AccountID = id
	res.Action = action
	if res.Message == "" && res.Error == "" {
		res.OK = true
	}
	return res, nil
}

func TestCheckinOnlyClientAppearsInTheScheduleRows(t *testing.T) {
	c := &checkinOnlyClient{fakeBareClient: &fakeBareClient{name: "ci"}, accounts: []core.AccountRecord{liveAccount("a1")}}
	p := statusPanel(t, &fakeScheduler{
		st:  scheduler.Status{Enabled: true},
		cfg: scheduler.Config{Enabled: true, Checkin: scheduler.Group{Enabled: true, Hours: []int{8, 20}}},
	})
	p.opts.Registry = registryOf(c)

	rec := httptest.NewRecorder()
	p.handleSchedule(rec, httptest.NewRequest(http.MethodGet, "/panel/api/schedule", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("schedule = %d (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rows, _ := out["rows"].([]any)
	var row map[string]any
	for _, raw := range rows {
		candidate, _ := raw.(map[string]any)
		if candidate["client"] == "ci" && candidate["batch"] == core.CheckinBatchName {
			row = candidate
			break
		}
	}
	if row == nil {
		t.Fatalf("rows = %v, want a synthetic check-in row for ci", out["rows"])
	}
	codes, _ := row["codes"].([]any)
	if len(codes) != 1 || codes[0] != core.CheckinBatchName {
		t.Errorf("codes = %v, want [%s]", row["codes"], core.CheckinBatchName)
	}
}

func TestSyntheticCheckinSweepCallsCheckinForEveryEnabledAccount(t *testing.T) {
	c := &checkinOnlyClient{
		fakeBareClient: &fakeBareClient{name: "ci"},
		accounts:       []core.AccountRecord{liveAccount("a1"), parkedAccount("a2"), liveAccount("a3")},
	}
	p := batchPanel(t, c)

	run := runSweep(t, p, c, core.CheckinBatchName)

	if run.State != batchDone {
		t.Fatalf("state = %q, want %q", run.State, batchDone)
	}
	if run.Ran != 2 || run.Refused != 0 || run.Failed != 0 {
		t.Errorf("ran/refused/failed = %d/%d/%d, want 2/0/0", run.Ran, run.Refused, run.Failed)
	}
	if run.Done != 2 {
		t.Errorf("done = %d, want 2", run.Done)
	}
	if len(c.calls) != 2 || c.calls[0] != "a1" || c.calls[1] != "a3" {
		t.Errorf("Checkin calls = %v, want [a1 a3]", c.calls)
	}
	if len(run.Steps) != 2 || run.Steps[0].Code != core.CheckinBatchName {
		t.Errorf("steps = %+v, want one %q step per enabled account", run.Steps, core.CheckinBatchName)
	}
}

func TestSyntheticCheckinSweepCountsAVendorRefusal(t *testing.T) {
	c := &checkinOnlyClient{
		fakeBareClient: &fakeBareClient{name: "ci"},
		accounts:       []core.AccountRecord{liveAccount("a1")},
		res:            core.CheckinResult{OK: false, Error: "already checked in today"},
	}
	p := batchPanel(t, c)

	run := runSweep(t, p, c, core.CheckinBatchName)

	if run.Refused != 1 || run.Failed != 0 {
		t.Errorf("refused/failed = %d/%d, want 1/0: a vendor refusal is not a failure", run.Refused, run.Failed)
	}
	if len(run.Steps) != 1 || !run.Steps[0].Refused {
		t.Errorf("steps = %+v, want one step marked refused", run.Steps)
	}
}

func TestSyntheticCheckinSweepSkipsAnActionScopedToAnotherChannel(t *testing.T) {
	c := &checkinOnlyClient{
		fakeBareClient: &fakeBareClient{name: "zcode"},
		accounts: []core.AccountRecord{{
			ID:      "api-key-row",
			Label:   "api-key-row",
			Enabled: true,
			Fields:  map[string]any{"kind": "api-key"},
		}},
		actions: []core.CheckinAction{{ID: "claim", Label: "claim", Channels: []string{"jwt"}}},
	}
	p := batchPanel(t, c)

	run := runSweep(t, p, c, core.CheckinBatchName)

	if len(c.calls) != 0 {
		t.Fatalf("Checkin calls = %v, want none for an api-key row", c.calls)
	}
	if run.Skipped != 1 || run.Ran != 0 || run.Refused != 0 || run.Failed != 0 {
		t.Errorf("skipped/ran/refused/failed = %d/%d/%d/%d, want 1/0/0/0", run.Skipped, run.Ran, run.Refused, run.Failed)
	}
	if len(run.Steps) != 1 || !run.Steps[0].Skipped || run.Steps[0].Refused {
		t.Errorf("steps = %+v, want one skipped, non-refused step", run.Steps)
	}
}

func TestBatchStartStillRefusesAPlannerThatCannotRun(t *testing.T) {

	c := &accountOnlyClient{fakeBareClient: &fakeBareClient{name: "planner"}, accounts: []core.AccountRecord{liveAccount("a1")}}
	p := batchPanel(t, c)
	p.opts.Registry = registryOf(c)

	rec := httptest.NewRecorder()
	p.batchStart(rec, httptest.NewRequest(http.MethodPost, "/panel/api/clients/planner/batches/checkin/run", nil), c, "checkin")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("batchStart = %d, want 501 for a planner that cannot run tasks", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "does not run tasks") {
		t.Errorf("body = %s, want the does-not-run-tasks reason", rec.Body.String())
	}
}
