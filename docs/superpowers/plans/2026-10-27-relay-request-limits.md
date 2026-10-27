# 中转站请求次数限额 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 让用户在中转站编辑框里用表单配置可叠加的请求次数规则，并让计数跨重启持久化。

**Architecture:** 核心配置新增 `request_limits` 规则数组，同时保留旧的两个分钟字段并转换成规则。OpenAI-compatible 中转站用一个独立的 limiter 统一执行滚动、自然周期和首次请求窗口，计数写入该中转站数据目录。面板只负责把规则渲染成表单、读取表单并保存，不暴露 JSON 编辑器。

**Tech Stack:** Go、标准库 `time`/`encoding/json`、现有 `core.WriteJSONAtomic`、内联 HTML/CSS/JavaScript、`httptest`、Node 语法检查。

**Spec:** `docs/superpowers/specs/2026-10-27-relay-request-limits-design.md`

## Global Constraints

- 只改 `D:\client2api-src`；不碰 `D:\client2api`，不停止、重启或替换正在运行的服务。
- 面板不提供 JSON 输入框；JSON 只是配置落盘格式。
- 旧 `requests_per_minute` 和 `requests_per_minute_per_account` 必须继续兼容，旧行为等价于 `reset: "first"`。
- 请求计数必须落盘；计数写入失败时拒绝本次上游请求，不能静默超额。
- 不引入新的第三方依赖。
- 新规则只处理请求次数；现有 `max_in_flight` 行为不变。
- 验证命令：`go build ./...`、`go test ./...`，以及面板脚本的 Node 语法测试。
- 不执行安装包，不修改正式部署的配置或数据。
- 当前会话不提交 commit、不推送；完成源码修改和验证后交给用户检查。

## Review Focus

- 只有旧分钟字段的配置必须保持“首次请求起算”的原行为，不能悄悄改成滚动窗口。
- 滚动窗口和自然周期的边界必须精确，尤其不能因重启或跨过重置时刻而多放行。
- 多条规则必须原子扣减；单 Key 满了要换 Key，整个中转站满了才返回忙。
- 计数文件写入失败或损坏时不能静默当成 0；必须 fail closed 并留下可读错误。
- 面板迁移旧字段、保存新规则时必须保留显式 `max_in_flight: 0`，且不能出现 JSON 文本框。

---

### Task 1: 配置模型、校验和旧字段迁移

**Files:**
- Modify: `internal/core/sources.go`
- Modify: `internal/core/source_limits_test.go`

**Interfaces:**
- Produces:
  - `type SourceRequestLimit struct { Scope string; Limit int; Window string; Reset string; Timezone string; ResetAt string }`
  - `func (cfg SourceConfig) EffectiveRequestLimits() []SourceRequestLimit`
  - `func (limit SourceRequestLimit) Signature() string`
- `SourceConfig` gains `RequestLimits []SourceRequestLimit` with JSON key `request_limits`.

- [x] **Step 1: Write failing tests for parsing and legacy conversion**

Add `TestParseSourcesAcceptsRequestLimits` to `internal/core/source_limits_test.go`:

- Parse a source with:
  - account rule `limit: 100`, `window: "24h"`, `reset: "rolling"`
  - source rule `limit: 500`, `window: "24h"`, `reset: "calendar"`, `timezone: "Asia/Shanghai"`, `reset_at: "08:00"`
- Assert both entries survive, the calendar timezone/reset_at survive, and `Signature()` is stable for identical rules.

Add `TestEffectiveRequestLimitsFromLegacy`:

- `SourceConfig{RequestsPerMinute: 120, RequestsPerMinutePerAccount: 30}` yields two rules:
  - `{Scope:"source", Limit:120, Window:"1m", Reset:"first"}`
  - `{Scope:"account", Limit:30, Window:"1m", Reset:"first"}`
- A non-empty `RequestLimits` list wins over both legacy fields.
- Zero legacy fields produce no synthesized rule.

Add `TestParseSourcesRejectsInvalidRequestLimits` table cases:

