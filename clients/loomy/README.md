# `loomy` client

A `client2api` client module that exposes **Loomy** (讯飞 Loomy, iFlytek's desktop
AI agent) as an OpenAI-compatible chat endpoint.

* Route prefix: `loomy/<model>` (e.g. `loomy/deepseek-v4-flash-0731`)
* Registered name: `loomy` (from `init()` in `loomy.go`)
* Upstream wire protocol: plain **HTTP + SSE** — `POST /api/v1/chat/completions`
  speaks standard OpenAI chat completions, so there is no envelope to unwrap and
  no body translation beyond the usual `core.ChatRequest` mapping.
* Auth: a per-account **session token** (32 lowercase hex) sent in a header.
  Sessions are minted by an SMS login against the iFlytek account service, which
  is signed with **HMAC-SHA1** over a nine-segment canonical string.
* Dependencies: **standard library only** — nothing outside `client2api/internal/core`
  and the Go standard library is imported.

This module is a clean-room Go reimplementation of the Loomy support in the
reference TypeScript plugin (`iJetLi/deepseek-harness-codearts`, `src/loomy*.ts`).
The signing routine is written with `crypto/hmac`, not transliterated line by
line; the protocol constants, the canonical-string layout and the endpoint
behaviour are copied from the reference.

---

## The one thing to understand first: the session cannot be renewed

A Loomy session is minted by an SMS (or WeChat) login and **declared by the
client to last 14 days** (`expire: 1209600`). The vendor returns no refresh
token and exposes **no refresh endpoint** — there is nothing to call.

This is the single most important design constraint, and the module degrades
honestly rather than pretending otherwise:

* `isLoomyRefreshable` is false, always. `RefreshAccount` **re-verifies** the
  stored session instead of renewing it, and says so in its message.
* When the vendor answers `100002` (`登录状态失效`) the account is **parked**
  (`dead`), not retried: retrying a dead session only burns quota on a request
  that cannot succeed. The panel shows the account as `invalid` with the note
  *"the vendor rejected this session; log in again and import the new token"*.
* `Status` reports not-ready with *"the session has expired and Loomy has no
  refresh endpoint; log in again (SMS or WeChat) and import the new token"*.
* `expires_at` is computed **locally** as login time + 14 days, because the
  server never sends one. A credential with **no** `expires_at` is treated as
  *not expired*: it is better to try a possibly-stale token — the server answers
  `100002` and we recognise it — than to block the operator on a guess.

**The operator must re-import a fresh token roughly every 14 days.** There is no
way around this from inside the module.

---

## Where credentials come from

Manual import is the primary path. Put the session token in the host config
(`clients.loomy.access_token`, or an `accounts[]` entry) or in the environment
(`CLIENT2API_LOOMY_ACCESS_TOKEN`), or add it from the panel via the account
form (`access_token`, plus optional `userid` / `phone` / `nickname` /
`expires_at`). `AddAccount` probes the token against
`GET /api/v1/points/records` before storing it and **refuses** one the vendor
rejects; an unverifiable token (network down) is stored anyway, with a log line.

An operator who has the Loomy desktop app can read the session out of it. The
module cannot do that for you.

### The SMS login, and the panel's one-click auto-add

`login.go` implements the full two-step SMS flow:

1. `POST https://account.xfinfr.com/login/phone/sendMsgCode` with
   `{ccode, phone, expire: 300}` → returns a `msgid`
2. `POST https://account.xfinfr.com/login/phone/checkCode` with
   `{ccode, phone, mcode, msgid, expire: 1209600}` → returns `session` + `userid`

Both are signed with the HMAC-SHA1 scheme below, using the product AccessKey.
`importSession` then stores the session with a locally-computed 14-day expiry.

**This flow is exposed through `core.AutoLoginProvider`, not
`core.LoginProvider`.** `LoginProvider` is a *URL-and-poll* shape — the panel
opens a URL and then polls a session id — and has no way to submit the SMS code
the user just received, so this module does not claim it.  `AutoLoginProvider`
is a job the module runs itself, which is exactly what an HTTP phone-code login
needs: the panel rents a number from the one-time-SMS platform
(`core.SMSProvider`, see `sms.go`), the module calls `sendMsgCode`, reads the
code back from the platform, calls `checkCode`, and stores the session.  The
operator clicks one button and watches a progress log; no browser is involved
because Loomy's login is a plain HTTP exchange.

