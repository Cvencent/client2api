package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// The fleet-wide chore queue.
//
// As with the single-chore board, nothing here knows a vendor: the queue is
// driven entirely by core.TaskProvider and by what a scan reports, so a module
// that gains or loses the capability changes these results and no code.
// ---------------------------------------------------------------------------

// queuePath builds one client-scoped chore-queue route.
func queuePath(client, tail string) string {
	return "/panel/api/clients/" + client + "/tasks/" + tail
}

// waitQueue polls the queue endpoint until the round leaves "running".
func waitQueue(t *testing.T, p *panel, client string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, out := doTask(t, p, http.MethodGet, queuePath(client, "queue"), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("queue status: status=%d body=%s", rec.Code, rec.Body.String())
		}
		running, _ := out["running"].(bool)
		total, _ := out["total"].(float64)
		if !running && total > 0 {
			return out
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the queue never drained")
	return nil
}

// ---------------------------------------------------------------------------
// Scanning
// ---------------------------------------------------------------------------

func TestTaskScanAllReportsOnlyAutomatableChores(t *testing.T) {
	c := &fakeTaskClient{name: "wb", tasks: []core.TaskInfo{
		{Code: "accept", Auto: true},
		// Past its target but not claimed: running it is what claims the
		// reward, so it is still outstanding work.
		{Code: "claim", Auto: true, Current: 5, Target: 5},
		{Code: "done", Auto: true, Claimed: true},
		// Locked until the vendor unlocks the next link, so queuing it would
		// only produce an attempt that cannot land.
		{Code: "next", Auto: true, Locked: true},
		// The module says this one cannot be automated at all.
		{Code: "manual", Auto: false},
	}}
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, queuePath("wb", "scan_all"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("scan_all: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n, _ := out["pending_count"].(float64); n != 2 {
		t.Errorf("pending_count = %v, want 2 (%v)", out["pending_count"], out)
	}
	if got := c.codes(); len(got) != 0 {
		t.Errorf("a scan must run nothing, but ran %v", got)
	}

	rows, _ := out["accounts"].([]any)
	if len(rows) != 1 {
		t.Fatalf("accounts = %v, want one row", out["accounts"])
	}
	row, _ := rows[0].(map[string]any)
	pending, _ := row["pending"].([]any)
	got := map[string]bool{}
	for _, it := range pending {
		m, _ := it.(map[string]any)
		code, _ := m["code"].(string)
		got[code] = true
	}
	if len(got) != 2 || !got["accept"] || !got["claim"] {
		t.Errorf("pending = %v, want exactly accept and claim", got)
	}
}

func TestTaskScanAllRejectsAModuleWithoutABoard(t *testing.T) {
	p := taskPanel(t, &fakeBareClient{name: "wb"})
	rec, _ := doTask(t, p, http.MethodPost, queuePath("wb", "scan_all"), "")
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rec.Code)
	}
}

