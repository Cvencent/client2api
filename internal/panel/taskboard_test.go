package panel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// The point of these tests is that the panel knows nothing about any particular
// vendor.  Every behaviour below is driven by whether a client happens to
// implement core.TaskProvider, so a module can gain or lose the task board
// without the panel changing at all.
// ---------------------------------------------------------------------------

// fakeTaskClient implements core.Client + core.TaskProvider and nothing else.
type fakeTaskClient struct {
	name     string
	tasks    []core.TaskInfo
	listErr  error
	runErr   error
	runDelay time.Duration
	result   core.TaskResult

	// ran is appended by the run goroutine the panel spawns and read by the
	// test goroutine, so it carries its own lock rather than relying on the
	// runner having finished.
	mu    sync.Mutex
	ran   []string
	ranAt []time.Time
}

// fakeTaskBalanceClient is a task module that can also report a live balance.
// The panel uses that capability to refresh the account-pool row after a chore
// changes the vendor's balance.
type fakeTaskBalanceClient struct {
	*fakeTaskClient

	mu           sync.Mutex
	balance      core.Balance
	balanceErr   error
	balanceCalls int
}

func (f *fakeTaskBalanceClient) AccountBalance(context.Context, string, time.Duration) (core.Balance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.balanceCalls++
	if f.balanceErr != nil {
		return core.Balance{}, f.balanceErr
	}
	return f.balance, nil
}

func (f *fakeTaskBalanceClient) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.balanceCalls
}

// codes returns a copy of the task codes RunTask has been asked to run.
func (f *fakeTaskClient) codes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ran...)
}

// runTimes returns a copy of the wall-clock instants RunTask was entered.
func (f *fakeTaskClient) runTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.ranAt...)
}

func (f *fakeTaskClient) Name() string                                 { return f.name }
func (f *fakeTaskClient) Models(context.Context) ([]core.Model, error) { return nil, nil }
func (f *fakeTaskClient) Chat(context.Context, *core.ChatRequest) (core.Stream, error) {
	return nil, nil
}
func (f *fakeTaskClient) Status(context.Context) core.Status { return core.Status{Ready: true} }

func (f *fakeTaskClient) Tasks(context.Context, string) ([]core.TaskInfo, error) {
	return f.tasks, f.listErr
}

func (f *fakeTaskClient) RunTask(ctx context.Context, _, code string) (core.TaskResult, error) {
	if f.runErr != nil {
		return core.TaskResult{}, f.runErr
	}
	f.mu.Lock()
	f.ranAt = append(f.ranAt, time.Now())
	f.mu.Unlock()
	if f.runDelay > 0 {
		select {
		case <-time.After(f.runDelay):
		case <-ctx.Done():
			return core.TaskResult{}, ctx.Err()
		}
	}
	f.mu.Lock()
	f.ran = append(f.ran, code)
	f.mu.Unlock()
	return f.result, nil
}

// fakeBareClient is the same module without the task capability.
type fakeBareClient struct{ name string }

func (f *fakeBareClient) Name() string                                 { return f.name }
func (f *fakeBareClient) Models(context.Context) ([]core.Model, error) { return nil, nil }
func (f *fakeBareClient) Chat(context.Context, *core.ChatRequest) (core.Stream, error) {
	return nil, nil
}
func (f *fakeBareClient) Status(context.Context) core.Status { return core.Status{} }

func taskPanel(t *testing.T, clients ...core.Client) *panel {
	t.Helper()
	reg := core.NewRegistry()
	for _, c := range clients {
		reg.Add(c)
	}
	p := &panel{
		opts:      Options{Registry: reg, Version: "test", Listen: "127.0.0.1:0"},
		runs:      newTaskRuns(),
		chores:    newTaskQueues(),
		taskLocks: newAccountLocks(),
	}
	p.initTaskBoardCache()
	return p
}

