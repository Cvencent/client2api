# Model Routing Groups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add per-model-group platform priorities and operator-configurable equivalent model names.

**Architecture:** Extend the core registry with `ModelGroup` objects and a group-aware candidate resolver. Project a new top-level `model_groups` config section into the registry, validate it in the panel writer, advertise it in `/v1/models`, and edit it in the Platforms page.

**Tech Stack:** Go, standard `net/http`, embedded HTML/JavaScript panel.

**Spec:** `docs/superpowers/specs/2026-10-10-model-routing-groups-design.md`

## Global Constraints

- Explicit `client/model` requests lock the platform and never expand through a group.
- Lower platform priority wins; group priority overrides global priority, then `priority_schedule` is only the global fallback.
- Existing `aliases`, `platforms`, `Auto/`, failover, health, and sticky-session behavior remain compatible.
- `model_groups` is hot-reloadable; no process restart is required.
- Missing catalogue members are skipped at runtime.

## Review Focus

- A group name that is also an alias must be rejected rather than resolved nondeterministically.
- A missing group member must not make the whole group unusable when another member is healthy.
- Removing one platform priority must delete that key rather than leaving a stale merge value.
- Case-only duplicate group names or members must be rejected.
- Group routing must still respect disabled models and platform health.

---

### Task 1: Core model-group routing

**Files:**
- Create: `internal/core/model_groups.go`
- Create: `internal/core/model_groups_test.go`
- Modify: `internal/core/registry.go:90-190`, `internal/core/registry.go:500-690`

**Interfaces:**
- Produces: `type ModelGroup struct { Members []ModelGroupMember; PlatformPriorities map[string]int }`
- Produces: `type ModelGroupMember struct { Client, Model string }`
- Produces: `func (r *Registry) SetModelGroups(groups map[string]ModelGroup)`
- Produces: `func (r *Registry) ModelGroups() map[string]ModelGroup`
- Produces: group resolution through `func (r *Registry) ResolveCandidates(ctx context.Context, model string) ([]Candidate, error)`

- [ ] **Step 1: Write failing tests** for group-name routing, member-name routing, explicit-platform locking, group priority override/fallback, and missing members in `internal/core/model_groups_test.go`.
- [ ] **Step 2: Run the focused test** with `go test ./internal/core -run TestModelGroup -v`; expect failures because the type/methods do not exist.
- [ ] **Step 3: Implement** `ModelGroup`, normalised lookup tables on `Registry`, and group candidate resolution.
- [ ] **Step 4: Run the focused test**; expect PASS.
- [ ] **Step 5: Run `go test ./internal/core`**; expect PASS.

### Task 2: Config parsing and hot reload

**Files:**
- Modify: `cmd/client2api/main.go:59-190`, `cmd/client2api/main.go:1200-1235`, `cmd/client2api/main.go:1515-1555`
- Test: `cmd/client2api/model_groups_test.go`

**Interfaces:**
- Consumes: `core.ModelGroup` and `core.ModelGroupMember` from Task 1.
- Produces: `fileConfig.ModelGroups map[string]modelGroupConfig`
- Produces: `func (c *fileConfig) modelGroups() map[string]core.ModelGroup`

- [ ] **Step 1: Write failing projection and JSON round-trip tests** in `cmd/client2api/model_groups_test.go`.
- [ ] **Step 2: Run `go test ./cmd/client2api -run ModelGroup -v`**; expect compile or assertion failure.
- [ ] **Step 3: Add the config type, projection, startup installation, and reload installation.**
- [ ] **Step 4: Run the focused test**; expect PASS.
- [ ] **Step 5: Run `go test ./cmd/client2api`**; expect PASS.

### Task 3: Panel config validation and write order

**Files:**
- Modify: `internal/panel/configwrite.go:38-42`, `internal/panel/configwrite.go:287-430`
- Test: `internal/panel/model_groups_config_test.go`

**Interfaces:**
- Consumes: the `model_groups` JSON shape from the spec.
- Produces: validation errors used by `PATCH /panel/api/config`.

- [ ] **Step 1: Write failing validation tests** for valid groups, slash/empty names, malformed members, duplicate members, alias collisions, unknown priority platforms, and non-integer priorities.
- [ ] **Step 2: Run `go test ./internal/panel -run ModelGroup -v`**; expect failure.
- [ ] **Step 3: Implement validation and add `model_groups` to `configKeyOrder`.**
- [ ] **Step 4: Run the focused test**; expect PASS.
- [ ] **Step 5: Run `go test ./internal/panel`**; expect PASS.

### Task 4: Gateway catalogue and routing integration

**Files:**
- Modify: `internal/gateway/server.go:270-390`
- Test: `internal/gateway/model_groups_test.go`

**Interfaces:**
- Consumes: `Registry.ModelGroups()` from Task 1 and routing from Task 1.
- Produces: virtual `/v1/models` entries with `owned_by: "group"`.

- [ ] **Step 1: Write failing tests** that `/v1/models` lists the group and a `POST /v1/chat/completions` using the group name reaches the highest-priority member.
- [ ] **Step 2: Run `go test ./internal/gateway -run ModelGroup -v`**; expect failure.
- [ ] **Step 3: Implement group catalogue entries and preserve existing entries.**
- [ ] **Step 4: Run the focused test**; expect PASS.
- [ ] **Step 5: Run `go test ./internal/gateway`**; expect PASS.

### Task 5: Platforms-page editor

**Files:**
- Modify: `internal/panel/index.html:1080-1100` (styles), `internal/panel/index.html:5580-6040` (render/save)
- Test: `internal/panel/model_groups_ui_test.go`

**Interfaces:**
- Consumes: `model_groups` config and `/panel/api/models` catalogue.
- Produces: add/remove group and member controls saved through `PATCH /panel/api/config`.

- [ ] **Step 1: Write failing UI contract tests** for the group section, member editor, priority inputs, add/remove buttons, and `patch.model_groups`.
- [ ] **Step 2: Run `go test ./internal/panel -run ModelGroupUI -v`**; expect failure.
- [ ] **Step 3: Implement the group editor and save merge semantics.**
- [ ] **Step 4: Run the focused test**; expect PASS.
- [ ] **Step 5: Run `go test ./internal/panel`**; expect PASS.

### Task 6: Documentation and full verification

**Files:**
- Modify: `README.md`, `README.zh-CN.md`, `configs/client2api.example.json`
- Test: existing Go suite.

**Interfaces:**
- Produces: operator documentation and an example config.

- [ ] **Step 1: Update both READMEs and the example config.**
- [ ] **Step 2: Run `gofmt` on edited Go files.**
- [ ] **Step 3: Run `go build ./...`; expect exit 0.**
- [ ] **Step 4: Run `go test ./...`; expect exit 0.**
- [ ] **Step 5: Review the full diff against the spec and record any deferred minors.**
