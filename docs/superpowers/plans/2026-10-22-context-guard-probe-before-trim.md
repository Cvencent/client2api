# Context Guard Probe-Before-Trim Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the vendor decide first: a request the gateway's estimator only *thinks* is
too big is forwarded whole once, recorded as a sticky per-model probe, and only trimmed on
the vendor's own refusal.

**Architecture:** Add a persistent `probeStore` keyed by (client, model) holding the probe
outcome and the largest estimate the vendor has accepted. `prepareCandidateAttempt` consults
it through a new `probeDecision` that either forwards whole (probe) or keeps the existing
trim path. `openStreamRecorded` already handles vendor refusal (learn window + trim +
one retry); it gains the "record accepted" write and marks `refused` probes. The panel
models table gains a probe column plus a clear action.

**Tech Stack:** Go (standard library only), existing `net/http` gateway, embedded
HTML/JS panel, `go test ./...`.

**Spec:** ../specs/2026-10-22-context-guard-probe-before-trim-design.md

## Global Constraints

- Do not run or restart the deployed instance at `D:\client2api`; the operator upgrades on their own schedule.
- Change source only under `D:\client2api-src`. Verify with `go build ./...` and `go test ./...`.
- Do not touch the unrelated uncommitted panel/router work already in the tree (`internal/panel/route_speed.go`, `stickysessions*.go`, `panel.go`, `index.html`, `README.md`, `route_plan_ui_test.go`).
- New setting `features.probe_before_trim`, default **true**. Config file `configs\client2api.json`; panel: settings page, features group.
- Probe state is sticky per (client, model): once probed, never probed again until the operator clears it.
- `probeFloor` relaxes the trim budget only. It must never raise the advertised context window.
- Manual override (`ModelOverrides`) still outranks everything; `probeFloor` must not exceed it.
- Storage read failures collapse to an empty store; a corrupt cache must never fail a request.

## Review Focus

- A corrupt or unreadable `observed_context_windows.json` must not fail or crash a request; the guard keeps working on the published window.
- `probeFloor` must not raise the budget above a manual override, and must not change `/v1/models`.
- Probe must not fire when the window is unknown (`promptBudget <= 0`) - that path already forwards untrimmed and must not be double-counted.
- A probe that is refused must still end in the existing single retry, and must not retry a second time.
- Clearing a probe in the panel must not delete the learned window or the manual override.

---
### Task 1: Probe store (persistence)

**Files:**
- Create: `internal/gateway/context_probe.go`
- Test: `internal/gateway/context_probe_test.go`

**Interfaces:**
- Consumes: `core.ReadJSON`, `core.WriteJSONAtomic` (same helpers `learnedWindowStore` uses).
- Produces:
  - `type probeRecord struct { Probed bool; Outcome string; EstimateTokens int64; VendorTokens int64; At time.Time }`
  - `type probeStore struct` with `get(client, model string) (probeRecord, bool)`, `recordAccepted(client, model string, estimate, vendor int64) bool`, `recordRefused(client, model string, estimate int64) bool`, `clear(client, model string) bool`, `snapshot() map[string]map[string]probeRecord`
  - `func openProbeStore(path string, warnf func(string, ...any)) *probeStore`
  - `const probeOutcomeAccepted = "accepted"`, `const probeOutcomeRefused = "refused"`

- [ ] **Step 1: Write the failing test**

Tests in `internal/gateway/context_probe_test.go`:

