# `zcode` client

A `client2api` client module that exposes the **ZCode** coding-plan upstream as an
OpenAI-compatible chat endpoint.

* Route prefix: `zcode/<model>` (e.g. `zcode/GLM-5.3`)
* Upstream wire protocol: **Anthropic Messages** (`POST <base>/v1/messages`)
* Translation: OpenAI Chat Completions ⇄ Anthropic Messages (request *and* response,
  streaming and non-streaming)
* Dependencies: **standard library only** — nothing outside `client2api/internal/core`
  and the Go standard library is imported.

The gateway already speaks OpenAI, so this module's job is the OpenAI → Anthropic →
OpenAI round trip: it builds an Anthropic request body, talks to the upstream, and
decodes the Anthropic SSE stream (or the non-streaming JSON document) back into
`core.Event`s.

---

## Model ids

| Model | Notes |
| --- | --- |
| `GLM-5.3` | 1M context, 128K max output |
| `GLM-5.3-Flash` | 1M context, 128K max output |
| `GLM-5.3-FlashX` | 1M context, 128K max output |
| `GLM-5.2` | 1M context, 128K max output |
| `GLM-5.1` | 200K context, 128K max output |
| `GLM-5` | 200K context, 128K max output |
| `GLM-5-Turbo` | 200K context, 128K max output |
| `GLM-4.7` | 200K context, 128K max output |
| `GLM-4.6` | 200K context, 128K max output |
| `GLM-4.5` | 128K context, 96K max output |
| `GLM-4.5-Air` | 128K context, 96K max output |

The upstream is the authority on this list. A model it refuses answers with the
envelope code `3006` ("model not allowed"), which this module maps to
`core.ErrUnsupported` (HTTP 400 at the gateway). Override the published list with
`models` if your account is provisioned differently — but note that a wrong id is
rejected by the *upstream*, not by us, so `models` only changes what we advertise.

`Models()` answers from a five-minute cache. The gateway calls it on every
`/v1/models` request and the panel calls it on every refresh; only an expired
cache reaches the vendor, and a failed refresh keeps the last good list.

---

## Configuration

The module reads the JSON object under `clients.zcode` in the host config. Every
key is optional; an absent or malformed object degrades to the defaults below
(a malformed object is logged and then ignored — it never fails the factory).

```jsonc
{
  "clients": {
    "zcode": {
      "accounts": [ /* see below */ ],
      "upstream_base": "",
      "oauth_provider": "zai",
      "auto_discover": true,
      "models": [
        "GLM-5.3", "GLM-5.3-Flash", "GLM-5.3-FlashX", "GLM-5.2",
        "GLM-5.1", "GLM-5", "GLM-5-Turbo", "GLM-4.7",
        "GLM-4.6", "GLM-4.5", "GLM-4.5-Air"
      ],
      "max_tokens_default": 32768,
      "max_account_attempts": 5,
      "cooldown_seconds": 300,
      "timeout_seconds": 600,
      "inject_system_blocks": true,
      "inject_cache_control": true,
      "captcha_command": "",
      "captcha_args": [],
      "captcha_region": "",
      "captcha_browser": true,
      "captcha_browser_path": "",
      "identity": { /* see below */ }
    }
  }
}
```

