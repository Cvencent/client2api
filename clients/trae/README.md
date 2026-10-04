# client2api — `trae` module

Exposes a Trae (Trae CN / TRAE SOLO) account as an OpenAI-compatible model
behind the `trae/` route prefix.

- Upstream: `POST https://trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat`
  (SOLO channel, `function: solo_work_lite`).
- Catalogue: `POST https://trae-api-cn.mchost.guru/api/ide/v1/get_detail_param`.
- Credential: the one the Trae desktop app keeps in
  `%APPDATA%\Trae CN\User\globalStorage\storage.json`, decrypted in-process.

Nothing here needs configuration: an empty `{}` under `clients.trae` is the
recommended setup. Everything below exists so that a version bump on the
upstream side can be fixed from the config file instead of a rebuild.

## Files

| File | Responsibility |
| --- | --- |
| `trae.go` | `core.Client` — `Name`, `Models`, `Chat`, `Status`, registration |
| `config.go` | the `clients.trae` schema, defaults, accessors |
| `auth.go` | credential discovery + the `Auth` account value |
| `tcrypt.go` | the `tc` container decryptor (AES-128-CBC, four salt tables) |
| `pool.go` | round-robin account selection and per-account health/cooldowns |
| `poolstate.go` | the secret-free health file: what has to survive a restart |
| `payload.go` | OpenAI request → Trae body rewriting |
| `sse.go` | the Trae SSE frame scanner and `core.Event` mapping |
| `upstream.go` | error taxonomy, headers, HTTP calls, token refresh |

The pool's health is kept in `<data_dir>/pool.json`, beside `accounts.json`: the
vendor's verdict on each account and the deadline that came with it, never a
token. Restarting therefore does not replay a request the vendor just refused —
a 12 h plan-limit park survives it. Only accounts that are **not** healthy are
recorded, so a start with nothing wrong writes no file at all. `invalid` is
deliberately **not** written, because in this module it is a permanent park and
every start re-reads the credential from its source; the reasoning is in the
header of `poolstate.go`.

## Configuration

The object under `"clients"."trae"` in `configs/client2api.json` — see
`config.example.json` for the same keys with their defaults. Every key is
optional; malformed JSON degrades to the defaults rather than failing the
module.

### Credentials

| Key | Meaning |
| --- | --- |
| `storage_path` | one explicit `storage.json` to read (skips discovery) |
| `storage_paths` | several such paths |
| `auto_discover` | default `true`; scan the standard `%APPDATA%` locations |
| `access_token` / `refresh_token` | pin a token directly (no file needed) |
| `user_id` | the `X-Uid` value; read from the credential when blank |
| `access_token_expiry` | RFC3339 or unix seconds/milliseconds |
| `machine_id` / `device_id` | override the discovered device fingerprint |
| `account_host` | overrides the host used for token exchange |
| `region` | default `CN` |

### Channel and model

| Key | Default | Meaning |
| --- | --- | --- |
| `chat_host` | `https://trae-api-cn.mchost.guru` | chat + catalogue host |
| `function` | `solo_work_lite` | forced into every request body |
| `model` | `glm-5.2` | written into both `model` and `config_name` |
| `models` | *(empty)* | replace the catalogue entirely |
| `max_tokens` | `0` | default cap when the request does not set one |

### Header identity (every version is configurable)

`user_agent`, `app_id`, `app_version`, `ide_version`, `ide_version_code`,
`ide_version_type`, `device_type`, `os_version`, `device_brand`, `device_cpu`,
`request_traffic_type`, `client_id`, and `extra_headers` (a string map merged
last, for headers neither reference sends).

The two references disagree on the IDE version: the Go port hardcodes
`0.1.52` / `20260811`, while the Node reference sends the IDE manifest version
(`3.3.67` CN / `3.5.51` SG) with a `20260401` code. The defaults here follow
the Go port (they are the values that were observed working); override them in
config to match whichever build you are imitating. `user_agent` defaults to
`Trae/` + `ide_version`. `x-ide-version` is the header that decides which model
catalogue the upstream hands back, so this pair matters more than the cosmetic
ones. Note that `TestHeaderVersionsAreConfigurable` pushes the CN-classic values
through a *custom* config to prove every one of them is honoured: it asserts
that config wins, not that CN-classic is the factory default, so it does not
contradict the defaults above.

