# `qwenwork` client

A `client2api` client module that exposes **QwenWork / QoderWork** (千问办公, the CN
desktop agent backend at `gateway.qwenwork.cn`) as an OpenAI-compatible chat
endpoint.

* Route prefix: `qwenwork/<model>` (e.g. `qwenwork/pro`)
* Upstream wire protocol: a signed **agent envelope** POSTed to an SSE endpoint
  (`POST /algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common`)
* Auth: a **COSY** signature over a per-session RSA-wrapped AES key, plus a
  PKCE(S256)-minted bearer credential
* Dependencies: **standard library only** — nothing outside `client2api/internal/core`
  and the Go standard library is imported.

The upstream is not OpenAI-shaped in either direction: the request is an "agent
envelope" carrying `chat_context`, `model_config`, `system`, `messages`, `tools`
and `parameters`, and the response is an SSE stream of framed envelopes whose
`body` is an OpenAI *chunk*. This module translates in both directions and owns
the credential, the COSY session cache, and the multi-account cooldown pool.

---

## Model ids

| Model | Notes |
| --- | --- |
| `pro` | default; also reachable as `auto`, `advanced` |
| `flash` | also reachable as `lite` |
| `qwen3.8-max-preview` | also reachable as `max`, `qwen3.8-max` |

The built-in catalogue above is what `Models()` answers with before (or
without) a live account, so a caller can always discover the ids; a live
`GET /algo/api/v2/model/list` (cached for `models_ttl`, default 5 min) replaces
it. `Models()` **never blocks**: the refresh runs in the background and is
throttled to one attempt per 30 s, because the registry calls `Models` on every
module for a bare model name.

An unrecognised model id is passed through to the vendor verbatim (after
lower-casing) rather than rejected here; the vendor is the authority.

---

## Configuration

The module reads the JSON object under `clients.qwenwork` in the host config
(`configs/client2api.json`). **Every key is optional**, and the module is usable
with no configuration at all — credential discovery is usually enough.

An absent object is the same as `{}`. A **malformed** object is logged and then
replaced with defaults (`clients.qwenwork` is never a construction failure); the
factory logs a line containing `invalid config` and Status reports not-ready.

See `config.example.json` for a copy-paste starting point.

```jsonc
{
  "clients": {
    "qwenwork": {
      "base_url": "",
      "user_agent": "",
      "accounts": [ /* see below */ ],
      "access_token": "",
      "uid": "",
      "nickname": "",
      "email": "",
      "refresh_token": "",
      "expires_at": "",
      "max_attempts": 3,
      "models_ttl": "5m",
      "models_timeout": "20s",
      "chat_timeout": "10m",
      "idle_timeout": "2m",
      "cooldown": "60s",
      "short_cooldown": "15s",
      "quota_cooldown": "24h",
      "refresh_margin": "30m",
      "max_tokens": 32000,
      "login": false,
      "client_id": "",
      "redirect_uri": "",
      "device_auth_url": "",
      "poll_interval": "3s",
      "login_timeout": "10m"
    }
  }
}
```

