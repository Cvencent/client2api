# `lobsterai` client

A `client2api` client module that exposes **LobsterAI** (有道龙虾, Youdao) as an
OpenAI-compatible chat endpoint.

* Route prefix: `lobsterai/<model>` (e.g. `lobsterai/deepseek-v4-pro`)
* Portal: `https://lobsterai.youdao.com` · API base: `https://lobsterai-server.youdao.com`
* Upstream wire protocol: **OpenAI-shaped**, but **SSE only** — see below
* Dependencies: **standard library only** — nothing outside `client2api/internal/core`
  and the Go standard library is imported.

This module owns the credential, the browser-hand-off sign-in flow, the
three-call check-in, the balance lookup and the multi-account cooldown pool.

---

## The three ways LobsterAI breaks the repo's shape

Read this first; everything else follows from it.

1. **The chat endpoint is SSE-only.** `POST /api/proxy/v1/chat/completions` is
   OpenAI-shaped, but sending `"stream": false` makes the upstream answer
   **500**. The bridge therefore **always sends `stream: true`** and
   re-aggregates the frames into a single answer when the caller asked for one.
   A caller that sets `Stream: false` gets a normal one-shot reply; the vendor
   simply never learns that.
2. **Chat is the one endpoint with no `{code, msg, data}` envelope.** Every other
   route wraps its payload; the chat response is **bare SSE**. Applying the
   envelope check to it would fail every request, so it is deliberately not
   applied. `lobsterai_test.go` pins both halves of this rule.
3. **There is no image support.** An image part is refused with
   `core.ErrUnsupported` before any network call rather than silently dropped —
   an answer produced without the image the user sent is a wrong answer.

Credentials are also unusual: there is no password grant. Sign-in is a
**browser hand-off** (below), and the credential is only obtainable that way or
by pasting a token you already have.

---

## Model ids

The remote catalogue is `GET /api/models/available` (cached for `models_ttl`,
default 1 h). Each row carries `modelId`, `modelName`, `provider`, `apiFormat`,
`contextWindow`, `supportsImage`, `supportsThinking`, `costMultiplier` and more —
**a per-model context window, but no output budget**. `modelName` becomes the
catalogue's `display_name`, and `contextWindow` becomes `context_length` when it
is non-null.

Before (or without) a live account, `Models()` answers from a built-in
catalogue of 19 ids measured 2026-08-06: `deepseek-v4-flash`, `deepseek-v4-pro`,
`qwen3.7-max`, `qwen3.7-plus`, `qwen3.6-plus`, `qwen3.5-plus-2026-04-20`,
`kimi-k2.7-code`, `kimi-k2.7-code-highspeed`, `kimi-k2.6`, `kimi-k2.5`,
`doubao-seed-2-1-pro-260628`, `doubao-seed-2-1-turbo-260628`,
`doubao-seed-2-0-code-preview-260215`, `glm-5.2`, `glm-5.1`, `glm-5v-turbo`,
`glm-5`, `MiniMax-M3`, `MiniMax-M2.7`. `Models()` **never fails**: a failed
refresh falls back to the last good list and then to the built-in one, and
`RefreshModels` returns the last good list *alongside* its error.

An unrecognised model id is passed through to the vendor verbatim; the vendor is
the authority.

### `ModelLimitsProvider` is deliberately not implemented

The module does **not** implement `core.ModelLimitsProvider`. The vendor
publishes no per-model output budget, and the only way to discover one would be
to fetch the model list — which that interface's contract explicitly forbids
(it is consulted on every request that omitted `max_tokens`, so it must never
touch the network). Inventing a number would be a fabricated cap; answering
"cannot say" is the honest result, and it is what `clients/minimaxcode` and
`clients/trae` do in the same situation.