The one-time-SMS platform token comes from `clients.loomy.sms_token`, the
shared `EOMSG_TOKEN` environment variable, or a token pasted in the panel.
`sms_keyword` defaults to `讯飞` (the sender the account service uses) and can
be overridden per run.  A re-login pins the account's own phone number, so
refreshing an expired session does not create a second Loomy account.

The **WeChat** login path is deliberately **not** implemented: its QR-code flow
lives in a reference file that could not be fetched, so it could not be
reproduced faithfully.

---

## Endpoints, headers and the signature

Two different base URLs, and they want **different** auth headers. Getting this
wrong is silent: the vendor answers **HTTP 200** with `{"code":"100002","desc":"缺少 token"}`.

| Endpoint | Auth header |
| --- | --- |
| `POST {api_base}/chat/completions` | `Authorization: Bearer <token>` **and** `token: <token>` |
| `GET {api_base}/models` | `token: <token>` only |
| `GET {api_base}/points/records` | `token: <token>` only |
| `POST {api_base}/points/first-login` | `token: <token>` only |
| `POST {account_base}/login/phone/*` | HMAC-SHA1 `Authorization: account <id>:<sig>` |

`api_base` defaults to `https://loomyad.xunfei.cn/api/v1`; `account_base` to
`https://account.xfinfr.com`. The chat request sends **both** headers on purpose:
`/chat/completions` rejects a bare `token`, and the business endpoints reject
`Authorization`. `Bearer ` is required — without the prefix the server still
answers `100002 缺少 token`.

### Business envelopes

Business failures always arrive as **HTTP 200** with a non-zero `code`, so
success is judged by `code`, never by the status line:

```json
{ "code": "000000", "desc": "success", "data": { } }
```

`000000` is success. `100002` means the session is dead (park the account, do
not retry). `100001` is a bad request. Any other code is reported with the
vendor's `desc`.

### The HMAC-SHA1 signature (account endpoints only)

The canonical string is **nine segments joined with `\n`**, in this order:

```
1  METHOD (uppercased)
2  escaped path            (leading "/", no trailing "/", per-segment RFC 3986)
3  escaped query           (caller's order, never sorted)
4  content MD5 as base64   ("" when the body is empty -- NOT the MD5 of zero bytes)
5  Content-Type
6  Date                    (HTTP date, UTC)
7  Nonce                   (a UUID)
8  ""                      (SignedHeaders -- no x-* header is signed)
9  ""                      (CanonicalizedHeaders)
```

so the string always ends with two newlines. The signature is
`base64(HMAC-SHA1(access_key_secret, canonical_string))`, sent as
`Authorization: account <access_key_id>:<signature>`, alongside `Date`,
`Nonce`, `Content-Type` and — only when the body is non-empty — `Content-MD5`.

The AccessKey (`2thryby66wxi53sk` / `zsak6eadrbawz683wf5r3m2snrwj868r`, app
`GM3LOOMY`) is a **product** credential taken from the Loomy desktop app's
`.env.prod`. It only signs SMS logins; the business endpoints use the user
session, so the AccessKey grants no access to any user's data.

---

## Model ids

`Models()` answers from a built-in catalogue measured against the live vendor on
2026-09-26, so a caller can always discover ids without an account. A live
`GET /api/v1/models` (cached for `models_ttl`, default 30 min) replaces it.

`Models()` **never blocks and never fails**: the refresh runs in the background
(`core.GoSafe`), throttled to one attempt per 30 s, because the registry calls
`Models` on every module for a bare model name.

| Model id | Display name | Context |
| --- | --- | --- |
| `deepseek-v4-flash-0731` | DeepSeek V4 Flash 0731 · x3.0 | 1048576 |
| `MiniMax-M3` | MiniMax M3 · x4.0 | 1048576 |
| `Kimi-k2.6` | Kimi k2.6 · x6.5 | 262144 |
| `qwen-3.8-max` | Qwen 3.8 Max · x12.0 | 1000000 |
| `GLM-5.3-Flash` | GLM 5.3 Flash · x0.8 | 1048576 |
| `qwen3.8-flash` | qwen 3.8 flash · x0.8 | 1000000 |
| `spark-x` | Spark X2.5 · x0.1 | 1048576 |
| `mimo-v2.5` | MiMo V2.5 · x3.3 | 1048576 |

