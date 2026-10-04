package panel

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"client2api/internal/core"
)

// ---------------------------------------------------------------------------
// The per-account task verbs.
//
// Nothing here knows a vendor either: every result below comes from whether the
// fake happens to implement core.TaskAccepter, BulkTaskAccepter, TaskClaimer or
// TaskAutoRunner.  A module that loses one of them must turn into a 501 and
// nothing else.
// ---------------------------------------------------------------------------

// verbPath builds one per-account task verb route.
func verbPath(client, id, tail string) string {
	return "/panel/api/clients/" + client + "/accounts/" + id + "/tasks/" + tail
}

// fastAutoGap shrinks the round's inter-chore pause.  The real gap exists to
// stay under a vendor's burst scoring; a test has no vendor.
func fastAutoGap(t *testing.T) {
	t.Helper()
	old := autoAllGap
	autoAllGap = time.Millisecond
	t.Cleanup(func() { autoAllGap = old })
}

// fakeVerbClient implements every task verb on top of the pool-aware fake, so
// the only thing deciding a test's outcome is which interfaces it satisfies.
type fakeVerbClient struct {
	*fakePoolTaskClient

	listErr error

	verbMu   sync.Mutex
	accepted [][]string
	bulk     core.BulkAccept
	bulkErr  error
	claim    core.TaskClaim
	claimErr error
	auto     map[string]core.AutoTaskResult
	autoErr  map[string]error
	ranAuto  []string

	// entered and release let a test hold a run open: the 409-on-busy test
	// needs one call in flight while it makes the second.
	entered chan string
	release chan struct{}
}

func verbClient(name string, tasks []core.TaskInfo, accounts ...core.AccountRecord) *fakeVerbClient {
	return &fakeVerbClient{
		fakePoolTaskClient: &fakePoolTaskClient{
			fakeTaskClient: &fakeTaskClient{name: name, tasks: tasks},
			accounts:       accounts,
		},
		auto:    map[string]core.AutoTaskResult{},
		autoErr: map[string]error{},
	}
}

func (f *fakeVerbClient) Tasks(context.Context, string) ([]core.TaskInfo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.fakeTaskClient.tasks, nil
}

func (f *fakeVerbClient) AcceptTasks(_ context.Context, _ string, codes []string) error {
	f.verbMu.Lock()
	defer f.verbMu.Unlock()
	f.accepted = append(f.accepted, append([]string(nil), codes...))
	return nil
}

func (f *fakeVerbClient) AcceptAllTasks(context.Context, string) (core.BulkAccept, error) {
	return f.bulk, f.bulkErr
}

func (f *fakeVerbClient) ClaimTask(context.Context, string, string) (core.TaskClaim, error) {
	return f.claim, f.claimErr
}

func (f *fakeVerbClient) RunTaskAuto(ctx context.Context, _, code string) (core.AutoTaskResult, error) {
	f.verbMu.Lock()
	f.ranAuto = append(f.ranAuto, code)
	entered, release := f.entered, f.release
	f.verbMu.Unlock()

	if entered != nil {
		entered <- code
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return core.AutoTaskResult{}, ctx.Err()
		}
	}
	if err := f.autoErr[code]; err != nil {
		return core.AutoTaskResult{}, err
	}
	return f.auto[code], nil
}

func (f *fakeVerbClient) codesAccepted() [][]string {
	f.verbMu.Lock()
	defer f.verbMu.Unlock()
	out := make([][]string, 0, len(f.accepted))
	for _, c := range f.accepted {
		out = append(out, append([]string(nil), c...))
	}
	return out
}

func (f *fakeVerbClient) codesAutoRun() []string {
	f.verbMu.Lock()
	defer f.verbMu.Unlock()
	return append([]string(nil), f.ranAuto...)
}

// okOf, numOf and boolOf read the response keys the frontend also reads, so a
// rename here fails a test rather than the browser.
func okOf(t *testing.T, m map[string]any) bool {
	t.Helper()
	v, _ := m["ok"].(bool)
	return v
}