The catalogue reports the vendor's **published** `contextWindow` per model
(measured 2026-10-01: `1000000` for `deepseek-flash`, `glm-5.3` and the `qwen3.8`
family; `262144` for the `kimi-k2.x` family; `256000` for `doubao-seed-2-1`).
Nine rows — `MiniMax-M2.7`, `qwen3.6-plus`, `qwen3.5-plus-2026-04-20`,
`kimi-k2.6`, `kimi-k2.5`, `glm-5.1`, `glm-5v-turbo`, `glm-5` and
`doubao-seed-2-0-code-preview-260215` — send `contextWindow: null`, and those
keep the `131072` **bridge-layer estimate** used by the built-in fallback table.
That estimate is *not* an output budget, and it is never exposed through
`max_output_tokens`.

---

## Configuration

The module reads the JSON object under `clients.lobsterai` in the host config
(`configs/client2api.json`). **Every key is optional** and the module is usable
with no configuration at all — sign in from the panel and the credential lands
in `accounts.json`.

An absent object is the same as `{}`. A **malformed** object is logged and then
replaced with defaults (`clients.lobsterai` is never a construction failure);
the factory logs `config rejected, continuing with defaults: …` and Status
reports not-ready.

See `config.example.json` for a copy-paste starting point.

### Top-level keys

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `portal_url` | string | `https://lobsterai.youdao.com` | Origin the sign-in page is built from. |
| `base_url` | string | `https://lobsterai-server.youdao.com` | Origin every API endpoint is built from. |
| `version_url` | string | the api-overmind manifest | Where the client version is read from. |
| `client_version` | string | `0.1.0` | `X-LobsterAI-Client-Version`. A value that is not version-shaped (`latest`, `v0.1.0`) is discarded for the default. The live value replaces it as soon as the manifest answers. |
| `client_capabilities` | string | `kimi-k3-agentic-v1` | `X-LobsterAI-Client-Capabilities`. |
| `user_agent` | string | `LobsterAI/0.1.0` | `User-Agent` on every request. |
| `login_port` | int | `0` | Loopback port for the sign-in callback. `0` picks a free one. |
| `callback_path` | string | `/auth/callback` | Path the callback server listens on. |
| `include_usage` | bool | `false` | Ask for a usage frame via `stream_options`. |
| `extra_models` | array | `[]` | Extra ids appended to the built-in catalogue. |
| `extra_headers` | object | `{}` | Extra headers. Reserved names (`Authorization`, `Content-Type`, `Accept`, `User-Agent`, the two `X-LobsterAI-*` headers) are dropped. |
| `login_timeout` | duration | `10m` | Budget for one sign-in session; a session older than this expires. |
| `chat_timeout` | duration | `10m` | Deadline for one `Chat` call. |
| `models_timeout` | duration | `20s` | Deadline for the catalogue, the exchange and a token refresh. |
| `checkin_timeout` | duration | `30s` | Deadline for one check-in. |
| `version_timeout` | duration | `10s` | Deadline for the version manifest. |
| `models_ttl` | duration | `1h` | How long a live catalogue is served from cache. |
| `version_ttl` | duration | `6h` | How long a fetched client version is reused. |
| `cooldown` | duration | `60s` | Park duration for a rate-limit, a not-found or a repeated soft error. |
| `quota_cooldown` | duration | `12h` | Park duration when the account is out of credit. |
| `refresh_margin` | duration | `30m` | Renew a token proactively when it has less than this left. |
| `max_in_flight` | int | `2` | Concurrent requests per account before `core.ErrBusy`. |

Durations accept a Go duration string (`"90s"`) or a bare JSON number read as
seconds (`"cooldown": 300`); anything else falls back to the default rather than
failing the module.

### `accounts[]` entries

