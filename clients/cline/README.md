# `cline` client

A client module for the **Cline** (cline.bot) upstream. It speaks the vendor's
OpenAI-shaped inference API, carries a WorkOS-minted credential, and merges three
model catalogues into one list.

- **Route prefix** `cline/` — a bare `cline-free/deepseek-v4.1-flash` resolves here.
- **Upstream wire protocol** OpenAI `POST /api/v1/chat/completions`, streamed SSE.
- **Auth** a WorkOS device-code flow exchanged for a Cline token (`workos:eyJ…`).
- **Dependencies** standard library only.

Cline's own client is the reference for every detail below. Where the vendor
deviates from the OpenAI convention (a prefixed bearer token, a camelCase refresh
body, an account id that is *not* the JWT subject) this module follows the
vendor, because following the convention is what the upstream rejects.

---

## Model ids

`Models` merges **three** sources. Reading only the first one is the mistake this
module exists to avoid: `/api/v1/models` lists roughly 460 ids and contains **no
`cline-free/*` id at all**, so a client that trusts it alone shows the operator no
free model whatsoever.

| Source | Shape | What it contributes |
| --- | --- | --- |
| `GET /api/v1/models` | `{id, object, created, owned_by}` | the metered catalogue (~460 ids) |
| `GET /api/v1/ai/cline/recommended-models` | `{id, name, description, tags}` plus a top-level `free` array | display names, descriptions and **every free id** |
| built-in table | compiled in | the five free models, and a usable list when the network is down |

Merge rule: built-in **<** `/api/v1/models` **<** recommended. The recommended
source wins because it is the only one that carries a display name and the free
verdict. `Extra["free"]` is written unconditionally by the `/api/v1/models` pass,
so a lower-precedence guess can never survive a higher-precedence answer.

### Free models

| id | name | context | max output | images |
| --- | --- | --- | --- | --- |
| `stealth/space-bunny-alpha` | Space Bunny Alpha | 1 000 000 | 524 288 | yes |
| `cline-free/mimo-v2.6-flash` | MiMo-V2.6-Flash | 1 048 576 | 131 072 | yes |
| `cline-free/deepseek-v4.1-flash` | DeepSeek V4.1 Flash | 1 048 576 | 131 072 | yes |
| `cline-free/gemini-3.8-flash` | Gemini 3.8 Flash | 1 048 576 | **65 536** | yes |
| `cline-free/muse-spark-1.3-contributor` | Muse Spark 1.3 Contributor | 1 048 576 | 943 718 | yes |

**Freeness is never decided from the model name.** The rule is the union of four
independent signals:

1. the id appears in the remote `free` array, or
2. the id ends with `:free`, or
3. the id starts with `cline-free/`, or
4. the entry carries its own `isFree` flag.

The distinction is not academic: `cline-free/deepseek-v4.1-flash` (free) and
`deepseek/deepseek-v4.1-flash` (metered) are two *different* entries with the same
model name, and a name-based rule would mark the metered one free.

**The `cline-free/*` ids are product-surface-only and cannot be served to a plain
API caller.** They are listed by the vendor's own `GET /api/v1/models`, so the
catalogue advertises them, but a request for one is refused:

| request | answer |
| --- | --- |
| `POST /api/v1/chat/completions`, no `X-CLIENT-TYPE` | `403` `Error 403: cline-free/gemini-3.8-flash is only available via Cline product surfaces. If you are using an old version of Cline, please update to the latest version` |
| the same request with `X-CLIENT-TYPE: cline-sdk` (the header this module sends, as the official client does) | `404` `{"error":"model not found","success":false}` |

