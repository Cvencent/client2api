# client2api

One OpenAI-compatible gateway in front of fourteen AI backends:

| route | client | what it wraps |
|---|---|---|
| `workbuddy/…` | WorkBuddy | Tencent WorkBuddy / CodeBuddy (intl + cn realms) |
| `trae/…` | Trae | TRAE SOLO / Trae CN |
| `zcode/…` | ZCode | Z.AI coding plan (Anthropic Messages upstream) |
| `kimi/…` | Kimi | Kimi Code CLI |
| `qwenwork/…` | QwenWork | 千问办公 / QoderWork CN desktop agent |
| `tabbit/…` | Tabbit | Tabbit Browser (via a sidecar) |
| `minimaxcode/…` | MiniMax Code | MiniMax Code desktop agent (Anthropic Messages upstream) |
| `cline/…` | Cline | Cline (cline.bot) — WorkOS credential, OpenAI-shaped upstream |
| `lobsterai/…` | LobsterAI | 有道龙虾 LobsterAI (Youdao) — OpenAI-shaped, SSE only |
| `codearts/…` | CodeArts | 华为 CodeArts IDE model service (SDK-HMAC-SHA256) |
| `loomy/…` | Loomy | 讯飞 Loomy (iFlytek) — HMAC-SHA1 signed account endpoints |
| `raccoon/…` | Raccoon | 商汤小浣熊 Raccoon Work (SenseTime) |
| `openrouter/…` | OpenRouter | OpenRouter (openrouter.ai) — multi-vendor router whose ids contain a slash; free models only by default (`free_only`) |
| `opencode/…` | OpenCode Zen | OpenCode Zen (opencode.ai) — one OpenAI-shaped façade in front of Anthropic-, Google- and OpenAI-native models |

Everything is served from **one** HTTP surface — `POST /v1/chat/completions`,
`GET /v1/models`, `GET /v1/status`, `GET /healthz`, plus a web panel at
`/panel/`.

