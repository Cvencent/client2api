# Account-First Task Center Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the task center account-first: Loomy growth tasks appear in schedules, batch execution and board share one batch model, and every task state is filtered by account.

**Architecture:** Extend the existing generic `core.Batch` and scheduler config instead of inventing a second task system. Loomy declares a `growth` batch with `PendingOnly`; the scheduler filters pending codes per account; the panel exposes account scope and an all-account task aggregate; the frontend sends and remembers the selected account across all three task-center tabs.

**Tech Stack:** Go 1.27, `internal/scheduler`, `internal/panel`, `clients/loomy`, vanilla JS in `internal/panel/index.html`.

---

## File Map

- Modify: `internal/core/batches.go` — add `PendingOnly`.
- Create: `clients/loomy/batches.go` — Loomy `BatchPlanner`.
- Create: `clients/loomy/batches_test.go` — growth batch tests.
- Modify: `internal/scheduler/scheduler.go` — pending-only execution and account scope.
- Modify: `internal/scheduler/scheduler_test.go` — pending-only and scope tests.
- Modify: `cmd/client2api/main.go` — project account scope config.
- Modify: `cmd/client2api/schedule_test.go` — config projection tests.
- Modify: `internal/panel/configwrite.go` — validate account scope.
- Modify: `internal/panel/configwrite_test.go` — bad scope validation.
- Modify: `internal/panel/scheduleview.go` — schedule rows carry account scope and pending-only.
- Modify: `internal/panel/schedule_test.go` — schedule row tests.
- Modify: `internal/panel/taskboard.go` — account/all task aggregate and account-filtered runs.
- Modify: `internal/panel/taskboard_test.go` — API aggregate tests.
- Modify: `internal/panel/index.html` — shared account selector and account-aware task board.
- Modify: `internal/panel/taskboard_ui_test.go` — frontend account binding tests.

---

### Task 1: Loomy growth batch

**Files:**
- Modify: `internal/core/batches.go`
- Create: `clients/loomy/batches.go`
- Create: `clients/loomy/batches_test.go`

- [ ] **Step 1: Write the failing test**

Create `clients/loomy/batches_test.go`:

```go
package loomy

import (
	"testing"

	"client2api/internal/core"
)

func TestLoomyPlansTheGrowthBatch(t *testing.T) {
	c := &Client{}
	batches := c.Batches()
	if len(batches) != 1 {
		t.Fatalf("Batches() = %d batches, want one growth batch", len(batches))
	}
	b := batches[0]
	if b.Name != "growth" {
		t.Fatalf("batch name = %q, want growth", b.Name)
	}
	if !b.PendingOnly {
		t.Fatal("growth batch must skip tasks the vendor already reports complete")
	}
	if len(b.Codes) != len(onboardingRegistry) {
		t.Fatalf("codes = %d, want %d", len(b.Codes), len(onboardingRegistry))
	}
	want := make(map[string]bool, len(onboardingRegistry))
	for _, ch := range onboardingRegistry {
		want[ch.Key] = true
	}
	for _, code := range b.Codes {
		if !want[code] {
			t.Fatalf("unexpected code %q", code)
		}
	}
}

func TestLoomyGrowthBatchIsAPlannerAndTaskProvider(t *testing.T) {
	var c *Client
	_ = core.BatchPlanner(c)
	_ = core.TaskProvider(c)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -count=1 ./clients/loomy -run 'TestLoomyPlansTheGrowthBatch|TestLoomyGrowthBatchIsAPlannerAndTaskProvider'`

Expected: FAIL because `Batches` and `PendingOnly` do not exist.

- [ ] **Step 3: Add `PendingOnly` to `core.Batch`**

In `internal/core/batches.go`, add after `Claim`:

```go
	// PendingOnly means the executor must re-read Tasks for each account and
	// run only codes that are still actionable.  It is for one-off growth
	// chores: running an already-claimed chore is harmless but pollutes the
	// run journal and looks like work that still needs doing.
	PendingOnly bool `json:"pending_only,omitempty"`
```

- [ ] **Step 4: Add Loomy `Batches()`**