Every request also carries the Node reference's trace pair
(`trae2api/src/auth.js:1179-1181`): `x-custom-trace-id` (32 hex) and
`x-flow-traceparent` = `04-<trace-id>-<span-id>-01`. The leading `04` is copied
verbatim — it is not the W3C default `00`, and "correcting" it would be a
regression. Both are minted per request and both are sent on non-streaming calls
too, matching the reference. `Last-Event-ID` is still not sent: it only matters
for resuming an interrupted stream, which this module never does.

### Transport fingerprint

This module ships a hand-built ClientHello, because the vendor's own client was
captured. The Trae CN and TRAE SOLO CN applications were run behind a blind TCP
passthrough that splits the TLS record out of the stream without terminating it,
and **both** of them sent the same hello to `api.trae.com.cn` — three connections
across two separate launches — JA3 md5 `3ad2731f6e4c8022713ec48eca82ade9`.

What that hello is:

| field | value |
| --- | --- |
| cipher suites | 18, with no TLS 1.3 suite among them |
| extensions | 7, in order: `0 10 11 13 35 23 65281` |
| `supported_groups` | `29-23-24` |
| `ec_point_formats` | `0` |
| ALPN | **absent** |
| `supported_versions` | **absent** |

Two absences are the fingerprint rather than an oversight. With no
`supported_versions` extension the handshake cannot leave TLS 1.2, and with no
ALPN the client never names a protocol. `ProfileTrae` reproduces the capture byte
for byte at the JA3 layer, and `internal/fingerprint` pins uTLS's `MaxVersion` to
TLS 1.2 for this profile so the stack does not plan a 1.3 handshake the hello
never advertised.

| key | values | default | meaning |
| --- | --- | --- | --- |
| `tls_profile` | a name from `fingerprint.Profiles()` | `""` | imitate that client's ClientHello |
| `tls_protocol` | `"h2"`, `"http/1.1"`, `""` | `""` | pin the ALPN; empty offers the profile's own list |

`configs/client2api.json` ships `tls_profile: "trae"` with
`tls_protocol: "http/1.1"`, so vendor-facing calls present the captured hello
exactly and nothing is widened. An unset `tls_profile` keeps whatever client the
core supplied, so the profile is opt-out rather than mandatory.

Verified live and not only in a unit test: a real `trae/custom_model_gpt-5` chat
answered HTTP 200 over the TLS 1.2-only handshake, which is the evidence that the
vendor's edge accepts what its own client sends. Only the TLS half is closed —
h2 frames would still be `golang.org/x/net/http2`'s — which is part of why the
shipped protocol is `http/1.1`.

### Behaviour

| Key | Default | Meaning |
| --- | --- | --- |
| `self_renew` | `false` | allow token refresh via `ExchangeToken` |
| `timeout_sec` | `120` | per short JSON call |
| `idle_timeout_sec` | `300` | streaming stall watchdog |
| `cooldown_sec` | `60` | base cooldown |
| `refresh_skew_sec` | `300` | refresh this long before expiry |
| `attempts` | `3` | upstream attempts before giving up |
| `extra_body` | `{}` | extra fields merged into the body when absent |

**`self_renew` is off by default on purpose.** Refreshing rotates the refresh
token family, and a rotation performed behind the desktop app's back can log
the user out of Trae. Turn it on only if you accept that. With it off, an
expired access token simply makes the account unusable until the desktop app
is opened once.

## Credential discovery

`New()` looks, in order, at:

1. an explicit `access_token` in the config;
2. `storage_path`, then each `storage_paths` entry;
3. `%APPDATA%\Trae CN\User\globalStorage\storage.json`,
   `%APPDATA%\TRAE SOLO CN\...`, `%APPDATA%\Trae\...`.

Each file is parsed for the property
`iCubeAuthInfo://icube.cloudide`, whose value is base64 of a `tc` container.
`tcrypt.go` unwraps it: the container is `[6-byte header][32 random][ciphertext]`,
the key is `sha512(sha512(random) XOR salt)[0:16]` and the IV `[16:32]`, the
cipher is AES-128-CBC, and the plaintext is `[64-byte sha512 digest][payload]`.
The digest is verified, so a wrong key is reported rather than returned as
garbage. The payload is the flat auth object (`token`, `refreshToken`,
`expiredAt`, `refreshExpiredAt`, `host`, `userId`, `account.username`, …).

The device fingerprint comes from the desktop app's `storage.json`, and the
mapping to headers is exact rather than interchangeable:

| Header | Source | Fallback |
| --- | --- | --- |
| `x-machine-id` | `telemetry.machineId` (64 hex) | — |
| `x-device-id` | the digits of the key `iCubeAuthInfo://icube-dc:<id>` (16 digits) | 32-bit rolling hash of the machine id |

The reference states this mapping directly (`trae2api/docs/PROTOCOL.md:26-27`)
and repeats it in code (`trae2api/src/auth.js:1143-1148`, with the comment
"SOLO real traffic uses aha device id (digits), not hash(machineId)").
`telemetry.devDeviceId` is read by the reference for its per-account pool
identity, **not** as the value of `x-device-id` — putting the UUID there would
be wrong even though the field's name makes it look right.

**This matters**: the upstream silently drops requests from an unregistered
device id — the TCP/TLS connection succeeds and no response ever arrives — so
the discovered values are used rather than invented ones (`auth.js:1163-1167`,
measured 2026-09-09). Override with `machine_id` / `device_id` only if you know
the real values.

A missing credential is not an error: the module loads, `Status()` explains
what is wrong, and `Chat` returns `core.ErrNotConfigured` (HTTP 503).  When
credentials exist but none is usable — every token expired while `self_renew` is
off — the terminal answer is a classified `auth` failure instead.

## Status

`GET /v1/status` → `clients.trae`:

- `ready` — some account has a live access token, or a refresh token while
  `self_renew` is on.
- `detail` — `"<n> ready, <n> cooling; channel solo_work_lite; self_renew off"`,
  plus a request/failure counter and the last error once something has failed.
- `accounts[]` — one entry per credential: `id` (user id), `label` (username),
  `enabled`, `state` (`ready` / `cooling` / `exhausted` / `invalid` /
  `unknown`), `expires_at` (RFC3339), `note` (the last classified failure), and
  `extra` with `product`, `source`, `failures`, and — once a `notify_usage`
  frame has been seen — `billing_mode`, `ide_credits`, `work_credits`.
- `models` — the bare model ids.

`Status()` never makes a network call and never blocks.

## Error handling

`Classify` reads the business `code` out of the body first, then the HTTP
status:

| Signal | Kind | Cooldown | Another account? |
| --- | --- | --- | --- |
| 1005 `plan_limit` | `ErrPlanLimit` | 12 h, `exhausted` | **no** |
| 4008 quota | `ErrQuota` | 6 h, `exhausted` | yes |
| 1001 / 4010 auth | `ErrAuth` | 60 s, `cooling` | yes |
| 4001 / 4023 bad request | `ErrParam` | 5 min, `cooling` | no |
| 4011 / 429 | `ErrSoftRate` | 60 s, `cooling` | yes |
| 9074 check-in contention | `ErrRetryLater` | 5 min, `cooling` | yes |
| 401 | `ErrSessionDead` | 1 h, `invalid` | yes |
| 404 | `ErrNotFound` | 60 s, `cooling` | no |
| ≥ 500 | `ErrServer` | 30 s, `cooling` | yes |
| other 4xx | `ErrClient` | 5 min, `cooling` | no |

The **business-code failover set is exactly `{4008, 1001, 4010}`**
(`FailoverCode`); `1005` is deliberately excluded, and `4001` is treated as a
request problem rather than an account problem — the upstream spec's table
marks `4001` "failover" while its own summary line restricts failover to
`{4008, 1001, 4010}`, and the protocol notes say an unknown model must not
switch accounts. The stricter reading is implemented and tested.

A business failure can also arrive as `event:error` **inside an HTTP 200 SSE
stream**; that path is parsed and classified, not inferred from the status
code.

### The shared failure contract

The terminal error of a `Chat` call is wrapped with
`core.Fail("trae", <account>, <kind>, <status>, err)` (`failure.go`), so the
gateway's rotation, per-account attribution and HTTP mapping act on it instead
of seeing an opaque `502 upstream_error`:

| trae kind | `core.FailureKind` | gateway may retry |
| --- | --- | --- |
| `ErrPlanLimit`, `ErrQuota` | `quota` | yes |
| `ErrAuth` | `auth` | yes |
| `ErrSessionDead` | `session_dead` | yes |
| `ErrSoftRate`, `ErrRetryLater` | `rate_limited` | yes |
| `ErrServer`, `ErrTransport` | `upstream` | yes |
| `ErrParam`, `ErrNotFound`, `ErrClient` | `other` | no |

