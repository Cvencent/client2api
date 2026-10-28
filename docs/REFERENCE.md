# client2api

[English](../README.md) | [简体中文](../README.zh-CN.md) | [使用教程](../TUTORIAL.zh-CN.md)

> Reference documentation for client2api.
> For the project introduction and feature overview, see [README.md](../README.md).

One OpenAI-compatible gateway in front of more than twenty AI backends:

| route | client | what it wraps |
|---|---|---|
| `workbuddy/…` | WorkBuddy | Tencent WorkBuddy / CodeBuddy (intl + cn realms) |
| `autoclaw/…` | AutoClaw | AutoClaw desktop (independent platform) |
| `trae/…` | Trae | TRAE SOLO / Trae CN |
| `zcode/…` | ZCode | Z.AI coding plan (Anthropic Messages upstream) |
| `kimi/…` | Kimi | Kimi Code CLI |
| `qoder/…` | Qoder CN | Qoder CN browser/desktop accounts, credits, check-in and chat |
| `qwenwork/…` | QwenWork | Qwen Office / QoderWork CN desktop agent |
| `tabbit/…` | Tabbit | Tabbit Browser (via a sidecar) |
| `minimaxcode/…` | MiniMax Code | MiniMax Code desktop agent (Anthropic Messages upstream) |
| `gemini/…` | Gemini | Google Cloud Code Assist / Antigravity free tier |
| `cline/…` | Cline | Cline (cline.bot) — WorkOS credential, OpenAI-shaped upstream |
| `lobsterai/…` | LobsterAI | Youdao LobsterAI — OpenAI-shaped, SSE only |
| `codearts/…` | CodeArts | Huawei CodeArts IDE model service (SDK-HMAC-SHA256) |
| `loomy/…` | Loomy | iFlytek Loomy — HMAC-SHA1 signed account endpoints |
| `raccoon/…` | Raccoon | SenseTime Raccoon Work |
| `openrouter/…` | OpenRouter | OpenRouter (openrouter.ai) — multi-vendor router whose ids contain a slash; free models only by default (`free_only`) |
| `opencode/…` | OpenCode Zen | OpenCode Zen (opencode.ai) — one OpenAI-shaped façade in front of Anthropic-, Google- and OpenAI-native models |
| `kilocode/…` | Kilo Code | Kilo AI Gateway (api.kilo.ai) — OpenRouter-shaped multi-vendor router; free models served anonymously, keys optional |
| `doubao/…` | Doubao | ByteDance Doubao desktop client / web app |
| `doubaoime/…` | Doubao IME | Doubao Input Method (豆包输入法) online speech-to-text — transcription only, not chat |
| `officeace/…` | OfficeAce / Huawei | Huawei MaaS / AgentArts model gateway; built in and displayed as **Huawei** |
| `openai-compat/…` | onmiRoute | Legacy compatible sources and the local OmniRoute bridge; existing `openai-compat/<provider>/<model>` routes remain valid |
| `<source-id>/…` | Custom relay | Hot-defined OpenAI-compatible platforms, each with its own API root, API-key pool, model scan and routing policies |

Everything is served from **one** HTTP surface — `POST /v1/chat/completions`,
`POST /v1/responses`, inbound Anthropic `POST /v1/messages` and
`POST /v1/messages/count_tokens`, `POST /v1/audio/transcriptions`,
`GET /v1/models`, `GET /v1/status`, `GET /healthz`, plus a web panel at `/panel/`.

## Speech transcription

`POST /v1/audio/transcriptions` accepts an OpenAI-style multipart upload
(`file` plus `model`, optional `language` and `response_format`) and returns
`{"text": "..."}`, plain text, SRT, verbose JSON, or VTT. A platform opts in by implementing `core.TranscriptionProvider`
(`TranscriptionModels` and `Transcribe`); the only such module today is
`doubaoime`. Transcription models are kept **out** of `Client.Models()` and the
chat routing catalogue, then listed by `GET /v1/models` with
`extra.kind = "transcription"` so a client can discover them without entering
the chat pool. The `doubaoime` module accepts WAV, FLAC, MP3, MP4/M4A, OGG and
WebM; WAV is decoded in-process and the rest are decoded by the bundled FFmpeg.
Errors map to 400 (bad upload or audio), 422 (no speech), 502
(upstream/protocol) and 504 (timeout).

## Anthropic Messages

`POST /v1/messages` and `POST /v1/messages/count_tokens` expose the Anthropic
Messages wire shape for Claude Code and Anthropic SDK clients. The ingress
translates string or block content, system prompts, tools, tool results,
thinking, usage, streaming SSE, non-streaming responses and Anthropic-shaped
errors into the same internal chat request used by the OpenAI-compatible
endpoints. `count_tokens` is a local estimate and does not call upstream.

Authentication accepts the gateway's inbound key either as `x-api-key` or as
`Authorization: Bearer ...`. Point the client's Anthropic base URL at the
gateway root, not at `/v1/messages`; the client appends the path itself.

## Custom Relay Platforms

In **Platform Configuration**, select **Add Relay**, set a unique platform ID,
display name and API root (for example `https://api.example.com/v1`), and
optionally provide the first API Key. Creation, editing, disabling and removal
take effect without restarting. Additional keys are added through that
platform's **Account Pool**. **Scan Models** queries authenticated `GET /models`
for each enabled key and saves the union catalogue. The account table shows
which models each key can use; failed scans retain the last successful result.
If the API root changes, scan again to populate its new catalogue.

```json
{
  "sources": {
    "my-relay": {
      "label": "My relay",
      "base_url": "https://api.example.com/v1",
      "max_tokens_field": "max_tokens",
      "proxy": "socks5://127.0.0.1:1080",
      "user_agent": "codex_cli_rs/0.0.1 (client2api)",
      "api_format": "responses",
      "disabled": false
    }
  },
  "platforms": {
    "my-relay": {
      "priority": -10,
      "max_in_flight": 4,
      "max_in_flight_per_account": 2
    }
  }
}
```

API roots must use HTTP(S); include the service's version prefix when needed.
`max_tokens_field` accepts `max_tokens` or `max_completion_tokens`. `proxy`
(optional) is an `http`, `https`, `socks5` or `socks5h` URL, and routes every
request this one relay makes — chat, model scan, balance and account tests — through
it; leave it out to keep using the gateway's own egress. `user_agent`
(optional) replaces the module's User-Agent on every request this relay makes —
some relays gate by client allowlist and refuse a generic UA with 401
"unauthorized client detected".
`api_format`
(optional) switches the chat wire: `responses` posts to `/v1/responses` for
relays whose models reject `/chat/completions` with "please use the Responses
API"; the default posts to `/chat/completions`. IDs use
lowercase ASCII letters, digits and hyphens, start with a letter, and cannot
reuse a built-in platform or reserved directory name. Display names can change
without changing routing IDs. Call `my-relay/<upstream-model>` using the
gateway's inbound Key, or use an unqualified model/model group with the existing
platform and account priorities. Upstream IDs containing `/` are preserved.

Keys and their scanned model lists live in `data/my-relay/accounts.json`, not
the source definition. Instance backup/restore includes both. Removing a source
retains its account file; re-adding the same ID and API root restores that pool.
Every source is conversation-sticky: a multi-turn conversation keeps landing on
the key that warmed its upstream prompt cache, so a relay that bills per cached
prefix does not re-charge the whole prompt when the pool rotates. The binding is
per source and per conversation, and it is ignored while the key is disabled,
cooling down or missing the requested model from its scanned catalogue.
OfficeAce (Office Claw) is built in as `officeace` and needs no `sources` entry.
Use **Browser sign-in** in its account pool: the panel completes the Huawei
account OAuth flow, stores the model-gateway App Key / App Secret in
`data/officeace/accounts.json`, and publishes the scanned models as
`officeace/<model>`. Each account keeps the `model_api_url_base` returned by its
own login. Requests use `Authorization: Basic base64(app_key:app_secret)`.
A fresh sign-in also opens the free personal-trial entitlement
`officeace.personal.package.trial`, which carries 500 `officeace_points` and
200 online searches; an account that already has the trial is only checked and
is not ordered again.
The cloud credential from the same login is stored alongside it, which is what
backs the WorkBuddy-style facilities: **Check-in** posts
`POST /v1/subscription/bonus/claim` (idempotent), **Balance** reads the
`officeace_points` position and the online-search meter, and **Packages** lists
the plan quota and bonus activities (new-user and daily-check-in bonuses appear
as separate activity rows). Expired temporary credentials are renewed from the
OAuth refresh token or the IAM v3/STS exchange.
Windows can still use **Import -> Discover local credentials** for an already
signed-in OfficeAce desktop client.

The existing `clients.openai-compat` configuration and data remain supported;
the panel displays that legacy module as **onmiRoute**.

Cline forwards declared tools when `tool_choice` is omitted or `null`, using
the upstream's default automatic selection. Only an explicit `"none"` suppresses
the tool definitions.

OpenCode keeps Console login tokens separate from workspace inference keys and
addresses. A logged-in workspace without Zen configuration remains saved but
is not ready for inference; the account refresh/test actions explain the missing
configuration. Overlapping OpenCode requests wait up to `queue_timeout` (default
`60s`) for local account capacity while respecting the configured concurrency
limits and caller cancellation. Upstream `server_error` responses, including
HTTP 429 endpoint outages, are classified as upstream failures.

