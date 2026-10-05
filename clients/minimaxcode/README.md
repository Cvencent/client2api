# `minimaxcode` client

A `client2api` client module that exposes the **MiniMax Code** desktop client's
upstream as an OpenAI-compatible chat endpoint.

* Route prefix: `minimaxcode/<model>` (e.g. `minimaxcode/MiniMax-M3`)
* Upstream wire protocol: **Anthropic Messages**
  (`POST https://agent.minimax.cn/mavis/api/v1/llm/v1/messages`)
* Translation: OpenAI Chat Completions ⇄ Anthropic Messages (request *and*
  response, streaming and non-streaming)
* Dependencies: the Go standard library, `client2api/internal/core`, and
  `client2api/internal/fingerprint` for the TLS ClientHello.

The gateway already speaks OpenAI, so this module's job is the
OpenAI → Anthropic → OpenAI round trip: it builds an Anthropic request body,
talks to the upstream, and decodes the Anthropic SSE stream (or the
non-streaming JSON document) back into `core.Event`s.

---

## Model ids

| Model | Context | Max output | Notes |
| --- | --- | --- | --- |
| `MiniMax-M3.1-Flash-Preview` | 512 000 | 128 000 | text/image/video, up to 4 attachments, reasoning forced on |
| `MiniMax-M3` | 512 000 | 128 000 | text/image/video, up to 9 attachments, reasoning switchable |
| `MiniMax-M2.7-highspeed` | 200 000 | 128 000 | text only, reasoning forced on |
| `MiniMax-M2.7` | 200 000 | 128 000 | text only, reasoning forced on |

**This list comes from the desktop client's own `config.yaml`**, not from the
vendor: MiniMax publishes **no model-list endpoint** for this product. The
obvious candidate answers

```
GET https://agent.minimax.cn/mavis/api/v1/llm/v1/models   (Authorization: Bearer …)
503 {"error":"open platform service unavailable, please retry","errorCode":50115,
     "errorReason":"direct_route_not_configured"}
```

so `Models()` reads `<home>/.minimax/config.yaml` instead — the `models:` blocks
for the limits and the `whitelist:` for the order. A missing or malformed file
degrades to the compiled-in table above **without an error**; the `models` config
key is an operator filter applied on top.

`Models()` never makes a network call: the gateway calls it on every `/v1/models`
request and the panel calls it on every refresh.

---

## Configuration

The module reads the JSON object under `clients.minimaxcode` in the host config.
Every key is optional; an absent or malformed object degrades to the defaults
below (a malformed object is logged and then ignored — it never fails the
factory).

```jsonc
{
  "clients": {
    "minimaxcode": {
      "accounts": [ /* see below */ ],
      "base_url": "",
      "auth_dir": "",
      "config_yaml": "",
      "models": ["MiniMax-M3.1-Flash-Preview", "MiniMax-M3", "MiniMax-M2.7-highspeed", "MiniMax-M2.7"],
      "auto_discover": true,
      "max_tokens_default": 8192,
      "max_account_attempts": 3,
      "cooldown_seconds": 120,
      "timeout_seconds": 120,
      "refresh_enabled": true,
      "oauth_token_url": "https://account.minimax.cn/oauth2/token",
      "oauth_client_id": "",
      "tls_profile": "chrome",
      "tls_protocol": "http/1.1"
    }
  }
}
```

