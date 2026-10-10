# Dynamic Relay Platforms Implementation Plan

> Execute inline using the existing Go module, registry and panel patterns.

**Goal:** Hot-defined OpenAI-compatible platforms with independent key pools.

**Architecture:** Core owns the source schema, validation and runtime lifecycle.
The OpenAI-compatible module registers the source factory and reuses its
existing body/SSE transport. Panel controls write `sources` and invoke reload.

**Tech Stack:** Go, standard net/http, existing embedded HTML/JavaScript panel.

**Spec:** ../specs/2026-10-10-dynamic-relay-platforms-design.md

## Constraints

Preserve `openai-compat` configuration/routes and compiled module isolation.
Do not run or restart the deployed service. No installer or release changes.
Keys live only in private source account stores; panel lists never expose them.

## Tasks

1. Core lifecycle (`internal/core/sources.go`, `sources_test.go`): write failing
   tests for unsafe IDs/URLs, compiled collisions, add/update/remove and retaining
   an unchanged instance. Implement `SourceConfig`, `ValidateSources`,
   `RegisterSourceFactory`, `Registry.ReconcileSources` and source update contract.
2. Source pool (`clients/openaicompat/source*.go`): failing httptest cases for
   authenticated scan, persistence, slash IDs, last-good cache and multi-key
   selection. Implement AccountManager and ModelRefresher using isolated stores,
   shared generic transport, admission slots, priorities, rotation/cooldowns.
3. Main and panel config (`cmd/client2api/main.go`, `internal/panel/configwrite.go`):
   tests for parsing/validation and hot-only source edits. Reconcile at startup
   and reload using the initial process data directory and HTTP client.
4. Panel (`internal/panel/index.html`, source API tests): add source CRUD and
   scoped scan controls, display-label lookup and source-aware config rendering.
   Verify a real browser flow and safe escaped labels.
5. Documentation and review: update README, run gofmt, full Go tests/build/vet,
   targeted race and isolated panel browser checks. Review bundle restore and
   concurrent scan/edit behavior before reporting completion.

## Completed Verification (2026-10-10)

- [x] Core validation and hot source lifecycle.
- [x] Isolated API-key stores, concurrent scans, per-key permissions and priority.
- [x] Authenticated unified gateway, qualified/bare/group routing and bundle restore.
- [x] Dynamic panel names, source CRUD, account-pool navigation and scanning.
- [x] Draft priority/model edits survive scans; absent model catalogues retain blacklists.
- [x] Retired and pre-restore scans cannot overwrite newer account stores.
- [x] Catalogue scans preserve chat cooldowns; old probes cannot clear replacement key cooldowns.
- [x] Read-only code review findings resolved with regression tests.
- [x] `go test ./...`, `go build ./...` and `go vet ./...` passed.
- [x] Targeted `go test -race` passed for core, compat, panel, main and gateway.
- [x] Isolated Playwright flow passed at 1440x1000 and 390x844, including long
  labels, form validation, model scanning, source routing and configuration separation.

Browser screenshots are in `output/playwright/dynamic-source-dialog.png`,
`dynamic-sources-desktop.png` and `dynamic-sources-mobile.png`. The final browser
reported no console errors or warnings. Only the owned temporary gateway and
mock upstream were launched; both were stopped after validation.

The deployed service, config and account data were untouched. The feature is
included in the v0.1.32 release, with a credential-free Windows upgrade package.