- `scope: "key"` -> error mentions `scope`
- `limit: 0` -> error mentions `limit`
- `window: "later"` -> error mentions `window`
- `reset: "sometimes"` -> error mentions `reset`
- `timezone: "Mars/Olympus"` -> error mentions `timezone`
- `reset_at: "25:00"` -> error mentions `reset_at`
- calendar `window: "7h"` -> error mentions `calendar`
- duplicate rules with the same signature -> error mentions `duplicate`

- [x] **Step 2: Run the tests and confirm they fail**

Run:

```text
go test ./internal/core -run 'Test(ParseSourcesAcceptsRequestLimits|EffectiveRequestLimitsFromLegacy|ParseSourcesRejectsInvalidRequestLimits)'
```

Expected: FAIL because the new field, method, and validation do not exist.

- [x] **Step 3: Implement the core types and validation**

In `internal/core/sources.go`:

- Add `SourceRequestLimit` near `SourceConfig`.
- Add `RequestLimits` to `SourceConfig`.
- Normalize defaults in `Signature()`:
  - empty `reset` -> `rolling`
  - empty `reset_at` -> `00:00`
  - empty `timezone` -> `time.Local.String()`
  - normalize `window` with `time.ParseDuration(...).String()`
- Implement `EffectiveRequestLimits()` with the legacy mapping above.
- Validate each new rule in `ValidateSources`:
  - allowed scopes: `account`, `source`
  - `limit > 0`
  - `window` parses as a positive Go duration
  - allowed resets: `rolling`, `calendar`, `first`
  - calendar window divides 24 hours
  - `reset_at` parses with `time.Parse("15:04", ...)`
  - timezone loads with `time.LoadLocation`
  - duplicate signatures are rejected
- Keep existing strict unknown-field behavior.

- [x] **Step 4: Run the tests and confirm they pass**

Run the same targeted command. Expected: PASS.

---

### Task 2: 请求限额引擎和持久化

**Files:**
- Create: `clients/openaicompat/request_limits.go`
- Create: `clients/openaicompat/request_limits_test.go`

**Interfaces:**
- Consumes: `core.SourceRequestLimit` and `core.SourceRequestLimit.Signature()`.
- Produces:
  - `type requestLimitBlock struct { Scope string; Until time.Time }`
  - `func newSourceRequestLimiter(path string, limits []core.SourceRequestLimit) (*sourceRequestLimiter, error)`
  - `func (l *sourceRequestLimiter) Configure(limits []core.SourceRequestLimit) error`
  - `func (l *sourceRequestLimiter) PruneAccounts(ids map[string]struct{})`
  - `func (l *sourceRequestLimiter) AccountBlocked(accountID string, now time.Time) *requestLimitBlock`
  - `func (l *sourceRequestLimiter) SourceBlocked(now time.Time) *requestLimitBlock`
  - `func (l *sourceRequestLimiter) Acquire(accountID string, now time.Time) (*requestLimitBlock, error)`

The limiter owns a mutex and a `request_limits.json` state file. The file schema is:

```go
type requestLimitFile struct {
    Version  int                                          `json:"version"`
    Counters map[string]map[string]requestLimitCounter    `json:"counters"`
}

type requestLimitCounter struct {
    Timestamps []int64 `json:"timestamps,omitempty"`
    Start      int64   `json:"start,omitempty"`
    Used       int     `json:"used,omitempty"`
}
```

The source-wide subject is the literal `__source__`.

- [x] **Step 1: Write failing limiter tests**

Add these tests to `clients/openaicompat/request_limits_test.go`:

- `TestRequestLimiterRollingPerAccount`
  - 1 account, limit 100 / 24h rolling.
  - 100 acquisitions succeed.
  - 101st returns a block with `Scope == "account"` and `Until` equal to the oldest timestamp + 24h.
  - A second account has its own 100-request budget.
- `TestRequestLimiterCalendarResetsAtConfiguredTime`
  - 1 account, limit 1 / 24h calendar, timezone `Asia/Shanghai`, reset_at `08:00`.
  - 07:59:59 and 08:00:00 are in different periods.
  - A second acquisition at 07:59:59 is blocked; at 08:00:00 it is allowed.
- `TestRequestLimiterFirstWindow`
  - limit 1 / 1m first.
  - First request at T succeeds; T+59s blocked; T+1m succeeds.