func doTask(t *testing.T, p *panel, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	rec := httptest.NewRecorder()
	p.handleClientScoped(rec, httptest.NewRequest(method, path, rd))
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

// waitRun polls the run-status endpoint until the run leaves "running".
func waitRun(t *testing.T, p *panel, client, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec, out := doTask(t, p, http.MethodGet, "/panel/api/clients/"+client+"/tasks/runs/"+id, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("run status: status=%d body=%s", rec.Code, rec.Body.String())
		}
		run, _ := out["run"].(map[string]any)
		if run["state"] == taskRunDone {
			return run
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s never finished", id)
	return nil
}

func TestTaskListReportsTheChores(t *testing.T) {
	c := &fakeTaskClient{name: "wb", tasks: []core.TaskInfo{
		{Code: "chat_5", Title: "发 5 条消息", Credit: 100, Current: 2, Target: 5, Auto: true},
		{Code: "skill_1", Title: "用一次技能", Auto: false, Note: "需要人工"},
	}}
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodGet, "/panel/api/clients/wb/tasks", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	list, _ := out["tasks"].([]any)
	if len(list) != 2 {
		t.Fatalf("want 2 tasks, got %d (%s)", len(list), rec.Body.String())
	}
	first, _ := list[0].(map[string]any)
	if first["code"] != "chat_5" || first["credit"].(float64) != 100 {
		t.Fatalf("unexpected first task: %v", first)
	}
	// Progress must be emitted even mid-task: a board that hid 0/5 would be
	// indistinguishable from a board that does not know the progress.
	if first["current"].(float64) != 2 || first["target"].(float64) != 5 {
		t.Fatalf("progress lost: %v", first)
	}
	if first["auto"] != true {
		t.Fatalf("auto flag lost: %v", first)
	}
}

func TestTaskListOnAModuleWithoutTheCapabilityIs501(t *testing.T) {
	p := taskPanel(t, &fakeBareClient{name: "plain"})
	rec, _ := doTask(t, p, http.MethodGet, "/panel/api/clients/plain/tasks", "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d (%s)", rec.Code, rec.Body.String())
	}
	// And the capability probe must agree, so the UI never draws the board.
	rec, out := doTask(t, p, http.MethodGet, "/panel/api/clients/plain/capabilities", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("capabilities: status=%d", rec.Code)
	}
	if out["tasks"] == true {
		t.Fatalf("bare module advertised tasks: %s", rec.Body.String())
	}

	tp := &fakeTaskClient{name: "wb"}
	p2 := taskPanel(t, tp)
	_, out = doTask(t, p2, http.MethodGet, "/panel/api/clients/wb/capabilities", "")
	if out["tasks"] != true {
		t.Fatalf("task module did not advertise tasks: %s", out)
	}
}

func TestTaskRunIsAsynchronousAndJournalled(t *testing.T) {
	c := &fakeTaskClient{
		name:     "wb",
		runDelay: 120 * time.Millisecond,
		result:   core.TaskResult{OK: true, Message: "claimed", Credit: 300},
	}
	p := taskPanel(t, c)

	start := time.Now()
	rec, out := doTask(t, p, http.MethodPost, "/panel/api/clients/wb/tasks/chat_5/run", `{"account":"a1"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d (%s)", rec.Code, rec.Body.String())
	}
	// The whole point: the POST returns before the chore is done, because a
	// real one can sleep for tens of seconds between upstream events.
	if elapsed := time.Since(start); elapsed >= c.runDelay {
		t.Fatalf("POST blocked for %v — a slow chore would time the request out", elapsed)
	}
	run, _ := out["run"].(map[string]any)
	id, _ := run["id"].(string)
	if id == "" {
		t.Fatalf("no run id: %s", rec.Body.String())
	}
	if run["account"] != "a1" || run["code"] != "chat_5" {
		t.Fatalf("run lost its identity: %v", run)
	}

	done := waitRun(t, p, "wb", id)
	res, _ := done["result"].(map[string]any)
	if res["ok"] != true || res["credit"].(float64) != 300 {
		t.Fatalf("result not journalled: %v", done)
	}
	if ran := c.codes(); len(ran) != 1 || ran[0] != "chat_5" {
		t.Fatalf("runner not invoked: %v", ran)
	}

	// The board folds the recent runs in so a row can show its last outcome.
	_, out = doTask(t, p, http.MethodGet, "/panel/api/clients/wb/tasks", "")
	runs, _ := out["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("want 1 journalled run, got %d (%s)", len(runs), out)
	}
}

func TestTaskRunRefreshesTheBalanceAfterSuccess(t *testing.T) {
	c := &fakeTaskBalanceClient{
		fakeTaskClient: &fakeTaskClient{name: "loomy", result: core.TaskResult{OK: true, AccountID: "a1"}},
		balance:        core.Balance{Credits: 10000, Unit: "credits"},
	}
	p := taskPanel(t, c)
	p.initBalanceCache()
	seedBalanceCache(p, "loomy", "a1", core.Balance{Credits: 7000, Unit: "credits"})

	rec, out := doTask(t, p, http.MethodPost, "/panel/api/clients/loomy/tasks/share_soul/run", `{"account":"a1"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run: status=%d body=%s", rec.Code, rec.Body.String())
	}
	run, _ := out["run"].(map[string]any)
	id, _ := run["id"].(string)
	waitRun(t, p, "loomy", id)

	entry, ok := p.balanceCache.entry("loomy", "a1")
	if !ok {
		t.Fatal("the balance cache lost the account")
	}
	if entry.Credits != 10000 {
		t.Fatalf("cached balance = %d, want the post-task balance 10000", entry.Credits)
	}
	if got := c.calls(); got != 1 {
		t.Fatalf("AccountBalance calls = %d, want one after the task", got)
	}
}

func TestTaskRunRecordsARefusalAsAResultNotAnError(t *testing.T) {
	// A run error means "the attempt could not be made".  A vendor refusal is
	// still an answer, and the board must be able to show it.
	c := &fakeTaskClient{name: "wb", runErr: context.DeadlineExceeded}
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, "/panel/api/clients/wb/tasks/chat_5/run", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("the start must still succeed, got %d (%s)", rec.Code, rec.Body.String())
	}
	run, _ := out["run"].(map[string]any)
	id, _ := run["id"].(string)

	done := waitRun(t, p, "wb", id)
	res, _ := done["result"].(map[string]any)
	if res["ok"] != false {
		t.Fatalf("want ok:false, got %v", done)
	}
	if res["error"] == "" || res["error"] == nil {
		t.Fatalf("the refusal reason was dropped: %v", done)
	}
}

func TestTaskRunRejectsBadRequests(t *testing.T) {
	c := &fakeTaskClient{name: "wb"}
	// "plain" has to be registered too: an unregistered client is a 404 from
	// the route, which would hide the 501 this test is actually about.
	p := taskPanel(t, c, &fakeBareClient{name: "plain"})

	if rec, _ := doTask(t, p, http.MethodGet, "/panel/api/clients/wb/tasks/chat_5/run", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on run: want 405, got %d", rec.Code)
	}
	if rec, _ := doTask(t, p, http.MethodPost, "/panel/api/clients/wb/tasks/runs", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bare /tasks/runs: want 400, got %d", rec.Code)
	}
	if rec, _ := doTask(t, p, http.MethodGet, "/panel/api/clients/wb/tasks/runs/nope", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run: want 404, got %d", rec.Code)
	}
	if rec, _ := doTask(t, p, http.MethodPost, "/panel/api/clients/plain/tasks/chat_5/run", ""); rec.Code != http.StatusNotImplemented {
		t.Fatalf("run on a bare module: want 501, got %d", rec.Code)
	}
	if ran := c.codes(); len(ran) != 0 {
		t.Fatalf("a rejected request still ran something: %v", ran)
	}
}

func TestTaskRunStatusIsScopedToItsClient(t *testing.T) {
	a := &fakeTaskClient{name: "aa", runDelay: 200 * time.Millisecond}
	b := &fakeTaskClient{name: "bb"}
	p := taskPanel(t, a, b)

	_, out := doTask(t, p, http.MethodPost, "/panel/api/clients/aa/tasks/chat_5/run", "")
	run, _ := out["run"].(map[string]any)
	id, _ := run["id"].(string)

	// A run id carries no client identity, so the route has to enforce it.
	rec, _ := doTask(t, p, http.MethodGet, "/panel/api/clients/bb/tasks/runs/"+id, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("run leaked across clients: %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestTaskListRedactsSecretsInUpstreamStrings(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJlLWhlcmU"
	c := &fakeTaskClient{name: "wb", tasks: []core.TaskInfo{
		{Code: "x", Title: "ok", Note: "token " + jwt, Desc: "also " + jwt},
	}}
	p := taskPanel(t, c)

	rec, _ := doTask(t, p, http.MethodGet, "/panel/api/clients/wb/tasks", "")
	body := rec.Body.String()
	if strings.Contains(body, jwt) {
		t.Fatalf("a JWT reached the panel: %s", body)
	}
	if !strings.Contains(body, "ok") {
		t.Fatalf("redaction ate the harmless text too: %s", body)
	}
}

func TestTaskRunsStorePrunesFinishedRuns(t *testing.T) {
	s := newTaskRuns()
	id := s.start("wb", "", "chat_5")
	s.finish(id, core.TaskResult{OK: true}, time.Second)

	// Age the run past the TTL by rewriting its finish stamp, then start
	// another: the prune runs on start, which is the only place it has to.
	s.mu.Lock()
	s.byID[id].FinishedAt = time.Now().Add(-2 * taskRunTTL).Format(time.RFC3339Nano)
	s.mu.Unlock()

	fresh := s.start("wb", "", "chat_5")
	if _, ok := s.get(id); ok {
		t.Fatalf("expired run survived a prune")
	}
	if _, ok := s.get(fresh); !ok {
		t.Fatalf("the prune ate the run it had just created")
	}
	// A running run must never be pruned, however old it looks.
	s.mu.Lock()
	s.byID[fresh].StartedAt = time.Now().Add(-2 * taskRunTTL).Format(time.RFC3339Nano)
	s.mu.Unlock()
	s.start("wb", "", "other")
	if _, ok := s.get(fresh); !ok {
		t.Fatalf("an unfinished run was pruned")
	}
}

func TestTaskRunCodesWithSlashesAndDotsSurvive(t *testing.T) {
	// Task codes are vendor strings.  The reference set is all underscores,
	// but nothing in core promises that, and a code is a path segment — so
	// verify the escaping round-trip rather than assuming.
	code := "Sequential_Tasks_3"
	c := &fakeTaskClient{name: "wb"}
	p := taskPanel(t, c)

	rec, _ := doTask(t, p, http.MethodPost, "/panel/api/clients/wb/tasks/"+code+"/run", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(c.codes()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if ran := c.codes(); len(ran) != 1 || ran[0] != code {
		t.Fatalf("code mangled: %v", ran)
	}
}

func TestTaskListFiltersRunsByAccount(t *testing.T) {
	c := &fakeTaskClient{
		name:   "loomy",
		tasks:  []core.TaskInfo{{Code: "share_soul", Auto: true}},
		result: core.TaskResult{OK: true, AccountID: "a1"},
	}
	p := taskPanel(t, c)
	p.runs.start("loomy", "a1", "share_soul")
	p.runs.start("loomy", "a2", "share_soul")

	rec, out := doTask(t, p, http.MethodGet, "/panel/api/clients/loomy/tasks?account=a1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	runs, _ := out["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("runs = %v, want only a1", runs)
	}
	run, _ := runs[0].(map[string]any)
	if run["account"] != "a1" {
		t.Fatalf("run account = %v, want a1", run["account"])
	}
}

func TestTaskListAllGroupsByAccount(t *testing.T) {
	c := newSweepClient("loomy", core.TaskResult{OK: true}, liveAccount("a1"), liveAccount("a2"))
	c.fakeTaskClient.tasks = []core.TaskInfo{{Code: "share_soul", Auto: true}}
	p := taskPanel(t, c)
	p.runs.start("loomy", "a1", "share_soul")
	p.runs.start("loomy", "a2", "share_soul")

	rec, out := doTask(t, p, http.MethodGet, "/panel/api/clients/loomy/tasks?all=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	accounts, _ := out["accounts"].([]any)
	if len(accounts) != 2 {
		t.Fatalf("accounts = %v, want a1 and a2", accounts)
	}
	for _, raw := range accounts {
		row, _ := raw.(map[string]any)
		runs, _ := row["runs"].([]any)
		if len(runs) != 1 {
			t.Fatalf("row %v has runs %v, want one account-scoped run", row["account"], runs)
		}
		if len(row["tasks"].([]any)) != 1 {
			t.Fatalf("row %v lost tasks", row)
		}
	}
}
