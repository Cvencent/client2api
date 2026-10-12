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
  WAV upload comes back as `{"text": "..."}`.
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

- OpenAI-compatible `POST /v1/chat/completions` and `POST /v1/responses`.
- OpenAI-compatible `POST /v1/audio/transcriptions` (multipart `file` + `model`)
  for speech platforms; `GET /v1/models` lists their models with `extra.kind = "transcription"`.
- `GET /v1/models`, `GET /v1/status` and `GET /healthz`.
- One inbound API Key for every platform, with optional local panel authentication.
- Explicit `platform/model` routes, bare model names, aliases and `Auto/<model>` aggregation.
- Custom `model_groups` for binding the same model across platforms.
- Platform priority, account priority and time-based priority schedules.
- Per-platform and per-account concurrency limits.
- Conversation stickiness, retries, cooldown and automatic failover, with panel controls that show each live binding's model and can force-unbind or force-switch a session. A force-switch can clear platform/account stickiness for automatic selection again or pin a chosen platform while still auto-selecting its account.
- OpenAI-shaped error mapping for auth, quota, WAF, timeout and rate-limit failures.
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
- Cap alignment is opt-in per model and off by default. When enabled, a caller's
  top level `xhigh` is promoted to the model's real highest level only when that
  level is higher (for example WorkBuddy `max`); lower requests and models without
  a higher tier are left unchanged. The usage log keeps both values and shows
  `xhigh → max` when they differ.

- The embedded V4.1 defaults follow the platform-specific facts rather than one global
  `max`: WorkBuddy CN is `low/high/max` (default `high`), WorkBuddy global is
  `high` only, Cline uses its one global wire vocabulary, Loomy tops out at
  `xhigh`, and Raccoon/CodeArts expose on/off thinking. Platforms that publish no
  per-model effort ladder stay unset.

### Accounts and platforms

- Account pool with enable/disable, notes, identity labels, priority and balance.
- Manual import, local credential discovery, bundle import and browser/device login where supported.
- Re-login, revive, test, check-in, balance refresh and account deletion.
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
- Usage
- Alerts
- Credits
- Packages
- Task Center
- Models and Profiles
- Platform Configuration
- Config
- Runtime Log

### Tasks, usage and accounting

- Scheduled tasks with Asia/Shanghai times and per-platform overrides, including second/minute/hour intervals with symmetric jitter.
- Batch execution and a task board for supported account actions.
- Daily balance refresh can run by time, by interval, or by a 5/6-field cron expression.
- Recovery probes, daily balance refresh and check-in.
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
- Reasoning summaries and encrypted agent content handling.
- Streaming event ordering required by the Codex parser.
- Stateless operation and `previous_response_id` compatibility behavior.

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