- `TestRequestLimiterMultiRuleChargesAtomically`
  - account rules: 2/min first and 3/day rolling.
  - The third request within the same minute is blocked by the minute rule.
  - After the minute window, the fourth request is blocked by the daily rule; the daily counter must reflect exactly the allowed attempts.
- `TestRequestLimiterSourceScopeBlocksAllAccounts`
  - source rule limit 1 / 24h rolling.
  - Account A succeeds; account B is blocked.
- `TestRequestLimiterPersistsAcrossReload`
  - Acquire with limit 1 / 24h rolling.
  - Create a second limiter at the same path; its first `Acquire` is blocked.
- `TestRequestLimiterWriteFailureFailsClosed`
  - Make the limiter path point under a regular file (so atomic write fails).
  - `Acquire` returns an error and the in-memory counter does not record the request.
- `TestRequestLimiterCorruptFileRejected`
  - Write invalid JSON to the path.
  - `newSourceRequestLimiter` returns an error mentioning the counter file.

- [x] **Step 2: Run the tests and confirm they fail**

Run:

```text
go test ./clients/openaicompat -run 'TestRequestLimiter'
```

Expected: FAIL because the limiter does not exist.

- [x] **Step 3: Implement the limiter**

Implementation requirements:

- Parse each rule once into a compiled form with `window time.Duration`, `location *time.Location`, and `resetAt time.Duration`.
- `rolling`: keep Unix-millisecond timestamps; prune timestamps at or before `now-window`.
- `calendar`: compute the current period start from the configured timezone, reset_at, and window; store `Start` and `Used`.
- `first`: store `Start` and `Used`; a missing/expired start resets the counter.
- `AccountBlocked` checks only account rules for the given ID; `SourceBlocked` checks only source rules.
- `Acquire` checks all applicable rules, returns the block with the latest `Until` if any rule is full, and charges all rules only when every rule has room.
- Snapshot affected counters, persist through `core.WriteJSONAtomic`, and restore the snapshot if persistence fails.
- Missing file means empty state. Corrupt file or unsupported version is an error from `newSourceRequestLimiter`.
- `Configure` replaces the compiled rule list without clearing counters for signatures that remain present.
- `PruneAccounts` removes account counters whose IDs are no longer present; it never removes the source-wide subject.

- [x] **Step 4: Run the tests and confirm they pass**

Run the same targeted command. Expected: PASS.

---

### Task 3: 接入 OpenAI-compatible 中转站运行时

**Files:**
- Modify: `clients/openaicompat/source.go`
- Modify: `clients/openaicompat/source_limits_test.go`

**Interfaces:**
- Consumes: `sourceRequestLimiter` from Task 2.
- `source` gains `limits *sourceRequestLimiter`.
- `newSource` creates the limiter at `filepath.Join(deps.DataDir, "request_limits.json")`.
- `ConfigureSource` calls `limits.Configure(cfg.EffectiveRequestLimits())` and prunes deleted account IDs.

- [x] **Step 1: Write failing integration tests**

Add to `clients/openaicompat/source_limits_test.go`:

- `TestSourceDailyAccountLimitRotatesAndReportsBusy`
  - Two accounts, each with an account rule of `1 / 24h rolling`.
  - First request uses key A; second request uses key B; third request returns `core.ErrBusy`.
  - Assert both upstream counters are 1.
- `TestSourceSourceLimitReturnsBusy`
  - Two accounts, source rule `1 / 24h rolling`.
  - First request succeeds; second request returns `core.ErrBusy` even though the second key is free.
- `TestSourceRequestLimitStatusShowsRecovery`
  - Reach the account limit.
  - `Status()` returns the account with `State == "cooling"`, `Note == "request limit reached"`, a non-empty `ExpiresAt`, and a `model_cooldowns` entry with `kind == "request_limit"`.
- `TestSourceRequestLimitPersistsAcrossReload`
  - Use one account and an account rule of `1 / 24h rolling`.
  - First source instance succeeds; a second `newSource` on the same `DataDir` is blocked before the upstream call.

Keep the existing `TestSourceRPM...` tests green to pin legacy behavior.

- [x] **Step 2: Run the tests and confirm they fail**

Run:

```text
go test ./clients/openaicompat -run 'TestSource(DailyAccountLimitRotatesAndReportsBusy|SourceLimitReturnsBusy|RequestLimitStatusShowsRecovery|RequestLimitPersistsAcrossReload|RPM)'
```

Expected: FAIL because `source` still uses the old in-memory RPM fields.

- [x] **Step 3: Replace the old RPM path with the limiter**

In `clients/openaicompat/source.go`:

- Remove `rpmWindow`, `rpmUsed`, `accountRPM`, and `sourceRateWindow`.
- Build the request body before acquiring an account slot so local body errors do not consume quota.
- Check `limits.SourceBlocked(now)` before selecting candidates.
- Skip candidate accounts whose `limits.AccountBlocked(id, now)` is non-nil.
- After `AcquireAccountSlot`, call `limits.Acquire(id, now)`.
  - On error: release the slot and return the error.
  - On account block: release the slot and continue with the next candidate.
  - On source block: release the slot and stop the loop.
- Keep `core.ErrBusy` for exhausted limits and include the recovery time in the error text.
- In `Status`, add the account request-limit block as:
  - `a.State = "cooling"`
  - `a.Note = "request limit reached"`
  - `a.ExpiresAt = block.Until.UTC().Format(time.RFC3339)`
  - a `model_cooldowns` entry for each supported model with `kind: "request_limit"` and the same `until`.
- Keep the existing upstream cooldown and invalid-key behavior unchanged.

- [x] **Step 4: Run the tests and confirm they pass**

Run:

```text
go test ./clients/openaicompat
```

Expected: PASS.

---

### Task 4: 面板规则表单、旧字段迁移和状态展示

**Files:**
- Modify: `internal/panel/index.html`
- Modify: `internal/panel/sources_test.go`
- Modify: `internal/panel/account_status_chinese_ui_test.go`

**Interfaces:**
- HTML gains `#sourceRequestLimits`, `#sourceRequestLimitsEmpty`, and `#btnAddSourceLimit`.
- JS gains pure helpers:
  - `sourceLimitConfig(cfg) []`
  - `sourceLimitWindowParts(window) { every, unit }`
  - `sourceLimitWindow(every, unit) string`
  - `addSourceLimitRow(rule)`
  - `renderSourceLimits(rules)`
  - `readSourceLimits() []`
- `openSource(id)` renders `sourceLimitConfig(SOURCE_DEFS[id] || {})`.
- `saveSource` writes `cfg.request_limits = readSourceLimits()` and does not write the legacy RPM fields.

- [x] **Step 1: Write failing panel tests**

In `internal/panel/sources_test.go`:

- Replace the old RPM editor assertions with checks for:
  - `id="sourceRequestLimits"`
  - `id="btnAddSourceLimit"`
  - `function sourceLimitConfig(cfg)`
  - `function readSourceLimits()`
  - `sourceLimitConfig(cfg)` call in `openSource`
  - `cfg.request_limits = readSourceLimits()` in `saveSource`
  - no `sourceRPM` / `sourceAccountRPM` input IDs
  - no `textarea` or JSON editor inside the relay form
- Add `TestSourceHandlerPersistsRequestLimits`:
  - PUT a source with two `request_limits` entries.
  - Assert both survive in `configs/client2api.json`.
- Add `TestSourceLimitsMigrateLegacyRPM` using the existing Node helper pattern:
  - Extract `sourceLimitConfig` from the inline script.
  - Feed `{requests_per_minute:120, requests_per_minute_per_account:30}`.
  - Assert two rules with `window:"1m"`, `reset:"first"`.
- Add `TestSourceLimitsEditorPreservesExplicitZeroInFlight`:
  - Save `max_in_flight:0` with a request rule.
  - Assert the persisted config still has numeric zero.

In `internal/panel/account_status_chinese_ui_test.go`:

- Add `"request limit reached": "请求限额已满"` to the expected translation table.

- [x] **Step 2: Run the tests and confirm they fail**

Run:

```text
go test ./internal/panel -run 'TestSource(Limits|HandlerPersistsRequestLimits)|TestAccountStatusNoteTranslationCoversKnownModulePhrases'
```

