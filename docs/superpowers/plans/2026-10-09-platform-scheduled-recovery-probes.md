# Platform Scheduled Recovery Probes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a per-platform scheduled recovery probe with configurable interval and random jitter, while keeping existing daily batch schedules backward compatible.

**Architecture:** Extend the scheduler's timetable with one periodic `recovery` group. The scheduler keeps one next-fire instant per platform, applies a random offset in `[every-jitter, every+jitter]`, and invokes a host hook for that platform. The panel renders one synthetic recovery row per registered platform, grouped under that platform, and writes the same `schedule.*` config shape already used by the task centre.

**Tech Stack:** Go 1.24, `internal/scheduler`, `cmd/client2api`, `internal/panel/index.html`, `internal/core`, Go `testing`.

**Spec:** User request in the active conversation: group scheduled tasks by platform, add recovery probing to every platform, and let each interval task use a configurable random value in minutes or hours.

## Global Constraints

- Source of truth is `D:\client2api-src`; do not touch the running `D:\client2api` service.
- Existing daily batches (`checkin`, `travel`, `activity`, `keepalive`, `blackcat`, `growth`) keep their current semantics.
- Existing config files with no recovery keys must keep working and receive defaults: enabled, 4 hours, jitter 1 hour.
- `jitter` is symmetric: `every=4h`, `jitter=1h` means the next probe is between 3h and 5h.
- The panel must expose minutes and hours, not raw seconds only.
- Explicit operator actions (refresh, test, revive) must bypass the automatic recovery window.
- Do not commit, push, or build an installer unless the user asks separately.

## Review Focus

- A disabled recovery group must not fire or appear as scheduled.
- A zero or negative jitter must behave as "no jitter", not as an invalid probe.
- A platform with no recovery override must inherit the shared defaults; a platform override must not mutate the shared defaults.
- A recovery tick must not get stuck after the host hook returns, and `Status` must show the next stored fire.
- Existing daily-hour config serialization and the panel's old fields must remain readable after adding interval fields.

---

### Task 1: Scheduler periodic recovery group

**Files:**
- Modify: `internal/scheduler/scheduler.go`
- Test: `internal/scheduler/scheduler_test.go`

**Interfaces:**
- Produces: `RecoveryTaskName = "recovery"`, `Group.Every time.Duration`, `Group.Jitter time.Duration`, `Config.Recovery Group`, `Deps.OnRecoveryProbe func(ctx context.Context, client string)`, `Runner.RunRecoveryNow(ctx context.Context, client string) bool`.
- Consumes: existing `Config.GroupFor`, `Runner.plan`, `Runner.Status`.

- [ ] **Step 1: Write failing tests** for:
  - `Config{Rest:..., Recovery: Group{Enabled:true, Every:4*time.Hour, Jitter:time.Hour}}` validating successfully.
  - `GroupFor("loomy","recovery")` returning the override when configured.
  - a fake registry client receiving `OnRecoveryProbe` after the stored next fire.
  - `RunRecoveryNow` invoking the hook without changing the stored next fire.
  - `Status().NextByClient["fake/recovery"]` being present and inside the 3h..5h window.
- [ ] **Step 2: Run the scheduler package tests** and verify the new tests fail because the recovery group does not exist.
- [ ] **Step 3: Implement the minimal scheduler changes:** add `Every`/`Jitter`, include `recovery` in shared groups, store per-client next recovery instants, add jittered period calculation, include recovery fires in `plan`/`Run`/`Status`, and add `RunRecoveryNow`.
- [ ] **Step 4: Run the scheduler tests** and verify the new tests pass with the existing daily-batch tests.
- [ ] **Step 5: Commit** (only if the user later asks for commits).

### Task 2: Config projection and platform-scoped host hook

**Files:**
- Modify: `cmd/client2api/main.go`
- Modify: `cmd/client2api/renew.go`
- Modify: `internal/panel/balancecache.go`
- Modify: `internal/core/accounts.go`
- Test: `cmd/client2api/schedule_test.go`
- Test: `cmd/client2api/background_probe_test.go`
- Test: `internal/scheduler/scheduler_test.go`