The **rate multiplier** (`x3.0`, `x12.0`, …) is not a separate field upstream —
it lives inside the `name` string in mixed full-width/half-width brackets. It is
normalised to `{name} · {rate}` and concatenated into the model's display name,
because the composer's model switcher renders only `name`. `resolveModelID`
accepts the id, the display name, or the name without the multiplier; an
unrecognised name is passed to the vendor **verbatim** rather than rejected
here — the vendor is the authority.

Only rows with `type == "chat"` and a non-empty `id` are listed. This
deliberately does **not** use `output_modalities`: five chat models carry
`image` in `input_modalities` (vision input), which is unrelated to being an
image-generation model.

`reasoning_effort` is forwarded from `ChatRequest.Options` (keys
`reasoning_effort` / `reasoningEffort` / `reasoning` / `effort`) without
validating it against the model's advertised tiers. The built-in catalogue
records `['none','low','medium','high','xhigh']` and a default of `high` — the
latter deliberately not the vendor's own `low`, matching the reference, because
the gateway sends the module-declared default when the user picks nothing.

---

## Configuration

The module reads the JSON object under `clients.loomy` in the host config
(`configs/client2api.json`). **Every key is optional** and the module is usable
with no configuration at all.

An absent object is the same as `{}`. A **malformed** object is logged and then
replaced with defaults — `clients.loomy` is never a construction failure, and
`New` never returns an error for a bad config or a missing credential. With no
usable credential, `Chat` returns `core.ErrNotConfigured`.

See `config.example.json` for a copy-paste starting point.

```jsonc
{
  "clients": {
    "loomy": {
      "api_base": "https://loomyad.xunfei.cn/api/v1",
      "account_base": "https://account.xfinfr.com",
      "access_key_id": "2thryby66wxi53sk",
      "access_key_secret": "zsak6eadrbawz683wf5r3m2snrwj868r",
      "app_id": "GM3LOOMY",

      "accounts": [ { "id": "", "label": "", "access_token": "",
                      "userid": "", "phone": "", "nickname": "",
                      "expires_at": "", "enabled": true } ],
      "access_token": "", "userid": "", "phone": "",
      "nickname": "", "expires_at": "",

      "request_timeout": "60s",
      "models_timeout": "20s",
      "chat_timeout": "10m",
      "idle_timeout": "2m",
      "first_byte_timeout": "2m",
      "models_ttl": "30m",
      "probe_timeout": "30s",
      "cooldown": "60s",
      "auth_cooldown": "24h",
      "max_attempts": 3
    }
  }
}
```

Durations accept `"90s"`, `"10m"`, `"24h"`, `"1h30m"` or a bare nanosecond
integer; an unparsable value falls back to its default rather than failing the
module. `expires_at` accepts a millisecond timestamp, a second timestamp, or a
date an operator would type (`2026-10-10`, `2026-10-10T00:00:00Z`); `0`/empty
means *unknown*, which is **not** treated as expired.

Environment overrides (each only applies when non-blank):

| Variable | Sets |
| --- | --- |
| `CLIENT2API_LOOMY_ACCESS_TOKEN` | `access_token` |
| `CLIENT2API_LOOMY_USERID` | `userid` |
| `CLIENT2API_LOOMY_PHONE` | `phone` |
| `CLIENT2API_LOOMY_EXPIRES_AT` | `expires_at` |
| `CLIENT2API_LOOMY_API_BASE` | `api_base` |
| `CLIENT2API_LOOMY_ACCOUNT_BASE` | `account_base` |
| `CLIENT2API_LOOMY_ACCESS_KEY_ID` | `access_key_id` |
| `CLIENT2API_LOOMY_ACCESS_KEY_SECRET` | `access_key_secret` |
| `CLIENT2API_LOOMY_APP_ID` | `app_id` |

Config beats the environment: the first entry carrying a given account id wins,
and config entries are read before the environment.

---

## State on disk