[![build](https://github.com/Cvencent/client2api/actions/workflows/go-binaries.yml/badge.svg)](https://github.com/Cvencent/client2api/actions/workflows/go-binaries.yml)
[![release](https://img.shields.io/github/v/release/Cvencent/client2api?include_prereleases)](https://github.com/Cvencent/client2api/releases)

Current version: **0.4.5**. Full history: [CHANGELOG.md](../CHANGELOG.md).

### Request-count limits

A relay's request-count limits are added as form rows under
**Platform Configuration → Edit Relay → Request-count limits**. Each row is one
entry in the `sources.<id>.request_limits` array of `configs/client2api.json`,
so the JSON is never edited by hand:

```json
{
  "sources": {
    "my-relay": {
      "request_limits": [
        {"scope": "account", "limit": 60, "window": "1m", "reset": "rolling"},
        {"scope": "account", "limit": 100, "window": "24h", "reset": "calendar", "timezone": "Asia/Shanghai", "reset_at": "00:00"},
        {"scope": "source", "limit": 500, "window": "24h", "reset": "first"}
      ]
    }
  }
}
```

| Field | Meaning |
|---|---|
| `scope` | `account` counts each key on its own; `source` shares one counter across the whole relay |
| `limit` | Upstream requests allowed in the window; must be greater than 0 |
| `window` | The period as a Go duration (`30s`, `5m`, `2h`, `24h`), at least 1 second; the panel takes a number plus seconds/minutes/hours/days |
| `reset` | `rolling` (default), `calendar` or `first` |
| `timezone` | `calendar` only; an IANA zone such as `Asia/Shanghai`, defaulting to the process's local zone |
| `reset_at` | `calendar` only; `HH:MM`, default `00:00`; every day aligns to this clock time |

The three reset modes:

- `rolling` — an exact sliding window. A request frees its slot one `window`
  after it happened, so with 24 hours / 100 requests the 101st waits until the
  earliest request is 24 hours old.
- `calendar` — a fixed natural period aligned by `timezone` and `reset_at`.
  `window` must divide 24 hours and cannot exceed it (`1h`, `30m`, `24h`), so
  natural weeks or months are not supported; use `rolling` or `first` with a
  long window instead. Daylight-saving transitions follow Go's zone rules.
- `first` — a fixed window that starts at the first charged request and clears
  after one full `window`; this is what the legacy `requests_per_minute*` fields
  did.

Multiple rules apply together: before any upstream request is sent for a key,
every applicable rule must still have room, and one full rule blocks the send.
`account` rules constrain only the current key, so a full key rotates to the next
one; when every usable key is full, or a `source` rule is full, the gateway
answers busy (`core.ErrBusy`, HTTP 429). A request is charged before it leaves
for upstream: local validation failures, account-slot refusals and request-body
build failures do not count, while upstream 401, 402, 429, 5xx and network
errors all count as one.

Counters are stored per “rule signature + counted subject (a key, or the whole
relay)” in `data/<source>/request_limits.json`, so a restart does not reset the
quota. Changing only `limit` keeps the usage already charged and blocks at the
new ceiling immediately; changing `scope`, `window`, `reset`, `timezone` or
`reset_at` makes a different rule with fresh counters; deleting a key prunes its
counters. Two rules with the same `scope + window + reset + timezone + reset_at`
are rejected as duplicates. A missing counter file means no usage yet; a corrupt
one fails to load the relay rather than treating the usage as zero.

The legacy `requests_per_minute` (whole relay) and
`requests_per_minute_per_account` (per key) fields are still read and behave like
`source` / `account` rules with a `1m` window and `first` reset; when
`request_limits` is non-empty the array wins and the old fields are ignored.
Opening an old relay in the panel shows those two fields as rule rows, and
saving writes `request_limits` without the old fields.

## Free models and free-quota accounts

Two different notions of "free" are easy to conflate:

- **Free model** — the vendor prices the id at zero. The module marks
  `extra.free`, the router sets `core.ChatRequest.FreeModel`; on modules that opted into the
  account-side rule, an account parked for an empty wallet still serves it, and
  only the vendor refusing that model on that account parks the pair. WorkBuddy, Cline, CodeArts, Raccoon,
  QwenWork, OpenRouter, Kilo Code and OpenCode implement the account-side rule.
- **Free-quota account** — the account receives credits, a trial package or a
  daily allowance. Those credits are spent by any paid model, so check the
  multiplier on the **Models and Profiles** page before using one.

The vendor's live catalogue is the authority; the table below records the stable
capability of each module. Use the panel's **Re-fetch** button for the live list.

| Route | Free model | Free quota / recurring allowance |
|---|---|---|
| `openrouter/...` | Yes: `pricing.prompt` and `pricing.completion` must both be zero; `free_only` defaults on; `openrouter/free` is the alias. | The vendor caps free-model calls per day (`free-models-per-day`); an out-of-credit key still serves free ids. No check-in. |
| `kilocode/...` | Yes: anonymous free tier; `free_only` defaults on; the vendor `isFree` flag, a `free`-style id, or published-zero prices mark a model free. | Anonymous access is shared per IP and rate-limited (200 requests/hour); a signed-in account or API key gets its own limits. No check-in. |
| `opencode/...` | Yes: anonymous free realm with `x-api-key: public`; `free_only` defaults on, `-free` ids are recognized, and `free_models` adds others. | Anonymous access is rate-limited; a signed-in Console account can still serve free ids. No check-in. |
| `cline/...` | Yes: the vendor's `recommended-models` `free` array is authoritative; `free_only` defaults on. | No check-in. WorkOS device-code sign-in only. |
| `gemini/...` | Yes: the Cloud Code Assist / Antigravity free tier. | No check-in; quota is the vendor's rolling 5-hour and weekly windows. |
| `officeace/...` | No zero-price model marker: the four selectable ids all draw on trial/bonus credits (`glm-5.2`, `glm-5.1`, `deepseek-v4-flash-0731`, `deepseek-v4.1-flash`). | A fresh sign-in opens `officeace.personal.package.trial`: 500 `officeace_points` + 200 online searches. New-user and daily-check-in bonuses appear as separate activity packages. |
| `workbuddy/...` | Yes: a zero effective multiplier, an open promotion, or a vendor free label; e.g. `hy4-preview` is night-free while `glm-5.2` is only a 0.50x night discount. | CN check-in / intl daily-activity conversation; promotions are only believed inside their validity window. |
| `codearts/...` | Yes: the vendor's free-quota ("benefit") list; fallback `glm-5.3-flash` and `deepseek-v4.1-flash`. | Daily check-in when the package reports `is_credit_package`. |
| `qwenwork/...` | Yes: a vendor `credits` multiplier of zero; built-in `pro`, `flash`, `qwen3.8-max-preview`. | Sash daily check-in. |
| `raccoon/...` | Yes: an effective `billing_multiplier` of zero (including limited-time campaigns); built-in `sn-glm-5-3`, `sn-kimi-k3`, `sn-glm-5-3-flash`, `sn-deepseek-v4-1-flash`. | The desktop login grant extends the day's 300 credits, but the vendor grants it to its own desktop app only. |
| `loomy/...` | No: the built-in catalogue is not zero-priced; the daily gift funds the models. | Daily first-login gift allowance. |
| `qoder/...` | The vendor's Credits decide. | Daily `CLAIM_BENEFIT` Credits when the activity is open. |
| `tabbit/...` | The vendor catalogue decides. | Daily sign-in restores a slice of the quota (shown as a usage percentage). |
| `minimaxcode/...` | The vendor catalogue decides. | Seven-day daily sign-in credits. |
| `lobsterai/...` | The vendor catalogue decides; the Chinese free-quota message parks an exhausted account. | Daily check-in, roughly +100 credits. |
| `autoclaw/...` | The vendor catalogue decides. | Daily credits claim. |
| `trae/...` | The vendor catalogue decides. | CN check-in and the 1024 activity; welfare grants (e.g. 4000 credits) are read from the vendor's entitlements. |
| `zcode/...` | The vendor catalogue decides. | Promotional plan claim (token grants). |
| `doubao/...` | The vendor catalogue decides. | Credits and package quota are displayed; no check-in. |
| `kimi/...` | No free-model or free-credit mechanism is implemented. | No check-in. |
| `openai-compat/...`, custom relays | Vendor-dependent; the platform's `/models` scan decides. | Read the balance through New API, Sub2API or OpenAI Billing when the vendor supports it. |
| `doubaoime/...` | Speech transcription only. | No chat quota. |

These are vendor ids; prefix them with the route when calling the gateway, e.g.
`kilocode/kilo-auto/free`, `cline/cline-free/gemini-3.8-flash`,
`opencode/space-bunny-free`, or `openrouter/openrouter/free`.

Representative built-in/fallback free ids: `openrouter/free`; Kilo's
`kilo-auto/free`, `stepfun/step-5-preview-free`,
`nvidia/nemotron-3-ultra-550b-a55b:free`,
`dots-studio/dots-3-note-preview:free`, `thinkingmachines/inkling-small:free`;
OpenCode's `space-bunny-free`, `deepseek-v4-flash-free`,
`mimo-v2.6-flash-free`, `nemotron-3-ultra-free`; Cline's
`stealth/space-bunny-alpha`, `cline-free/mimo-v2.6-flash`,
`cline-free/gemini-3.8-flash`; Gemini's `gemini-3.8-flash`; OfficeAce's four
trial-credit models; and CodeArts' `glm-5.3-flash`,
`deepseek-v4.1-flash`.

### Recent highlights (0.1.63 - 0.1.73)

* **OfficeAce (Office Claw) platform:** `officeace` is built in with Huawei-account browser sign-in, automatic model-gateway credential storage, account-level gateway addresses, and `officeace/<model>` publication. Windows local credential discovery remains a fallback.
* **OfficeAce parity with WorkBuddy:** the built-in platform now offers daily check-in, credit balance and package breakdown, with automatic renewal of the temporary cloud credential.

* **Anthropic Messages ingress:** Claude Code and Anthropic SDK clients can use
  `POST /v1/messages` and `count_tokens`, including tools, tool results,
  thinking, usage, SSE and Anthropic-shaped errors.
* **Gemini / Google Cloud Code Assist:** Google OAuth login, account discovery
  and refresh, quota reads, Cloud Code Assist translation and streaming.
* **Final outward model whitelist:** `exposed_models` filters the completed
  catalogue and every call entry point, with a hot-reloaded selector under
  **Models and Profiles → Exposed Models**. `platform/*` entries expose a whole
  platform prefix without opening any other platform.
* **Per-account model policy and 429 parking:** disable a model on one account,
  see when a rate limit is released, and park the `(platform, account, model)`
  triple for 60 seconds, doubling to a 30-minute cap or the vendor's
  `Retry-After`.
* **Route-plan preview and speed test:** Platform Configuration → Link Test
  statically replays failover order without reading account state or calling
  upstream. Speed Test resolves the same chain and sends one live request per
  platform through that platform's account pool (256 max tokens, asking the
  model to count), reporting first-event latency, total elapsed time, the
  serving account, completion tokens and tok/s. The tok/s denominator is the
  generation window — the last content event minus the first content event — so
  queueing, prompt processing and trailing bookkeeping never deflate it, and
  first-token latency is reported on its own. When the whole reply lands inside
  one clock tick (typically a single content event) no decode rate can be
  measured, so tok/s shows `-`; a reply with no content event at all falls back
  to total elapsed time.
* **Route-chain reordering:** result rows can be dragged or moved with Up/Down.
  Saving writes the displayed order back to platform priority, model-group
  member priority or schedule-window priority and hot-reloads it.
* **Managed egress:** route all upstream traffic through an existing HTTP/SOCKS
  proxy or a subscription managed by a local mihomo process.
* **ZCode captcha prefetch:** opt into a one-slot parameter pool with
  `captcha_pool` and `captcha_pool_ttl_seconds`.
* **Stream and routing fixes:** retryable errors carried inside an HTTP 200 SSE
  stream fail over before commit, zero-credit accounts leave rotation, explicit
  `platform/model` pins are not silently redirected by model groups, and empty
  Responses turns retry once.
* **Measured context-window retry:** when a vendor refuses an oversized prompt
  with `N tokens > M maximum`, the gateway decodes the `\u003e` escaping, trims
  the oldest history to the measured proportion and retries the same request
  once, so a conversation that used to fail on every retry is served instead.

### Earlier highlights (0.1.26 - 0.1.29)

* **The panel has one polished workspace design:** all eleven views now share an Apple-like
  material system, semantic light/dark colors, smoother page and panel transitions, consistent
  controls, responsive layouts and a reduced-motion fallback.
* **Account health is visible across more platforms:** codearts, openai-compat, tabbit,
  lobsterai and opencode now report pool health and real in-flight counts, and conversation
  affinity keeps a multi-turn session on the account that first served it.
* **Qoder CN is a full chat module:** signed model discovery and streaming chat, packages,
  campaign tasks, SMS auto-login, health, pool stats and sticky sessions are all wired to
  real vendor behavior. Account test buttons now verify chat-capable platforms with one
  minimum-size real completion.
* **Windows installation is quieter and more predictable:** the installer no longer flashes
  a console window, remembers the autostart choice across upgrades, and keeps the existing
  config, accounts, usage and task data.
* **Large account pools are easier to operate:** the accounts table pages at 20 / 50 / 100 / 200
  rows, filters by account state, and sorts account, state, model, priority, balance,
  success/failure, in-flight, usage, last-success and expiry columns. Multi-channel accounts
  stay together across paging and sorting.
* **Account states are more precise:** WorkBuddy risk control and credential faults are separate.
  Risk control is a vendor behaviour restriction that re-login normally cannot fix; a credential
  fault is an expired token or session and can be repaired by signing in again. Legacy
  `account_fault` rows render as risk.
* **The Task Centre is account-first:** schedules are grouped into platform tabs, recovery probes,
  daily balance refreshes, batch runs and the task board share the same account scope, and
  unsaved edits are explicit before Save & Apply.
* **Temporary parked states recover automatically:** each platform gets a recovery probe every
  four hours with +/- one hour of jitter, plus one daily balance sweep in the 30 minutes after
  midnight so a daily grant returns the account to routing.
* **The task board is cached:** previously scanned accounts render from cache, only new accounts
  are fetched, and Refresh task state forces a rescan. An account turns green when all automated
  work is done or only human/locked items remain.
* **Routing is more flexible:** platform and account priorities accept negative integers, where
  a lower number wins, and a platform can override its base priority in Beijing-time windows.
* **Usage is easier to read:** Recent calls now records the caller's session id, falls back to a
  content-derived `d-` key when none was supplied, and resolves account labels through the same
  operator note / module label / raw id order as the Accounts page.
* **Platform coverage was extended and corrected:** Qoder CN, QwenWork credits, Tabbit's daily
  3% sign-in, Raccoon's daily 300 credits and Loomy's first-login growth tasks are represented
  by real module capabilities rather than panel-only buttons.
* **Qoder CN now chats:** the model catalogue and completion stream go straight to
  `gateway.qoder.com.cn` with the module's own COSY signing, and every platform that
  can chat verifies an account with one minimum-size real request.
* **Panel actions are consistent:** one topbar Refresh button is present on all eleven views and
  dispatches per view; Chat test and Runtime log now have refresh too. Model context length and
  max output are visible, editable and restorable from the Models view.

## The one design rule

> **Each client is an independent module. Changing one cannot affect another.**

That is enforced mechanically, not by convention:

```
        cmd/  ──┐
    internal/  ─┴──►  internal/core   ◄──  clients/<name>
```

* Nothing under `internal/` or `cmd/` may import a `clients/*` package.
* No `clients/*` package may import another `clients/*` package.
* Every module registers itself from `init()` via `core.Register`.
* `clients/all/all.go` is the **only** file that knows the full list — it is
  nothing but one blank import per registered client module.
* `cmd/client2api/main.go` builds each module in turn; **if one fails to
  construct, it is logged and skipped**, and the process keeps running with the
  rest.
* Every module keeps its state inside its own `data/<name>/` directory.

So: to add a client you create `clients/<name>/`, add one import line, and touch
nothing else. To remove one you delete the directory and the line. To rewrite one
you never look at the other modules.

See [`docs/MODULE-CONTRACT.md`](MODULE-CONTRACT.md) for the interface, and
[`docs/upstream/<name>.md`](upstream) for a client's protocol notes where one
exists — several early modules have a dossier there (workbuddy, trae, qwenwork,
tabbit, kimi, zcode). Later modules document their protocol inside their own
`clients/<name>/README.md`.

## Build

Go 1.27 is vendored in the workspace (there is no system Go). Every shell needs:

```powershell
$tools='D:\client2api-lab\_tools'
$env:PATH="$tools\go\bin;$env:PATH"
$env:GOCACHE="$tools\gocache"; $env:GOTMPDIR="$tools\gotmp"; $env:GOMODCACHE="$tools\gomodcache"
$env:GOPROXY='https://goproxy.cn,direct'
Set-Location 'D:\client2api-src'
```

The cache redirects are required: the sandbox denies Go's default
`%LOCALAPPDATA%\go-build`, and `go build` from outside the repo root fails with
`cannot find main module`.

```powershell
go vet ./...
go build ./...
go test ./...                       # offline unit tests, no network
go build -o client2api.exe ./cmd/client2api
```

`make` wraps exactly those commands (plus `gofmt` and `-race`) and builds into
`bin/` instead of the repository root. On a machine with `make`:

```bash
make check          # gofmt -l, go vet, go test -- the same gate CI runs
make race           # the same suite under the race detector (needs CGO_ENABLED=1)
make build          # bin/client2api + bin/probe, stripped, version string injected
make run            # build, then start the gateway on the configured listen address
make help           # every target
```

`make build` injects the version from `var version` in `cmd/client2api/main.go`,
which is the same single source of truth the release workflow reads, so a locally
built binary and a tagged release can never disagree about their version.

## Run

```powershell
.\client2api.exe -config configs/client2api.json
# or
.\client2api.exe -listen 127.0.0.1:8788 -proxy http://192.168.0.217:18867
```

Flags: `-config`, `-listen`, `-data-dir`, `-proxy`, `-list-clients`.

Then:

```powershell
curl.exe -s http://127.0.0.1:8788/healthz
curl.exe -s http://127.0.0.1:8788/v1/models
curl.exe -s http://127.0.0.1:8788/v1/status
curl.exe -s -X POST http://127.0.0.1:8788/v1/chat/completions `
  -H "Content-Type: application/json" `
  -d '{\"model\":\"zcode/GLM-5.3\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"stream\":true}'
```

The same models are also served over the **Responses API**, which is the only
wire protocol Codex CLI speaks (`wire_api = "responses"`):

```powershell
curl.exe -s -X POST http://127.0.0.1:8788/v1/responses `
  -H "Content-Type: application/json" `
  -d '{\"model\":\"zcode/GLM-5.3\",\"input\":\"hi\",\"stream\":true}'
```

Point Codex at this gateway and it needs no translating relay in between:

```toml
model = "zcode/GLM-5.3"
model_provider = "client2api"

[model_providers.client2api]
name = "client2api"
base_url = "http://127.0.0.1:8788/v1"
wire_api = "responses"
env_key = "CLIENT2API_KEY"
```

See *The Responses API* below for what is translated and what is deliberately not.

Panel: <http://127.0.0.1:8788/panel/>.

> Use `curl.exe`, not `Invoke-WebRequest` — the latter is broken in this
> non-interactive shell.

## Deploy

This project is designed for personal, single-user study and local use. Run it
on `127.0.0.1`; hosting it as a shared server is not supported.

### Release bundles

`.github/workflows/go-binaries.yml` builds five platforms
(windows/linux/darwin × amd64, plus linux and darwin arm64) on every push to
`main`, asserts that the injected version string is really in the binary, and on
a `v*` tag publishes zip/tar.gz archives plus `checksums.txt`. Each archive
carries the binary, `configs/client2api.example.json` and this README. Copy the
example to `configs/client2api.json` and **fill in `api_key` before starting** —
an empty key leaves both the gateway and the panel unauthenticated, which the
program will warn about on every start.

### Windows installer

`installer\build.ps1` stages a release build into `installer\setup\payload`
and builds `dist\client2api-setup-<version>.exe`, a self-contained installer
with a GUI wizard and a scriptable console mode. No NSIS or Inno Setup.

```powershell
pwsh -File installer\build.ps1            # carries the staged config and data/
pwsh -File installer\build.ps1 -NoData    # empty, shareable package
```

The version is never typed twice: it is read from `var version` in
`cmd/client2api/main.go` and injected into both binaries with `-X`, and
`go test ./installer/...` fails if `installer/setup/main.go` drifts from it.
Bump that one literal before cutting a new setup .exe.

The build refuses to produce an installer whose control panel does not boot.
`cmd/panelsmoke` starts the staged gateway on a random loopback port and loads
`/panel/` in headless Chrome or Edge, asserting the shell's own
`<html data-c2a-ready="1">` marker. The same check runs on the target machine
before the existing service is stopped, and again on the files that landed -- a
failure there restores the previous version from a snapshot. `-SkipSmoke` exists
only for machines with no browser at all.

Installing over an existing install is an upgrade, not a reset. The program
files are replaced, while `configs/client2api.json` and everything under
`data/` are left exactly as they were found -- the live config, the account
pool and the usage history survive, including when the new package was built
with `-NoData`. Uninstall first if a clean slate is what you want.

The setup image is built as a Windows GUI executable, so double-clicking it does not add a
console window beside the wizard. Silent or scripted console flags attach back to the
launcher's console when one exists. The autostart checkbox is remembered across upgrades;
when upgrading from a build that predates the preference, an existing Startup shortcut is
treated as checked.

## Credential handling

Modules discover credentials from each client's own on-disk store and keep them
under `data/<name>/`. Nothing renders a credential: both exits that expose a
`core.Status` — `GET /v1/status` and `/panel/api/status` — pass it through
`core.RedactStatus`, which masks JWT-shaped strings, `Bearer …` values and
`key=value` credential pairs (`internal/core/redact.go`). Identifiers that are not
secrets (`3595881099822378`, `cli-login`, `zcode-config:builtin:bigmodel`) pass
through untouched.

This is defence in depth, not the primary control: a module is expected not to put
a credential in `Status` at all. It exists because a module whose account carries
no stable user id may fall back to a token prefix as its id.

### ZCode accounts and credits

A single Zhipu account can hold a plan JWT, a coding-plan API key and an
OAuth-derived key at the same time. The panel groups those credentials by
account identity into "one account, multiple channels" instead of showing three
separate accounts.

Web sign-in has two realms: International uses Z.AI (chat.z.ai), China uses
BigModel (bigmodel.cn). They are different services, and one phone number cannot
be used across them: a China number must use the China option, while the
international option fails validation with "request failed". After sign-in the
account carries a `realm` tag. The China realm uses the plan JWT returned by the
vendor's polling flow directly (bigmodel has no api.z.ai exchange flow), so do
not treat it as an international account for key exchange, or the result will be
401/404.

Credits come from two upstream APIs and are merged in the panel:

- `open.bigmodel.cn/api/monitor/usage/quota/limit`: the 5-hour rolling window,
  the daily token window, current usage, remaining amount and reset time;
  `subscription/list` adds the plan tier and expiry date.
- `zcode.z.ai/api/v1/zcode-plan/billing/balance`: one-off or recurring token
  grants such as activity packages and Start Plan.

The promotion claim is JWT-only: the Claim activity package button needs a jwt
channel in the account (the module uses CheckinAction.Channels to say so). The
panel groups a plan JWT and a coding-plan API key under one account, so the
button appears on both of that account's rows - including the API-key row,
which the module redirects to the sibling JWT. An isolated API key (an account
with no jwt at all) gets no button, because the plan billing endpoint only
accepts plan credentials. The backend still resolves sibling channels for the
same account, so claims started from the task board or scheduled batches (which
receive just one account id) can still find that account's JWT. If the vendor
returns "current user has no coding plan", the key itself does not belong to a
coding-plan package; it is not a panel display bug. Sign in with the matching
package account in the ZCode client and import it again.

After a successful claim (or a vendor "already claimed" response), an earlier
`exhausted` verdict caused by depleted credits is cleared and the account
returns to routing immediately; a balance refresh that reads available credits
clears it too. This means "already claimed 100M but the state is still cooling
down" no longer keeps the account out of rotation.

## Model routing

Three accepted forms:

| form | meaning |
|---|---|
| `zcode/GLM-5.3` | explicit: module `zcode`, model `GLM-5.3` |
| `GLM-5.3` | the unique module whose catalog lists it (ambiguous → 400 listing the owners) |
| `gpt-4o` | whatever the `aliases` table in the config says |
| `deepseek-v4.1-flash` | a `model_groups` group name, or a bare member model id: route across every member of that group |
| `Auto/GLM-5.3` | aggregate every platform that serves the model, ordered by platform priority |

A module always receives the **bare** model name, never the qualified one.

The first segment of a slashed request is a module name **only if a module of
that name exists**. Some upstreams put a slash inside their own model ids —
`cline` ships `cline-free/gemini-3.8-flash`, and every OpenRouter id looks like
`nvidia/nemotron-3.5-lightning:free` — so a request whose first segment names
no module is looked up in the catalogs as a whole id before it is rejected.
Both spellings work: `nvidia/nemotron-3.5-lightning:free` and
`openrouter/nvidia/nemotron-3.5-lightning:free` reach the same model, and the
module receives its own id either way. A module name always wins over a catalog
id that happens to start with the same word.

`Auto/<model>` is additive: the gateway still lists every platform-specific id
unchanged, and also exposes one aggregated name per model.  That name resolves
through the same platform-priority, health, cooldown, and failover path as a
bare request.  Use a qualified id when the request must stay on one platform.

`model_groups` is the operator-defined equivalent of `Auto/`: declare the same
model under its platform-specific ids once, and from then on the group name and
every member id route across all available members. Group names are
case-insensitive and must not contain `/`; a member is `client/model`, may
belong to only one group, and cannot collide with an `aliases` key. An explicit
`client/model` still locks that platform and never expands through a group.
`Auto/<model>` may also be used as a dynamic member: it expands to every live
platform whose catalogue serves that model (subject to the same blacklist and
health checks), then the concrete members listed alongside it are added and
deduplicated. A group can use `Auto` in `platform_priorities` for those dynamic
members to position that whole pool relative to named members; inside the pool
the normal `platforms.<name>.priority` and `priority_schedule` still decide
which provider is tried first.
Inside a group the member platform's `platform_priorities` are tried first;
platforms without a group priority fall back to the global
`platforms.<name>.priority` and `priority_schedule`. Members missing from a
live catalogue are skipped, so one flaky module cannot take the group down.

`exposed_models` sits on top of all of this. It does not change how a request
routes; it decides which final ids the gateway admits at all. The value is the
list of ids a caller may use, matched against the request's original model string
before any resolution, so `Auto/GLM-5.3`, a group name, an alias and an explicit
`platform/model` are each allowed or denied on their own.

Five GPT-6+ groups ship as built-ins so a Codex client can ask for the native
lowercase name with no platform prefix: `gpt-6-astra`, `gpt-6.1-sol`,
`gpt-6-sol`, `gpt-6-luna` and `gpt-6.1-sol-pro`, mapped onto the OpenCode and
OpenRouter members that currently serve them, plus an `Auto/<model>` member so
custom relays that publish the same model are discovered automatically. They
appear in **Platforms → Model Routing Groups** marked **built-in**. Editing one
writes an override into `model_groups` that replaces the default from then on;
a config entry with the same name and an explicit empty member list suppresses
it.

When a router-chosen candidate (a bare name or `Auto/`) answers `404 model not
found`, the gateway demotes that platform/model pair briefly and tries the next
candidate instead of returning the 404: the caller did not choose the platform,
so one vendor retiring an id must not fail the request.  An explicitly
qualified id is not rescued -- there the 404 is the honest answer.

## The Responses API

`POST /v1/responses` serves the same models as `/v1/chat/completions` over the
OpenAI Responses wire format. OpenCode providers using `@ai-sdk/openai-compatible`
use `/v1/chat/completions`; providers using the Responses model from
`@ai-sdk/openai` use `/v1/responses`. Both use a base URL ending in `/v1`.
The Chat stream identifies the assistant in its first chunk and omits unset
roles in subsequent deltas, including tool, finish and usage chunks.

The Responses endpoint was added because **Codex CLI no longer
speaks Chat Completions.** Its `WireApi` enum has a single variant, `Responses`,
and `wire_api = "responses"` is the only value a provider block may name — so an
endpoint that only implements Chat Completions makes Codex fail at the first
turn.

The usual fix is to put a translating relay between Codex and the gateway. That
works until the request carries **encrypted agent content** — the ciphertext a
delegated sub-agent's message is wrapped in. A relay cannot translate what it
cannot decrypt, so its only honest answer is a 400, and the turn ends before any
vendor is reached. This endpoint removes the relay from that path: it speaks
Responses natively, so there is no translation step to fail.

### What is translated

| Responses request | becomes |
|---|---|
| `instructions` | a leading `system` message |
| `input` as a string | one `user` turn |
| `input` as an array | ordered `core.Message`s (see below) |
| `message` item (`user`/`assistant`/`system`/`developer`) | a message; `developer` maps to `system` |
| `input_text` / replayed `output_text` | text content, including assistant history |
| `input_image` with `image_url` and `detail` | an image content part |
| `function_call` item | an `assistant` turn carrying `tool_calls`; a namespaced call uses the name the vendor was offered |
| `function_call_output` item | a `tool` turn keyed by `call_id` |
| `reasoning` item | its `summary` text, kept as the turn's reasoning |
| `compaction` item | its plaintext summary, when it holds one |
| `tools` (flat, nested under `function`, or a `namespace` container) | `core.Tool` declarations; a namespace's children flatten to `namespace__child` |
| built-in tools (`web_search`, …) | skipped, never fatal |
| `reasoning.effort` / `reasoning_effort` | the request's thinking level, forwarded to upstream modules |
| `max_output_tokens` | `max_tokens` |
| an unknown item type | skipped, never fatal |

A **namespace** tool is how Codex groups one tool server's children, e.g.
`{"type":"namespace","name":"mcp__codegraph","tools":[…]}`. Chat has no
namespaces and cannot call a container that carries no schema, so each child is
declared as `namespace__child` and a call to that name is split back into the
`(namespace, name)` pair Codex dispatches on. Forwarding the container unchanged
offers the vendor a function named after the server, and every call to it comes
back as `unsupported call`.

The array form is where the two protocols genuinely differ, and the translation
is not a field rename. Responses has no `messages` array: a tool call, its
result and a reasoning echo are **siblings** in one flat `input` list. Chat
models express the same facts as an assistant turn carrying `tool_calls`
followed by `tool`-role turns, so a `function_call` opens an assistant turn and
the matching `function_call_output` closes it. Reasoning that precedes a call is
attached to that call's turn, which is where a Chat model expects to find it.

### What is emitted

Streaming follows the sequence Codex's parser requires:

```
response.created
  response.output_item.added            (on first appearance, with a stable ID/index)
  response.content_part.added / response.reasoning_summary_part.added
  response.output_text.delta …          (assistant text)
  response.reasoning_summary_text.delta … (reasoning)
  response.function_call_arguments.delta … (tool arguments)
  corresponding text/arguments/part.done
  response.output_item.done             (one per reasoning / message / function_call)
response.completed                      (carries response.id and usage)
```

Two details are load-bearing and easy to get wrong. Codex dispatches on the
`type` field **inside** each payload rather than on the SSE `event:` header, so
both are written from one string and can never disagree. And
`response.output_item.done` — not `.added` — is what Codex acts on for both
tool dispatch and assistant-message finalisation, so a stream that only emits
deltas shows text and never runs a tool. A stream that breaks mid-flight emits
`response.failed` rather than hanging.
Items retain their IDs and indexes through deltas, `.done`, and final output.
Every SSE payload carries an increasing `sequence_number`, including failures.
Output text always includes `text` and `annotations` (even `""` and `[]`), and
function-call items always include their `arguments` string, so strict SDK
validators can read buffered replies and terminal items. A call into a tool
namespace also carries `namespace` beside `name`: Codex dispatches on that pair,
so the flattened name it was declared under is split back before the item is
emitted.
Token-limit termination sets `status = "incomplete"` and
`incomplete_details.reason = "max_output_tokens"` in both response modes;
streaming emits the terminal `response.incomplete` event.

Non-streaming (`"stream": false`) returns one `response` object whose `output`
array carries a `reasoning` item when there was reasoning, a `message` item, and
one `function_call` item per call.

### Encrypted agent content

An item whose content is encrypted is **accepted, not rejected** — that is the
difference this endpoint exists to make. The gateway does not try to read the
bytes; it carries through the one fact the model can act on and drops the rest:

* An `agent_message` keeps its plaintext envelope (author, recipient, task name)
  and its text parts. The ciphertext is replaced by
  `[encrypted agent payload omitted]`.
* A `reasoning` item keeps its `summary` text. Its `encrypted_content` cannot be
  replayed to any vendor but its issuer, so it is dropped rather than forwarded.
* A `compaction` item keeps its plaintext summary when it holds one; a blob this
  gateway minted is verified and replayed (see *Compaction* below), and a real
  ciphertext blob is skipped.

The ciphertext is **never** forwarded to a vendor and never decoded into
invented text: the gateway does not hold the key, so anything it produced would
be fabrication. The sub-agent payload's *content* is therefore not replayed;
what is preserved is the fact that a delegated message exists and who sent it.

### Compaction

`POST /v1/responses/compact` folds a conversation into a handover summary and
returns an object with `object: "response.compaction"` whose `output` is the
retained turns followed by exactly one `type: "compaction"` item. On
`POST /v1/responses`, `context_management: [{"type":"compaction",
"compact_threshold": N}]` folds the conversation automatically once the estimated
input reaches `N` tokens and appends the same item to that turn's output.

The item's `encrypted_content` is **gateway-owned**, not OpenAI's opaque state. It
is a summary this deployment produced and HMAC-signed with a key from
`data/compaction.key`, which survives an upgrade. Replay rules:

* A blob this deployment signed is verified and materialised as an `assistant`
  turn, so the folded state is genuinely carried forward.
* A blob with this gateway's `c2c1.` shape whose signature does **not** verify
  (another deployment, a tampered value, or a lost key) is dropped. It is never
  replayed as prose.
* Genuine OpenAI ciphertext, and a relay's plaintext summary smuggled through the
  same field, keep their previous behaviour: ciphertext is dropped, plaintext is
  preserved.

Summarisation runs through the normal routed model, so account rotation, platform
failover and usage accounting behave as they do for a user-visible turn. When the
summariser cannot run at all, the endpoint still returns a usable
`response.compaction` object built from the retained turns rather than a 500.

### Context-window refusals

A vendor refusal that means "the prompt is too large" is surfaced as
`error.code = "context_window_exceeded"`, which is the code a Responses client
acts on by compacting. This holds even when a module only saw a plain 400 and
classified nothing: the gateway itself recognises the wording ("prompt is too
long", "maximum context length", "run compaction", and their Chinese
equivalents) and promotes the failure. A numbered refusal additionally teaches
the gateway the model's real window, so later requests are trimmed before they
are sent.

### Limits

* **Stateless.** `store` and `previous_response_id` are accepted and ignored.
  The caller replays its history in `input`, which is what Codex does anyway.
* **Function tools only.** A built-in tool has no function name to forward, so
  it is skipped.
* **No `output_text` convenience field** on the response object; read it from
  the `message` item's `content[0].text`.

## Configuration

`configs/client2api.json` — the seven keys the gateway needs, plus the optional
sections below:

```json
{
  "listen": "127.0.0.1:8788",
  "api_key": "",
  "data_dir": "data",
  "proxy": "",
  "egress": { "enabled": false, "mode": "subscription", "url": "", "mihomo_path": "" },
  "aliases": { "gpt-4o": "trae/custom_model_gpt-5" },
  "exposed_models": null,
  "disabled": [],
  "clients": {
    "<name>": { }
  },
  "model_groups": {
    "deepseek-v4.1-flash": {
      "members": ["opencode/deepseek-v4.1-flash", "cline/cline-free/deepseek-v4.1-flash"],
      "platform_priorities": { "opencode": 10, "cline": 20 }
    }
  },
  "features": { "sanitize_blacklist_fingerprints": true },
  "cooldown": { "soft_rate": "600s", "soft_rate_max": "2h" },
  "prompt": { "mode": "passthrough", "file": "" },
  "schedule": { "enabled": false }
}
```

* `api_key` empty ⇒ **no inbound authentication**. Set it before exposing the
  port beyond loopback.
* `clients.<name>` is handed to that module verbatim as `core.Deps.Config`; each
  module owns its own schema and documents it in `clients/<name>/README.md`.
* `proxy` is applied to every module's vendor transport. A module that builds its
  own fingerprinted client forces HTTP/1.1 whenever a proxy is set, because
  `x/net/http2` cannot traverse one.
* `egress` is the optional managed outlet. `client` mode accepts an
  `http`, `https`, `socks5` or `socks5h` proxy URL. `subscription` mode starts a
  local mihomo binary with the URL as a proxy provider, then routes the gateway
  through its loopback mixed port. An enabled `egress` takes precedence over
  `proxy`; `mihomo_path` may be set explicitly or left empty for PATH / Clash
  Verge discovery. The subscription URL is a credential and is redacted in the
  panel.
* `exposed_models` is the final outward model whitelist. It is applied **after**
  routing builds the full catalogue, so it filters `platform/model` ids,
  `Auto/<model>` names, `model_groups` names, aliases and transcription ids, and
  each final id is its own switch -- exposing a group member does not expose the
  group name, and exposing an alias target does not expose the alias. Selecting a
  model group also keeps its exact member `platform/model` ids callable, without
  adding those members to the `/v1/models` catalogue. An entry
  ending in `/*` exposes every model under that platform prefix (for example
  `my-relay/*`) without opening any other platform. Matching is
  case-insensitive and ignores surrounding space. `null` (the default, or the key
  absent) serves every final model; `[]` serves none. The whitelist gates
  `GET /v1/models` and every call entry point, so a hidden id is rejected even
  when it is named directly. Edited from **Models and Profiles -> Exposed Models**.

### Environment overrides

Every deployment-level knob can also come from the environment, which is what a
service manager or supervisor should use — the file stays the operator's document and
the environment is the deployment's. `CLIENT2API_*` is this program's spelling;
the reference project's `WB2A_*` names are accepted as aliases, so an existing
`workbuddy2api-panel` deployment keeps working unchanged. When both spellings are
set, `CLIENT2API_*` wins.

| variable | config key |
|---|---|
| `CLIENT2API_LISTEN` | `listen` |
| `CLIENT2API_API_KEY` | `api_key` |
| `CLIENT2API_DATA_DIR` | `data_dir` |
| `CLIENT2API_PROXY` | `proxy` |
| `CLIENT2API_SOFT_RATE` / `CLIENT2API_SOFT_RATE_MAX` | `cooldown.soft_rate` / `soft_rate_max` |
| `CLIENT2API_PROMPT_MODE` / `CLIENT2API_PROMPT_FILE` | `prompt.mode` / `prompt.file` |
| `CLIENT2API_EXPIRING_SOON` / `CLIENT2API_PREFER_EXPIRING` | `pool.expiring_soon` / `pool.prefer_expiring` |
| `CLIENT2API_SANITIZE_FINGERPRINTS` | `features.sanitize_blacklist_fingerprints` |
| `CLIENT2API_AUTH_DIR` | `clients.workbuddy.accounts_dir` |
| `CLIENT2API_USER_AGENT`, `CLIENT2API_CLIENT_VERSION`, `CLIENT2API_CLI_VERSION`, `CLIENT2API_CLIENT_NAME` | the same keys under `clients.workbuddy` |
| `CLIENT2API_DEVICE_TOKEN`, `CLIENT2API_DEVICE_TOKEN_FILE` | `clients.workbuddy.device_token` / `device_token_file` |
| `CLIENT2API_TIMEOUT_SECONDS`, `CLIENT2API_HEADER_TIMEOUT_SECONDS`, `CLIENT2API_IDLE_TIMEOUT_SECONDS` | the same keys under `clients.workbuddy` |
| `CLIENT2API_PASSTHROUGH_IP` | `clients.workbuddy.passthrough_ip` |

Three rules, all inherited from the reference:

* **Precedence is defaults < file < environment.** The environment is applied
  after the file is parsed and before defaulting, so it can set a knob the file
  never mentions, and a defaulting pass cannot overwrite what it just said.
* **Only a non-empty value counts.** `unset` and `set to empty` mean the same
  thing, so a supervisor that inherits a blank variable does not erase the file's
  setting.
* **A value that does not parse is ignored**, and the file's value stands.
  Silently flipping a safety switch is worse than the typo.

The reference kept its upstream identity (`user_agent`, `client_version`,
`cli_version`, `client_name`, `device_token`, `device_token_file`,
`passthrough_ip`, the three timeouts) in one global `upstream` section, because it
served a single vendor. Those knobs are per-client here, so the corresponding
variables patch `clients.workbuddy` — a block that is otherwise opaque JSON, and
is re-encoded around the override rather than replaced. A block that is present
but is not a JSON object is left exactly as the operator wrote it, so the module
can report the mistake in its own words.

`WB2A_STATE_FILE` has no counterpart: the pool's runtime state lives beside the
credentials it describes (`pool.json` in workbuddy's account directory) and cannot
be addressed separately. `WB2A_AUTH_DIR` maps to `accounts_dir` rather than
`data_dir`, because that is the directory it actually named.

The environment is applied on **both** the load and the reload path — a reload
calls the same loader — so `POST /panel/api/reload` picks up a changed variable.
The panel's own save path reads the file from disk, merges the form's changes and
writes it back, so an environment value is never copied into the file, which is
what you want when the environment holds a secret. On a fresh install the
generated `api_key` is still written to disk for the operator's benefit, and the
startup log says so explicitly when the environment's key is the one in force.

### Which sections are honoured

Two of the optional sections are **hot**: a reload re-reads them and pushes them
to every module that implements `core.LiveReloader`, with no restart.

| section | state |
|---|---|
| `features.sanitize_blacklist_fingerprints` | hot; defaults **on** (a false value must be written explicitly) |
| `cooldown.soft_rate` / `soft_rate_max` | hot |
| `prompt.mode` / `prompt.file` | hot; **only workbuddy acts on it** — see below |
| `pool.max_in_flight` / `pool.max_in_flight_global` | hot; enforced by workbuddy |
| `pool.breaker_*` / `pool.degrade_*` / `pool.idle_weight_*` / `pool.prefer_expiring` / `pool.expiring_soon` / `pool.cost_explore_interval` | hot; acted on by workbuddy |
| `session_sticky.enabled` / `ttl` / `gc_interval` | hot; the switch and the window reach every module that binds conversations |
| `platforms.<name>.*` | hot; priority and the model blacklist reach routing immediately, the ceilings are enforced by the gateway, and `reserve_credits` is enforced by workbuddy |
| `model_groups` | hot; group names and member ids start routing immediately, group priorities included |
| `exposed_models` | hot; a reload re-reads the whitelist and it takes effect on the next request |
| `panel.package_detail_limit` | restart; the panel reads it once, at construction |
| `schedule.*` | live (`Reconfigure`), but the running loop only reacts to the *next* wake; the `schedule` block is reported in `GET /panel/api/status` |

`platforms.<name>.priority` and `platforms.<name>.account_priorities` both accept any integer, including negatives. Lower numbers are higher priority: for example, `-10` is tried before `-5`, and the default is `0`.

`platforms.<name>.priority_schedule` overrides the base `priority` during fixed
time windows, which is how an operator gives a platform a better or worse
routing position only during part of the day:

```json
{"platforms":{"zcode":{"priority":5,"priority_schedule":[
  {"start":"01:00","end":"05:00","priority":1},
  {"start":"05:00","end":"08:00","priority":-2}
]}}}
```

Each rule uses Beijing time (UTC+8) regardless of the host timezone, is
half-open `[start, end)`, and may cross midnight (`23:00`-`06:00`). The first
matching rule wins; when no rule matches, the base `priority` is used. Rules
on one platform must not overlap, and `start` must differ from `end`; the panel
rejects both cases before writing. The same values are editable in the panel's
platform cards under 「分时规则」.

`prompt` is the system-prompt strategy: `passthrough` (default), `custom`
(replace every `system`/`developer` turn with `file`, or with the shipped
engineering prompt when `file` is empty) or `append` (insert after the leading
system turns). It exists because a vendor content filter rejected a client's own
system template verbatim; `clients/workbuddy/README.md` records the behaviour and
why the other modules deliberately leave their vendor's prompt alone.

`schedule` runs the daily chores each module advertises through
`core.PlannedBatches` — "run everything" for the whole pool, not just the one client
you have the panel open on. A module that declares no batches of its own but
implements `core.CheckinProvider` gets one synthetic `checkin` batch, so its
account-row check-in button and the timetable are the same chore. Hour lists
are Asia/Shanghai local hours, and an **empty list means "never run this one"**
even when the master switch is on. Accounts are walked serially with a 45 s
gap, because the vendors' anti-abuse checks roll back chores whose events
burst.

 A platform can override the shared hours with its own timetable at `schedule.clients.<platform>.<batch>`. The packaged config gives zcode's `checkin` batch `[0, 9, 12, 18, 21]`: its daily activity quota is limited, `0` aligns with the Shanghai day boundary, and the preview is an unauthenticated GET, so only a real claim opens the browser. The hours are editable in the panel under Task Centre -> Timetable.

`session_sticky` has two levels.  Inside a module it pins a conversation to the
account that last served it.  At the gateway it also pins the **platform**: when
a bare model id is served by several platforms, the platform that last answered
stays first for the next turn, and the other candidates remain as failover.  A
failed platform is replaced by whichever platform then succeeds, so a broken
sibling is not retried on every turn.  `session_sticky.ttl` retunes both levels
on a live reload; `session_sticky.enabled=false` turns both off.

Independent of stickiness, the gateway demotes a platform that cannot serve
**right now** -- every account busy or cooling, or no usable account at all --
behind the healthy candidates for 30 seconds. The check runs *before* the
platform is entered, not after it fails: `Health`, the module's optional
per-model availability probe (for parks that apply to one `(account, model)`
pair) and the gateway's own `max_in_flight` / `max_in_flight_per_account`
counters are all local, so a candidate that cannot serve is skipped for the next
priority without spending a call or filing a failed-candidate row. The last
candidate in a chain is never pre-checked, because a local verdict is allowed to
reorder the chain but not to refuse the request.  This is backpressure, not a
failure: it raises no alert, and the platform stays in the candidate list as a
last resort.  Repeated upstream failures still take the full 10-minute cooldown.
`schedule.balance_refresh_enabled` (default **on**, as in the reference) adds a
tick of its own every `schedule.balance_refresh_minutes` (default **5**) — it asks
every module that can report a balance for one, which is how a credit park lifts
by itself, and then refreshes the model lists. Two check-ins are hours apart, so
without it credits go stale and a balance-recovered account stays parked until
the next chore. Set the minutes to `0` to drop the tick without touching the
switch. It is still gated by `schedule.enabled`, like every other batch.

`schedule.recovery_enabled` (default **on**) adds one platform-scoped recovery probe
every `schedule.recovery_every_minutes` (default **240**) with a symmetric random
spread of `schedule.recovery_jitter_minutes` (default **60**): `4h +/- 1h` means
the next probe lands between three and five hours later. A platform can override
the shared values at `schedule.clients.<platform>.recovery` with
`every_minutes` / `jitter_minutes` or `every_seconds` / `jitter_seconds`; `0`
jitter means no spread. The probe
rechecks temporary parked states such as cooling, exhausted or rate-limited and
renews expiring credentials. The frequent balance sweep deliberately leaves those
accounts alone, while an explicit test, refresh or manual recovery press still
checks immediately. The Task Centre groups the rules by platform and exposes both
interval controls in the recovery row.

`schedule.daily_balance_enabled` (default **on**) adds one platform-scoped
balance sweep. The shared default is `schedule.daily_balance_hours` (default
**0**, i.e. midnight CST), spread randomly across the following 30 minutes; an
empty list turns the task off, and `schedule.daily_balance_cron` can provide a
shared cron alternative instead. The platforms that grant a daily quota top
accounts up at the turn of their day, so this is the sweep that turns a grant
into a usable account: it refreshes every account including the parked ones, so
an exhausted account that now has credits unfreezes without an operator press.
The Task Centre lists it as a `每日余额刷新` row next to recovery and offers three
per-platform modes: **by time** (`hours`, with the original 30-minute spread),
**by interval** (`every_seconds` plus `jitter_seconds`, for example `20m` and
`3m`, or `30s` for a second-level interval; jitter is symmetric), and **Cron**
(`cron`, a 5-field minute expression or 6-field expression with leading seconds,
evaluated in Asia/Shanghai). Switching modes clears the other mode's keys, and
following the shared schedule removes the platform override entirely.

The accounts page does not wait for that sweep. Its balance column reads a
persistent cache (`data/panel/balance_cache.json`) and only ever nudges a few
accounts at a time (3 per pass, 2s apart, at most one pass per 45s), so
opening the page cannot turn into a fleet-wide burst against the vendor.

`pool.max_in_flight` (default **3**) caps how many requests one account may have
in flight at once; `pool.max_in_flight_global` (default **2**) is the tighter cap
for `global`-realm accounts, which is where the reference ever only saw its WAF
403s. `0` means "no ceiling" for the per-account value, while the global value
falls back to the per-account one when it is not set (or is not positive), so
"unset" and "allow nothing" stay different values. An account at its ceiling is
**skipped by the picker rather than queued for** — waiting would still deliver the
burst the ceiling exists to avoid — and when every usable account is at its
ceiling the module returns `core.ErrBusy`, which the gateway answers as **429**
with `Retry-After: 1`. The live counters are visible in `/v1/status`
(`client_health`) and, per module, in `GET /panel/api/status` as `pool`
(`in_flight`, `in_flight_full`, `sticky_sessions`) and `health`; a module that
implements neither is simply reported without those keys.

`pool.max_in_flight` is the internal pool ceiling for a module that keeps its
own account pool. WorkBuddy is the main consumer and reads the whole `pool.*` /
`cooldown.*` block; OpenRouter's single-key pool honours `pool.max_in_flight`,
`pool.breaker_threshold` and `pool.breaker_cooldown`. A WorkBuddy request takes
an internal pool slot first and a gateway `platforms.workbuddy.*` slot second, so
the two ceilings stack and the tighter one governs in practice. The panel now
keeps the root-level `pool.*` / `cooldown.*` controls inside the WorkBuddy card
on the platform-config page rather than on the main config page; the labels there should not be confused with
the two gateway ceilings below. The JSON keys are unchanged.

Each platform can also carry two gateway-level ceilings in the `platforms`
section. `max_in_flight` (default **2**) caps all requests in flight against that platform;
`max_in_flight_per_account` (default **2**) caps requests in flight against any one account
inside it. Both are live; leave the key absent for the default of 2, and set it to `0` to mean no ceiling. The platform ceiling is
checked first, so a platform at its limit answers `429` without invoking a
module. A full account is skipped by the module's account rotation; if every
usable account is full, the platform also answers `429`. For example,
`{"platforms":{"tabbit":{"max_in_flight":5,"max_in_flight_per_account":3}}}`
allows up to five concurrent requests across the platform while sending no
more than three to any one account.

`reserve_credits` is the per-platform low-balance guard. A configured
`0` (the default) parks an account whose **last known** balance is zero;
a positive value raises the threshold, and `-1` turns the guard off for
that platform. An unknown balance is never parked, so a fresh install
does not start with an empty pool. The park is visible in the panel as
`credits are insufficient; account paused`, and the next `schedule.balance_refresh_minutes` sweep
revives the account automatically once its balance is above the threshold;
a check-in or a manual balance refresh has the same effect. Changing the
value is live and re-evaluates every account immediately. For example,
`{"platforms":{"workbuddy":{"reserve_credits":0}}}` keeps zero-credit
WorkBuddy accounts out of routing and shows them as paused instead of green.

Only a balance reading the vendor's own reply corroborates parks an
account. When a reply contradicts itself about a package that still
advertises capacity -- WorkBuddy's trial package keeps a stale lifetime
`CapacityRemain` after its monthly cycle is spent -- the conservative
number is still shown, but the contradiction is resolved instead of being
left as a pending state for the operator. A positive remainder is recorded
and the account is marked usable; a contradictory zero is probed through
the same real Auto model call as the panel's Test button, so success marks
it usable and failure applies the normal cooling, credit or risk verdict.
Background sweeps remember that decision for six hours per account, so a
contradictory zero is not probed on every pass. A park the vendor earned
with a real refusal keeps its own deadline.

The guard has a pool-side implementation in workbuddy and loomy. In loomy an
HTTP 402 / insufficient-credit chat refusal is also treated as the vendor's
authoritative zero-balance verdict: after the same-account retry it parks the
account with the same low-credit state, so the transient 30s/1m/2m chat
cooldown ladder cannot put an unfunded account back into rotation. A funded
balance read or a successful daily check-in clears the park. A module without
a balance-aware pool ignores the setting.

`account_priorities` is the per-account routing order inside one platform.
It maps the account id shown by that module to an integer; lower numbers
are tried first. Accounts at the same value keep the module's normal
round-robin, weighted rotation or LRU policy. This is therefore a tiered
preference, not a permanent pin: when every preferred account is cooling,
disabled or at its concurrency ceiling, the next tier is used. The account
pool page edits this value inline and saves it through the same live config
path as the other platform settings.

```json
{"platforms":{"workbuddy":{"account_priorities":{"account-id-1":-1,"account-id-2":10}}}}
```

`account_notes` is the operator's own label for one account inside a platform:
the phone number or e-mail that credential signs in with. It maps the account
id shown by that module to free text, is display metadata only (routing never
reads it), and is edited from the Note button on the account row through
the same live config path as `account_priorities`. It exists because most
vendors' user-info replies carry a nickname rather than the identity the
credential was issued to, so the panel cannot derive it — and without it an
expired account cannot be told apart from its siblings before a re-login.
When set, the note takes the account row's headline and the module's own
label moves down to the hint line; a re-login names the same identity.

```json
{"platforms":{"trae":{"account_notes":{"3595881099822378":"13800138000"}}}}
```

`pool.breaker_threshold` (default **3**) is how many consecutive failures park an
account on the breaker axis; the parking time doubles per trip, starting from
`pool.breaker_cooldown` (default **30m**) and capped by
`pool.breaker_cooldown_max` (default **6h**). `pool.degrade_threshold` (default
**5**) is a *separate* consecutive-failure counter, fed only by failures that say
nothing about the account — transport errors and unrecognised 4xx — and
`pool.degrade_cooldown` (default **10m**, clamped by `pool.degrade_cooldown_max`,
default **2h**) is the flat time it earns. The breaker, the degrade penalty and
the ordinary classification cooldown are three independent deadlines and the
**latest** one wins: they are never summed, so a retry after the longest of them
is always allowed. `pool.idle_weight_per_hour` (default **0.5**) adds weight to
an account for every hour it has gone unused, capped by `pool.idle_weight_max`
(default **5.0**); the picker is a smooth weighted round-robin, so this tilts the
rotation towards cold accounts without ever starving the hot ones.

`pool.prefer_expiring` (default **true**) orders candidates by the earliest
expiry among their credits, which is what stops an expiring grant from being
wasted behind a long-lived one; it only applies when that batch falls inside
`pool.expiring_soon` (default **168h** — an empty or zero value switches the
preference off). `pool.cost_explore_interval` (default **30m**) is the cost-tier
Changing `pool.expiring_soon` is hot: the new window is projected into the live
snapshot and every cached WorkBuddy expiry classification is discarded, so the
pool, balance refresh and panel all use the new window on the next read.
exploration window: once a model is known to be free on some account, accounts
with no observation for that model are normally passed over, and this window is
how often one of them is allowed through instead. The exploration is a
**diversion of a request that was going upstream anyway, never an extra call**,
so it adds nothing to the IP's request count; `"0"` switches it off.

`session_sticky.enabled` (default **true**) is the master switch, and it is
deliberately not the same knob as the window: turning it off means **no binding
is ever made**, so every request selects an account from scratch — the reference
builds no session router at all in that case (`cmd/server/main.go`). It is a
pointer because "absent" and "explicitly false" must stay distinguishable.
Switching it off also drops the bindings already held, because those were chosen
under a routing policy that is no longer in force; the reference, never having
built the router, has no such memory either. Setting `ttl` to `"0"` is *not* how
you disable stickiness — that would read as "expire immediately", so a
non-positive window is resolved back to the 30m default instead.

`session_sticky.ttl` (default **30m**) is how long a conversation stays pinned to
the account it started on. The key comes from `conversation_id`,
`conversationId`, `prompt_cache_key` or the request's user field, in that order;
when a caller names none of them the router derives one from the message content
(`core.DeriveConversationKey`), hashing the system prompt plus the first user turn
and returning it under a `d-` prefix. Every module now takes that content fallback,
so a client that sends no id at all still keeps its account instead of rotating
every turn and re-billing the whole prefix — including the custom relay sources
under `sources.<id>`, whose key pool is sticky per source. `gc_interval`
(default **5m**) is how often expired bindings are swept. All three are hot: every
module implements `core.LiveReloader`, so a reload retunes the window on the live
process without a restart. A binding whose account has been parked, is cooling
down for the requested model, is disabled or removed, or does not advertise the
model is dropped rather than served, so stickiness can never turn into a failure.

`panel.package_detail_limit` (default **5**) is the panel's own display setting,
not a routing one: the credits view sorts each account's live batches by earliest
expiry and shows only the first N, folding the remainder and the already-spent
ones behind clickable group headers. It lives in the shared config rather than a
client block because the panel is shared, and unlike the keys above it is read
once when the panel is constructed — changing it needs a restart. A value of `0`
or a negative one resolves to the default rather than to an empty table, so
writing `0` is a legal way to say "unset".

## Error mapping

Every failure is an OpenAI-shaped envelope
(`{"error":{"message":…,"type":…,"code":…,"gateway_hint":…}}`). The reference
panel always names the failure symbolically in `error.code` and keeps the
vendor's own numeric business code inside `message` — it never promotes
`4008`/`14018`/`11102` to the wire code — and this gateway follows that:

| module returns | HTTP | `error.code` |
|---|---|---|
| `core.ErrNotConfigured` | 503 | `no_healthy_account` |
| `core.ErrUnsupported` | 400 | `invalid_request` |
| `core.ErrBusy` | 429 + `Retry-After: 1` | `rate_limit_exceeded` |
| context deadline | 504 | `upstream_timeout` |
| `FailureWAF` | 403 | `waf_ip_blocked` |
| `FailureRateLimited` | 429 | `rate_limit_exceeded` |
| `FailureQuota` | 429 | `quota_exhausted` |
| `FailureAuth` | 401 | `account_auth_failed` |
| `FailureSessionDead` | 401 | `session_dead` |
| `FailureContentBlocked` | 400 | `content_blocked` |
| a rejected inbound bearer | 401 | `invalid_api_key` |
| a local limitation (no streaming, 5xx) | 5xx | `server_error` |
| anything else | 502 | `upstream_error` |

A mid-stream failure is framed the same way: the stream breaking under us is
`upstream_parse`, and an `error` event the module classified carries its own
kind's code. `gateway_hint` is added only when the gateway has advice for that
failure; the vendor's raw text is passed through untouched, so a client that
wants the vendor code can still read it out of `message`.

Advice comes from two places, in order. A module that implements
`core.HintProvider` is asked first, because only it can explain its own vendor
business codes — `clients/workbuddy` reads the `11133`/`11135` image family and
its own `ErrKind` table there. An empty answer is a hand-off rather than a
verdict, so the gateway then falls back to the shared rule table in
`internal/hint`, which covers the codes that are common to several modules.
Either way the hint rides every error surface: the buffered envelope, and both
mid-stream SSE error frames (the stream has already answered `200`, so the frame
is the only place left to put it).

The context a module receives — bare model name, whether the request carried an
image, and what the catalogue says about that model's image support — is
resolved lazily. A request that succeeds never builds one, so it never pays for
the catalogue read that only an explanation needs. When the catalogue lists a
model without stating its image capability the gateway reports "not stated"
rather than "cannot see images", so a module never puts an invented capability
fact in front of a caller.

`core.ErrBusy` is backpressure, not a fault: every usable account is at its
in-flight ceiling, so the caller should retry rather than treat the route as
broken. `redis_mode` is reported as `"noop"` — this build keeps its state in
process (`internal/redisstore`'s no-op store is the supported single-machine
configuration), and the panel says so instead of implying a Redis mirror.

## Status of each module

| module | licence position | state |
|---|---|---|
| workbuddy | MIT upstream, ported | see `clients/workbuddy/README.md` |
| autoclaw | independent desktop platform; protocol and credential format observed from the client | see `clients/autoclaw/README.md` |
| trae | MIT upstream, ported | see `clients/trae/README.md` |
| zcode | AGPL upstream → **clean-room rewrite** | see `clients/zcode/README.md` |
| kimi | MIT upstream, ported | see `clients/kimi/README.md` |
| qoder | public OpenAPI + observed desktop credential format, no upstream code copied | see `clients/qoder/README.md` |
| qwenwork | **no licence** → **clean-room rewrite** | see `clients/qwenwork/README.md` |
| tabbit | GPL-3.0 upstream → **sidecar only, no code reuse** | see `clients/tabbit/README.md` |
| minimaxcode | **no upstream** → **clean-room rewrite** | see `clients/minimaxcode/README.md` |
| gemini | public Google OAuth / Cloud Code Assist protocol; repository embeds no Google OAuth client | see `clients/gemini/README.md` |
| cline | **clean-room** — protocol reconstructed from observed traffic | see `clients/cline/README.md` |
| lobsterai | **clean-room** — from a TypeScript integration plan for another project | see `clients/lobsterai/README.md` |
| codearts | MIT upstream, ported (wire protocol only) | see `clients/codearts/README.md` |
| loomy | MIT upstream → **clean-room rewrite** | see `clients/loomy/README.md` |
| raccoon | MIT upstream, ported (wire protocol only) | see `clients/raccoon/README.md` |
| openrouter | public REST API, no upstream code read | see `clients/openrouter/README.md` |
| opencode | public REST API; the gateway's own server source is open and was read | see `clients/opencode/README.md` |
| kilocode | public REST API, no upstream code read | see `clients/kilocode/README.md` |
| doubao | web API and desktop credential format observed from the client; module implements the wire flow itself | see `clients/doubao/README.md` |
| openai-compat | public REST API, no upstream code read | see `clients/openaicompat/README.md` |
| doubaoime | **clean-room** — Doubao Input Method ASR protocol reconstructed from observed traffic, no upstream code | see `clients/doubaoime/README.md` |

Provenance matters here: `zcode` and `qwenwork` are written from observed
protocol behaviour only, and `tabbit` never links GPL code. Each module's README
records what was read and what was written.

## The panel

<http://127.0.0.1:8788/panel/> (`/` redirects there). It is laid out like the
original WorkBuddy dashboard: a left sidebar carrying eleven views (Accounts / Chat test /
Usage / Alerts / Credits / Clients / Task Centre / Models and Profiles / Platform config /
Config / Runtime log), a sticky topbar with the page title, a theme switch
(dark -> light -> auto), one always-visible Refresh button, and Add account. Above the
account table sits a row of counters derived from the data — total / ready / cooling /
risk / account fault / disabled / clients / models — not a hardcoded number.

The topbar Refresh button is the single page-refresh entry point. It dispatches to the
current view, including Chat test and Runtime log, which previously had no refresh button
at all. Duplicate refresh buttons were removed from card headers; actions that really do
something different remain distinct, such as renewing credentials, refreshing balances,
re-fetching the model catalogue and restarting the process.

Each account row carries its own controls. Note records the phone number or
e-mail that account signs in with (see `account_notes` below). The enabled/disabled
control is state-aware: an enabled account shows Disable; any other state shows
Restore or Enable. Enable and revive are deliberately one button because the
operator's intent is the same, and the revive path also clears cooling and breaker
penalties when the module supports it. Re-login goes one step further and re-runs
the vendor's own login for that one row, overwriting the credential in place. The
re-login button only appears where it can help: a module that implements
`core.LoginProvider`/`core.SMSProvider`, and an account the module itself flagged
as dead (`fields.relogin`, or a state/note that says expired, invalid or
unauthorized). A module without login cannot offer one, and a healthy row stays clean.

The Add-account dialog carries an optional SMS-platform proxy beside the platform
token. It is stored in this browser only and is sent with each SMS-platform call;
the module then builds a request-scoped transport that overrides the proxy for
that call. Empty keeps the normal transport, and the override never touches the
vendor's own requests or the gateway-wide `proxy`. `http`, `https` and `socks5`
proxy URLs are accepted.

Row colour follows the account-level state summary, not the first channel's raw string.
Green is ready, amber is cooling with the vendor's reset time where available, and red is
either risk control or an account fault. Risk control is a vendor behaviour restriction
that re-login normally cannot fix; an account fault is an expired token or dead session,
where Re-login can help. Legacy `account_fault` rows are rendered as risk. The same
account-level rule feeds the status filter and the counters above the table.

Automation lives in one place: the Task Centre view owns the master switch, the
per-platform schedule tabs, the recovery and daily-balance controls, and the run
journal. The Config view no longer duplicates those controls, so `schedule.*` has
a single editing surface. The separate global-default editor has been removed:
rows with no override inherit the shared schedule and show it as a placeholder, while
edited rows gain a Custom badge and share the Save & Apply action at the top right.
Switching platform tabs preserves unsaved edits, and the dirty indicator stays visible.

The Usage overview subtab reports the window through a row of metric tiles rather
than a stack of identical grey rows: success rate / input / output / total / cache hit rate / total cost / average latency each
carry their own accent and a subtitle that says where the number came from (the
failure count under the success rate, each side's share of the total under the
token counts, the average rate under the total, the window under the latency) —
so a glance says which figure is the one being looked for. Cache hit rate is the
cached prompt tokens divided by prompt tokens, counting only requests whose
upstream actually reported the cache fields; older buckets that never reported
them are excluded rather than counted as misses. An unreported or zero prompt
total renders `—`. Total cost is the CNY sum of the per-call prices. An unknown
price leaves the total marked unknown instead of silently adding zero. The chart under them
is interactive: hovering a column reports that hour's input / output / request
count in a readout above the plot, and the arrow keys walk the same points
(Home / End jump to the ends). The column hit area is wider than the column
itself, because at a 72-hour window the columns are three or four pixels wide
with empty gaps between them. The reveal animation and the success-rate meter
both stand down under `prefers-reduced-motion`.

The Scheduled Tasks subtab now groups rules by platform instead of flattening every
platform into one long table. Each platform tab shows only its own batches, with the
effective hours in the input and the inherited shared value in the placeholder. The
Custom badge and Follow-global action appear only on rows that actually override the
shared schedule; both are added and removed as the row is edited. The hourly syntax
explainer sits with the table toolbar it describes, and changes are submitted through
the one Save & Apply button.

The panel is an administrative surface: it holds credentials and can rewrite the
config, so every response it produces carries a policy, including the shell and
the 401s. `Content-Security-Policy` starts from `default-src 'none'` and names the
inline script **by hash** (`script-src 'self' 'sha256-…'`) rather than by
`'unsafe-inline'`, so a script injected into the page cannot run even though the
page legitimately contains one; `frame-ancestors 'none'` and `base-uri 'none'`
block framing and `<base>` injection, `connect-src 'self'` confines `fetch` to
this origin, and `form-action 'none'` means nothing here can be POSTed off-box.
Alongside it: `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY` for
browsers that predate `frame-ancestors`, `Referrer-Policy: no-referrer`,
`Cross-Origin-Opener-Policy` and `Cross-Origin-Resource-Policy: same-origin`. The
hash is computed once from the embedded shell at startup
(`internal/panel/security.go`); if the shell's shape ever changes so the script
block cannot be located, the policy falls back to `'unsafe-inline'` rather than
leaving a blank page — a readable panel beats a blank one, and the other five
headers are unaffected either way. (The reference splits its JavaScript into a
separate `app.js` for the same reason; hashing reaches the same place without a
build step or moving 140 KB of embedded assets.)

**Chat test** sends a real request through the gateway, not through a panel-shaped
shortcut: it calls `POST /v1/chat/completions` from the browser (the panel handler
is mounted on the same mux as `/v1/`, so there is no CORS hop) and prints the raw
status code, elapsed time, time-to-first-token, `usage`, `finish_reason`, and the
gateway's `error.code` rather than a friendly paraphrase. It can pin the request
to one account — the account dropdown is only enabled for modules that implement
`core.ConversationBinder`, and the tab binds a fresh key through
`POST …/clients/<name>/conversations` before sending `options.conversation_id`.
Modules without a stickiness table say so in the dropdown instead of offering a
choice that would be ignored.

Backpressure and pool state are shown where a module actually has them. The counter
row carries **Sticky sessions** (the summed `pool.sticky_sessions` across the modules that
report a pool), the account table has an **In flight** column per account
(`in_flight / in_flight_limit`; `∞` when the module reports no ceiling, `—` when it
reports no lease at all), and the sidebar's Redis tile reads
`N clients / M ready · local memory` (`· Redis mirror` when the gateway is mirrored into
Upstash). Beside the account table the annotation appends the module's own reason
when `health.servable` is false (`unservable: <note>`) and `N accounts at their in-flight ceiling` when that
client currently has every usable account at its ceiling. The two extra keys come
from `GET /panel/api/status`, because neither `overview` nor the account records
carry `pool`/`health`/`extra`. A module that implements neither interface shows
nothing there: a missing key is rendered as blank, never as `0`.

Every chat request also prints one aligned row to the console, and the very same
line is mirrored into the **Runtime log** view through the gateway's log ring:

```
| #014 | 02:12:20 | workbuddy/glm-5.2          | stream | 200 | work(5f4a1c2e)         | TTFB=812ms    | tok=486    | 61.5tok/s   | total=7.9s |
```

Read left to right: the sequence number, the wall clock, the resolved
`<client>/<model>`, the transport (`stream` or `sync`), the status the caller
actually received, the account label (`nick(uid8)`, or `uid8` alone when the
module knows no nickname), the time to the first upstream frame, the completion
tokens the upstream reported, the derived tok/s, and the total wall time. A
request that never reached an upstream still gets a row — that is what keeps a
rejected or malformed request visible — and a module that fails names the
account it tried. The token figure is whatever the module's `usage` carried, so
`-` means the upstream reported nothing rather than zero. Padding is measured in
display columns rather than bytes (`internal/logfmt`), so a Chinese nickname
does not shift the rest of the row. Set `SetChatLogOutput` at startup (the
binary points it at the same `io.MultiWriter` as the logger) and stdout and the
panel's log view stay in sync; tests silence the rows through `TestMain`.

Because chat rows are written on every request, they would otherwise flush every
scheduled chore's result out of the ring before anyone could read it. The ring
therefore tags each line with a **channel** (`gateway.LogEntry{ts, ch, text}`) and
the log view filters on it: Tasks / Chats / System chips over the same buffer, with the
per-channel counts shown in the all view and the tag column suppressed once a
single channel is selected (every row would carry the same tag). Classification is
a prefix match on the line as it is written — `| #` is a chat row, the chore verbs
(`checkin`, `activity`, `streak-bonus`, `travel`, `blackcat`, `lottery`,
`keepalive`, `balance`, `school`, `user-resource`, `panel: …`) are tasks,
everything else is system — and every prefix carries its own trailing space so
`checking something unrelated` stays a system line. The timestamp on each entry is
the line's own, parsed from the stamp the logger already wrote, not the moment the
ring stored it. `Lines()` remains as a projection over `Entries()` for callers
that only want text.

The account in that column — and the account a request is filed under in the
usage ledger — comes from the module, not from the gateway. `core.ChatRequest`
carries a per-request `ServedBy *string` slot that the gateway allocates and
reads exactly once after `Chat` returns; a module calls `core.NoteServedBy` at
the point it settles on a credential, so a module that rotates names each
attempt and the last writer wins. The gateway cannot see inside a module's pool,
and a failure is the only case where it can learn an account from the error
alone — so before this slot existed every *successful* request was recorded with
an empty account, which showed as a blank label in the row above and collapsed
the whole by-account table into a single `(unrouted)` row. A failure still prefers the
account its own error names and falls back to the slot only when the error names
none. A module that never writes the slot is not wrong, merely unattributed.

**Usage → Recent calls** is the per-request journal (`recent.json`), which
`GET /panel/api/usage` returns as `recent`. Each priced row also carries cached prompt tokens, that row's cache hit rate, and its CNY cost; an unknown price renders `—`, while an explicitly free model renders `¥0`. Each row carries the caller's own
**session id**: the conversation id the gateway already resolved
(`metadata.conversation_id` / `conversationId`, or the top-level spelling), then
the option spellings the router also accepts (`conversation_id`,
`conversationId`, `prompt_cache_key`), and only when the caller named none of
those the `X-Conversation-Request-ID` header. The request's `user` field is
deliberately never used: an end-user id is not a conversation, and writing it
into a column called Session ID would claim a session the caller never named. The
row falls back, when the caller named none of them, to `core.DeriveConversationKey` —
the same content-derived key the router uses for stickiness — so a conversation that
arrived without an id reads as one conversation instead of a run of blank cells. That
key carries the `d-` prefix (`core.DerivedKeyPrefix`) and the panel renders such a value
as `derived …`, so an inferred session is never mistaken for one the client declared; a
request with nothing signable to derive from still shows No session ID. The derived key
never enters `ChatRequest.ConversationID` or `Options`, so routing is unchanged. The
account column resolves the recorded account id the way the Accounts page does: the
operator's own note (the phone number or e-mail kept for a re-login) wins, then
the module's label, and the raw id is the last resort so a row never goes blank
after an account is deleted. The hover title keeps the raw id, the module label
and the vendor identity, so a label that only names the credential ("ZCode plan
JWT") is still traceable to the account behind the channel.
A **thinking effort** column records the level the caller asked for. The relay
spelling (`reasoning_effort`), the Responses-shaped `reasoning.effort`, and the
`client2api.reasoning_effort` option are all accepted and normalised to lower
case; a request that names no level stays visibly empty instead of borrowing a
module default.

**Usage** is a real chart rather than a row of bars. `GET /panel/api/usage` returns
`series` — one `UsagePoint` per bucket, carrying `t`, `scope` (`hour` or `day`),
`prompt_tokens`, `completion_tokens`, `requests`, `failures` and `total_tokens` —
and the view draws it as inline SVG: stacked prompt/completion columns, four
gridlines with `fmtTok`-formatted ticks, a baseline drawn *after* the columns so
it cannot sit on top of their feet, and at most six x labels placed by taking the
real column nearest each evenly spaced position, so a label always lands on a
point that has data. Points whose timestamp will not parse are dropped whole
rather than coerced to `NaN`, which would otherwise flatten the entire chart, and
the label format is chosen per point, because a window that reaches past the
rollup boundary mixes `hour` and `day` buckets in one series.

**Credit composition** answers a different question — not "how much have I used", but "how
long is what is left good for". It takes every credit batch the module reports,
subtracts each batch from its account's own remaining total (the upstream records
the same grant under more than one batch, so summing the batches would overstate
the balance), and stacks what survives onto one row per exact remaining-days
value, with a segment per account in a stable per-account colour. A batch with no
parseable expiry is counted in the footer and left out of the chart: a guessed
expiry date is worse than an absent one. Segment opacity falls off with the
remaining days, so the rows that are about to expire are the ones that read as
solid. Both views are drawn from data the panel already had — the gateway did not
have to learn anything new.

It is not a read-only dashboard. Every module that can manage its own
credentials gets an **accounts** section (list / add / enable / disable / test /
delete), a **discover + import** section when it can read credentials already on
this machine, and a **check-in** section when the platform really hands out a
daily reward.

The list is a list of **credentials**, and one vendor account can legitimately
appear as several of them: a desktop client's provider config, a plan JWT beside
it, and a sign-in made from this panel can all be the same person. So a module
may put the vendor's own account number in `AccountRecord.Identity`, and the
panel then draws **one row per account**, with the individual channels nested
underneath it — each channel still separately testable, enableable and
deletable, and the account's own state taken from whichever channel is healthy.
The group header carries no buttons on purpose: a bulk action would merge two
credentials' states, which is the confusion the grouping exists to remove. A
record with **no** identity is never merged with another — "the module cannot
say" is a different statement from "these are the same account". Modules that
report no identity (which is most of them) render exactly as before. The
account and channel counts both appear in the view header, and the sidebar
badge counts accounts, not credentials.

A pool of a few dozen accounts is taller than any screen, so the table pages.
The default is 50 accounts per page with 20/50/100/200 selectable, and the
pager collapses entirely when everything fits on one page — a control that can
never do anything reads as a broken one. Two details matter:

- **Paging cuts between groups, never inside one.** The unit is the account
  group, so a multi-channel account keeps its header and all of its channel
  rows on the same page. Slicing the credential rows instead would make one
  vendor account look like two accounts.
- **The counts stay whole.** The header still reports the full pool, and the
  pager adds `N-M of X accounts`; both count the filtered list, not the
  current page. Paging is for the eye, the numbers still answer "how many
  accounts do I manage".

Switching client, typing in the search box, and changing the page size all
return to page 1: a page number that was meaningful for the previous set has no
meaning for the new one, and a search that narrows 138 accounts to 5 would
otherwise leave the operator staring at an empty table.

The same toolbar carries a state filter whose buckets are the account-level states used
by the table chips and the counters: all, ready, cooling, risk, account fault, disabled
and unknown. Filtering is client-side and never re-requests the upstream, and the result
line reports how many accounts remain. Ten columns can be sorted: account, state, model,
priority, balance, success/failure, in-flight, usage, last success and expiry. The cycle
is ascending, descending, then default; columns with a natural high-value-first order such
as balance and usage start descending, while priority starts ascending so the most
preferred account is first. Missing values always sort last. A background poll re-renders
in place without resetting the page or scroll position; only an explicit sort, client
switch, search or page-size change returns to page 1.

Discovering credentials on this machine is not the only way in. A module can also
accept a **document the operator supplies** — an export produced by another tool
— by implementing `core.BundleImporter` instead of (or as well as)
`core.CredentialImporter`; the panel then renders an upload control behind the
separate `capabilities.import_bundle` flag. The panel never parses the document:
whose schema it is, is the module's business, and the panel only forwards the
bytes and reports back the module's per-row verdicts (`imported` / `skipped` /
`errors`). `workbuddy` is the one module that does this today, reading the
cockpit export's JSON array. The two mechanisms stay separate because they answer
different questions — `Import` walks files the module found itself, `ImportBundle`
takes a file the operator chose — and a module that can do one cannot necessarily
do the other.

A module that can drive the vendor's **growth centre** — the daily chores the
official client performs on the user's behalf — additionally gets a **task
board** (Task Centre). It lists each chore with its real progress and reward, runs
one on demand, and offers an *execute everything automatable* queue. Runs are
asynchronous (the panel returns a run id and polls), and the queue advances
**serially**: the vendors' anti-abuse checks roll back chores whose events are
bursting only seconds apart, so pace is treated as part of the protocol.

The board remembers each account's last task list in
`data/panel/task_board_cache.json`. Reopening the board renders that cache and
only fetches accounts with no entry; **Refresh task state** forces a fresh
platform scan. An account card turns green once it has no actionable automated
chore left (everything claimed, or only human/locked entries remain).

Four modules publish a board: `workbuddy` (the reference's whole growth centre),
`zcode`, `minimaxcode` and `loomy` (its first-login onboarding checklist).
`codearts` is the one module whose vendor *does* ship a conversation-task
endpoint and which this repo still answers `no` for: the reference's board lives
on a different origin (`policyCenterDomain`) from every other endpoint the module
uses, its response fields are not derivable from the binary, and the vendor's own
renderer never calls it either — so there is nothing honest to implement from.

| module | manage | import | login | check-in |
|---|---|---|---|---|
| workbuddy | yes | yes | yes — polls `/v2/plugin/auth/state` → `/token` and opens `copilot.tencent.com/login` | yes — `daily-checkin` (CN) and `daily-activity` (intl) |
| autoclaw | yes | yes — Electron safeStorage/DPAPI desktop import, JSON and bundle import | yes — loopback phone + SMS-code login; plus one-click SMS auto-login the module runs itself (`core.AutoLoginProvider` + `core.SMSProvider`, platform project 智谱华章) | yes — daily points claim |
| trae | yes | yes | yes — loopback redirect on `127.0.0.1`, opens `www.trae.cn/authorization` | yes — `daily-checkin` (CN only) |
| zcode | yes | yes | yes — polls `POST {api}/oauth/cli/init` → `GET …/poll/{flow_id}` | yes — claims the promotion via the built-in Edge/Chrome captcha solver; `captcha_command` is the headless fallback |
| kimi | yes | yes | yes — RFC 8628 device grant against `auth.kimi.com`; the CLI is not required | no — no such endpoint exists |
| qoder | yes | yes — Electron safeStorage + DPAPI on Windows; a pasted token elsewhere | yes — browser device grant + PKCE; plus one-click SMS auto-login through Aliyun SSO (`core.AutoLoginProvider` + `core.SMSProvider`) | yes — the daily `CLAIM_BENEFIT` campaign; also a real campaign task board, package breakdown, pool health and model output limits |
| qwenwork | yes | no — nothing on disk holds a usable token | yes — PKCE device flow | yes — `daily`, against the Sash check-in API |
| tabbit | yes | yes | yes — browser hand-off: opens the sign-in page inside the Tabbit browser through its own launcher, then reads the session cookie back (`local_app` + `handoff_path`; with no launcher it falls back to returning the URL) | yes — daily `sign-in` for the `desktop_pet` scene (normally 3% credit), with a fresh `request_no` idempotency key |
| minimaxcode | yes | yes | yes — RFC 8628 device grant against `account.minimax.cn`, PKCE, with the CN `user_code` variant; the desktop GUI is not required | yes — the daily sign-in board and credits; sign-in also completes the first-run account onboarding gate |
| gemini | yes | yes — Google OAuth token discovery and import | yes — Google OAuth loopback callback | no — quota is a rolling 5-hour/weekly window, not a check-in |
| cline | yes | yes | yes — WorkOS device-code flow, the same client id the desktop app uses | no — no such endpoint exists |
| lobsterai | yes | yes | yes — browser hand-off; the module returns the URL and watches for the credential it leaves behind | yes — a daily check-in action |
| codearts | yes | no — the IDE plugin keeps no discoverable credential file | yes — the portal's own sign-in, driven as a pending session | yes — the daily check-in |
| loomy | yes | no — nothing on disk holds a usable session | yes — one-click SMS login the module runs itself (`core.AutoLoginProvider` + `core.SMSProvider`): the panel rents a number, the module sends the code, reads it back and stores the 14-day session | yes — the first-login allowance |
| raccoon | yes | yes | yes — loopback QR/SMS page with a stdlib QR encoder; WeChat scan or SMS, phone encrypted host-side | yes — `login-points`, the desktop login grant that extends the daily 300 credits; the vendor grants it to its own desktop app only, so a gateway call settles as "unavailable" |
| opencode | yes | yes — the `opencode` entry of `~/.local/share/opencode/auth.json`, plus every `*.json` in `<data_dir>/import/` | yes — two realms: anonymous free (`x-api-key: public`) and Console device-code OAuth | no — Zen has no check-in and no task API |
| openrouter | yes | yes — `OPENROUTER_API_KEY`, the conventional credential files, an `env:NAME` reference, or a pasted key | yes — browser PKCE; the issued key is stored like a pasted one | no — the vendor has no check-in |
| kilocode | yes | yes — API key, `KILO_API_KEY`, Kilo CLI/Desktop sign-in file, or the built-in anonymous credential | yes — device authorisation; API keys and anonymous free access are also supported | no — no check-in, balance, or task API |
| doubao | yes | yes — read-only desktop-client cookie import | yes — controlled browser web login | no — check-in, task centre, SMS/auto login are not implemented |
| openai-compat | yes | no — a bare API key is pasted, not discovered on disk | no — the operator pastes one key per provider | no — a generic OpenAI-shaped endpoint has no check-in |

The first-generation modules include six that expose a login and were verified against
the live vendor endpoints from a running gateway, not just from tests: each
`POST /panel/api/clients/<name>/login` returns a real `state: "pending"` with a
working browser URL (`kimi` also returns the user code to confirm). `tabbit` is
the one whose URL only works inside the Tabbit browser: the module opens the page
there through that browser's own launcher and answers with `local_app` plus the
credential path to import afterwards, so the panel shows a Read credential button instead
of a link a normal browser cannot use. `minimaxcode` joined them with an
RFC 8628 device grant recovered from its desktop bundle; unlike the six above it
has only been exercised against a scripted transport, so a real sign-in from
this machine is still the open item.

The later modules are a different story, and the table above says only what
their code implements. Not every one has been exercised against a live account:
for several there is no account or API key on this machine, so wire facts in
their READMEs are derived from a reference implementation or the vendor's own
traffic and are pinned by fixtures rather than a real session. Their READMEs
mark those claims `[unverified]` rather than asserting them. The router-style
modules also have hermetic-tested login flows, but no live vendor sign-in has
been completed from this machine, so a real QR scan, PKCE redirect or device-code
authorisation remains an open item. The unauthenticated catalogue probes for the
router-style backends were run live, because listing models does not require a
key.

The capability flags are derived by type assertion on the registered client
(`core.CapabilitiesOf`), so a module that does not implement an interface simply
does not get that section. Nothing in the core changes when a module gains or
loses one. `docs/MODULE-CONTRACT.md` §6 states the rules a panel implementation
must follow; the short version is *implement only what you can really do, a
refusal is a result and not an error, never write the main config, never return
a secret*.

Panel state stays inside each module's own `data/<client>/` directory. The
`clients.<name>` blocks in `configs/client2api.json` are operator-owned and the
panel never edits them.

**Balances** are cached, not live. The accounts page's balance column is served
from `data/panel/balance_cache.json` — the last number each vendor gave for
each account — so opening the page shows the previous values immediately and
costs no upstream calls. A page open also schedules one background pass over
the **3** accounts whose cached value is oldest (spaced 2s apart), throttled to
one pass per 45s; the explicit `POST …/balances/refresh` schedules the same
pass at a 5s throttle. Rows that have never been read render `-` rather than
`0`, because zero is a real balance. The per-account
`POST …/accounts/<id>/balance` button is still a live vendor read, and it
updates the cache too. The scheduled `schedule.balance_refresh_minutes` sweep
also refills the cache, so the column converges without anyone pressing a
button. The file lives under `data/`, which an installer upgrade preserves.

**Check-in** is per-account: each `core.CheckinAction` the module advertises
becomes its own button, so workbuddy shows two (the two realms are two different
rewards) while trae, tabbit, zcode, minimaxcode, qwenwork and raccoon show one. The
button on an account *row* is driven by a second, live flag (`checkin_ready`)
rather than by the module-level capability bit, so a module whose accounts cannot
claim anything at the moment does not offer a click that could only fail; the
capability matrix and the bulk routes keep reading the static bit.
"Check in all" walks the enabled accounts one at a
time — deliberately serial, so a burst of upstream calls cannot look like abuse.
An already-claimed day reports success, because the operator's question is "is
today's reward in hand?".

**One-click local sources** are the shortcut past an add form for a service that
needs no credential of its own. A module that implements
`core.QuickConnectProvider` advertises targets (rendered as `quick_connect` in
its capabilities); the panel probes one read-only and, on the operator's click,
calls `POST …/quick-connect/<id>/connect`, which creates the source through the
same `AddAccount` path a manual row takes, then tests it. `openai-compat` is
the first user: it offers OmniRoute at `http://127.0.0.1:20128/v1`, model
`auto`, no API key. A loopback `base_url` is the only place an empty key is
accepted, and the module never installs or supervises the service it points at
— a probe that finds nothing says so rather than creating a dead row.

**Sign-in** is browser-first. The panel calls `POST /login`, shows the returned
URL (and, when the flow has one, a user code the operator confirms), then polls
`GET /login/<session>` on a timer until the state goes terminal. No module asks
the operator to install a vendor CLI to sign in: kimi, qwenwork, zcode, cline
and minimaxcode are pure device/poll flows, trae runs a temporary loopback
listener to catch its OAuth callback, workbuddy polls the vendor's own state
endpoint, and tabbit opens its sign-in page in the Tabbit browser itself and asks
the panel to import the cookie afterwards (falling back to returning the URL when
no launcher is installed); `raccoon` serves a loopback QR/SMS page,
`openrouter` catches a PKCE callback on a loopback listener, and `opencode`
offers an anonymous free credential or a device-code sign-in. A module that
cannot sign in must say so instead of showing a button — the panel renders that
button from the module's own `login` capability, so a refusal never has to be
faked.

**Model catalogue** is not a static list. A module that can ask its vendor
implements `core.ModelRefresher`:

```go
RefreshModels(ctx context.Context) ([]Model, error)
```

`GET /panel/api/models` renders one row per model with its client, and marks a
row upstream only when the module reported it from a live fetch; every other row (a
module's own built-in catalogue, an operator-configured list, the metadata
table) is marked as local and names where it came from. `POST
/panel/api/models/refresh` is the only path that re-asks the vendor, and it does
so **only** for modules that implement the interface (`can_refresh` in the
response tells the truth for each one). `Models()` itself keeps a TTL cache so a
panel refresh does not hammer the vendor. A failed refresh must never *fabricate*
a catalogue: it returns the last good list, or the module's own built-in list
where it has one, or the operator's configured list, or nothing — workbuddy,
which has no built-in list, answers empty rather than inventing ids.

The models view carries a multiplier column, because a credit multiplier is the one
per-model fact an operator actually shops on and it has no home in the OpenAI
shape. `core.Model.Extra` travels through both surfaces: `GET /v1/models` emits
it as an `extra` object on the entry (omitted entirely when the module published
none, so a plain catalogue stays byte-for-byte OpenAI), and `/panel/api/models`
passes the same map through. The column reads, in order: an in-force campaign
(`promo_credits` with `promo_label` and the struck-through list price, the
campaign's tooltip), then a badge-only campaign (`promo_label` beside the list
price), then the list price (`credits`, with Raccoon/TRAE/Qoder variants folded
in), then a bare multiplier (`display_multiplier`, `cost_multiplier`, or the
multiplier embedded in a display name). A numeric zero is rendered as `免费`,
not `x0`; `free_unlimited` is free, while `free_metered` is not treated as a
multiplier. A model with none of them renders `—`. What each module puts in
`Extra` is its own business — see the per-client READMEs. Platforms with no
vendor multiplier field (CodeArts, Kimi, Gemini, Doubao, ZCode, Cline, Kilo,
OpenCode, OpenRouter) deliberately stay blank rather than substituting price or
a free flag for a rate.

Every `/v1/models` entry also carries a resolved `context_length` and
`max_output_tokens`, and the same two numbers are mirrored into `extra`. The
resolution order is operator override > the value the module reported > the
official preset in `internal/modelmeta/table.json`; an unknown field is omitted
rather than guessed. This is what lets an OpenAI-compatible client (Codex, Claude
Code, …) size its context from the catalogue instead of falling back to a tiny
built-in default when a vendor reports nothing. When a chat request arrives with
no `max_tokens` / `max_completion_tokens`, the gateway fills the same resolved
output cap; a caller that supplied its own number is never second-guessed.

The Models and Archive page turns context, output, and price fields into number inputs, labels each field's
source (Manual / Upstream / Preset / Not public), and saves through
`POST /panel/api/model_context`. Manual values live in
`<data_dir>/model_context.json` (written atomically, `0600`), survive an upgrade
with the rest of `data/`, and are removed — falling back to the upstream value or
the preset — when the field is cleared and saved, or when the row's
Restore official default button is used. Prices are shown in CNY per million
tokens. Their resolution order is operator override > the value the module
reported > a built-in vendor-published price. The official price layer covers
DeepSeek, Zhipu/BigModel, Kimi, MiniMax, Alibaba Model Studio,
Volcengine/Doubao, Tencent Hunyuan, Xiaomi MiMo, Anthropic, Google, and OpenAI;
USD-only list prices are converted at USD/CNY 7.20. Tiered models show their
standard lowest tier in the single-value model list. Remaining prices fall
back to the models.dev snapshot dated 2026-10-10 in
`internal/modelmeta/pricing.json`, also converted at USD/CNY 7.20. DeepSeek
Flash and Pro use the official CNY list prices, including the Beijing-time
peak/off-peak schedule. The model list shows DeepSeek's off-peak price; a
completed request is priced using the rate in force at its completion time.
When the catalogue publishes a
cache-read price, cached prompt tokens use it; otherwise they use the ordinary
input price. An unknown price is rendered `—`, not `¥0`; only an explicitly
free model is zero-cost.

The same page edits each model's supported reasoning-effort levels, its default
level, and a per-model **cap-alignment** toggle. Effort resolution is operator
override > the module's cached catalogue > the embedded preset, so a manual
vocabulary wins over a stale cache. Cap alignment defaults on for every model
on every platform; when it is on and the caller requests the top Codex level
`xhigh`, the gateway promotes it only to a genuinely higher level the model
advertises (for example WorkBuddy, Cline and OpenRouter `max`). Lower requests
and models whose top level is already `xhigh` (Loomy's V4.1 Flash, whose vendor
catalog advertises nothing above it) are left alone rather than sent an invented
value. A model with no published effort ladder is also safe: the default is on,
but there is no higher level to send. The usage row keeps the requested and sent
levels and renders `xhigh → max` when they differ.

The panel is protected by the gateway's top-level `api_key`
(`configs/client2api.json:3`). Empty — the default — leaves it unauthenticated,
which is a supported single-operator deployment. When it is set, every
`/panel/api/*` route requires it as `Authorization: Bearer <key>` or
`X-Api-Key: <key>`; an unauthenticated request gets `401` with
`WWW-Authenticate`, so a browser prompts instead of showing a JSON error. The key
is read through the live holder, which `POST /panel/api/reload` refreshes from
the config file — so a key rotated on the config page is enforced on the next
request after a reload, with no process restart. (The config page's own patch
response still reports `restart_required: true`, because the sections it can also
touch — `listen`, `data_dir`, the per-client blocks — genuinely need one.) The
HTML shell is deliberately *not* gated: the page has to load before it can offer
a key. The original
`dashboard.html`'s client-side password screen (`admin`) is not reproduced — a
password check that runs in the browser only pretends to protect something.

Endpoints, all scoped per client:

```
GET    /panel/api/status
GET    /panel/api/clients
POST   /panel/api/reload
GET    /panel/api/overview
GET    /panel/api/logs?lines=N                           entries carry {ts,ch,text}
GET    /panel/api/models
POST   /panel/api/models/refresh
GET    /panel/api/exposed_models                         the final catalogue plus the selected whitelist
POST   /panel/api/route_plan                             {"model":"…","catalogs":{…}}; static only
POST   /panel/api/route_speed                            same body; one live request per platform
GET    /panel/api/route_live?limit=N&window=HOURS        recent requests, each with the candidates it tried
GET    /panel/api/model_probes                          (optional; the probe file)
GET    /panel/api/usage?window=HOURS
POST   /panel/api/usage/save
GET    /panel/api/config
POST   /panel/api/config                                {"patch":{…}}
GET    /panel/api/clients/<name>/capabilities
GET    /panel/api/clients/<name>/accounts
POST   /panel/api/clients/<name>/accounts              {"fields":{…}}
DELETE /panel/api/clients/<name>/accounts/<id>
POST   /panel/api/clients/<name>/accounts/<id>/enabled {"enabled":bool}
POST   /panel/api/clients/<name>/accounts/<id>/test
POST   /panel/api/clients/<name>/accounts/<id>/checkin {"action":"…",
                                                      "captcha_param":"…"?,
                                                      "captcha_region":"…"?} (optional)
GET    /panel/api/clients/<name>/captcha?action=…       (optional; the browser scene)
POST   /panel/api/clients/<name>/accounts/<id>/revive   (optional)
POST   /panel/api/clients/<name>/accounts/<id>/balance  (optional; one vendor call)
GET    /panel/api/clients/<name>/balances               (optional; last-known
                                                        balances from the cache,
                                                        never a vendor call; a read
                                                        does schedule one paced
                                                        batch)
POST   /panel/api/clients/<name>/balances/refresh       (optional; starts one
                                                        paced batch of at most 3
                                                        accounts and answers
                                                        immediately)
GET    /panel/api/clients/<name>/accounts/<id>/tasks    (optional)
POST   /panel/api/clients/<name>/accounts/<id>/tasks/accept     {"task_codes":[…]} (optional)
POST   /panel/api/clients/<name>/accounts/<id>/tasks/accept_all (optional)
POST   /panel/api/clients/<name>/accounts/<id>/tasks/claim      {"task_code":"…"} (optional)
POST   /panel/api/clients/<name>/accounts/<id>/tasks/auto       {"task_code":"…"} (optional)
POST   /panel/api/clients/<name>/accounts/<id>/tasks/auto_all   (optional)
GET    /panel/api/clients/<name>/packages               (optional)
GET    /panel/api/clients/<name>/school/vouchers        (optional)
GET    /panel/api/clients/<name>/conversations?key=…    (optional; reads back a pinned key)
POST   /panel/api/clients/<name>/conversations          {"account":"…","key":"…"?,"model":"…"?} (optional)
POST   /panel/api/clients/<name>/conversations/unbind   {"key":"…"} (optional)
GET    /panel/api/clients/<name>/tasks?account=<id>      (optional)
POST   /panel/api/clients/<name>/tasks/<code>/run       {"account":"…"} (optional)
GET    /panel/api/clients/<name>/tasks/runs/<id>        (optional)
POST   /panel/api/clients/<name>/tasks/scan_all         (optional)
GET    /panel/api/clients/<name>/tasks/queue            (optional)
POST   /panel/api/clients/<name>/tasks/run_queue        (optional)
GET    /panel/api/clients/<name>/batches                (optional)
POST   /panel/api/clients/<name>/batches/<batch>/run    (optional)
GET    /panel/api/clients/<name>/batches/queue          (optional)
GET    /panel/api/clients/<name>/batches/runs/<id>      (optional)
POST   /panel/api/clients/<name>/<verb>_all             (optional; one-click aliases:
                                                        checkin_all, travel_all,
                                                        activity_all, keepalive_all,
                                                        blackcat_all, growth_all —
                                                        each starts the matching
                                                        sweep, and a module that
                                                        declares no such batch
                                                        answers 501)
POST   /panel/api/clients/<name>/balance_all            (optional; refreshes every
                                                        account's balance from the
                                                        upstream at once. It is not
                                                        a batch — no module plans
                                                        one — so it goes straight to
                                                        the scheduler's fleet-wide
                                                        sweep, and the <name> in the
                                                        path is ignored. 501 when
                                                        there is no scheduler, or
                                                        when its host never wired a
                                                        balance hook)
POST   /panel/api/clients/<name>/accounts/refresh      {"id":"…"} or {}
GET    /panel/api/clients/<name>/discover
POST   /panel/api/clients/<name>/import                {"paths":[…]} or {"all":true}
POST   /panel/api/clients/<name>/import/bundle         raw body, or multipart with a "file" field
POST   /panel/api/clients/<name>/login
GET    /panel/api/clients/<name>/login/<session>
DELETE /panel/api/clients/<name>/login/<session>
```

An account id may contain characters that need URL escaping — the panel escapes
them, and so must any other client of this API.

One route sits outside `/panel/api/` because it is a document, not JSON:

```
GET    /panel/captcha?action=…&scene_id=…&region=…&prefix=…   the captcha iframe
```

It is the only page the panel frames, and it carries its own
Content-Security-Policy (naming the vendor's script and verification origins,
with no `'self'` in `connect-src`) plus `X-Frame-Options: SAMEORIGIN`, because
the shell's `DENY` would render the dialog permanently blank. It is
unauthenticated like the shell itself, and holds no secret: every value it needs
arrives in its query string and is something the vendor hands to any browser. See
`clients/zcode/README.md` → "The browser captcha path" for the module side.

### Routes of the original that are served differently

Parity here means *behaviour*, not path-for-path copying: each of these three
routes exists to serve one vendor's data or sign-up flow, so the shared layer
serves them **per client** rather than hardcoding that vendor's schema or region
list.

| reference route | verdict |
|---|---|
| `GET /panel/api/login/regions` | **kept, but per client.** The route is `…/clients/<name>/login/regions` and answers with the realms *that module* offers — an empty array for a module whose upstream has only one sign-up. The reference's version was a static whitelist of seven registration regions for workbuddy's international sign-up, which is why it could not live in the shared layer. workbuddy publishes its own two realms (`cn` China, `global` international) through `core.RealmLoginProvider`, and the panel draws the picker from that response instead of a hardcoded list. |
| `POST /panel/api/import/cockpit` | **served per client as `POST …/clients/<name>/import/bundle`.** The reference's path named one vendor's export, and its body is that vendor's schema (`uid`/`access_token`/`refresh_token`/`domain`), so neither the path nor the parser belongs in the shared layer. A module that can read such a document implements `core.BundleImporter` and declares `capabilities.import_bundle`; `internal/panel` forwards the bytes and never parses them. `workbuddy` is the module that reads this particular format. |
| `GET /panel/api/school/vouchers` | **kept, but per client.** The route is `…/clients/<name>/school/vouchers` and answers `501` for every module that does not implement `core.VoucherProvider`; only workbuddy can, because the school activity is workbuddy's. |

Everything else in the reference's route table — `revive`, `balance`,
`packages`, the five task verbs, `usage/save`, `model_probes`, the batch sweeps —
is present, and each of them is opt-in per module: a module that does not
implement the interface behind a route answers `501` with a sentence naming what
it cannot do, instead of the panel hiding a capability that is actually there or
showing a button that lies.

## Probing output ceilings

`max_output_tokens` in a vendor's model list is a claim, not a measurement, and
the gateways behind these clients are often generous with it. The failure mode is
quiet: ask for 100000 output tokens, get 48000, and the response says
`finish_reason: "length"` with no error anywhere. The only way to know the real
ceiling is to ask for more than it and watch where it stops.

`cmd/probe` does that, and writes what it learns to `data/output_probes.json` —
the file `GET /panel/api/model_probes` serves and the panel annotates its model
table from. It is a separate binary because this tree has no script runtime: the
same reason the panel is a Go handler rather than the original's Node app.

```sh
go build -o probe ./cmd/probe
./probe --base http://127.0.0.1:8788/v1 --key "$CLIENT2API_KEY" --models glm-5.2 \
        --tiers 40000,100000 --panel-out data/output_probes.json
```

Point `--base` at this gateway rather than at a vendor. The number worth knowing
is the one *you* will get, after the namespace prefix, the pool, and whatever the
upstream does underneath.

Each rung is a request that asks the model to count to a target derived from the
rung, then reads `finish_reason` and `usage.completion_tokens` off the stream:

| what came back | what it means | recorded as |
|---|---|---|
| `length`, fewer tokens than requested | the upstream clamped it; the token count is the real ceiling | `clamped` with a measured value |
| `length`, exactly as many as requested | it supports at least this much; climb | `at_least` |
| `stop`, fewer than requested | the model decided it was done — this rung proves nothing | retried with firmer wording, then `at_least` or `inconclusive` |

A `stop` is why the retry exists: re-sending the same prompt to a model that just
ignored it is wasted tokens, so the second attempt switches wording. A rung whose
stream carries no `usage` block at all is *not* read as "zero tokens" — that would
look exactly like a clamp.

Writing the file **merges**: a run that retests two models keeps the records for
every other model in the file, so probing a newly added model does not throw away
last month's measurements. A corrupt file is rebuilt rather than being allowed to
block a run that has already spent the tokens, and the write is `tmp` + rename so
a panel query landing mid-run never reads half a document.

The file is optional. Without it the route answers `{"probes":{},"exists":false}`
and the panel simply shows no annotation; with it, the panel picks the change up
on the next request — nothing needs restarting. `--budget` caps the wall time per
model (the reference measured one model that needed 720 s to emit 40000 tokens, so
an unbounded ladder can spend half an hour on a single rung), `--resume` skips
models already recorded in `--out`, and `--dry-run` lists what would be probed.
