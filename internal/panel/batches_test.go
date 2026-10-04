package panel

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// runBatch is the panel's own executor for a scheduled sweep -- not the
// scheduler's -- and until these tests existed nothing called it directly.
//
// What is worth pinning is the counter it keeps, because the panel's run record
// is the only place an operator can see what an unattended sweep actually did.
// The trap is that a module answers "no" two different ways:
//
//   - a vendor refusal is core.TaskResult{OK: false, Error: "..."} with a nil
//     Go error -- the chore simply did not happen, which is normal, and
//   - a transport failure is a real Go error.
//
// runOne copies TaskResult.Error into the step either way, so the two are
// distinguishable only by the step's Refused flag.  The counter switch has to
// test Refused first, or every refusal is reported as a failure and the run
// record contradicts its own step list.
// ---------------------------------------------------------------------------

// sweepClient is the full module shape a sweep needs.  core.Client,
// core.TaskProvider and core.AccountManager come from fakePoolTaskClient; only
// core.BatchPlanner is missing, so the declaration lives here.
type sweepClient struct {
	*fakePoolTaskClient
	batches []core.Batch
}

func (s *sweepClient) Batches() []core.Batch { return s.batches }

// accountOnlyClient lists accounts and declares a batch but cannot run a chore:
// the shape runBatch must reject before it spends a 45-second account gap.
type accountOnlyClient struct {
	*fakeBareClient
	accounts []core.AccountRecord
}

func (a *accountOnlyClient) Batches() []core.Batch {
	return []core.Batch{fastBatch("checkin", "claim")}
}

func (a *accountOnlyClient) AccountFields(context.Context) []core.FieldSpec { return nil }

func (a *accountOnlyClient) Accounts(context.Context) ([]core.AccountRecord, error) {
	return a.accounts, nil
}

func (a *accountOnlyClient) AddAccount(context.Context, core.AccountSpec) (core.AccountRecord, error) {
	return core.AccountRecord{}, nil
}

func (a *accountOnlyClient) RemoveAccount(context.Context, string) error { return nil }

func (a *accountOnlyClient) SetAccountEnabled(context.Context, string, bool) error { return nil }

func (a *accountOnlyClient) TestAccount(context.Context, string) (core.TestResult, error) {
	return core.TestResult{}, nil
}

func (a *accountOnlyClient) RefreshAccount(context.Context, string) ([]core.RefreshResult, error) {
	return nil, nil
}

// fastBatch keeps the pacing knobs out of the test's way.  The executor waits
// AccountGap between two accounts and Settle before declaring victory, and the
// production defaults are 45s and 5s.
func fastBatch(name string, codes ...string) core.Batch {
	return core.Batch{
		Name:       name,
		Codes:      codes,
		AccountGap: time.Millisecond,
		Settle:     time.Millisecond,
	}
}

// newSweepClient is a module that answers every chore with result and plans a
// single "checkin" batch over "claim".
func newSweepClient(name string, result core.TaskResult, accounts ...core.AccountRecord) *sweepClient {
	return &sweepClient{
		fakePoolTaskClient: &fakePoolTaskClient{
			fakeTaskClient: &fakeTaskClient{name: name, result: result},
			accounts:       accounts,
		},
		batches: []core.Batch{fastBatch("checkin", "claim")},
	}
}

func liveAccount(id string) core.AccountRecord {
	return core.AccountRecord{ID: id, Label: id, Enabled: true}
}

func parkedAccount(id string) core.AccountRecord {
	return core.AccountRecord{ID: id, Label: id, Enabled: false}
}

// batchPanel is taskPanel plus the sweep journal.  taskPanel deliberately
// leaves sweeps nil -- the task-board tests never touch a batch -- while
// runBatch writes its whole progress through it, so a batch test must supply
// one or the first mutate nil-derefs.
func batchPanel(t *testing.T, clients ...core.Client) *panel {
	t.Helper()
	p := taskPanel(t, clients...)
	p.sweeps = newBatchRuns()
	return p
}

