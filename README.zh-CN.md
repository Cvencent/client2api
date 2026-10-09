# client2api

[English](README.md) | [简体中文](README.zh-CN.md)

一个 OpenAI 兼容网关，统一接入十六个 AI 客户端/平台：

| 路由 | 客户端 | 包装的服务 |
|---|---|---|
| `workbuddy/…` | WorkBuddy | 腾讯 WorkBuddy / CodeBuddy（国际版 + 国内版） |
| `trae/…` | Trae | TRAE SOLO / Trae CN |
| `zcode/…` | ZCode | Z.AI coding plan（上游为 Anthropic Messages） |
| `kimi/…` | Kimi | Kimi Code CLI |
| `qoder/…` | Qoder CN | Qoder CN 浏览器/桌面账号、积分与签到（不提供对话） |
| `qwenwork/…` | QwenWork | 千问办公 / QoderWork CN 桌面端 |
| `tabbit/…` | Tabbit | Tabbit 浏览器（通过本地 sidecar） |
| `minimaxcode/…` | MiniMax Code | MiniMax Code 桌面端（上游为 Anthropic Messages） |
| `cline/…` | Cline | Cline（cline.bot），WorkOS 凭据，OpenAI 形状上游 |
| `lobsterai/…` | LobsterAI | 有道龙虾 LobsterAI，OpenAI 形状，仅 SSE |
| `codearts/…` | CodeArts | 华为 CodeArts IDE 模型服务（SDK-HMAC-SHA256） |
| `loomy/…` | Loomy | 讯飞 Loomy，账号接口使用 HMAC-SHA1 签名 |
| `raccoon/…` | Raccoon | 商汤小浣熊 Raccoon Work |
| `openrouter/…` | OpenRouter | OpenRouter（openrouter.ai），模型 id 可包含斜杠；默认只路由免费模型（`free_only`） |
| `opencode/…` | OpenCode Zen | OpenCode Zen（opencode.ai），在 Anthropic、Google 和 OpenAI 原生模型前提供统一 OpenAI 形状 |
| `openai-compat/…` | OpenAI 兼容来源 | 一个配置驱动的模块，可接 Groq、Cerebras、SiliconFlow、Mistral、NVIDIA NIM、Together、Fireworks、DeepInfra、Chutes、HuggingFace；路由格式是 `openai-compat/<provider>/<model>` |

所有能力都从 **同一套 HTTP 接口**提供：`POST /v1/chat/completions`、
`GET /v1/models`、`GET /v1/status`、`GET /healthz`，以及 `/panel/` 管理面板。