Both files live under the host data directory (`Deps.DataDir`), never anywhere
else. Nothing is written when `DataDir` is empty.

| File | Contents |
| --- | --- |
| `accounts.json` | the imported credentials (`id`, `label`, `access_token`, `userid`, `phone`, `nickname`, `expires_at_ms`, `enabled`) |
| `state.json` | per-account runtime penalties (`dead`, `failures`, `last_error`, `cooldown_until`, `last_used`) |

Credentials from the config are tagged `origin: config` and **cannot be removed
from the panel** (`RemoveAccount` refuses, pointing at the config file); stored
ones can. The session token is never used as an account id, never echoed in an
`AccountRecord`, and never written into a log line or status string unredacted
(`core.Redact`).

---

## Account selection

`candidates()` returns every *selectable* account — enabled, not parked, with a
non-blank token, outside its cooldown — sorted by `last_used` ascending, with
locally-expired accounts ranked **last**. Expiry only reorders; it never
withholds, because the local expiry is a guess and the server's `100002` is the
fact.

`Chat` walks the candidate list up to `max_attempts` times:

* success → clear the account's penalties, stamp `last_used`, and record the
  serving account in `ChatRequest.ServedBy` (`core.NoteServedBy`).
* `100002` / HTTP 401 / 403 → park the account for `auth_cooldown` (24 h) and
  try the next one.
* anything else → cool the account down for `cooldown` (60 s) and try the next.

If every candidate fails, the caller gets the last error. If there are no
candidates at all, it gets either an explicit *"session expired, re-import
required"* error naming the account, or *"every account is parked or cooling
down; retry shortly"*.

---

## Streaming

There is no shared SSE helper in the repository, so `stream.go` owns the reader:
a line-buffered `bufio.Scanner` over the response body, decoding `data:` frames
and terminating cleanly on `io.EOF`. It carries

* an **idle timeout** (`idle_timeout`, default 2 min) that aborts a stream which
  has stopped producing frames, and
* a **first-byte timeout** (`first_byte_timeout`, default 2 min) around the
  initial response, reported as `"loomy: chat: no response within 2m0s"`.

`Close()` is idempotent: it stops the timers, cancels the request context and
closes the body exactly once, so a gateway that closes a stream twice cannot
panic or double-cancel.

A 2xx response whose `Content-Type` is not `text/event-stream` is read as a JSON
envelope and reported as a business failure — the vendor sometimes answers a
stream request with a plain envelope, and a silent empty stream would be worse.

---

## What this module does **not** implement, and why

