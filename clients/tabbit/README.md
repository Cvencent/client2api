# clients/tabbit — two transports for the Tabbit web session

This module exposes Tabbit's models under the `tabbit/<model>` route through **one of two
transports**, chosen per request:

* **web** (preferred) — talks to the vendor's own web API at `web.tabbit.com` with the
  session cookie a signed-in Tabbit browser holds. No sidecar, no Node.js, no extra
  process.
* **sidecar** — talks HTTP to a locally running
  [`hwttop5/tabbit2api`](https://github.com/hwttop5/tabbit2api) bridge, which drives the
  browser itself.

`transport` picks between them: `"auto"` (the default) uses **web** whenever a web
credential exists and has not been rejected, and falls back to the sidecar otherwise.
`"web"` and `"sidecar"` pin one.

## Licence boundary (important)

`hwttop5/tabbit2api` is **GPL-3.0-only**. This module therefore contains **no derived
code**: it never copies, ports, links against, vendors or reads anything inside that
project's checkout. It talks to the sidecar exclusively over HTTP, as a separate
process — the standard "use it as a program" boundary that the GPL permits. Everything
under `clients/tabbit/` is original code.

The **web** transport is our own reverse-engineering of the public web application's own
HTTP calls (see below). It derives from nothing but observed requests.

## The web transport

Every chat request performs the same four steps, because the vendor retires a room as
soon as a run finishes (`room_reset_required`, reason `terminal_snapshot_available`):

1. `POST /panel/session` — no body, `accept: application/json` → `{"chat_session_id": …}`.
   This is the room.
2. `POST /api/v3/chat/rooms/<room>/join` — opened **before** the run is submitted, so no
   early event is missed. The answer is `text/event-stream`.
3. `POST /api/v3/chat/rooms/<room>/runs` — the user turn:
   `{"client_run_id":"tab_<uuid>","client_message_id":"tab_<uuid>","agent_plan_mode_enabled":true,"input_payload":{"task_name":"chat","content":<text>,"parent_message_id":null,"selected_model":[<display name>],"references":[],"metadatas":{"html_content":"<p>…</p>"},"page_info_list":[],"is_mobile":false,"agent_mode":false}}`
   → `{"run_id": …, "status":"QUEUED"}`.
4. Read the join stream until `run_completed` (or `room_reset_required`), then drop the
   room.

Authentication is a single header: `Cookie: token=<JWT>`. The JWT is parsed locally
(`sub`, `iss`, `exp`) — never verified, only read for the uid and the expiry. Requests
also carry `x-req-ctx` (the browser build context, `1.15.17(10115017)` base64-encoded) and
the join call carries `x-nonce`, `x-signature` and `x-timestamp`.

SSE events, and what this module does with each:

| event | handling |
| --- | --- |
| `snapshot_begin`, `message_history`, `active_runs_snapshot`, `snapshot_end` | ignored — replay of the room's history |
| `assistant_message_delta` **before** `run_started` | ignored — that is the replay, not our answer |
| `run_queued` | ignored |
| `run_started`, `assistant_message_started` | marks the stream as live; later deltas count |
| `assistant_message_delta` | `payload.delta` (falling back to `payload.chunk_payload.delta`/`content`) becomes a `core.EventDelta`; a `chunk_payload.reasoning_content` on the same frame is forwarded as `Reasoning` |
| `event_message_chunk` with `chunk_type=thinking` | the reasoning channel: `chunk_payload.reasoning_content` becomes a `core.EventDelta` with only `Reasoning` set. The answer text never arrives this way — a live run on 2026-10-02 carried 103 `thinking` frames and 28 `assistant_message_delta`/`message_chunk` frames — but a `default:` here loses the model's entire explanation of its answer |
| `event_message_chunk` with `chunk_type=thinking_finished` | ignored — the other end of the reasoning channel, and it carries no text |
| `user_message_created`, `assistant_message_finished`, `session_title_updated` | ignored |
| `run_completed` | ends the stream; a status other than `COMPLETED` is an error |
| `room_reset_required` | ends the stream normally |
| `run_failed`, `run_error`, `error` | `core.EventError` |
| anything else | ignored, and an unreadable frame is logged rather than fatal |

**Multi-turn:** the room cannot be reused, and `input_payload` carries one `content`
string, so a multi-message request is flattened into a single prompt
(`User: …\n\nAssistant: …`; every non-assistant role is labelled `User`). A single user
message is sent verbatim.

The catalogue comes from the vendor too:
`GET /proxy/v1/model_config/models?a=0&scene=chat` → 23 chat models on 2026-09-29. The
vendor publishes **no id**, so `display_name` is both the model id and the value
`selected_model` carries. `GET /api/commerce/quota/v1/usage?user_id=<sub>` is the vendor's
own quota report (a *percentage*, not a credit count); it is folded into the panel's
**Test** result as a note. It is deliberately **not** a `core.BalanceProvider`: mapping a
percentage onto the panel's `credits` field would invent a unit.

## Credential import (the Tabbit launcher)

The cookie can be pasted in by hand (`fields: {"kind":"web-token","token":"<JWT>"}`), but
the panel's **导入凭据** action can read it straight out of the running browser. That path
uses the Tabbit browser's own command line (`tabbit-cli.exe`), which exposes a Playwright
runtime: a small JavaScript program calls `context.cookies()` and returns the `token`
cookie.

The browser-cookie entry remains selectable after its first import and is labelled
*已导入，可更新*. Signing in again in the Tabbit browser and re-importing it replaces
the existing `tabbit-web:<uid>` token in place; it does not add a duplicate account.

* The CLI is **discovered**, never hard-coded: `tabbit_cli` in the config, then
  `%LOCALAPPDATA%\Tabbit\LocalAgent\bin\tabbit-cli.exe`. The `cliPath` inside
  `launcher-target.json` is *not* used to invoke anything — it is only evidence that the
  browser is installed.
* Each program runs under its own task (`create --task …` first, then `nodejs`, then
  `finish`), with the program on stdin, and results above 16 KiB are read back in
  fragments.
* **Honest limitation.** On the machine this was developed on, the launcher refuses to
  start when it is spawned by a Go child process — every variant tried fails with
  `BROWSER_LAUNCH_FAILED` (exit 69), while the identical commands typed into a shell
  succeed. Ruled out: piped, file-backed, inherited and discarded stdio; a custom
  environment; `%TMP%`/`%TEMP%`; the working directory; the task name; `CREATE_NEW_CONSOLE`
  and `DETACHED_PROCESS`; and putting `cmd.exe` or `powershell.exe` in between. What *is*
  visible is that the service runs inside a Windows job object (`CREATE_BREAKAWAY_FROM_JOB`
  is refused with *Access is denied*), so the difference is in the process tree the service
  is started from, not in the arguments this module builds. **The import therefore reports
  the launcher's own error instead of failing silently, and appends the manual route when
  it recognises `BROWSER_LAUNCH_FAILED`.** The cookie can always be pasted in by hand
  (DevTools → Application → Cookies → `https://web.tabbit.com` → `token`).
* Because a best-effort scan must not be ruined by one unavailable source,
  `Import(all=true)` refreshes the browser-cookie source even when that account already
  exists, but **skips** it when the source is broken (and logs it); naming that source
  explicitly in `paths` returns the real error.

## Config

The object under `"clients"."tabbit"` in the main config (see `config.example.json`).
Every field is optional; an absent object behaves like `{}`.

| key | type | default | meaning |
| --- | --- | --- | --- |
| `transport` | string | `auto` | `web`, `sidecar` or `auto` (web when a usable web credential exists, else sidecar). |
| `web_base_url` | string | `https://web.tabbit.com` | The vendor's web root. It does **not** follow the sidecar `base_url`; they are different hosts. |
| `web_token` | string | — | A static web cookie, used when no account was added through the panel. Adding an account is preferable: the panel can disable and remove it. |
| `tabbit_cli` | string | *discovered* | Path of the Tabbit browser's own command line, used by 导入凭据. Empty means "discover it". |
| `base_url` | string | *auto-probed* | Sidecar origin, e.g. `http://127.0.0.1:50124`. Setting it declares the sidecar "configured" and disables auto-discovery. |
| `api_key` | string | `sk-tabbit-local` | Bearer token the sidecar expects. |
| `manage` | bool | `false` | Opt in to letting this module start the sidecar process for you. |
| `command` / `args` / `workdir` | | *discovered* | What to start when `manage` is true. |
| `health_path` / `models_path` / `chat_path` | string | `/health`, `/v1/models`, `/v1/chat/completions` | Sidecar endpoints. |
| `web_host` | string | `web.tabbit.com` | The host the browser login opens, and the host you expect the sidecar to drive. Used to build the login URL; host drift is reported in `Status()`. |
| `model_prefix` | string | `tabbit/` | Namespace applied when forwarding and stripped when listing. |
| `extra_models` | []string | `[]` | Extra ids appended to the fallback catalogue. |
| `extra_headers` | map | `{}` | Extra request headers, applied to web calls last (they win). |
| `include_usage` | bool | `true` | Ask the sidecar for a usage chunk (`stream_options.include_usage`). |
| `timeouts.*` | float seconds | see below | `status_seconds` 0.9, `health_seconds` 1.5, `models_seconds` 10, `request_seconds` 900, `start_seconds` 20, `cache_seconds` 5, `models_cache_seconds` 60. |

A malformed config is never fatal: `New` keeps the parse error, logs
`config rejected, continuing with defaults: …`, and `Status()` reports
`invalid config: …`. `Chat` then returns `core.ErrNotConfigured` (HTTP 503).

## Accounts

Two kinds, chosen by the panel's `kind` field:

* `web-token` — the Tabbit browser's own `token` cookie. The id is `tabbit-web:<uid>`
  (uid from the JWT), and the stored token is never echoed back to the panel or written to
  a log. An expired or malformed JWT is refused at add time with the expiry in the message.
* `sidecar` — the base URL of a local bridge (the default kind).

A web-token account always uses `web_base_url` as its base: the vendor host is fixed, so a
per-account `base_url` would be a lie.

## Discovery (sidecar path)

**Base URL**, first match wins: `base_url` → `CLIENT2API_TABBIT_BASE_URL` →
`TABBIT_BASE_URL` → `<data-dir>/tabbit/sidecar.json` (written when this module started a
sidecar itself) → the default candidate `http://127.0.0.1:50124`. With no explicit
`base_url`, every candidate is probed once per `Chat` (bounded by `health_seconds`) and
the first one that answers `/health` wins.

**API key**, first match wins: `api_key` → `CLIENT2API_TABBIT_API_KEY` →
`TABBIT_API_KEY` → state file → the built-in `sk-tabbit-local`. The key is never written
to disk and never logged in full (`core.MaskSecret`).

**Command**, first match wins: `command` → state file → `CLIENT2API_TABBIT_CMD` →
`TABBIT_SIDECAR_CMD` → a small discovery list (`%LOCALAPPDATA%\tabbit2api\bin\…`,
`…\node_modules\.bin\…`, `…\src\cli.js`, `%APPDATA%\npm\tabbit2api*`,
`%ProgramFiles%\tabbit2api\tabbit2api.exe`) and then `PATH` (`tabbit2api[.cmd|.bat|.exe]`).
A discovered `.js` entry point is run through `node` from `PATH`. Discovery only
*stats* files; it never executes anything.

The endpoint these rules resolve is deliberately **not** listed as an account in the
panel: the account table is the panel's editing surface, and `RemoveAccount` /
`SetAccountEnabled` only accept endpoints added through the panel. Where the gateway would
actually talk to is reported by `Status()` instead.

## Behaviour

* `Models(ctx)` never returns an error. With a usable web credential it serves the
  vendor's real catalogue (cached for `models_cache_seconds`); otherwise the sidecar's,
  otherwise a **built-in fallback catalogue** of the 23 `display_name`s observed on
  2026-09-29 (`Default`, `MiMo-V2.6-Pro`, `GLM-5.3`, `DeepSeek-V4.1-Flash`, …). The
  fallback deliberately contains **no** model the vendor does not publish. Returned ids
  are bare (the prefix is stripped) because the gateway adds `tabbit/` itself.
* `Chat(ctx, req)` returns `core.ErrUnsupported` for a request that cannot be expressed
  (nil request, empty model) **before** any network I/O; `core.ErrNotConfigured` when
  neither transport is usable; and a plain error (→ HTTP 502) for an upstream failure.
  A 401/403 from the vendor is reported as an expired cookie, with the 导入凭据 hint.
* Streaming: deltas become `core.EventDelta`, then exactly one `EventDone` carrying a
  normalised finish reason (`stop` by default). **Both** transports forward the model's
  reasoning as `core.EventDelta.Reasoning`: the web path takes it from
  `event_message_chunk`/`thinking` frames, the sidecar path from its `reasoning_content`
  field. The sidecar path additionally re-frames tool calls and one `EventUsage`; the web
  path has no usage frame, so it does not claim one.
* `Status(ctx)` is cheap and bounded (`status_seconds`, 0.9s) and **never spawns a
  process**. On the web path it asks the catalogue endpoint once — unless a verdict
  younger than 15 s already exists (a finished chat stores its own), in which case the
  result is reused instead of spending the budget again. When that call runs out of
  budget the last real verdict is reported instead of "unknown": the account table
  reads the same verdict, so a slow vendor must not paint the account-pool badge red
  while the row it labels still reads ready. A success only counts that way for
  15 min, and a refusal (401/403) is never softened. Fields: `Ready`, `Detail`
  (`N of M Tabbit web session(s) usable` / `sidecar <url> ok (…)`), `Accounts` (one row
  per account, with the uid, the model count and the expiry in the note), and `Models`.
* State lives only in the data dir (`<data-dir>/tabbit/`): `endpoints.json` (the accounts
  the panel added, including the cookie), `sidecar.json` (the last located sidecar) and
  `sidecar.log`. Nothing else is written anywhere.
* Everything is standard library only. No dependency was added.

## Daily sign-in (每日签到)

`tabbit` implements `core.CheckinProvider` for the vendor's daily sign-in, which hands a
slice of the account's quota back once per day. The endpoint pair was read off the
vendor's own web bundle, not guessed:

```
POST /api/commerce/activity/v1/sign-in         body: {"scene_code":"desktop_pet"}
GET  /api/commerce/activity/v1/sign-in/status  ?scene_codes=desktop_pet
```

* **The reward is quota, not credits.** The vendor's own i18n table files it under the usage
  ledger as "Sign-in Reward", next to "Usage Restored" and "Usage Reset Coupon". Since the
  quota endpoint reports a *percentage* of the cycle's allowance, a claim shows up as
  `usage_percentage` dropping — which the panel's balance column already renders. Press
  **余额** afterwards to read the new number.
* **No amount is hard-coded.** The bundle carries no figure, so the reply's own
  `rewarded` is what the panel reports. Nor does the module claim a window (calendar day
  vs rolling 24h vs cycle): the vendor decides and the reply is the only authority.
* **HTTP 200 is not a grant.** A `granted:false` reply is reported as a refusal
  (`OK=false`, nil error) — the normal answer for "already signed in today" — so the
  operator is never shown a success toast over an unchanged balance. A Go error is
  reserved for "could not even be attempted" (no such id).
* **Web-token accounts only.** A `sidecar` endpoint is a local bridge with no vendor
  session behind it, so `CheckinActions` reports no action at all when only sidecar
  accounts exist — the button would be a guaranteed 401.
* A dead cookie surfaces the shared web-transport wording, including the 导入凭据 fix.
  `sign-in/status` is implemented as the read side; it is not yet surfaced by the panel.

## Signing in

`tabbit` implements `core.LoginProvider`, but the module performs **no login of its own**,
and the login URL it hands out only works inside the Tabbit browser (an ordinary browser
redirects to the marketing site). `StartLogin` therefore **opens that page in the Tabbit
browser itself**, through the browser's own launcher — the same `tabbit-cli` 导入凭据
uses — and answers with `local_app: true` plus `handoff_path`. That pair is a
vendor-neutral signal the panel reads as "no link to open here; finish by importing the
credential I just named", which is what an account row's 重登 now does. If the launcher
is missing or refuses to come up (`BROWSER_LAUNCH_FAILED`), the state keeps
`local_app` false and returns the URL together with the launcher's own failure, so the
panel still offers the manual route. There are two real paths to a credential:

1. **Import (preferred).** Sign in inside the Tabbit browser, then let the panel read the
   browser-cookie entry (`handoff_path` names exactly which one). That stores a
   `web-token` account and the web transport uses it directly — no sidecar at all, and a
   re-login updates the same `tabbit-web:<uid>` row instead of adding a second one.
   `PollLogin` reports only the sidecar path, because an imported cookie lands in the
   account table, not in a login session: a hand-off session stays `pending` until the
   panel imports the cookie, which is why polling one never relaunches the browser.
2. **Sidecar.** Start `tabbit2api`, let it drive the browser, then poll. The verdict is
   deliberately **not** `/health`: the sidecar answers that even when nobody is signed in.
   The module asks for the model list instead, and the session turns `success` as soon as
   that list is non-empty. While it is empty the session stays `pending` with the reason,
   plus a host-drift warning when `web_host` and the host the sidecar reports disagree.
   A session lives for 30 minutes (then `failed`), `CancelLogin` is idempotent, and an
   unknown session id is refused.

## Installing and running the sidecar

Only needed when the sidecar transport is used. Out of scope for this repo (the upstream
is GPL-3.0 and separate):

1. Install `hwttop5/tabbit2api` yourself, following *its* instructions.
2. Start it (`tabbit2api`) — it listens on `127.0.0.1:50124` by default with the key
   `sk-tabbit-local`.
3. Either set `"base_url"` in the config, or leave it unset and let discovery find the
   default port. Set `"manage": true` only if you want client2api to start the sidecar
   for you.

## Hazards (documented, not fixed in-process)

* **The room is single-use.** The vendor requires a fresh room per run
  (`room_reset_required`); reusing one is not supported by the vendor.
* **Web-host drift.** The reference hardcodes `web.tabbit.ai`, while a live session on
  this machine uses `web.tabbit.com`. Set `web_host` so `Status()` shows what you expect.
* **Launcher vs. child process.** See *Credential import*: the Tabbit launcher refuses to
  start when spawned by this process on the development machine, so 导入凭据 may report
  `BROWSER_LAUNCH_FAILED` while the same command works from a shell. Pasting the cookie
  always works.
* **Sidecar hazards** (only on that path): brittle webpack/DOM bridge, a profile copy that
  needs every Tabbit window closed, prompt truncation above ~19 000 characters, and
  snapshot-diff streaming that may duplicate or reorder text.

## Tests

`go test ./clients/tabbit/...` runs entirely offline — no Node.js, no Playwright, no
network, no credentials. The sidecar path uses a fake `http.RoundTripper`; the web path
uses an `httptest.Server` that speaks the protocol above, including the replay deltas that
must be dropped, an unknown event that must be ignored, and the quota endpoint. The data
dir is a temp directory. Live checks belong in a separate `*_live_test.go` guarded by
`CLIENT2API_LIVE=1`.