[![build](https://github.com/Cvencent/client2api/actions/workflows/go-binaries.yml/badge.svg)](https://github.com/Cvencent/client2api/actions/workflows/go-binaries.yml)
[![release](https://img.shields.io/github/v/release/Cvencent/client2api?include_prereleases)](https://github.com/Cvencent/client2api/releases)

当前版本：**0.1.24**。完整更新记录见 [CHANGELOG.md](CHANGELOG.md)。

## 核心设计规则

> **每个客户端都是独立模块，修改一个模块不能影响另一个。**

这不是口头约定，而是由结构强制保证：

```text
        cmd/  ──┐
    internal/  ─┴──►  internal/core   ◄──  clients/<name>
```

* `internal/` 和 `cmd/` 下的代码不能导入任何 `clients/*` 包。
* 一个 `clients/*` 包不能导入另一个 `clients/*` 包。
* 每个模块通过 `init()` 里的 `core.Register` 注册自己。
* `clients/all/all.go` 是**唯一**知道完整模块列表的文件，里面只有若干空导入。
* `cmd/client2api/main.go` 逐个构造模块；**某个模块构造失败时只记录并跳过**，进程继续用其余模块运行。
* 每个模块的状态只保存在自己的 `data/<name>/` 目录。

因此，新增客户端只需要创建 `clients/<name>/` 并加一行导入；删除客户端只需要删目录和导入；
重写某个客户端时不需要看其他模块。

模块接口见 [`docs/MODULE-CONTRACT.md`](docs/MODULE-CONTRACT.md)，
协议记录见 [`docs/upstream/<name>.md`](docs/upstream)；没有独立协议文档的模块会在
自己的 `clients/<name>/README.md` 里说明。

## 快速开始

### Windows 安装

从 [Releases](https://github.com/Cvencent/client2api/releases) 下载最新的
`client2api-setup-<version>.exe`，双击安装。安装完成后打开托盘菜单，或访问：

```text
http://127.0.0.1:8790/panel/
```

端口可以在托盘菜单的“设置”里修改。

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

也可以直接使用 Docker：

```sh
docker compose up -d --build
docker compose logs -f
```

### 数据安全

`data/` 包含账号凭据和运行状态，`configs/client2api.json` 包含本机配置；
两者都已加入 `.gitignore`，不会进入源码仓库。对外分发安装包时使用：

```powershell
pwsh -File installer\build.ps1 -NoData
```

`-NoData` 会生成不含账号和本机配置的空数据包。升级时安装器会保留现有的
`configs/client2api.json`、`data/`、账号池、用量和任务数据。

## 发布与更新

每次发布安装包时，必须同步完成：

1. 更新 `cmd/client2api/main.go` 与 `installer/setup/main.go` 的版本号。
2. 在 [CHANGELOG.md](CHANGELOG.md) 写明新增、修复和行为变化。
3. 创建同版本 Git tag 和 GitHub Release，并把安装包与 SHA256 作为发布资产。
4. 在 Release 说明中写清楚升级方式、是否需要重新登录、是否涉及数据迁移。

## 构建

工作区的 Go 工具链放在 `D:\client2api-lab\_tools`。每个新 shell 先设置：

```powershell
$tools='D:\client2api-lab\_tools'
$env:PATH="$tools\go\bin;$env:PATH"
$env:GOCACHE="$tools\gocache"; $env:GOTMPDIR="$tools\gotmp"; $env:GOMODCACHE="$tools\gomodcache"
$env:GOFLAGS='-mod=mod'
$env:GOPROXY='off'
$env:CGO_ENABLED='0'
Set-Location 'D:\client2api-src'
```

常用命令：

```powershell
go vet ./...
go build ./...
go test ./... -count=1
go build -o client2api.exe ./cmd/client2api
```

`make` 包装了同样的检查（另加 `gofmt` 和 race detector），构建产物进入 `bin/`：

```bash
make check          # gofmt -l、go vet、go test，与 CI 同一套门禁
make race           # 带 race detector 的测试，需要 CGO_ENABLED=1
make build          # bin/client2api + bin/probe
make run            # 构建并启动网关
```

版本号只有一个来源：`cmd/client2api/main.go` 里的 `var version`。
本地构建和 tagged release 都从这里读取，不会出现版本不一致。

## 运行

```powershell
.\client2api.exe -config configs\client2api.json
# 或者
.\client2api.exe -listen 127.0.0.1:8788 -proxy http://192.168.0.217:18867
```

参数：`-config`、`-listen`、`-data-dir`、`-proxy`、`-list-clients`。

健康检查与接口：

```powershell
curl.exe -s http://127.0.0.1:8788/healthz
curl.exe -s http://127.0.0.1:8788/v1/models
curl.exe -s http://127.0.0.1:8788/v1/status
curl.exe -s -X POST http://127.0.0.1:8788/v1/chat/completions `
  -H "Content-Type: application/json" `
  -d '{"model":"zcode/GLM-5.3","messages":[{"role":"user","content":"hi"}],"stream":true}'
```

面板地址：<http://127.0.0.1:8788/panel/>。

## 部署

### Docker

```sh
mkdir -p configs data && sudo chown -R 10001:10001 configs data
docker compose up -d --build
docker compose logs -f | grep 'generated api_key'
```

镜像是一个静态二进制，面板内嵌，不需要 Node，也没有第二个进程或外挂资源。
镜像**故意不打包配置文件**：首次启动时程序会生成随机 `api_key`，写入
`configs/client2api.json` 并打印到日志，读取后再填进面板配置页。

容器内默认监听 `0.0.0.0:8788`。`/healthz` 在没有可用账号时返回 **503**，
所以“端口开着但没有可用账号”的容器会被报告为 `unhealthy`。

### Release 压缩包

`.github/workflows/go-binaries.yml` 会在每次推送到 `main` 时构建五个平台：
Windows/Linux/macOS amd64 以及 Linux/macOS arm64，并验证二进制中的版本号。
推送 `v*` tag 时会发布 zip/tar.gz、`checksums.txt` 和英文/中文 README。

压缩包里包含 `configs/client2api.example.json`。复制为
`configs/client2api.json` 后，**启动前必须填写 `api_key`**；留空会让网关和面板都不做鉴权。

### Windows 安装包

```powershell
pwsh -File installer\build.ps1            # 带当前配置和 data/
pwsh -File installer\build.ps1 -NoData    # 空数据包，可对外分发
```

版本号从 `cmd/client2api/main.go` 读取，并注入两个二进制；
`go test ./installer/...` 会检查 `installer/setup/main.go` 是否漂移。

打包过程会运行真实浏览器面板冒烟测试：启动网关、用 headless Chrome/Edge 加载
`/panel/`，要求页面出现 `<html data-c2a-ready="1">`。失败时拒绝产出安装包。
安装到已有目录时会替换程序文件，但完整保留 `configs/client2api.json` 和 `data/`。

## 凭据处理

模块从各客户端自己的本地存储发现凭据，并保存到 `data/<name>/`。
任何会输出 `core.Status` 的出口（`GET /v1/status` 和 `/panel/api/status`）
都会经过 `core.RedactStatus`，屏蔽 JWT 形状字符串、`Bearer …` 和
`key=value` 凭据对。不是密钥的标识（账号号、`cli-login`、
`zcode-config:builtin:bigmodel`）会原样通过。

这是纵深防御，不是唯一防线：模块本身不应把凭据放进 `Status`。

### ZCode 账号与额度

一个 Zhipu 账号可能同时有计划 JWT、coding-plan API key 和 OAuth 派生 key。
面板按账号身份把它们聚合成“一个账号、多条通道”，不会显示成三个独立账号。

网页登录有两个通道：

* 国际版：Z.AI（`chat.z.ai`）
* 国内版：BigModel（`bigmodel.cn`）

这是两个不同服务，同一张手机号不能混用。国内号必须在“国内版”页面登录。
登录后的账号带 `realm` 标签区分国内/国际。

额度来自两套接口：`open.bigmodel.cn` 的滚动窗口和每日 Token 限制，
以及 `zcode.z.ai` 的活动套餐、Start Plan 余额。活动套餐领取只走 JWT 通道；
coding-plan API key 行不会显示领取按钮。领取成功后，之前因额度耗尽留下的
`exhausted` 判定会清除，账号立即回到调度；余额刷新读到有额度时也会清除。

## 模型路由

支持四种写法：

| 写法 | 含义 |
|---|---|
| `zcode/GLM-5.3` | 显式指定模块 `zcode` 和模型 `GLM-5.3` |
| `GLM-5.3` | 在唯一声明该模型的模块中路由（歧义时返回 400 并列出候选） |
| `gpt-4o` | 使用配置里的 `aliases` |
| `Auto/GLM-5.3` | 聚合所有能提供该模型的平台，并按平台优先级路由 |

模块收到的永远是**裸模型名**，不包含模块前缀。

带斜杠的请求只有第一段确实是已注册模块名时才会当作模块前缀。某些上游模型 id
自身带斜杠，例如 `nvidia/nemotron-3.5-lightning:free`，因此第一段不是模块名时，
网关会先按完整 id 查目录，再决定是否拒绝。

`Auto/<model>` 是增量能力：原来的平台专用 id 仍然保留，同时增加一个聚合名称。
这个名称走与普通请求相同的平台优先级、健康状态、冷却和故障切换。
如果必须固定某个平台，就使用显式的 `平台/模型`。

## 配置

`configs/client2api.json` 的核心键和可选配置段如下：

```json
{
  "listen": "127.0.0.1:8788",
  "api_key": "",
  "data_dir": "data",
  "proxy": "",
  "aliases": { "gpt-4o": "trae/custom_model_gpt-5" },
  "disabled": [],
  "clients": {
    "<name>": {}
  },
  "features": { "sanitize_blacklist_fingerprints": true },
  "cooldown": { "soft_rate": "600s", "soft_rate_max": "2h" },
  "prompt": { "mode": "passthrough", "file": "" },
  "schedule": { "enabled": false }
}
```

* `api_key` 为空表示**不校验入站请求**。只要端口不只监听 loopback，就应该设置。
* `clients.<name>` 原样交给模块，每个模块自己定义结构和文档。
* `proxy` 会应用到所有模块的厂商请求。带指纹客户端的模块在设置代理时强制使用
  HTTP/1.1，因为 `x/net/http2` 无法穿过该代理。

### 环境变量覆盖

部署级配置可以用环境变量覆盖。`CLIENT2API_*` 是本项目命名，
参考项目的 `WB2A_*` 也可作为别名；两者同时存在时 `CLIENT2API_*` 优先。

| 环境变量 | 配置键 |
|---|---|
| `CLIENT2API_LISTEN` | `listen` |
| `CLIENT2API_API_KEY` | `api_key` |
| `CLIENT2API_DATA_DIR` | `data_dir` |
| `CLIENT2API_PROXY` | `proxy` |
| `CLIENT2API_SOFT_RATE` / `CLIENT2API_SOFT_RATE_MAX` | `cooldown.soft_rate` / `soft_rate_max` |
| `CLIENT2API_PROMPT_MODE` / `CLIENT2API_PROMPT_FILE` | `prompt.mode` / `prompt.file` |
| `CLIENT2API_EXPIRING_SOON` / `CLIENT2API_PREFER_EXPIRING` | `pool.expiring_soon` / `pool.prefer_expiring` |
| `CLIENT2API_SANITIZE_FINGERPRINTS` | `features.sanitize_blacklist_fingerprints` |

优先级是：**默认值 < 配置文件 < 环境变量**。空值等于未设置；无法解析的值会忽略，
配置文件中的值继续生效。

### 配置段与热更新

| 配置段 | 状态 |
|---|---|
| `features.sanitize_blacklist_fingerprints` | 热更新，默认开启 |
| `cooldown.soft_rate` / `soft_rate_max` | 热更新 |
| `prompt.mode` / `prompt.file` | 热更新，仅 WorkBuddy 响应 |
| `pool.*` | 热更新，主要由 WorkBuddy 执行 |
| `session_sticky.enabled` / `ttl` / `gc_interval` | 热更新，影响所有绑定会话的模块 |
| `platforms.<name>.*` | 热更新，优先级、模型黑名单、并发上限立即生效 |
| `panel.package_detail_limit` | 重启生效 |
| `schedule.*` | 通过 `Reconfigure` 生效，下一次唤醒时采用 |

`platforms.<name>.priority` 和 `platforms.<name>.account_priorities` 都接受负数。
数字越小优先级越高，例如 `-10` 优先于 `-5`，默认是 `0`。

## 错误映射

错误统一为 OpenAI 形状：

```json
{"error":{"message":"…","type":"…","code":"…","gateway_hint":"…"}}
```

| 模块返回 | HTTP | `error.code` |
|---|---|---|
| `core.ErrNotConfigured` | 503 | `no_healthy_account` |
| `core.ErrUnsupported` | 400 | `invalid_request` |
| `core.ErrBusy` | 429 + `Retry-After: 1` | `rate_limit_exceeded` |
| 上下文超时 | 504 | `upstream_timeout` |
| `FailureWAF` | 403 | `waf_ip_blocked` |
| `FailureRateLimited` | 429 | `rate_limit_exceeded` |
| `FailureQuota` | 429 | `quota_exhausted` |
| `FailureAuth` | 401 | `account_auth_failed` |
| `FailureSessionDead` | 401 | `session_dead` |
| `FailureContentBlocked` | 400 | `content_blocked` |
| 无效入站 Bearer | 401 | `invalid_api_key` |
| 本地限制 | 5xx | `server_error` |
| 其他 | 502 | `upstream_error` |

流式请求中途失败也使用同一套错误框架。`core.ErrBusy` 是背压而非故障：
所有可用账号都达到并发上限时应重试，而不是认为路由损坏。

## 模块状态

| 模块 | 许可与来源 | 说明 |
|---|---|---|
| workbuddy | MIT 上游，移植 | 见 `clients/workbuddy/README.md` |
| trae | MIT 上游，移植 | 见 `clients/trae/README.md` |
| zcode | AGPL 上游，**净室重写** | 见 `clients/zcode/README.md` |
| kimi | MIT 上游，移植 | 见 `clients/kimi/README.md` |
| qoder | 公共 OpenAPI + 观察到的桌面凭据格式，未复制上游代码 | 见 `clients/qoder/README.md` |
| qwenwork | **无许可证**，**净室重写** | 见 `clients/qwenwork/README.md` |
| tabbit | GPL-3.0 上游，**仅 sidecar，不复用代码** | 见 `clients/tabbit/README.md` |
| minimaxcode | **无上游**，**净室重写** | 见 `clients/minimaxcode/README.md` |
| cline | **净室实现**，协议由流量重建 | 见 `clients/cline/README.md` |
| lobsterai | **净室实现** | 见 `clients/lobsterai/README.md` |
| codearts | MIT 上游，移植（仅线路协议） | 见 `clients/codearts/README.md` |
| loomy | MIT 上游，**净室重写** | 见 `clients/loomy/README.md` |
| raccoon | MIT 上游，移植（仅线路协议） | 见 `clients/raccoon/README.md` |
| openrouter | 公共 REST API，未阅读上游代码 | 见 `clients/openrouter/README.md` |
| opencode | 公共 REST API；阅读了其开源服务端代码 | 见 `clients/opencode/README.md` |
| openai-compat | 公共 REST API，未阅读上游代码 | 见 `clients/openaicompat/README.md` |

各模块能力矩阵：

| 模块 | 管理 | 导入 | 登录 | 签到 |
|---|---|---|---|---|
| workbuddy | 是 | 是 | 是，轮询厂商登录状态 | 是，国内 `daily-checkin` / 国际 `daily-activity` |
| trae | 是 | 是 | 是，本机 loopback 回调 | 是，仅国内 `daily-checkin` |
| zcode | 是 | 是 | 是，设备轮询 | 是，支持浏览器验证码 |
| kimi | 是 | 是 | 是，RFC 8628 设备授权 | 否 |
| qoder | 是 | 是 | 是，浏览器设备授权 + PKCE | 是，每日 `CLAIM_BENEFIT` |
| qwenwork | 是 | 否 | 是，PKCE 设备流 | 是，Sash 每日签到 |
| tabbit | 是 | 是 | 是，交给 Tabbit 浏览器 | 否 |
| minimaxcode | 是 | 是 | 是，RFC 8628 设备授权 | 否 |
| cline | 是 | 是 | 是，WorkOS 设备码 | 否 |
| lobsterai | 是 | 是 | 是，浏览器交接 | 是，每日签到 |
| codearts | 是 | 否 | 是，门户登录 | 是，每日签到 |
| loomy | 是 | 否 | 是，一键短信登录 | 是，首次登录奖励 |
| raccoon | 是 | 是 | 是，本机二维码/短信页 | 是，领取每日 300 积分（桌面登录发放接口） |
| opencode | 是 | 是 | 是，匿名免费或 Console 设备码 | 否 |
| openrouter | 是 | 是 | 是，浏览器 PKCE | 否 |
| openai-compat | 是 | 否 | 否 | 否 |

能力标志全部来自类型断言（`core.CapabilitiesOf`）。模块没有实现某个接口，
面板就不会显示对应入口；核心层不需要因为模块增减能力而改变。

## 面板

面板地址：<http://127.0.0.1:8788/panel/>。左侧包含九个视图：
账号池、对话测试、用量、积分构成、客户端、任务中心、模型与档案、配置、运行日志。
顶部提供主题切换、刷新和添加账号。

账号池：

* 每个账号行可以单独测试、启用、停用、恢复和删除。
* “备注”记录登录手机号或邮箱，模块的厂商接口经常只返回昵称。
* “重登”只在模块确实支持登录、且账号被标志为失效时出现，就地覆盖旧凭据。
* 支持分页、按余额排序、批量刷新和批量签到。
* 模型列显示当前可用模型数、限流模型数和账号不支持的模型数；
  点击可查看该账号所属分区的完整模型清单。
* 绿色表示可用，黄色表示限流并显示解除时间，红色表示账号不支持。

任务中心：

* 统一管理定时任务开关、每个批次的默认时间、平台覆盖和运行记录。
* 模块通过 `core.PlannedBatches` 声明任务；没有批次但实现了
  `core.CheckinProvider` 的模块会自动生成一个 `checkin` 批次。
* 时间使用 Asia/Shanghai 时区；空列表表示永不运行该批次。
* 每个平台都有一条“恢复探测”任务。默认每 4 小时探测一次，随机范围为 ±1 小时
  （即 3–5 小时触发一次），可填 `4h`、`240m` 这类分钟/小时值；平台行可以单独覆盖间隔
  和随机范围。
* 恢复探测会重新检查冷却、额度耗尽、限流等临时状态的账号，并顺带续期即将过期的凭据；
  高频余额刷新会避开这些账号，避免频繁打扰厂商。手动测试、刷新余额或点“立即执行”
  仍然立即检查。
* 每个平台还有一条「每日余额刷新」任务，默认每天 0 点后的 30 分钟内随机触发一次。
  带每日额度的平台在跨天后会把额度补给账号，这次扫描会刷新包括冷却/额度耗尽在内的所有
  账号余额，让刚回满积分的账号自动恢复调度，不用手动点测试。可用 `daily_balance_hours`
  改触发时点（留空 = 不跑），平台行可以单独覆盖。
* 同一账号的任务严格串行，不会重叠；不同账号之间默认间隔 45 秒，避免厂商反滥用检查回滚。
  Loomy 的首次登录/新手任务使用独立的 10 秒 `TaskGap`（带 ±20% 抖动，约 8–12 秒），
  避免多个新手任务在过短时间内完成。
* 任务看板会把每个账号最近一次任务列表写入 `data/panel/task_board_cache.json`。
  再次打开时直接显示缓存，只有新账号才会主动向平台拉取；点“刷新任务状态”可强制重新拉取
  当前平台的全部账号或当前选中账号。某个账号已经没有可自动执行的待办（全部完成，
  或只剩需人工/未解锁项）时，账号卡片会变绿。

用量：

* “总览”显示成功率、输入、输出、合计和平均延迟。
* 图表是可交互的内联 SVG，支持悬停和方向键查看每个时间点。
* “积分构成”按剩余有效期展示，帮助判断哪些额度会先过期。
* “最近调用”显示调用方传入的会话 ID；没有会话 ID 时会用内容派生的
  `d-` 前缀键，并标注为推断值；无法推断时显示“无会话标识”。

模型与档案：

* 每个模型显示 `context_length` 和 `max_output_tokens`。
* 解析顺序是：操作员手动值 > 模块上报值 > `internal/modelmeta/table.json` 官方预设。
* 手动值保存在 `data/model_context.json`，升级不会丢失，可恢复为官方默认值。
* `/v1/models` 同时返回这两个字段，OpenAI 兼容客户端可直接读取。

面板鉴权使用顶层 `api_key`。为空时不做鉴权；设置后所有 `/panel/api/*`
都要求 `Authorization: Bearer <key>` 或 `X-Api-Key: <key>`。
HTML 外壳不拦截，因为页面必须先加载才能输入密钥。

## 面板 API

所有接口按客户端作用域划分：

```text
GET    /panel/api/status
GET    /panel/api/clients
POST   /panel/api/reload
GET    /panel/api/overview
GET    /panel/api/logs?lines=N
GET    /panel/api/models
POST   /panel/api/models/refresh
GET    /panel/api/model_probes
GET    /panel/api/usage?window=HOURS
POST   /panel/api/usage/save
GET    /panel/api/config
POST   /panel/api/config
GET    /panel/api/clients/<name>/capabilities
GET    /panel/api/clients/<name>/accounts
POST   /panel/api/clients/<name>/accounts
DELETE /panel/api/clients/<name>/accounts/<id>
POST   /panel/api/clients/<name>/accounts/<id>/enabled
POST   /panel/api/clients/<name>/accounts/<id>/test
POST   /panel/api/clients/<name>/accounts/<id>/checkin
POST   /panel/api/clients/<name>/accounts/<id>/revive
POST   /panel/api/clients/<name>/accounts/<id>/balance
GET    /panel/api/clients/<name>/balances
POST   /panel/api/clients/<name>/balances/refresh
GET    /panel/api/clients/<name>/login/regions
POST   /panel/api/clients/<name>/login
GET    /panel/api/clients/<name>/login/<session>
DELETE /panel/api/clients/<name>/login/<session>
POST   /panel/api/clients/<name>/import
POST   /panel/api/clients/<name>/import/bundle
```

模块没有实现对应接口时返回 `501`，并说明不能做什么。

## 探测输出上限

厂商模型列表里的 `max_output_tokens` 往往只是声明值，不一定是真实上限。
`cmd/probe` 会实际发起请求，逐步提高输出长度，读取 `finish_reason` 和
`usage.completion_tokens`，把测量结果写到 `data/output_probes.json`。

```sh
go build -o probe ./cmd/probe
./probe --base http://127.0.0.1:8788/v1 --key "$CLIENT2API_KEY" \
        --models glm-5.2 --tiers 40000,100000 \
        --panel-out data/output_probes.json
```

`--base` 应指向本网关，而不是厂商。探测结果会合并写入，重新测两个模型不会丢掉其
他模型的记录；文件损坏时会重建，写入使用临时文件加 rename，面板不会读到半个文档。
