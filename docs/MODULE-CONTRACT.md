# client2api — module contract

This file is the **frozen interface** between the core and every client module.
Read it before touching anything under `clients/`.

## 0. The one rule

```
        cmd/  ──┐
    internal/  ─┴──►  internal/core   ◄──  clients/<name>
```

* Nothing under `internal/` or `cmd/` may import a `clients/*` package.
* No `clients/*` package may import another `clients/*` package.
* `clients/all` is the **only** file that knows the full client list (blank imports).

Everything a module needs comes from `core.Deps`. If you find yourself wanting a
new field on `Deps`, that is a design smell — prefer putting the knob in your own
`Deps.Config` JSON.

## 1. What you must implement

In `clients/<name>/<name>.go`:

```go
func init() { core.Register("<name>", New) }

func New(deps core.Deps) (core.Client, error) { ... }
```

`core.Client`:

```go
Name() string
Models(ctx context.Context) ([]core.Model, error)
Chat(ctx context.Context, req *core.ChatRequest) (core.Stream, error)
Status(ctx context.Context) core.Status
```

`core.Stream`:

```go
Recv() (core.Event, error)   // io.EOF exactly once at the clean end
Close() error                // must be idempotent
```

### Rules that the gateway relies on

* `Name()` must equal the string passed to `core.Register`, and is the routing
  prefix (`zcode/GLM-5.3`).
* `req.Model` arrives **already stripped** of the `<client>/` prefix. Never
  re-add or re-strip it.
* `Chat` returns `core.ErrNotConfigured` when no usable credential exists.
  The gateway maps that to HTTP 503. Return `core.ErrUnsupported` for a request
  shape you cannot express upstream → HTTP 400. Any other error → HTTP 502.
* **Name the credential that served the turn.** `req.ServedBy` is a `*string`
  the gateway allocates per request; call `core.NoteServedBy(req, id)` at the
  point you pick the account, and it is read exactly once after `Chat` returns.
  You are the only place this is known: the gateway cannot see inside your pool,
  so without it every success is filed under `(unrouted)` in the usage ledger and
  shows a blank account on the console row.
  * A module that rotates names each attempt — the last writer wins, which is
    what makes the recorded account the one that actually got through.
  * Call it where the account is *settled* (right after the pick, or where you
    bind the conversation), not only on success: on failure the gateway prefers
    the account named by the error itself and falls back to yours when the error
    names none.
  * Write the id the panel's account table uses, and never an account you did
    not use. Not calling it is allowed and means "unknown" — it is not a lie,
    and the gateway handles it; naming the wrong account is a lie and is worse
    than silence.
  * Like `ConversationID`, this is a gateway↔module channel. Never copy it into
    the upstream body, and never read it as input.
* `Recv` emits `core.EventDelta` / `EventToolCall` / `EventUsage` /
  `EventDone` / `EventError`. It returns `io.EOF` **once** at clean end.
  Do not return `io.EOF` and then keep going.
* Emit at most one `EventUsage` and at most one `EventDone`. `EventDone.Finish`
  is an OpenAI finish reason (`stop`, `length`, `tool_calls`, `content_filter`).
* `Chat` must be safe to call concurrently. `Recv` on one stream is single-goroutine.
* Honour `ctx` cancellation: return promptly and close the upstream body.
* Never panic. A module that panics takes the whole process down.

### State

All persistent state goes under `Deps.DataDir` (`data/<name>/`, already created
with mode 0700) and nowhere else. Use `core.WriteJSONAtomic` /
`core.WriteFileAtomic` (0600, tmp+rename) / `core.ReadJSON` / `core.EnsureDir`.
Never write outside `DataDir`. Never read another module's `DataDir`.

### Secrets

Never log a full token, key or cookie. Use `core.MaskSecret`.

## 2. Config

`Deps.Config` is the raw JSON of the `clients.<name>` object from
`configs/client2api.json`, or nil if absent. **You own that schema** — define
your own struct, unmarshal it, document it in `clients/<name>/README.md`.

Prefer *auto-discovery* of credentials already on this machine (the desktop
clients are all installed) with an explicit config override. A module that
needs no config at all is the best module.

## 3. Build & test

