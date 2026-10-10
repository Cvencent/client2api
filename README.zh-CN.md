# client2api

[English](README.md) | [简体中文](README.zh-CN.md)

一个 OpenAI 兼容网关，统一接入十六个 AI 客户端/平台：

| 路由 | 客户端 | 包装的服务 |
|---|---|---|
| `workbuddy/…` | WorkBuddy | 腾讯 WorkBuddy / CodeBuddy（国际版 + 国内版） |
| `trae/…` | Trae | TRAE SOLO / Trae CN |
| `zcode/…` | ZCode | Z.AI coding plan（上游为 Anthropic Messages） |
| `kimi/…` | Kimi | Kimi Code CLI |
| `qoder/…` | Qoder CN | Qoder CN 浏览器/桌面账号、积分、签到与对话 |
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
| `openai-compat/…` | onmiRoute | 旧兼容来源与本机 OmniRoute 桥接；原有 `openai-compat/<provider>/<model>` 路由继续可用 |
| `<source-id>/…` | 自定义中转站 | 热定义的 OpenAI 兼容平台，各自拥有 API 地址、Key 账号池、模型扫描与路由策略 |

所有能力都从 **同一套 HTTP 接口**提供：`POST /v1/chat/completions`、
`POST /v1/responses`、`GET /v1/models`、`GET /v1/status`、`GET /healthz`，
以及 `/panel/` 管理面板。

## 自定义中转站

在 **平台配置 → 添加中转站** 中填写唯一的平台 ID、显示名称和 API 地址
（如 `https://api.example.com/v1`），也可以同时填写首个 API Key。
新增、编辑、停用和删除均立即热生效，无需重启。点击平台的 **账号池** 可添加
更多 Key；点击 **扫描模型**，或账号池中的同名按钮，会用每个启用 Key 请求
`GET /models` 并保存目录。模型列表合并各 Key 的可用模型，账号表会显示各 Key
的模型权限。扫描失败保留上次成功的目录；更换 API 地址后需要重新扫描。