### Top-level keys

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `base_url` | string | `https://gateway.qwenwork.cn` | Origin every endpoint is built from. A trailing slash is tolerated (paths are joined without doubling it) and the value is otherwise reported back verbatim. |
| `user_agent` | string | `qoderwork/1.0.5` | `User-Agent` on every request. |
| `accounts` | array | `[]` | Explicit credential list, tried ahead of the shorthand and the environment. |
| `access_token` | string | `""` | Single-account shorthand: the COSY bearer credential. |
| `uid` | string | `""` | Shorthand: the vendor's user id (embedded in the COSY header). |
| `nickname` / `email` | string | `""` | Shorthand: display identity, used for the session and the Status label. |
| `refresh_token` | string | `""` | Shorthand: renews `access_token` on a 401/403. |
| `expires_at` | string | `""` | Shorthand: unix seconds, unix **milliseconds** (>1e12), RFC3339(Nano), or `2006-01-02 15:04:05`. Unparseable ⇒ 0 = unknown (never expires on our side). |
| `max_attempts` | int | `3` | Upper bound on upstream attempts per `Chat` (at most one per account). |
| `models_ttl` | duration | `5m` | How long a live catalogue is served from cache. |
| `models_timeout` | duration | `20s` | Deadline for the catalogue lookup *and* for a token refresh. |
| `chat_timeout` | duration | `10m` | Deadline for one `Chat` call, opened in `Chat`. |
| `idle_timeout` | duration | `2m` | The upstream accepts the request then goes quiet for this long ⇒ the stream fails with `upstream stopped sending data (idle timeout)`. |
| `cooldown` | duration | `60s` | Park duration for an auth failure. A client error parks nothing. |
| `short_cooldown` | duration | `15s` | Park duration for a transient/network failure. |
| `quota_cooldown` | duration | `24h` | Park duration when the account is out of credit. |
| `refresh_margin` | duration | `30m` | Refresh an access token proactively when it has less than this left. |
| `max_tokens` | int | `32000` | Default completion budget; capped at 32768 (the vendor's ceiling). |
| `login` | bool | `false` | Start the PKCE device flow at construction time and print the URL. |
| `client_id` | string | the desktop app's | PKCE `client_id`. Override with your own registration. |
| `redirect_uri` | string | `qwenwork-cn://` | PKCE `redirect_uri`. |
| `device_auth_url` | string | `<base_url>/device/selectAccounts` | Full override of the authorisation URL (the query string is still built by us). |
| `poll_interval` | duration | `3s` | How often the device flow asks whether the human has authorised. |
| `login_timeout` | duration | `10m` | Overall budget for one device-authorisation flow. |

**Durations** accept both a Go duration string (`"90s"`, `"2h"`) and a bare JSON
**number**, which is read as seconds: `"cooldown": 300`, `"cooldown": "300"` and
`"cooldown": "5m"` are the same thing. A value that is neither (`0`, `-5s`,
`"nonsense"`, an object) falls back to the default rather than failing the
module.

### `accounts[]` entries

| Key | Type | Meaning |
| --- | --- | --- |
| `label` | string | Human note for the panel; shown as the account's `Note`, never as the token. |
| `uid` | string | Vendor user id. |
| `nickname`, `email` | string | Display identity. |
| `access_token` | string | The COSY bearer credential. **Required** — an entry without it is dropped. |
| `refresh_token` | string | Renews `access_token` on 401/403. |
| `expires_at` | string | As above. |
| `disabled` | bool | Start the account parked. It is still reported, but never selected. |

### Environment

An access token can be handed to the process instead of being written to a
config file. Explicit config **wins** over the environment (`mergeAccount`
overlays the config onto the stored record).

| Variable | Used for |
| --- | --- |
| `CLIENT2API_QWENWORK_TOKEN` | `access_token` |
| `CLIENT2API_QWENWORK_REFRESH_TOKEN` | `refresh_token` |
| `CLIENT2API_QWENWORK_UID` | `uid` |
| `CLIENT2API_QWENWORK_NICKNAME` | `nickname` |
| `CLIENT2API_QWENWORK_EMAIL` | `email` |
| `CLIENT2API_QWENWORK_LOGIN` | `1`/`true`/`yes`/`on`/`y` ⇒ same as `"login": true` |

### Transport fingerprint

Two keys replace the TLS ClientHello, so vendor-facing calls stop carrying Go's
handshake.  Both are off by default, and an unset `tls_profile` keeps exactly
the client the core supplied:

| key | values | default | meaning |
| --- | --- | --- | --- |
| `tls_profile` | a name from `fingerprint.Profiles()` | `""` | imitate that client's ClientHello (`chrome`, `node`, `firefox`, …) |
| `tls_protocol` | `"h2"`, `"http/1.1"`, `""` | `""` | pin the ALPN; empty offers the profile's own list |

A named profile becomes `Client.vendorClient`, which `httpClient()` returns
before anything else, so there is no path by which a request escapes it.  The Go
hello is one of the few things about a proxy a vendor can see without reading a
single header.

Only the TLS half is closed: the h2 frames are still written by
`golang.org/x/net/http2`, so an h2 connection remains a Go-shaped h2
connection.  `tls_protocol: "http/1.1"` sidesteps that entirely.  A proxy in
`Deps.Proxy` forces HTTP/1.1 whatever you set, because `x/net/http2` cannot
tunnel through one.

This module ships **no** default profile.  `qoderwork/1.0.5` is a custom UA and
says nothing about which ClientHello the real client sends, so a profile must be
chosen from a capture of that client rather than guessed.  See the package
comment in `internal/fingerprint` for how the one captured profile (`node`) was
derived, and note that Go cannot reproduce an Electron app's BoringSSL hello
byte for byte — a uTLS parrot is the closest available approximation.

---

## Credential discovery

Credentials are looked for in this order, and the pool is rebuilt from the
union on every (re)load:

1. `clients.qwenwork.accounts[]` — explicit list.
2. `clients.qwenwork.access_token` (+ `uid`, `nickname`, `email`,
   `refresh_token`, `expires_at`) — single-account shorthand.
3. `CLIENT2API_QWENWORK_TOKEN` (+ the other `CLIENT2API_QWENWORK_*` variables).
4. **`accounts.json` in `Deps.DataDir`** — the module's own credential store,
   written by the device flow and by a token refresh.

`accounts.json` is read on the **first** load only. After that the configured
set is the roster, so an account the config stopped naming is dropped rather
than resurrected from a stale file; `reload` (used after a completed device
flow) is the one path that re-reads it. This is also why a pool with **no**
config but a populated `accounts.json` is fully usable — the store is a
credential source in its own right, not just a cache.

### The device-authorisation flow

There is no login subcommand in `cmd/`, and this module may not add one, so the
PKCE(S256) flow is reachable two ways:

* `RunDeviceFlow(ctx, deps, out)` is **exported and self-contained** — a future
  `client2api -login qwenwork` would call it with the same `core.Deps`. It
  prints the authorisation URL, polls `<base_url>/api/v1/deviceToken/poll`
  every `poll_interval`, and on success writes the credential to `accounts.json`.
* `"login": true` (or `CLIENT2API_QWENWORK_LOGIN=1`) makes `New` start it in the
  background when nothing is usable yet, printing the URL to **stdout**. That is
  enough for an operator watching the log, and Status then says which URL to
  open. The flow needs a human to open the URL and click "authorise"; nothing
  here can complete it on its own.

While a flow is in progress a `login.json` lives in `Deps.DataDir` holding the
URL, the PKCE verifier and an expiry, so a restart does not lose a flow the
human is halfway through. It is removed when the flow finishes, and a pending
record names the URL in `Status.Detail`.

A completed authorisation **revives** the account it names: `upsertStoredAccount`
clears `disabled`, `cooldown_until` and `last_error` on the existing record and
re-writes it. A fresh credential must never be shadowed by an old verdict.

---

## Files in `Deps.DataDir`

Everything this module persists lives under `core.Deps.DataDir`. With no
`DataDir` the module is fully functional and simply persists nothing — it can
never write outside that directory.

| File | Contents |
| --- | --- |
| `accounts.json` | The credential store: one record per account, including the cooldown that was in force when it was last written. This is the **authority** for a cooldown, which is what makes a 24 h quota park survive a restart. |
| `state.json` | The health mirror: the same verdicts in a shape that carries **no credential** (`state`, `cooldown_until`, `last_error`, `last_used`, `disabled`), written behind a 5 s debounce. Read back only for the LRU timestamp and the cooling/exhausted label. |
| `login.json` | A device-authorisation flow in progress. |

A credential is never written to `state.json`, and no token, key or cookie is
ever logged in full — anything that reaches a log line or a `Status` detail goes
through `cleanErrorText` (which strips HTML, truncates to 200 chars and redacts
labelled secrets, bare JWTs and COSY triples) and `core.MaskSecret`.

---

## Account pool, cooldowns and errors

Selection is **least-recently-used first**, with a stable sort so equal
timestamps keep config order. An entry is *usable* only if it is not disabled,
carries a token, is not parked, and is not within 60 s of expiry (an account
that expires mid-request is no use).

`Chat` walks up to `max_attempts` accounts. On a failure the account is parked
in memory *and* on its record, and the next account is tried — **except for a
`client` failure, which parks nothing**, because the request was wrong rather
than the credential:

| Upstream condition | Classification | Park | State | Retry another account | Notes |
| --- | --- | --- | --- | --- | --- |
| transport error, no response | network | `short_cooldown` (15 s) | `cooling` | yes | Also drives Status's "upstream unreachable" line. |
| `401`, `403` | auth | `cooldown` (60 s) | `cooling` | yes | **One** `refresh_token` retry first, and that retry costs no attempt slot. If the refresh itself fails, the account is **disabled** (`markDead`) until a fresh authorisation revives it. |
| `402 Payment Required` | quota | `quota_cooldown` (24 h) | `exhausted` | yes | |
| `429` whose body carries `code` `14018`, `14019` or `14020` — or a credit keyword (`积分不足`, `额度不足`, `余额不足`, `quota exceeded`, `insufficient credit`, …) | quota | `quota_cooldown` (24 h) | `exhausted` | yes | 14018 is the vendor's "积分不足" (out of credit); 14019/14020 are the neighbouring codes on the same path and are treated identically. |
| `429` with no credit signal | transient | `short_cooldown` (15 s) | `cooling` | yes | |
| any `5xx` | transient | `short_cooldown` (15 s) | `cooling` | yes | |
| any other `4xx` | client | none | `ready` | **no** | Our request was wrong; it will be wrong on every account, so the loop stops. The credential itself is healthy, so it is not parked. |
| `Retry-After`, `X-Ratelimit-Reset`, `Retry-After-Ms` | — | lengthens the park above | — | — | Sanity-capped at 2 h so a broken or hostile header cannot park an account for a week. It can only ever **lengthen**, never shorten. |
| a COSY-session build failure | client | none | `ready` | yes | A local crypto failure, not the credential's fault. Counts as an attempt. |

Order matters in the classifier: `402` is always quota; `429` is quota only
when the body says so; `5xx` before the generic `4xx` branch.

When no account can serve the request at all, `Chat` returns
`core.ErrNotConfigured` → **HTTP 503** at the gateway, not the last upstream
429. `core.ErrUnsupported` → **400**, and any other error → **502**.

Unsupported requests are refused **before** any network call (and even with no
credential at all):

* a `nil` request, or a blank/whitespace model → `core.ErrUnsupported`;
* any message carrying an image part (`image_url`, or an `ImageURL`) →
  `errImageUnsupported`. The documented envelope has nowhere to put an image,
  and answering without it would be a silently wrong answer;
* a cancelled context → the context error.

---

## Daily check-in

One action, `daily`, over two Sash endpoints on the same host as chat:

1. `GET /sash/api/v1/me/daily-check-in/status` — reads `status`, one of
   `CLAIMABLE`, `CLAIMED_TODAY`, `DISABLED`, `NOT_STARTED`, `ENDED`, plus
   `rewardCredits`, `nextClaimAt`, `currentStreakDays`, `totalClaimDays`,
   `totalRewardCredits`, `lastClaimedAt` and `rewardExpiresAt`.
2. `POST /sash/api/v1/me/daily-check-in/claim` — sent **only** when the status is
   `CLAIMABLE`. The reply's `result` is `CLAIMED` or `ALREADY_CLAIMED`.

Both replies are wrapped in the vendor's envelope. The module unwraps `data`
exactly as the vendor's own `unwrapData()` does — an object whose `data` member
is also an object — and reports an unrecognised status or result as
**unrecognised** rather than forcing it into one of the known values.

The two-step shape is deliberate: this endpoint has **no idempotency key**, so
claiming blind would make every click a write. Asking first means a repeat click
on an already-claimed day costs one read and no write.

`CLAIMED_TODAY` and `ALREADY_CLAIMED` are **success** (`OK == true`, with
`data.already_done == true`), the same rule the other check-in modules follow. An
upstream refusal is a **result**, not a Go error; a Go error is returned only for
an account id that is not in the pool. A client with no account at all is
`core.ErrNotConfigured`, reported as a refusal. `CheckinActions` returns nothing
while the pool is empty — a button that cannot act makes the operator pay for the
discovery with a failed request.

Only four headers are sent: `Accept`, `Content-Type`, `User-Agent: QoderWork` and
`Authorization`. The vendor's own client also sends six `X-QwenWork-*` identity
headers naming its app version, build number, platform, architecture and release
channel. This module sends **none** of them: it is not that binary, and inventing
a version number to look like one would put a lie in the request.

Account health follows the chat path: a 404 or any other 4xx is `client` and does
**not** park the account (a request we got wrong is not a credential problem), a
402 — or a 429 whose body carries a credit marker — parks it for a day, a 401 is
retried once after a token renewal, and a renewal the vendor also rejects marks
the account **dead** instead of cooling it. A local transport failure returns the
raw Go error and leaves the pool untouched.

**Never verified live.** The vendor only offers the button once the server has
pushed the `announcement:credits-growth-card` operation with `checkinEnabled`
true. This machine's server has never pushed it — the desktop log repeats
`Dispatch null on initial sync for opt-in registration with no remote value
{"namespace":"operations","key":"announcement:credits-growth-card"}` — so a live
`404` is the expected answer here and the success path has not been observed
against a real account. The wire shapes above come from the vendor's own parsers
in the QwenWorkCN bundle (`DAILY_CHECK_IN_STATUS_PATH`,
`DAILY_CHECK_IN_CLAIM_PATH`, `FETCH_TIMEOUT_MS = 8e3`, `USER_AGENT = "QoderWork"`),
never from guesswork.

---

## Credit balance

`AccountBalance` implements `core.BalanceProvider` through the desktop
client's own account-context endpoint:

`GET /api/v1/adapter/user/account-context?include=user,plan,quota,page,data_sharing`

The endpoint is opened with the account's bearer token. The response may be
wrapped in `data`; the user quota is read from `quota.user_quota`,
`quota.userQuota`, or the `quota` object itself. The module uses the vendor's
`remaining` value when present and otherwise computes `max(total - used, 0)`.
The vendor's default `credits` unit is rendered as `积分` in the panel.

A rejected access token is refreshed and the read is retried once, matching the
account-context behavior of the desktop client. A malformed reply is an error;
the panel must not turn an unknown quota into a fabricated zero.

---

## Streaming

`Chat` always returns a `core.Stream` over the upstream SSE body.

* Frames are decoded as they arrive. A `[DONE]` frame ends the stream and
  **nothing after it is delivered**.
* The vendor wraps chunks in an envelope; a bare non-envelope chunk (such as a
  trailing usage-only frame) is passed straight to the chunk parser.
* Emitted **at most once each**: `EventUsage` and `EventDone`.
* `Recv` reports `io.EOF` exactly once at the end; `Close` is idempotent and
  closes the upstream body and releases the request context.
* Finish reasons normalise to `stop` / `length` / `tool_calls` /
  `content_filter`; a stream that saw tool calls reports `tool_calls`.
* Usage maps `prompt_tokens`, `completion_tokens`, `total_tokens`,
  `reasoning_tokens` and `cached_tokens` from the aliases the vendor uses
  (`input_tokens`/`output_tokens`, `prompt_cache_hit_tokens`,
  `cache_read_input_tokens`, `prompt_tokens_details.cached_tokens`,
  `completion_tokens_details.reasoning_tokens`, …), and derives the total when
  only its parts are present.
* A stream that produces no data at all reports `errEmptyStream` as an
  `EventError`, never a silent empty success.

---

## How `Status` is rendered

`Status()` is deliberately cheap — **no network calls**, one small file read
(the pending `login.json`) and a mutex-guarded snapshot. The panel polls it
every 10 s, so it must never block.

`Status.Models` is the cached catalogue, or the built-in three when the cache is
cold. `Status.Accounts` is one `core.AccountStatus` per account:

| Field | Value |
| --- | --- |
| `ID` | `id` → `uid:<uid>` → `tok:<first 16 chars of the token>` |
| `Label` | `nickname (uid)` → `nickname` → `uid` → `email` → `account` — never the token |
| `Enabled` | `!disabled` |
| `State` | `ready` / `cooling` / `exhausted` / `invalid` / `unknown` |
| `Note` | the last (cleaned) error, with ` (Ns left)` appended while a cooldown is live |
| `ExpiresAt` | RFC3339 UTC when the token expiry is known |
| `Extra` | `failures`, `uid`, `expires_in`, `cooldown_until` |

`State` and `Ready` are computed from one function, so two accounts in the same
condition can never be reported differently, and `ready()`/`pick()` can never
select an account that `Status` calls cooling or exhausted.

`Status.Detail` is one human line, chosen in this order:

| Condition | `Ready` | `Detail` |
| --- | --- | --- |
| no accounts at all | false | `no credential: set clients.qwenwork.access_token, CLIENT2API_QWENWORK_TOKEN, or run the device flow ("login": true)` — or `device flow not completed: open <url>` while one is pending |
| at least one usable account | **true** | `<summary>; models: <ids>`, where summary is `N ready, M invalid, …` |
| nothing usable, last failure was a network error | false | `upstream unreachable: <msg>` |
| nothing usable (any other reason) | false | `all accounts unavailable (<summary>)` plus `; last error: <msg>` |

A not-ready detail from the last two rows gets ` (last attempt <age> ago)`
appended, where the age is rendered `0s` / `Ns` / `Nm` / `Nh`.

---

## Known gaps

* **No chat tool round-trip is exercised live.** Tools are sent in the vendor's
  `tools` shape and tool-call deltas are streamed back, but the end-to-end loop
  against a real account has not been run from here (no credential available in
  this environment); the offline fixtures are the only evidence.
* **Images are refused, not translated.** `image_url` parts return
  `errImageUnsupported` even though the envelope carries `is_vl: true`.
* **The daily check-in has never been claimed live.** The endpoints, the header
  set and both response shapes come from the vendor's own parsers, and every
  branch is covered by offline fixtures, but the server has never pushed the
  `announcement:credits-growth-card` operation to this account, so a live call
  answers `404`. The success path is therefore unobserved against a real
  account — see "Daily check-in" above.
* **Credit balance is read live, but expiry tranches are not exposed.**
  The account-context response is parsed with the QwenWorkCN client's own rules,
  including `user_quota`/`userQuota` and the `remaining` or `total - used`
  calculation. The reply does not provide per-tranche expiry buckets, so
  `Expiring` and `EarliestAt` stay zero rather than guessing from plan dates.
* **`/api/v1/userinfo` is declared and unused as well.** It was outside this
  cleanup, so it stays, but it has the same problem: a constant with no caller
  looks like a feature that exists.
* **A 403 is reported as an auth failure; no edge-WAF signal exists.** `classify`
  maps 401 and 403 alike to `kindAuth`, which becomes `core.FailureAuth` --
  retryable, so the gateway rotates. There is no risk-control code in the vendor
  vocabulary this module knows, and no WAF-shaped 403 has ever been observed
  here (**UNVERIFIED**). If one shows up, the fix belongs in `classify`, not in
  the gateway.
* **`reasoning_tokens` and `cached_tokens` are best-effort.** They are read from
  the aliases the vendor has been observed to use; an unannounced rename means
  they silently report zero while the totals stay right.
* **The model list is recon-time knowledge and can drift.** A stale id is passed
  through and comes back as whatever the vendor answers.
* **The `pro`/`flash`/`qwen3.8-max-preview` catalogue is a built-in guess** built
  from the vendor's desktop app; only a live `model_list` is authoritative.
* **No `X-Ratelimit-*` accounting.** Only the reset header is honoured, and only
  to lengthen a park.
* **`login.json` is not garbage-collected on crash.** An abandoned flow leaves
  the file until the next flow overwrites it; it is ignored once expired.
* **One credential per `uid`.** Two accounts with the same `uid` collapse into
  one pool entry (that is the identity the vendor uses).
* **No proxy support in-module.** `Deps.HTTPClient` is used when supplied, so the
  host's proxy setting (if any) is what applies.

---

## Testing

```sh
go vet ./clients/qwenwork/...
go test ./clients/qwenwork/... -v
go test ./... -count=1
```

Everything runs **offline** and needs no credential: upstream payloads are
inline string fixtures and the transport is a fake `http.RoundTripper`
(`fakeTransport` records every `recordedRequest` so a test can assert method,
URL, headers and body). Nothing in this module's suite requires
`CLIENT2API_LIVE`, and no test touches the network.

---

## Provenance and licence

**Clean-room.** The reference implementation `qwenwork2api-makers` (under
`_upstream/` in the lab tree) ships **no licence at all**. No code, comment,
identifier, or file structure from it was copied into this module. It was read
**only** to learn the wire protocol; the cryptography here is Go's `crypto/*`
written from the RFCs, not a transliteration of the reference's JavaScript.

What was read, and what it was used for:

* the chat endpoint, its query parameters and the SSE framing — protocol facts;
* the agent-envelope field names and the `chat_context` / `model_config` /
  `parameters` shape — protocol facts;
* the COSY signature scheme (AES-128-CBC with `key+IV`, RSA PKCS#1 v1.5 wrap of
  the temp key, MD5 over `payloadB64/cosyKey/date/body/path/algo/algo`) and the
  fixed client constants (`cosyVersion`, `ideVersion`, channel, platform) —
  protocol facts, reimplemented against Go's standard library;
* the device-authorisation endpoints, the PKCE parameters and the grant field
  names — protocol facts;
* the vendor's error codes (`14018` "积分不足" and neighbours) and the
  credit-exhaustion wording in both languages — protocol facts;
* the account-pool policy (LRU, ≤3 attempts, refresh-and-retry, 15 s / 60 s /
  24 h cooldowns) — a **policy**, reimplemented here.

The request/response schema, the header names and the error codes are
interoperability facts, not creative expression; the Go implementation is
original. No file from the reference is vendored, and this repository remains
dependency-free (stdlib only).

### Security note

Never log a credential. Every failure message that can reach a log line or a
`Status` detail is passed through `cleanErrorText`, which redacts labelled
secrets (`bearer …`, `access_token`, `refresh_token`, `cosy-key`,
`authorization`, …), bare JWTs and `COSY.<…>.<…>` triples, and truncates to 200
bytes. `core.MaskSecret` (an 8-character prefix plus the length) is the
redactor used inside `cleanErrorText` for every labelled secret, bare JWT and
`COSY.<…>.<…>` triple, so no secret reaches a log line or a `Status` detail in
full. The credential store and the health mirror both live under `Deps.DataDir`,
and the health mirror is constructed so it *cannot* hold a secret.

**One caveat, pinned by the test suite.** `Status.Accounts[].ID` is the
account's `id`, and `account.id()` (`pool.go:60-71`) falls back to
`"tok:" + AccessToken[:16]` when a record carries neither an `id` nor a `uid` —
a 16-character token prefix. `qwenwork_test.go:1710-1719` pins that behaviour, so
it is deliberate here and this module does not override it. In practice a stored
record always has a `uid` (the device flow mints one) and a configured record
should set `uid` or `id`; only a hand-written entry with nothing but an
`access_token` exposes the prefix. The panel serialises `core.Status` verbatim
(`internal/panel/panel.go:35`), so that prefix is visible on the panel. Supply
`uid` (or `id`) on every configured account to avoid it. `Label` never contains
any part of the token.

### Account-ban risk

Read this before pointing the module at a real account.

* The module reuses the **first-party desktop app's** `client_id`
  (`e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb`) and `redirect_uri`
  (`qwenwork-cn://`) for the device flow, and signs requests with that app's
  COSY constants (`cosyVersion 1.1.18`, `ideVersion 1.0.5`, `qoderwork/1.0.5`
  user agent). The upstream therefore sees this traffic as that desktop client.
  Register your own `client_id`/`redirect_uri` and override `user_agent` if you
  can, and treat a ban as a real possibility regardless.
* The multi-account pool with LRU rotation and a **24 h quota cooldown** is, in
  effect, automation for farming free-tier credit across accounts. That is what
  the policy does by design, and it is the behaviour most likely to be
  classified as abuse. Run it against one account you own.
* Risk scoring can accumulate server-side. A cooldown here is a local
  bookkeeping decision: it does not clear anything the vendor has recorded.
* An account can be disabled (`markDead`) by this module without any signal from
  the vendor beyond a failed refresh; that is recoverable only by re-running the
  device flow.
* **This module was not validated against a live account.** The protocol details
  come from documentation and a read of the reference; some are unverified (see
  *Known gaps*).