func numOf(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("%s: want a number, got %#v (body keys: %v)", key, m[key], keysOf(m))
	}
	return v
}

func boolOf(t *testing.T, m map[string]any, key string) bool {
	t.Helper()
	v, ok := m[key].(bool)
	if !ok {
		t.Fatalf("%s: want a bool, got %#v (body keys: %v)", key, m[key], keysOf(m))
	}
	return v
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// ---------------------------------------------------------------------------
// The board
// ---------------------------------------------------------------------------

func TestAccountTasksListsOneBoard(t *testing.T) {
	c := verbClient("verb", []core.TaskInfo{
		{Code: "a", Title: "A", Auto: true},
		{Code: "b", Title: "B"},
	}, core.AccountRecord{ID: "ui-1"})
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodGet, verbPath("verb", "ui-1", ""), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !okOf(t, out) || out["account"] != "ui-1" {
		t.Fatalf("unexpected envelope: %v", out)
	}
	list, ok := out["tasks"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("want 2 tasks, got %#v", out["tasks"])
	}
}

func TestAccountTasksRejectsAGoneAccount(t *testing.T) {
	c := verbClient("verb", []core.TaskInfo{{Code: "a"}}, core.AccountRecord{ID: "ui-1"})
	p := taskPanel(t, c)

	rec, _ := doTask(t, p, http.MethodGet, verbPath("verb", "ui-9", ""), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 for an account the module does not hold, got %d", rec.Code)
	}
}

func TestAccountTasksRejectsAModuleWithoutABoard(t *testing.T) {
	p := taskPanel(t, &fakeBareClient{name: "verb"})

	rec, _ := doTask(t, p, http.MethodGet, verbPath("verb", "ui-1", ""), "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("want 501 without a board, got %d", rec.Code)
	}
}

func TestAccountTasksRejectsTheWrongMethod(t *testing.T) {
	c := verbClient("verb", nil, core.AccountRecord{ID: "ui-1"})
	p := taskPanel(t, c)

	rec, _ := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", ""), "{}")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// accept / accept_all
// ---------------------------------------------------------------------------

func TestTaskAcceptPassesTheCodesThrough(t *testing.T) {
	c := verbClient("verb", nil, core.AccountRecord{ID: "ui-1"})
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "accept"),
		`{"task_codes":["a","b"]}`)
	if rec.Code != http.StatusOK || !okOf(t, out) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got := c.codesAccepted()
	if len(got) != 1 || len(got[0]) != 2 || got[0][0] != "a" || got[0][1] != "b" {
		t.Fatalf("the module saw %v, want one call with [a b]", got)
	}
}

func TestTaskAcceptRequiresCodes(t *testing.T) {
	c := verbClient("verb", nil, core.AccountRecord{ID: "ui-1"})
	p := taskPanel(t, c)

	rec, _ := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "accept"), `{"task_codes":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an empty list, got %d", rec.Code)
	}
	if n := len(c.codesAccepted()); n != 0 {
		t.Fatalf("an empty request reached the vendor %d time(s)", n)
	}
}

func TestTaskAcceptRejectsAModuleWithoutTheVerb(t *testing.T) {
	// fakePoolTaskClient has a board and accounts, but no accept verb.
	c := &fakePoolTaskClient{
		fakeTaskClient: &fakeTaskClient{name: "verb"},
		accounts:       []core.AccountRecord{{ID: "ui-1"}},
	}
	p := taskPanel(t, c)

	rec, _ := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "accept"), `{"task_codes":["a"]}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("want 501 without the verb, got %d", rec.Code)
	}
}

func TestTaskAcceptAllSummarisesNothingToDo(t *testing.T) {
	c := verbClient("verb", nil, core.AccountRecord{ID: "ui-1"})
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "accept_all"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if out["message"] != msgAllAccepted {
		t.Fatalf("want the fixed wording, got %#v", out["message"])
	}
	failed, ok := out["failed"].([]any)
	if !ok || len(failed) != 0 {
		t.Fatalf("failed must always be an array, got %#v", out["failed"])
	}
}