### Top-level keys

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `accounts` | array | `[]` | Explicit credentials, tried before discovered ones. |
| `upstream_base` | string | `""` | **Explicit, off-by-default** override: send *every* request to `<upstream_base>/v1/messages` with **no credentials at all**. Intended for proving the translation layer against a locally running Anthropic-wire gateway. Never used as a silent fallback. |
| `oauth_provider` | string | `zai` | Default sign-in realm for the panel's add-account flow: `zai` (international, chat.z.ai) or `bigmodel` (mainland, bigmodel.cn). Sets the realm the picker starts on and what a bare `StartLogin` uses; see [Panel sign-in realms](#panel-sign-in-realms). |
| `auto_discover` | bool | `true` | Read credentials already on the machine (see *Credential discovery*). |
| `models` | array | the 11 GLM ids in [Model ids](#model-ids) | Model ids advertised by `Models()`. The built-in list is used when the upstream catalogue cannot be read. |
| `max_tokens_default` | int | `32768` | `max_tokens` when the caller sends none and the catalogue has no published budget. The known plan models are filled from the vendor's docs instead (see `builtinModelSpecs`). |
| `max_account_attempts` | int | `5` | Upper bound on upstream attempts per `Chat` call (one per account at most). |
| `cooldown_seconds` | int | `300` | How long an account is parked after `3012` risk control / `429`. |
| `timeout_seconds` | int | `600` | Per-attempt deadline. `0` uses the default; a **negative** value means "no extra deadline". |
| `inject_system_blocks` | bool | `true` | Prepend the official CLI/agent/environment system blocks. Required by the upstream; turn it off only to reproduce a failure. |
| `inject_cache_control` | bool | `true` | Mark the last message block with an ephemeral cache marker. |
| `captcha_command` | string | `""` | Optional executable that prints a `VERIFY_PARAM=<value>` line. Used after a context token and before the built-in browser; a headless fallback when no browser can run. |
| `captcha_args` | array | `[]` | Arguments for `captcha_command`; `{scene}`, `{region}`, `{prefix}` are substituted from the runtime region document. |
| `captcha_region` | string | `""` | Force the Aliyun region instead of reading it from the endpoint. Does not suppress the scene read — see [Captcha paths](#captcha-paths). |
| `captcha_browser` | bool | `true` | Mint the Aliyun parameter with the machine's Edge or Chrome in a throwaway off-screen window. Requires a desktop session; set `false` on a headless server. |
| `captcha_browser_path` | string | `""` | Pin the browser executable when auto-detection picks the wrong one or finds none. Ignored when `captcha_browser` is false. |
| `identity` | object | see below | Header identity of the companion desktop client. |

### `accounts[]` entries

| Key | Type | Meaning |
| --- | --- | --- |
| `id` | string | Stable identifier used in `Status()` and for persisted state. Generated if omitted. |
| `label` | string | Human label for the panel. Falls back to `id`. |
| `provider` | string | `zai` or `bigmodel`. |
| `mode` | string | `api_key` (no captcha) or `jwt` (start-plan channel, captcha required). The JWT channel uses the built-in browser solver by default, so it is usable on an ordinary desktop install. |
| `api_key` | string | The 32-hex API key. Sent as `x-api-key`. |
| `jwt` | string | The start-plan JWT. Sent as `Authorization: Bearer`. |
| `base_url` | string | Endpoint family override; defaults per provider/mode (see below). |
| `enabled` | bool | `false` parks the account without deleting it. |

Default endpoint families:

| provider | mode | base URL |
| --- | --- | --- |
| any | `jwt` | `https://zcode.z.ai/api/v1/zcode-plan/anthropic` |
| `zai` | `api_key` | `https://api.z.ai/api/anthropic` |
| `bigmodel` | `api_key` | `https://open.bigmodel.cn/api/anthropic` |

### `identity` object

Mirrors the headers the official desktop client sends. Values must be printable
ASCII; anything else is dropped rather than sent.

| Key | Default | Header |
| --- | --- | --- |
| `app_version` | `3.14.4` | `X-ZCode-App-Version` |
| `agent` | `glm` | `X-ZCode-Agent` |
| `platform` | `win32-x64` | `X-Platform` |
| `os_category` | derived from `platform` | `X-Os-Category` |
| `os_version` | `10.0.22631` | `X-Os-Version` |
| `release_channel` | `stable` | `X-Release-Channel` |
| `language` | `zh-CN` | `X-Client-Language` |
| `timezone` | `Asia/Shanghai` | `X-Client-Timezone` |
| `title` | `Z Code@electron` | `X-Title` |
| `referer` | `https://zcode.z.ai/` | `HTTP-Referer` |
| `device_mid` | generated once, persisted | `X-Device-Mid` |

`User-Agent` is `ZCode/<app_version>`.

`app_version` is a baseline, not a pin.  The vendor gates the daily Start
Plan promotion on the reported build (3.14.4 returns a claimable plan where
3.11.2-3.13 return an empty list), so whenever a plan preview or balance
read comes back empty the module reads the official update manifest
(`/api/v1/releases/electron/manifest`) and, if it advertises a newer build,
adopts it for `X-ZCode-App-Version`, `User-Agent` and every `app_version`
query parameter.  The read is cached for an hour and a failure falls back to
the configured value.  A configured version that is *newer* than the
manifest's is kept as-is, so an explicit override can only move forward.

`os_category` is derived with the same rule the desktop client uses:
`win*`/`windows*` → `windows`, `darwin*`/`mac*` → `macos`, otherwise `linux`.

### Transport fingerprint

Two keys replace the TLS ClientHello, so vendor-facing calls stop carrying Go's
handshake.  Both are off by default, and an unset `tls_profile` keeps whatever
client the core supplied, byte for byte:

| key | values | default | meaning |
| --- | --- | --- | --- |
| `tls_profile` | a name from `fingerprint.Profiles()` | `""` | imitate that client's ClientHello (`chrome`, `node`, `firefox`, …) |
| `tls_protocol` | `"h2"`, `"http/1.1"`, `""` | `""` | pin the ALPN; empty offers the profile's own list |

A named profile replaces `hc` in `New` *before* it is handed to the account pool,
so both `Client.http` and `pool` go through it and no credential path escapes.

Only the TLS half is closed: the h2 frames are still written by
`golang.org/x/net/http2`, so an h2 connection remains a Go-shaped h2
connection.  A proxy in `Deps.Proxy` forces HTTP/1.1 whatever you set, because
`x/net/http2` cannot tunnel through one.

This module ships **no** default profile of its own: an unset `tls_profile` keeps
whatever client the core supplied, byte for byte.  A capture settles which name to
use when you do turn it on.  The ZCode desktop application was run behind a blind
TCP passthrough that splits the TLS record out of the stream without terminating
it, and its connection to `zcode.z.ai` came back as BoringSSL's: GREASE in the
cipher list and at both ends of the extension list, ECH (`65037`), ALPS
(`17613`), X25519MLKEM768 (`4588`), and `h2,http/1.1` in ALPN.

Once GREASE is normalised away, its **cipher list, `supported_groups` and set of
extension types are exactly `chrome`'s** — the three lists JA3 shares with the
preset.  Only the extension *order* differs, and Chrome randomises that order on
every connection, so **no fixed spec can reproduce this capture's JA3 digest** and
neither can the real client reproduce it on its next connection.
`TestTheChromeProfileMatchesTheCapturedZCodeHello` asserts the three lists agree
and will fail if a future uTLS preset drifts out.  `configs/client2api.json`
therefore ships `tls_profile: "chrome"` with `tls_protocol: "http/1.1"`: narrowing
the ALPN list moves no field JA3 is computed from, and extension 16 is one entry
either way.

So this is an approximation and not an impersonation, and the README says so
rather than implying the transport is clean.  See the package comment in
`internal/fingerprint` for how the hand-built profiles (`node`, `trae`) were
derived from their captures.

**Login is deliberately exempt.**  `weblogin.go`'s OAuth walk is the panel's own
first-party flow, not a request the desktop client makes, and
`weblogin_test.go` asserts it must not grow desktop identity headers or
override the UA.  A `tls_profile` still applies to it, because the transport is
shared — see `## Known gaps` for why that is recorded rather than hidden.

---

## Credential discovery

With `auto_discover` on (the default), the module reads credentials the ZCode
desktop client already wrote to the current user's profile. It never writes to
those files.

| Source | What is read | Becomes |
| --- | --- | --- |
| `~/.zcode/v2/config.json` → `provider` map | entries whose `kind` is `anthropic` and whose `options.apiKey` is a usable literal | one account each, `id` = `zcode-config:<provider name>` |
| `~/.zcode/v2/credentials.json` → `zcodejwttoken` | the start-plan JWT | one account, `id` = `zcode-credentials:zcodejwttoken`, provider `zai`, mode `jwt` |

Rules that matter:

* Values prefixed `enc:v1:` are **skipped** — they are encrypted with a
  machine-bound key we do not hold, so they cannot be used.
* A value containing exactly two dots and a base64url payload is treated as a JWT
  (mode `jwt`); anything else is treated as an API key.
* Entries with a non-`anthropic` `kind` are ignored.
* `oauth:*` values in `credentials.json` are **not** turned into accounts: they
  are OAuth access tokens for the key-issuance flow, not chat credentials.
* Discovered accounts are de-duplicated **by id and by secret**, because the
  desktop config stores the same API key under more than one provider name.
* The JWT's user id is read from the token payload (`user_id`, else `sub`) and
  injected as `metadata.user_id`.

Explicit `accounts` are always tried first; discovered ones fill in behind them.

---

## Status

`Status()` is cheap by construction: it makes **no network calls** and takes a
mutex, so it is safe for the panel's 10-second refresh.

* `Ready` — true when the `upstream_base` override is set, or at least one
  account is selectable right now.
* `Detail` — one line: credential and account counts (credentials sharing a
  vendor identity are one account), a split by mode, `usable=`, plus
  `cooling=` / `exhausted=` / `invalid=` when non-zero, a note when the JWT
  channel is disabled for lack of a solver, and the last upstream error.
  With no accounts at all it reads
  `no zcode credentials found (looked in config, ~/.zcode/v2/config.json, ~/.zcode/v2/credentials.json)`.
* `Accounts[]` — `ID`, `Label`, `Enabled`, `State`
  (`ready` / `cooling` / `exhausted` / `invalid` / `unknown`),
  `ExpiresAt` (RFC 3339, empty when unknown), `Note`, and
  `Extra{provider, mode, source}`. A cooldown whose deadline has passed reads as
  `ready`. A JWT account with no solver configured carries the note
  `JWT channel needs a captcha solver (no captcha_command and no Edge/Chrome found)`.
* `Models[]` — the bare ids from `models`.

**Secrets never appear in `Status()`.** Accounts carry their credential in an
unexported field, the persisted state file contains no secret at all, and the
only place a secret could surface is an upstream error body — which is passed
through `redact()`, i.e. replaced with `core.MaskSecret(secret)`.

---

## Panel account management

The module implements nine of the optional panel capabilities:

| Capability | Implemented | Notes |
| --- | --- | --- |
| `core.AccountManager` | **yes** | list, add, remove, enable/disable, test, refresh |
| `core.CredentialImporter` | **yes** | read-only discovery of the desktop client's stores, then an explicit import |
| `core.LoginProvider` | **yes** | browser hand-off OAuth; see `weblogin.go` |
| `core.RealmLoginProvider` | **yes** | two sign-in realms, `zai` and `bigmodel`; see [Panel sign-in realms](#panel-sign-in-realms) |
| `core.ModelRefresher` | **yes** | live catalogue from the vendor's config endpoint |
| `core.CheckinProvider` | **yes** | one action, `claim` — claims a promotional plan; `Channels: ["jwt"]` keeps the panel from offering it on an account's coding-plan API key row |
| `core.BalanceProvider` | **yes** | per-entitlement token balances |
| `core.PackageProvider` | **yes** | the same buckets as package rows |
| `core.TaskProvider` | **yes** | the task board's `claim` row; see [Tasks and scheduling](#tasks-and-scheduling) |
| `core.BatchPlanner` | **yes** | one batch, `checkin`, so the claim can run on the scheduler's timetable |
| `core.CaptchaProvider` | **yes** | reports the Aliyun scene the panel runs; see [Captcha paths](#captcha-paths) |

### Panel sign-in realms

Zhipu runs two separate services and a credential from one cannot sign in on
the other, so the panel's add-account flow asks which one to use. The module
advertises both through `core.RealmLoginProvider`; the operator's pick is sent
to `/oauth/cli/init` as `provider`, which is what makes the returned
`authorize_url` point at the matching login page.

| Realm | Service | Sign-in page | What the poll returns |
| --- | --- | --- | --- |
| `zai` (international, default) | Z.AI / chat.z.ai | `chat.z.ai/api/oauth/authorize` | an OAuth access token, which the module walks through `api.z.ai` to mint a coding-plan API key; the plan JWT is kept as a fallback |
| `bigmodel` (mainland) | BigModel / bigmodel.cn | `bigmodel.cn/login?appId=zcode` | the plan JWT the vendor hands back directly; there is **no** `api.z.ai` business-token walk |

A mainland phone number entered on the international page passes its human
check and then fails with the vendor's "请求失败", and vice versa, so the realm
has to match the account: pick **国内版** for a `bigmodel.cn` (mainland) account
and **国际版** for a `chat.z.ai` account.

`oauth_provider` sets the realm the picker starts on (and what a bare
`StartLogin` uses); it does not stop the operator from picking the other one.
An unrecognised configured value falls back to `zai` so a typo cannot make
sign-in impossible, but a realm the panel picked that the module does not know
is refused rather than silently mapped onto the wrong service — that would
store a credential the operator cannot use.

The realm also becomes the new account's `region`, and therefore its provider
and default base URL, so a mainland sign-in is filed as `bigmodel` rather than
as an international account. Sessions started before the picker existed carry
no realm and keep using the international walk, which is the only path that
existed then.

### Field schema

`AccountFields` returns a fixed six-field schema. `kind` decides which credential
field is required; the other credential field is ignored.

| Key | Type | Required | Default | Notes |
| --- | --- | --- | --- | --- |
| `kind` | `select` | yes | `api-key` | `api-key` or `jwt` |
| `api_key` | `password` | when `kind=api-key` | — | max 512 bytes, printable ASCII, no whitespace |
| `jwt` | `textarea` | when `kind=jwt` | — | max 8192 bytes, printable ASCII, no whitespace |
| `label` | `text` | no | `<region> <kind>` | control characters stripped, truncated to 120 runes |
| `region` | `select` | no | `zai` | `zai` or `bigmodel`; picks the default base URL |
| `base_url` | `text` | no | per region/kind | must be `http(s)` with a host; overrides the default |

`region` maps to the same base URLs the config file uses: `zai` →
`https://api.z.ai/api/anthropic` (or the plan endpoint for `kind=jwt`), `bigmodel`
→ `https://open.bigmodel.cn/api/anthropic`.

Validation failures always **name the offending field** and never echo the
value. A JWT submitted as an API key is rejected with a pointer at `kind="jwt"`.

### Where accounts live

| File | Contents |
| --- | --- |
| `<data_dir>/managed_accounts.json` | accounts added through the panel, **credentials in clear text**, mode `0600`. Written only here; the main config file is never touched. |
| `<data_dir>/accounts.json` | runtime state only — no secret at all (unchanged) — plus a `removed` array of ids the operator deleted, so discovery cannot resurrect them. |
| `~/.zcode/v2/*` | the desktop client's own files. **Read only, never written.** |

Account ids are stable:

* hand-added accounts get `zcode-managed:<12 hex>`, or the `id` you supply;
* imported credentials keep their source id (`zcode-config:<provider>`,
  `zcode-credentials:zcodejwttoken`).

`AccountRecord.Fields` carries only non-secret metadata: `kind`, `provider`,
`source`, `managed`, and a `fingerprint` — the first 4 bytes of the SHA-256 of
the credential, so two keys can be told apart without either being revealed. No
secret and no prefix of one is ever published.

`AccountRecord.Identity` is the one thing that is published deliberately: the
Zhipu user id the credential belongs to. It is the vendor's own non-secret
account number, read out of the token payload (`user_id`, else `sub`) or, for an
API key, out of the key name in the desktop client's credential store — an API
key cannot be asked who it belongs to, so that name is the only place the answer
exists. Its purpose is that **one account reached through several channels is
drawn as one account**: `~/.zcode/v2/config.json`'s provider API key, the
`credentials.json` plan JWT, and a panel sign-in can all be the same Zhipu user,
and the panel groups the rows that share an identity. A credential whose account
cannot be determined reports no identity and stays its own row rather than being
merged on a guess.

### Actions

| Action | Behaviour |
| --- | --- |
| `GET .../accounts` | every account from the config, the desktop client, and `managed_accounts.json`, de-duplicated by id and by secret. An empty pool is an empty list, never an error. |
| `POST .../accounts` | validates, rejects a credential that is already configured (naming the account that holds it), appends to the live pool and to `managed_accounts.json`. |
| `DELETE .../accounts/<id>` | a managed account is dropped from the store; an account that came from the config or from discovery is tombstoned in `accounts.json` instead, so it stays deleted across restarts. Unknown id → error. |
| `POST .../accounts/<id>/enabled` | updates the live pool and both files; a disabled account is no longer selectable. |
| `POST .../accounts/<id>/test` | one minimal request (`max_tokens=16`) against the first id in `models` (default `GLM-5.3`). |
| `POST .../accounts/refresh` | with no `id`, refreshes every account. |
| `GET .../discover` | `~/.zcode/v2/config.json#provider.<name>`, `~/.zcode/v2/credentials.json#zcodejwttoken`, and the `%APPDATA%\ZCode` profile directory. |
| `POST .../import` | `paths` (or `all: true`); a bare file path imports every credential in that file, a `#fragment` path imports one. Already-present credentials are skipped, not errors. |
| `POST .../accounts/<id>/checkin` | the single `claim` action: preview the claimable plans, then claim the highest-priority one. Needs a `jwt` account and a captcha token — from the built-in browser solver, the panel's browser, or `captcha_command`; see [Captcha paths](#captcha-paths). The scheduler's `checkin` batch calls the same action — see [Tasks and scheduling](#tasks-and-scheduling). |
| `POST .../accounts/<id>/balance` | token balances per entitlement, from `zcode-plan/billing/balance`. A vendor refusal is a real error (the panel answers 502), because a balance with no number is worse than no balance. |
| `GET .../packages` | the same buckets as package rows, named `<plan> · <entitlement>` and sorted by remaining tokens. |

### Testing and refreshing an account

`TestAccount` reports an upstream refusal as a **result**, not as a Go error —
this is the point of the action:

| Outcome | `TestResult` |
| --- | --- |
| a normal completion | `OK: true`, `Model`, `Reply` (truncated to 400 characters), `ElapsedMS` |
| `401` / expired key (e.g. `令牌已过期或验证不正确`) | `OK: false`, `Error` carrying the classified upstream error |
| `429`, `402`, risk-control, concurrency, captcha | `OK: false`, `Error` |
| `kind=jwt` with no local solver (`captcha_browser` off/failed and no `captcha_command`) | `OK: false`, and **no request is sent** — the channel is disabled, not broken |
| unknown or empty `id` | a real Go error; the network is never touched |

A probe is a genuine observation, so it moves the account's state like any other
request: a `401` leaves it `invalid`, a `429` leaves it `cooling`. Both then read
that way in `Status()` and in the panel list.

`RefreshAccount` is honest about what this module can and cannot do:

* `api-key` → `OK: false`, `api-key credentials cannot be renewed here: rotate
  the key in the vendor console and add the new one`. There is no renewal
  concept for an API key, and pretending otherwise would be a lie.
* `jwt` → re-reads the desktop client's store and adopts the token **only if it
  actually changed**. The renewal itself happens in the ZCode desktop client;
  this module can only pick up the result. If the token is unchanged it says so
  rather than reporting a success it did not achieve.

### Error mapping

| Situation | Result |
| --- | --- |
| invalid form (kind, credential, region, `base_url`, id) | Go error naming the field, HTTP 400 from the panel |
| credential already configured | Go error naming the existing account id |
| unknown account id (remove, enable, test, refresh) | Go error |
| upstream refusal during a test | `TestResult{OK: false, Error: …}`, no Go error |
| nothing importable at an explicit path | Go error naming the path |
| `paths` empty and `all` false | empty list, no error (the panel rejects this first) |

### Not implemented

* **A standalone solver binary.** The module now ships a browser-driven
  solver, but that still needs Edge or Chrome and a desktop session — there is
  no pure-Go/Node-free signed binary to point `captcha_command` at. A headless
  server needs either an installed Chromium-family browser it can run or its
  own `captcha_command`.
* **Writing to the desktop client's stores.** Discovery and import are read-only
  by design; `managed_accounts.json` is the only thing this module writes.
* **Quota in `RefreshAccount`.** That renews a credential, not a usage figure;
  the balance figures come from `AccountBalance`/`AccountPackages` on demand.
* **Importing from `%APPDATA%\ZCode`.** That directory is an Electron profile
  (`session/Local Storage/leveldb`, `Cache`, `IndexedDB`) and holds no plaintext
  credential this module can read. It is reported by `Discover` with
  `Importable: false` and a note explaining why, rather than being hidden.

---

## Tasks and scheduling

`tasks.go` gives the claim a second way in: the panel's **任务** tab and the
scheduler's timetable, both of which go through the same `Checkin` the manual
button calls. The action itself is unchanged — only its trigger is.

| Surface | Row |
| --- | --- |
| `GET .../tasks` | one row, code `claim`, group `活动套餐`, describing the plan a claim would take right now (name, its token grants, and how many other plans are waiting). If the preview is empty because the plan was already claimed, the balance document turns the row into `Claimed` / `活动套餐已领取` instead of reporting that nothing exists. |
| `POST .../tasks/claim/run` | the same claim; `RunTask` clears the preview cache afterwards, because a claim invalidates it |
| `Batches()` | one batch, `checkin`, whose single code is `claim` |

`Batches()` is declared even with no solver configured, so the refusal is
*discoverable* rather than invisible: the batch appears on the timetable and
every run reports one refusal explaining what is missing. A batch that is never
declared cannot be diagnosed.

The task board is deliberately read-only about *whether* a claim will succeed.
Three states produce one locked row each, naming the fix, and none of them sends
a request:

| Situation | The row says |
| --- | --- |
| no `jwt` account at all | 没有可用的 ZCode 计划 (jwt) 账号；活动套餐只走 jwt 通道 |
| a `jwt` account but no local solver (built-in browser unavailable and no `captcha_command`) | 这个看板的领取按钮没有浏览器可用；请在账号列表里点「领取」，那一步会用你自己的浏览器过验证码 |
| the preview is empty and the balance has an active plan | `活动套餐已领取` with the active plan name and remaining token grants; the panel renders this as 已领取 / 已完成 and offers no second claim button |
| the preview is empty and the balance is empty too | 厂商当前没有可领取的活动套餐 |
| the preview is empty and the balance read fails | 厂商当前没有可领取的活动套餐（已领取计划暂无法确认） |

`Tasks` caches its preview, and the active-plan balance read that follows an
empty preview, for ten seconds (`claimBoardTTL`), because the panel re-reads the
board every three seconds while a run is live and both are billing endpoints. A
*failed* preview is never cached.

Two deliberate omissions in the result mapping:

* `TaskResult.Code` is the **task** code (`claim`), not the vendor's numeric
  business code — `core.TaskResult.Code` is a string, so the vendor's code
  survives in `Message` as its human text instead.
* `Credit` stays zero. A plan grants tokens and the board's reward column is
  denominated in 积分; reporting a token count there would be a unit error.

**Cadence.** The batch runs on the scheduler's `checkin` group — `checkin_hours`
in the config, `[9, 21]` by default — not on the reference's ten-minute
frontend timer. A refusal is a `TaskResult{OK: false}`, which the scheduler
counts as *refused*, not *failed*, so an unattended run cannot manufacture an
outage out of a missing local solver.

The daily promotion is a limited pool, so the shipped config gives zcode a
denser timetable of its own — `schedule.clients.zcode.checkin.hours`,
`[0, 9, 12, 18, 21]` — instead of the shared `[9, 21]`. A per-client entry
wins over the shared group for that one client, and the hour `0` puts a run at
the CST day boundary, which is when a daily quota resets. This is cheap to run
often: `claimPreview` is a tokenless GET and the Aliyun captcha is only minted
once a run actually has a plan to claim, so an empty sweep costs four small
requests and no browser. Edit the hours in the panel's 任务中心 time table (or
`schedule.clients.zcode.checkin.hours` in the config) to taste.

**Liveness reporting.** The claim endpoints are the one place the vendor decides
from activity whether to offer a plan at all, so every `claimPreview` first POSTs
`/api/v1/event/report` twice — `app_launch` and `app_daily_active` — with
`{event, device_mid, platform:"win32", app_version}` and **no** credential.
Measured against the vendor: without the pair `preview` answers
`{"code":0,"data":{"plans":[]}}`; with it a plan appears. The server dedupes by
`device_mid` + date, so repeating it is cheap and covers the case where the day's
first preview arrives before the events have landed.

A failed report is deliberately **not** fatal: it is logged (`zcode: <event>
event report failed: …`) and the preview still runs, because a liveness signal is
best-effort and an empty preview is a fact rather than an error. `platform` here
is the literal `win32`, not the configured `identity.platform` — the three
endpoints disagree about what they accept (see *Configuration*), so each sends
the value measured for it.

`planDo` therefore status-checks when the caller passed no `out`: only this route
uses that form, and without the check a rejected report would be
indistinguishable from an accepted one.

---

## Captcha paths

There are three sources for the Aliyun parameter, tried in this order:

1. A token attached to the request context by the panel. The operator just
   watched it succeed in their own browser, so it wins.
2. A cached parameter minted by the built-in browser solver. One parameter
   is reused for 45 seconds; a vendor `3007` drops it immediately and the same
   account is retried once.
3. `captcha_command`, then the built-in browser solver. The command is the
   headless escape hatch; the browser solver is the no-setup default.

### Built-in browser solver

`captcha_browser.go` starts the machine's Edge or Chrome in a throwaway
profile, loads a local page that runs Aliyun's own SDK, waits for
`startTracelessVerification()`, and closes the browser tree. The window is
real and positioned off-screen: headless Chromium and a jsdom shim were both
refused with `F001 verifyResult:false` against the live scene, while a normal
windowed browser passed in about two seconds.

The solver is on by default. It resolves to unavailable when
`captcha_browser` is false or no Edge/Chrome is installed, and the JWT
channel then reports `ErrNotConfigured` instead of sending a doomed request.
Use `captcha_browser_path` to pin a non-standard browser executable.

### Panel browser fallback

The claim endpoint wants a token in `X-Aliyun-Captcha-Verify-Param` that only
Aliyun's own JavaScript can mint, against the caller's real browser fingerprint.
The built-in solver does that in the background; the panel is also a browser,
so it can run the same SDK when the operator needs an interactive fallback.

The built-in solver above is the default; the panel path below is the
interactive fallback.

The panel flow is split across two packages:

| Step | Where | What happens |
| --- | --- | --- |
| 1 | `CaptchaScene` (`captcha.go`) | the module reports `{required, enabled, scene_id, region, prefix}` from the vendor's `client/configs` document |
| 2 | `GET /panel/api/clients/zcode/captcha?action=claim` | the panel asks for that scene, only when the operator clicks 领取 |
| 3 | `/panel/captcha` (`internal/panel/captcha.html`) | a standalone document frames the vendor's SDK, runs `startTracelessVerification()`, and posts `{type:"c2a-captcha", ok, param, region}` back to the shell |
| 4 | `POST .../accounts/<id>/checkin` | the shell re-sends the click with `captcha_param` / `captcha_region`, which the panel turns into `core.WithCaptchaSolution(ctx, …)` |
| 5 | `solveCaptcha` (`zcode.go`) | the context token wins over the cache, `captcha_command` and the built-in browser; the claim goes out carrying it |

Three properties worth knowing:

* **`Required` is keyed on the scene id and on there being no local solver.**
  No scene id means the widget cannot start, so saying "required" would only
  produce a dead popup; a local solver (the built-in browser or
  `captcha_command`) means the server already mints tokens unattended,
  including from the scheduler, so popping a second browser window at the
  operator would be a regression. The context token is still preferred if one
  ever arrives.
* **The vendor's `enabled` flag is reported, not obeyed.** It says whether the
  vendor currently thinks a captcha is needed; it is not a promise that the
  claim endpoint will accept a tokenless request, and getting that wrong in the
  permissive direction is a silent failure at claim time.
* **The region override does not suppress the scene read.** `captcha_region`
  exists so a deployment can pin the region without talking to the endpoint,
  which is what the solver path wants; but a scene id can only come from the
  endpoint, so `sceneFor` fetches regardless. `regionFor` (the solver path)
  keeps the no-network guarantee, `sceneFor` (the browser path) does not.

The scheduler cannot use the panel path: an unattended run has no operator to
click through a popup. It can still claim through the built-in browser solver
or `captcha_command`; with neither, its `claim` reports `ErrNotConfigured` —
a refusal, not a failure.

---

## Behaviour

### Retry and account state

`Chat` walks the pool, at most `max_account_attempts` times, never retrying the
same account inside one call:

| Upstream signal | Classification | Action |
| --- | --- | --- |
| envelope `3006` | model rejected | **return `core.ErrUnsupported` immediately** (not account-specific) |
| envelope `3007`, or `403` whose body mentions captcha/verify | captcha | refresh the verify param and retry |
| envelope `3009` | concurrency slots full | cool the account for 15 s, try the next |
| envelope `3012`, or `405` | risk control | cool the account for `cooldown_seconds`, try the next |
| `402`, or an exhaust keyword (`quota`, `insufficient`, `balance`, `exhaust`, `额度`, `余额不足`) | quota exhausted | mark `exhausted`, try the next |
| `401` / `403` without captcha wording | bad credential | mark `invalid`, try the next |
| `429` | rate limited | cool the account, try the next |
| anything else | — | try the next |

Order matters: the envelope codes are checked first, `402` before the keyword
scan, and `429` **before** the keyword scan — so a `429` whose body happens to
say "quota exceeded" stays a rate limit instead of wrongly exhausting the account.

An `exhausted` mark carries no cooldown of its own — a plan's quota does not
come back on a timer — so it is cleared by evidence instead of by the clock: a
successful plan claim, the vendor's `1003 already claimed` (which says the
account already holds the plan), and a funded balance read through the panel all
drop the verdict and put the account straight back into rotation. Only `invalid`
stays parked, because a dead credential needs a real fix rather than a retry.

When no account is usable, `Chat` returns `core.ErrNotConfigured` (HTTP 503 at
the gateway) with a detail line explaining why.

Note that `3006` and `3009` arrive as **HTTP 200** with a non-zero envelope code,
so a 200 body is inspected too; only a healthy payload is passed on.

### Streaming vs. non-streaming

`Chat` honours `req.Stream` and always returns a `core.Stream`:

* `Stream: true` — the upstream is asked to stream; its Anthropic SSE frames are
  decoded into events as they arrive.
* `Stream: false` — the upstream returns one JSON document, which is decoded into
  the same event sequence (text, tool calls, usage, done) so the gateway's
  aggregator sees a uniform shape.

Emitted at most once each: `EventUsage` and `EventDone`. `Recv` reports `io.EOF`
once the stream is finished and keeps reporting it afterwards; `Close` is
idempotent and always closes the upstream body and cancels the request context.

Finish reasons map as `end_turn`/`stop_sequence`/`pause_turn` → `stop`,
`max_tokens` → `length`, `tool_use` → `tool_calls`, `refusal` → `content_filter`.

Usage maps as
`prompt_tokens = input_tokens + cache_read_input_tokens + cache_creation_input_tokens`,
`cached_tokens = cache_read_input_tokens`, `completion_tokens = output_tokens`.

### Headers

Every request carries `Content-Type`, `Accept`, `Anthropic-Version: 2023-06-01`,
`User-Agent`, and the identity headers above.

* **`api_key` mode** adds `x-api-key`. No trace headers, no captcha headers.
* **`jwt` mode** adds `Authorization: Bearer <jwt>` and **only** `x-request-id`,
  `x-zcode-session-type: main`, `x-zcode-trace-id`. `x-query-id` and `x-session-id`
  are deliberately *not* sent — the reference reports they trip risk control
  (`3012`) on the start-plan channel. A captcha verify param is attached as
  `X-Aliyun-Captcha-Verify-Param` + `X-Aliyun-Captcha-Verify-Region`.

The captcha **region is read at runtime** from
`https://zcode.z.ai/api/v1/client/configs?app_version=<ver>&platform=<platform>`
(`data.configs.captcha.{region,sceneId,prefix}`), cached for 10 minutes, and
never hardcoded. Both query parameters echo the configured `identity` block, the
same values the chat and claim requests send. The vendor validates them
strictly: `platform=win32` is answered with
`HTTP 400 {"code":3001,"msg":"parameter error"}`, which silently costs the
module its scene id and its region — the identity default is `win32-x64`.
`captcha_region` overrides the region; a failed lookup is not fatal. The scene
id and prefix always come from that endpoint, which is why the browser path
reads it even when the region is pinned — see
[The browser captcha path](#the-browser-captcha-path).

### System blocks

The upstream rejects a request that lacks the official CLI-prefix /
agent-identity / environment system blocks (it answers `3012`), so they are
injected ahead of the caller's own system text, followed by
`- You are powered by the model named <model>.` The *content* of those blocks was
transcribed into `system.go`; the reference's `zcode_system.json` file itself was
not copied.

---

## Known gaps

* **The JWT ("start-plan") channel needs a real browser.** Every request on it
  needs a fresh Aliyun traceless-verification parameter. The built-in solver
  mints it with the machine's Edge or Chrome and is on by default, so a normal
  desktop install can use the channel out of the box. A headless server, or a
  machine without a Chromium-family browser, is reported as unavailable rather
  than silently attempted; `captcha_command` is the escape hatch. See
  [Captcha paths](#captcha-paths).
* **Captcha solving is fragile by nature.** The reference notes roughly a 2/3
  success rate and breakage on any Aliyun fingerprint change; a Node + jsdom
  solver is required upstream. This module deliberately does not depend on
  Node. The browser-driven path mints the token with the vendor's own SDK in a
  real browser, on the operator's own network. Aliyun's FAQ (Q13) still warns
  that traceless verification can be intercepted on a bare page, in which case
  the widget falls back to an interactive click — the panel dialog supports
  that, but the built-in off-screen window does not.
* **Risk-control (`3012`) accumulates.** A block is treated as an account-level
  cooldown, but the underlying risk score may persist server-side; the reference
  recommends a throwaway account when experimenting.
* **`3009` concurrency slots** are a shared, server-side budget. This module backs
  off and rotates, but cannot see how many slots the desktop app is holding.
* **A `tls_profile` also applies to the sign-in flow.** `weblogin_test.go`'s
  `assertNoDisguiseHeaders` (`weblogin_test.go:158-174`) pins the *headers* of the
  OAuth walk — no `X-Device-Mid`, no desktop `User-Agent` — but the transport is
  the module's, so a named profile is used there too. That is a deliberate
  trade-off rather than an oversight: the sign-in flow talks to the same
  `zcode.z.ai` origin as everything else, and shipping a second un-fingerprinted
  client purely for it would be one more place for a request to leak a Go hello.
  Recorded here because the two facts pull in opposite directions.
* **The JWT has no `exp` claim.** Expiry is decided server-side and announced
  only as a failed request, so `ExpiresAt` stays empty on the account row;
  `AccountBalance`/`AccountPackages` are what report the real numbers.
* **Claiming is captcha-gated before anything else.** A `claim` with a
  deliberately non-existent plan id still answers `3007 captcha verify failed`,
  which proves the vendor checks the Aliyun parameter *before* the plan, so
  there is no dry-run and no pure-HTTP path. `claim` therefore needs a real
  token: the built-in browser solver, the panel's browser, or `captcha_command`,
  exactly like the JWT chat channel. What needs no token is the *preview* — a plain GET that
  reports whether anything is claimable at all.
* **No signature handshake.** The upstream can advertise an Ed25519
  `codingPlanSignature` requirement via `agent/configs`; if it does, requests on
  that channel will be refused and this module will report the failure verbatim.
* **Thinking signatures are dropped.** Anthropic thinking deltas carry a
  `signature` that must be round-tripped for multi-turn reasoning replay;
  `core.Message` has nowhere to carry it, so `signature_delta` frames are ignored.
* **Remote images are dropped.** `image_url` parts pointing at an `http(s)` URL
  cannot be expressed as an Anthropic base64 source without fetching them; only
  `data:` URLs are forwarded. The part is skipped, not turned into an error.
* **The published model list is recon-time knowledge** and may drift; a stale id
  shows up as a `3006` → HTTP 400.

---

## The `upstream_base` stopgap

Set `upstream_base` to route every request at one Anthropic-wire endpoint with no
credentials — useful to prove the translation layer end to end against a locally
running gateway:

```json
{ "clients": { "zcode": { "upstream_base": "http://127.0.0.1:3000", "auto_discover": false } } }
```

This is explicit and off by default. It is **never** entered as a fallback when
real credentials fail; if it is unset, a credential problem surfaces as
`core.ErrNotConfigured`.

---

## Testing

```sh
go vet ./clients/zcode/...
go test ./clients/zcode/... -v
```

Everything runs **offline** and needs no Node: upstream payloads are inline
string fixtures and the transport is a fake `http.RoundTripper`; credentials are
written into a temporary home directory (`USERPROFILE`/`HOME` are redirected so
the developer's real `~/.zcode` cannot leak into a test). Nothing in the suite
requires `CLIENT2API_LIVE`.

---

## Provenance and licence

**Clean-room.** The reference implementations (`zcode2api` and its forks) are
licensed **AGPL-3.0**. No code, comment, identifier, or file structure from them
was copied into this module. They were read **only** to learn the wire protocol,
the header contract, the credential layout, and the upstream error codes; every
line of Go here was written from scratch against the `client2api` module
contract.

What was read, and what it was used for:

* the Anthropic Messages request/response shape and SSE frame names — protocol facts;
* the required CLI/agent/environment system blocks — the *text content* was
  transcribed into `system.go` (the reference's JSON asset file was not copied);
* the companion header set and the `x-query-id`/`x-session-id` risk-control
  warning — protocol facts;
* the credential file layout under `~/.zcode/v2/` — needed to discover accounts;
* the upstream error envelope and the `3006`/`3007`/`3009`/`3012` codes, plus the
  `402`/`429`/`401`/`403` classification rules — protocol facts, reimplemented.

Nothing was transcribed from the reference's Python, and no AGPL file is
vendored. The upstream endpoints, header names, and envelope codes are
interoperability facts, not creative expression.

### Security note

During recon, a live API key was observed in clear text in a credential file and
in session logs. **That key must be rotated.** It is not hardcoded here, not
committed here, and never logged: this module masks secrets with
`core.MaskSecret` before they can reach a log line.