// runSweep drives one batch through the panel's executor and returns the
// finished record.  It calls runBatch directly instead of POSTing through the
// queue so the assertions never race the background worker.
func runSweep(t *testing.T, p *panel, c core.Client, name string) batchRun {
	t.Helper()
	b, ok := core.BatchOf(c, name)
	if !ok {
		t.Fatalf("%s plans no batch named %q", c.Name(), name)
	}
	run := p.sweeps.add(&batchRun{
		Client:   c.Name(),
		Batch:    b.Name,
		State:    batchQueued,
		QueuedAt: time.Now().Format(time.RFC3339Nano),
	})
	p.runBatch(context.Background(), run.ID, c, b)

	done, ok := p.sweeps.get(run.ID)
	if !ok {
		t.Fatalf("the journal lost run %s", run.ID)
	}
	return done
}

func TestSweepCountsAVendorRefusalAsRefusedNotFailed(t *testing.T) {
	c := newSweepClient("wb", core.TaskResult{
		OK:      false,
		Message: "already claimed today",
		Error:   "already claimed today",
	}, liveAccount("a1"))
	p := batchPanel(t, c)

	run := runSweep(t, p, c, "checkin")

	if run.Ran != 1 {
		t.Fatalf("ran = %d, want 1", run.Ran)
	}
	if run.Refused != 1 {
		t.Errorf("refused = %d, want 1: a vendor refusal is not a failure", run.Refused)
	}
	if run.Failed != 0 {
		t.Errorf("failed = %d, want 0: the module returned a nil error", run.Failed)
	}
	if run.State != batchDone {
		t.Errorf("state = %q, want %q", run.State, batchDone)
	}
	if run.Error != "" {
		t.Errorf("error = %q, want empty: nothing went wrong", run.Error)
	}
	if len(run.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(run.Steps))
	}
	// The step carries BOTH fields -- that is exactly what made the old switch
	// order wrong -- and Refused is the one that has to decide the counter.
	if !run.Steps[0].Refused {
		t.Errorf("the step is not marked refused: %+v", run.Steps[0])
	}
	if run.Steps[0].Error == "" {
		t.Errorf("the refusal lost its reason: %+v", run.Steps[0])
	}
	if run.Credit != 0 {
		t.Errorf("credit = %d, want 0 for a refusal", run.Credit)
	}
}

func TestSweepCountsATransportFailureAsFailed(t *testing.T) {
	c := newSweepClient("wb", core.TaskResult{}, liveAccount("a1"))
	c.runErr = errors.New("dial tcp 10.0.0.1:443: connect: connection refused")
	p := batchPanel(t, c)

	run := runSweep(t, p, c, "checkin")

	if run.Failed != 1 {
		t.Errorf("failed = %d, want 1", run.Failed)
	}
	if run.Refused != 0 {
		t.Errorf("refused = %d, want 0: a transport error is not a vendor refusal", run.Refused)
	}
	if run.Ran != 1 {
		t.Errorf("ran = %d, want 1: the chore was attempted", run.Ran)
	}
	if len(run.Steps) != 1 {
		t.Fatalf("steps = %d, want 1", len(run.Steps))
	}
	if run.Steps[0].Refused {
		t.Errorf("a transport error was marked as a refusal: %+v", run.Steps[0])
	}
	if !strings.Contains(run.Steps[0].Error, "connection refused") {
		t.Errorf("the step lost the error text: %+v", run.Steps[0])
	}
}

func TestSweepCountsASuccessAndItsCredit(t *testing.T) {
	c := newSweepClient("wb", core.TaskResult{
		OK:      true,
		Message: "claimed 20",
		Credit:  20,
	}, liveAccount("a1"))
	p := batchPanel(t, c)

	run := runSweep(t, p, c, "checkin")

	if run.Ran != 1 || run.Refused != 0 || run.Failed != 0 {
		t.Errorf("ran/refused/failed = %d/%d/%d, want 1/0/0", run.Ran, run.Refused, run.Failed)
	}
	if run.Credit != 20 {
		t.Errorf("credit = %d, want the module's 20", run.Credit)
	}
	if run.Done != 1 {
		t.Errorf("done = %d, want 1", run.Done)
	}
}

