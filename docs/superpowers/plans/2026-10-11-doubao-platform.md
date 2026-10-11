# Doubao Platform Module Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `doubao` client module that serves the mainland Doubao web app over the gateway's OpenAI-compatible API, with a controlled-browser web login.

**Architecture:** A new self-contained module under `clients/doubao` owns the cookie account pool, the `/samantha/chat/completion` SSE transport and its own optional capabilities. The only shared change is one additive method on the existing CDP driver so the login can read HttpOnly cookies.

**Tech Stack:** Go 1.27, `internal/browser` (CDP), `internal/core` contracts, `net/http`, `bufio` SSE parsing.

**Spec:** `docs/superpowers/specs/2026-10-11-doubao-platform-design.md`

## Global Constraints

- Edit only `D:\client2api-src`; never touch the running service at `D:\client2api`.
- No `clients/*` package may import another `clients/*` package; only `internal/core` and `internal/browser` are allowed.
- All state lives under `Deps.DataDir` and is written with `core.WriteJSONAtomic`/`core.WriteFileAtomic`.
- Credentials never appear in logs, `Status`, panel responses, or errors; human-facing text passes through `core.Redact`.
- No new third-party dependency; `GOPROXY=off`, `CGO_ENABLED=0`.
- Build env before every command:
  `$tools='D:\client2api-lab\_tools'; $env:PATH="$tools\go\bin;$env:PATH"; $env:GOCACHE="$tools\gocache"; $env:GOTMPDIR="$tools\gotmp"; $env:GOMODCACHE="$tools\gomodcache"; $env:GOFLAGS='-mod=mod'; $env:GOPROXY='off'; $env:CGO_ENABLED='0'`

## Review Focus

- A cookie jar that is present but expired must read as invalid, not as "no account".
- A chat request with no account must return `core.ErrNotConfigured`, never a panic.
- SSE frames without a space after `data:` and a final frame without a blank line must still parse.
- Browser launch failure must leave the login session `pending` with a usable message, not crash.
- `Status` and `Health` must never leak cookie material.

---

### Task 1: CDP cookie read

**Files:**
- Modify: `internal/browser/cdp.go`
- Test: `internal/browser/cookies_test.go`

**Interfaces:**
- Produces: `type Cookie struct { Name, Value, Domain, Path string; Expires float64; HTTPOnly, Secure bool }`
- Produces: `func (p *Page) Cookies(ctx context.Context, urls []string) ([]Cookie, error)`

- [ ] **Step 1: Write the failing test** — a fake CDP websocket server that answers `Network.getCookies` with two cookies; assert `Cookies` returns them with names/values/domains and that the request carried the `urls` param.
- [ ] **Step 2: Run it** — `go test ./internal/browser/ -run TestPageCookies -v`; expect a compile failure (undefined `Cookies`).
- [ ] **Step 3: Implement** — add the `Cookie` type and `Cookies` method that calls `Network.getCookies` (falling back to `Storage.getCookies` for empty urls) through `p.call`.
- [ ] **Step 4: Run it** — same command; expect PASS. Then `go test ./internal/browser/`.
- [ ] **Step 5: Commit** — `git commit -m "browser: add CDP cookie read"`.

### Task 2: module skeleton, config, registration

**Files:**
- Create: `clients/doubao/config.go`, `clients/doubao/doubao.go`, `clients/doubao/config.example.json`
- Modify: `clients/all/all.go`
- Test: `clients/doubao/config_test.go`

**Interfaces:**
- Produces: `func init() { core.Register("doubao", New) }`, `func New(deps core.Deps) (core.Client, error)`
- Produces: `config` with accessors `webBase() string`, `requestTimeout()`, `chatTimeout()`, `probeTimeout()`, `cooldown()`, `authCooldown()`, `modelsTTL()`, `browserHeadless() bool`, `browserPath() string`.

- [ ] **Step 1: Failing test** — `config.example.json` parses, and an empty config yields the documented defaults.
- [ ] **Step 2: Run it** — expect FAIL (package does not exist).
- [ ] **Step 3: Implement** — config schema/defaults, `Name()`, a `Models` fallback list, a `Status` that reports "no account" cleanly, and a `Chat` returning `core.ErrNotConfigured`. Add the blank import to `clients/all/all.go`.
- [ ] **Step 4: Run** — `go test ./clients/doubao/ ./clients/all/`.
- [ ] **Step 5: Commit** — `git commit -m "doubao: module skeleton and config"`.

### Task 3: account store and pool

**Files:**
- Create: `clients/doubao/accounts.go`
- Test: `clients/doubao/accounts_test.go`

**Interfaces:**
- Produces: `type account struct { ID, Label, Cookie string; Enabled bool; State string; CooldownUntil time.Time; Failures int; LastError string; LastUsed time.Time; Identity string }`
- Produces: `(*Client) pickAccount() (*account, error)`, `noteSuccess`, `noteFailure`, `release`, `snapshot`, persisted to `accounts.json`/`state.json`.

- [ ] **Step 1: Failing test** — add two accounts, snapshot returns both; pick skips a disabled one; a cooldown account is skipped until its time passes; the persisted file round-trips.
- [ ] **Step 2: Run** — expect FAIL.
- [ ] **Step 3: Implement** — the store with load/save, the picker with priority + round-robin, and the runtime state file.
- [ ] **Step 4: Run** — `go test ./clients/doubao/`.
- [ ] **Step 5: Commit** — `git commit -m "doubao: account store and pool"`.

