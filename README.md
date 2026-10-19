# client2api

[English](README.md) | [简体中文](README.zh-CN.md) | [使用教程](TUTORIAL.zh-CN.md)

[![build](https://github.com/Cvencent/client2api/actions/workflows/go-binaries.yml/badge.svg)](https://github.com/Cvencent/client2api/actions/workflows/go-binaries.yml)
[![release](https://img.shields.io/github/v/release/Cvencent/client2api?include_prereleases)](https://github.com/Cvencent/client2api/releases)

Current version: **0.1.74**. Full changelog: [CHANGELOG.md](CHANGELOG.md).

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
- Serves `POST /v1/chat/completions`, `POST /v1/responses`, and inbound Anthropic
  `POST /v1/messages` / `POST /v1/messages/count_tokens` with one inbound API Key.
- Serves `POST /v1/audio/transcriptions` for platforms that expose speech, so a
  WAV/FLAC/MP3/MP4/M4A/OGG/WebM upload comes back as `{"text": "..."}` (or
  text, SRT, verbose JSON, VTT).
- Can route all upstream traffic through an optional managed egress relay:
  use an existing HTTP/SOCKS proxy, or let the process manage mihomo from a
  subscription.
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

### OpenCode: a bare HTTP 400 is retried once

Zen occasionally answers a correct history with `HTTP 400` and an **empty body**.
Nothing in that answer names a cause, and the identical request is usually served
a moment later. `clients/opencode` therefore spends one rotation of its account
pool on this shape (anonymous → signed-in, or the next usable account) before
reporting it. A 400 that carries an error type or message — an unknown model, a
malformed field — stays final, and so does every other status code, so a caller's
mistake is never multiplied across the pool.

## Features

### Gateway and routing

- OpenAI-compatible `POST /v1/chat/completions`, `POST /v1/responses`, and inbound Anthropic `POST /v1/messages` / `POST /v1/messages/count_tokens`.
- The Anthropic Messages ingress supports Claude Code's string or block
  content, system prompts, tools, tool results, thinking, usage, streaming SSE,
  non-streaming responses and Anthropic-shaped errors. It can be served by any
  module that can handle the translated chat request.
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
  its own meaning and skips groups, searching the concrete catalogues directly. Every
  group entry also publishes the fixed `low/medium/high/xhigh/max` ladder in
  `extra.supported_efforts`; each selected member still applies its own effort policy.
- An operator-set outward model whitelist (`exposed_models`). The gateway builds the
  full final catalogue first -- `platform/model` ids, `Auto/<model>` names,
  `model_groups` names, aliases and transcription ids -- then serves only the ids the
  operator selected. Each final id is its own switch: selecting a group member does not
  expose the group name, and selecting an alias target does not expose the alias.
  Virtual rows are editable under the reserved `__virtual__` client in the model page:
  the override applies to the published `Auto/<model>` or group name and never to the
  concrete platform members.
  An entry ending in `/*` exposes a whole platform prefix (for example
  `my-relay/*`) without opening any other platform. Matching is case-insensitive
  and ignores surrounding space. Unset means every final
  model is served; an explicit `[]` means none is. The whitelist gates `GET /v1/models`
  and every call entry point (chat, responses, messages, count_tokens and audio
  transcriptions), so a hidden id is not callable even when it is named directly.
- Platform priority, per-model priority, account priority and time-based
  priority schedules.  The effective priority of a (platform, model) pair is the
  most specific rung that is set: the per-model override on the platform card,
  then the model group's own platform priority, then a matching time window,
  then the platform's base priority.  A rung that is not configured is skipped
  rather than treated as zero, so an untouched model keeps inheriting whatever
  it inherited before.  The Platform Configuration page shows the per-model
  number on each model row, and the route chain reads the same value, so the two
  views cannot disagree.
- Optional `egress` relay. `client` mode accepts an HTTP, HTTPS, SOCKS5 or
  SOCKS5H proxy URL; `subscription` mode hands a subscription URL to a managed
  mihomo process and points gateway traffic at its loopback mixed port. An
  enabled `egress` takes precedence over the top-level `proxy`; changing this
  block requires a restart.
- Per-platform and per-account concurrency limits.
- Per-account per-model enable/disable from the account's model dialog, on every platform: WorkBuddy keeps its module-side policy (including "force enable"), the others write a shared blacklist the gateway's account hook enforces on the next request. Each model carries its current effective multiplier (or a free tag) when the platform publishes one; a rate-limited model also shows when it will be released and how long is left.
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
- WorkBuddy's international realm follows the official WorkBuddy AI 5.8.4 VLM
  client: `WorkBuddy/5.8.4 WorkBuddy/5.8.4 CLI/2.161.0`,
  `X-IDE-Version: 5.8.4`, `X-Product: SaaS`, `X-Agent-Purpose: conversation`,
  `X-Private-Data: true`, a process-stable `X-Conversation-ID` /
  `acp-connection-id`, and the client's W3C/B3 trace headers. It omits the
  browser-only Origin/Referer and machine/session headers on chat calls. A
  global request missing the conversation id, or still carrying the retired CN
  pair, is answered with business code `11-128` ("request illegal"). Every
  outbound chat body also gets a leading `system` turn, because the same code is
  what the vendor returns for a conversation that opens with a user turn, and
  `11-128` is classified as a content refusal rather than an auth fault, so a
  healthy account is not parked
  and the request gets the degraded-prompt retry instead. CN keeps its
  5.7.7 / 2.137.1 pair and its header set unchanged.
- Context-window guard: a request larger than the model's window is shortened before
  it is sent, so it is served instead of refused. The system prompt and the newest
  turn are always kept; only the stale middle is dropped. Every trimmed request is
  marked `已裁剪` in the usage log with how much was dropped and the sizes either
  side.
- A trim never leaves an orphaned tool result behind. The kept window cannot begin
  with a `tool` message whose assistant call was dropped: vendors validate that
  pairing, so an orphan turns a recoverable "too long" into an unrecoverable
  "bad parameters" (WorkBuddy's code 11133). When the boundary lands inside a tool
  round trip, the whole round trip is kept and the cut moves earlier.
- An inline base64 image is priced as an image rather than as text. `view_image`
  returns a screenshot as a `data:image/...;base64,` URL, and a single 466x466 PNG
  is tens of thousands of characters that the plain text divisor used to read as
  ~18k tokens. That one overcount put the guard's estimate an order of magnitude
  ahead of the vendor's own measurement on any conversation that had just looked
  at a screenshot.
- A virtual model advertises the widest member window so the downstream client can
  keep the large-window experience. If routing falls through to a narrower platform,
  that candidate alone is trimmed and, if needed, retried; the next candidate always
  starts from the caller's pristine history. A candidate whose newest turn cannot fit
  is skipped in priority order and the request is refused only when no candidate can
  serve it.
- When a vendor refuses an oversized prompt, the limit it names
  (`prompt is too long: 100001 tokens > 100000 maximum`) is learned and persisted to
  `data/learned_context_windows.json`, and `/v1/models` then advertises that number
  instead of the catalogue's. This is what stops the recurrence: clients size their
  own packing from `/v1/models`. A learned limit is shown with its own `实测`
  (measured) source and never overrides a manual value from `model_context.json`.
- A refusal that names no numbers still gets one conservative retry. Loomy answers
  an oversized prompt with prose ("内容太长了，请精简输入或开启新对话") that names
  neither the size it measured nor the limit it compared against, so there is no
  proportion to keep and nothing to learn. The gateway trims to a fraction of the
  window it already advertises and retries once; a served answer beats a dead end,
  and a refusal that repeats is returned to the caller unchanged.
- The cheapest refusal is the one that never leaves the gateway. Loomy's own
  catalogue declares which models accept image input, and the module replaces
  every image part with a short text placeholder for a model that does not —
  DeepSeek and Kimi behind Loomy are text-only and read a base64 data URL as
  prompt text, so the screenshots a coding agent's history keeps re-sending
  refuse the request outright. Vision-capable Loomy models keep their images.
  Details in `clients/loomy/README.md`.
- A refusal that names both numbers is also a measurement of the request that was
  just sent. When the gateway's own estimate read low enough to forward that request
  unchanged, the history is trimmed to the proportion the vendor's window can hold and
  the request is retried once, so a conversation that would otherwise fail on every
  `继续` is served instead. The `\u003e` form WorkBuddy's body actually carries is
  read too. The retry's trim lands on the usage row like any other.
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
- `Auto/<model>` and custom model groups expose the same editable context window,
  max-output, thinking and cap-alignment fields as a concrete platform/model
  row. The effective context is the widest member plus any `__virtual__`
  override; the effort list is
  the stable union of members and the default follows the first platform in the
  operator's routing order. Editing the virtual row is a gateway-level override,
  so it is applied before the per-candidate policy.
- A virtual name injects its default effort only when the caller omitted one.
  Each platform still aligns that level to its own advertised ladder (preferring
  the nearest lower rung) and keeps the user's platform priority unchanged; the
  usage row records the caller's original request and the level actually sent.
- Cap alignment is on by default for every model on every platform, including
  `Auto/<model>` and model-group rows; the panel still lets an operator turn it
  off or restore the default per model. The built-in rule is `xhigh → top`,
  where `top` resolves to the highest level that candidate actually publishes.
  It only moves upward: a model that publishes only `high` keeps `xhigh`
  unchanged instead of being downgraded, and a model with no published effort
  ladder is left untouched. The usage log keeps both values and shows
  `xhigh → max` when they differ.
- The row's mapping editor accepts multiple comma-separated one-step rules,
  for example `high → max, xhigh → max`. Rules are matched by their source
  exactly and are not chained. A custom rule is authoritative and is sent as
  written, even when the candidate's catalogue does not list the target: this
  is what lets `max → high` and `max → max` work on a model that only advertises
  `high`. A custom `xhigh` rule replaces the built-in `xhigh → top` rule for
  that model; a custom no-op such as `xhigh → xhigh` cancels the built-in
  promotion without turning the whole policy off. A model-group row's explicit
  mapping is applied to every member, while a row without one follows each
  candidate's own mapping. Without a matching custom rule, the built-in
  `xhigh → top` rule still only moves upward to a published tier, and the
  generic unsupported-tier alignment remains the fallback for other levels.

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
- A model the platform advertises as free -- a zero multiplier, a free-quota
  list, or a vendor label naming the free tier in the display name, the group or
  the id (`cline-free/...`, `vendor/model:free`) -- is routed by the vendor's
  answer, not by the balance: an account parked for an empty wallet still serves
  it, and only the vendor refusing *that* model on *that* account takes the pair
  out. Cooldowns that are not about money -- rate limits, WAF blocks, dead
  sessions, risk flags, the breaker -- keep applying. WorkBuddy, Cline,
  CodeArts, Raccoon, QwenWork and OpenRouter implement the account-side rule today; the
  module-facing contract is `core.ChatRequest.FreeModel`, and the free labelling
  itself is shared in `core.ModelIsFree`.
- Manual import, local credential discovery, bundle import and browser/device login where supported.
- Re-login, revive, test, check-in, balance refresh and account deletion.
- Gemini / Google Cloud Code Assist accounts support Google OAuth login,
  account discovery and refresh, quota reads, and Cloud Code Assist request
  translation with streaming responses. Its model list is pulled live from
  `fetchAvailableModels` (quota buckets as the fallback), so the picker follows
  the vendor without a code change. Browser sign-in needs no configuration: the
  module embeds the Cloud Code IDE's own installed-app OAuth client, and
  `client_id` / `client_secret` (or `CLIENT2API_GEMINI_CLIENT_ID` /
  `CLIENT2API_GEMINI_CLIENT_SECRET`) override it with a client this deployment
  registered.
- ZCode can opt into a one-slot captcha-parameter prefetch pool. A request takes
  the fresh parameter and the pool replenishes in the background; the pool is
  off by default because prefetching roughly doubles device-quota consumption.
  Tune it with `captcha_pool` and `captcha_pool_ttl_seconds`.
- Loomy balance, package and model-catalogue refreshes are display-only: a failed read is reported on that row and does not cool down, park, or disable the account; a real chat request or the explicit Test action is what changes account health.
- ZCode JWT / API key channels grouped under one account.
- OfficeAce (Office Claw) ships as a built-in platform (`officeace`), so there is no relay entry to add. Open its account pool and use **Browser sign-in**: the panel runs the same Huawei account OAuth flow as the OfficeAce client, keeps the returned model-gateway App Key / App Secret in this gateway's own account store, and scans the catalogue published as `officeace/<model>`. Each account keeps the model gateway returned by its own login, so different regions or gateways can coexist; the OfficeAce desktop app does not need to stay open. On Windows, **Import -> Discover local credentials** remains available as a fallback for an already signed-in OfficeAce client.

- Dynamic OpenAI-compatible relay platforms with their own API root and Key pool.
  Relay requests are normalised for the DeepSeek/LiteLLM family before they go
  out, because a relay that fronts those models rejects several shapes that are
  otherwise valid OpenAI: a forced `tool_choice` (`"required"` or a named
  function) is downgraded to `"auto"`, a tool whose schema has no `properties`
  -- notably the bare `{}` a namespace or MCP declaration can produce -- gets an
  empty object skeleton filled in, and every assistant turn carries
  `reasoning_content` (as `""` when there is no trace). Without these the relay
  answers 400 with messages like "当前模型或上游不支持工具调用" or a bare
  "HTTP 400", neither of which means what it says.
- Custom relays have their own rate-limit and cooldown policy, editable in the panel's
  relay dialog. `requests_per_minute` caps upstream attempts across the relay and
  `requests_per_minute_per_account` caps each key; a full key is skipped in favour of
  the next, and the relay reports busy only when every key is full. `max_in_flight`
  and `max_in_flight_per_account` override the platform/account in-flight ceilings:
  leave them unset to inherit the platform policy, or set `0` for no ceiling.
  `cooldown` is the shared fallback; `rate_limit_cooldown`, `quota_cooldown`,
  `auth_cooldown` and `upstream_cooldown` override it per failure kind. An unset
  specific value uses the built-in default (30s for rate limits, 5m for quota/auth,
  10s for upstream failures); `0s` disables that cooldown.
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
  data directory, upstream proxy, optional egress relay, session stickiness,
  panel display, aliases and disabled clients. Platform Configuration owns
  everything platform-specific:
  routing, blacklists, concurrency, model groups, each module's own
  `clients.<module>` section (rendered as labelled controls inside that
  platform's card) and the WorkBuddy-only prompt/fingerprint controls. An empty
  control restores the module default, and keys the panel does not yet describe
  stay available under `Other keys (JSON)`. Module config changes take effect
  after restarting the process.
- The Platform Configuration route chain has two separate actions. **Link Test**
  statically replays the failover chain - which platform would be tried first,
  which one comes next, the effective priority and where it comes from
  (platform priority, a time window, a model group or the dynamic Auto pool) -
  and shows each platform model's effective context window and source so the
  order can be judged with context capacity in view, without reading account
  state or calling upstream. **Speed Test** resolves the same chain, then sends
  one minimal live request per platform through that platform's own account pool
  and reports first-event latency, total elapsed time, the serving account,
  completion tokens and tok/s. The result rows can be dragged or moved with
  Up/Down; saving writes the displayed order back to the per-model priority on
  each platform's model row -- a row outranks the one below it by one, so moving
  a platform above a row showing `0` writes `-1` -- and hot-reloads it. Clearing
  that box deletes the override so the model inherits the platform priority
  again.
- Runtime Log

### Tasks, usage and accounting

- Scheduled tasks with Asia/Shanghai times and per-platform overrides, including second/minute/hour intervals with symmetric jitter.
- Batch execution and a task board for supported account actions.
- Daily balance refresh can run by time, by interval, or by a 5/6-field cron expression.
- Recovery probes, daily balance refresh and check-in.
- A check-in the vendor settles (already done today, or the vendor says the account cannot check in today) greys out that account's button for the rest of the Shanghai day, whether it came from the account button, task centre or scheduler, and is skipped by batch runs; transient network/auth errors stay clickable and reset automatically at the next Shanghai day.
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

### OfficeAce models

OfficeAce (Office Claw) is built in as the `officeace` platform. Its desktop client exposes exactly four selectable models -- `glm-5.2`, `glm-5.1`, `deepseek-v4-flash-0731` and `deepseek-v4.1-flash` -- through the Huawei MaaS / AgentArts OpenAI-compatible model gateway. No relay entry or manual API root is required:

1. Open **Platform Configuration** and expand **Huawei** (the built-in `officeace` platform). It is present on a fresh install; it is shown as **built-in** and has no Delete button.
2. Open its account pool and choose **Auto add**. The panel rents a number from the shared one-time-SMS platform, drives a headless browser through the Huawei phone-code login, reads the verification code and submits it for you (including the Huawei Cloud user-agreement step shown for a fresh account). Paste the SMS-platform token into the dialog (or set `EOMSG_TOKEN` once) and leave the default sender keyword **华为**; the number is released automatically when the run ends. If you would rather sign in yourself, **Browser sign-in -> Get authorization link** still works. That link first visits Huawei's logout endpoint to clear the previous browser session, then returns to the login page with the OAuth request intact, so adding a second account does not silently reuse the first one. Either way the panel exchanges the authorization code for the account's model-gateway credential, writes it into this gateway's account store, and makes sure the account has the free `officeace.personal.package.trial` entitlement (500 credits and 200 online searches). An account that already has the trial is only checked and is not ordered again.
3. Model scanning runs automatically after a successful sign-in. **Scan models** can be pressed again later. The catalogue is published as `officeace/<model>`, and only those four ids are exposed. Other entries in Huawei's raw catalogue are ignored, matching the desktop client's picker exactly.

Each account keeps the `model_api_url_base` returned by its own Huawei login, so accounts on different regions or model gateways can share one `officeace` platform. Requests use HTTP Basic authentication and behave like every other platform: account rotation, failover, conversation affinity, usage accounting, streaming and tool calls all work unchanged. Credentials stay in the local `data/officeace/accounts.json`; they are never written to the config file or the example config.

The cloud credential minted during sign-in is stored with the account too, which is what brings the platform's WorkBuddy-style facilities online: **Check-in** on the account pool posts the model gateway's daily-reward endpoint (idempotent, so a second click reports that today's reward is already claimed); **Balance** shows the `officeace_points` position and the online-search meter; **Packages** splits the plan quota and each bonus activity. The trial order uses the IAM v3 `SDK-HMAC-SHA256` credential and does not enable auto-renewal or auto-pay. If the cloud has not yet materialised the trial, Balance, Packages and Check-in repair it lazily; a login whose trial order hard-fails is reported as failed instead of being presented as a complete account. When the temporary credential expires it is renewed from the OAuth refresh token or the IAM v3/STS exchange, again without the desktop app running.

On Windows, **Import -> Discover local credentials** remains available as a fallback: it reads a login already stored by an OfficeAce desktop client (DPAPI + AES-GCM), reports its upstream, and imports the App Key / App Secret pair. Imported accounts on a different OfficeAce gateway keep that account-level gateway instead of being rejected. **Add manually** is the last-resort path when neither browser sign-in nor local discovery is available: put the application key in `API Key / App Key`, expand **Advanced settings**, and put the matching secret in `App Secret`.

The account pool also offers the raw SMS verbs for a manual run: **Rent number**, **Read code** and **Release / Blacklist**. The token can be pasted in the panel per call, so no restart is needed; `EOMSG_TOKEN` is the process-level fallback, and the default sender keyword for a Huawei code is **华为**.

To expose only this platform, set `exposed_models` to `["officeace/*"]`.
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