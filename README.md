# client2api

[English](README.md) | [简体中文](README.zh-CN.md) | [使用教程](TUTORIAL.zh-CN.md)

[![build](https://github.com/Cvencent/client2api/actions/workflows/go-binaries.yml/badge.svg)](https://github.com/Cvencent/client2api/actions/workflows/go-binaries.yml)
[![release](https://img.shields.io/github/v/release/Cvencent/client2api?include_prereleases)](https://github.com/Cvencent/client2api/releases)

Current version: **0.1.43**. Full changelog: [CHANGELOG.md](CHANGELOG.md).

client2api is a personal, local-only OpenAI-compatible gateway. It aggregates
the accounts and API Keys **you own** on different AI coding clients and
platforms, then exposes one uniform OpenAI-style API plus a web control panel.

This is a single-user project for personal use and learning. It is not a public
service, not a shared relay, and not a server-hosting solution. Run it on your
own machine and do not expose it to the internet or use it to serve other
people. The built-in default is `127.0.0.1`.

> **Personal use only.** You are responsible for following every upstream
> platform's terms, account rules and API policy. Do not use client2api to
> bypass quotas, resell access, or operate a hosted proxy service.

![Accounts panel](docs/images/accounts.png)

## What It Does

- Aggregates your own accounts across supported platforms into one account pool.
- Serves `POST /v1/chat/completions` and `POST /v1/responses` with one inbound API Key.
- Serves `POST /v1/audio/transcriptions` for platforms that expose speech, so a
  WAV/FLAC/MP3/MP4/M4A/OGG/WebM upload comes back as `{"text": "..."}` (or
  text, SRT, verbose JSON, VTT).
- Routes by platform, account, model, alias, `Auto/<model>` or custom model groups.
- Keeps one conversation on the same account where the platform supports sticky sessions.
- Handles account priority, cooldown, concurrency limits, retries and failover.
- Provides a complete web panel for accounts, usage, tasks, models, credits, logs and configuration.
- Supports Windows installers, portable release bundles and local source builds.
- Keeps all credentials and runtime state in the local `data/` directory.

## Supported Platforms

| Route | Client / platform | Upstream |
|---|---|---|
| `workbuddy/...` | WorkBuddy | Tencent WorkBuddy / CodeBuddy |
| `autoclaw/...` | AutoClaw | AutoClaw desktop (independent platform) |
| `trae/...` | Trae | TRAE SOLO / Trae CN |
| `zcode/...` | ZCode | Z.AI coding plan |
| `kimi/...` | Kimi | Kimi Code CLI |
| `qoder/...` | Qoder CN | Qoder CN |
| `qwenwork/...` | QwenWork | Qwen Office / QoderWork CN |
| `tabbit/...` | Tabbit | Tabbit Browser |
| `minimaxcode/...` | MiniMax Code | MiniMax Code |
| `gemini/...` | Gemini | Google Cloud Code Assist (Antigravity) |
| `cline/...` | Cline | Cline |
| `lobsterai/...` | LobsterAI | Youdao LobsterAI |
| `codearts/...` | CodeArts | Huawei CodeArts |
| `loomy/...` | Loomy | iFlytek Loomy |
| `raccoon/...` | Raccoon | SenseTime Raccoon Work |
| `openrouter/...` | OpenRouter | OpenRouter |
| `opencode/...` | OpenCode Zen | OpenCode Zen |
| `kilocode/...` | Kilo Code | Kilo AI Gateway |
| `doubao/...` | Doubao | ByteDance Doubao desktop client / web app |
| `doubaoime/...` | Doubao IME | Doubao Input Method (豆包输入法) online speech-to-text (transcription only) |
| `openai-compat/...` | onmiRoute | Legacy compatible sources |
| `<source-id>/...` | Custom relay | Any OpenAI-compatible platform you add |

## Features

### Gateway and routing

- OpenAI-compatible `POST /v1/chat/completions`, `POST /v1/responses`, and inbound Anthropic `POST /v1/messages` / `POST /v1/messages/count_tokens`.
- OpenAI-compatible `POST /v1/audio/transcriptions` (multipart `file` + `model`)
  for speech platforms; `GET /v1/models` lists their models with `extra.kind = "transcription"`.
- `GET /v1/models`, `GET /v1/status` and `GET /healthz`.
- One inbound API Key for every platform, with optional local panel authentication.
- Explicit `platform/model` routes, bare model names, aliases and `Auto/<model>` aggregation.
- Custom `model_groups` for binding the same model across platforms. A group is a
  failover chain reached by a bare name (or the group name itself): every member is
  tried in priority order, and a member the caller spelled out explicitly is tried
  first. Writing `platform/model` is the caller naming one platform, so it is answered
  by that platform alone even when the same id is also a group member: a group never
  silently redirects an explicit platform choice, and a failure on it is reported as
  that platform's own failure rather than hidden behind a switch. `Auto/<model>` keeps
  its own meaning and skips groups, searching the concrete catalogues directly.
- An operator-set outward model whitelist (`exposed_models`). The gateway builds the
  full final catalogue first -- `platform/model` ids, `Auto/<model>` names,
  `model_groups` names, aliases and transcription ids -- then serves only the ids the
  operator selected. Each final id is its own switch: selecting a group member does not
  expose the group name, and selecting an alias target does not expose the alias.
  Matching is case-insensitive and ignores surrounding space. Unset means every final
  model is served; an explicit `[]` means none is. The whitelist gates `GET /v1/models`
  and every call entry point (chat, responses, messages, count_tokens and audio
  transcriptions), so a hidden id is not callable even when it is named directly.
- Platform priority, account priority and time-based priority schedules.
- Per-platform and per-account concurrency limits.
- Per-account per-model enable/disable from the account's model dialog, on every platform: WorkBuddy keeps its module-side policy (including "force enable"), the others write a shared blacklist the gateway's account hook enforces on the next request. A rate-limited model shows when it will be released and how long is left.
- Any upstream 429 parks the `(platform, account, model)` triple for a cooldown: 60 seconds at first, doubling per consecutive failure and capped at 30 minutes, or the vendor's own `Retry-After` when it sent one (also capped). The gateway refuses that account for that model while the park is live, so a module rotates to another account or platform instead of walking back into the same rate limit, and the account's model dialog shows when the park is released and how much time is left. A module that reports every attempt through `core.ChatRequest.NoteAccountFailure` is covered even when it recovers on another account; the rest fall back to the gateway's terminal-failure record.
- Conversation stickiness, retries, cooldown and automatic failover, with panel controls that show each live binding's model and can force-unbind or force-switch a session. A force-switch can clear platform/account stickiness for automatic selection again or pin a chosen platform while still auto-selecting its account.
- OpenAI-shaped error mapping for auth, quota, WAF, timeout and rate-limit failures.
- Failover also covers failures a platform reports after the stream has already been
  opened. Some vendors answer a spent quota with HTTP 200 and an SSE frame carrying
  the error, so the gateway reads the first frame before committing to the platform:
  if that frame is a retryable error (an out-of-credit account, an auth failure, a
  rate limit, a WAF block), the stream is closed and the request is served by the
  next candidate instead of being forwarded as a 502. The frame already read is not
  lost, it is handed to whichever platform answers.
- An out-of-credit account is parked out of the rotation rather than put in a
  time-based cooldown, because a zero balance does not come back on its own. On
  LobsterAI the Chinese free-quota messages ("免费额度已用完", "请升级套餐") are read
  this way: the account stops being selected for new requests, the panel shows it as
  已耗尽, and the next balance refresh that reports a positive credit clears the park
  automatically. A platform that still has a usable account is therefore preferred
  over one that has none, whatever the configured priority says.
- WorkBuddy tool schemas: a tool declared with empty `parameters` (`{}`) is
  normalized to `{"type":"object","properties":{}}` before it goes upstream.
  DeepSeek V4.1 Flash rejects the empty object with HTTP 400 / business code
  `11129` ("invalid function call parameters"); the other WorkBuddy models
  tolerate it, which is why the failure looked model-specific. A tool with no
  real arguments and a tool with real properties are both unaffected.
- Context-window guard: a request larger than the model's window is shortened before
  it is sent, so it is served instead of refused. The system prompt and the newest
  turn are always kept; only the stale middle is dropped. Every trimmed request is
  marked `已裁剪` in the usage log with how much was dropped and the sizes either
  side.
- When a vendor refuses an oversized prompt, the limit it names
  (`prompt is too long: 100001 tokens > 100000 maximum`) is learned and persisted to
  `data/learned_context_windows.json`, and `/v1/models` then advertises that number
  instead of the catalogue's. This is what stops the recurrence: clients size their
  own packing from `/v1/models`. A learned limit is shown with its own `实测`
  (measured) source and never overrides a manual value from `model_context.json`.
- A refusal number is cross-checked against what the model was actually observed to
  serve: a learned limit is never allowed below 80% of the largest prompt this
  gateway has had that model answer successfully (read from the usage ledger, so it
  survives restarts). A refusal often names a limit that applies to one condition
  rather than the model's real window, and this stops one such refusal from
  permanently shrinking a model to a fraction of what it demonstrably handles.
- A request whose newest turn alone exceeds the window is refused with
  `context_window_exceeded` rather than trimmed, because cutting into the question
  would answer a different one.
- Reasoning-effort policy is resolved per platform and model from the operator's
  manual override, then the module's cached catalogue, then the embedded preset.
  The model page can edit the supported levels and default, and those values are
  authoritative over a stale module cache.
- Cap alignment is on by default for every model on every platform; the panel
  still lets an operator turn it off or restore the default per model. When it is
  on, a caller's top level `xhigh` is promoted to the model's real highest level
  only when that level is higher (for example WorkBuddy, Cline and OpenRouter
  `max`). Lower requests and models whose top level is already `xhigh` - Loomy,
  for example - are left unchanged rather than sent a value the vendor never
  advertised. A model with no published effort ladder is also harmless: the
  default is on, but the gateway has nothing higher to send. The usage log keeps
  both values and shows `xhigh → max` when they differ.

- The embedded effort defaults still follow the platform-specific facts rather
  than one global `max`: WorkBuddy CN is `low/high/max` (default `high`),
  WorkBuddy global is `high` only, Cline uses its one global wire vocabulary,
  OpenRouter publishes `low/high/max`, Loomy tops out at `xhigh`, and
  Raccoon/CodeArts expose on/off thinking. Platforms that publish no per-model
  effort ladder keep the effort list unset; cap alignment is still on for those
  models but is a no-op. Only rows whose real top level is above `xhigh` change
  the request.

### Accounts and platforms

- Account pool with enable/disable, notes, identity labels, priority and balance.
- Manual import, local credential discovery, bundle import and browser/device login where supported.
- Re-login, revive, test, check-in, balance refresh and account deletion.
- Loomy balance, package and model-catalogue refreshes are display-only: a failed read is reported on that row and does not cool down, park, or disable the account; a real chat request or the explicit Test action is what changes account health.
- ZCode JWT / API key channels grouped under one account.
- Dynamic OpenAI-compatible relay platforms with their own API root and Key pool.
- Model scanning for relay platforms, with cached catalogues when a scan fails.
- Balance refresh for relay platforms via `balance_protocol`: `auto` (default) probes New API `GET /api/usage/token/`, then Sub2API `GET /v1/usage`, then the New API console `GET /api/user/self` (only when console credentials are set), then OpenAI Billing `credit_grants` + `subscription`. Pin a single protocol with `newapi`, `sub2api` or `openai_billing`, or set `none` to skip balance reads.
- `balance_quota_per_usd` (default `500000`) converts New API quota units to USD; Sub2API and OpenAI Billing values are read as real USD.
- Optional per-account advanced fields `balance_access_token` and `balance_user_id` supply the New API console credentials when the plain API Key cannot read the balance.

### Panel

- Accounts
- Sticky-session management from the Accounts overview: inspect account/platform bindings and their last model, force-unbind an account, or force-switch back to normal selection or to a chosen platform
- Chat Test
- Chat Test switches to a microphone transcription check when the selected model is a
  speech model (for example `doubaoime/...`): the recording is posted to
  `POST /v1/audio/transcriptions` and the raw status code and response body are shown
- Usage
- Alerts
- Credits
- Packages
- Task Center
- Models and Profiles, which holds model capabilities and an Exposed Models tab. The
  Exposed Models tab picks which final ids leave the gateway: search, filter by kind,
  select all / invert / clear, and per-row checkboxes. Saving writes `exposed_models`
  and reloads with no restart; an id that is configured but missing from the current
  catalogue is kept as a `missing` row so a save cannot drop it.
- Platform Configuration
- Config keeps only genuinely global settings: listen address, inbound API Key,
  data directory, upstream proxy, session stickiness, panel display, aliases and
  disabled clients. Platform Configuration owns everything platform-specific:
  routing, blacklists, concurrency, model groups, each module's own
  `clients.<module>` section (rendered as labelled controls inside that
  platform's card) and the WorkBuddy-only prompt/fingerprint controls. An empty
  control restores the module default, and keys the panel does not yet describe
  stay available under `Other keys (JSON)`. Module config changes take effect
  after restarting the process.
- The Platform Configuration route-plan test: enter a model name (for example
  Auto/MIMO-2.6Flash) and the panel shows the static failover chain - which
  platform would be tried first, which one comes next, the effective priority
  and where it comes from (platform priority, a time window, a model group or
  the dynamic Auto pool). It is a local replay of the routing rules only: no
  upstream call is made and no account state is read.
- Runtime Log

### Tasks, usage and accounting

- Scheduled tasks with Asia/Shanghai times and per-platform overrides, including second/minute/hour intervals with symmetric jitter.
- Batch execution and a task board for supported account actions.
- Daily balance refresh can run by time, by interval, or by a 5/6-field cron expression.
- Recovery probes, daily balance refresh and check-in.
- A check-in the vendor settles (already done today, or the vendor says the account cannot check in today) greys out that account's button for the rest of the Shanghai day and is skipped by batch runs; transient network/auth errors stay clickable and reset automatically at the next Shanghai day.
- Success rate, latency, token totals, cache hit rate and recent calls.
- Per-request session IDs and per-model / per-account statistics.
- CNY cost estimates, official price tables, models.dev fallback and manual overrides.
- Credit composition by expiry and package/client views.
- Platform alerts and structured runtime logs.
- SSE termination diagnostics (`stream: id=...`) include protocol, platform/model,
  upstream event count, `finish_reason`, text/reasoning byte counts, tool-call count,
  completion tokens, first-event latency, elapsed time and terminal reason. Output
  limited by the caller is marked `terminal=incomplete`. Conversation content and
  credentials are never logged. These appear after upgrading.

### Responses API

The Responses API is translated natively for Codex and compatible clients:

- `instructions`, string/array `input`, messages, text and images.
- Function calls and function-call outputs.
- Namespaced tool declarations (`type: "namespace"`, the shape Codex now uses
  for MCP servers): children are flattened for the vendor and split back into the
  `(namespace, name)` pair Codex dispatches on.
- Reasoning summaries and encrypted agent content handling.
- Streaming event ordering required by the Codex parser.
- Stateless operation and `previous_response_id` compatibility behavior.
- A dead turn -- `finish_reason=stop` with no tool call and nothing the caller can
  act on, either no visible text at all or a short fragment after a long
  deliberation on a request that offered tools -- is retried once upstream
  instead of being handed back as a finished turn, which is the shape that made
  Codex close the turn and wait for the operator to type the next prompt.
- A tool-call block a model wrote as text that the parser cannot read is surfaced
  as visible text instead of being dropped, so a swallowed call cannot look like
  a clean stop.

See the detailed reference for translation rules and limitations.

## Screenshots

| Accounts | Platform Configuration |
|---|---|
| ![Accounts](docs/images/accounts.png) | ![Platforms](docs/images/platforms.png) |

| Models | Usage |
|---|---|
| ![Models](docs/images/models.png) | ![Usage](docs/images/usage.png) |

| Task Center | Chat Test |
|---|---|
| ![Task Center](docs/images/tasks-center.png) | ![Chat](docs/images/chat.png) |

## Quick Start

### Windows installer

Download the latest `client2api-setup-<version>.exe` from
[Releases](https://github.com/Cvencent/client2api/releases), install it, then
open:

```text
http://127.0.0.1:8790/panel/
```

### Build from source

```powershell
$tools='D:\client2api-lab\_tools'
$env:PATH="$tools\go\bin;$tools\w64devkit\bin;$env:PATH"
$env:GOCACHE="$tools\gocache"
$env:GOTMPDIR="$tools\gotmp"
$env:GOMODCACHE="$tools\gomodcache"
$env:GOFLAGS='-mod=mod'
$env:GOPROXY='off'
$env:CGO_ENABLED='0'
go build -o client2api.exe ./cmd/client2api
.\client2api.exe -config configs\client2api.json
```

The gateway listens on `127.0.0.1` by default. Do not run two instances against
the same `data/` directory, and do not run the development binary from the
source tree if an installed instance is already using the same port.

## Documentation

- [Chinese tutorial](TUTORIAL.zh-CN.md)
- [Detailed reference (English)](docs/REFERENCE.md)
- [详细参考文档（中文）](docs/REFERENCE.zh-CN.md)
- [Module contract](docs/MODULE-CONTRACT.md)
- [Upstream protocol notes](docs/upstream)
- [Changelog](CHANGELOG.md)

## License and Use

This repository is for personal use, study and research. It does not grant any
right to run a public or commercial relay service, and it does not change the
terms of any upstream provider. See [LICENSE](LICENSE).