| Key | Type | Meaning |
| --- | --- | --- |
| `label` | string | Human note for the panel; never a token. |
| `uid` | string | Vendor user id. Also the account's identity in the pool. |
| `user_id` | string | The vendor's `userId`, replayed on renewal when present. |
| `uuid` | string | The device uuid minted at sign-in. **Replayed on renewal.** |
| `nickname` | string | Display identity. |
| `access_token` | string | **Required** — an entry without it is dropped. |
| `refresh_token` | string | Renews `access_token`. |
| `expires_at` | string | RFC3339, unix seconds or unix milliseconds. Unparseable ⇒ 0 = unknown. |
| `first_keyfrom` | string | **Replayed on renewal** (see below). |
| `latest_keyfrom` | string | **Replayed on renewal.** |
| `disabled` | bool | Start the account parked. It is still reported, never selected. |

### Environment

**No environment variables are read.** This module's credentials come from the
config, the sign-in flow, or `accounts.json` in `Deps.DataDir` — nothing else.

---

## Credential sources and the sign-in flow

Credentials are looked for in this order, and the pool is rebuilt from the union:

1. `clients.lobsterai.accounts[]` — explicit list.
2. `clients.lobsterai.access_token` (+ `uid`, `uuid`, `refresh_token`, …) —
   single-account shorthand.
3. **`accounts.json` in `Deps.DataDir`** — the module's own credential store.

The **store wins** on an id collision: it is the copy the renewal path keeps
current, so a stale config entry cannot shadow a token that has since been
refreshed.

### The browser hand-off

There is no password grant. `LoginProvider` implements the flow the desktop app
uses, and the module **does not launch a browser itself** — it hands the URL to
the panel, because no other module in this repository spawns a process and a
library doing so is intrusive.

1. `StartLogin` opens a loopback listener on `127.0.0.1:{login_port}`, mints a
   `state` and a `uuid`, and returns the sign-in URL:

   ```
   {portal_url}/portal#/login?source=electron&redirect_uri=http://127.0.0.1:{port}/auth/callback&state={state}
   ```

   Note the URL is a **hash route**: the query lives in the fragment, not the
   query string. The test suite parses the fragment for exactly this reason.

2. The human opens that URL and approves it. The vendor redirects to the loopback
   callback with `?code=…&state=…`. A **state mismatch is answered with HTTP 400**
   and fails the session; the callback page then reads
   `登录成功，可以关闭此窗口了`.

3. `PollLogin` exchanges the code: `POST {base_url}/api/auth/exchange` with

   ```json
   {"authCode": "…", "firstKeyfrom": "…", "latestKeyfrom": "…", "uuid": "…", "version": "…"}
   ```

   and **no `Authorization` header**. At sign-in `firstKeyfrom` and
   `latestKeyfrom` are the **same instant** — the credential records one
   timestamp, and the renewal path replays it forever.

   The account's uid falls back through exactly four levels:
   `user.id` → `user.userId` → `user.yid` → `sha256(accessToken)[:16]`.
   Expiry comes from `expiresIn`, else from the JWT's `exp` claim (the signature
   is deliberately not verified — we are reading our own token, not trusting it).

4. The credential is written to `accounts.json` and the session reports
   `LoginSuccess`. Sessions expire after `login_timeout`; `CancelLogin` is
   idempotent.

### Renewal, and the fields that must be persisted

`POST {base_url}/api/auth/refresh` carries **no `Authorization` header** — the
vendor does not require the old token to mint a new one. The body is:

```json
{"firstKeyfrom": "…", "latestKeyfrom": "…", "version": "…", "uuid": "…", "userId": "…", "refreshToken": "…"}
```

`uuid` and `userId` are sent only when non-empty.