Create `clients/loomy/batches.go`:

```go
package loomy

import "client2api/internal/core"

// Batches implements core.BatchPlanner.  The onboarding registry is static,
// so the scheduler can discover the growth batch without touching the network.
func (c *Client) Batches() []core.Batch {
	codes := make([]string, 0, len(onboardingRegistry))
	for _, ch := range onboardingRegistry {
		codes = append(codes, ch.Key)
	}
	return []core.Batch{{
		Name:        "growth",
		Codes:       codes,
		PendingOnly: true,
	}}
}
```

- [ ] **Step 5: Run the tests and make sure they pass**

Run: `go test -count=1 ./clients/loomy ./internal/core`

Expected: PASS.

---

### Task 2: Pending-only scheduler execution

**Files:**
- Modify: `internal/scheduler/scheduler.go`
- Modify: `internal/scheduler/scheduler_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/scheduler/scheduler_test.go`:

```go
func TestPendingOnlyBatchSkipsClaimedCodes(t *testing.T) {
	c := &scheduleTaskClient{
		tasks: []core.TaskInfo{
			{Code: "done", Claimed: true, Auto: true},
			{Code: "todo", Auto: true},
			{Code: "locked", Locked: true, Auto: true},
		},
	}
	reg := core.NewRegistry()
	reg.Add(c)
	r := New(Deps{
		Registry: reg,
		Now:      time.Now,
		Sleep:    func(context.Context, time.Duration) bool { return true },
		Logf:     func(string, ...any) {},
	})
	r.Reconfigure(Config{Enabled: true, Growth: Group{Enabled: true, Hours: []int{12}}})
	c.batch = core.Batch{Name: "growth", Codes: []string{"done", "todo", "locked"}, PendingOnly: true}
	rep := r.runBatch(context.Background(), c.Name(), "growth")
	if rep.Ran != 1 {
		t.Fatalf("ran = %d, want only the pending task", rep.Ran)
	}
	if got := c.ranCodes(); len(got) != 1 || got[0] != "todo" {
		t.Fatalf("ran codes = %v, want [todo]", got)
	}
}
```

If `scheduleTaskClient` does not exist, add a minimal fake near the other scheduler fakes:

```go
type scheduleTaskClient struct {
	name    string
	tasks   []core.TaskInfo
	batch   core.Batch
	mu      sync.Mutex
	ran     []string
}

func (c *scheduleTaskClient) Name() string { return c.name }
func (c *scheduleTaskClient) Models(context.Context) ([]core.Model, error) { return nil, nil }
func (c *scheduleTaskClient) Chat(context.Context, *core.ChatRequest) (core.Stream, error) { return nil, nil }
func (c *scheduleTaskClient) Status(context.Context) core.Status { return core.Status{Ready: true} }
func (c *scheduleTaskClient) Tasks(context.Context, string) ([]core.TaskInfo, error) { return c.tasks, nil }
func (c *scheduleTaskClient) RunTask(_ context.Context, _, code string) (core.TaskResult, error) {
	c.mu.Lock()
	c.ran = append(c.ran, code)
	c.mu.Unlock()
	return core.TaskResult{OK: true}, nil
}
func (c *scheduleTaskClient) Batches() []core.Batch { return []core.Batch{c.batch} }
func (c *scheduleTaskClient) ranCodes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ran...)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -count=1 ./internal/scheduler -run TestPendingOnlyBatchSkipsClaimedCodes`

Expected: FAIL because the scheduler runs all three codes.

- [ ] **Step 3: Implement pending-only filtering**

In `internal/scheduler/scheduler.go`, add helper:

```go
func pendingCodes(ctx context.Context, tp core.TaskProvider, account string, codes []string) ([]string, error) {
	list, err := tp.Tasks(ctx, account)
	if err != nil {
		return nil, err
	}
	pending := make(map[string]bool)
	for _, t := range list {
		if t.Claimed || t.Locked || !t.Auto {
			continue
		}
		pending[t.Code] = true
	}
	out := make([]string, 0, len(codes))
	for _, code := range codes {
		if pending[code] {
			out = append(out, code)
		}
	}
	return out, nil
}
```

