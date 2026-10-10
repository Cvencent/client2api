# codearts

A `client2api` client module for **华为 CodeArts** (Huawei CodeArts IDE's model
service). It signs requests with Huawei's SDK-HMAC-SHA256 scheme, refreshes its
credential through the Huawei STS token endpoint, reads the vendor's model
catalogue, and streams chat completions.

Registered name: `codearts`. Qualify a model as `codearts/<model-id>`.

This module is a faithful port of the reference implementation
`iJetLi/deepseek-harness-codearts` (TypeScript), restricted to the wire protocol.
See [Deviations from the reference](#deviations-from-the-reference) for the parts
that were deliberately left out and why.

---

## Status: verified against fixtures only

**No live CodeArts account was available on the machine this module was written
on.** Everything below is derived from the reference implementation's source and
from the vendor's own error strings, and is covered by fixture tests
(`httptest` servers and `http.RoundTripper` stubs). Nothing was confirmed against
the real service.

Where this matters it is called out explicitly in the text below with
**[unverified]**. Treat those lines as "the reference implementation does this,
so this module does too" rather than "this was observed working".

---

## Credentials

### Where they come from

Three sources, in this precedence order (`configuredAccounts` in `config.go`):

1. `accounts: [ … ]` — a list of credential objects.
2. The single-account shorthand at the top level of the config
   (`access_key_id` + `secret_access_key` + …).
3. The environment, but **only when both halves are present**:
   `CLIENT2API_CODEARTS_AK` and `CLIENT2API_CODEARTS_SK` (plus the optional
   `CLIENT2API_CODEARTS_SECURITY_TOKEN`, `CLIENT2API_CODEARTS_REFRESH_TOKEN`,
   `CLIENT2API_CODEARTS_EXPIRES_AT`).

An entry that is missing either the access key id or the secret access key is
dropped rather than half-loaded, so a typo cannot produce an account that fails
every request.

Credentials obtained by signing in are stored on disk, under `Deps.DataDir`:

| File | Contents |
| --- | --- |
| `accounts.json` | the credential list (mode 0600) |
| `state.json` | per-account pool state: cooldown, failure count, note |
| `login.json` | the pending sign-in session (carries **no** secret) |
| `models.json` | the cached model catalogue |
| `benefit_models.json` | the cached free-quota model list |

The last two move to `cacheDir()` when it is set — i.e. to
`$DSH_CODEARTS_CACHE_DIR` — so a shared data directory does not mix catalogues
between machines. `accounts.json`, `state.json` and `login.json` always stay in
`DataDir`.

With `Deps.DataDir == ""` the module is **read-only**: it serves whatever the
config supplies, cannot sign in, and `AddAccount` fails with an explicit error
rather than pretending to save.

### Account identity is the access key id

Every other client module in this repository identifies an account by its
`access_token`. CodeArts is the exception, in this module and in the reference
implementation alike: `account.identity()` returns the **access key id**.

Two records carrying the same access key id are the same account reached
through two channels, and the panel groups them. This is what makes
`Accounts()` and `Status()` agree with each other instead of showing the same
credential twice.

### Signing in

`StartLogin` returns immediately with a URL the operator must open, then drives
the flow on a background goroutine. `PollLogin` reports progress; `CancelLogin`
aborts.

The primary flow is the portal OAuth flow with PKCE plus DPoP:

- Authorize URL: `https://codearts.huaweicloud.com/portal/authorize`
  with `theme=2`, `locale=zh-cn`, `uri_scheme=codearts-agent`,
  `client_id=codearts-agent`, `port=<callback port>`,
  `code_challenge`, `code_challenge_method=SHA-256`,
  `plugin-name=snap_AIIDE`, `plugin-version=5.2.0`.
- The callback server listens on `127.0.0.1` on a port **≥ 10000**; it re-binds
  until it gets one. **[unverified]** The reference implementation documents that
  the portal rejects low ports, and that the failure only appears *after* the
  operator has finished signing in — which is why the port is checked up front
  instead of being discovered at callback time.
- `code_challenge_method` is the literal string `SHA-256`, **not** the RFC 7636
  spelling `S256`. **[unverified]** With `S256` the portal falls back to the
  legacy ticket flow.
- The authorize URL must **not** carry `auth_callback_url`; the reference
  implementation notes that the portal treats it as abnormal and falls back.
- On success the browser is redirected to
  `https://codearts.huaweicloud.com/portal/login?login_succeed=true&…` so the
  IDE shows a "signed in" page.

The callback handler also accepts a `secret` parameter, which is the legacy
ticket flow: it polls
`https://snap-access.cn-north-4.myhuaweicloud.com/snap-manager/v1/login/ticket?ticket_id=…&secret=…`
with the headers `plugin-name: snap_jetbrains`, `plugin-version: 26.3.3`, once a
second for up to 120 attempts. That request is deliberately **not signed** — it
is the endpoint that *produces* the access key and secret.

`login: true` in the config (or `CLIENT2API_CODEARTS_LOGIN`) starts the flow
without waiting for an operator to press a button.

### Refreshing

`exchangeRefreshToken` POSTs `application/x-www-form-urlencoded` to
`https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens` with
`grant_type=refresh_token`, `client_id=codearts-agent`, the refresh token, and
the stored PKCE verifier, and carries a **DPoP proof** (see below) in a `DPoP`
header.

A refresh is only possible when the stored credential has **all** of: an access
key id, a secret access key, a refresh token, and the DPoP private key. A
credential supplied through the config has the first two but not the last two —
**the config format has no `dpop_private_key_jwk` or `code_verifier` key** — so
`RefreshAccount` on a config-only credential honestly reports
`this credential has no refresh token, so it cannot be renewed; sign in again`
rather than failing obscurely later.

Only two answers are treated as terminal — the refresh token is dead and the
operator must sign in again:

- `error == "invalid_grant"`
- `error_code` containing `ExpiredRefreshToken`

`InvalidDPoPHeader` is deliberately **not** terminal: it is a per-request
rejection, and treating it as terminal would throw away a perfectly good refresh
token over a transient proof problem. The reference implementation removed it
from the terminal set for the same reason.

---

## Endpoints

All paths below are relative to `base_url`, which defaults to
`https://snap-access.cn-north-4.myhuaweicloud.com`.

| Purpose | Method and path |
| --- | --- |
| Chat completions | `POST /api/v2/chat/completions` |
| Model catalogue | `GET /v1/model/builtin` |
| Queue status | `GET /api/v1/queue/status` |
| Legacy login ticket | `GET /snap-manager/v1/login/ticket` |
| Plugin statistics | `GET /snap-manager/v1/statistics/plugin` |
| Activity list | `GET /v1/ops/delivery?channel=IDE` |
| Claim an activity | `POST /v1/ops/claim` |
| Confirm a claim | `POST /v1/ops/confirm` |

Two absolute endpoints are not under `base_url`:

| Purpose | URL |
| --- | --- |
| STS token exchange | `https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens` |
| Free-quota model list | `https://opengw.developer.huaweicloud.com/api/v1/gateway/config` |

`chat_url`, `models_url`, `sts_url`, `gateway_url`, `ticket_url` and `base_url`
all override these. `chat_url` is the **full** URL, not a base.

---

## Signing: SDK-HMAC-SHA256

`sign.go` implements Huawei's scheme by hand. The canonical request is:

```
METHOD
<path with a forced trailing slash>
<query string, without the leading '?'>
<lowercased k:v lines, sorted by header name, joined by "\n">
<empty line>
<semicolon-joined signed header names>
<sha256 hex of the body>
```

then

```
stringToSign = "SDK-HMAC-SHA256\n" + xSdkDate + "\n" + sha256Hex(canonicalRequest)
signature    = HMAC-SHA256(secretAccessKey, stringToSign)      // hex
```

and the credential rides in

```
Authorization: SDK-HMAC-SHA256 Access=<ak>,SignedHeaders=<h;h;…>,Signature=<hex>
```

Base headers, always present and always signed: `host`, `x-sdk-date`,
`x-sdk-content-sha256`, and `x-security-token` when the credential has one.
A non-GET request additionally signs `content-type: application/json`; a GET
does **not** (the vendor rejects a GET that claims a content type).

### The header-ordering trap

This is the single most fragile part of the protocol, and it is asserted in
`sign_test.go`, `upstream_test.go` and `chat_test.go`:

- **`maas_type: benefit` must be signed.** It is passed into the signer as an
  extra header so it participates in both the canonical request and
  `SignedHeaders`. **[unverified]** Omitting it for a free-quota model makes the
  vendor answer `InferHub.002002009.404 "model is not registered"`.
- **`Agent-Type` and `X-Language` must NOT be signed.** They are appended to the
  request *after* signing. **[unverified]** Signing them makes the vendor answer
  `401 {"error_code":"APIG.0301","error_msg":"…verify ak sk signature fail"}`.
- **`host` is never copied out of the signer's map.** The HTTP runtime sets it;
  copying it produces a second, disagreeing `Host` header.
- **`content-type` is signed but not copied** from the signed map — the request
  builder sets it explicitly.

`sendChat` therefore signs `maas_type` only when the model is a free-quota
model, and appends `Chat-Id`, `Session-Id` and `lang: en` unsigned. The
queue-status probe appends the unsigned `x-snap-traceid`, `Agent-Type:
INFERHUB_AGENT` and `X-Language: en`. The catalogue request appends the unsigned
`Agent-Type: PromptCenter` and `X-Language: zh-cn`.

### DPoP

`crypto.go` builds an ES256 JWS by hand — no JWT library. The protected header
is `{"alg":"ES256","typ":"dpop+jwt","jwk":{…}}` with the **public** P-256 key as
a JWK, and the payload is `{"htm":…,"htu":…,"iat":…,"jti":…}`. The signature is
the raw 64-byte `R || S` concatenation, not the ASN.1 form Go's `ecdsa.Sign`
returns.

The private key is persisted as a JWK in `accounts.json` and rebuilt with
`keyPairFromStoredJwk`. Its RFC 7638 thumbprint is available as `Jkt` for any
future `DPoP` confirmation.

---

## Chat request

The body is:

```json
{
  "model": "…",
  "messages": [ … ],
  "stream": true,
  "prompt_cache_key": "<session id>",
  "include": ["reasoning.encrypted_content"],
  "reasoning_summary": "auto",
  "tool_stream": true,
  "max_tokens": 65536
}
```

with `"thinking": {"type": "disabled"}` added when `reasoning_effort` is `off`,
and `tools` added when the caller supplied any.

Two details are load-bearing:

- **`max_tokens` is 65536 by default, not 131072.** **[unverified]** The
  reference implementation records that 131072 makes the vendor return an empty
  stream. A caller-supplied `MaxTokens` is passed through unchanged.
- **Assistant history messages always carry a `reasoning_content` field**, as an
  empty string when the model produced no reasoning. **[unverified]** The
  `deepseek-v4` family answers `400 Missing \`reasoning_content\` field`
  otherwise. Only assistant messages get the key; a user or tool message does
  not. This is implemented with a `*string` so that `omitempty` omits `nil` but
  still emits an explicit `""`.

`ConversationID` is projected upstream as the vendor's own `prompt_cache_key`.
`ServedBy` is never sent. No key is invented on this side and sent to the
vendor: everything in the body is a field the vendor documents.

### Queueing

**[unverified]** Under peak load the vendor answers `400` with `TM.00001041` or
wording about peak usage, and the request is queued rather than refused. The
module then polls `/api/v1/queue/status?model=…&task_id=…` every
`queue_poll_interval` for up to `queue_max_wait`, and resends once when the
queue reports `working`. A `queue_full` or `error` status ends the attempt.

### Retrying

`openStream` tries up to `max_attempts` accounts. For each attempt it picks an
available account, records it as tried, and calls `NoteServedBy` **before**
sending, so a failure part-way through still reports which account was used.

- An auth rejection (`401`/`403`, `APIG.0602`, or invalid-token wording)
  triggers **one** silent refresh and resend, which does not consume an attempt.
- A queue rejection triggers the queue wait and one resend.
- A client error (`400` that is not a queue error) is terminal and does **not**
  cool the account — the request was malformed, the credential is fine.
- Because an account is marked tried before its attempt, **rotation needs at
  least two accounts**: a single-account pool cannot retry the same account, and
  a rate-limit refusal ends the request. That is the honest behaviour — retrying
  the same rate-limited credential in a loop is worse than failing.

`ModelOutputLimit`-style budgets come from `ModelMaxOutputTokens`, which answers
from the cached catalogue only and never guesses: an unknown model reports
`ok=false` and the gateway sends no cap at all.

---

## Model catalogue

The catalogue is assembled from two sources:

- `GET /v1/model/builtin` — the regular models, signed, with the unsigned
  `Agent-Type: PromptCenter` and `X-Language: zh-cn` headers.
- `GET https://opengw.developer.huaweicloud.com/api/v1/gateway/config` — the
  **free-quota** ("benefit") models. This one is **not signed** and carries no
  `Authorization` header at all.

Ids are normalised by stripping a trailing four-digit build stamp, so
`deepseek-v4-flash-0731` and `deepseek-v4-flash` are the same model. The rule is
deliberately narrow — exactly four digits, and only on an id longer than five
characters — so an id that legitimately ends in four digits is not mangled.

Models whose id contains `-VL-` or ends in `-VL` are skipped: the module does
not send images to CodeArts, and listing a vision-only model would offer a model
that cannot answer.

The free-quota list records only ids that were **not** rewritten by
normalisation. This is subtle and easy to get wrong: `deepseek-v4-flash` and
`deepseek-v4-flash-0731` are different backend models with opposite
free-quota-ness, so mapping the dated id onto the undated one would mark a paid
model as free.

When neither the live list nor the disk cache is readable, the module falls back
to `glm-5.3-flash` and `deepseek-v4.1-flash` — the same two ids the reference
implementation carries — so an operator whose gateway is briefly unreachable
still gets the free tier rather than silently paying.

`Models` answers from the cache and refreshes in the background, so a bare-name
model resolve never blocks on the network.

---

## Daily check-in and balance

`CheckinActions` offers exactly one action, `daily-login`, and only while an
account exists. `Checkin` requires the account's package to report
`is_credit_package == true`; otherwise it returns `OK=false` with
`{"status":"inactive"}`.

Activity entries use `campaignId` (a number) and `benefitAmount` — not the
`campaign_id`/`amount` spellings one would guess. A claim is followed by a
confirm step only when the claim response carries a non-zero `id`.

`AccountBalance` reads the credit total from the `usageTotalPackageCredit`
metric and falls back to summing the basic, on-demand and bonus categories
**only** when the total metric is absent. An absent metric is not a zero metric:
reporting `0` credits for an account that simply did not report one would be a
lie.

Every vendor refusal here is a **result**, not an error: `CheckinResult.OK` is
false and `Error` says why. A transport failure, by contrast, *is* an error.

---

## Configuration

Every field is optional. A config that fails to decode is logged
(`codearts: invalid config, using defaults: …`) and the module carries on with
defaults — a typo can never take the module down or wedge it.

Durations accept either a Go duration string (`"30s"`, `"2h"`) or a bare JSON
number, which is read as **seconds**.

| Key | Default | Meaning |
| --- | --- | --- |
| `base_url` | `https://snap-access.cn-north-4.myhuaweicloud.com` | API host |
| `chat_url` | `base_url` + `/api/v2/chat/completions` | full chat URL |
| `models_url` | `base_url` + `/v1/model/builtin` | catalogue URL |
| `sts_url` | the Huawei STS endpoint | token exchange URL |
| `gateway_url` | the vendor gateway config URL | free-quota model list |
| `ticket_url` | `base_url` + `/snap-manager/v1/login/ticket` | legacy login |
| `user_agent` | `client2api/codearts` | `User-Agent` |
| `accounts` | `[]` | credential list |
| `access_key_id` … `user_name` | — | single-account shorthand |
| `max_attempts` | `3` | accounts tried per request (non-positive falls back to the default) |
| `models_ttl` | `2h` | how long a catalogue is trusted |
| `models_timeout` | `20s` | catalogue request timeout |
| `chat_timeout` | `30m` | whole chat request, including the stream |
| `idle_timeout` | unset | coarse watchdog for **both** stream timers; the two keys below win over it |
| `first_token_timeout` | `300s` | how long the stream may stay silent before its first byte |
| `chunk_timeout` | `600s` | how long it may stay silent between two bytes |
| `cooldown` | `60s` | how long an account rests after a transient failure |
| `short_cooldown` | `15s` | after a queue rejection |
| `quota_cooldown` | `24h` | after a quota rejection |
| `refresh_margin` | `30m` | refresh a credential this long before it expires |
| `login_timeout` | `10m` | how long a sign-in session stays open |
| `queue_poll_interval` | `10s` | queue-status poll interval |
| `queue_max_wait` | `30m` | give up on the queue after this |
| `max_tokens` | `65536` | default output budget |
| `reasoning_effort` | unset | `off`/`none`/`disabled`/`false` pins thinking off |
| `models` | `[]` | override the built-in fallback catalogue |
| `login` | `false` | start the sign-in flow on start |

Environment overrides: `CLIENT2API_CODEARTS_AK`, `…_SK`,
`…_SECURITY_TOKEN`, `…_REFRESH_TOKEN`, `…_EXPIRES_AT`, `…_BASE_URL`,
`…_LOGIN`, plus `DSH_CODEARTS_CACHE_DIR`,
`DSH_CODEARTS_SSE_FIRST_TOKEN_TIMEOUT_MS` and
`DSH_CODEARTS_SSE_CHUNK_TIMEOUT_MS`. The last three keep the reference
implementation's names so a machine already tuned for that project keeps its
settings. The two `_MS` values are milliseconds.

`idle_timeout` deliberately has **no** default. The two watchdog defaults are
different numbers (300 s and 600 s), so a single generic default would silently
shorten the first-token budget that the vendor's queue needs. It does nothing
until an operator sets it.

---

## What this module does NOT implement, and why

It implements, and the panel discovers: `AccountManager`, `LoginProvider`,
`CheckinProvider`, `ModelRefresher`, `ModelLimitsProvider`, `Reviver`,
`BalanceProvider` and `HintProvider`.
It also implements `HealthProvider` and `PoolStatsReporter`; the pool census
drives the status badge and the in-flight count.

Deliberately absent, with reasons:

| Interface | Why not |
| --- | --- |
| `CredentialImporter` | CodeArts is an IDE plugin; it keeps no credential file on disk to discover. Signing in is the only real path. |
| `BundleImporter` | No bundle format exists for this vendor. |
| `RealmLoginProvider` | There is exactly one realm (`cn-north-4`). A realm picker with one entry is noise. |
| `CaptchaProvider` | The sign-in flow is a browser redirect to Huawei's own portal, which handles its own challenges. |
| `TaskProvider`, `BatchPlanner` | **The vendor has no task API.** The only recurring action is the daily check-in, which `CheckinProvider` covers. Implementing these would mean inventing an API. |
| `Degrader` | No cheaper model tier to degrade to. |
| `LiveReloader` | Credentials are re-read on demand; there is nothing to hot-swap. |
| `PackageProvider`, `VoucherProvider` | The vendor exposes credit *totals* but no per-package or voucher breakdown. `AccountBalance` reports what genuinely exists. |
| `ConversationProvider` | The vendor has no conversation-list API. |

No optional interface is implemented as an empty shell, and no capability is
advertised that the vendor does not have.

---

## Testing

```powershell
$env:Path = "$env:LOCALAPPDATA\Programs\go1.27.1\go\bin;" + $env:Path
$env:GOPROXY='https://goproxy.cn,direct'; $env:GOSUMDB='sum.golang.google.cn'
$env:CGO_ENABLED='0'
go test -count=1 -timeout 20m ./clients/codearts/
```

Every test is fixture-driven — `httptest` servers and `http.RoundTripper` stubs,
never the real network. The suite covers config parsing and defaults, credential
persistence round-trips, the canonical request and the header-ordering trap,
PKCE and DPoP construction (the DPoP proof is re-verified with `ecdsa.Verify`
against the public key in its own header), token refresh, catalogue parsing,
SSE parsing, the queue and auth retry paths, and the `ErrNotConfigured` path.

The tests were checked against deliberate mutations of the production code — see
the module's report — so that each one is known to fail when the behaviour it
names is broken.

`gofmt -l ./clients/codearts` prints nothing and `go vet ./clients/codearts`
exits 0.

---

## Deviations from the reference

The reference implementation is a TypeScript IDE plugin. Four of its components
exist to adapt *that host's* message shape and UI, not to speak the vendor's
protocol, and were deliberately not ported:

- `DsmlContentExtractor` — parses `<｜DSML｜tool_calls>` and `<thought>` markers
  out of `delta.content`. Those markers are produced by the reference's own
  prompt, not by CodeArts. This module forwards `delta.tool_calls` and
  `reasoning_content` as core events, which is what the vendor actually sends.
- The reasoning-loop detector and the blank-reasoning suppressor — UI guards for
  a model that can loop.
- `stripCourseLeak` history self-healing — rewrites the host's own history.
- `resolveToolPairing` / `normalizeHarnessMessages` — normalise the host's
  message array before sending.

The module also does not implement the reference's `codearts-credits` check-in
*scheduling* (it runs on a timer there). Here the check-in is an operator-driven
action exposed through `CheckinProvider`, which is how every other module in
this repository works.

### Things in the reference that looked wrong

- **`InvalidDPoPHeader` in the terminal-refresh set.** It is not in the ported
  code. A DPoP proof problem is per-request; treating it as "your refresh token
  is dead" would force an unnecessary re-login.
- **`refreshAll()` filtering by `enabled`.** The reference's own
  `docs/adding-a-new-provider.md` records this as a real defect it fixed;
  refresh here is driven per-account and never gated on the enabled flag.
- **The `account.create` placeholder dance.** The reference registers a
  placeholder account and deletes it if activation fails, because its browser
  must open immediately. This module returns the URL immediately and registers
  the account only on success, so there is no placeholder to clean up.

---

## Files

| File | Contents |
| --- | --- |
| `codearts.go` | `Client`, `newClient`, `init()` registration, `Status` |
| `config.go` | config parsing, defaults, env overrides, path resolution |
| `accounts.go` | the credential struct, identity, masking, validation |
| `pool.go` | the account pool: selection, cooldown, persistence |
| `sign.go` | SDK-HMAC-SHA256 |
| `crypto.go` | PKCE, DPoP ES256, JWK, thumbprint, UUID |
| `oauth.go` | STS token exchange, portal URLs |
| `login.go` | the callback server and both sign-in flows |
| `models.go` | the catalogue, free-quota detection, caching |
| `upstream.go` | HTTP plumbing, error classification, the chat body |
| `sse.go` | the SSE reader and the stream |
| `chat.go` | `Chat`, retries, queue waiting |
| `panel.go` | the `AccountManager` / `LoginProvider` surface |
| `credits.go` | the daily check-in and the credit balance |