- `TestProbeStoreRecordsAcceptedAndRaisesTheFloor`: record accepted estimate 1_043_449 / vendor 961_759; `get` reports ok, `Outcome == probeOutcomeAccepted`, `EstimateTokens == 1_043_449`; a later accepted record at 900_000 leaves the floor at 1_043_449; a later accepted record at 1_200_000 raises it to 1_200_000.
- `TestProbeStoreRefusedHasNoFloor`: record refused; `get` ok, `Outcome == probeOutcomeRefused`, `EstimateTokens == 0`.
- `TestProbeStoreClearRemovesOnlyThatEntry`: record two models, clear one, the other survivor still reports ok.
- `TestProbeStoreSurvivesReload`: write, reopen from the same path, `get` returns the same record.
- `TestProbeStoreCorruptFileCollapsesToEmpty`: write `not json` to the path, `openProbeStore` returns a store reporting `ok == false` that still accepts a new record.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/gateway -run TestProbeStore -v`
Expected: FAIL - `undefined: openProbeStore`.

- [ ] **Step 3: Implement the store**

Mirror `learnedWindowStore` in `context_guard.go`: `mu sync.RWMutex`, `path`, `warnf`, `entries map[probeKey]probeRecord`. `recordAccepted` sets `Probed`, `Outcome`, `VendorTokens`, `At: time.Now()`, and `EstimateTokens = max(existing.EstimateTokens, estimate)`; returns whether anything changed. `recordRefused` sets `Probed`, `Outcome: probeOutcomeRefused`, `EstimateTokens: 0`, `At`. Mutators persist with `WriteJSONAtomic`. `clear` deletes and persists. Unknown versions and unreadable files are ignored with a warning.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/gateway -run TestProbeStore -v`
Expected: PASS 5/5.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/context_probe.go internal/gateway/context_probe_test.go
git commit -m "feat(gateway): persistent context probe store"
```

---

### Task 2: probeDecision

**Files:**
- Modify: `internal/gateway/context_guard.go` (near `promptBudget`, ~line 83)
- Test: `internal/gateway/context_probe_test.go`

**Interfaces:**
- Consumes: `probeStore.get` (Task 1), `promptBudget`, `estimateRequestTokens`.
- Produces:
  - `type probeVerdict int` with `probeProceed`, `probeForwardWhole`, `probeTrimExceeded`
  - `func (s *server) probeDecision(ctx context.Context, req *core.ChatRequest, client core.Client, model string, maxOutput int, limit int64) probeVerdict`

- [ ] **Step 1: Write the failing test**

- `TestProbeDecisionForwardsWholeWhenNeverProbed`: 1_000_000 window, request estimating ~1_050_000, no probe record -> `probeForwardWhole`.
- `TestProbeDecisionTrimsWhenNothingFits`: newest turn alone exceeds the budget -> `probeTrimExceeded`.
- `TestProbeDecisionUsesAcceptedFloor`: accepted floor 1_043_449, request of that size -> `probeProceed`.
- `TestProbeDecisionAboveFloorStillTrims`: accepted floor 1_043_449, request now ~1_300_000 -> `probeTrimExceeded`.
- `TestProbeDecisionAlreadyRefusedDoesNotProbe`: refused record -> `probeProceed`.
- `TestProbeDecisionUnknownWindowNeverProbes`: no window -> `probeProceed`.
- `TestProbeDecisionHasNoHardCeiling`: window 1_000_000, estimate 5_000_000 -> `probeForwardWhole` (there is deliberately no "too far over" cutoff).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/gateway -run TestProbeDecision -v`
Expected: FAIL - `undefined: probeForwardWhole`.

- [ ] **Step 3: Implement probeDecision**

Order: unknown window -> `probeProceed`; `estimate <= budget` -> `probeProceed`; existing probe -> accepted floor (`estimate <= floor` proceeds, else trims) or refused -> `probeProceed`; otherwise `probeForwardWhole`. There is no hard ceiling: the estimator is the thing under test, so any size it rejects still goes to the vendor once.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/gateway -run TestProbeDecision -v`
Expected: PASS 7/7.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/context_guard.go internal/gateway/context_probe_test.go
git commit -m "feat(gateway): probe-before-trim decision"
```

---
### Task 3: Wire the decision into the request path

**Files:**
- Modify: `internal/gateway/context_guard.go` (`guardRequestContext`, ~line 1015)
- Modify: `internal/gateway/server.go` (`Options` ~line 74, `NewServer` ~line 185)
- Modify: `internal/gateway/usage.go` (`UsageRecord`, ~line 173)
- Test: `internal/gateway/context_probe_path_test.go`

**Interfaces:**
- Consumes: `probeDecision` (Task 2), `probeStore` (Task 1).
- Produces:
  - `Options.ProbeStore *probeStore`, `Options.ProbeBeforeTrim bool`
  - `UsageRecord.Probed bool`, `UsageRecord.ProbedFromTokens int64`
  - `guardRequestContext` leaves the request untouched on `probeForwardWhole`, recording `rec.Probed` and `rec.ProbedFromTokens`.

- [ ] **Step 1: Write the failing test**