### Task 4: web transport and cookie auth

**Files:**
- Create: `clients/doubao/web.go`
- Test: `clients/doubao/web_test.go`

**Interfaces:**
- Produces: `(*Client) doJSON(ctx, acct *account, path string, params url.Values, body any, out any) error`
- Produces: `commonParams() url.Values` and `cookieHeader(cookie string) string`
- Produces: `classifyHTTP(status int, body []byte) error` marking auth vs rate-limit vs transport.

- [ ] **Step 1: Failing test** — an `httptest.Server` asserts the request carries the cookie header, `Agw-Js-Conv: str`, the common query params, and a JSON body; a 401 maps to the auth error and a 429 to the rate-limit error.
- [ ] **Step 2: Run** — expect FAIL.
- [ ] **Step 3: Implement** — the transport plus a `probeAccount` that calls `/alice/user/get_account_info`.
- [ ] **Step 4: Run** — `go test ./clients/doubao/`.
- [ ] **Step 5: Commit** — `git commit -m "doubao: web transport and cookie auth"`.

### Task 5: chat SSE

**Files:**
- Create: `clients/doubao/chat.go`, `clients/doubao/sse.go`
- Test: `clients/doubao/chat_test.go`, `clients/doubao/sse_test.go`

**Interfaces:**
- Produces: `(*Client) Chat(ctx, req) (core.Stream, error)` posting `{messages, completion_option}` to `/samantha/chat/completion`.
- Produces: `buildChatBody(req *core.ChatRequest) ([]byte, error)`.
- Produces: `doubaoStream` implementing `core.Stream` with `CMPL`/`FIN`/`ERR` mapping.

- [ ] **Step 1: Failing test** — `buildChatBody` produces the expected `messages`/`completion_option`; the SSE reader tolerates `data:{...}` with no space and a final frame with no blank line; a scripted stream yields delta events then one done.
- [ ] **Step 2: Run** — expect FAIL.
- [ ] **Step 3: Implement** — the body builder, the tolerant SSE reader and the stream.
- [ ] **Step 4: Run** — `go test ./clients/doubao/`.
- [ ] **Step 5: Commit** — `git commit -m "doubao: chat SSE"`.

### Task 6: web login

**Files:**
- Create: `clients/doubao/login.go`, `clients/doubao/browserlogin.go`
- Test: `clients/doubao/login_test.go`

**Interfaces:**
- Produces: `(*Client) StartLogin/PollLogin/CancelLogin` per `core.LoginProvider`.
- Produces: a package-level `launchBrowser = func(ctx, browser.LaunchOpts) (loginBrowser, error)` seam, overridden in tests.

- [ ] **Step 1: Failing test** — with a fake browser returning two cookies, `PollLogin` stores an account and flips to `success`; with a fake returning no session cookie it stays `pending`; a launch error yields a `pending` state whose message names the manual fallback.
- [ ] **Step 2: Run** — expect FAIL.
- [ ] **Step 3: Implement** — the session store, the launch/poll/capture flow, and the store-on-success step.
- [ ] **Step 4: Run** — `go test ./clients/doubao/`.
- [ ] **Step 5: Commit** — `git commit -m "doubao: web login"`.

### Task 7: AccountManager and models

**Files:**
- Create: `clients/doubao/models.go`
- Modify: `clients/doubao/accounts.go`
- Test: `clients/doubao/accounts_test.go`, `clients/doubao/models_test.go`

**Interfaces:**
- Produces: `AccountFields`, `Accounts`, `AddAccount`, `RemoveAccount`, `SetAccountEnabled`, `TestAccount`, `RefreshAccount`.
- Produces: `RefreshModels(ctx) ([]core.Model, error)`.

- [ ] **Step 1: Failing test** — adding an account with a cookie stores it and returns a record without the cookie; `TestAccount` against a fake upstream reports ok; `Models` returns the configured catalogue.
- [ ] **Step 2: Run** — expect FAIL.
- [ ] **Step 3: Implement** — the account form fields (`cookie`, `label`), CRUD, test/refresh via `probeAccount`, and the model catalogue.
- [ ] **Step 4: Run** — `go test ./clients/doubao/`.
- [ ] **Step 5: Commit** — `git commit -m "doubao: account management and models"`.

### Task 8: balance, health, revive, affinity

**Files:**
- Create: `clients/doubao/balance.go`, `clients/doubao/health.go`, `clients/doubao/revive.go`, `clients/doubao/affinity.go`
- Test: `clients/doubao/balance_test.go`, `clients/doubao/health_test.go`

- [ ] **Step 1: Failing test** — balance parses a scripted credits reply; `Health` reports not-servable on an empty pool and servable on a ready one; `PoolStats` counts in-flight; `Revive` clears a cooldown.
- [ ] **Step 2: Run** — expect FAIL.
- [ ] **Step 3: Implement** — all four capabilities.
- [ ] **Step 4: Run** — `go test ./clients/doubao/`.
- [ ] **Step 5: Commit** — `git commit -m "doubao: balance, health, revive, affinity"`.

### Task 9: docs and full verification

**Files:**
- Create: `clients/doubao/README.md`
- Modify: `README.md`

- [ ] **Step 1: Docs** — document the module's capabilities, config keys, the web-login flow and the deliberate omissions.
- [ ] **Step 2: Full build/test** — `go build ./...` then `go test ./...`; both clean.
- [ ] **Step 3: Commit** — `git commit -m "doubao: docs and verification"`.