The account named on the failure is the one that produced the terminal answer,
not whoever went first.  Request-level errors stay `other` on purpose: trae does
not rotate them and `core.Retryable` excludes `other`, so the gateway cannot
start rotating where the module refuses to.  One divergence is deliberate and
worth knowing — a plan limit is `quota`, which the shared contract calls
retryable, so the gateway may offer a `1005` to another account; the module's own
loop still never rotates it, and the account that hit it is parked for 12 h.

`Chat` also paces its own rotation: before the second and every later attempt it
waits `core.SleepCtx(ctx, core.JitterDur(core.BackoffFrom(base, attempt)))` with
`base = core.RotateBackoffBase` (500 ms), and stops immediately if the caller's
context dies.  trae has no config knob for this — `cooldown_sec` is the pool's
per-account cooldown, not an inter-attempt wait.

## Known gaps

- **The catalogue over-advertises.** `/v1/models` mirrors the upstream list, and
  some of those ids are not actually servable through the SOLO channel. Observed
  on a live account: `gpt-5.5` is listed but every request returns business code
  `4001` `param is invalid` — streaming *and* non-streaming, deterministically.
  A streaming client sees a `role` frame, then an `event:error` frame carrying
  `4001`, then `[DONE]`; a non-streaming client sees the same failure as an
  immediate `502`. Verified working on the same account: `glm-5.2`, `glm-5.3`,
  `kimi-k3`, `DeepSeek-V4-Pro`, `Doubao-Seed-2.1-Pro`, `custom_model_gpt-5`.
  Agent-internal ids in the list (`summary`, `sagitta`, `aquila`,
  `explore_sub_agent_v2`, `browser_use_subagent`, `file_search_agent`,
  `computer_use_subagent`) are almost certainly not chat models either. A listed
  id is therefore not a promise that it works.
- **One channel.** Only the SOLO channel (`solo_work_lite`) is implemented.
  Classic Trae CN (`chat_v3`) has a different, mutually exclusive catalogue;
  the plumbing (configurable `function` and `models`) is in place, but the
  channel-specific body/response differences have not been verified.
- **No OAuth web login.** `trae2api-web` can drive a browser login; this module
  only reuses a credential the desktop app already wrote. Implementing login
  would need a second `http.Server` on `127.0.0.1:18080` for the callback, and
  the upstream `/authorization` URL was observed to carry no `state`/`nonce`
  binding — a CSRF hole that should be fixed before shipping it.
- **`create_agent_task` is not implemented** (neither reference does it), and
  the TRAE Work channel is not reproducible.
- **Token rotation is opt-in** (`self_renew`), so an expired access token needs
  the desktop app; see above.
- **Device fingerprint is discovered, not minted**, because minted ids are
  silently dropped upstream.
- **`stream:false` still streams.** The upstream has no buffered mode; the
  gateway's `bufferCompletion` reassembles the frames, so a non-streaming
  client sees a normal `chat.completion`.

## Tests

`go test ./clients/trae/...` runs entirely offline: the transport is a fake
`http.RoundTripper`, credentials live in `t.TempDir()`, and the SSE and `tc`
fixtures are inline literals. Covered: the payload rewriting rules (content
must become an array, tool parameters must become a JSON string, the model must
land in both `model` and `config_name`, `function` is forced, `tool_choice`
normalisation), the SSE scanner including `event:error` inside an HTTP 200
stream, the error classifier and the `{4008, 1001, 4010}` failover set with
`1005` excluded, account failover, the cooldown policy, the pool, credential
discovery, and the `tc` decryptor against a recorded container.
`failure_test.go` adds the shared contract on top, against a local
`httptest.Server`: the vendor code → `core.FailureKind` mapping per code, a
`1005` that provably never spends a second account, `core.Retryable` agreement
(with the plan-limit divergence pinned), the inter-attempt pacing, an aborted
backoff on a cancelled context, and a nil `deps.Guard`.

`CLIENT2API_LIVE=1 go test ./clients/trae/... -run TestLive -v` performs one
real request using the credential on this machine.

## Provenance

Ported from the MIT-licensed references in `client2api-lab/_upstream`:

- `trae2api-web` (Go 1.22, MIT) — the port target: `payload.go`, `solosse.go`,
  `client.go`, `headers.go`, `constants.go`.
- `trae2api` (Node 22.5+, MIT, © wangqi233) — the written spec: `docs/PROTOCOL.md`
  and `src/trae-decrypt.js`, plus the header/identity detail in `src/auth.js`.

Pure standard library — no third-party dependency was added. `crypto/aes`,
`crypto/cipher`, `crypto/sha512` and `encoding/base64` cover the container.