Expected: FAIL because the new controls and translation do not exist.

- [x] **Step 3: Implement the form and serialization**

In `internal/panel/index.html`:

- Replace the two RPM number inputs with a “请求次数限额” fieldset.
- Render each rule as a card with:
  - scope select: `account` / `source`
  - count number input
  - every number input
  - unit select: `s` / `m` / `h` / `d`
  - reset select: `rolling` / `calendar` / `first`
  - `reset_at` time input, visible only for calendar
  - timezone text input, visible only for calendar
  - remove button
- Add “添加规则” button and an empty-state line.
- Default new rule: account, 1 day, 100 times, rolling.
- `sourceLimitConfig`:
  - use `cfg.request_limits` when non-empty
  - otherwise synthesize the two legacy minute rules with `reset:"first"`
  - otherwise return `[]`
- `sourceLimitWindow`:
  - seconds -> `"<n>s"`
  - minutes -> `"<n>m"`
  - hours -> `"<n>h"`
  - days -> `"<n*24>h"`
- `readSourceLimits`:
  - require integer `limit >= 1`
  - require integer `every >= 1`
  - require valid scope/unit/reset
  - default calendar timezone to `Asia/Shanghai`
  - default calendar reset_at to `00:00`
  - throw a visible dialog error on invalid input
- Add an input/change listener so changing reset to/from calendar toggles the calendar-only fields.
- Add `$("#btnAddSourceLimit").addEventListener("click", () => addSourceLimitRow())`.
- Keep the rest of the relay form, including explicit `max_in_flight: 0`, unchanged.
- Add the Chinese translation for `"request limit reached"`.

- [x] **Step 4: Run the tests and confirm they pass**

Run:

```text
go test ./internal/panel -run 'TestSource(Limits|HandlerPersistsRequestLimits)|TestAccountStatusNoteTranslationCoversKnownModulePhrases|TestPanelInlineScriptsAreSyntacticallyValid'
```

Expected: PASS.

---

### Task 5: 文档和全量验证

**Files:**
- Modify: `README.md`
- Maybe modify: `docs/REFERENCE.zh-CN.md` if the relay limit section has a matching reference entry.

**Interfaces:**
- No new interfaces.

- [x] **Step 1: Update the relay documentation**

Replace the current `requests_per_minute` description with:

- `request_limits` is the new form-driven rule list.
- Each rule has scope, limit, window, reset mode, timezone, and reset_at.
- `rolling`, `calendar`, and `first` semantics.
- Per-account rules rotate keys; source rules return busy.
- Counters persist in `data/<source>/request_limits.json`.
- Legacy `requests_per_minute*` fields are accepted and migrated by the panel.

- [x] **Step 2: Format Go files**

Run:

```text
gofmt -w internal/core/sources.go internal/core/source_limits_test.go clients/openaicompat/request_limits.go clients/openaicompat/request_limits_test.go clients/openaicompat/source.go clients/openaicompat/source_limits_test.go internal/panel/sources_test.go internal/panel/account_status_chinese_ui_test.go
```

Expected: no output.

- [x] **Step 3: Run the full build and test suite**

Run:

```text
go build ./...
go test ./...
```

Expected: both commands exit 0.

- [x] **Step 4: Run the targeted panel syntax check**

Run:

```text
go test ./internal/panel -run TestPanelInlineScriptsAreSyntacticallyValid
```

Expected: PASS.

- [x] **Step 5: Review the final diff**

Run:

```text
git diff --stat
git diff --check
```

Expected: no whitespace errors; the diff contains only the planned source, tests, docs, and design/plan files.

---

## Self-Review

- Spec coverage: core schema, legacy compatibility, three reset modes, multi-rule enforcement, per-key rotation, source-wide busy, persistence, fail-closed writes, panel form, no JSON input, status visibility, docs, and tests all have a task.
- Type consistency: `SourceRequestLimit` and `Signature()` are defined in Task 1 and used by Task 2; `requestLimitBlock` is defined in Task 2 and used by Task 3; panel helper names are defined in Task 4 and tested in Task 4.
- No open TBDs: calendar windows are restricted to divisors of 24 hours; weekly/monthly calendar rules remain out of scope and use rolling/first.