- `TestProbeForwardsWholeAndRecordsTheRow`: fake vendor accepting anything; request above the estimator budget; assert the vendor received every message, the usage row has `Probed == true` and `ProbedFromTokens` equal to the pre-trim estimate, and no compaction item was emitted.
- `TestProbeRefusalStillTrimsAndRetriesOnce`: fake vendor refusing the first call with `prompt is too long: 1100000 tokens > 1000000 maximum` then accepting; assert two vendor calls, the second had fewer messages, row `Trimmed == true` and `Probed == true`.
- `TestProbeDisabledKeepsTrimming`: `ProbeBeforeTrim: false`, same oversized request -> the vendor sees the trimmed count on the first call and the row has `Probed == false`.
- `TestProbeNotRepeatedAfterAcceptance`: first request probes and is accepted, then a second request whose estimate grew but stays under the recorded floor -> vendor still receives every message and the row is not marked probed again.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/gateway -run 'TestProbe(Forwards|Refusal|Disabled|NotRepeated)' -v`
Expected: FAIL - unknown field `Probed`, or the vendor saw a trimmed count.

- [ ] **Step 3: Implement the wiring**

In `guardRequestContext`, before `applyContextBudget`, when `s.opts.ProbeBeforeTrim`: compute `estimate := estimateRequestTokens(req)`, call `probeDecision`; on `probeForwardWhole` set `rec.Probed = true`, `rec.ProbedFromTokens = int64(estimate)`, and return `trimReport{}, false` without touching `req.Messages`. Anything else falls through to today's `applyContextBudget`. Evaluate the probe against the pristine history so failover siblings are judged independently.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/gateway -run TestProbe -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/context_guard.go internal/gateway/server.go internal/gateway/usage.go internal/gateway/context_probe_path_test.go
git commit -m "feat(gateway): forward whole once, trim only on refusal"
```

---
### Task 4: Record the probe outcome on success and refusal

**Files:**
- Modify: `internal/gateway/server.go` (`openStreamRecorded`, ~line 1235)
- Modify: `internal/gateway/usage.go` (where `rec.PromptTokens` is set, ~line 200)
- Test: `internal/gateway/context_probe_outcome_test.go`

**Interfaces:**
- Consumes: `probeStore.recordAccepted` / `recordRefused` (Task 1), `rec.Probed` (Task 3).
- Produces: a successful probed turn records accepted; a `FailureContextWindow` on a probed turn records refused before the existing learn+retry.

- [ ] **Step 1: Write the failing test**

- `TestAcceptedProbePersistsTheFloor`: probe accepted -> reopen the store from disk; record exists with `Outcome == accepted` and the logged estimate.
- `TestRefusedProbePersistsTheOutcome`: probe refused with numeric sizes -> store `Outcome == refused`, and the learned window file is still written.
- `TestRefusedProbeWithoutNumbersStillRetriesOnce`: refusal without numeric sizes -> exactly two vendor calls, `Outcome == refused`.
- `TestSuccessAfterTrimIsNotAProbeOutcome`: a trimmed non-probed successful turn creates no probe record.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/gateway -run TestAcceptedProbe -v`
Expected: FAIL - no probe record written.

- [ ] **Step 3: Implement the write-back**

Read `usage.go:195-240` first: `rec.PromptTokens` is populated from the usage frame, so the accepted write belongs where the row is finalized, not at stream open. Refusal: inside the existing `if f.Kind == core.FailureContextWindow` block, before `learnContextWindowFromFailure`, when `rec != nil && rec.Probed` call `recordRefused(ctx, client.Name(), req.Model, rec.ProbedFromTokens)`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/gateway -run TestProbe -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/server.go internal/gateway/usage.go internal/gateway/context_probe_outcome_test.go
git commit -m "feat(gateway): persist probe outcomes"
```

---

### Task 5: Config switch (default on)

**Files:**
- Modify: `cmd/client2api/main.go` (`featuresConfig` ~line 577, `liveSnapshot` ~line 1066, gateway options ~line 1605)
- Modify: `internal/livecfg/livecfg.go` (`Snapshot`, ~line 26)
- Test: `cmd/client2api/config_probe_test.go`

**Interfaces:**
- Consumes: `Options.ProbeBeforeTrim` and `Options.ProbeStore` (Task 3).
- Produces: `featuresConfig.ProbeBeforeTrim *bool`, `livecfg.Snapshot.ProbeBeforeTrim bool`.

- [ ] **Step 1: Write the failing test**

- `TestProbeBeforeTrimDefaultsOn`: parsing `{}` yields `liveSnapshot().ProbeBeforeTrim == true`.
- `TestProbeBeforeTrimCanBeDisabled`: `{"features":{"probe_before_trim":false}}` yields false.
- `TestProbeBeforeTrimExplicitOn`: explicit true yields true.
- `TestProbeStorePathFollowsDataDir`: `data_dir` set -> probe store path is `<data_dir>/observed_context_windows.json`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cmd/client2api -run TestProbeBeforeTrim -v`
Expected: FAIL - `liveSnapshot().ProbeBeforeTrim` undefined.

- [ ] **Step 3: Implement the switch**

`featuresConfig.ProbeBeforeTrim *bool`, projected with `boolOr(c.Features.ProbeBeforeTrim, true)` like `SanitizeBlacklistFingerprints`. Add the field to `livecfg.Snapshot` so a reload moves it. In `main.go` also set `ProbeStore` to `openProbeStore(filepath.Join(cfg.DataDir, "observed_context_windows.json"), logger.Printf)` and `ProbeBeforeTrim` from the live snapshot.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./cmd/client2api -run TestProbeBeforeTrim -v`
Expected: PASS 4/4.