func TestTaskAcceptAllReportsPartialFailure(t *testing.T) {
	c := verbClient("verb", nil, core.AccountRecord{ID: "ui-1"})
	c.bulk = core.BulkAccept{Accepted: 3, Failed: []string{"x"}}
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "accept_all"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a partial refusal is not an error: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if numOf(t, out, "accepted") != 3 {
		t.Fatalf("accepted lost: %v", out)
	}
	if out["message"] != msgAcceptPartial {
		t.Fatalf("want the retry wording, got %#v", out["message"])
	}
}

func TestTaskAcceptAllKeepsTheModulesOwnMessage(t *testing.T) {
	c := verbClient("verb", nil, core.AccountRecord{ID: "ui-1"})
	c.bulk = core.BulkAccept{Accepted: 1, Message: "已接受 1 个任务（小程序）"}
	p := taskPanel(t, c)

	_, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "accept_all"), "")
	if out["message"] != "已接受 1 个任务（小程序）" {
		t.Fatalf("the module's own wording was replaced: %#v", out["message"])
	}
}

func TestTaskAcceptAllReportsAVendorFailure(t *testing.T) {
	c := verbClient("verb", nil, core.AccountRecord{ID: "ui-1"})
	c.bulkErr = errors.New("upstream said no; access_token=abcdef123456")
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "accept_all"), "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502 on a vendor refusal, got %d", rec.Code)
	}
	msg, _ := out["error"].(string)
	if !contains(msg, "accept all: ") || contains(msg, "abcdef123456") {
		t.Fatalf("the refusal must be prefixed and redacted, got %q", msg)
	}
}

// ---------------------------------------------------------------------------
// claim
// ---------------------------------------------------------------------------

func TestTaskClaimReportsTheAmount(t *testing.T) {
	c := verbClient("verb", nil, core.AccountRecord{ID: "ui-1"})
	c.claim = core.TaskClaim{Credit: 5, Energy: 2}
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "claim"), `{"task_code":"a"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if numOf(t, out, "credit") != 5 || numOf(t, out, "energy") != 2 {
		t.Fatalf("the amounts were lost: %v", out)
	}
	if _, present := out["already_claimed"]; present {
		t.Fatalf("a real reward must not report already_claimed: %v", out)
	}
}

func TestTaskClaimTreatsAZeroRewardAsAlreadyClaimed(t *testing.T) {
	c := verbClient("verb", nil, core.AccountRecord{ID: "ui-1"})
	c.claim = core.TaskClaim{}
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "claim"), `{"task_code":"a"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !boolOf(t, out, "already_claimed") || out["message"] != msgAlreadyClaimed {
		t.Fatalf("zero reward should read as already claimed: %v", out)
	}
}

func TestTaskClaimHonoursTheModulesAlreadyClaimed(t *testing.T) {
	c := verbClient("verb", nil, core.AccountRecord{ID: "ui-1"})
	c.claim = core.TaskClaim{AlreadyClaimed: true, Credit: 5, Message: "已领取过"}
	p := taskPanel(t, c)

	_, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "claim"), `{"task_code":"a"}`)
	if !boolOf(t, out, "already_claimed") || out["message"] != "已领取过" {
		t.Fatalf("the module's verdict and wording must win: %v", out)
	}
}

func TestTaskClaimRequiresACode(t *testing.T) {
	c := verbClient("verb", nil, core.AccountRecord{ID: "ui-1"})
	p := taskPanel(t, c)
	rec, _ := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "claim"), `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 without a code, got %d", rec.Code)
	}

	rec, _ = doTask(t, p, http.MethodGet, verbPath("verb", "ui-1", "claim"), "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405 on GET, got %d", rec.Code)
	}
}

func TestTaskClaimReportsAVendorFailure(t *testing.T) {
	c := verbClient("verb", nil, core.AccountRecord{ID: "ui-1"})
	c.claimErr = errors.New("reward refused")
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "claim"), `{"task_code":"a"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
	msg, _ := out["error"].(string)
	if !contains(msg, "claim: ") {
		t.Fatalf("want the claim prefix, got %q", msg)
	}
}