Inside the account loop in `runBatchAs`, after `acc := rec.ID`, replace the fixed code loop source with:

```go
		accountCodes := codes
		if b.PendingOnly {
			next, err := pendingCodes(ctx, tp, acc, codes)
			if err != nil {
				rep.Failed++
				rep.Errors = append(rep.Errors, err.Error())
				r.log("[scheduler] %s/%s account %q: reading pending tasks failed: %v", clientName, batchName, acc, err)
				continue
			}
			accountCodes = next
		}
```

Then change the inner loop to `for _, code := range accountCodes`.

- [ ] **Step 4: Run the test and make sure it passes**

Run: `go test -count=1 ./internal/scheduler -run TestPendingOnlyBatchSkipsClaimedCodes`

Expected: PASS.

---

### Task 3: Account scope in scheduler config

**Files:**
- Modify: `internal/scheduler/scheduler.go`
- Modify: `cmd/client2api/main.go`
- Modify: `cmd/client2api/schedule_test.go`
- Modify: `internal/panel/configwrite.go`
- Modify: `internal/panel/configwrite_test.go`

- [ ] **Step 1: Write failing scheduler test**

Add to `internal/scheduler/scheduler_test.go`:

```go
func TestScheduledBatchHonoursAccountScope(t *testing.T) {
	c := &scheduleTaskClient{
		name: "wb",
		tasks: []core.TaskInfo{{Code: "todo", Auto: true}},
		batch: core.Batch{Name: "growth", Codes: []string{"todo"}},
	}
	reg := core.NewRegistry()
	reg.Add(c)
	r := New(Deps{Registry: reg, Now: time.Now, Sleep: func(context.Context, time.Duration) bool { return true }})
	r.Reconfigure(Config{
		Enabled: true,
		Growth: Group{
			Enabled: true,
			Hours:   []int{12},
			Accounts: AccountScope{
				Mode:    AccountScopeInclude,
				Include: []string{"a2"},
			},
		},
	})
	rep := r.runBatch(context.Background(), "wb", "growth")
	if rep.Ran != 0 {
		t.Fatalf("ran = %d, want no account in the include scope", rep.Ran)
	}
}
```

- [ ] **Step 2: Implement account scope types**

In `internal/scheduler/scheduler.go`, add near `Group`:

```go
const (
	AccountScopePlatform = "platform"
	AccountScopeInclude  = "include"
	AccountScopeExclude  = "exclude"
)

type AccountScope struct {
	Mode    string
	Include []string
	Exclude []string
}

func (s AccountScope) Allows(account string) bool {
	switch strings.ToLower(strings.TrimSpace(s.Mode)) {
	case AccountScopeInclude:
		for _, id := range s.Include {
			if id == account {
				return true
			}
		}
		return false
	case AccountScopeExclude:
		for _, id := range s.Exclude {
			if id == account {
				return false
			}
		}
		return true
	default:
		return true
	}
}
```

Add `Accounts AccountScope` to `Group`.

- [ ] **Step 3: Apply the scope**

In `runBatchAs`, after `accounts, err := accountRecordsOfForBatch(...)`, filter:

```go
	group, _ := r.cfg.GroupFor(clientName, batchName)
	filtered := accounts[:0]
	for _, rec := range accounts {
		if group.Accounts.Allows(rec.ID) {
			filtered = append(filtered, rec)
		}
	}
	accounts = filtered
```

- [ ] **Step 4: Project file config**

In `cmd/client2api/main.go`, add:

```go
type scheduleAccountScope struct {
	Mode    string   `json:"mode"`
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}
```

Add `Accounts *scheduleAccountScope` to `scheduleOverride`.

In `schedClients()` map it onto `scheduler.Group{...}`.

- [ ] **Step 5: Validate config**

In `validateScheduleConfig`, for each override:

```go
			if a, ok := om["accounts"]; ok && a != nil {
				am, ok := a.(map[string]any)
				if !ok {
					return fmt.Errorf("schedule.clients.%s.%s.accounts must be an object", client, batch)
				}
				if mode, ok := am["mode"]; ok && mode != nil {
					s, ok := mode.(string)
					if !ok || (s != "platform" && s != "include" && s != "exclude") {
						return fmt.Errorf("schedule.clients.%s.%s.accounts.mode must be platform, include or exclude", client, batch)
					}
				}
				for _, key := range []string{"include", "exclude"} {
					if raw, ok := am[key]; ok && raw != nil {
						arr, ok := raw.([]any)
						if !ok {
							return fmt.Errorf("schedule.clients.%s.%s.accounts.%s must be an array of account ids", client, batch, key)
						}
						for _, item := range arr {
							if _, ok := item.(string); !ok {
								return fmt.Errorf("schedule.clients.%s.%s.accounts.%s must contain only strings", client, batch, key)
							}
						}
					}
				}
			}
```

- [ ] **Step 6: Run tests**

Run: `go test -count=1 ./internal/scheduler ./cmd/client2api ./internal/panel`

Expected: PASS.

---

### Task 4: Schedule rows expose scope and Loomy growth

**Files:**
- Modify: `internal/panel/scheduleview.go`
- Modify: `internal/panel/schedule_test.go`

- [ ] **Step 1: Write failing test**

Add to `internal/panel/schedule_test.go`:

```go
func TestScheduleRowsIncludeLoomyGrowthAndAccountScope(t *testing.T) {
	c := &fakePoolTaskClient{
		fakeTaskClient: &fakeTaskClient{name: "loomy"},
		accounts: []core.AccountRecord{{ID: "a1", Label: "one", Enabled: true}},
	}
	c.batch = core.Batch{Name: "growth", Codes: []string{"share_soul"}, PendingOnly: true}
	p := statusPanel(t, &fakeScheduler{
		cfg: scheduler.Config{
			Enabled: true,
			Growth: scheduler.Group{
				Enabled: true,
				Hours:   []int{12},
				Accounts: scheduler.AccountScope{Mode: scheduler.AccountScopeExclude, Exclude: []string{"a2"}},
			},
		},
	}, c)
	rows := p.scheduleRows(p.opts.Scheduler.Config(), p.opts.Scheduler.Status())
	if len(rows) != 1 || rows[0].Client != "loomy" || rows[0].Batch != "growth" {
		t.Fatalf("rows = %+v, want loomy/growth", rows)
	}
	if rows[0].AccountScope.Mode != "exclude" || len(rows[0].AccountScope.Exclude) != 1 {
		t.Fatalf("account scope lost: %+v", rows[0].AccountScope)
	}
}
```

Adjust the fake type in the test file if needed so `fakePoolTaskClient` can expose a `Batches()` method.

- [ ] **Step 2: Add fields to `scheduleRow`**

In `internal/panel/scheduleview.go`, add:

```go
	PendingOnly  bool               `json:"pending_only"`
	AccountScope scheduler.AccountScope `json:"account_scope"`
```

Set them from the batch and group in `scheduleRows`.

- [ ] **Step 3: Run the test**

Run: `go test -count=1 ./internal/panel -run TestScheduleRowsIncludeLoomyGrowthAndAccountScope`

Expected: PASS.

---

### Task 5: Account-aware task board API

**Files:**
- Modify: `internal/panel/taskboard.go`
- Modify: `internal/panel/taskboard_test.go`

- [ ] **Step 1: Write failing test**

Add to `internal/panel/taskboard_test.go`:

```go
func TestTaskListFiltersRunsByAccount(t *testing.T) {
	c := &fakeTaskClient{
		name: "loomy",
		tasks: []core.TaskInfo{{Code: "share_soul", Auto: true}},
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
```

- [ ] **Step 2: Implement account filtering**

In `taskList`, after `list, err := tp.Tasks(ctx, accountID)`:

```go
	runs := p.runs.forClient(c.Name(), 50)
	if accountID != "" {
		filtered := runs[:0]
		for _, run := range runs {
			if run.Account == accountID {
				filtered = append(filtered, run)
			}
		}
		runs = filtered
	}
```

Then return `runs` instead of `p.runs.forClient(...)`.

