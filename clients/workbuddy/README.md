# clients/workbuddy — WorkBuddy (Tencent CodeBuddy)

`workbuddy` serves Tencent's **WorkBuddy / CodeBuddy** coding-agent models through
the gateway's OpenAI surface. Route models as `workbuddy/<model>` (the gateway
strips the prefix before `Chat` sees it).

The client speaks the vendor's `/v2/chat/completions` endpoint, which is
**stream-only**: every request is forced to `stream: true` and the SSE stream is
re-emitted as `core.Event`s.

## Two realms

WorkBuddy runs two independent deployments. An account belongs to exactly one of
them, and the realm decides the host it talks to:

| realm    | chat host                  | console/billing host   |
|----------|----------------------------|------------------------|
| `cn`     | `https://copilot.tencent.com` | `https://www.codebuddy.cn` |
| `global` | `https://www.workbuddy.ai`    | `https://www.workbuddy.ai` |

Both realms use the same paths (`/v2/chat/completions`,
`/v2/plugin/auth/token/refresh`); only the host, the `Origin`/`Referer` pair, the
`Accept-Language`, the enterprise headers and the User-Agent product segment
differ. The realm is stored per credential (`"realm"`, or inferred from the
token `domain`). Nothing is hardcoded to one realm.

## Files

| file | contents |
|------|----------|
| `workbuddy.go` | module wiring: `init`/`New`/`Name`/`Models`/`Chat`/`Status`, config schema, model cache, retry policy, `core.Event` emitter |
| `auth.go` | credential document, load/save (0600, atomic), realm helpers, vendor-store discovery |
| `headers.go` | the vendor's header set (common, chat, refresh, billing), message/trace ids |
| `body.go` | outbound body builder (the 10-step rewrite pipeline) + prompt-cache-key injection |
| `sse.go` | SSE frame reader, frame normaliser, whole-stream aggregation, usage aliasing |
| `dsml.go` | streaming parser for tool calls the model writes as DSML/XML inside the text |
| `upstream.go` | transport, error classification (`ErrKind`), auth refresh, model discovery |
| `pool.go` | account pool: health states, rotation, cooldowns |
| `poolstate.go` | the account pool's health on disk, restored at startup (secret-free) |
| `tasks.go` | the growth-centre task board: `core.TaskProvider` (`Tasks` / `RunTask`), the list/accept/claim endpoints, the report channel, the desktop fingerprint, and the ported runners |
| `sms.go` | the optional one-time-SMS platform: `core.SMSProvider` (`SMSStatus` / `AcquirePhone` / `PollSMSCode` / `ReleasePhone`), the eomsg client, the pool-duplicate guard, province rotation and code extraction |
| `workbuddy_test.go` | offline unit tests |
| `tasks_test.go` | offline unit tests for the task board |
| `workbuddy_live_test.go` | live checks, skipped unless `CLIENT2API_LIVE=1` |
| `poolstate_test.go` | offline unit tests for the persisted pool health |

The pool's health is kept in `<data_dir>/cache/pool.json` — the vendor's verdict
on each account plus the deadline that came with it, never a token. A restart
therefore does not replay a request the vendor just refused: a 12 h
`accountFault` park or a 6 h credit park survives it. Only accounts that are
**not** healthy are recorded, so a start with nothing wrong writes no file at
all. It is deliberately **not** written beside the credentials, because the
credential loader globs `<accounts_dir>/*.json` and would parse the state file as
an account.
Unlike `clients/trae`, `invalid` **is** persisted here: in this module it carries
a one-hour deadline that `usable()` honours, so the record cannot outlive an
hour, whereas trae's `invalid` is a permanent park.

## Configuration

Everything is optional; with no config at all the module discovers credentials in
its own data directory and uses the vendor defaults. The object below goes under
`"clients"."workbuddy"` in the main config file (see `config.example.json`).