// ---------------------------------------------------------------------------
// auto
// ---------------------------------------------------------------------------

func TestTaskAutoReportsTheReadBack(t *testing.T) {
	c := verbClient("verb", []core.TaskInfo{{Code: "a", Title: "A", Auto: true}},
		core.AccountRecord{ID: "ui-1"})
	c.auto["a"] = core.AutoTaskResult{
		TaskResult:     core.TaskResult{OK: true, Code: "a", Credit: 7, Energy: 1},
		ProgressBefore: "2/5",
		ProgressAfter:  "5/5",
		Claimable:      true,
		Attempt:        true,
		Claimed:        true,
	}
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "auto"), `{"task_code":"a"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if out["progress_before"] != "2/5" || out["progress_after"] != "5/5" {
		t.Fatalf("progress read-back lost: %v", out)
	}
	if !boolOf(t, out, "claimable") || !boolOf(t, out, "attempt") || !boolOf(t, out, "claimed") {
		t.Fatalf("verdicts lost: %v", out)
	}
	if !boolOf(t, out, "verify_supported") {
		t.Fatalf("verify_supported must be true: %v", out)
	}
	// The amounts ride on the embedded TaskResult; an outer duplicate would
	// shadow them and quietly report zero.
	if numOf(t, out, "credit") != 7 || numOf(t, out, "energy") != 1 {
		t.Fatalf("the rewarded amounts were lost: %v", out)
	}
}

func TestTaskAutoRefusesAChoreWithoutAnInterface(t *testing.T) {
	c := verbClient("verb", []core.TaskInfo{{Code: "a", Title: "A", Auto: false}},
		core.AccountRecord{ID: "ui-1"})
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "auto"), `{"task_code":"a"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("want 501 for a chore needing a human, got %d", rec.Code)
	}
	if out["error"] != msgTaskAutoNoInterface {
		t.Fatalf("want the reference wording verbatim, got %#v", out["error"])
	}
	if n := len(c.codesAutoRun()); n != 0 {
		t.Fatalf("a chore that cannot be automated was attempted %d time(s)", n)
	}
}

func TestTaskAutoReportsAnUnknownChoreAsNotFound(t *testing.T) {
	c := verbClient("verb", []core.TaskInfo{{Code: "a", Auto: true}},
		core.AccountRecord{ID: "ui-1"})
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "auto"), `{"task_code":"zz"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	if out["error"] != msgTaskNoSuch {
		t.Fatalf("want the fixed wording, got %#v", out["error"])
	}
}

func TestTaskAutoReportsAVendorFailure(t *testing.T) {
	c := verbClient("verb", []core.TaskInfo{{Code: "a", Auto: true}},
		core.AccountRecord{ID: "ui-1"})
	c.autoErr["a"] = errors.New("boom")
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "auto"), `{"task_code":"a"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
	msg, _ := out["error"].(string)
	if !contains(msg, "执行失败: ") {
		t.Fatalf("want the reference prefix, got %q", msg)
	}
}