The header is what the official client sends, so the module keeps sending it; the
consequence is the less obvious of the two messages. Metered ids such as
`anthropic/claude-haiku-4.5` work either way and are the ones to use. A `404`/`400`
is classified `kindClient`, which names the **request** as the problem, so the
account is not parked — see
[Account pool, cooldowns and errors](#account-pool-cooldowns-and-errors).

### Output limits

The module implements `core.ModelLimitsProvider`. `ModelMaxOutputTokens` answers
from the **cached** catalogue only — it runs inside a chat request and must never
make a metadata round trip — and declines (`false`) on a cold cache rather than
guess. The number is also published as `core.Model.Extra["max_output_tokens"]`.

This matters concretely. The limit is not uniform across the free models: sending
`max_tokens=131072` to `cline-free/gemini-3.8-flash` is rejected upstream with

```
has a maxOutputTokens value of 131072 but the supported range is from 1 (inclusive) to 65537 (exclusive)
```

so the catalogue carries 65 536 for that model and 131 072 for its siblings.

`Models` never blocks. A fresh cache is returned as-is; a stale one is returned
while a single-flighted refresh runs in the background (throttled to one attempt
per 30 s, because the registry calls `Models` on every module to resolve a bare
model name); an empty one returns the built-in table immediately.

---

## Configuration

The module reads `clients.cline` from the host configuration. **Every key is
optional**: an absent object is treated as `{}`, and a malformed object is logged
and replaced with defaults — the factory logs a line containing `invalid config`
and `Status` reports not-ready rather than refusing to build.

```jsonc
{
  "clients": {
    "cline": {
      "api_base": "",              // default https://api.cline.bot
      "app_base": "",              // default https://app.cline.bot
      "workos_base": "",           // default https://api.workos.com
      "workos_client_id": "",      // default the Cline desktop client id

      "max_attempts": 3,           // accounts tried per request
      "max_concurrency": 4,        // in-flight chat requests before ErrBusy

      "models_ttl": "5m",          // catalogue freshness
      "models_timeout": "20s",     // catalogue + token-endpoint timeout
      "chat_timeout": "10m",       // whole-request deadline
      "idle_timeout": "2m",        // reserved for the stream idle guard

      "cooldown": "60s",           // park after an auth refusal
      "short_cooldown": "15s",     // park after 5xx / network / 429
      "quota_cooldown": "24h",     // park after 402 / credit exhaustion
      "refresh_margin": "30m",     // pre-emptively refresh this close to expiry

      "max_tokens": 32000,         // used when the request names no limit
      "reasoning_effort": "high",  // see "Reasoning effort"

      "login": false,              // start the device flow at construction
      "poll_interval": "5s",
      "login_timeout": "10m",

      // single-account shorthand
      "access_token": "",
      "account_id": "",
      "nickname": "",
      "email": "",
      "refresh_token": "",
      "expires_at": "",

      "accounts": [
        {
          "label": "personal",
          "account_id": "",
          "nickname": "",
          "email": "",
          "access_token": "",
          "refresh_token": "",
          "expires_at": "",
          "disabled": false
        }
      ]
    }
  }
}
```

### Top-level keys

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `api_base` | string | `https://api.cline.bot` | inference and account endpoints |
| `app_base` | string | `https://app.cline.bot` | the browser URL the device flow prints |
| `workos_base` | string | `https://api.workos.com` | device authorisation endpoints |
| `workos_client_id` | string | the Cline desktop client id | identifies the app to WorkOS |
| `max_attempts` | int | `3` | accounts tried before the request fails |
| `max_concurrency` | int | `4` | in-flight requests; the next one gets `core.ErrBusy` |
| `models_ttl` | duration | `5m` | how long a catalogue answer is fresh |
| `models_timeout` | duration | `20s` | catalogue fetch and token exchange |
| `chat_timeout` | duration | `10m` | the whole chat request |
| `idle_timeout` | duration | `2m` | stream idle guard |
| `cooldown` | duration | `60s` | park after `kindAuth` (a client error parks nothing) |
| `short_cooldown` | duration | `15s` | park after `kindTransient` / `kindNetwork` |
| `quota_cooldown` | duration | `24h` | park after `kindQuota` |
| `refresh_margin` | duration | `30m` | refresh a token this close to expiry |
| `max_tokens` | int | `32000` | `max_tokens` when the request omits one |
| `reasoning_effort` | string | `high` | tier used when the request names none |
| `login` | bool | `false` | start the device flow at construction |
| `poll_interval` | duration | `5s` | device-flow poll cadence (server may shorten it) |
| `login_timeout` | duration | `10m` | device-flow deadline |
| `access_token` | string | — | the `workos:eyJ…` credential |
| `account_id` | string | — | `usr-…`, the id the balance probe needs |
| `nickname`, `email` | string | — | display only |
| `refresh_token` | string | — | enables rotation |
| `expires_at` | string | — | RFC 3339 or a unix timestamp |
| `accounts` | array | — | the multi-account roster |

**Durations** accept a Go duration string (`"90s"`, `"10m"`) **or** a bare JSON
number, which is read as seconds (`120` means two minutes). A value that is
neither is ignored and the default applies — a typo degrades rather than fails.

### `accounts[]` entries

| Key | Type | Meaning |
| --- | --- | --- |
| `label` | string | shown on the panel; never a secret |
| `account_id` | string | `usr-…`; recovered from the JWT when omitted |
| `nickname`, `email` | string | display only |
| `access_token` | string | required; a bare JWT is prefixed for you |
| `refresh_token` | string | optional |
| `expires_at` | string | RFC 3339 or unix |
| `disabled` | bool | excluded from selection until re-enabled |

Precedence: `accounts[]` first, then the single-account shorthand, then the
environment. An entry with an empty access token is dropped rather than stored.

### Environment

| Variable | Meaning |
| --- | --- |
| `CLIENT2API_CLINE_TOKEN` | the `workos:eyJ…` credential |
| `CLIENT2API_CLINE_REFRESH_TOKEN` | refresh token |
| `CLIENT2API_CLINE_ACCOUNT_ID` | `usr-…` |
| `CLIENT2API_CLINE_EMAIL` | display only |
| `CLIENT2API_CLINE_NICKNAME` | display only |
| `CLIENT2API_CLINE_ID` | pool key, when the same credential should keep a stable label |
| `CLIENT2API_CLINE_LOGIN` | truthy (`1`, `true`, `yes`, `on`, `y`) starts the device flow |

Environment values only fill fields the config leaves empty.

---

## Credential discovery

`Discover` looks in exactly one place, the vendor's own store:

```
~/.cline/data/settings/providers.json
```

The file is parsed as arbitrary JSON and the access token is found by walking it
(depth-capped, preferring `accessToken` / `access_token` / `clineAccessToken` /
`token`). A candidate must look like a credential — either the `workos:` prefix or
an `eyJ…` JWT with two dots — so an unrelated `"token": "abc"` elsewhere in the
settings tree is not mistaken for one.

When Cline is not installed the file simply does not exist. That is **not** an
error: `Discover` reports the path with a note and nothing importable, and the
module degrades to `core.ErrNotConfigured` on `Chat` with a `Status` that names
the ways to supply a credential.

### The device-authorisation flow

Cline has no password grant. A credential is minted by the WorkOS device flow:

1. `POST {workos_base}/user_management/authorize/device` returns a device code, a
   human code and a verification URL.
2. The operator opens the URL and approves.
3. The module polls `POST {workos_base}/user_management/authenticate` until the
   grant stops being `authorization_pending`.
4. The WorkOS tokens are exchanged at `POST {api_base}/api/v1/auth/register` for
   Cline's own token, which is what gets stored.

There are two ways to reach it:

- **`"login": true`** (or `CLIENT2API_CLINE_LOGIN=1`) — `New` starts the flow in
  the background when nothing is usable yet and prints the URL to stdout.
- **`RunDeviceFlow(ctx, deps, w)`** — the exported entry point, for a caller that
  wants to drive it itself. The in-progress flow is persisted to `login.json`, so
  a restart does not lose a half-completed authorisation, and `Status.Detail`
  reports the URL while it is pending.

---

## Files in `Deps.DataDir`

| File | Contents |
| --- | --- |
| `accounts.json` | the credential roster: tokens, ids, labels, expiry |
| `state.json` | the health mirror: state, cooldown, last error, last use |
| `login.json` | a device flow in progress, removed when it completes |

Writes go through `core.WriteJSONAtomic`. The health mirror is debounced (5 s), so
a burst of requests does not turn into a burst of disk writes. A credential is
never written to `state.json`, and no token is ever logged in full — everything
logged passes through `core.Redact`, and an error string is scrubbed of the
account's own secrets before it is recorded.

---

## The three traps

These are measured behaviours of the vendor, not defensive guesses. Each one has
a test in `cline_test.go` asserting it directly.

### 1. The `workos:` prefix must stay in the Authorization header

The stored value is `workos:eyJ…`. It is sent **verbatim**:

```
Authorization: Bearer workos:eyJ…
```

Measured: with the prefix, `GET /api/v1/users/me` answers 200; with the prefix
stripped, it answers **401** whose body misleadingly says *"make sure you're using
the latest version of Cline"* — pointing at the client version rather than at the
header. The prefix is stripped in exactly one place, `bareToken`, which exists
only to decode the JWT payload locally; it is never used to build a header.

### 2. The refresh body is camelCase, not OAuth-standard

```json
{"refreshToken": "<token>", "grantType": "refresh_token"}
```

Both fields are required. The OAuth-standard `refresh_token` / `grant_type`
spellings produce a *generic* authentication failure rather than a "missing
field" error, which is why the distinction is easy to miss and expensive to
debug.

### 3. `account_id` is the vendor id, not the JWT subject

The balance route is `GET /api/v1/users/{account_id}/balance`, so it needs
`account_id`, which looks like

```
usr-01M3BCV4FYCGJKAWD3MJG3DBQM
```

The JWT's `sub` claim looks like

```
user_01M3BCQ86DV4S9KKBT85X4GKTV
```

and passing it returns `400 {"error":"Invalid request format"}`. The two are
easy to conflate because both are opaque `usr-`/`user_` strings. `account_id` is
taken from `userInfo.clineUserId` in the token response, never from the JWT, and a
stored id that disagrees with the credential is reported rather than used.

The response envelope itself has two further sharp edges:

- **`{"success": true, "data": {…}}`** — the success test is
  `success && data.accessToken`. Testing a bare `accessToken` would accept a
  failure envelope, because a `{success:false}` answer can still carry a `data`
  object.
- **A refresh may omit `userInfo` entirely.** `account_id`, `email` and `nickname`
  are therefore *preserved* from the previous record rather than blanked. An
  `expiresAt` may also be absent; an undefined expiry is treated conservatively as
  **not expired**, leaving the decision to the server's 401. `expiresAt` is
  ISO 8601, and a numeric value below 1e12 is read as seconds, otherwise
  milliseconds.

---

## Reasoning effort

| Tier | Wire value |
| --- | --- |
| `none` | `None` |
| `low` | `Low` |
| `medium` | `Medium` |
| `high` | `High` |
| `max` | `Extra` |

The tier and the wire value differ deliberately (`max` → `Extra`). The remote
catalogue never publishes per-model tiers, so **one table serves every model** and
there is no per-model switch to consult.

**Omitting `reasoning_effort` means the model does not think at all.** The field
is therefore always sent; the resolution order is the per-request option
(`reasoning_effort`, `reasoningEffort`, `reasoning`, or `effort` in
`ChatRequest.Options`), then the configured `reasoning_effort`, then `high`.

An unknown tier is passed through **verbatim**. The upstream silently ignores it
(`reasoning_effort: "banana"` is HTTP 200 with zero thinking), so the worst case
is a no-op switch — and rewriting the value would hide the difference between
"we sent `High`" and "we sent `banana`" from the operator who typed it.

---

## Account pool, cooldowns and errors

Accounts are tried least-recently-used first, skipping any the request has
already used. The account bound to a conversation is preferred while it is still
available, so a multi-turn exchange stays on one credential. Every entry's state
is recomputed from its cooldown on read, so an elapsed park reports `ready`
without a write.

| Upstream condition | Kind | Park | State | Retry another account |
| --- | --- | --- | --- | --- |
| network failure, no response | `kindNetwork` | `short_cooldown` | `cooling` | yes |
| HTTP 402 | `kindQuota` | `quota_cooldown` | `exhausted` | yes |
| HTTP 429, body mentions credits | `kindQuota` | `quota_cooldown` | `exhausted` | yes |
| HTTP 429, otherwise | `kindTransient` | `short_cooldown` | `cooling` | yes |
| HTTP 5xx | `kindTransient` | `short_cooldown` | `cooling` | yes |
| HTTP 401 / 403 | `kindAuth` | `cooldown` | `cooling` | yes |
| other HTTP 4xx | `kindClient` | none | `ready` | no |
| `Retry-After` present | — | lengthens the park | — | — |

A `kindAuth` verdict on an account that still has a refresh token triggers a
rotation and a retry *without* consuming an attempt; if the rotation fails, or
there is no refresh token, the account is parked as dead. A `Retry-After` header
(also `X-Ratelimit-Reset`, `Retry-After-Ms`, capped at 2 h) can only ever lengthen
a park, never shorten it.

**A `kindClient` failure does not park the account at all.** It is the *request*
that was wrong — a model id the vendor does not serve, a body it rejected, a
context deadline, a local decode error — so the credential stays selectable and
`Status` keeps reporting `ready`. Two things must hold at once: the loop does not
retry the request on another account (it would fail identically everywhere), and
the account that sent it is not punished. An earlier revision parked the account
for the full `cooldown`; a live run showed the cost — one chat naming a stale
model id answered `503 no_healthy_account` for every *other* model on that
credential until the park lapsed. `TestAClientErrorNeverParksAHealthyAccount`
pins the fix and `TestAnAuthErrorStillParksTheAccount` pins that the arm which
*is* the credential's fault still parks.

Failures are surfaced through `core.Fail("cline", accountID, kind, status, err)`
so the host's failure contract sees them.

Errors the host sees:

| Condition | Error |
| --- | --- |
| no credential at all | `core.ErrNotConfigured` (host maps to 503) |
| nil request, or a blank model | `core.ErrUnsupported` (400) |
| at the concurrency ceiling | `core.ErrBusy` |
| anything else | `core.Fail(...)` (502) |

**A vendor refusal is a result, not a Go error.** `TestAccount` returns a
populated `TestResult.Error` with a nil `error`, because "the upstream said no" is
information about the credential, not a failure of the probe.

---

## Streaming

- `data: [DONE]` ends the stream; a final frame without a trailing blank line is
  still delivered.
- `usage` and the terminal event are emitted at most once each.
- `Recv` returns `io.EOF` exactly once; `Close` is idempotent and releases the
  in-flight slot.
- Finish reasons are normalised to `stop` / `length` / `tool_calls` /
  `content_filter`.
- An error member mid-stream becomes an `EventError`; an unparseable frame is
  skipped rather than fatal.
- A stream that carried no data at all is an error, not a silent success.
- Reasoning arrives as `reasoning_content` (or `reasoning`) and is forwarded as
  `Event.Reasoning`.

---

## How `Status` is rendered

`Status` makes no network call — it reads the cached catalogue, a mutex-guarded
snapshot and the last recorded upstream verdict. The panel polls it, so it must
never block.

| `Status` field | Source |
| --- | --- |
| `Name` | `"cline"` |
| `Ready` | whether any account is available |
| `Models` | the cached ids, else the built-in table |
| `Accounts` | one `core.AccountStatus` per entry |
| `UpdatedAt` | now |

`Detail` is one of:

- `device flow not completed: open <URL>` — a login is pending;
- `no credential: set clients.cline.access_token, CLIENT2API_CLINE_TOKEN, or run the device flow ("login": true)`;
- the pool summary plus the model ids — at least one account is ready;
- `all accounts unavailable (<reason>)` — every account is parked;
- and any of the above gains ` (last attempt <age> ago)` once an upstream verdict
  has been recorded, where the age renders as `0s`, `45s`, `12m` or `3h`.

---

## Known gaps

- **Live-validated 2026-10-01** against a real signed-in Cline desktop account
  (`acct:usr-01M3VRAYHPSNWHF6VRHE9XH900`), after importing
  `~/.cline/data/settings/providers.json` through the panel. That session
  exercised the whole credential path: the stored access token was already
  expired, and the module renewed it unaided (`expires_at` moved forward,
  `state ready`); the live catalogue then refreshed from 5 built-in ids to **468**
  live ids; and a non-streaming `POST /v1/chat/completions` against
  `cline/cline-free/deepseek-v4-flash` returned a real completion. Three real
  defects were found and fixed by this run: an expired-account refresh deadlock,
  a stale `stateReady` in the panel projection, and `Import` dropping the refresh
  token.
- **The balance route is now measured, not guessed.** `AccountBalance` calls
  `GET /api/v1/users/{account_id}/balance`, which answers
  `{"data":{"userId":"usr-…","balance":496429},"success":true}`. Earlier versions
  reused `GET /api/v1/users/me` and scanned the profile for a credit figure; that
  endpoint carries none at all — it returns only `id`, `email`, `displayName`,
  `termsAcceptedAt`, `clineBenchConsent`, `organizations`, `createdAt`,
  `updatedAt` — so the probe failed for every account and the panel showed a
  permanent `502 "the vendor's account payload carries no credit figure"`. The
  near-miss `/api/v1/users/me/balance` is a *different* route: it answers
  `400 {"error":"Invalid request format"}` with only a per-request id under
  `data.ID`, for every spelling of the id as a query parameter, and `405` for
  `POST`. `Total` is left at zero because this endpoint publishes no ceiling.
  When the stored `account_id` is empty the probe reads one out of the credential
  via `GET /api/v1/users/me`; when neither names an id it says so instead of
  building a path it cannot justify.
- **No image-generation or embedding surface.** Only chat completions are
  implemented.
- **`idle_timeout` is configured but not yet enforced** on a stalled stream; the
  request-level `chat_timeout` is the current guard.
- **`app_base` is only used for the device-flow URL.** It is not consulted for
  anything else.
- **The register request shape is inferred** from the response contract: it sends
  `accessToken`, `token`, `refreshToken` and `clientId` so that whichever spelling
  the upstream expects is present. The measured evidence covers the *response*,
  not the request field names.
- **`max_attempts` counts accounts, not HTTP attempts.** A token rotation retry
  does not consume one.
- **The `-race` result is now measured**, not merely reasoned: the module is part
  of the repository-wide `go test -race ./...` baseline, which passes with zero
  `WARNING: DATA RACE` (the production build still uses `CGO_ENABLED=0`; the race
  run needs `CGO_ENABLED=1` + `CC=clang`).
- **Per-model reasoning tiers are not modelled**, because no endpoint publishes
  them.

---

## Testing

```sh
go vet ./clients/cline/...
go test ./clients/cline/... -v
go test ./... -count=1
```

Everything runs offline and needs no credential. The suite stands up an
`httptest` server that records **every** request (method, path, headers, raw
body), so the traps are asserted on what was actually sent rather than on an
outcome: the `Authorization` header really carries `workos:`, the refresh body
really uses `refreshToken`/`grantType` and really omits the snake_case spellings,
and the catalogue merge is checked against a `/api/v1/models` fixture that
deliberately contains no `cline-free/*` id. The no-fetch rule is asserted by
counting requests: `ModelMaxOutputTokens` on a cold cache must decline *and* leave
the request counters at zero.

Discovery tests point `HOME`/`USERPROFILE` at a temp directory, so they exercise
both the "Cline is not installed" path and a fixture in the vendor's own settings
shape. (On the development machine Cline *is* now installed, and the real
`~/.cline/data/settings/providers.json` was imported live — see **Known gaps**.)

---

## Provenance and licence

Clean-room: the protocol was reconstructed by observing the official client's
traffic and is documented from measurement, not from decompiled source. Where a
detail was inferred rather than observed it is marked as a guess in the code and
in **Known gaps** above.

### Security note

`accounts.json` holds live bearer tokens in plaintext, like every other credential
store in this project. It is written with `0600`-equivalent permissions through
`core.WriteJSONAtomic` and lives under `Deps.DataDir`. Nothing is logged
unredacted: `core.Redact` covers log lines, and error strings are scrubbed of the
account's own secrets before they are recorded.

### Account-ban risk

Automating a consumer account can violate the vendor's terms. The device flow
uses the same client id as the desktop app so that requests look like the app's,
which is a compatibility measure, not a licence to exceed a fair-use limit. Run
this against accounts you are willing to lose.