| Interface | Why not |
| --- | --- |
| `core.LoginProvider` | URL-and-poll only; it cannot carry the SMS code back. See above. |
| `core.CredentialImporter` / `core.BundleImporter` | There is no discoverable credential source on disk (the session lives inside the desktop app's own store, in a format that could not be verified) and no bundle format. |
| `core.CaptchaProvider` | The SMS flow needs no captcha. |
| `core.BatchPlanner` | The onboarding board is a one-off checklist: each entry is a first-login chore, not a recurring task the scheduler could pace. `core.TaskProvider` alone is enough to show it and run one entry at a time. |
| `core.HintProvider`, `core.HealthProvider`, `core.Degrader`, `core.PoolStatsReporter` | Not needed for correctness; the module's `Status` already reports per-account state honestly. |

Implemented optional interfaces: **`core.AccountManager`**, **`core.Reviver`**,
**`core.ModelRefresher`**, **`core.BalanceProvider`**, **`core.PackageProvider`**,
**`core.CheckinProvider`**, **`core.ModelLimitsProvider`**, **`core.TaskProvider`**,
**`core.SMSProvider`**, **`core.AutoLoginProvider`**.

`client_test.go` asserts the ones it claims *and* the ones it must not
(`LoginProvider`, `CredentialImporter`, `BundleImporter`), so a future change that
fabricates a capability fails the build.

### `TaskProvider` — the first-login onboarding board

`GET /api/v1/onboarding/tasks` returns a map of chore key → done flag; this module
turns it into `core.TaskInfo` rows and completes one with
`POST /api/v1/onboarding/tasks/complete`. Measured live 2026-10-01: eight keys,
10,000 credits in total.

The complete endpoint has **no idempotency key**, so the board is read first and a
chore already marked done is never posted again. `TaskResult.OK=false` is the
honest answer for "the vendor declined"; a Go error is reserved for "this could
not even be attempted". Only the unknown-key path (business code `100001`) was
ever exercised live — the happy path was deliberately not run against a real
account, so it is fixture-tested only.

### `ModelLimitsProvider`

`GET /api/v1/models` publishes a per-model `max_output_tokens` (measured live
2026-10-01: `deepseek-v4-flash-0731` 384000, `GLM-5.3-Flash` 131072, `Kimi-k2.6`
65536). An earlier revision of this module dropped it and asserted its absence
here on the belief that the vendor published none — the live body disproved that,
and a request that omitted `max_tokens` therefore reached the vendor uncapped.

`ModelMaxOutputTokens` now answers **from the live catalogue cache only**. The
interface is consulted on every request that omitted `max_tokens`, so it must
never touch the network: a cold cache returns `ok=false`, and the built-in
fallback table is deliberately *not* consulted because it carries no
vendor-published number. The honest half of the old concern — never invent a
number — is what `TestTheModuleReportsAPublishedBudgetAndNeverInventsOne` pins.

### Credits and the daily allowance

`AccountBalance` reads `GET /points/records` and reports `{Credits, Total,
Unit: "积分"}`. `Expiring` is **always 0**: this vendor has no expiring tranche,
and `soon` is deliberately ignored rather than answered with an invention.

`Checkin` posts `POST /points/first-login` with `{}` — Loomy's daily gift
allowance. It is a real endpoint the reference drives, not a fabricated
check-in. An already-claimed day is reported as a **success** with
`granted: 0`, not as an error.

---

## Tests

```powershell
$env:Path = "$env:LOCALAPPDATA\Programs\go1.27.1\go\bin;" + $env:Path
$env:GOPROXY='https://goproxy.cn,direct'; $env:GOSUMDB='sum.golang.google.cn'; $env:CGO_ENABLED='0'

gofmt -l ./clients/loomy          # must print nothing
go vet ./clients/loomy/           # must exit 0
go test -count=1 -timeout 20m ./clients/loomy/
```

166 top-level test functions, all fixture-driven: every one injects an
`http.RoundTripper` stub (`helpers_test.go`) or runs against an `httptest` server
on a loopback socket, and every file written goes to `t.TempDir()`. **No test
touches the network**, and none needs a live account.

All of the suites except `server_test.go` use an `http.RoundTripper` stub, which
is fast and precise but cannot exercise the HTTP client itself. `server_test.go`
therefore runs a real `net/http` server over a loopback socket — see the note
below, because that difference caught a bug the stubs could not.

| File | Covers |
| --- | --- |
| `sign_test.go` | four golden HMAC-SHA1 vectors (computed by an independent PowerShell/.NET oracle), the exact nine-segment canonical string and its order, `Content-MD5`, RFC 3986 escaping, header assembly, the nonce |
| `config_test.go` | defaults, duration flexibility, malformed config, env overrides, `expires_at` spellings, account flattening and precedence |
| `accounts_test.go` | credential persistence round-trip, parking a rejected session across a restart, cooldown, expired-but-tried, candidate ordering, `Status` on every account state, `AddAccount`/`TestAccount`/`RefreshAccount`, removal of config-owned accounts, id derivation |
| `client_test.go` | registration, `ErrNotConfigured`, both-auth-header discipline, `ServedBy`, business-error-in-a-200, multi-account failover, `/models` header discipline, capability assertions |
| `body_test.go` | `buildBody` — stream flag, empty content, structured parts, tool/tool_choice handling, reasoning effort |
| `stream_test.go` | SSE frame parsing, `[DONE]`, usage, tool-call deltas, idle timeout, idempotent `Close` |
| `login_test.go` | the SMS two-step flow, with the stub **recomputing the signature from the bytes it actually received** |
| `quota_test.go` | balance, packages and the daily allowance, including the empty-ledger and already-claimed paths |
| `onboarding_test.go` | the first-login task board: the vendor's own registry, flags and prices, a chore the registry does not know, cache-vs-failed-read, `RunTask` posting the key the vendor client posts, a repeat reported as a success that awarded nothing, an unknown code refused without a call, no account, an unknown id, and a dead session parking the account |
| `models_test.go` | the multiplier/display-name rules, both remote catalogue shapes, `isChat`, the fallback table and `ResolveModelID` |
| `envelope_test.go` | the live code-less `/models` envelope: a code-less payload is a success, a failed envelope is still a failure, a non-JSON body is still rejected |
| `cancel_test.go` | a cancelled caller must not park the account or burn the other candidates, while the module's *own* timeout still cools it down |
| `server_test.go` | the same paths against a **real** `httptest` server: the HTTP client, URL joining, header writing and live SSE framing |

The signature tests are real guards, verified by breaking the production code:
deleting the ninth canonical-string segment (`sign.go`, the
`CanonicalizedHeaders` line) makes `TestSignatureGoldenVectors`,
`TestCanonicalStringEndsWithTwoNewlines`, `TestCanonicalStringOrderIsLoadBearing`,
`TestSendSMSCodeSignsTheBytesItActuallySends` and
`TestLoginWithSMSCodeSignsTheBytesItActuallySends` all fail. Restoring the line
returns the suite to green.

`server_test.go` earned its place the same way. It was written last, after every
stub-based test was already passing, and it failed immediately with
`Recv: loomy: reading the stream: context canceled`. The cause was in
`openChatStream`: the request was issued with `firstCtx`, a cancellable child of
the chat-timeout context, and the **success** path cancelled `firstCtx` before
returning the stream — which aborts the response body the caller is about to
read. Every real streamed completion would have died on its first read. A
`RoundTripper` stub cannot catch this, because its canned body is an
`io.NopCloser` that is not tied to the request context at all: the stubs were
testing the decoder, never the client. The fix leaves `firstCtx` alive as a child
of the chat-timeout context (so `Close` still releases it) and adds a guard for a
first-byte deadline that fires in the instant before the timer is stopped.

---

## Not verified without a live account

Honest list of what the tests cannot prove, because they never leave the
process. **Live-validated 2026-10-01** against a real signed-in Loomy account
(`loomy-261001212259353798`): `/models` returned 8 chat rows with
`max_output_tokens`, a non-streaming and a streaming `/chat/completions` both
returned real completions with `reasoning_content` and usage, and
`/points/records` answered `9998/9998 积分`. That session also exposed the
code-less `/models` envelope bug this module used to reject.

* **The exact live response shape** of `/points/first-login` beyond what the
  reference source documents. `/models`, `/points/records` and
  `/chat/completions` are now measured; the effort tiers were measured by the
  reference author on 2026-09-26/28 and are reproduced as-is, and the live
  `/models` body now confirms them.
* **The happy path of `POST /api/v1/onboarding/tasks/complete`.** Only the
  unknown-key path was exercised live (`/api/v1/onboarding/tasks` returns 8 keys
  totalling 10,000 credits; posting a key the vendor does not know answers
  business code `100001`). Completing a real chore would have moved real credits
  and was deliberately not run, so the success path — the awarded amount, and
  what a repeat returns — is fixture-tested only.
* **The SMS login flow end to end.** The request shape, the signature and the
  response parsing are tested against a stub, but no real SMS has been sent.
* **Whether the vendor's 14-day session window is enforced exactly.** The
  `expire: 1209600` value and the local expiry are taken from the reference.
* **WeChat login**, which is not implemented at all.
* The **`spark-x` context window**: the vendor advertises `1048576` while the
  Loomy desktop client locally forces `262144`. This module reports what the
  vendor says (`1048576`) and does not reproduce the client's override, since
  the override is the desktop client's business and not a server-side limit.

## Deviations from the reference

* `isLoomyRefreshable` is reproduced as a hard `false`, but the module does not
  expose it: there is no `Refreshable` field on `core.AccountRecord`, so the
  honest signal is carried by the account `State` (`invalid`) and its note.
* The reference's `openai-compat.ts` shared layer is not reproduced; the SSE
  reader and the body builder are local to this module, per the repository's
  no-cross-module-import rule.
* The reference's `MODEL_CONTEXT_OVERRIDES` (desktop-client-side) is **not**
  copied — see above.
* `core.NoteServedBy` is used to report the serving account, which the reference
  has no equivalent for.