Every shell invocation must set these (the sandbox denies Go's default cache):

```powershell
$tools='D:\client2api-lab\_tools'
$env:PATH="$tools\go\bin;$env:PATH"
$env:GOCACHE="$tools\gocache"; $env:GOTMPDIR="$tools\gotmp"; $env:GOMODCACHE="$tools\gomodcache"
$env:GOPROXY='https://goproxy.cn,direct'
Set-Location 'D:\client2api-src'
```

`Set-Location` is mandatory — `go build` from anywhere else fails with
`cannot find main module`.

Then:

```powershell
go vet ./...
go build ./...
go test ./clients/<name>/... -v
```

**Dependencies:** a module should add nothing beyond the two dependencies the
project already has — `github.com/refraction-networking/utls` (TLS fingerprints)
and `golang.org/x/net` (HTTP/2). A module may pull a dependency that only it
needs, but a dependency another module could also need does not belong to a
module: that couples them. If you genuinely need one, add it with `go get` and
say why in the module's README.

### Live test

```powershell
go build -o client2api.exe ./cmd/client2api
.\client2api.exe -config configs/client2api.json
# then
curl.exe -s http://127.0.0.1:8788/v1/models
curl.exe -s http://127.0.0.1:8788/v1/status
curl.exe -s -X POST http://127.0.0.1:8788/v1/chat/completions -H "Content-Type: application/json" -d '{\"model\":\"<name>/<model>\",\"messages\":[{\"role\":\"user\",\"content\":\"say hi\"}],\"stream\":true}'
```

Network access needs the proxy: `http://192.168.0.217:18867`
(`-proxy` flag, or `HTTPS_PROXY` env). **Use `curl.exe`, not
`Invoke-WebRequest`** — the latter is broken in this non-interactive shell.

## 4. Tests

Each module ships `clients/<name>/<name>_test.go` with **offline** unit tests:

* pure functions (encoders, parsers, body builders, SSE frame decoders) —
  table-driven, using recorded upstream fixtures inline as string literals;
* a fake `http.RoundTripper` or `httptest.Server` for the transport layer;
* credential loading from a temp dir.

Tests must pass with **no network**. Live/credentialed checks go in a separate
`*_live_test.go` guarded by an env var (e.g. `CLIENT2API_LIVE=1`) and skipped
by default.

## 5. Panel / status

`Status()` is rendered by the panel. Fill it in honestly:

* `Ready` — true only when a request could plausibly succeed right now.
* `Detail` — one human line: account count, mode, last error.
* `Accounts` — one `core.AccountStatus` per credential, with `State` one of
  `ready|cooling|exhausted|invalid|unknown`.
* `Models` — the model ids you serve (bare names). This list **may be cached**:
  the panel calls it on every refresh, so a module that asks its vendor for the
  catalogue should keep a TTL (see §6, `core.ModelRefresher`). Caching `Models`
  is allowed; making `Status` expensive is not.

`Status` must never block for more than ~1s and must never make a network call
that can hang; it is called on every panel refresh (10s).

### What the panel itself guarantees

Two things a module never has to think about, but should not fight either:

* **Every panel response carries a policy.** The shell, the JSON API and even the
  401s go out with `Content-Security-Policy`, `X-Content-Type-Options: nosniff`,
  `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer`,
  `Cross-Origin-Opener-Policy` and `Cross-Origin-Resource-Policy: same-origin`
  (`internal/panel/security.go`). The policy starts from `default-src 'none'` and
  names the shell's single inline script by SHA-256 hash rather than by
  `'unsafe-inline'`; the hash is computed from the embedded `index.html` once at
  startup, and if the script block cannot be located the policy degrades to
  `'unsafe-inline'` instead of leaving a blank page. `connect-src 'self'` is what
  lets the panel call `/v1/chat/completions` from the browser without CORS — a
  module must not expect the panel to fetch a third-party origin.
* **Log lines are channelled, not just stored.** The gateway's ring keeps
  `LogEntry{ts, ch, text}` and classifies each line by prefix as `chat` (`| #`
  rows), `task` (the chore verbs — `checkin`, `activity`, `streak-bonus`,
  `travel`, `blackcat`, `lottery`, `keepalive`, `balance`, `school`,
  `user-resource`, and the panel's own `panel: …` verbs) or `sys` (everything
  else). So a module that logs its chores through `deps.Logf` with one of those
  verb prefixes gets them filed under 任务 in the panel for free; a line that
  starts with something else is still readable, just filed under 系统. Note the
  trailing spaces in the prefix table: they are what keeps `checking something`
  out of `checkin`.

## 6. Optional: panel account management

`Status()` is read-only. A module that can *change* its credentials implements one
or more of the interfaces in `internal/core/accounts.go` **in its own package**
(conventionally `clients/<name>/accounts.go`). Nothing else in the tree changes:
the panel type-asserts the registered value, so a module that does not implement
them is simply read-only and no sibling module notices.

| Interface | Methods | What the panel shows |
|---|---|---|
| `core.AccountManager` | `AccountFields`, `Accounts`, `AddAccount`, `RemoveAccount`, `SetAccountEnabled`, `TestAccount`, `RefreshAccount` | Per-client account table, "add account" form, enable/disable, test, refresh |
| `core.CredentialImporter` | `Discover(ctx) ([]DiscoveredCredential, error)`, `Import(ctx, paths []string, all bool) ([]AccountRecord, error)` | Import screen listing what was found on this machine |
| `core.BundleImporter` | `ImportBundle(ctx, name string, data []byte) (BundleImportReport, error)` | 「上传导出文件」控件 on the import screen, behind the separate `capabilities.import_bundle` flag. The panel hands over the raw document and never parses it: the shape belongs to whichever third-party tool produced the dump. Per-row failures go in the report's `Errors` (one unusable row must not cost the other ninety-nine); a returned `error` means the document as a whole was unreadable, which the panel reports as 502 |
| `core.LoginProvider` | `StartLogin`, `PollLogin`, `CancelLogin` | "Sign in" button that shows a URL (plus a user code when the flow carries one), then polls until the operator finishes |
| `core.CheckinProvider` | `CheckinActions(ctx) []CheckinAction`, `Checkin(ctx, id, action string) (CheckinResult, error)` | "Check in" button per account, plus "check in all" |
| `core.ModelRefresher` | `RefreshModels(ctx context.Context) ([]Model, error)` | "Refresh" button on the 模型与档位 view, and an upstream-vs-fallback marker per model row |
| `core.ConversationBinder` | `BindConversation(conversationKey, accountID string)`, `UnbindConversation(conversationKey string) bool`, `ConversationAccount(conversationKey, model string) (string, bool)` | The 对话测试 view's account picker, via `GET/POST …/clients/<name>/conversations` and `POST …/conversations/unbind`. It is also the routing contract the module keeps to itself; `core.AsConversationBinder` is how a caller discovers it. Modules without it report `capabilities.conversations = false` and answer 501 |
| `core.TaskProvider` | `Tasks(ctx, accountID string) ([]TaskInfo, error)`, `RunTask(ctx, accountID, code string) (TaskResult, error)` | 任务中心 → 任务看板: the vendor's chore list with progress, one run button per automatable row, and each row's last outcome |
| `core.TaskAccepter` | `AcceptTasks(ctx, accountID string, codes []string) error` | 任务看板每行的「接受」按钮（把任务标记为已接） |
| `core.BulkTaskAccepter` | `AcceptAllTasks(ctx, accountID string) (BulkAccept, error)` | 「全部接受」按钮，回答 `{accepted, failed, message}` |
| `core.TaskClaimer` | `ClaimTask(ctx, accountID, code string) (TaskClaim, error)` | 「领取」按钮；零增量算成功（幂等），不是错误 |
| `core.TaskAutoRunner` | `RunTaskAuto(ctx, accountID, code string) (AutoTaskResult, error)` | 「自动完成」按钮 + 进度回读（`progress_before/after`、`claimable`、`claimed`、`claim_error`） |
| `core.Reviver` | `ReviveAccount(ctx, id string) error` | 账号行的「恢复」按钮：清掉该账号在**本进程内**的全部惩罚（冷却/熔断/停用），让池子立刻重新尝试 |
| `core.BalanceProvider` | `AccountBalance(ctx, id string, soon time.Duration) (Balance, error)` | 账号行的「余额」按钮，外加「即将到期」桶（`soon` 窗口内会过期的额度） |
| `core.PackageProvider` | `AccountPackages(ctx, id string) (PackageReport, error)` | 「积分构成」视图：逐账号的额度分片（`remain/size/expires_at`），失败是**行级**错误 |
| `core.VoucherProvider` | `AccountVouchers(ctx, id string) ([]Voucher, error)` | 成长任务队列里的券码列表（券码原样展示、可复制） |
| `core.HealthProvider` | `Health() Health` | 状态页每个客户端的 `health` 块：`servable`（现在能不能服务）、`ready/cooling/disabled/total` 计数、`realms`、以及不能服务时的 `note` 原因 |
| `core.PoolStatsReporter` | `PoolStats() PoolStats` | 状态页每个客户端的 `pool` 块：`in_flight`、`in_flight_full`、`sticky_sessions`（面板的「N 个账号在途占满」与「粘性会话」tile 读的就是它） |
| `core.HintProvider` | `Hint(kind FailureKind, message string, ctx HintContext) string` | 上游失败时 `error.gateway_hint` 的文案。网关先问模块、再回落 `internal/hint` 的共享规则表：返回 `""` 表示「我这条没话说」，不是终判。模块自己的业务码（如 workbuddy 的 11133/11135 图片族）只有模块能解释，所以这条链路是模块专有知识的唯一出口 |

### The account table is the panel's editing surface

Every row `Accounts()` returns is rendered with 删除 and 停用 buttons, and those
buttons call `RemoveAccount` / `SetAccountEnabled` unconditionally. So a module
must list only records those two can actually act on: **an endpoint or credential
the module merely *resolves* is configuration, not an account.** `clients/tabbit`
used to append `locate()`'s result as a `managed:false` row, which on a fresh
install meant the built-in `http://127.0.0.1:50124` default sat in the pool as an
invalid, undeletable row — the only two buttons it had could do nothing but fail
(`RemoveAccount` answered "endpoint … was not added through the panel; nothing to
remove").

Do not over-correct, though: a **real** credential found on this machine is not
that case and must stay listed. trae marks those `managed:false` and still lists
them, refusing only 删除 ("account … was not added through the panel; disable it
instead") while `SetAccountEnabled` genuinely suppresses them — the disable
button works, so the row is honest.

Nothing is hidden by dropping a resolved endpoint from the table; it moves to the
status surface. tabbit reports the effective sidecar URL and why, in
`Status().Detail` ("no sidecar configured and nothing listening on `<url>`
(`<err>`)"), which both `/v1/status` and the config page show.

### Task verbs

`TaskProvider.RunTask` is the vendor's "make progress happen". The four verbs
above are the reference's separate operations, and they stay separate because
the operator needs to retry exactly one of them: a chore whose progress is
complete but whose reward failed must be claimable on its own, without running
the chore a second time (which some vendors count as a separate attempt).

Rules:

- **A chore you cannot automate is an error naming the reason**, not a silent
  `OK:false` — the panel answers `501` with your message, which is how it tells
  "go do this in the official client" apart from "the vendor refused". If the
  chore list carries a per-task "automatable" flag, report it there too (`TaskInfo.Auto`).
- **Collecting a reward that was already collected is success.** Answer with
  `TaskClaim.AlreadyClaimed` (or zero credit *and* zero energy); the panel then
  says "该奖励此前已领取" instead of showing a zero-credit success or a failure.
- **You do not need to lock anything.** The panel answers `409` while one task
  action for that account is in flight, so two rounds can never overlap; a module
  that also locks is free to, but must not deadlock behind the panel's lock.
- **`RunTaskAuto` reports the read-back**, not just success: `ProgressBefore`,
  `ProgressAfter` and `Claimable` are what the operator judges the run by, and
  `ClaimError` is how you say "the chore passed, the reward did not" so the panel
  can offer the claim button again. The amounts go on the embedded
  `TaskResult.Credit`/`Energy` — `AutoTaskResult` deliberately does not redeclare
  them (an outer field of the same name would shadow the embedded one).

### Credits, packages and vouchers

These three are the reference's 积分构成 view and its account-row balance button.
They are opt-in one by one because a vendor can genuinely have a pooled balance
with no tranches, or tranches with nothing else to say.

- `Balance` has **no JSON tags** — the panel owns the wire shape
  (`{ok, credits, credits_total, expiring?, earliest_at?, earliest_remaining?}`).
  Fill the expiring buckets only when `soon > 0`; `soon == 0` means the operator
  does not want that bucket, not "expire now".
- A vendor refusal here **is** an error: the panel answers `502` rather than
  drawing a zero balance, because a wrong number on a credit screen is worse than
  no screen.
- Voucher `Code` values are **not** redacted on the way out: the redemption code
  is the entire payload of that view. Everything else that quotes a vendor string
  (row `error`, `message`) still goes through `core.Redact`.

### Model catalogue

`Models()` answers "what can this module serve" and is allowed to be a static
list. `ModelRefresher` is how a module says "I can go ask the vendor right now":

- `Models()` may serve from a cache (a TTL of a few minutes is the norm) because
  the panel calls it on every refresh. `RefreshModels()` **must bypass** that
  cache — it exists precisely to force a re-read.
- **A failed refresh must not empty the catalogue.** Return the last known-good
  list *alongside* the error. The panel renders the list and marks the rows as
  fallback; a picker that goes blank because the vendor hiccupped is worse than a
  stale one.
- If no credential is available at all, return the configured/built-in list with
  a nil error and make no request — and do **not** count that as an attempt, so
  the next refresh after a sign-in can succeed immediately instead of waiting out
  the TTL.
- `GET /panel/api/models` reports `can_refresh` per module from
  `core.CapabilitiesOf`, so a module without this interface is never shown a
  button that lies. `POST /panel/api/models/refresh` calls `RefreshModels` only
  on modules that implement it and `Models` on the rest.
- **A claimed output ceiling is not a measured one.** Whatever a module puts in
  `core.Model.Extra` as `max_output_tokens` is the vendor's *claim*; the panel
  also reads an optional `data/output_probes.json` (written by the `cmd/probe`
  tool, not by any module) and annotates the row when a probe found the real
  ceiling lower. The two are shown side by side rather than merged, so a module
  never has to reconcile them and a missing probe file simply means no
  annotation. `GET /panel/api/model_probes` serves that file as-is; see the
  README's *Probing output ceilings* for the verdict vocabulary.

### Conversation → account affinity

`Chat` may route a request to any account in its pool, and a module whose vendor
bills a *per-account* prompt cache (workbuddy salts `prompt_cache_key` per
credential) pays for a conversation that wanders between accounts: the ~8k-token
prefix is re-billed on every cache miss. `core.ConversationBinder` is how a
module says "this conversation is pinned to the account that already served it".

```go
// Implemented in the module's own package, e.g. clients/<name>/affinity.go.
func (c *Client) BindConversation(conversationKey, accountID string)
func (c *Client) UnbindConversation(conversationKey string) bool
func (c *Client) ConversationAccount(conversationKey, model string) (string, bool)
```

Rules:

- **Opt-in, and nil-safe.** `internal/core/affinity.go` provides the mechanism
  (`core.NewAffinity(ttl)`, `Bind`/`Resolve`/`Unbind`/`Count`/`GC`/`StartGC`/
  `StopGC`). Every exported method on a nil `*Affinity` is a no-op or reports
  "nothing bound", so a module can hold a `nil` field and call it freely. A
  module that ignores the interface entirely routes exactly as it did before —
  no sibling module notices, and no core edit is required to adopt it.
- **Off is not a zero window.** `session_sticky.enabled` (default **true**) is a
  knob separate from the TTL, and turning it off must skip `Bind`/`Resolve`
  entirely rather than shorten the window: the reference builds no session router
  at all when it is false, so every request selects an account from scratch.
  `core.LiveSettings.AffinityEnabled` carries it as a pointer for the same reason
  as the ceilings — `nil` means "the file said nothing" and leaves the current
  state alone. Disabling also drops the bindings already held, because they were
  chosen under a policy that is no longer in force; a TTL of `0` is *not* the way
  to disable stickiness, and `SetTTL` resolves a non-positive value back to the
  30m default instead of to "expire immediately".
- **The key comes from the request, never from a counter.** Use
  `core.ConversationKeyOf(req)`: the conversation id the gateway already
  resolved — `metadata.conversation_id` → `metadata.conversationId` → top-level
  `conversation_id` → top-level `conversationId` — and otherwise the raw option
  spellings `conversation_id` → `conversationId` → `prompt_cache_key` → trimmed
  `req.User`. Prefer it over `core.ConversationKey(req.Options, req.User)`: a
  client that follows the OpenAI convention carries its id in `metadata`, where
  the option map cannot see it, so keying off options alone silently drops
  stickiness for exactly the clients that are most explicit about their
  conversations. A caller that supplies nothing yields `""`, which must turn
  stickiness **off** for that request rather than inventing a shared key.
- **Content-derived keys are opt-in, and they are namespaced.** A client that
  sends no conversation id *and* no `user` still deserves stickiness: without it
  every turn rotates accounts and re-bills the whole prefix, because the cache the
  previous account had already warmed is unreachable. `core.DeriveConversationKey(msgs)`
  is that fallback — it hashes the system prompt plus the **first** user turn
  (`sha256(system + "\x00" + firstUser)[:16]`) and returns the digest under the
  `d-` prefix, which keeps it from ever colliding with an id a client really sent.
  Only those two turns go in: a multi-turn request appends history every turn, so
  a whole-transcript hash would move each turn and be worse than no stickiness at
  all. Non-text parts contribute `[type]` rather than their content, so an image
  whose signed URL is re-minted per turn does not move the key, while "no image"
  and "one image" still differ — and a first turn that is *only* an image still
  derives.
  It is a **separate call**, deliberately not folded into `ConversationKeyOf`:
  tabbit and qwenwork pin "a request that names no conversation leaves the table
  empty and rotates through the pool", and whether an unattributed request should
  stick is a per-module decision. `clients/workbuddy` is the one module that opts
  in (the reference itself is workbuddy-only), and even there every explicit
  spelling — `req.ConversationID`, the option map, `meta.ConversationID` — wins
  over the derived key.
- **Stickiness yields to health, always.** `ConversationAccount` takes the model
  as well as the key so the query can answer "not this account" when the bound
  account is parked (cooling, exhausted, invalid, or model-cooled for *this*
  model). A dead binding is dropped, not remembered: the caller picks normally
  and re-binds. Blocking a request, or serving a parked account to honour a
  binding, is a bug in both directions.
- **Bind at pick time, not after success.** Two concurrent requests for a brand
  new conversation must agree on one account instead of racing onto two and
  splitting the cache. Re-binding an existing key is an overwrite.
- **Bindings expire.** The default TTL is 30 minutes of *idle* time
  (`core.DefaultAffinityTTL`) and `StartGC` sweeps them every
  `core.DefaultAffinityGCInterval` (5 minutes), so the table cannot grow with
  conversations nobody asks about any more.
- **Bindings are process state, not credentials.** The table holds one opaque
  account id per conversation key and is rebuilt after a restart. Nothing in it
  may be written to a state file or rendered in the panel.

### Account selection policy

The account a `Chat` picks is module policy, not a contract — but two properties
are expected of any pool:

- **Never turn rotation into a tight retry loop.** Rotation is pacing-sensitive:
  a sub-second burst across credentials is what gets a pool flagged. Honour
  `core.BackoffFrom` (jittered, capped) between attempts.
- **A per-model park must stay runtime-only and must be swept.** Losing one
  model on one credential is not losing the credential. Park the model, not the
  account, and drop lapsed parks on the pick path so the map cannot grow with
  every model an account ever touched.

### Daily check-in

`CheckinProvider` is how a module says "this platform hands out a daily
reward and here is how to claim it". The panel draws **one button per
`CheckinAction`**, so an action list of two (workbuddy: `daily-checkin` for
CN, `daily-activity` for the international realm) becomes two buttons rather
than one button that guesses.

Rules:

- **Advertise only what exists.** `CheckinActions` must return `nil` when the
  module has no claimable reward, or when no configured account could claim
  one. `core.CapabilitiesOf` turns a non-empty list into `checkin: true`; an
  empty list means the panel shows no button at all. Do not invent an endpoint
  because a sibling client has one — of the fourteen shipped modules only
  **workbuddy** (CN `POST /v2/billing/meter/daily-checkin`, intl daily-activity
  conversation), **trae** (`POST api.trae.cn/trae/api/v2/ug/checkin_credits/status`
  then `/claim`), **zcode** (`zcode-plan/billing/preview` then `/claim`, one
  action named `claim`), **minimaxcode** (`GET /minimax-cloud/api/v1/signin/status`
  then `POST …/signin/claim`, one action named `daily-signin`), **qwenwork**
  (`GET /sash/api/v1/me/daily-check-in/status` then `/claim`, one action named
  `daily`), **raccoon** (`POST /api/web/desktop/v1/login/points/grant`, one
  action named `login-points` — that desktop *login* grant is exactly how the
  daily 300 credits are claimed: a real grant writes a `daily_grant` of +300 to
  the points ledger, and a same-day repeat answers `granted:false`. The reply
  carries no reason, so a refusal is reported honestly, not guessed at),
  **lobsterai**, **codearts** and **loomy** have a real one. kimi, tabbit,
  openrouter and opencode have none.
  A module whose action is blocked by a vendor control it cannot satisfy still
  advertises the action and reports the block honestly — zcode's claim is
  captcha-gated ahead of every other check, so it fails with a message naming
  `captcha_command` rather than pretending the feature is absent.
- **Two bits, two questions.** `core.Capabilities.Checkin` is the *static* bit
  ("this module implements `CheckinProvider`") and drives the capability matrix,
  the bulk routes and the scheduler's scan. `core.Capabilities.CheckinReady` is
  the *live* bit (`len(Actions) > 0`) and is the only thing the account row's
  button reads. Collapsing the two is the defect the split exists to prevent: a
  module whose accounts are all expired (`minimaxcode`'s imported store) or
  absent (`raccoon` with an empty pool) keeps its ✓ in the matrix — it really
  does implement check-in — while offering no click that could only fail.
  `internal/core/capabilities_test.go` pins the split.
- **"Already claimed today" is success, not failure.** Report
  `CheckinResult{OK: true}` with `Data["already_done"] = true` (trae reads the
  `checked_in` boolean, workbuddy accepts upstream code `10001`). The operator
  asked "is today's reward in hand?" — the answer is yes.
- **Persist the timestamp.** If you track "done today" locally, write it into
  your own state file so it survives a restart, and compare only the date
  prefix (`2006-01-02`) — see `clients/workbuddy/auth.go`'s `didToday`.
- **Gate on the realm.** A CN-only action offered for an international account
  must return `OK: false` with an explanatory `Error` **without sending a
  request**; a wrong-realm call can burn the account's own rate limit.
- **Never leak the credential.** Pass `Error`, `Message` and every string in
  `Data` through `core.Redact`, exactly as for `TestAccount`.

### Task board

`TaskProvider` is how a module exposes the vendor's **growth centre** — the daily
chores the official client performs for the user (send five messages, adopt the
mascot, click through a template) — as something the panel can drive. Core knows
only the shape; every vendor specific lives in the module, so two modules may
disagree about what a task even is.

Surface:

| Route | Meaning |
|---|---|
| `GET <base>/tasks?account=<id>` | the chore list + this client's recent runs |
| `POST <base>/tasks/<code>/run` | start one; body `{"account":"…"}`, optional |
| `GET <base>/tasks/runs/<id>` | poll one run |

Rules:

- **A run is asynchronous and the panel enforces it.** `POST …/run` answers
  `202` with a run id before the chore starts. The run executes on a context
  with a 45-minute deadline that is deliberately **not** derived from the
  request — the response is written long before the chore ends, so `r.Context()`
  would cancel a run the operator just asked for the moment they changed view.
  Do not try to make `RunTask` synchronous; some chores sleep ~45 s between
  upstream events and a couple of dozen of them are queued behind one button.
- **A vendor refusal is a result, not an error.** "Already claimed", "prerequisite
  not met", "anti-cheat rolled the progress back" are all real answers: return
  `TaskResult{OK: false, Error: "…"}`. Reserve a non-nil error for *the attempt
  could not be made at all* — no credential, transport failure. The panel
  journals both, but only an error means "we never asked".
- **`Auto: false` is a legitimate answer.** Some chores genuinely need a human: a
  real connector authorisation, a captcha, an actual donation. Say so in `Note`
  rather than offering a button that cannot work. The panel renders such a row
  read-only instead of hiding it, because the operator still wants to see the
  progress.
- **Advertise only what you can do.** `Tasks` must return an empty slice (nil
  error) when there is nothing to show, and must not mutate upstream state —
  it is called on a timer. `core.CapabilitiesOf` turns the interface into
  `tasks: true`; the board is only drawn for modules that implement it.
- **Pacing is yours.** The panel runs "execute all automatable tasks"
  **serially**, one chore at a time, precisely because the reference
  implementation measured that bursting the same events a couple of seconds
  apart makes the vendor roll the whole run back. Any additional spacing (the
  reference uses 45 s ± 10 s jitter between chat events) belongs inside
  `RunTask`, not in the panel.
- **Never leak the credential.** `Title`, `Desc`, `Note` and `Group` are
  upstream strings: the panel passes them through `core.Redact`, and a module
  that puts a token in a task title would defeat that. Do not.

### Interactive sign-in

`LoginProvider` is how a module signs an operator in **without asking them to
install a CLI**. The panel calls `StartLogin`, shows `LoginState.URL` (and
`LoginState.Code` when the flow carries a user code) as something the operator
opens in a browser, then polls `PollLogin(session_id)` on a timer until the
state turns terminal.

Rules:

- **Prefer the web/HTTP flow; make the CLI a fallback.** Every shipped module
  that has a sign-in at all signs in over HTTP: kimi uses the RFC 8628 device
  grant against `https://auth.kimi.com`, trae opens a loopback redirect on
  `127.0.0.1` and catches the callback, zcode polls
  `POST {api}/oauth/cli/init` → `GET …/oauth/cli/poll/{flow_id}`, workbuddy
  polls `/v2/plugin/auth/state` → `/token`, qwenwork uses a PKCE device flow.
  A module whose only route is shelling out to a vendor CLI must say so in
  `Status().Detail` and keep the CLI path behind the HTTP one — an operator who
  clicked "Sign in" should not be told to go install a binary if the vendor
  offers a URL.
- **`state` must start non-empty.** `LoginState.State` is the session's gate:
  every transition (`pending → success|failed|cancelled`) is a no-op while the
  state is `""`. Build the session with `State: core.LoginPending` in the
  struct literal, or normalise `""` to pending in the snapshot you return.
  Getting this wrong makes the callback answer "session is no longer active"
  for every login and swallows every outcome.
- **Pending is not an error.** `PollLogin` returns
  `(LoginState{State: pending}, nil)` while the operator is still in the
  browser. Reserve a non-nil error for a programming mistake (unknown session
  id, empty id). A vendor outage, a 5xx or a transport failure is a *pending*
  state with a message, so a retry can still succeed.
- **A failed start is a state, not an error.** If the device-authorisation call
  fails outright, return `LoginFailed` with the reason scrubbed into `Message`.
  "No network" and "the client id was retired" are facts for the operator, not
  Go errors.
- **Honour the vendor's polling interval.** RFC 8628's `slow_down` means add
  `interval` seconds (kimi adds 5s) and say so in the message. Cache the next
  allowed poll time and answer `pending` locally without a request when the
  panel polls faster than the vendor allows — the panel's timer is not the
  vendor's rate limit.
- **A sign-in token is not an account credential.** The access token an OAuth
  flow returns is short-lived and was never issued for API use. Store it only
  if the module is going to use it as a session token; **never** persist it as
  a durable account entry. If the post-login exchange (access → API key) fails,
  report `LoginFailed` — do not fall back to storing the sign-in token, which
  expires silently and hides the real error. The exception is a vendor that
  returns *only* a plan JWT and no access token: that JWT is the issued
  credential and belongs in the store.
- **Keep the token inside `DataDir`.** `LoginProvider` writes its own state file
  in the module's data directory. Writing into a vendor CLI's credential store
  (`~/.kimi-code/credentials/`, `%APPDATA%\…`) is out of bounds: it makes the
  module a co-owner of another program's login state.
- **Scrub every message.** Poll responses echo request material back in error
  text. Pass `Message` and any error string through `core.Redact` — and, where
  the module has its own `scrubSecrets` helper, through the live session
  secrets too (zcode accumulates the poll token, then the access token).
- **Say when there is no sign-in.** A module that cannot sign in must not pretend
  it can: implement no `LoginProvider` (so the panel shows no button) and answer
  `POST /login` with a plain refusal such as
  `{"error":"<module> has no interactive login"}`. Every module here now has one —
  tabbit's is a **browser hand-off with two real paths**, because the vendor session
  can only be created by a browser and the login URL only opens inside the Tabbit
  browser: `clients/tabbit/login.go` points the operator at 导入凭据 (read the
  `token` cookie out of the running browser into a `web-token` account) and, failing
  that, at the sidecar, whose **model list** — not `/health`, which answers even when
  nobody is signed in — is the verdict. The rule is kept for the next module, and for
  a flow that can only be a CLI step: describe the step in `Status().Detail` rather
  than exposing a `StartLogin` that always fails.

Rules that apply to all optional interfaces:

1. **Implement only what you can actually do.** `core.CapabilitiesOf` derives the
   advertised capabilities from type assertions, so declaring an interface you
   cannot honour shows the operator a button that lies. Say why in `Status().Detail`
   instead — e.g. a module whose vendor offers no HTTP sign-in at all should
   describe the manual step it needs rather than exposing a `StartLogin` that
   cannot succeed.
2. **A refusal is a result, not an error.** `TestAccount` on a credential the
   upstream rejects returns `TestResult{OK: false, Error: "…"}` with a `nil`
   error. Return a Go error only for a programming mistake, such as an unknown id.
3. **Never write to the main config.** Panel-side state goes in the module's own
   `DataDir`. The `clients.<name>` block of `configs/client2api.json` is
   operator-owned and the panel must not edit it.
4. **Never return a secret.** `AccountRecord.Fields` is rendered in a browser:
   no tokens, no token prefixes, no key fingerprints that would help an attacker.
   Pass any upstream text through `core.Redact` before putting it in `Note` or
   `TestResult.Error`.
5. **A disabled account must survive a restart.** Whatever "disable" means for
   your credential model, it has to persist across process restarts — a token
   pool can carry a flag in its own state file, a file-per-account module can
   rename the file.
6. **Adding a panel surface needs no core edit.** If you find yourself editing
   `internal/` or another `clients/` package, the module boundary has been
   violated — fix the module, not the core.
7. **Say when a credential is dead instead of making the panel guess.** If the
   pool can tell that the vendor rejected the token itself — as opposed to a
   quota cooldown or an upstream 5xx — set `AccountRecord.Fields["relogin"] =
   true`. The panel reads it in `needsRelogin()` (`internal/panel/index.html`)
   and renders that row's 「重登」 button, which re-runs the vendor's own login
   in place instead of only clearing the cooldown. Without it the operator is
   left with the 「恢复」 path, which cannot bring back a credential the vendor
   has already revoked. trae is the reference implementation
   (`clients/trae/accounts.go`, `credentialDead`).

### Which optional mechanisms each module actually implements

Recorded here because "the interface exists" and "a module implements it" are
different facts, and the panel shows a button for the second one, not the first.
`✓` means a module really implements the interface (with the file that proves
it); `—` means it does not, and the row says why that is the right answer rather
than a gap waiting to be filled. See `internal/core/capabilities_test.go` and each
module's `accounts.go` for the assertions that derive `core.Capabilities`.

| Mechanism (interface) | workbuddy | trae | qwenwork | zcode | kimi | tabbit | minimaxcode |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `TaskProvider` (board + `RunTask`) | ✓ `clients/workbuddy/tasks.go:1860` | — | — | ✓ `clients/zcode/tasks.go:153` | — | — | ✓ `clients/minimaxcode/tasks.go:80` |
| `BatchPlanner` (scheduled batches) | ✓ `clients/workbuddy/batches.go` (the reference's six names) | — | — | ✓ `clients/zcode/tasks.go:67` (one `checkin` batch) | — | — | ✓ `clients/minimaxcode/tasks.go:39` (one `checkin` batch) |
| `TaskAccepter` / `BulkTaskAccepter` / `TaskClaimer` / `TaskAutoRunner` | ✓ `clients/workbuddy/panel_tasks.go` | — | — | — | — | — | — |
| `BalanceProvider` / `PackageProvider` / `VoucherProvider` | ✓ `clients/workbuddy/panel_quota.go` | — | — | ✓ balance + packages `clients/zcode/claim.go` (no vouchers) | — | — | ✓ balance + packages `clients/minimaxcode/signin.go:600`, `:642` (no vouchers) |
| `Degrader` (degraded prompt) | ✓ `clients/workbuddy/prompt.go:146` | — | — | — | — | — | — |
| `LiveReloader` (prompt, scrubber, in-flight ceilings, stickiness + session switch) | ✓ `clients/workbuddy/prompt.go:70` | ✓ `clients/trae/affinity.go:121` | ✓ `clients/qwenwork/affinity.go:171` | ✓ `clients/zcode/affinity.go:138` | ✓ `clients/kimi/affinity.go:113` | ✓ `clients/tabbit/affinity.go:271` | ✓ `clients/minimaxcode/affinity.go:177` |
| `HealthProvider` / `PoolStatsReporter` | ✓ `clients/workbuddy/health.go` | — | — | — | — | — | — |
| `HintProvider` (module-owned error advice) | ✓ `clients/workbuddy/hint.go` (11133/11135 image family + the vendor `ErrKind` table) | — | — | — | — | — | — |
| In-flight ceiling (`pool.max_in_flight*`) | ✓ honoured | — (trae has no WAF 403 family to hold back) | — | — | — | — | — |
| `CheckinProvider` | ✓ `clients/workbuddy/checkin.go:58`, `:94` | ✓ `clients/trae/checkin.go:119`, `:137` (pinned `:253`) | ✓ `clients/qwenwork/checkin.go:159`, `:452` (one action, `daily`) | ✓ `clients/zcode/claim.go` (one action, `claim`; captcha-gated) | — | — | ✓ `clients/minimaxcode/signin.go:454`, `:473` (one action, `daily-signin`) |
| `Reviver` | ✓ | ✓ | ✓ | ✓ | ✓ | — | — |
| `ConversationBinder` (affinity) | ✓ `clients/workbuddy/affinity.go:36` | ✓ `clients/trae/affinity.go:40` | ✓ `clients/qwenwork/affinity.go:88` | ✓ `clients/zcode/affinity.go:49` | ✓ `clients/kimi/affinity.go:68` | ✓ `clients/tabbit/affinity.go:133` | ✓ `clients/minimaxcode/affinity.go:49` |
| `ModelRefresher` | ✓ `workbuddy.go:413` | ✓ `trae.go:177` | ✓ `qwenwork.go:192` | ✓ `models.go:199` | ✓ `models.go:52` | ✓ `tabbit.go:99` | ✓ `minimaxcode.go:139` (re-reads the local `config.yaml`; there is no catalogue endpoint) |
| `CredentialImporter` | ✓ `accounts.go:33` | ✓ `accounts.go:41` | — | ✓ `accounts.go:46` | ✓ `accounts.go:58` | ✓ `accounts.go:46` | ✓ `accounts.go:537` |
| `BundleImporter` (uploaded export file) | ✓ `clients/workbuddy/cockpit.go` (the reference's cockpit JSON array) | — | — | — | — | — | — |
| `LoginProvider` | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ (browser hand-off) | ✓ `clients/minimaxcode/login.go` (RFC 8628 device code; the GUI is not required) |
| Static `modelmeta` table | 33 entries (27 verbatim + 6 effort-only) | — | — | — | 0 entries | 4 entries | — (catalogue comes from the desktop client's `config.yaml`) |
| Persisted pool/penalty state | `cache/pool.json` (unhealthy only) | invalid never persisted | `CooldownUntil`/`Disabled` + `state.json` | `accounts.json` fields | `health` + `enabled` maps | none | `accounts.json` (tombstones only) |

The **seven modules added later** are recorded the other way round — one row per
module — because fourteen prose columns would be unreadable. A bare name means the
interface is implemented in that file; "absent" is a design answer, not a gap,
and each module's README gives the reason.

| module | implements | deliberately absent |
| --- | --- | --- |
| `cline` | `AccountManager` `accounts.go:58`; `CredentialImporter` `:497`; `LoginProvider` `:371`; `BalanceProvider` `:299`; `Reviver` `:206`; `ModelRefresher` `models.go:534`; `ModelLimitsProvider` `models.go:625` | `TaskProvider`/`BatchPlanner`, `CheckinProvider`, `ConversationBinder` (and therefore `LiveReloader`), `HintProvider`, `Degrader`, `HealthProvider`/`PoolStatsReporter`, `PackageProvider`/`VoucherProvider`, `BundleImporter`, `RealmLoginProvider`, `CaptchaProvider` |
| `lobsterai` | `AccountManager` `accounts.go:51`; `CredentialImporter` `:332`; `BundleImporter` `:463`; `LoginProvider` `login.go:57`; `BalanceProvider` `balance.go:30`; `CheckinProvider` `checkin.go:22`; `PoolStatsReporter` `lobsterai.go:333`; `ModelRefresher` `lobsterai.go:185` | `Reviver` (a refresh here is unconditional), `ModelLimitsProvider` (the vendor publishes no output budget, and the interface forbids fetching), `TaskProvider`/`BatchPlanner`, `ConversationBinder`, `HintProvider`, `Degrader`, `HealthProvider`, `PackageProvider`/`VoucherProvider`, `RealmLoginProvider`, `CaptchaProvider` |
| `codearts` | `AccountManager` `panel.go:27`; `LoginProvider` `:412`; `CheckinProvider` `credits.go:64`; `BalanceProvider` `credits.go:360`; `Reviver` `codearts.go:176`; `HintProvider` `codearts.go:184`; `ModelRefresher` `models.go:466`; `ModelLimitsProvider` `models.go:501` | `CredentialImporter` (the IDE plugin keeps no discoverable credential file), `BundleImporter`, `RealmLoginProvider` (one realm), `CaptchaProvider` (Huawei's own portal handles the challenge), `TaskProvider`/`BatchPlanner` (the vendor has no task API), `Degrader`, `HealthProvider`/`PoolStatsReporter`, `PackageProvider`/`VoucherProvider`, `ConversationBinder` |
| `loomy` | `AccountManager` `accounts.go:454`; `Reviver` `:573`; `BalanceProvider` `quota.go:177`; `PackageProvider` `quota.go:210`; `CheckinProvider` `quota.go:263`; `TaskProvider` `onboarding.go:232`, `:310`; `ModelRefresher` `loomy.go:183` | `ModelLimitsProvider` (the catalogue carries no output-token field), `LoginProvider` (the interface cannot carry an SMS code), `CredentialImporter`/`BundleImporter`, `CaptchaProvider` (none needed), `BatchPlanner` (the onboarding board is a one-off checklist — nothing on it is a chore the scheduler could pace), `ConversationBinder`, `HintProvider`, `Degrader`, `HealthProvider`/`PoolStatsReporter`, `VoucherProvider` |
| `raccoon` | `AccountManager` `accounts.go:38`; `CredentialImporter` `:210`; `LoginProvider` `login.go:214` (loopback QR/SMS page, stdlib QR in `qr.go`); `Reviver` `:123`; `BalanceProvider` `balance.go:46`; `PackageProvider` `balance.go:67`; `CheckinProvider` `checkin.go:60`, `:155` (one action, `login-points` — the desktop login grant, which claims the daily 300 credits); `ModelRefresher` `raccoon.go:156`; `ModelLimitsProvider` `raccoon.go:285` | `RealmLoginProvider` (a single sign-in flow), `VoucherProvider` (no vendor concept), `TaskProvider`/`BatchPlanner`, `CaptchaProvider` (the SMS tab loads the vendor's Aliyun slider in the browser; the module solves nothing server-side), `HintProvider`, `BundleImporter`, `ConversationBinder`, `Degrader`, `HealthProvider`/`PoolStatsReporter` |
| `opencode` | `AccountManager` `accounts.go:119`; `CredentialImporter` `credential.go:227`, `:305`; `LoginProvider` `login.go:86`; `RealmLoginProvider` `login.go:91` (realms `free` anonymous + `oauth` device code); `ModelRefresher` `opencode.go:202`; `ModelLimitsProvider` `models.go:417`; `PoolStatsReporter` `pool.go:634` | `BalanceProvider` (**Zen exposes no balance, credit or quota endpoint at all**: `/zen/v1/key`, `/zen/v1/credits` and `/zen/v1/me` are 404 with an HTML body, and the binary contains no such path — billing is console-only, so a fabricated `0` would be worse than saying so), `Reviver`/`CaptchaProvider` (OAuth tokens are refreshed lazily before a call; there is no separate health record to revive and no captcha), `CheckinProvider`/`TaskProvider`/`BatchPlanner` (no such API), `PackageProvider`/`VoucherProvider`/`BundleImporter`, `ConversationBinder`, `HintProvider`, `Degrader`, `HealthProvider` |
| `openrouter` | `AccountManager` `accounts.go:24`; `CredentialImporter` `credential.go:177`, `:265`; `LoginProvider` `login.go:273` (browser PKCE); `BalanceProvider` `balance.go:29`; `ModelRefresher` `models.go:443`; `ModelLimitsProvider` `models.go:522`; `PoolStatsReporter` `openrouter.go:291`; `HealthProvider` `openrouter.go:299`; `LiveReloader` `openrouter.go:326` | `Reviver`/`RealmLoginProvider`/`CaptchaProvider` (a PKCE-issued key never expires on its own, and there is one realm and no captcha), `CheckinProvider`/`TaskProvider`/`BatchPlanner` (the vendor has no check-in and no task board), `VoucherProvider`/`PackageProvider` (no such concept), `ConversationBinder` (one upstream host makes stickiness meaningless), `BundleImporter`, `HintProvider`, `Degrader` |

Two facts this table makes visible that no single module README states:

1. **`core.BatchPlanner` has exactly three implementations, and the scheduler
   no longer needs one to schedule a check-in.**
   `core.PlannedBatches` gives a module that only implements
   `core.CheckinProvider` one synthetic `checkin` batch, and
   `internal/scheduler/scheduler.go` plus `internal/panel/batches.go` execute
   that batch through `CheckinProvider.Checkin`. So `workbuddy` (`batches.go`
   plans the reference's six batches, `tasks.go` runs the codes they name),
   `zcode` (one `checkin` batch over its `claim` action) and `minimaxcode`
   (one `checkin` batch over `daily-signin`) keep the task-code path, while
   `trae`, `qwenwork`, `lobsterai`, `codearts`, `loomy` and `raccoon` appear in
   the task centre under the same shared `checkin` timetable, backed by the
   check-in their account rows already offer. A module with neither capability
   still answers `[scheduler] idle: nothing scheduled`, and the six chore
   aliases in `internal/panel/batches.go` answer 501 for it. The seventh alias,
   `balance_all`, is the exception and is deliberately *not* a batch: a balance
   is not a chore any module plans, so `internal/panel/batches.go` intercepts it
   by name and hands it to `Scheduler.RunBalanceRefreshNow`, which sweeps every
   module that can report a balance regardless of which client is in the path.
   Routing it through the batch table would have made the panel's 刷新余额 button
   a guaranteed 501. Inside workbuddy the split is deliberate:
   `blackcat` publishes the real board code `black_cat`, while
   checkin/travel/activity/keepalive/growth publish synthetic chore codes that
   `RunTask` dispatches *before* it reads the board — a scheduled chore has no
   board row, and the board path would refuse it for that reason alone.
2. **The static metadata table covers three modules, not fourteen.** `trae`,
   `qwenwork`, `zcode`, `minimaxcode`, `cline`, `lobsterai`, `codearts`, `loomy`,
   `raccoon`, `openrouter` and `opencode` have no section in
   `internal/modelmeta/table.json`, so their fallback is the vendor catalogue,
   then `models.dev`, then the on-disk cache. That is deliberate — no
   authoritative source named their ids — but it means "the fallback table" is
   not uniform and must not be described as such. `minimaxcode` is the extreme
   case: the upstream has no catalogue endpoint at all, so the only source is the
   desktop client's own `config.yaml`.

Justified "no"s, in the modules' own words: qwenwork has no `CredentialImporter`
because its desktop client keeps the session inside the client's own store
(`clients/qwenwork/accounts.go:26`, pinned by `clients/qwenwork/accounts_test.go:725`);
tabbit has no `Reviver` because its `State`/`Note` are recomputed from the last
probe — the sidecar's `/health`, or the vendor catalogue for a web account — and
`config.go locate()` never consults health, so a revive button would provably do
nothing. (tabbit *does* implement `CredentialImporter`
(`clients/tabbit/accounts.go:46`): it lists the accounts it stores **and** the
browser cookie it can read out of the running Tabbit browser via the vendor's own
launcher, and it gained `LoginProvider` in `clients/tabbit/login.go`, a browser
hand-off rather than an HTTP flow.)

### A module may have more than one transport

The interface contract says nothing about *how* a module reaches its vendor, and
`clients/tabbit` is the worked example of a module that reaches it two ways:

- **web** — the vendor's own web API (`web.tabbit.com`), authenticated by the
  `token` cookie of a signed-in Tabbit browser. The protocol is documented in
  `clients/tabbit/README.md`: create a room (`POST /panel/session`), open the
  `join` SSE stream *before* submitting (`POST …/runs`), take the deltas until
  `run_completed`, drop the room. The vendor retires a room after every run, which
  is why a multi-message request is flattened into one `content` string.
- **sidecar** — the GPL `tabbit2api` bridge over HTTP, unchanged.

`transport` (`"auto"` by default) picks per request, and `"auto"` prefers web
whenever a web credential exists and has not been rejected. Three consequences are
worth copying into the next such module:

1. **A transport choice must not change what the caller sees.** Both paths produce
   the same `core.Event` sequence and the same `core.AccountRecord` shape; only the
   sidecar path has `reasoning_content`, tool calls and a usage frame, and the web
   path does not claim them.
2. **A second transport needs its own credential kind, not a flag.** `web-token`
   accounts store a cookie; `sidecar` accounts store a base URL. Adding the cookie
   to the existing kind would have made "is this row usable" undecidable.
3. **A transport that is a percentage, not a count, must not become a
   `BalanceProvider`.** The vendor reports `usage_percentage`; the panel renders
   `Balance.Credits`. Mapping one to the other would invent a unit, so the quota is
   folded into the **Test** result as a note instead
   (`clients/tabbit/web.go` `usageNote`).



Two more optional interfaces describe the *pool*, not the vendor. They exist so
that the panel can say what the process is really doing, and so that a burst does
not look like a vendor refusal:

- **`core.PoolStatsReporter.PoolStats() PoolStats`** — `in_flight`,
  `in_flight_full`, `sticky_sessions`. `in_flight` counts the live leases the
  request path takes, `in_flight_full` how many *usable* accounts are at their
  ceiling, `sticky_sessions` how many conversation bindings the module is
  holding. A module that does not implement it is reported without the key
  rather than as zeros.
- **`core.HealthProvider.Health() Health`** — `servable`, plus
  `ready`, `cooling`, `disabled`, `total`, `realms`, and a `note` naming the
  reason when `servable` is false. `servable` is deliberately stricter than "an
  account is healthy": a pool whose healthy accounts are **all** at their
  in-flight ceiling is not servable, which is what makes `/healthz` answer 503
  instead of sending the caller into the same wall.
- **`core.ErrBusy`** is the third piece: when the request path finds every usable
  account at its ceiling it returns `core.ErrBusy` rather than waiting, and the
  gateway maps it to **429 + `Retry-After: 1`**. Queueing would still deliver the
  burst the ceiling exists to prevent.
- **`error.code` is the gateway's, not the module's.** A module never decides what
  goes in that field: it returns a `*core.Failure` (or a plain error) and
  `internal/gateway/server.go`'s `errorCodeFor` names it symbolically
  (`waf_ip_blocked`, `rate_limit_exceeded`, `quota_exhausted`,
  `account_auth_failed`, `session_dead`, `content_blocked`, `upstream_error`, …),
  exactly as the reference does at its `writeOpenAIError` call sites. A vendor's
  numeric business code (`4008`, `14018`, `11102`) is **never** promoted to
  `error.code`; it stays inside `message`, which is the only place the reference
  keeps it. That is why no module needs a code field on `core.Failure`.
- **`pool.max_in_flight` / `pool.max_in_flight_global`** arrive through
  `core.LiveSettings` (pointers: `nil` means "the file said nothing", so a
  partial reload does not clear a ceiling). `0` for the per-account value means
  "no ceiling"; the global value falls back to the per-account one when it is
  unset, so "unset" and "allow nothing" are not the same value.
  `session_sticky.enabled` travels as a `*bool` and is a **switch, not a
  duration**: a non-nil `false` stops binding conversations, while the TTL pair
  (`ttl` / `gc_interval`) keeps its own `0` = "not mentioned" convention and a
  module then keeps the window it was built with.
- **`core.PoolTuning`** carries the pool policy knobs —
  `breaker_threshold`/`breaker_cooldown`/`breaker_cooldown_max`,
  `degrade_threshold`/`degrade_cooldown`/`degrade_cooldown_max`,
  `soft_rate_max`, `idle_weight_per_hour`/`idle_weight_max`, `prefer_expiring`
  and `cost_explore_interval`. Same pointer convention as the ceilings, with one
  deliberate exception: `cost_explore_interval` treats an explicit **`0` as a
  value** ("stop exploring"), not as "unmentioned", because there is no other way
  to switch exploration off.
  A module is free to implement any subset; the ones it ignores simply keep
  their built-in behaviour, and `core.ApplyLive` reports how many modules it
  reached rather than failing when none does.
- **Penalties are an or-gate, not a sum.** A module that implements the breaker
  and the degrade counter keeps three independent deadlines — the ordinary
  classification cooldown, the breaker's exponential backoff and the degrade
  penalty — and refuses an account while **any** of them is still in the future.
  The effective wait is therefore the *latest* of them, which is what makes
  "the longest wins, never added together" structural rather than arithmetic.
  The two counters are fed by disjoint failure sets: the breaker takes
  account-level evidence (token refresh failures that are not session-dead, and
  5xx), the degrade counter takes failures that say nothing about the account
  (transport errors and unrecognised 4xx). A single failure never feeds both.
- **Credits feed back into the pool.** `core.BalanceProvider.AccountBalance`
  returns the remaining credits *and*, when `soon > 0`, the part expiring inside
  that window plus the earliest expiry among live batches. A module that reads
  the balance is expected to write it back into its own picker (so an account
  parked for exhaustion is unfrozen once credits return, and so
  `prefer_expiring` has something to sort on) rather than only reporting it to
  the panel. Unfreezing on balance is deliberately narrow: it clears an
  exhaustion park and nothing else, because soft-rate, WAF and session-dead
  parks each have their own recovery evidence and clearing them on a balance
  refresh would compress their real lifetime down to the refresh period.
- **`core.BundleImporter`** is the operator-supplied counterpart to
  `core.CredentialImporter`: `Import` walks files the module found on this
  machine, `ImportBundle` accepts a document the caller uploads. Per-record
  failures go into `BundleImportReport.Errors` rather than into the returned
  error — one unusable row out of a hundred must not lose the other ninety-nine
  — and the returned error means only that the document could not be read at
  all. `Capabilities.Bundle` lights the panel's upload control.
- **`redis_mode`** is reported as `"noop"`: the no-op store is a supported
  single-machine configuration, not a missing feature. There is no
  `internal/redisstore` package in this tree — nothing here depends on one.

Only workbuddy applies the ceilings (`clients/workbuddy/pool.go:435-582`,
`clients/workbuddy/health.go`); every module implements `LiveReloader` and so
applies the prompt, scrubber and stickiness window — see the capability table in
section 5 for the per-module file.

## 7. Adding or removing a client

1. Create/delete `clients/<name>/`.
2. Add/remove the blank import line in `clients/all/all.go`.
3. Nothing else. No core edit, no other module edit.

If step 3 is ever required, the module boundary has been violated — fix the
module, not the core.