**Interfaces:**
- Consumes: Task 1's `Group.Every`, `Group.Jitter`, `Deps.OnRecoveryProbe`.
- Produces: file-config keys `recovery_enabled`, `recovery_every_minutes`, `recovery_jitter_minutes`; per-client override keys `every_minutes`, `jitter_minutes`; `core.IsRecoverableAccountState(state string) bool`; `refreshBalancesForClient(..., includeRecoverable bool)`.

- [ ] **Step 1: Write failing projection tests** proving:
  - missing recovery keys project to enabled/4h/1h;
  - explicit `recovery_enabled:false` disables the shared group;
  - a per-platform `{enabled:false, every_minutes:120, jitter_minutes:30}` override projects to that platform only;
  - a zero jitter override stays zero and does not inherit the default.
- [ ] **Step 2: Run `go test ./cmd/client2api ./internal/scheduler`** and verify the new assertions fail.
- [ ] **Step 3: Implement the config projection and hook:** add pointer-backed interval/jitter fields, build the shared and per-platform `recovery` groups, and wire `OnRecoveryProbe` to a platform-scoped balance/renewal sweep.
- [ ] **Step 4: Implement the probe split:** global background sweeps skip accounts in cooling/rate-limited/exhausted/quota states; the recovery hook includes those accounts and bypasses only the per-account recovery gate. Explicit panel refreshes remain unchanged.
- [ ] **Step 5: Run the focused tests** and then `go test ./... -count=1`.
- [ ] **Step 6: Commit** (only if the user later asks for commits).

### Task 3: Panel API recovery rows and grouping data

**Files:**
- Modify: `internal/panel/scheduleview.go`
- Test: `internal/panel/schedule_test.go`

**Interfaces:**
- Consumes: Task 1's `RecoveryTaskName`, `Group.Every`, `Group.Jitter`.
- Produces: one `scheduleRow` per registered client with `batch:"recovery"`, `every_minutes`, `jitter_minutes`, and effective `next`; groups API entry for `recovery`.

- [ ] **Step 1: Write a failing API/view test** that builds a fake registry with two clients and asserts:
  - every client gets a recovery row even when it plans no daily batch;
  - the recovery row reports the effective interval and jitter;
  - `next` is keyed by `client/recovery`.
- [ ] **Step 2: Run `go test ./internal/panel -run Schedule`** and verify it fails.
- [ ] **Step 3: Implement the synthetic recovery row and include `recovery` in the schedule payload's groups.**
- [ ] **Step 4: Run the panel schedule tests** and verify they pass.
- [ ] **Step 5: Commit** (only if the user later asks for commits).

### Task 4: Task-centre UI grouping and interval/jitter editing

**Files:**
- Modify: `internal/panel/index.html`
- Test: `internal/panel/schedule_ui_test.go`

**Interfaces:**
- Consumes: Task 3's `recovery` rows and root recovery fields.
- Produces: platform-grouped schedule table; recovery row controls; `PATCH /panel/api/config` payload with `recovery_*` and per-client `every_minutes`/`jitter_minutes`.

- [ ] **Step 1: Write failing static UI tests** that require:
  - `SC_RECOVERY`/`recovery` labels and controls;
  - platform group headers in `scRenderRows`;
  - recovery interval and jitter inputs in `scRowHTML`;
  - `scSave` writing `recovery_enabled`, `recovery_every_minutes`, `recovery_jitter_minutes`, and per-client interval/jitter keys;
  - minute/hour unit parsing.
- [ ] **Step 2: Run `go test ./internal/panel -run Schedule`** and verify the new tests fail.
- [ ] **Step 3: Implement the UI:** group rows by platform, render recovery rows as a first-class task, add duration controls with minute/hour units, and persist the new keys.
- [ ] **Step 4: Run the panel tests and the full suite.**
- [ ] **Step 5: Commit** (only if the user later asks for commits).

## Self-Review

- Spec coverage: platform grouping is Task 3/4; recovery task per platform is Tasks 1/3/4; interval plus minute/hour jitter is Tasks 1/2/4; avoiding excessive automatic probing is Task 2.
- Type consistency: `Every` and `Jitter` are `time.Duration`; file config uses minute pointers; `RecoveryTaskName` is the single batch key.
- Testability: each task has a focused command before the final full-suite command.
- Deferred intentionally: no push, no installer, no release; those require a later explicit request.