func TestTaskAutoSerialisesOneAccount(t *testing.T) {
	c := verbClient("verb",
		[]core.TaskInfo{{Code: "a", Auto: true}},
		core.AccountRecord{ID: "ui-1"}, core.AccountRecord{ID: "ui-2"})
	c.auto["a"] = core.AutoTaskResult{TaskResult: core.TaskResult{OK: true, Code: "a"}}
	c.entered = make(chan string, 4)
	c.release = make(chan struct{})
	p := taskPanel(t, c)

	first := make(chan int, 1)
	go func() {
		rec, _ := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "auto"), `{"task_code":"a"}`)
		first <- rec.Code
	}()
	// Wait for the first round to be inside the vendor call, so the second is
	// genuinely concurrent rather than merely sequenced by the scheduler.
	select {
	case <-c.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first run never entered the vendor call")
	}

	rec, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "auto"), `{"task_code":"a"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 while the same account is busy, got %d", rec.Code)
	}
	if out["error"] != msgTaskBusy {
		t.Fatalf("want the reference wording, got %#v", out["error"])
	}

	// A different account of the same vendor is a different lock: the
	// reference has separate locks per account and so must we.  The proof is
	// that the second call reaches the vendor instead of being refused.
	other := make(chan int, 1)
	go func() {
		rec2, _ := doTask(t, p, http.MethodPost, verbPath("verb", "ui-2", "auto"), `{"task_code":"a"}`)
		other <- rec2.Code
	}()
	select {
	case <-c.entered:
		// ui-2 got as far as the vendor, so it was never blocked.
	case <-time.After(5 * time.Second):
		t.Fatal("ui-2 never reached the vendor: the lock is not per account")
	}

	close(c.release)
	select {
	case code := <-first:
		if code != http.StatusOK {
			t.Fatalf("the first run should have finished 200, got %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the first run never returned")
	}
	select {
	case code := <-other:
		if code != http.StatusOK {
			t.Fatalf("the second account's run should have finished 200, got %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second account's run never returned")
	}

	// The lock must be free again, or one refusal would wedge the account for
	// the rest of the process's life.
	rec, _ = doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "auto"), `{"task_code":"a"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("the lock was not released: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTaskAutoRejectsAModuleWithoutTheVerb(t *testing.T) {
	c := &fakePoolTaskClient{
		fakeTaskClient: &fakeTaskClient{name: "verb", tasks: []core.TaskInfo{{Code: "a", Auto: true}}},
		accounts:       []core.AccountRecord{{ID: "ui-1"}},
	}
	p := taskPanel(t, c)

	rec, _ := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "auto"), `{"task_code":"a"}`)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("want 501 without the verb, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// auto_all
// ---------------------------------------------------------------------------

func TestTaskAutoAllReportsEveryStep(t *testing.T) {
	fastAutoGap(t)
	c := verbClient("verb", []core.TaskInfo{
		{Code: "todo", Title: "待做", Auto: true},
		{Code: "done", Title: "已领", Auto: true, Claimed: true},
		{Code: "human", Title: "需人工", Auto: false},
	}, core.AccountRecord{ID: "ui-1"})
	c.bulk = core.BulkAccept{Accepted: 2}
	c.auto["todo"] = core.AutoTaskResult{
		TaskResult:    core.TaskResult{OK: true, Code: "todo", Credit: 3},
		ProgressAfter: "1/1",
		Claimed:       true,
	}
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "auto_all"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	rows, ok := out["results"].([]any)
	if !ok || len(rows) != 3 {
		t.Fatalf("want the accept row plus two chore rows, got %#v", out["results"])
	}
	first, _ := rows[0].(map[string]any)
	if first["task_code"] != bulkAcceptRow || first["status"] != "done" {
		t.Fatalf("the accept phase must lead the report, got %v", first)
	}
	second, _ := rows[1].(map[string]any)
	if second["task_code"] != "todo" || second["status"] != "done" {
		t.Fatalf("the automated chore is missing, got %v", second)
	}
	if second["progress_after"] != "1/1" {
		t.Fatalf("the read-back is missing, got %v", second)
	}
	third, _ := rows[2].(map[string]any)
	if third["task_code"] != "done" || third["status"] != "skipped" {
		t.Fatalf("an already-claimed chore must be reported as skipped, got %v", third)
	}
	// A chore with no automation is not a row at all: reporting it as an error
	// would blame the operator for a chore nobody can automate.
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row["task_code"] == "human" {
			t.Fatalf("a non-automatable chore must not appear: %v", row)
		}
	}
	if got := c.codesAutoRun(); len(got) != 1 || got[0] != "todo" {
		t.Fatalf("the round ran %v, want only [todo]", got)
	}
}