- [ ] **Step 3: Add all-account aggregate**

Add `taskListAll` that:

1. Lists accounts via `core.AsAccountManager`.
2. Calls `tp.Tasks(ctx, accountID)` for each enabled account.
3. Filters runs per account.
4. Returns `{"accounts": [...]}`.

Keep concurrency capped at `taskScanConc`.

- [ ] **Step 4: Route `?all=1` in `taskList`**

If `r.URL.Query().Get("all") == "1"`, call `taskListAll` and return.

- [ ] **Step 5: Run tests**

Run: `go test -count=1 ./internal/panel -run 'TestTaskListFiltersRunsByAccount|TestTaskListAll'`

Expected: PASS.

---

### Task 6: Frontend account-first task center

**Files:**
- Modify: `internal/panel/index.html`
- Modify: `internal/panel/taskboard_ui_test.go`

- [ ] **Step 1: Add shared state**

Near `const TB = { client: null, tasks: [], runs: [], timer: null };`, add:

```js
const TASKCENTER = {
  client: null,
  account: "",
  allAccounts: false,
  batch: "",
};
```

- [ ] **Step 2: Render account selector**

In `renderTaskBoard`, after rendering client chips:

1. Fetch `/panel/api/clients/<client>/accounts` or reuse `ACC[client].accounts` if available.
2. Render a select-like chip row for accounts plus `全部账号`.
3. Persist `TASKCENTER.account`.
4. Request `/tasks?account=...` for one account or `/tasks?all=1` for all.

- [ ] **Step 3: Filter runs by account**

Replace:

```js
const tbRuns = code => TB.runs.filter(r => r.code === code);
```

with:

```js
const tbRuns = code => TB.runs.filter(r =>
  r.code === code &&
  (!TASKCENTER.account || r.account === TASKCENTER.account)
);
```

- [ ] **Step 4: Send account on run**

Replace the `runOneTask` POST body `{}` with:

```js
{ method: "POST", body: { account: TASKCENTER.account } }
```

- [ ] **Step 5: Cross-link schedule/batch**

Add a link or button in schedule rows to switch to `taskscenter` with the same client and batch. Add a button in the batch card to switch to the board with that client and account.

- [ ] **Step 6: Add UI checks**

In `internal/panel/taskboard_ui_test.go`, assert the HTML contains:

```go
for _, want := range []string{
	`const TASKCENTER =`,
	`/tasks?account=`,
	`body: { account: TASKCENTER.account }`,
} {
	if !strings.Contains(page, want) {
		t.Fatalf("task center UI missing %q", want)
	}
}
```

- [ ] **Step 7: Run UI tests**

Run: `go test -count=1 ./internal/panel -run 'TestTask|TestSchedule'`

Expected: PASS.

---

### Task 7: Full verification

- [ ] **Step 1: Format**

Run: `gofmt -w clients/loomy/batches.go clients/loomy/batches_test.go internal/core/batches.go internal/scheduler/scheduler.go internal/scheduler/scheduler_test.go cmd/client2api/main.go cmd/client2api/schedule_test.go internal/panel/configwrite.go internal/panel/configwrite_test.go internal/panel/scheduleview.go internal/panel/schedule_test.go internal/panel/taskboard.go internal/panel/taskboard_test.go`

- [ ] **Step 2: Run all tests**

Run: `go test -count=1 ./...`

Expected: PASS.

- [ ] **Step 3: Build**

Run: `go build ./...`

Expected: exit 0.

- [ ] **Step 4: Commit**

```bash
git add clients/loomy/batches.go clients/loomy/batches_test.go internal/core/batches.go internal/scheduler/scheduler.go internal/scheduler/scheduler_test.go cmd/client2api/main.go cmd/client2api/schedule_test.go internal/panel/configwrite.go internal/panel/configwrite_test.go internal/panel/scheduleview.go internal/panel/schedule_test.go internal/panel/taskboard.go internal/panel/taskboard_test.go internal/panel/index.html internal/panel/taskboard_ui_test.go
git commit -m "feat: redesign task center around accounts"
```