### Top-level keys

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `accounts` | array | `[]` | Explicit credentials, tried before discovered ones. |
| `base_url` | string | `https://agent.minimax.cn/mavis/api/v1/llm` | Upstream family root; `/v1/messages` is appended. |
| `auth_dir` | string | `<home>/.minimax/auth` | Root of the desktop client's credential store. |
| `config_yaml` | string | `<home>/.minimax/config.yaml` | The desktop client's own model catalogue. |
| `models` | array | `[]` (publish everything the catalogue holds) | Operator filter on the advertised ids. |
| `auto_discover` | bool | `true` | Read credentials already on the machine (see *Credential discovery*). |
| `max_tokens_default` | int | `8192` | `max_tokens` when the caller sends none. |
| `max_account_attempts` | int | `3` | Upper bound on upstream attempts per `Chat` call (one per account at most). |
| `cooldown_seconds` | int | `120` | How long an account is parked after a `429` / risk-control / upstream failure. |
| `timeout_seconds` | int | `120` | Per-attempt deadline. |
| `refresh_enabled` | bool | `true` | Allow the OAuth refresh path (see *Credential renewal*). |
| `oauth_token_url` | string | `https://account.minimax.cn/oauth2/token` | Token endpoint used for renewal. |
| `oauth_client_id` | string | `""` (the account's own client id, then `mcode_tool`, then `mcode-public`) | OAuth client id presented when renewing. |
| `tls_profile` | string | `""` | Imitate that client's TLS ClientHello (`chrome`, `node`, `firefox`, …). |
| `tls_protocol` | string | `""` | Pin ALPN: `h2`, `http/1.1`, or empty for the profile's own list. |
| `timezone` | string | inferred from the host | IANA zone sent as `timezone_id` on the daily sign-in routes. Only set it when the account's own zone differs from this machine's. |

### `accounts[]` entries

| Key | Type | Meaning |
| --- | --- | --- |
| `id` | string | Stable identifier used in `Status()` and for persisted state. Generated if omitted. |
| `label` | string | Human label for the panel. Falls back to `id`. |
| `access_token` | string | The `mmoat_…` bearer token. |
| `base_url` | string | Per-account upstream override. |
| `enabled` | bool | `false` parks the account without deleting it. |

---

## Transport fingerprint

Two keys replace the TLS ClientHello, so vendor-facing calls stop carrying Go's
handshake. An unset `tls_profile` keeps whatever client the core supplied, byte
for byte.

| key | values | default | meaning |
| --- | --- | --- | --- |
| `tls_profile` | a name from `fingerprint.Profiles()` | `""` | imitate that client's ClientHello |
| `tls_protocol` | `"h2"`, `"http/1.1"`, `""` | `""` | pin the ALPN |

A named profile replaces the `*http.Client` in `New` *before* it is handed to the
account pool, so every credential path — chat, probe, and OAuth renewal — goes
through it.

**`chrome` is an inference, not a capture.** The request-level capture of the
desktop client recorded the headers only (`Authorization`, `Content-Type`,
`anthropic-version`), not the handshake. MiniMax Code is an Electron application
and its chat path is `@ai-sdk/anthropic`, so a Chromium-shaped hello is the
closest available preset; `configs/client2api.json` therefore ships
`tls_profile: "chrome"`. Nothing here was derived from a MiniMax capture, and the
module does not claim otherwise.

The `User-Agent` sent to the upstream is likewise **inferred**:
`Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko)
Chrome/131.0.0.0 Safari/537.36`. The capture did not record one, and Go's default
UA would be a louder tell than a Chromium one.

Only the TLS half is closed: the h2 frames are still written by
`golang.org/x/net/http2`, so an h2 connection remains a Go-shaped h2 connection.
A proxy in `Deps.Proxy` forces HTTP/1.1 whatever you set, because
`x/net/http2` cannot tunnel through one.

---

## Credential discovery

With `auto_discover` on (the default), the module reads the credential the
MiniMax Code desktop client already wrote to the current user's profile. It never
writes to those files.

| Source | What is read | Becomes |
| --- | --- | --- |
| `<auth_dir>/<buildEnv>/<region>/<clientId>/auth.json` | the newest `mmoat_…` record | one account, `id` = `minimaxcode:<buildEnv>/<region>/<clientId>` |
| the same directory's `auth-state.json` | `buildEnv`, `region`, `clientId` when the path alone is ambiguous | fills the gaps in the id |
| `%APPDATA%\MiniMax\minimax-agent-cn-config.json` | `sharedUser.subUserName` | the account's label |

Rules that matter:

* The records map is keyed by strings that contain a **NUL byte**, so its key
  shape is not assumed. The module walks the map and picks the record whose
  `accessToken` starts `mmoat_` and whose `expiresAtMs` is the largest; if none
  carries that prefix it falls back to the record with the largest `expiresAtMs`
  and any non-empty `accessToken`.
* A store with no usable token produces **no row at all** — it is skipped, not
  surfaced as a broken account.
* `auth.json` is read only. Import copies the credential into this module's own
  store; the desktop client's file is byte-for-byte unchanged, and a test asserts
  exactly that.
* The label comes from the desktop client's own config, so a discovered row reads
  as the operator's MiniMax account name (`MiniMax558560` on the machine this was
  developed against) rather than a machine id.

Explicit `accounts` are always tried first; discovered ones fill in behind them.

---

## Credential renewal