func TestTaskAutoAllReportsAVendorFailureAsARow(t *testing.T) {
	fastAutoGap(t)
	c := verbClient("verb", []core.TaskInfo{{Code: "todo", Auto: true}},
		core.AccountRecord{ID: "ui-1"})
	c.autoErr["todo"] = errors.New("vendor exploded; access_token=abcdef123456")
	p := taskPanel(t, c)

	rec, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "auto_all"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("a round is a report, not an error: status=%d body=%s", rec.Code, rec.Body.String())
	}
	rows, _ := out["results"].([]any)
	// The accept phase leads (this fake can bulk-accept), then the chore that
	// blew up: a round is a report, and a vendor refusal is one line of it.
	if len(rows) != 2 {
		t.Fatalf("want the accept row plus the failed chore, got %#v", out["results"])
	}
	row, _ := rows[1].(map[string]any)
	if row["task_code"] != "todo" || row["status"] != "error" {
		t.Fatalf("want status=error on the chore, got %v", row)
	}
	msg, _ := row["message"].(string)
	if contains(msg, "abcdef123456") {
		t.Fatalf("the row leaks a credential: %q", msg)
	}
}

func TestTaskAutoAllIsRefusedWhileTheAccountIsBusy(t *testing.T) {
	fastAutoGap(t)
	c := verbClient("verb", []core.TaskInfo{{Code: "a", Auto: true}},
		core.AccountRecord{ID: "ui-1"})
	c.auto["a"] = core.AutoTaskResult{TaskResult: core.TaskResult{OK: true, Code: "a"}}
	c.entered = make(chan string, 4)
	c.release = make(chan struct{})
	p := taskPanel(t, c)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "auto_all"), "")
	}()
	select {
	case <-c.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the round never entered the vendor call")
	}

	rec, out := doTask(t, p, http.MethodPost, verbPath("verb", "ui-1", "auto_all"), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("want 409 while the account is busy, got %d", rec.Code)
	}
	if out["error"] != msgTaskBusy {
		t.Fatalf("want the reference wording, got %#v", out["error"])
	}

	close(c.release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the first round never returned")
	}
}

func TestTaskAutoAllRejectsTheWrongMethodAndMissingVerb(t *testing.T) {
	c := verbClient("verb", nil, core.AccountRecord{ID: "ui-1"})
	p := taskPanel(t, c)

	rec, _ := doTask(t, p, http.MethodGet, verbPath("verb", "ui-1", "auto_all"), "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405 on GET, got %d", rec.Code)
	}

	bare := &fakePoolTaskClient{
		fakeTaskClient: &fakeTaskClient{name: "verb"},
		accounts:       []core.AccountRecord{{ID: "ui-1"}},
	}
	p2 := taskPanel(t, bare)
	rec, _ = doTask(t, p2, http.MethodPost, verbPath("verb", "ui-1", "auto_all"), "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("want 501 without the verb, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// Capability reporting
// ---------------------------------------------------------------------------

func TestCapabilitiesReportTheTaskVerbs(t *testing.T) {
	full := verbClient("full", nil)
	caps := core.CapabilitiesOf(context.Background(), full)
	if !caps.Tasks || !caps.TaskAccept || !caps.TaskClaim || !caps.TaskAuto {
		t.Fatalf("a module with every verb must advertise all of them: %+v", caps)
	}

	// A board and nothing else: the frontend must still render the board, and
	// hide every per-task button.
	board := &fakePoolTaskClient{fakeTaskClient: &fakeTaskClient{name: "board"}}
	caps = core.CapabilitiesOf(context.Background(), board)
	if !caps.Tasks || caps.TaskAccept || caps.TaskClaim || caps.TaskAuto {
		t.Fatalf("a board-only module must advertise no verbs: %+v", caps)
	}
}