```json
{
  "sources": {
    "my-relay": {
      "label": "我的中转站",
      "base_url": "https://api.example.com/v1",
      "max_tokens_field": "max_tokens",
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

API 地址只接受 HTTP(S)，需要 `/v1` 等前缀的服务请一并填写。
输出上限字段可选 `max_tokens` 或 `max_completion_tokens`。平台 ID 以小写
字母开头，只含小写字母、数字和连字符，不能与内置平台或保留目录重名。
显示名称可以随时改，路由 ID 保持不变。客户端使用网关统一的入站 Key 调用
`my-relay/上游模型名`；也可以按现有规则使用裸模型名、模型组、平台优先级、
账号优先级和自动故障切换。上游模型名中的 `/` 原样保留。

Key 与扫描目录保存在各平台的 `data/<平台ID>/accounts.json`，不放进 `sources`
配置。整份备份包含平台定义和账号数据。删除平台保留账号文件，重新添加相同
ID 与 API 地址即可恢复账号池。每个自定义中转站都是**会话粘性**的：多轮对话会
持续落在已经预热上游提示缓存的同一个 Key 上，因此按缓存前缀计费的中转站不会因为
账号池轮换而重收整段前缀。粘性按“中转站 + 会话”绑定；Key 被停用、冷却或扫描目录里
没有当前模型时，这条绑定会被忽略。旧 `clients.openai-compat` 配置、存储与调用地址
继续兼容，页面将这个旧模块显示为 **onmiRoute**。

[![build](https://github.com/Cvencent/client2api/actions/workflows/go-binaries.yml/badge.svg)](https://github.com/Cvencent/client2api/actions/workflows/go-binaries.yml)
[![release](https://img.shields.io/github/v/release/Cvencent/client2api?include_prereleases)](https://github.com/Cvencent/client2api/releases)

当前版本：**0.1.33**。完整更新记录见 [CHANGELOG.md](CHANGELOG.md)。

### 近期重点（0.1.26 - 0.1.29）

* **面板统一成一套精致工作台视觉**：11 个页面共用近似 Apple 的材质、亮暗语义色、
  页面和面板过渡、统一控件状态和响应式布局，并支持减少动画模式。
* **更多平台能看到真实健康状态**：codearts、openai-compat、tabbit、lobsterai、opencode
  已补上池健康和在途统计；会话粘性会让多轮对话优先留在首次成功服务的账号上。
* **Qoder CN 补齐完整对话能力**：签名模型目录、流式对话、套餐明细、活动任务、接码自动登录、
  健康状态、池统计和会话粘性都已接通；可对话平台的“测试”会发送一次最小真实请求。
* **Windows 安装体验更安静、更稳定**：安装器不再闪现控制台窗口，升级会记住开机启动选择，
  并继续保留现有配置、账号池、用量和任务数据。
* **账号池更适合大批量账号**：默认分页，支持 20 / 50 / 100 / 200 条；可按状态筛选，
  账号、状态、模型、优先级、余额、成功/失败、在途、用量、最近成功、到期等列可排序；
  多通道账号分组不会在分页和排序时被拆散。
* **状态语义更清楚**：WorkBuddy 已把“风控”和“账号异常”拆开。风控是平台行为限制，
  重登通常解决不了；账号异常是 token/session 失效，可以重新登录修复。旧 `account_fault`
  数据升级后统一显示为“风控”，统计数字也分开计算。
* **任务中心改为账号优先**：定时任务按平台 Tab 展示，恢复探测、每日余额刷新、批量执行和
  任务看板共用账号范围；没有保存的改动会明确标出，并有统一的“保存并生效”。
* **临时状态会自动恢复**：每个平台默认每 4 小时做一次恢复探测，带 ±1 小时随机抖动；
  每天 0 点后的 30 分钟内刷新一次余额，让获得每日额度的账号自动回到调度。
* **任务看板有缓存**：已扫描过的账号直接读缓存，只拉取新账号；可手动“刷新任务状态”。
  自动化任务全部完成，或只剩人工/未解锁项时，账号标签会变绿。
* **平台路由更灵活**：平台和账号优先级都支持负数，数字越小越优先；平台还可以配置
  多个北京时间分时规则，在指定时段覆盖基础优先级。
* **用量记录更容易读懂**：最近调用带会话 ID，缺失时显示内容派生的 `d-` 推断值；
  账号列优先显示操作员备注或手机号，不再只显示 `ZCode plan JWT` 这类凭据类型。
* **平台能力持续补齐**：Qoder CN、QwenWork 积分、Tabbit 每日 3% 签到、
  Raccoon 每日 300 积分，以及 Loomy 首次登录成长任务，都对应到真实模块能力，不再是面板上的空按钮。
* **Qoder CN 支持真实对话**：模型目录与对话流直连 `gateway.qoder.com.cn`（模块自带 COSY 签名），
  账号「测试」与其它有聊天能力的平台一致，都发送一次最小真实请求才算通过。
* **面板操作统一**：顶栏刷新按钮在 11 个页面都可见，并按当前页面刷新；对话测试和
  运行日志也补上了刷新。模型上下文长度和最大输出可在“模型与档位”里查看、修改和恢复默认。

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

Windows 安装器本身以图形子系统构建，双击安装不会再额外闪出一个 CMD 窗口；
脚本方式运行 `-silent` 等参数时，如果父进程有控制台，安装器仍会把进度输出接回去。
“开机启动”的选择会跨升级保留，升级旧版本时如果检测到已有启动快捷方式，也会默认
保持勾选。

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

同一批模型也通过 **Responses 接口**提供，而这是 Codex CLI 唯一支持的协议
（`wire_api = "responses"`）：

```powershell
curl.exe -s -X POST http://127.0.0.1:8788/v1/responses `
  -H "Content-Type: application/json" `
  -d '{"model":"zcode/GLM-5.3","input":"hi","stream":true}'
```

让 Codex 直接指向本网关，中间不再需要翻译层：

```toml
model = "zcode/GLM-5.3"
model_provider = "client2api"

[model_providers.client2api]
name = "client2api"
base_url = "http://127.0.0.1:8788/v1"
wire_api = "responses"
env_key = "CLIENT2API_KEY"
```

翻译了什么、刻意不翻译什么，见下方《Responses 接口》一节。

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
| `deepseek-v4.1-flash` | `model_groups` 的组名，或组内成员的裸模型名：路由到整组成员 |
| `Auto/GLM-5.3` | 聚合所有能提供该模型的平台，并按平台优先级路由 |