func TestTaskScanAllRejectsTheWrongMethod(t *testing.T) {
	p := taskPanel(t, &fakeTaskClient{name: "wb"})
	rec, _ := doTask(t, p, http.MethodGet, queuePath("wb", "scan_all"), "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// fakePoolTaskClient is a module that both lists accounts and runs chores --
// the shape the queue's account sweep exists for.  It implements the whole of
// core.AccountManager, since that interface is what decides whether the sweep
// happens at all.
type fakePoolTaskClient struct {
	*fakeTaskClient
	accounts []core.AccountRecord

	// scanMu guards asked, which the scan goroutines append to.
	scanMu sync.Mutex
	asked  []string
}

func (f *fakePoolTaskClient) AccountFields(context.Context) []core.FieldSpec { return nil }

func (f *fakePoolTaskClient) Accounts(context.Context) ([]core.AccountRecord, error) {
	return f.accounts, nil
}

func (f *fakePoolTaskClient) AddAccount(context.Context, core.AccountSpec) (core.AccountRecord, error) {
	return core.AccountRecord{}, nil
}

func (f *fakePoolTaskClient) RemoveAccount(context.Context, string) error { return nil }

func (f *fakePoolTaskClient) SetAccountEnabled(context.Context, string, bool) error { return nil }

func (f *fakePoolTaskClient) TestAccount(context.Context, string) (core.TestResult, error) {
	return core.TestResult{}, nil
}

func (f *fakePoolTaskClient) RefreshAccount(context.Context, string) ([]core.RefreshResult, error) {
	return nil, nil
}

// Tasks records which account the queue asked about, which the embedded fake
// cannot do because it ignores the id.
func (f *fakePoolTaskClient) Tasks(_ context.Context, accountID string) ([]core.TaskInfo, error) {
	f.scanMu.Lock()
	f.asked = append(f.asked, accountID)
	f.scanMu.Unlock()
	return f.tasks, nil
}

func (f *fakePoolTaskClient) accountsAsked() []string {
	f.scanMu.Lock()
	defer f.scanMu.Unlock()
	return append([]string(nil), f.asked...)
}

func TestTaskScanSkipsParkedAccounts(t *testing.T) {
	c := &fakePoolTaskClient{
		fakeTaskClient: &fakeTaskClient{name: "wb", tasks: []core.TaskInfo{{Code: "accept", Auto: true}}},
		accounts: []core.AccountRecord{
			{ID: "a1", Label: "one", Enabled: true},
			{ID: "a2", Label: "parked", Enabled: false},
			{ID: "a3", Label: "two", Enabled: true},
		},
	}
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, queuePath("wb", "scan_all"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("scan_all: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n, _ := out["pending_count"].(float64); n != 2 {
		t.Errorf("pending_count = %v, want one per live account", out["pending_count"])
	}
	asked := c.accountsAsked()
	if len(asked) != 2 {
		t.Fatalf("asked about %v, want only the two enabled accounts", asked)
	}
	for _, id := range asked {
		if id == "a2" {
			t.Errorf("the queue woke a parked account: %v", asked)
		}
	}
}

// ---------------------------------------------------------------------------
// Draining
// ---------------------------------------------------------------------------

func TestTaskRunQueueDrainsEveryPendingChore(t *testing.T) {
	c := &fakeTaskClient{name: "wb", tasks: []core.TaskInfo{
		{Code: "accept", Auto: true},
		{Code: "claim", Auto: true},
		{Code: "done", Auto: true, Claimed: true},
	}, result: core.TaskResult{OK: true, Message: "claimed 20"}}
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, queuePath("wb", "run_queue"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("run_queue: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if out["started"] != true {
		t.Fatalf("started = %v, want true (%v)", out["started"], out)
	}
	if n, _ := out["total"].(float64); n != 2 {
		t.Errorf("total = %v, want 2", out["total"])
	}

	q := waitQueue(t, p, "wb")
	if n, _ := q["done"].(float64); n != 2 {
		t.Errorf("done = %v, want 2 (%v)", q["done"], q)
	}
	if n, _ := q["pending"].(float64); n != 0 {
		t.Errorf("pending = %v, want 0", q["pending"])
	}
	if got := c.codes(); len(got) != 2 {
		t.Errorf("ran %v, want exactly the two outstanding chores", got)
	}

	// Every item carries the journal id of the run it created, and that run
	// is the same record the single-chore board would have written.
	items, _ := q["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %v, want 2", q["items"])
	}
	for _, it := range items {
		m, _ := it.(map[string]any)
		id, _ := m["run_id"].(string)
		if id == "" {
			t.Fatalf("item %v has no run id", m)
		}
		rec, out := doTask(t, p, http.MethodGet, "/panel/api/clients/wb/tasks/runs/"+id, "")
		if rec.Code != http.StatusOK {
			t.Errorf("run %s: status=%d body=%s", id, rec.Code, rec.Body.String())
			continue
		}
		run, _ := out["run"].(map[string]any)
		if run["state"] != taskRunDone {
			t.Errorf("run %s state = %v, want done", id, run["state"])
		}
	}
}

func TestTaskQueueRefreshesTheBalanceAfterSuccess(t *testing.T) {
	c := &fakeTaskBalanceClient{
		fakeTaskClient: &fakeTaskClient{
			name:   "loomy",
			tasks:  []core.TaskInfo{{Code: "share_soul", Auto: true}},
			result: core.TaskResult{OK: true, AccountID: "a1"},
		},
		balance: core.Balance{Credits: 10000, Unit: "credits"},
	}
	p := taskPanel(t, c)
	p.initBalanceCache()
	seedBalanceCache(p, "loomy", "a1", core.Balance{Credits: 7000, Unit: "credits"})

	rec, out := doTask(t, p, http.MethodPost, queuePath("loomy", "run_queue"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("run_queue: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if out["started"] != true {
		t.Fatalf("started = %v, want true (%v)", out["started"], out)
	}
	waitQueue(t, p, "loomy")

	entry, ok := p.balanceCache.entry("loomy", "a1")
	if !ok {
		t.Fatal("the balance cache lost the account")
	}
	if entry.Credits != 10000 {
		t.Fatalf("cached balance = %d, want the post-task balance 10000", entry.Credits)
	}
	if got := c.calls(); got != 1 {
		t.Fatalf("AccountBalance calls = %d, want one after the queued task", got)
	}
}

func TestTaskRunQueueRefusesASecondRound(t *testing.T) {
	c := &fakeTaskClient{
		name:     "wb",
		tasks:    []core.TaskInfo{{Code: "slow", Auto: true}},
		runDelay: 300 * time.Millisecond,
		result:   core.TaskResult{OK: true},
	}
	p := taskPanel(t, c)

	if rec, _ := doTask(t, p, http.MethodPost, queuePath("wb", "run_queue"), ""); rec.Code != http.StatusOK {
		t.Fatalf("first round: status=%d", rec.Code)
	}
	rec, _ := doTask(t, p, http.MethodPost, queuePath("wb", "run_queue"), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("second round: status=%d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := c.codes(); len(got) > 1 {
		t.Errorf("ran %v: a refused second click must not double-run the chores", got)
	}
	waitQueue(t, p, "wb")
}

func TestTaskRunQueueWithNothingPending(t *testing.T) {
	c := &fakeTaskClient{name: "wb", tasks: []core.TaskInfo{{Code: "done", Auto: true, Claimed: true}}}
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, queuePath("wb", "run_queue"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("run_queue: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if out["started"] != false {
		t.Errorf("started = %v, want false", out["started"])
	}
	if msg, _ := out["message"].(string); msg != "no pending chores" {
		t.Errorf("message = %q", msg)
	}
	// A round that started nothing must not leave the queue looking busy, or
	// the operator's next click would be refused with nothing running.
	_, q := doTask(t, p, http.MethodGet, queuePath("wb", "queue"), "")
	if q["running"] != false {
		t.Errorf("running = %v after an empty round, want false", q["running"])
	}
	if got := c.codes(); len(got) != 0 {
		t.Errorf("ran %v, want nothing", got)
	}
}

func TestTaskQueuesArePerClient(t *testing.T) {
	slow := &fakeTaskClient{
		name:     "trae",
		tasks:    []core.TaskInfo{{Code: "s", Auto: true}},
		runDelay: 250 * time.Millisecond,
		result:   core.TaskResult{OK: true},
	}
	fast := &fakeTaskClient{
		name:   "wb",
		tasks:  []core.TaskInfo{{Code: "f", Auto: true}},
		result: core.TaskResult{OK: true},
	}
	p := taskPanel(t, slow, fast)

	if rec, _ := doTask(t, p, http.MethodPost, queuePath("trae", "run_queue"), ""); rec.Code != http.StatusOK {
		t.Fatalf("trae round: status=%d", rec.Code)
	}
	// One module's backlog must not sit in front of another's.
	rec, out := doTask(t, p, http.MethodPost, queuePath("wb", "run_queue"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("wb round while trae drains: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if out["started"] != true {
		t.Errorf("started = %v, want true: the queues are not shared", out["started"])
	}
	_, w := doTask(t, p, http.MethodGet, queuePath("trae", "queue"), "")
	if w["running"] != true {
		t.Errorf("trae's round was already over, so the two never overlapped: %v", w)
	}

	waitQueue(t, p, "trae")
	waitQueue(t, p, "wb")
}

func TestTaskQueueClampsConcurrency(t *testing.T) {
	for _, tc := range []struct {
		ask  int
		want int
	}{{0, 1}, {2, 2}, {99, 4}, {-3, 1}} {
		c := &fakeTaskClient{
			name:   "wb",
			tasks:  []core.TaskInfo{{Code: "x", Auto: true}},
			result: core.TaskResult{OK: true},
		}
		p := taskPanel(t, c)

		rec, out := doTask(t, p, http.MethodPost, queuePath("wb", "run_queue"),
			fmt.Sprintf(`{"concurrency":%d}`, tc.ask))
		if rec.Code != http.StatusOK {
			t.Fatalf("ask %d: status=%d body=%s", tc.ask, rec.Code, rec.Body.String())
		}
		q, _ := out["queue"].(map[string]any)
		if n, _ := q["conc"].(float64); int(n) != tc.want {
			t.Errorf("ask %d: conc = %v, want %d", tc.ask, q["conc"], tc.want)
		}
		waitQueue(t, p, "wb")
	}
}

func TestTaskQueueCapsOneRound(t *testing.T) {
	tasks := make([]core.TaskInfo, taskQueueMaxItems+50)
	for i := range tasks {
		tasks[i] = core.TaskInfo{Code: fmt.Sprintf("c%d", i), Auto: true}
	}
	c := &fakeTaskClient{name: "wb", tasks: tasks, result: core.TaskResult{OK: true}}
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, queuePath("wb", "run_queue"), `{"concurrency":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("run_queue: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if n, _ := out["total"].(float64); int(n) != taskQueueMaxItems {
		t.Errorf("total = %v, want the %d-item cap", out["total"], taskQueueMaxItems)
	}
	waitQueue(t, p, "wb")
	if got := c.codes(); len(got) != taskQueueMaxItems {
		t.Errorf("ran %d chores, want %d", len(got), taskQueueMaxItems)
	}
}

// ---------------------------------------------------------------------------
// Outcomes
// ---------------------------------------------------------------------------

func TestTaskQueueRecordsAVendorRefusalAsSkipped(t *testing.T) {
	c := &fakeTaskClient{
		name:   "wb",
		tasks:  []core.TaskInfo{{Code: "claim", Auto: true}},
		result: core.TaskResult{OK: false, Message: "already claimed today"},
	}
	p := taskPanel(t, c)

	if rec, _ := doTask(t, p, http.MethodPost, queuePath("wb", "run_queue"), ""); rec.Code != http.StatusOK {
		t.Fatalf("run_queue: status=%d", rec.Code)
	}
	q := waitQueue(t, p, "wb")
	if n, _ := q["skipped"].(float64); n != 1 {
		t.Errorf("skipped = %v, want 1 (%v)", q["skipped"], q)
	}
	if n, _ := q["failed"].(float64); n != 0 {
		t.Errorf("failed = %v, want 0: a refusal is a result, not a failure", q["failed"])
	}
	items, _ := q["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %v, want 1", q["items"])
	}
	m, _ := items[0].(map[string]any)
	if msg, _ := m["message"].(string); msg != "already claimed today" {
		t.Errorf("message = %q, want the vendor's own reason", msg)
	}
}

func TestTaskQueueReportsAnUnmakeableAttemptAsFailed(t *testing.T) {
	c := &fakeTaskClient{
		name:   "wb",
		tasks:  []core.TaskInfo{{Code: "claim", Auto: true}},
		runErr: errors.New("dial tcp: connection refused"),
	}
	p := taskPanel(t, c)

	if rec, _ := doTask(t, p, http.MethodPost, queuePath("wb", "run_queue"), ""); rec.Code != http.StatusOK {
		t.Fatalf("run_queue: status=%d", rec.Code)
	}
	q := waitQueue(t, p, "wb")
	if n, _ := q["failed"].(float64); n != 1 {
		t.Errorf("failed = %v, want 1 (%v)", q["failed"], q)
	}
	if n, _ := q["done"].(float64); n != 0 {
		t.Errorf("done = %v, want 0", q["done"])
	}
}

func TestTaskQueueEndpointsRejectTheWrongMethod(t *testing.T) {
	p := taskPanel(t, &fakeTaskClient{name: "wb"})
	for _, tc := range []struct {
		method string
		tail   string
	}{
		{http.MethodGet, "run_queue"},
		{http.MethodPost, "queue"},
	} {
		rec, _ := doTask(t, p, tc.method, queuePath("wb", tc.tail), "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status = %d, want 405", tc.method, tc.tail, rec.Code)
		}
	}
}

func TestTaskQueueRejectsAModuleWithoutABoard(t *testing.T) {
	p := taskPanel(t, &fakeBareClient{name: "wb"})
	for _, tail := range []string{"run_queue", "queue"} {
		rec, _ := doTask(t, p, http.MethodPost, queuePath("wb", tail), "")
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s: status = %d, want 501", tail, rec.Code)
		}
	}
}