MiniMax rotates the refresh token on every use, so renewal is deliberately
narrow. A row can renew when it holds a refresh token **and** a durable home for
the rotated pair: a credential discovered in the desktop client's store writes
back into that store, while a row the panel signed in itself owns its pair in
`managed_accounts.json`.

* It only fires when the stored `expiresAtMs` is within 60 seconds of now (or
  already past). A healthy token is never re-rotated.
* `POST <oauth_token_url>` with
  `{"grant_type":"refresh_token","refresh_token":"…","client_id":"…"}`.
* The client id is taken from the account's own id first, then `oauth_client_id`,
  then `mcode_tool` (the CLI) and `mcode-public` (the store), in that order.
* On success the new `accessToken` / `refreshToken` / `expiresAtMs` /
  `generation` are written back **atomically to the same `auth.json`**, as an
  edit of one record inside a decoded `map[string]any` — every other field, every
  other record, and the NUL-bearing keys survive. A panel-signed-in row has no
  `auth.json`, so its rotated pair is rewritten to `managed_accounts.json`
  instead: MiniMax has already retired the old refresh token by then, and a pair
  lost at exit is a manual re-login later.
* A panel-signed-in row never inherits the runtime state file's verdict for the
  same id (`accounts.json` deliberately does not record managed rows), and an
  empty desktop store does not park it either: the panel's own sign-in is the
  authority on that row.
* A failed renewal is logged and the request proceeds with the old token; it
  never fails the chat.
* Before spending its own copy of the refresh token, the module re-reads the
  shared store. If the desktop client signed in again or rotated the token
  since this process last looked, that record is adopted instead — the two
  writers share one single-use token, and racing it is what turns a renewal
  into `invalid_grant`.