模块收到的永远是**裸模型名**，不包含模块前缀。

带斜杠的请求只有第一段确实是已注册模块名时才会当作模块前缀。某些上游模型 id
自身带斜杠，例如 `nvidia/nemotron-3.5-lightning:free`，因此第一段不是模块名时，
网关会先按完整 id 查目录，再决定是否拒绝。

`Auto/<model>` 是增量能力：原来的平台专用 id 仍然保留，同时增加一个聚合名称。
这个名称走与普通请求相同的平台优先级、健康状态、冷却和故障切换。
如果必须固定某个平台，就使用显式的 `平台/模型`。

`model_groups` 是操作者自定义的等价名：把同一个模型在各平台的不同 id 写进一组
后，组名和任一成员模型名都会路由到整组。组名不分大小写且不能包含 `/`；成员
写成 `client/model`，只能属于一个组，也不能和 `aliases` 重名。显式的
`client/model` 仍然锁定平台，不会展开成整组。组内优先用该组的
`platform_priorities`；没配的平台回落到全局 `platforms.<name>.priority` 与
`priority_schedule`。成员在当前目录里缺失时会被跳过，单个平台掉线不会拖垮整组。

默认内置五个 GPT-6+ 路由组，让 Codex 可以直接使用不带平台前缀的原生小写名称：
`gpt-6-astra`、`gpt-6.1-sol`、`gpt-6-sol`、`gpt-6-luna` 和
`gpt-6.1-sol-pro`，分别映射到当前提供这些模型的 OpenCode / OpenRouter 成员。
它们会显示在**平台配置 → 模型路由组**里，并带有“内置”标记；修改其中一个会
把覆盖项写入 `model_groups`，之后以文件里的配置为准。若要在配置文件里停用某个
默认组，写入同名条目并把 `members` 设为空数组即可。

## Responses 接口

`POST /v1/responses` 用 OpenAI Responses 协议提供与 `/v1/chat/completions`
完全相同的模型。它存在只有一个原因：**Codex CLI 已经不再支持 Chat
Completions。** 它的 `WireApi` 只有一个取值 `Responses`，供应商配置块里
`wire_api = "responses"` 是唯一合法值——所以只实现 Chat Completions 的端点
会让 Codex 在第一个回合就失败。

常规做法是在 Codex 和网关之间插一层翻译中继。这能用，直到请求里带上
**加密的 agent 内容**——即子代理消息被包裹的密文。中继翻译不了解密的内容，
唯一诚实的回答就是 400，回合在还没碰到任何上游之前就结束了。本接口把这层
中继从链路里去掉：它原生说 Responses，因此不存在会失败的翻译步骤。

### 翻译了什么

| Responses 请求 | 转成 |
|---|---|
| `instructions` | 首条 `system` 消息 |
| `input` 为字符串 | 一条 `user` 消息 |
| `input` 为数组 | 有序的 `core.Message`（见下） |
| `message` 项（`user`/`assistant`/`system`/`developer`） | 一条消息；`developer` 映射为 `system` |
| `input_text` / 续接时的 `output_text` | 文本内容，保留助手历史 |
| 带 `image_url` 与 `detail` 的 `input_image` | 图片内容项 |
| `function_call` 项 | 带 `tool_calls` 的 `assistant` 回合 |
| `function_call_output` 项 | 按 `call_id` 关联的 `tool` 回合 |
| `reasoning` 项 | 其 `summary` 文本，保留为回合的推理 |
| `compaction` 项 | 其明文摘要（若其中有） |
| `tools`（扁平**或**嵌在 `function` 下） | `core.Tool` 声明 |
| 内置工具（`web_search` 等） | 跳过，不会致命 |
| `reasoning.effort` / `reasoning_effort` | 请求的思考档位，传给上游模块 |
| `max_output_tokens` | `max_tokens` |
| 未知项类型 | 跳过，不会致命 |

数组形式是两种协议真正不同的地方，翻译不是改字段名。Responses 没有
`messages` 数组：工具调用、它的结果、推理回显都是同一个扁平 `input` 列表里的
**兄弟项**。Chat 模型把同样的事实表达为「带 `tool_calls` 的 assistant 回合 +
随后按 `call_id` 关联的 `tool` 回合」，所以一个 `function_call` 开启一个
assistant 回合，与之匹配的 `function_call_output` 关闭它。工具调用之前的推理
会挂到该调用所属的回合上，这正是 Chat 模型期望找到它的位置。