func TestSweepSkipsParkedAccounts(t *testing.T) {
	c := newSweepClient("wb", core.TaskResult{OK: true},
		liveAccount("a1"), parkedAccount("a2"), liveAccount("a3"))
	p := batchPanel(t, c)

	run := runSweep(t, p, c, "checkin")

	// Accounts is a progress counter, not the account-list size, so it must
	// equal the number of accounts the sweep actually started.
	if run.Accounts != 2 {
		t.Errorf("accounts = %d, want only the two enabled ones", run.Accounts)
	}
	if run.Done != 2 {
		t.Errorf("done = %d, want 2", run.Done)
	}
	if len(run.Steps) != 2 {
		t.Errorf("steps = %d, want one per enabled account", len(run.Steps))
	}
	for _, s := range run.Steps {
		if s.Account == "a2" {
			t.Errorf("the sweep ran a parked account: %+v", s)
		}
	}
}

func TestSweepRecordsAnOutstandingGateAsASkip(t *testing.T) {
	c := newSweepClient("wb", core.TaskResult{OK: true}, liveAccount("a1"))
	// The gate chore is listed and NOT finished, so the batch is not worth
	// running yet.
	c.tasks = []core.TaskInfo{{Code: "buddy", Auto: true}}
	c.batches = []core.Batch{{
		Name:       "travel",
		Codes:      []string{"travel"},
		Gate:       "buddy",
		AccountGap: time.Millisecond,
		Settle:     time.Millisecond,
	}}
	p := batchPanel(t, c)

	run := runSweep(t, p, c, "travel")

	if run.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", run.Skipped)
	}
	if run.Ran != 0 {
		t.Errorf("ran = %d, want 0: the gate was still outstanding", run.Ran)
	}
	if len(run.Steps) != 1 {
		t.Fatalf("steps = %d, want the gate step", len(run.Steps))
	}
	if run.Steps[0].Code != "buddy" || !run.Steps[0].Refused {
		t.Errorf("the gate step is wrong: %+v", run.Steps[0])
	}
}

func TestSweepWithoutTheTaskCapabilityRefusesInOneLine(t *testing.T) {
	c := &accountOnlyClient{
		fakeBareClient: &fakeBareClient{name: "wb"},
		accounts:       []core.AccountRecord{liveAccount("a1")},
	}
	p := batchPanel(t, c)

	run := runSweep(t, p, c, "checkin")

	if run.State != batchDone {
		t.Errorf("state = %q, want %q", run.State, batchDone)
	}
	if !strings.Contains(run.Error, "does not run tasks") {
		t.Errorf("error = %q, want it to name the missing capability", run.Error)
	}
	if run.Ran != 0 || len(run.Steps) != 0 {
		t.Errorf("a sweep with no executor ran something: ran=%d steps=%d", run.Ran, len(run.Steps))
	}
}

func TestBatchViewCountsEveryAccountAndTheEnabledOnes(t *testing.T) {
	c := newSweepClient("wb", core.TaskResult{OK: true},
		liveAccount("a1"), parkedAccount("a2"), liveAccount("a3"))
	p := batchPanel(t, c)

	views := p.batchViews(c, c)
	if len(views) != 1 {
		t.Fatalf("views = %d, want the one declared batch", len(views))
	}
	// This is the OTHER "accounts" number: the size of the account list, not
	// the sweep's progress.  Both are reported under the same JSON key on two
	// different documents, which is how a live run read accounts:1 while this
	// view read accounts:3.
	if views[0].Accounts != 3 {
		t.Errorf("accounts = %d, want all three listed", views[0].Accounts)
	}
	if views[0].Enabled != 2 {
		t.Errorf("enabled = %d, want the two enabled ones", views[0].Enabled)
	}
	if views[0].Name != "checkin" {
		t.Errorf("name = %q, want checkin", views[0].Name)
	}
}