- [ ] **Step 5: Commit**

```bash
git add cmd/client2api/main.go internal/livecfg/livecfg.go cmd/client2api/config_probe_test.go
git commit -m "feat(config): features.probe_before_trim, default on"
```

---
### Task 6: Panel probe column and clear action

**Files:**
- Modify: `internal/panel/models.go` (`modelInfo`, `modelsFor`)
- Modify: `internal/panel/panel.go` (route table, ~line 241)
- Create: `internal/panel/modelprobe.go`
- Modify: `internal/panel/index.html` (models table row)
- Test: `internal/panel/modelprobe_test.go`

**Interfaces:**
- Consumes: `probeStore.snapshot` / `clear` (Task 1), `Options.ProbeStore`.
- Produces: `modelInfo.ProbeState *modelProbeView`, `POST /panel/api/model_probe/clear` with body `{"client":"...","model":"..."}`.

- [ ] **Step 1: Write the failing test**

- `TestModelsListCarriesProbeState`: store holds an accepted record; `GET /panel/api/models?client=raccoon` returns that model with `probe_state.probed == true`.
- `TestModelsListProbeStateAbsentWhenUnprobed`: no record -> `probe_state` omitted.
- `TestModelProbeClearRemovesOnlyThatModel`: two records, clear one, the other still probed.
- `TestModelProbeClearRejectsUnknownMethodAndBadBody`: `GET` -> 405; missing `client` -> 400.
- `TestModelProbeClearLeavesLearnedWindowAndOverride`: learned window and manual override survive the clear.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/panel -run 'TestModelsList.*Probe|TestModelProbe' -v`
Expected: FAIL - `probe_state` absent, route 404.

- [ ] **Step 3: Implement the panel surface**

`modelprobe.go` holds the handler (POST only, decode, call `clear`, 200) and `probeViewFor`. `modelsFor` attaches `ProbeState` per model. Register `api("/panel/api/model_probe/clear", p.handleModelProbeClear)`. In `index.html`, render a badge per row - `未探测` / `已探测 · 接受 <estimate>` / `已探测 · 拒绝` - plus a `清除` button POSTing the clear route, using the existing escaped-render helpers.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/panel -run 'TestModelsList.*Probe|TestModelProbe' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/panel/models.go internal/panel/panel.go internal/panel/modelprobe.go internal/panel/index.html internal/panel/modelprobe_test.go
git commit -m "feat(panel): show and clear context probe state"
```

---
### Task 7: Keep Responses compaction consistent

**Files:**
- Modify: `internal/gateway/responses.go` (`responsesWouldTrim`, ~line 1126)
- Test: `internal/gateway/responses_probe_test.go`

**Interfaces:**
- Consumes: `probeDecision` (Task 2).
- Produces: `responsesWouldTrim` returns true only when `probeDecision` is `probeTrimExceeded`.

- [ ] **Step 1: Write the failing test**

- `TestResponsesDoesNotCompactWhenProbeWillForwardWhole`: oversized Responses request, probe on, no record -> no `compaction` item, upstream received everything.
- `TestResponsesStillCompactsWhenTrimIsCertain`: recorded accepted floor exceeded, or over the hard ceiling -> compaction item present.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/gateway -run TestResponsesDoesNotCompactWhenProbe -v`
Expected: FAIL - compaction item appears on the probe path.

- [ ] **Step 3: Implement the consistency**

`responsesWouldTrim` resolves the first candidate as today, then returns `s.probeDecision(...) == probeTrimExceeded`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/gateway -run TestResponses -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gateway/responses.go internal/gateway/responses_probe_test.go
git commit -m "fix(gateway): probe verdict drives Responses compaction"
```

---

### Task 8: Full verification and docs

**Files:**
- Modify: `README.md` (context-guard section only)
- Modify: `docs/REFERENCE.zh-CN.md`
- Modify: `CHANGELOG.md`

- [ ] **Step 1: Run the full suite**

Run: `go build ./... && go test ./...`
Expected: build clean, all tests pass.

- [ ] **Step 2: Update docs**

- `README.md`: `features.probe_before_trim` (default true), the probe badge, the clear action.
- `docs/REFERENCE.zh-CN.md`: the guard decision order, and `probeFloor` (budget) versus the advertised window.
- `CHANGELOG.md`: one entry for probe-before-trim.

- [ ] **Step 3: Verify formatting**

Run: `gofmt -l .` (expect no output); `go vet ./...` (expect clean).

- [ ] **Step 4: Commit**

```bash
git add README.md docs/REFERENCE.zh-CN.md CHANGELOG.md
git commit -m "docs: probe-before-trim"
```