| key | type | default | meaning |
|-----|------|---------|---------|
| `accounts_dir` | string | the module's `DataDir` (`data/workbuddy/`) | where credential JSON files live |
| `chat_base_cn` | string | `https://copilot.tencent.com` | cn realm chat host (override) |
| `chat_base_global` | string | `https://www.workbuddy.ai` | global realm chat host (override) |
| `billing_base_cn` | string | `https://www.codebuddy.cn` | cn billing/console host |
| `billing_base_global` | string | `https://www.workbuddy.ai` | global realm billing/console host (override, independent of the chat host; the register wizard rides it too) |
| `web_base_cn` | string | `https://www.workbuddy.cn` | cn web host |
| `global_enabled` | bool | `true` | route global-realm accounts to the global host |
| `client_version` | string | `5.5.4` | `X-Client-Version` sent upstream |
| `cli_version` | string | `2.137.1` | CLI version advertised in headers |
| `user_agent` | string | derived from realm | full User-Agent override |
| `client_name` | string | `WorkBuddy` | attribution name (`X-IDE-Name` etc.) |
| `passthrough_ip` | bool | `false` | forward the caller's IP (`X-Forwarded-For`, `X-Real-IP`, `X-Client-IP`) |
| `device_token` | string | — | static device token |
| `device_token_file` | string | — | file to read a device token from |
| `timeout_seconds` | int | `120` | ceiling on a short RPC (token refresh, check-in, balance, model discovery); absent or non-positive means the default |
| `header_timeout_seconds` | int | falls back to `timeout_seconds` | how long a chat attempt waits for the first response byte before the transport gives up and the gateway rotates; absent or non-positive follows `timeout_seconds` |
| `idle_timeout_seconds` | int | `300` | abort a stream that goes quiet for this long (an explicit `0` disables the monitor) |
| `max_attempts` | int | `3` | how many accounts one `Chat` may try |
| `refresh_window_seconds` | int | `300` | refresh a token this long before it expires |
| `sanitize_fingerprints` | bool | `true` | scrub client-identifying text out of message bodies before they are sent |
| `models` | []string | — (no built-in list) | operator override: ids served only when discovery has never succeeded |
| `sms_token` | string | — (falls back to `EOMSG_TOKEN`) | one-time-SMS platform credential (eomsg). Empty means the panel's 接码 controls report "not configured" |
| `sms_keyword` | string | `腾讯科技` | SMS sender keyword the platform filters on |
| `sms_base` | string | `https://api.eomsg.com/zc/data.php` | platform endpoint (override) |
| `sms_provinces` | []string | the built-in 31-province rotation | replace the rotation pool; `["none"]` disables it and lets the platform choose |
| `browser_path` | string | — (auto-detected) | Chromium-family executable the one-click 自动添加 flow drives. Empty finds an installed Edge or Chrome |
| `browser_headless` | bool | `true` | run that browser without a window; `false` shows it, which is easier to debug but needs a desktop session |
| `auto_login_timeout_seconds` | int | `300` | ceiling on one 自动添加 run, end to end (wb-auto's `--timeout` default) |
| `sms_polls` | int | `12` | how many times one rented number is checked for its SMS before the run gives up on it |
| `sms_interval_seconds` | int | `5` | pause between two SMS polls |
| `dup_retries` | int | `6` | extra numbers to draw when the platform keeps returning one already in the pool |

### System prompt

This is the one module that acts on the gateway-wide `prompt` section, because
workbuddy is the one vendor whose content filter is known to reject the client's
own system template verbatim (upstream issue #36 / PR39). The other modules leave
their vendor's system prompt alone: replacing it with this module's shipped
engineering prompt would change how those clients behave upstream, which is a
worse failure than the risk it removes.

The section is hot — `ApplyLive` re-reads it on every config reload — and it is
the gateway's, not this module's, so it is documented in the root `README.md`:

| mode | effect |
|------|--------|
| `passthrough` (default) | the client's own system turns are sent unchanged |
| `custom` | every `system`/`developer` turn is deleted and replaced with `prompt.file` (or the shipped default prompt when `file` is empty) |
| `append` | the operator's prompt is inserted after the **leading run** of `system`/`developer` turns, which are kept |

Two pieces of behaviour are worth knowing before turning this on:

- **The stage runs once per request, before the account-rotation loop.** `append`
  is not idempotent, so running it per attempt would stack a second copy of the
  operator's prompt on the first. This is why it lives in `Client.Chat` and not
  in the per-attempt `Upstream.prepareBody`.
- **A content-policy refusal opens a degraded window until the next 00:00 CST**
  (the reference's reset point), during which *every* request is served the
  minimal `internal/prompt.Degraded` prompt rather than the operator's text.
  `append` collapses to replace inside the window on purpose: retrying with the
  client's own template is a deterministic way to be refused again. A `custom`
  prompt is never degraded — the operator chose it explicitly.

### Transport

This module talks to the vendor through the transport built by `newTransport`
(`upstream.go:63-77`), which is a line-for-line port of the reference
`internal/upstream/transport.go:74-88` and keeps HTTP/2 **off** on purpose: a
non-nil but *empty* `TLSNextProto` map is the only reliable way to do it,
because with a custom `DialContext`, `ForceAttemptHTTP2=false` still negotiates
h2 through ALPN and produces `http2: timeout awaiting response headers`.

There is deliberately **no** TLS-fingerprint key here.  The reference does not
impersonate the official client's ClientHello — it hardens the connection
instead (`dial-timeout` / `keepalive` / `TLSHandshakeTimeout` / short idle pool
/ `ResponseHeaderTimeout`) — and this module follows the reference rather than
inventing a divergence.  The vendor-facing disguise lives one layer up, in the
headers: see `headers.go` and `### More than one User-Agent, on purpose`.

One of those knobs is configurable rather than constant, again following the
reference: `ResponseHeaderTimeout` is `header_timeout_seconds`, which falls back
to `timeout_seconds` (see `Upstream.SetTimeouts`).  The constant in
`transport.go` is only the constructor default for a caller that never wired a
config file, so the deadline that governs a configured deployment comes from the
config, not from the constant.  `timeout_seconds` likewise bounds the
control-plane client, and `idle_timeout_seconds` bounds a *running* stream's
silence through `monitorBody` — the two cover different waits, which is why the
chat client itself carries no whole-request deadline.

## Credentials

One JSON file per account in `accounts_dir`, mode 0600, written atomically
(tmp + rename). The shape the module writes (`auth.go`) is nested:

```json
{
  "auth": {
    "accessToken": "…",
    "refreshToken": "…",
    "expiresAt": 1795348559,
    "domain": "copilot.tencent.com",
    "realm": "cn"
  },
  "account": { "uid": "…", "enterpriseId": "…", "nickname": "…" }
}
```

A flat document with the same keys is also accepted, so a credential exported by
another tool can usually be dropped in unchanged. Unparseable files are skipped,
never fatal; accounts are de-duplicated by `uid`.

### Discovery

`LoadAccounts` only reads **plaintext** JSON from `accounts_dir` — never from
another module's data directory. The vendor's own stores are inspected for
*existence only* and their paths are reported in `Status().Detail` when no
credential is loaded:

* `%LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth\workbuddy-desktop.info`
* `%LOCALAPPDATA%\WorkBuddy`, `%APPDATA%\WorkBuddy`, `~/.workbuddy`, `~/.codebuddy`

**On this machine that vendor store exists but is unusable**: its
`accessToken`/`refreshToken` are sealed in Tencent `$wbEncrypted` envelopes whose
wrapping key lives in `~/.workbuddy/keyblob` (`static-v1` protector), i.e. inside
the minified desktop bundle. The module therefore reports the store's presence
instead of guessing at a decryption, and no live end-to-end call is possible from
this machine alone.

To get a usable credential, obtain a token the way the vendor CLI does (this is
**not** on the request path; it is documented for completeness):

1. `GET https://<realm host>/v2/plugin/auth/state?platform=CLI`
2. `GET https://<realm host>/v2/plugin/auth/token?state=<state>`
3. `GET https://<realm host>/v2/plugin/login/account?state=<state>`

then write the tokens into a file in `accounts_dir` in the shape above. An expired
access token is refreshed automatically via
`POST /v2/plugin/auth/token/refresh` (realm host) using the refresh token.

### Importing an export file

Discovery walks what is already on this machine. The other way in is
`ImportBundle` (`cockpit.go`), which accepts a document the operator uploads
through the panel's 「上传导出文件」 control
(`POST /panel/api/clients/workbuddy/import/bundle`, raw body or multipart with a
`file` field) and implements `core.BundleImporter` — hence the separate
`capabilities.import_bundle` flag.

The accepted document is the cockpit export's JSON **array** of accounts, the same
shape the reference's `POST /panel/api/import/cockpit` took:

```json
[{ "id": "…", "uid": "…", "nickname": "…", "access_token": "…",
   "refresh_token": "…", "token_type": "Bearer", "expires_at": 1795348559000,
   "domain": "copilot.tencent.com", "email": "…", "status": "1",
   "payment_type": "0", "checkin_streak": 3 }]
```

Field semantics worth stating because they are easy to get wrong:

* `expires_at` is **milliseconds** in this format (the module's own files store
  seconds), so it is converted on the way in. `0` or absent means "no expiry
  stated", and the module then assumes a bounded default lifetime rather than an
  already-dead token.
* `status` and `payment_type` are **strings** in the export, not numbers.
* `realm` is not in the export: it is derived from `domain` the same way
  `auth.go` derives it everywhere else, so an export carrying
  `workbuddy.ai` lands on the international realm.
* `nickname` falls back to `email` when the export omits it.

Rows are applied one at a time: a row with no uid, no access token, or a uid that
cannot safely become a filename is **skipped with a reason** and the remaining
rows still land, so one bad line in a hundred-row export costs one account rather
than the whole file. The reply reports `total` / `imported` / `skipped` plus the
per-row reasons. Only a document that cannot be read as a list at all fails the
call as a whole. Importing the same uid twice overwrites it in place rather than
duplicating it, and each imported account's balance is read back once so the pool
learns its credits immediately.

## Status fields

### One-click adding (自动添加)

`Capabilities.AutoLogin` is true: the module implements `core.AutoLoginProvider`, so
the panel's 添加账号 → 浏览器登录 tab shows a 「一键自动添加」 button for workbuddy.
One click runs the whole flow `wb-auto/wb_add_account.py` runs: ask the vendor for
this run's authorisation URL, launch a throwaway Chromium profile, rent a number
from the SMS platform (skipping any number the pool already holds), open the
authorisation page, fill the phone, tick the agreement, request and read the SMS
code, submit, then poll this module's own login session until it stores the
credential. Progress streams back through `POST` / `GET` / `DELETE`
`/panel/api/clients/workbuddy/auto-login[/<id>]`, and the panel renders it as a
log. The same block serves the 恢复 flow: it pins the account's own number
instead of renting a new one. Relevant config: `browser_path`,
`browser_headless`, `auto_login_timeout_seconds`, `sms_polls`,
`sms_interval_seconds`, `dup_retries`.

The panel log is the operator's only window into a run that has no visible
browser, so a job's **last** line is always its outcome — `登录成功，已添加账号 <id>`
on success, or `失败：<原因>` on failure — never a trailing-off step. When
`browser_headless` is true (the default) the `浏览器已启动` line says the window
is suppressed on purpose. A selector failure names the page it was looking at
(title and URL), which is what tells "the page never loaded" apart from "the
vendor redesigned the page" without a screenshot.

## Status fields

`Status()` is cheap: no network call, never blocks, safe to poll every 10 s.

| field | meaning |
|-------|---------|
| `name` | `"workbuddy"` |
| `ready` | true only when at least one account is currently usable |
| `detail` | one human line: account/realm summary, or why nothing is ready (including the vendor-store paths when no credential was found) |
| `accounts` | one `AccountStatus` per credential |
| `models` | cached upstream ids, else the configured list, else empty |
| `updated_at` | when the snapshot was taken |

`AccountStatus`: `id` (uid or file name), `label` (nickname/uid), `enabled`
(usable right now), `state` (`ready` / `cooling` / `exhausted` / `invalid` /
`unknown`), `expires_at` (RFC 3339), `note` (cooldown reason / remaining time),
`extra.realm`, `extra.failures`.

### Low-balance guard

`platforms.workbuddy.reserve_credits` (see the root README) parks an account
whose last *known* balance is at or below the threshold.  Only a reading the
vendor's own reply corroborates can park it.  When the reply contradicts
itself -- the 体验版 package keeps a stale lifetime `CapacityRemain` after
its monthly cycle is spent -- a positive remainder is still recorded as
usable evidence, while a contradictory zero is resolved automatically with
the same real Auto model call as the panel's Test button.  Success marks the
account usable; failure applies the normal cooling, credit or risk verdict.
The decision is reused for six hours per account so a background balance
sweep does not turn every pass into another model request.

## Request path

1. `Chat` picks a usable account (round-robin, skipping ones already tried in
   this request) and pre-refreshes the token if it expires within
   `refresh_window_seconds`.  The pick is model-aware: an account the vendor
   has parked for that model is never a candidate, not even as a last resort,
   because handing it back would spend a call re-asking the question the
   vendor's own `11102`/`6004` already answered -- and a `6004` park
   re-recorded from "now" slides its reset window forward.  When nothing can
   take the model the module answers `core.ErrPlatformExhausted`, which the
   gateway turns into a failover rather than a failure against an account.
2. The body is rewritten in the reference's exact order: `stream: true`;
   `max_completion_tokens` → `max_tokens`; `stream_options.include_usage`;
   `tool_choice` flattened to the string form the vendor accepts; `developer` →
   `system`; string `image_url` → object form; broken tool-call/tool-result
   pairing repaired; thinking/`reasoning_effort` injected and snapped to a
   supported level; `reasoning_content` backfilled; then a prompt-cache key is
   injected (`wb2a-<uid8>-<sha256(uid|conversation)[:16]>`, account-salted so two
   accounts can never share a cached prefix).
3. The SSE stream is normalised and re-emitted as `core.Event`s: text and
   reasoning as `EventDelta`, native `delta.tool_calls` fragments as
   `EventToolCall`, tool calls the model writes as DSML inside the text parsed out
   of the text and re-emitted as `EventToolCall` (native and parsed calls get
   disjoint indexes), then exactly one `EventUsage` and one `EventDone`.
4. On failure the account is cooled: 401/403 → refresh then one retry; 429/5xx →
   short cooldown (honouring `Retry-After`); quota/credit exhaustion → long
   cooldown; the request then moves to the next account, up to `max_attempts`.
   A 400-class error is returned to the caller as-is (502 by the gateway) because
   retrying cannot help.

### More than one User-Agent, on purpose

This module sends several different `User-Agent` strings, and that is a faithful
port rather than an oversight: each one belongs to a different upstream surface
that the vendor's own clients label differently. The normal API path derives its
UA from `user_agent` (realm-dependent, CN `WorkBuddy/… WorkBuddy/… CLI/…`, intl
`… WorkBuddy AI/…`). The desktop-event fingerprint path pins
`WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1` because the reference's
`internal/upstream/desktop.go` pins exactly that string, and the CodeBuddy IDE
and CLI surfaces announce `CodeBuddyIDE/<v>` and `CLI/<v>` respectively. Do not
"unify" these without a live measurement: collapsing them to one value would
make the desktop/IDE/CLI event paths stop looking like the clients that are
supposed to be reporting them.

Note also that workbuddy is the only module that deliberately turns HTTP/2 off
(`upstream.go`, a non-nil empty `TLSNextProto` map). A custom `DialContext` plus
`ForceAttemptHTTP2 = false` still negotiates h2 over ALPN and then times out
waiting for response headers, so the map is the fix. A few older call sites still
fall back to `http.DefaultClient` and therefore lose that hardening; they are
listed in `## Known gaps`.

## Task board

This module implements `core.TaskProvider`, so the panel's **任务中心 → 任务看板**
renders WorkBuddy's growth-centre chores with live progress and one run button per
automatable row. The flag is derived by `core.CapabilitiesOf` through a type
assertion (`capabilities.tasks`); a module that does not implement the interface
is untouched by any of this.

| method | behaviour |
|--------|-----------|
| `Tasks(ctx, accountID)` | the merged chore list: the default growth list plus the miniprogram list (`X-Client-Platform: miniprogram`), de-duplicated by `task_code`. An empty `accountID` means "any usable account". Cached briefly — the panel calls it on every refresh — and returns an empty slice with a **nil** error when there is no credential. |
| `RunTask(ctx, accountID, code)` | dispatches one chore to its runner, then claims the reward. |

Error policy, matching `Checkin`:

* an unknown code, an unknown account, or no usable account → a Go **error**
  (the run could not be attempted);
* a vendor refusal, a transport failure, or a chore this account does not have →
  `core.TaskResult{OK: false, Error: …}`, a **result** that the panel journals
  and displays;
* already claimed → `OK: true`, an idempotent no-op.

### Async scoring

The vendor scores by events, not by API. A runner reports an event (for example
`chat_request_send` on `POST /v2/report`) and the board's `current`/`target` only
ticks several seconds later. A runner that read the board immediately after
reporting would see `0/1`, conclude it failed, and skip the claim — so `RunTask`
polls the task briefly before deciding.

### Cadence is part of the protocol

Reporting the same event several times seconds apart is accepted at first, briefly
shows `completed`, and is then **rolled back** by the vendor's anti-cheat, after
which `claim` answers `task not completed`. Only spaced-out events survive. The
ported runners therefore insert a deliberate delay between reports (the reference
implementation used 45 s ± 10 s of jitter), and the panel's "运行全部可自动任务"
queue runs **serially** for the same reason. If you are tempted to speed this up,
expect the rewards to disappear.

### Runners

`taskRunners` in `tasks.go` is the dispatch table, in dependency order (for
example `first_buddy` must not be attempted before `chat_5` has been credited).
`mp: true` marks the ones that live on the miniprogram channel.

| code | what the runner does |
|------|----------------------|
| `chat_5` | five CLI chat-activity reports |
| `first_buddy` | reports the prerequisite event, then adopts the buddy |
| `Model_chat_GLM5.2` | one real GLM-5.2 chat plus its report, with the model fields aligned |
| `RichMeow_Chat` | the desktop chat event sequence (needs a success receipt) |
| `Buddy_App`, `Buddy_App_QQ` | the buddyapp desktop event sequence |
| `automation_1` | a desktop automation-create event |
| `Library_read` | a web `web_element_click` on a 资料库 document |
| `template_5` | five desktop template-use sequences |
| `playbook_prompt` | a desktop playbook-prompt sequence |
| `create_canvas` | a desktop design-canvas sequence |
| `expert_5` / `Expert_team_use_3` | summon-and-really-use chains against the live expert market (five single experts, three teams) |
| `expert_actual_use` | one chain, for the chore that only needs the expert actually used once |
| `Hp_Appearance` | applies a theme through `/v2/user-asset/appearance/set` (an API, not an event) |
| `black_cat` | night chats; only meaningful between 23:00 and 08:00 |
| `school_season` | miniprogram school-season chat events |
| `Sequential_Tasks_1` / `_3` / `_6` | the miniprogram sequential chat tasks |
| `Sequential_Tasks_2` / `_5` / `_7` | the miniprogram expert, GLM-5.2 and playbook tasks |
| `Sequential_Tasks_4` | a miniprogram scheduled-task creation; its criterion is provisional |

Chores with no ported runner are still listed, but with `Auto: false` and a note
saying they must be completed in the client, rather than a button that cannot
work. Anything that would need a real connector authorisation, a real donation,
or a real Skill tool call is deliberately left in that state.

### Expert chains

`expert_5` and `Expert_team_use_3` score per expert the vendor can see was really
used, so the runner walks the **live** market (`MarketExpertList`) and completes a
full chain per entry: the summon sequence, a real chat whose `requestId` the
server issues, and the actual-use event carrying that id. An id invented locally,
or a `requestId` the runner made up, produces a well-formed event the vendor does
not count — which is why the runner cannot be reduced to a single report.

A chain reports ten desktop events (three summon + six chat + one actual-use) and
the runner waits `expertSummonGap` (6 s) between experts: five chains back to back
look like automation rather than use, and the vendor rolls those back. One expert
failing does not fail the chore — the rest of the market is still walked, and the
note reports how many chains completed. `RunTask` re-reads the board afterwards
and claims whatever the vendor did credit.

`Sequential_Tasks_4` (miniprogram scheduled-task creation) reuses the same
`desktopAutomationCreateEvent()` body as `automation_1`. The miniprogram bundle
has no automation emit point of its own, so the criterion is **provisional**: the
chore points at the PC-side event the reference measured on three accounts, and
whether the board credits it is the vendor's decision.

## Tests

```sh
go vet ./clients/workbuddy/...
go test ./clients/workbuddy/... -timeout 90s
```

Offline, no network, no credentials: **159 tests pass, 0 fail, 2 skip**, including
inline upstream fixtures, a fake `http.RoundTripper`, and credentials in temp
dirs. Of those, 22 cover the task board (`Tasks`/`RunTask`): board merging and
de-duplication, the `progress` shapes, an empty board with no credential, the
refusal-is-a-result policy, the anti-abuse gap, the miniprogram accept/verify
path, and the desktop event-sequence shapes. Always pass `-timeout` so a hang
fails fast.

Live checks are in `workbuddy_live_test.go` and skip unless
`CLIENT2API_LIVE=1` **and** a loadable plaintext credential exists
(`WORKBUDDY_LIVE_ACCOUNTS` overrides the directory; `WORKBUDDY_LIVE_MODEL`
overrides the model id). They are skipped on this machine — see
"Credential discovery" above.

## Known gaps

* **Fingerprint sanitisation is ported, but mirror-only.** The reference's
  `internal/upstream/sanitize.go` is reproduced here byte-for-byte (only the
  package clause changed) and wired into the last step of the body pipeline;
  `sanitize_fingerprints` defaults to **true**, exactly as the panel does. Its
  rewrite pairs are calibrated against the vendor's current rejection list, and
  that list is not observable from outside — if the vendor tightens it, our copy
  goes stale silently and nothing on the wire says so. The 2026-09 detection
  mechanism (a bare numeric error code and the Claude-Code/Codex identity
  sentences in the system prompt) is what the pairs target.
* **The vendor's live credential store cannot be imported** (encrypted
  `$wbEncrypted` envelopes), so `Status()` reports its presence instead of
  importing it. See "Credential discovery".
* **Model promotion schedules are evaluated against a hard-coded UTC+8 zone.**
  `/v3/config` promotions are applied only while their `schedule` says they are in
  force: validity range plus the daily windows, including windows that wrap past
  midnight. `promo.go` pins `time.FixedZone("CST", 8*3600)` rather than reading
  the `timezone` field, because the upstream always quotes Asia/Shanghai and
  Windows ships no IANA database for `time.LoadLocation`. If the vendor ever runs
  a campaign in another zone, the window will be evaluated eight hours off.
  The campaign list itself is read from either envelope — a bare array, which is
  what the wire sends, or an object keyed by campaign id — because a wrong guess
  would fail the whole `/v3/config` parse and take the catalogue down with it.
  On an equal `priority` the earlier campaign wins, matching the reference. Live
  examples: `glm-5.2` carries `夜间折扣` at `0.50x` between 23:00 and 08:00,
  `hy4-preview` is `夜间免费` for the same window, and `deepseek-v4-flash` /
  `deepseek-v4-pro` carry the badge-only `错峰使用` campaign, which has no
  discount object and so leaves `promo_factor` unset.
* **The DSML parser is a re-implementation.** No Go reference exists (the
  original was in an absent Python monolith), so `dsml.go` is written from the
  documented behaviour: tolerant, streaming, full-width-bar `｜DSML｜`/`antml:`
  prefixes, XML `<invoke>`/`<parameter>` and JSON payloads, unterminated blocks
  recovered at end of stream, truncated calls dropped.
* **Check-in and the scheduled chores are implemented, on demand and on a timer.**
  `Capabilities.Checkin` is true: the CN realm posts the billing check-in
  (`checkinCN`) and the international realm creates the console conversation it
  counts as a day of activity (`dailyActivity`), so the panel draws a 签到 button
  per account. The reference's `internal/scheduler/` is not compiled here (it
  couples to concrete `*auth.Auth` / `*upstream.Client` / `*pool.Pool`), but its
  six daily chores are declared in `batches.go` and run through `core.BatchPlanner`
  when the `schedule` block enables them — daily check-in, cat travel, activity
  report, token keepalive, the black-cat window and the growth scan. Five of the
  six publish synthetic chore codes that `RunTask` dispatches **before** it reads
  the growth board, because a scheduled chore has no board row and the board path
  would refuse it; `blackcat` publishes the real board code `black_cat`, whose
  runner already enforces the 23:00–08:00 counting window. Two reference
  behaviours are kept deliberately: the scheduled check-in and activity report
  spend **no** upstream call on a `global`-realm account (that realm credits
  neither one), and the growth batch scans the board when it runs instead of
  publishing a frozen code list, because the vendor unlocks the `Sequential` family
  one ring per day. The growth-centre **task board** itself is also implemented, on
  demand; see "Task board".
* **The in-flight ceiling is enforced, and the pool reports its own health.**
  `pool.max_in_flight` (default 3) caps concurrent upstream requests per account;
  `pool.max_in_flight_global` (default 2) tightens that for `global`-realm
  accounts, which is where the reference only ever saw its WAF 403s. `0` means
  "no ceiling" for the per-account value, while the global one falls back to it
  when unset, so "unset" and "allow nothing" are different values. An account at
  its ceiling is **skipped by the picker** (`Pick`, `PickForModel`, `Find`) rather
  than queued for; a slot is held for as long as the response body is open and
  released exactly once when it closes (`leasedStream`), so a client that
  abandons a stream cannot leak a slot. When every usable account is at its
  ceiling `Chat` answers `core.ErrBusy`, which the gateway reports as 429 with
  `Retry-After: 1` — queueing would still deliver the burst the ceiling exists to
  prevent. `Health()` (in `health.go`) reports `servable`, deliberately false when
  every healthy account is at its ceiling, which is what makes `/healthz` answer
  503 instead of sending the caller into the same wall; `PoolStats()` reports
  `in_flight`, `in_flight_full` and `sticky_sessions` for the panel. Both are
  optional core interfaces, and `session_sticky.ttl` / `gc_interval` reach the
  affinity table through the same `ApplyLive` push as the ceilings.
* **The model catalogue is pure-dynamic, like the reference.** There is no
  built-in list: ids come from `/console/enterprises/personal/models` and
  `/v3/config`, cached for 10 minutes. A failed fetch answers an empty catalogue
  (never a guess — a stale id only makes the client pick a model that answers
  `11102`), and a failure arms a 5-minute negative cache so a client polling
  `/v1/models` through an outage cannot turn it into a flood. That failure is
  deliberately *not* reported to the account pool: a models-endpoint hiccup says
  nothing about chat health, and feeding it to the breaker would penalise
  accounts whose chat path is fine. The only non-discovered ids ever served are
  the ones the operator writes in the `models` config key, and `RefreshModels`
  ignores the negative cache so the panel button always performs a real probe.
* **Non-streaming is emulated by the gateway**, not by this client: the vendor
  endpoint rejects `stream: false`, so a non-streaming request is answered by
  draining the stream.
* **A few call sites still use `http.DefaultClient`.** The dedicated transport
  (custom `DialContext`, HTTP/2 disabled) is what every normal request goes
  through, but the fallback paths at `upstream.go:598`, `upstream.go:727`,
  `upstream.go:1082`, `upstream.go:1206`, `checkin.go:265` and `tasks.go:545`
  pick up `http.DefaultClient` when no transport was injected, so those calls
  lose the h2-off hardening. Nothing has been observed failing because of it —
  it is an inconsistency, not a known break — so it is recorded rather than
  rushed; replacing each with the client's own transport is the fix.

## Provenance and licence

The upstream protocol layer is ported from **`workbuddy2api-panel`**
(`github.com/linguo2625469/workbuddy2api-panel`, Go, **MIT**), which is why the
header set, body pipeline, error classification and model discovery match the
vendor CLI closely. Ported: `internal/upstream/{payload,cache_key,transport,
usage,sse,tool_pairing,thinking,sanitize}.go`, the error/classify/refresh/
chat/models parts of `internal/upstream/client.go`, `internal/auth/auth.go`,
and `internal/upstream/desktop.go` (the event-side fingerprint fields). Not
ported: `internal/server/handler.go`, `internal/panel/`,
`internal/scheduler/` (they couple to concrete `*auth.Auth` /
`*upstream.Client` / `*pool.Pool`; the six chores that scheduler runs are
re-expressed as `core.BatchPlanner` in `batches.go`, so the behaviour is ported
even though those files are not). `dsml.go` has no reference and is original
work.
Wiring (`workbuddy.go`, `pool.go`, the `core.Event` emitter) is original.

```
MIT License — Copyright (c) the workbuddy2api-panel authors.
Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
the Software, and to permit persons to whom the Software is furnished to do so,
subject to the following conditions: the above copyright notice and this
permission notice shall be included in all copies or substantial portions of the
Software. THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
EXPRESS OR IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF
MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO
EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR
OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING
FROM, OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
```

No third-party Go modules are used: the module is stdlib-only.