### 发出了什么

流式事件序列遵循 Codex 解析器要求：

```
response.created
  response.output_item.added              （首次出现时分配稳定 ID 与索引）
  response.content_part.added / response.reasoning_summary_part.added
  response.output_text.delta …            （正文）
  response.reasoning_summary_text.delta … （推理）
  response.function_call_arguments.delta …（工具参数）
  对应的 text/arguments/part.done
  response.output_item.done               （每个 reasoning / message / function_call 一个）
response.completed                        （携带 response.id 与 usage）
```

两个细节是承重的，很容易写错。Codex 按每个 payload **内部**的 `type` 字段分发，
而不是按 SSE 的 `event:` 行，所以两者必须由同一个字符串写出、永不冲突。而
`response.output_item.done`（不是 `.added`）才是 Codex 用来分发工具调用和定稿
assistant 消息的事件——只发 delta 的流会显示文字但永远不执行工具。流中途断开
时发 `response.failed`，不会挂住。
每个输出项的 ID 与索引在 delta、`.done` 和最终输出中保持一致。
达到 token 上限时，流式与非流式的响应状态都设为 `incomplete`，
并在 `incomplete_details.reason` 中标明 `max_output_tokens`；
流式通过 `response.incomplete` 事件结束。

非流式（`"stream": false`）返回单个 `response` 对象，其 `output` 数组按顺序
包含：有推理时的 `reasoning` 项、一个 `message` 项、以及每次调用的
`function_call` 项。

### 加密的 agent 内容

内容加密的项会被**接受，而不是拒绝**——这正是本接口存在的意义。网关不去读那些
字节；它保留模型能利用的那部分事实，其余丢弃：

* `agent_message` 保留明文信封（author、recipient、任务名）与文本部分。密文替换为
  `[encrypted agent payload omitted]`。
* `reasoning` 项保留 `summary` 文本。其 `encrypted_content` 除了发给签发它的
  上游之外无法回放，因此丢弃而不转发。
* `compaction` 项在其中是明文摘要时保留，是真正的密文块时跳过。

密文**绝不**转发给任何上游，也不会被解码成杜撰的文本：网关不持有密钥，任何它
生成的内容都是伪造。因此子代理载荷的**内容**不会被回放；被保留的是「存在一条
委派消息、以及谁发给谁」这一事实。

### 限制

* **无状态。** `store` 与 `previous_response_id` 接受但忽略；调用方在 `input` 里
  回放历史——Codex 本来就是这样做的。
* **仅函数工具。** 内置工具没有可转发的函数名，会被跳过。
* **响应对象不带 `output_text` 便捷字段**；请从 `message` 项的
  `content[0].text` 读取。

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
| `model_groups` | 热更新，组名、成员名和组内优先级立即生效 |
| `panel.package_detail_limit` | 重启生效 |
| `schedule.*` | 通过 `Reconfigure` 生效，下一次唤醒时采用 |

`platforms.<name>.priority` 和 `platforms.<name>.account_priorities` 都接受负数。
数字越小优先级越高，例如 `-10` 优先于 `-5`，默认是 `0`。

`platforms.<name>.priority_schedule` 可以在固定时间段内覆盖基础优先级，例如只在
凌晨把某个平台提前：

```json
{"platforms":{"zcode":{"priority":5,"priority_schedule":[
  {"start":"01:00","end":"05:00","priority":1},
  {"start":"05:00","end":"08:00","priority":-2}
]}}}
```

规则按北京时间（UTC+8）判定，与主机时区无关；区间为左闭右开的 `[开始, 结束)`，
支持跨零点（如 `23:00`-`06:00`）。命中第一条规则就用它的优先级，没命中就用基础
`priority`。同一平台的规则不能重叠，开始和结束也不能相同；面板保存前会直接拦下。
面板平台卡片里的「分时规则」就是这套值的编辑入口。

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
| qoder | 是 | 是 | 是，浏览器设备授权 + PKCE；并支持通过阿里云 SSO 一键接码自动登录（`core.AutoLoginProvider` + `core.SMSProvider`） | 是，每日 `CLAIM_BENEFIT`，并提供活动任务板、套餐明细、健康状态和模型输出上限 |
| qwenwork | 是 | 否 | 是，PKCE 设备流 | 是，Sash 每日签到 |
| tabbit | 是 | 是 | 是，交给 Tabbit 浏览器 | 是，每日 `sign-in`（`desktop_pet` 场景，通常每日 3% 额度） |
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