[![build](https://github.com/Cvencent/client2api/actions/workflows/go-binaries.yml/badge.svg)](https://github.com/Cvencent/client2api/actions/workflows/go-binaries.yml)
[![release](https://img.shields.io/github/v/release/Cvencent/client2api?include_prereleases)](https://github.com/Cvencent/client2api/releases)

当前版本：**0.1.9**。完整更新记录见 [CHANGELOG.md](CHANGELOG.md)。

## 项目简介

client2api 把多个 AI 客户端/平台的账号能力聚合成一个 OpenAI 兼容网关。它统一提供
`/v1/chat/completions`、`/v1/models`、`/v1/status` 和 `/healthz`，并在
`/panel/` 提供账号池、平台路由、用量、任务中心、配置导入导出等管理页面。

主要能力：

- 多平台账号池和自动切换；同一模型可按平台优先级和账号健康状态路由。
- OpenAI 兼容 API，支持流式和非流式请求。
- 平台级模型白名单/黑名单、路由优先级、并发上限和账号余额保护。
- 账号登录、导入、导出、签到、余额刷新、定时任务和运行记录。
- 免费模型优先、失败重试、限流冷却、账号/平台熔断与自动恢复。
- Windows 安装器支持覆盖升级：配置、账号池和用量数据原地保留，并在覆盖前后各跑
  一次真实浏览器面板自检，失败就拒绝安装或自动回滚。

## 快速开始

### Windows 安装

从 [Releases](https://github.com/Cvencent/client2api/releases) 下载最新
`client2api-setup-<version>.exe`，双击安装。安装完成后打开托盘菜单或在浏览器访问
`http://127.0.0.1:8790/panel/`。端口可以在托盘菜单“设置”中修改。

### 从源码运行

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

也可以直接用 Docker：

```sh
docker compose up -d --build
docker compose logs -f
```

### 数据安全

`data/` 包含账号凭据和运行状态，`configs/client2api.json` 包含本机配置；
两者都已加入 `.gitignore`，不会进入源码仓库。仓库中的安装器构建脚本使用
`-NoData` 生成空数据包，升级时由安装器保留现有数据。

## 发布与更新

每次发布安装包时，必须同步完成：

1. 更新 `cmd/client2api/main.go` 与 `installer/setup/main.go` 的版本号。
2. 在 [CHANGELOG.md](CHANGELOG.md) 写明本版本新增、修复和行为变化。
3. 创建同版本 Git tag 和 GitHub Release，并用安装包文件加 SHA256 作为发布资产。
4. 在 Release 说明中写清楚升级方式、是否需要重新登录、是否涉及数据迁移。

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
  nothing but fourteen blank imports.
* `cmd/client2api/main.go` builds each module in turn; **if one fails to
  construct, it is logged and skipped**, and the process keeps running with the
  rest.
* Every module keeps its state inside its own `data/<name>/` directory.

So: to add a client you create `clients/<name>/`, add one import line, and touch
nothing else. To remove one you delete the directory and the line. To rewrite one
you never look at the other thirteen.

See [`docs/MODULE-CONTRACT.md`](docs/MODULE-CONTRACT.md) for the interface, and
[`docs/upstream/<name>.md`](docs/upstream) for a client's protocol notes where one
exists — six of the original seven have a dossier there (workbuddy, trae,
qwenwork, tabbit, kimi, zcode). The seven added later (cline, lobsterai, codearts,
loomy, raccoon, openrouter, opencode), plus minimaxcode, document their protocol
inside their own `clients/<name>/README.md`.

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

Panel: <http://127.0.0.1:8788/panel/>.

> Use `curl.exe`, not `Invoke-WebRequest` — the latter is broken in this
> non-interactive shell.

## Deploy

### Docker

```sh
mkdir -p configs data && sudo chown -R 10001:10001 configs data
docker compose up -d --build
docker compose logs -f | grep 'generated api_key'     # first start only
```

The image is one static binary with the panel embedded — no node, no second
process, no assets to mount. It deliberately **ships no config file**: a baked-in
config would either be somebody else's key or a placeholder the whole world
knows. On first start `configs/client2api.json` does not exist, so the program
generates a random `api_key`, writes it there and prints it
(`client2api: generated api_key: …`) — read it from the log, then set it in the
panel's config page.

`ENTRYPOINT` passes `-listen 0.0.0.0:8788` because the built-in default is
`127.0.0.1:8788`, which inside a container means "only reachable from inside the
container". If you want the panel's `listen` field to be authoritative instead,
drop that flag from `command:` **and** set `listen` to `0.0.0.0:8788` in the
config file first.

`HEALTHCHECK` probes `/healthz`, which answers **503** when no account is
servable — so a container that is running but has nothing usable is reported
`unhealthy` rather than merely "port open". Mount `./configs` and `./data` as
directories (not single files): the config file is created on first start and
the panel writes back to it, while `data/` holds the pool state, the usage
rollup and the model cache.

### Release bundles

`.github/workflows/go-binaries.yml` builds five platforms
(windows/linux/darwin × amd64, plus linux and darwin arm64) on every push to
`main`, asserts that the injected version string is really in the binary, and on
a `v*` tag publishes zip/tar.gz archives plus `checksums.txt`. Each archive
carries the binary, `configs/client2api.example.json` and this README. Copy the
example to `configs/client2api.json` and **fill in `api_key` before starting** —
an empty key leaves both the gateway and the panel unauthenticated, which the
program will warn about on every start.

`.github/workflows/docker-ghcr.yml` builds `linux/amd64` + `linux/arm64` and
pushes to `ghcr.io/<owner>/<repo>` (PRs build only, never push).

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

### ZCode 账号与额度

ZCode 的一个 Zhipu 账号可能同时拥有计划 JWT、coding-plan API key 和 OAuth 派生 key。
面板按账号身份把这三类凭证聚合为“一个账号、多条通道”，不会把它们当成三个独立账号。

额度来自两套上游接口并合并展示：

- `open.bigmodel.cn/api/monitor/usage/quota/limit`：5 小时滚动窗口、每日 Token 窗口、
  当前用量、剩余量和重置时间；`subscription/list` 补充套餐等级与到期时间。
- `zcode.z.ai/api/v1/zcode-plan/billing/balance`：活动套餐、Start Plan 等一次性或周期
  Token 额度。

活动套餐领取会解析同账号的 sibling 通道：从 API-key 行或 JWT 行点击都可以领取。
如果厂商返回“当前用户不存在coding plan”，说明该 key 本身不属于 coding-plan 套餐，
不是面板漏显示；请在 ZCode 客户端登录对应套餐账号后重新导入。
## Model routing

Three accepted forms:

| form | meaning |
|---|---|
| `zcode/GLM-5.3` | explicit: module `zcode`, model `GLM-5.3` |
| `GLM-5.3` | the unique module whose catalog lists it (ambiguous → 400 listing the owners) |
| `gpt-4o` | whatever the `aliases` table in the config says |
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

## Configuration

`configs/client2api.json` — the seven keys the gateway needs, plus the optional
sections below:

```json
{
  "listen": "127.0.0.1:8788",
  "api_key": "",
  "data_dir": "data",
  "proxy": "",
  "aliases": { "gpt-4o": "trae/custom_model_gpt-5" },
  "disabled": [],
  "clients": {
    "<name>": { }
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

### Environment overrides

Every deployment-level knob can also come from the environment, which is what a
container or a supervisor should use — the file stays the operator's document and
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
  thing, so a container that inherits a blank variable does not erase the file's
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
| `panel.package_detail_limit` | restart; the panel reads it once, at construction |
| `schedule.*` | live (`Reconfigure`), but the running loop only reacts to the *next* wake; the `schedule` block is reported in `GET /panel/api/status` |

`prompt` is the system-prompt strategy: `passthrough` (default), `custom`
(replace every `system`/`developer` turn with `file`, or with the shipped
engineering prompt when `file` is empty) or `append` (insert after the leading
system turns). It exists because a vendor content filter rejected a client's own
system template verbatim; `clients/workbuddy/README.md` records the behaviour and
why the other modules deliberately leave their vendor's prompt alone.

`schedule` runs the daily chores each module advertises through
`core.PlannedBatches` — "全部自动" for the whole pool, not just the one client
you have the panel open on. A module that declares no batches of its own but
implements `core.CheckinProvider` gets one synthetic `checkin` batch, so its
account-row check-in button and the timetable are the same chore. Hour lists
are Asia/Shanghai local hours, and an **empty list means "never run this one"**
even when the master switch is on. Accounts are walked serially with a 45 s
gap, because the vendors' anti-abuse checks roll back chores whose events
burst.

`session_sticky` has two levels.  Inside a module it pins a conversation to the
account that last served it.  At the gateway it also pins the **platform**: when
a bare model id is served by several platforms, the platform that last answered
stays first for the next turn, and the other candidates remain as failover.  A
failed platform is replaced by whichever platform then succeeds, so a broken
sibling is not retried on every turn.  `session_sticky.ttl` retunes both levels
on a live reload; `session_sticky.enabled=false` turns both off.

Independent of stickiness, the gateway demotes a platform that cannot serve
**right now** -- every account busy or cooling, or no usable account at all --
behind the healthy candidates for 30 seconds.  This is backpressure, not a
failure: it raises no alert, and the platform stays in the candidate list as a
last resort.  Repeated upstream failures still take the full 10-minute cooldown.
`schedule.balance_refresh_enabled` (default **on**, as in the reference) adds a
tick of its own every `schedule.balance_refresh_minutes` (default **5**) — it asks
every module that can report a balance for one, which is how a credit park lifts
by itself, and then refreshes the model lists. Two check-ins are hours apart, so
without it credits go stale and a balance-recovered account stays parked until
the next chore. Set the minutes to `0` to drop the tick without touching the
switch. It is still gated by `schedule.enabled`, like every other batch.

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
`积分不足，已暂停`, and the next `schedule.balance_refresh_minutes` sweep
revives the account automatically once its balance is above the threshold;
a check-in or a manual balance refresh has the same effect. Changing the
value is live and re-evaluates every account immediately. For example,
`{"platforms":{"workbuddy":{"reserve_credits":0}}}` keeps zero-credit
WorkBuddy accounts out of routing and shows them as paused instead of green.

The guard currently has a pool-side implementation in workbuddy, the reference's
credit-parking model; a module without a balance-aware pool ignores it.

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
the account it started on; the key comes from `conversation_id`,
`conversationId`, `prompt_cache_key` or the request's user field, in that order.
A request carrying none of them is not sticky *by default* — but a module may opt
in to a content-derived key (`core.DeriveConversationKey`), which hashes the
system prompt plus the first user turn and returns it under a `d-` prefix.
workbuddy opts in, so a client that sends no id at all still keeps its account
instead of rotating every turn and re-billing the whole prefix. `gc_interval`
(default **5m**) is how often expired bindings are swept. All three are hot: every
module that implements `core.LiveReloader` picks them up on a reload. The seven
original modules all implement it; the seven added later implement neither
`LiveReloader` nor `ConversationBinder`, so for them the switch has no effect.
Of the seven that do bind conversations — workbuddy, trae, qwenwork, zcode, kimi,
tabbit and minimaxcode — workbuddy and trae are the two where the switch has the
most visible effect. A binding whose account has been
parked or is cooling down for the requested model is dropped rather than served,
so stickiness can never turn into a failure.

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
| trae | MIT upstream, ported | see `clients/trae/README.md` |
| zcode | AGPL upstream → **clean-room rewrite** | see `clients/zcode/README.md` |
| kimi | MIT upstream, ported | see `clients/kimi/README.md` |
| qwenwork | **no licence** → **clean-room rewrite** | see `clients/qwenwork/README.md` |
| tabbit | GPL-3.0 upstream → **sidecar only, no code reuse** | see `clients/tabbit/README.md` |
| minimaxcode | **no upstream** → **clean-room rewrite** | see `clients/minimaxcode/README.md` |
| cline | **clean-room** — protocol reconstructed from observed traffic | see `clients/cline/README.md` |
| lobsterai | **clean-room** — from a TypeScript integration plan for another project | see `clients/lobsterai/README.md` |
| codearts | MIT upstream, ported (wire protocol only) | see `clients/codearts/README.md` |
| loomy | MIT upstream → **clean-room rewrite** | see `clients/loomy/README.md` |
| raccoon | MIT upstream, ported (wire protocol only) | see `clients/raccoon/README.md` |
| openrouter | public REST API, no upstream code read | see `clients/openrouter/README.md` |
| opencode | public REST API; the gateway's own server source is open and was read | see `clients/opencode/README.md` |

Provenance matters here: `zcode` and `qwenwork` are written from observed
protocol behaviour only, and `tabbit` never links GPL code. Each module's README
records what was read and what was written.

## The panel

<http://127.0.0.1:8788/panel/> (`/` redirects there). It is laid out like the
original WorkBuddy dashboard: a left sidebar carrying nine views (账号池 / 对话测试 /
用量 / 积分构成 / 客户端 / 任务中心 / 模型与档位 / 配置 / 运行日志), a sticky topbar with the page
title, a theme switch (dark → light → auto), refresh, and 添加账号. Above the
account table sits a row of counters derived from the data — total / ready /
cooling / disabled / clients / models — not a hardcoded number.

Automation lives in one place: the 任务中心 view owns the master switch, the
global default hours per batch (including the balance-refresh interval), the
per-platform overrides, and the run journal. The 配置 view no longer duplicates
those controls, so `schedule.*` has a single editing surface.

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

**对话测试** sends a real request through the gateway, not through a panel-shaped
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
row carries **粘性会话** (the summed `pool.sticky_sessions` across the modules that
report a pool), the account table has an **在途** column per account
(`in_flight / in_flight_limit`; `∞` when the module reports no ceiling, `—` when it
reports no lease at all), and the sidebar's Redis tile reads
`N 客户端 / M 就绪 · 本地内存` (`· Redis 镜像` when the gateway is mirrored into
Upstash). Beside the account table the annotation appends the module's own reason
when `health.servable` is false (`不可服务：<note>`) and `N 个账号在途占满` when that
client currently has every usable account at its ceiling. The two extra keys come
from `GET /panel/api/status`, because neither `overview` nor the account records
carry `pool`/`health`/`extra`. A module that implements neither interface shows
nothing there: a missing key is rendered as blank, never as `0`.

Every chat request also prints one aligned row to the console, and the very same
line is mirrored into the **运行日志** view through the gateway's log ring:

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
the log view filters on it: 任务 / 对话 / 系统 chips over the same buffer, with the
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
the whole 按账号 table into a single `(未路由)` row. A failure still prefers the
account its own error names and falls back to the slot only when the error names
none. A module that never writes the slot is not wrong, merely unattributed.

**用量** is a real chart rather than a row of bars. `GET /panel/api/usage` returns
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

**积分构成** answers a different question — not "how much have I used", but "how
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
board** (任务中心). It lists each chore with its real progress and reward, runs
one on demand, and offers an *execute everything automatable* queue. Runs are
asynchronous (the panel returns a run id and polls), and the queue advances
**serially**: the vendors' anti-abuse checks roll back chores whose events are
bursting only seconds apart, so pace is treated as part of the protocol.

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
| trae | yes | yes | yes — loopback redirect on `127.0.0.1`, opens `www.trae.cn/authorization` | yes — `daily-checkin` (CN only) |
| zcode | yes | yes | yes — polls `POST {api}/oauth/cli/init` → `GET …/poll/{flow_id}` | no — only activation events; the claim path needs a captcha solver |
| kimi | yes | yes | yes — RFC 8628 device grant against `auth.kimi.com`; the CLI is not required | no — no such endpoint exists |
| qwenwork | yes | no — nothing on disk holds a usable token | yes — PKCE device flow | yes — `daily`, against the Sash check-in API |
| tabbit | yes | yes | yes — browser hand-off: opens the Tabbit web sign-in, then confirms it from the sidecar's model list | no — the module only talks to a local sidecar |
| minimaxcode | yes | yes | yes — RFC 8628 device grant against `account.minimax.cn`, PKCE, with the CN `user_code` variant; the desktop GUI is not required | no — the product has no check-in |
| cline | yes | yes | yes — WorkOS device-code flow, the same client id the desktop app uses | no — no such endpoint exists |
| lobsterai | yes | yes | yes — browser hand-off; the module returns the URL and watches for the credential it leaves behind | yes — a daily check-in action |
| codearts | yes | no — the IDE plugin keeps no discoverable credential file | yes — the portal's own sign-in, driven as a pending session | yes — the daily check-in |
| loomy | yes | no — nothing on disk holds a usable session | yes — one-click SMS login the module runs itself (`core.AutoLoginProvider` + `core.SMSProvider`): the panel rents a number, the module sends the code, reads it back and stores the 14-day session | yes — the first-login allowance |
| raccoon | yes | yes | yes — loopback QR/SMS page with a stdlib QR encoder; WeChat scan or SMS, phone encrypted host-side | yes — `login-points`, the desktop login grant (the 300 daily credits still have no endpoint) |
| opencode | yes | yes — the `opencode` entry of `~/.local/share/opencode/auth.json`, plus every `*.json` in `<data_dir>/import/` | yes — two realms: anonymous free (`x-api-key: public`) and Console device-code OAuth | no — Zen has no check-in and no task API |
| openrouter | yes | yes — `OPENROUTER_API_KEY`, the conventional credential files, an `env:NAME` reference, or a pasted key | yes — browser PKCE; the issued key is stored like a pasted one | no — the vendor has no check-in |

Of the **original seven** modules, six expose a login and were verified against
the live vendor endpoints from a running gateway, not just from tests: each
`POST /panel/api/clients/<name>/login` returns a real `state: "pending"` with a
working browser URL (`kimi` also returns the user code to confirm). `tabbit` is
the one whose URL belongs to a browser its *sidecar* drives: the module cannot
sign in by itself, so it hands the URL over and then watches the sidecar's model
list to learn that the browser finished. `minimaxcode` joined them with an
RFC 8628 device grant recovered from its desktop bundle; unlike the six above it
has only been exercised against a scripted transport, so a real sign-in from
this machine is still the open item.

The **seven modules added later** (`cline`, `lobsterai`, `codearts`, `loomy`,
`raccoon`, `openrouter`, `opencode`) are a different story, and the table above
says only what their code implements. None of them has been exercised against a
live account — no account exists for any of them on this machine, and for
`openrouter` and `opencode` there is no API key in the environment either — so
every wire fact in their READMEs is derived from a reference implementation or
from the vendor's own traffic and is pinned by fixtures, not by a real session.
Their READMEs mark those claims `[unverified]` rather than asserting them. The
`raccoon`, `openrouter` and `opencode` now also have login flows, but those are
hermetic-tested only (`httptest` and fake transports); no live vendor sign-in
has been completed from this machine, so a real QR scan, a real PKCE redirect
and a real device-code authorisation are still open items. The one exception is
the *unauthenticated* half of the two new backends: the model catalogues and the
endpoint-existence probes were run live, because neither vendor requires a key
to list models.

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

**Check-in** is per-account: each `core.CheckinAction` the module advertises
becomes its own button, so workbuddy shows two (the two realms are two different
rewards) while trae, zcode, minimaxcode, qwenwork and raccoon show one. The
button on an account *row* is driven by a second, live flag (`checkin_ready`)
rather than by the module-level capability bit, so a module whose accounts cannot
claim anything at the moment does not offer a click that could only fail; the
capability matrix and the bulk routes keep reading the static bit.
"全部签到" walks the enabled accounts one at a
time — deliberately serial, so a burst of upstream calls cannot look like abuse.
An already-claimed day reports success, because the operator's question is "is
today's reward in hand?".

**Sign-in** is browser-first. The panel calls `POST /login`, shows the returned
URL (and, when the flow has one, a user code the operator confirms), then polls
`GET /login/<session>` on a timer until the state goes terminal. No module asks
the operator to install a vendor CLI to sign in: kimi, qwenwork, zcode, cline
and minimaxcode are pure device/poll flows, trae runs a temporary loopback
listener to catch its OAuth callback, workbuddy polls the vendor's own state
endpoint, and tabbit hands the browser off to its sidecar and confirms from the
model list that the sign-in landed; `raccoon` serves a loopback QR/SMS page,
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
row 上游 only when the module reported it from a live fetch; every other row (a
module's own built-in catalogue, an operator-configured list, the metadata
table) is marked as local and names where it came from. `POST
/panel/api/models/refresh` is the only path that re-asks the vendor, and it does
so **only** for modules that implement the interface (`can_refresh` in the
response tells the truth for each one). `Models()` itself keeps a TTL cache so a
panel refresh does not hammer the vendor. A failed refresh must never *fabricate*
a catalogue: it returns the last good list, or the module's own built-in list
where it has one, or the operator's configured list, or nothing — workbuddy,
which has no built-in list, answers empty rather than inventing ids.

The models view carries a 倍率 column, because a credit multiplier is the one
per-model fact an operator actually shops on and it has no home in the OpenAI
shape. `core.Model.Extra` travels through both surfaces: `GET /v1/models` emits
it as an `extra` object on the entry (omitted entirely when the module published
none, so a plain catalogue stays byte-for-byte OpenAI), and `/panel/api/models`
passes the same map through. The column reads, in order: an in-force campaign
(`promo_credits` with `promo_label` and the struck-through list price, the
campaign's hover text as the tooltip), then a badge-only campaign
(`promo_label` beside the list price), then the list price (`credits`), then a
bare multiplier (`display_multiplier`, which is what tabbit publishes). A model
with none of them renders `—`. What each module puts in `Extra` is its own
business — see the per-client READMEs.

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
| `GET /panel/api/login/regions` | **kept, but per client.** The route is `…/clients/<name>/login/regions` and answers with the realms *that module* offers — an empty array for a module whose upstream has only one sign-up. The reference's version was a static whitelist of seven registration regions for workbuddy's international sign-up, which is why it could not live in the shared layer. workbuddy publishes its own two realms (`cn` 国内版, `global` 国际版) through `core.RealmLoginProvider`, and the panel draws the picker from that response instead of a hardcoded list. |
| `POST /panel/api/import/cockpit` | **served per client as `POST …/clients/<name>/import/bundle`.** The reference's path named one vendor's export, and its body is that vendor's schema (`uid`/`access_token`/`refresh_token`/`domain`), so neither the path nor the parser belongs in the shared layer. A module that can read such a document implements `core.BundleImporter` and declares `capabilities.import_bundle`; `internal/panel` forwards the bytes and never parses them. `workbuddy` is the module that reads this particular format. |
| `GET /panel/api/school/vouchers` | **kept, but per client.** The route is `…/clients/<name>/school/vouchers` and answers `501` for every module that does not implement `core.VoucherProvider`; only workbuddy can, because the 开学季 activity is workbuddy's. |

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