**`first_keyfrom`, `latest_keyfrom` and `uuid` must be persisted and replayed
verbatim.** They are what the vendor issued at sign-in, and renewal stops
working if they are missing or invented. In particular this module does **not**
rewrite `latest_keyfrom` on refresh: the integration document's TypeScript
sketch suggested `latestKeyfrom: String(Date.now())` ("update it every
refresh"), but the only production-validated implementation changes the token
and the expiry and nothing else. A hand-added account that has no keyfrom values
cannot renew — it must be signed in again.

A terminal renewal failure is HTTP 401/403 or envelope code `40100`/`40101`;
that disables the account.

---

## Check-in

Three Bearer calls, in this order, with **no signature**:

1. `GET /api/client-activities/slot?placement=desktop_sidebar&clientVersion={v}&containerApiVersion=2&platform=win32`
   — requires `slotState == "available"` and an `activity` object; reads
   `activityCode` and `configRevision`. `platform=win32` is sent even off
   Windows (client disguise).
2. `GET /api/client-activities/{code}/context?configRevision={rev}` — requires
   `!state.claimedToday` and an `actions` list containing `check_in`. A day that
   is already claimed is reported as **success** with `already_done`, not as a
   failure.
3. `POST /api/client-activities/{code}/actions/check_in` with
   `{"configRevision": …, "idempotencyKey": "<uuid4>", "payload": {}}` — credits
   are read from `creditsGranted` → `rewardCredits` → `credits`. Roughly +100
   credits per account per day.

The `configRevision` value is echoed back **in the vendor's own JSON shape**
(whatever the slot returned) while being stringified for the query, so a vendor
that switches from a number to a string cannot break the round trip.

An upstream refusal is a **result**, not a Go error: `CheckinResult.OK == false`
with a message. A Go error is returned only when the attempt could not be made
at all (unknown account, disabled account, no token). `CheckinActions` returns
nothing unless an enabled account with a token exists — a button that cannot
work is a lie.

---

## Balance

`GET /api/user/profile-summary` → `data.totalCreditsRemaining`, with
`creditItems[]{type, creditsRemaining, expiresAt}` for the expiry breakdown.

**Do not use `/api/user/quota`.** It reports only `freeCreditsTotal` (300) and
omits the activity credits (measured ≈ 5000), so a well-funded account would
look nearly empty. This module never calls it, and the test suite asserts that
it does not.

Negative balances clamp to 0. Unlike check-in, a balance failure **is** a Go
error (`core.BalanceProvider` has no "refusal" concept).

---

## The client version

`GET https://api-overmind.youdao.com/openapi/get/luna/hardware/lobsterai/prod/update`
answers:

```json
{"data": {"value": {"version": "2026.9.4", "…": "…"}}, "code": 0, "msg": "OK"}
```

`code` and `msg` sit **outside** `data` on this endpoint, unlike everywhere else,
so the response is decoded by hand rather than through the envelope helper. The
real version is date-style (`2026.9.4`), not semver; the hardcoded `0.1.0` is a
value the vendor tolerates but it is not the truth.

The version is fetched asynchronously when a chat starts (a chat must not wait on
a third-party manifest service) and synchronously, but bounded, before check-in
and before the sign-in exchange — the check-in query parameter is required, and
signing in with a fake version is worse than waiting 10 s. A payload whose
`code` is non-zero is rejected even though `code` lives outside `data`: if the
endpoint reports an error, its `data.value.version` is not trustworthy.
`fetchVersion` only accepts strings matching
`^(\d+(?:\.\d+)*)(?:-[0-9A-Za-z.-]+)?$`, so `latest`, `v0.1.0` and an empty
string all fall back to `client_version`.

---

## Account pool, cooldowns and errors

Selection is least-recently-used first. An account is selectable when it is
enabled, carries a token, is not parked, and is under `max_in_flight`.

`Chat` walks up to **3** accounts (`maxRotate = 3`, counting the first). **Every
failure kind rotates**, including 404 and an unrecognised 4xx — the
production-validated reference does the same (`handler.go:218-243`), because
stopping on the first error turns one bad account into a total outage.

| Upstream condition | Kind | Effect | Retry another account |
| --- | --- | --- | --- |
| `402`, or a credit keyword (`积分不足`, `额度不足`, `余额不足`, `quota exceeded`, `insufficient credit`, …) | hard credit | park for `quota_cooldown` (12 h) | yes |
| envelope code `40100`/`40101`, `token rejected`, `refresh token was rejected`, or HTTP 401/403 | session dead | **disable** the account | yes |
| `429` | soft rate | park for `cooldown` (60 s) | yes |
| `404` | not found | park for `cooldown`, **no** error count | yes |
| any `5xx` | server | 3 consecutive ⇒ park for 10 m | yes |
| any other `4xx` | client | 3 consecutive ⇒ park for 10 m | yes |

When no account can serve the request at all, `Chat` returns `core.ErrBusy` →
**HTTP 429** (`rate_limit_exceeded` at the gateway — an in-flight ceiling is
backpressure, not an outage) or `core.ErrNotConfigured` → **HTTP 503**
(`no_healthy_account`) when no account exists at all; `core.ErrUnsupported` →
**400**. A vendor refusal (`402`, an out-of-credit 429) is a classified error
carrying `core.FailureQuota`, not a special result type — the gateway decides
what to do with it.

Unsupported requests are refused **before** any network call:

* a `nil` request, or a blank model → `core.ErrUnsupported`;
* any message carrying an image → `core.ErrUnsupported`;
* a cancelled context → the context error.

---

## Streaming

`Chat` always returns a `core.Stream` over the upstream SSE body, and the
caller's `Stream` flag decides only whether the frames are delivered
incrementally or aggregated into one `EventDelta` followed by the tool calls,
the usage and `EventDone`.

* Frames are decoded as they arrive. `data:` **with or without a space** after
  the colon is accepted, as are CRLF and a final frame with no trailing blank
  line.
* After `[DONE]` the stream **stops reading** rather than draining to EOF. The
  vendor's own client drains; this module does not, because a server that leaves
  the connection open would otherwise hang a request. `Close()` tears the body
  down instead. **This is a deliberate divergence from the reference.**
* `reasoning_content` is accumulated separately and delivered on `EventDelta`.
* Tool-call fragments are **merged by `index`**, and `arguments` fragments are
  **concatenated**, never overwritten — the first fragment carries the id and the
  name, the rest carry only argument slices. Both the streaming path and the
  aggregating path merge, because leaking half a tool call is worse than
  emitting none.
* A whole `message` (instead of a `delta`) is honoured, but **only when nothing
  has been accumulated yet** — otherwise the content is concatenated twice and
  `"A"` becomes `"AM"`.
* `Recv` reports `io.EOF` exactly once; `Close` is idempotent and releases the
  account's in-flight slot.
* `tool_choice` is normalised: `""`, `"none"`, `null` or absent deletes the
  field; a map is kept.
* The chat request sends `Authorization`, `Content-Type`, `Accept:
  text/event-stream, application/json`, `User-Agent`, `X-LobsterAI-Client-Capabilities`
  and `X-LobsterAI-Client-Version`. It does **not** send `X-Domain`,
  `X-Product`, `X-Product-Code`, any `X-IDE-*` header, or `prompt_cache_key`.

---

## Files in `Deps.DataDir`

| File | Contents |
| --- | --- |
| `accounts.json` | The credential store: one record per account, including the cooldown in force when it was last written. This is the **authority** for a cooldown, which is what makes a 12 h quota park survive a restart. |

With no `DataDir` the module still works but persists nothing — and it will not
write a credential file into the process working directory, because a credential
in the CWD is worse than a lost account.

Writes go through `core.WriteJSONAtomic`. No token is ever handed to the panel:
`core.AccountRecord.Fields` carries `uid`, `nickname`, `expires_at` and
`has_refresh_token`, never a secret, and anything reaching a log line or a
`Status` detail goes through `core.Redact` and `core.MaskSecret`.

### Discovery and import

`Discover` looks for credential files in `Deps.DataDir` and `Deps.DataDir/import`
only. It does not guess at another product's private directory — a path that
happens to exist is not evidence that the file is ours, and importing from one
would be a lie dressed as a feature.

`ImportBundle` accepts a vendor JSON file in any of the shapes
`decodeCredentialsBody` understands: the module's own store shape, a bare array,
a single object, or the vendor's nested
`{"auth": {…}, "account": {"uid": …, "userId": …, "yid": …, "nickname": …}}`.

---

## How `Status` is rendered

`Status()` makes **no network calls**. `Models` is the last good catalogue or the
built-in list; `Accounts` is one `core.AccountStatus` per account with the state
(`ready` / `cooling` / `invalid`) and the last (redacted) error. `Detail` is one
human line: the config-rejection message, or a prompt to sign in, or the pool
summary plus the client version and whether it is live.

`PoolStats` reports the in-flight count and how many accounts are at their
ceiling.

---

## Known gaps

* **Live-validated 2026-10-01 against a real signed-in LobsterAI account**
  (`lobsterai:104911`): a non-streaming `POST /v1/chat/completions` returned a
  real completion with `reasoning_content`, a streaming request returned SSE with
  `reasoning_content` and usage, the catalogue refresh pulled 29 live models, and
  `/balances` answered `399/399 积分`. The check-in reward amount (≈100 credits)
  is still the document's number, not an observation.
* **`ModelLimitsProvider` is not implemented**, so a request that omits
  `max_tokens` reaches the vendor without a cap. The live
  `/api/models/available` was re-read on 2026-10-01 and confirmed to publish **no**
  output budget on any of its 29 rows. See above.
* **The 19-model built-in catalogue is recon-time knowledge** and can drift; the
  live list is authoritative and is what the module serves once a refresh
  succeeds. Its `context_length: 131072` is a bridge estimate used only for rows
  that publish no `contextWindow`.
* **The version endpoint's `code` semantics are a guess.** The document shows
  `code: 0` and puts `code`/`msg` outside `data`, but never states that a
  non-zero `code` must be treated as a failure. This module treats it as one.
* **The sign-in flow needs a human.** Nothing here can complete it on its own,
  and the module will not open a browser for you.
* **No environment-variable credentials**, unlike some sibling modules.
* **No proxy support in-module.** `Deps.HTTPClient` is used when supplied, so the
  host's proxy setting (if any) is what applies.
* **One credential per uid.** Two accounts with the same uid collapse into one
  pool entry.

---

## Testing

```sh
gofmt -l ./clients/lobsterai
go vet ./clients/lobsterai/
go test -count=1 -timeout 20m ./clients/lobsterai/
```

Everything runs **offline** and needs no credential: upstream payloads are
inline string fixtures and the transport is an `httptest.Server` that records
every request (`method`, `path`, `query`, `header`, `body`) so a test can assert
the exact wire shape. The suite pins the exchange and renewal bodies, the
absence of `Authorization` on renewal, `stream: true` on every chat, the
aggregated answer for a non-streaming caller, the envelope being checked on the
account endpoints and **not** on the chat body, the balance coming from
`profile-summary` with no call to `/api/user/quota`, the three-call check-in
order, and the version fetch reading `data.value.version`.

---

## Provenance

**Clean-room.** The wire protocol was read from
`docs/lobsterai-integration-plan.md` (a plan for a *different*, TypeScript
project) and reimplemented in Go against the standard library. No code was
copied. Endpoint paths, header names, field names and error codes are
interoperability facts; the implementation is original.

### Account-ban risk

The module presents itself as the first-party desktop client — the
`LobsterAI/0.1.0` user agent, the `kimi-k3-agentic-v1` capability header, and
`platform=win32` on check-in even off Windows. The multi-account pool with
rotation and a 12 h quota cooldown is, in effect, automation for collecting
free-tier credit across accounts, and that is the behaviour most likely to be
read as abuse. Run it against one account you own.