面板地址：<http://127.0.0.1:8788/panel/>。左侧包含十一个视图：
账号池、对话测试、用量、平台告警、积分构成、客户端、任务中心、模型与档位、平台配置、配置、运行日志。
顶部提供主题切换、统一刷新和添加账号。

顶部刷新按钮是全局唯一入口：切换页面后仍常驻，点击时按当前页面重新读取对应数据。
账号池、用量、平台告警、积分、任务中心、模型、平台配置等卡片里重复的刷新按钮已撤掉；
“刷新凭据”“刷新余额”“重新拉取模型”等真正不同的动作仍保留。

账号池：

* 每个账号行可以单独测试、停用/恢复、刷新余额、签到、查看任务和删除。启用中的账号
  显示“停用”，其余状态显示“恢复”或“启用”，不再把启用和恢复拆成两个容易点错的按钮。
* “账号标识”记录登录手机号或邮箱，用来辨认账号和重登；它与“状态说明”不是同一列。
* “重登”只在模块确实支持登录、且账号被标志为失效时出现，就地覆盖旧凭据；支持接码的
  模块还会提供“接码重登”，但风控账号不会显示这个按钮。
* 账号池默认每页 50 条，可切换 20 / 50 / 100 / 200；分页以账号分组为单位，多通道账号
  的头和所有通道不会被拆到两页。
* 可按账号、状态、模型、优先级、余额、成功/失败、在途、用量、最近成功、到期等列排序；
  点一次升序、再点降序、第三次恢复默认。空值始终排在最后，后台轮询刷新不会把滚动位置弹走。
* 状态筛选与上方统计使用同一套账号级状态：全部、可用、冷却中、风控、账号异常、已停用、未知。
  统计数字也会按同一口径分开计算。
* 模型列显示当前可用模型数、限流模型数和账号不支持的模型数；
  点击可查看该账号所属分区的完整模型清单。
* 绿色表示可用，黄色表示冷却中并显示解除时间；红色表示风控或账号异常。风控来自平台行为
  限制，重登通常无效；账号异常来自 token/session 失效，可以重新登录修复。旧 `account_fault`
  数据升级后统一按风控显示。

任务中心：

* 定时任务按平台分 Tab，每个平台只编辑自己的任务，不再把二十个平台平铺成一整屏；
  没有自定义的行会继承共享时点并以 placeholder 回显。“全局默认”编辑块已移除，改动会显示
  “有未保存的改动”，统一走右上角“保存并生效”。
* 定时任务、批量执行和任务看板共用账号选择：先选账号，再查看或执行该账号的任务；
  同一账号的任务严格串行，不会重叠。
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

* “总览”显示成功率、输入、输出、合计、缓存命中率、总花费和平均延迟。
* 图表是可交互的内联 SVG，支持悬停和方向键查看每个时间点。
* “积分构成”按剩余有效期展示，帮助判断哪些额度会先过期。
* “最近调用”还显示缓存 tokens、该条缓存命中率和人民币花费；未知价显示 `—`，显式免费才显示 `¥0`。它会显示调用方传入的会话 ID；没有会话 ID 时会用内容派生的
  `d-` 前缀键，并标注为推断值；无法推断时显示“无会话标识”。

模型与档案：

* 每个模型显示 `context_length`、`max_output_tokens`，以及人民币/百万 tokens 的输入价、输出价、缓存读取价。
* 能力字段按“操作员手动值 > 模块上报值 > 官方预设”解析；价格同样遵循该顺序，官方默认价来自 2026-10-10 的 models.dev 快照，并按 1 美元 = 7.20 元换算。
* 缓存读取价存在时，缓存输入按缓存价计算，未缓存输入按输入价计算；没有缓存价时，全部输入都按输入价计算。未知价不会被当成 0 元。
* 手动值保存在 `data/model_context.json`，升级不会丢失，可恢复为官方默认值。`/v1/models` 仍返回上下文和最大输出字段，OpenAI 兼容客户端可直接读取。

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