* A vendor rejection of the refresh token itself (`invalid_grant`, "this
  refresh token can no longer be used") retires the whole authorization:
  the row is parked with the note `login_required` so the panel reads
  "需要重新登录" instead of a downstream `401`. A store the client has
  emptied (a sign-out) is treated the same way, without a wasted request.
* Renewal is **single-flight per account**: concurrent requests that all see a
  lapsed token queue on one per-account lock, and everyone after the first adopts
  the token that exchange produced instead of spending the (single-use) refresh
  token again. `POST .../accounts/refresh` takes the same lock.

`refresh_enabled: false` turns the whole path off, and the module then reports an
expired credential as an ordinary `401` instead.

---

## Status

`Status()` is cheap by construction: it makes **no network calls** and takes a
mutex, so it is safe for the panel's 10-second refresh.

* `Ready` — true when at least one account is selectable right now.
* `Detail` — `N of M account(s) usable`, plus `cooling=` / `exhausted=` /
  `invalid=` when non-zero and the last upstream error. With no accounts at all
  it reads
  `no account: add one in the panel, or sign in to MiniMax Code on this machine so its credential store can be read`.
* `Accounts[]` — `ID`, `Label`, `Enabled`, `State`
  (`ready` / `cooling` / `exhausted` / `invalid`), `ExpiresAt` (RFC 3339), and
  `Note`. A disabled row always reads `invalid`, so the table is never greener
  than the request path.

**Secrets never appear in `Status()`.** An account carries its credential in an
unexported field, the persisted state file holds no secret at all, and an
upstream error body is passed through `redactSecret` before it can become a note.

---

## Panel account management

The module implements seven of the optional panel capabilities:

| Capability | Implemented | Notes |
| --- | --- | --- |
| `core.AccountManager` | **yes** | list, add, remove, enable/disable, test, refresh |
| `core.CredentialImporter` | **yes** | read-only discovery of the desktop client's store, then an explicit import |
| `core.ModelRefresher` | **yes** | re-reads the desktop client's own `config.yaml` (no vendor endpoint exists) |
| `core.LoginProvider` | **yes** | RFC 8628 device grant against `account.minimax.cn`; the panel shows the URL and user code and polls to completion. The GUI is an alternative, not a requirement |
| `core.CheckinProvider` | **yes** | the seven-day daily sign-in; status-then-claim, never claims blind |
| `core.BalanceProvider` | **yes** | `/minimax-cloud/api/v1/credit/details` |
| `core.PackageProvider` | **yes** | the same credit tranches, listed as packages |
| `core.Reviver` | **yes** | clears the park; for a discovered row it adopts the credential the operator's fresh sign-in left in `auth.json` |

A capability that is not implemented is not implemented *as an interface*, so the
panel hides the button instead of the route answering `501` at click time.

### Field schema

| Key | Type | Required | Notes |
| --- | --- | --- | --- |
| `access_token` | `password` | yes | max 4096 bytes; the `mmoat_…` token |
| `label` | `text` | no | control characters stripped, truncated to 120 runes |
| `base_url` | `text` | no | must start `http://` or `https://`, max 512 bytes |

Validation failures **name the offending field** and never echo the value.

### Where accounts live

| File | Contents |
| --- | --- |
| `<data_dir>/minimaxcode/managed_accounts.json` | accounts added or signed in through the panel, **credentials in clear text**, mode `0600`. A sign-in row also carries `refresh_token`, `client_id` and `expires_at`, because MiniMax retires a refresh token the moment it is exchanged. Written only here; the main config file is never touched. |
| `<data_dir>/minimaxcode/accounts.json` | runtime state only — no secret at all — plus a `removed` array of ids the operator deleted, so discovery cannot resurrect them. |
| `~/.minimax/auth/**` | the desktop client's own files. **Read only.** |

Account ids are stable:

* hand-added accounts get `minimaxcode:managed:<8 hex>`, where the hex is the
  first 4 bytes of the SHA-256 of the credential, or the `id` you supply;
* discovered credentials get `minimaxcode:<buildEnv>/<region>/<clientId>`;
* credentials the panel signed in get that same `<buildEnv>/<region>/<clientId>`
  id, so a sign-in takes over the row discovery would have produced instead of
  standing beside it.

`AccountRecord.Fields` carries only non-secret metadata: `source`, `origin`, a
`tag` (the same 8-hex fingerprint), and `refreshable: true` when the row holds a
refresh token. No secret, no prefix of one, and no user id is ever published.

### Actions

| Action | Behaviour |
| --- | --- |
| `GET .../accounts` | every account from the config, the desktop client, and `managed_accounts.json`. An empty pool is an empty list, never an error. |
| `POST .../accounts` | validates, appends to the live pool and to `managed_accounts.json`. Re-adding a credential that was removed earlier restores it. |
| `DELETE .../accounts/<id>` | a managed account is really deleted; an account that came from the config or from discovery is **tombstoned** in `accounts.json` instead, so it stays hidden across restarts without the desktop client's file being touched. Removing a tombstoned id again is a no-op; an unknown id is an error. |
| `POST .../accounts/<id>/enabled` | updates the live pool and both files; a disabled account is no longer selectable. |
| `POST .../accounts/<id>/test` | one minimal request (`max_tokens=32`, `"say hi in 3 words"`) against the first advertised model. |
| `POST .../accounts/refresh` | with no `id`, refreshes every account that holds a refresh token. |
| `GET .../discover` | `<auth_dir>/<env>/<region>/<clientId>/auth.json`, one entry each. |
| `POST .../import` | `paths` (or `all: true`). The credential is copied into `managed_accounts.json`; the source file is left alone. Nothing importable at an explicit path is an error. |
| `POST .../login`, `GET .../login/<session>` | the browser-driven device-code sign-in above; the session is created by the first call and polled by the second. |

### Testing an account

`TestAccount` reports an upstream refusal as a **result**, not as a Go error —
this is the point of the action:

| Outcome | `TestResult` |
| --- | --- |
| a normal completion | `OK: true`, `Model`, `Reply` (truncated to 400 characters), `ElapsedMS` |
| `401`, `403`, `402`, `429`, risk control, `5xx` | `OK: false`, `Error` carrying the classified upstream error |
| unknown or empty `id` | a real Go error; the network is never touched |

A probe is a genuine observation, so it moves the account's state like any other
request: a `401` leaves it `invalid`, a `429` leaves it `cooling`.

### Refreshing an account

| Target | Result |
| --- | --- |
| a discovered account with a refresh token | the token is renewed and written back; `OK: true` |
| a credential typed into the panel | `OK: false`, `this credential carries no refresh token: import the desktop client's store (or sign in to MiniMax Code on this machine) so the module can renew it in place` — **no request is sent** |
| an id this module never held | a real Go error |
| no `id`, and nothing holds a refresh token | a real Go error |

### Error mapping

| Situation | Result |
| --- | --- |
| invalid form (empty or oversized token, bad `base_url`) | Go error naming the field, HTTP 400 from the panel |
| unknown account id (remove, enable, test, refresh) | Go error |
| upstream refusal during a test | `TestResult{OK: false, Error: …}`, no Go error |
| nothing importable at an explicit path | Go error naming the path |

### Sign-in

`POST .../login` starts the same OAuth 2.0 Device Authorization Grant (RFC
8628) the desktop client uses; it is not a stub and it does not need the GUI:

1. the module asks `<account origin>/oauth2/device/code` for a device code,
   sending the desktop client's own `client_id=mcode-public`,
   `scope=agent.default`, `audience=agent-backend`, and an S256 PKCE
   challenge;
2. the panel shows the `verification_uri` and the short `user_code`, then polls
   `GET .../login/<session>` on a timer -- the module itself rate-limits those
   polls to the vendor's interval, so the panel's 2 s cadence cannot turn into
   `slow_down`;
3. once the operator approves in a browser, the token endpoint returns an
   access/refresh pair, which is written to `managed_accounts.json` under the
   same `<buildEnv>/<region>/<clientId>` id discovery builds -- so signing in
   again replaces a broken row instead of stacking a second one beside it.

The CN service has been observed to answer the device-authorization request
with a `user_code` and an expiry in milliseconds but **no** `device_code`; the
module polls that variant by `user_code`, exactly as the desktop client does.

The protocol was recovered from the desktop bundle rather than captured from a
live sign-in, so the flow is pinned by a scripted transport instead of a real
authorization.

### Not implemented

* **Writing to the desktop client's store.** Discovery and import are read-only;
  the only write to `~/.minimax` is the atomic refresh write-back described
  above, and only when the stored token is about to expire.

### Daily sign-in and credits

The desktop client keeps a seven-day check-in board and a credit ledger on the
same host as the model endpoint, under `/minimax-cloud/api/v1/`. Both are
implemented here:

| Route | Method | Used for |
| --- | --- | --- |
| `/minimax-cloud/api/v1/signin/status?timezone_id=…` | `GET` | the seven-day board |
| `/minimax-cloud/api/v1/signin/claim?timezone_id=…` | `POST` (`{}`) | claiming today's reward |
| `/minimax-cloud/api/v1/credit/details` | `GET` | the granted credit tranches |

Three details are easy to get wrong and are handled deliberately:

* **`timezone_id` is a query parameter, and the claim needs it too.** Without it
  both sign-in routes answer `base_resp.status_code 1406010011`
  `invalid timezone_id`. The desktop client only gets away with a bare POST on
  the claim because its shared axios instance injects the parameter. The zone is
  inferred from the host (`TZ`, then a UTC-offset table tie-broken by the
  machine's own abbreviation, then `Asia/Shanghai`) and can be overridden with
  the `timezone` config key.
* **A wrong-but-valid zone is not an error.** Asking for `Europe/London` while
  the account lives in `Asia/Shanghai` answers `200` with a board on which *no*
  day is `is_today`. Treating that as success would silently never claim, so the
  module reports it and names the zone it used instead.
* **`credit/details` has no `data` wrapper.** Its `details` and `total_count`
  sit at the top level beside `base_resp`, unlike the sign-in routes. The shared
  `cloudCall` helper therefore returns the raw body as well as the decoded
  envelope.

Amounts on the credit ledger are decimal *strings* (`"781.47"`), and the panel's
balance fields are `int64`, so they are rounded to the nearest whole credit; a
negative remaining amount is clamped to zero, mirroring the desktop client.
`credit_type 2` is check-in credit and `credit_type 1` is purchased credit, which
is how the package rows get their 签到积分 / 购买积分 names. Check-in credits
expire 30 days after they are granted.

`Checkin` is status-then-claim and never claims blind: an already-claimed day is
a success with `already_done`, and anything the vendor refuses comes back as a
`CheckinResult` with `OK: false` and a nil error. `AccountBalance` and
`AccountPackages`, by contrast, do return a Go error on a vendor failure,
because the panel has no number to show without one.

---

## Behaviour

### Retry and account state

`Chat` walks the pool, at most `max_account_attempts` times, never retrying the
same account inside one call:

| Upstream signal | Classification | Action |
| --- | --- | --- |
| `400` / `404` / `422` naming a model, or `errorCode 50115` | model rejected | **return `core.ErrUnsupported` immediately** (not account-specific, so the pool is not rotated) |
| `401`, or a `403` with no risk wording | bad credential | mark `invalid`, try the next |
| `402`, or an exhaust keyword (`insufficient`, `balance`, `quota`, `exhaust`, `no resource`, `recharge`, `欠费`, `余额不足`) | quota exhausted | mark `exhausted`, try the next |
| `403` with risk wording (`unusual activity`, `risk`, `blocked`, `风控`, `异常`) | risk control | cool the account, try the next, and report the WAF to `Deps.Guard` |
| `429` | rate limited | cool the account, try the next |
| `5xx` | upstream | cool the account, try the next |
| anything else | — | try the next |

Order matters: the HTTP status is decided first, so a `429` whose body happens to
say "quota exceeded" stays a rate limit instead of wrongly exhausting the
account.

`Chat` also inspects an **HTTP 200 body**: the vendor reports application errors
in an envelope (`{"base_resp":{"status_code":…}}`, or an Anthropic `error`
member) and those are classified by the same rules. Only a healthy payload is
passed on.

When no account is usable, `Chat` returns `core.ErrNotConfigured` (HTTP 503 at
the gateway) with a detail line explaining why.

### Streaming vs. non-streaming

`Chat` honours `req.Stream` and always returns a `core.Stream`:

* `Stream: true` — the upstream is asked to stream; its Anthropic SSE frames are
  decoded into events as they arrive.
* `Stream: false` — the upstream returns one JSON document, which is decoded into
  the same event sequence (text, tool calls, usage, done) so the gateway's
  aggregator sees a uniform shape.

Emitted at most once each: `EventUsage` and `EventDone`. `Recv` reports `io.EOF`
once the stream is finished; `Close` is idempotent and always closes the upstream
body and cancels the request context.

Finish reasons map as `end_turn` / `stop_sequence` / `stop` / `pause_turn` →
`stop`, `max_tokens` → `length`, `tool_use` → `tool_calls`, `refusal` →
`content_filter`.

Usage maps as
`prompt_tokens = input_tokens + cache_read_input_tokens + cache_creation_input_tokens`,
`cached_tokens = cache_read_input_tokens`, `completion_tokens = output_tokens`.

### Headers

Every request carries exactly what the capture showed, and nothing more:

```
Content-Type: application/json
Accept: application/json
anthropic-version: 2023-06-01
User-Agent: Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36
Authorization: Bearer <mmoat_…>
```

**No `x-api-key`** is ever sent: the capture showed `Authorization: Bearer` and
only that, and a test asserts the module does not add one.

### Message shape

`core.Message` is normalised to Anthropic's block form, so a plain string prompt
goes out as

```json
{"role":"user","content":[{"type":"text","text":"say hi in 3 words"}]}
```

Anthropic accepts either form; blocks are what the translation layer already
produces for images and tool results, so there is one shape on the wire rather
than two.

---

## Known gaps

* **The TLS profile is an inference.** See *Transport fingerprint*: MiniMax Code
  is an Electron app, `chrome` is the nearest preset, and no MiniMax handshake
  was captured to confirm it. The request headers, however, are from a capture.
* **The sign-in flow is not live-verified.** The device-code endpoints, the
  `mcode-public` client id, the scopes and the CN `user_code` variant were
  recovered from the desktop bundle; the module's own start/poll/complete path is
  pinned by a scripted transport, but no real authorization has been completed
  from this machine.
* **The `User-Agent` is an inference.** The capture recorded no UA; a Chromium
  one is sent in place of Go's.
* **No model-list endpoint.** `RefreshModels` re-reads a **local file**; it is not
  a network refresh, and the README says so rather than implying the vendor was
  asked. If the desktop client is not installed on the machine, the compiled-in
  table is all there is.
* **The catalogue is a local file, so it can go stale.** A model the vendor
  removed will still be advertised until `config.yaml` changes.
* **Thinking signatures are dropped.** Anthropic thinking deltas carry a
  `signature` that must be round-tripped for multi-turn reasoning replay;
  `core.Message` has nowhere to carry it, so `signature_delta` frames are ignored.
* **Remote images are dropped.** An `image_url` part pointing at an `http(s)` URL
  cannot be expressed as an Anthropic base64 source without fetching it; only
  `data:` URLs are forwarded. The part is skipped, not turned into an error.
* **Refresh is single-flight per account.** Two concurrent `Chat` calls that both
  see an expiring token produce **one** exchange: the second waits on a per-account
  lock, re-reads the pool, and adopts the token the first one produced. MiniMax
  rotates the refresh token on every use, so without this the loser of that race
  would write a dead token into the shared store. The credential is read and
  written as a snapshot under the pool lock, so no request ever reads a field a
  concurrent refresh is rewriting (verified with `go test -race`). The manual
  `POST .../accounts/refresh` path takes the same lock.
* **The refresh token is written back in clear text**, because that is how the
  desktop client stores it and the file is the shared source of truth. The module
  does not encrypt it and does not claim to.
* **`auth-state.json` is trusted for location only.** Its `status` field is not
  consulted as proof of sign-in; a store whose records hold no usable token
  yields no account, whatever the state file says.
* **A tombstoned account is only hidden here.** Removing a discovered row records
  the intent in this module's `accounts.json`; the desktop client's store still
  holds the credential, and the MiniMax GUI will still show the account.

---

## Testing

```sh
go vet ./clients/minimaxcode/...
go test ./clients/minimaxcode/... -v
```

Everything runs **offline**: upstream payloads are inline string fixtures and the
transport is a fake `http.RoundTripper`. Discovery tests build their own
credential store in a temporary directory, and `newTestClient` turns
`auto_discover` **off** unless a test asks for it, so the developer's real
`~/.minimax` store can never leak into a run.

---

## Live verification

Recorded against a temporary instance (spare port, temporary `data_dir`, and a
**copy** of the credential store so the operator's own file could not be
touched). The transport was real; the upstream was the vendor.

| Check | Result |
| --- | --- |
| `GET /healthz` | `"clients":[…,"minimaxcode",…]`; startup log: `[minimaxcode] minimaxcode: tls handshake imitates the chrome client` / `[minimaxcode] loaded` / `clients: 14 loaded, 0 skipped, registered=14` |
| `GET /v1/models` | `minimaxcode/MiniMax-M2.7`, `minimaxcode/MiniMax-M2.7-highspeed`, `minimaxcode/MiniMax-M3`, `minimaxcode/MiniMax-M3.1-Flash-Preview` |
| `GET /panel/api/clients/minimaxcode/capabilities` | `manage`, `import`, `refresh_models` true; every other flag false |
| `GET /panel/api/clients/minimaxcode/accounts` | one row: `minimaxcode:prod/cn/mcode-public`, label `MiniMax558560`, `state: "ready"`, `expires_at: "2026-09-29T16:38:28Z"`, fields `{origin, refreshable:true, source:"discovered", tag:"9cdfd475"}` — **no token** |
| `POST /v1/chat/completions` (non-streaming, `MiniMax-M3`, `max_tokens:32`) | **200** in 1444 ms — `"content":"Hey there, friend!"`, `finish_reason:"stop"`, `usage {prompt_tokens:169, completion_tokens:6, total_tokens:175, prompt_tokens_details:{cached_tokens:128}}` |
| `POST /v1/chat/completions` (streaming, `MiniMax-M3`, `max_tokens:48`) | **200** in 701 ms, 1098 bytes — deltas `"1"`, `", 2, "`, `"3."`, then `finish_reason:"stop"`, then `[DONE]` |
| credential write-back | none: the store's `auth.json` was byte-identical and its mtime unchanged after every call |
| module state | `data/minimaxcode/accounts.json` written; no secret in it |

The non-streaming usage figures are the point of interest: `prompt_tokens` is
`41 + 128 + 0` — input, cache read, and cache creation summed — which is the
mapping the vendor's own envelope implies.

---

## Provenance

**Clean-room.** No code, comment, identifier, or file structure was copied from
any other implementation. What was read, and what it was used for:

* the Anthropic Messages request/response shape and SSE frame names — protocol
  facts;
* the endpoint, the `Authorization: Bearer` header, and `anthropic-version` —
  observed in a request capture;
* the credential file layout under `~/.minimax/auth/`, the `mmoat_`/`mmort_`
  prefixes, and the `config.yaml` model blocks — needed to discover accounts and
  read the catalogue;
* the OAuth refresh endpoint and its `invalid_grant` behaviour — verified by
  sending a deliberately invalid refresh token.

The upstream endpoint, header names, and error codes are interoperability facts,
not creative expression.

### Security note

A live access token was observed in clear text in a credential file during recon.
**That token should be rotated.** It is not hardcoded here, not committed here,
and never logged: this module masks secrets with `core.MaskSecret` and
`redactSecret` before they can reach a log line or a note.
