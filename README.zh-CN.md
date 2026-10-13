# client2api

[English](README.md) | [简体中文](README.zh-CN.md) | [使用教程](TUTORIAL.zh-CN.md)

[![build](https://github.com/Cvencent/client2api/actions/workflows/go-binaries.yml/badge.svg)](https://github.com/Cvencent/client2api/actions/workflows/go-binaries.yml)
[![release](https://img.shields.io/github/v/release/Cvencent/client2api?include_prereleases)](https://github.com/Cvencent/client2api/releases)

当前版本：**0.1.43**。完整更新记录见 [CHANGELOG.md](CHANGELOG.md)。

流式断流排查：升级后系统日志新增 `stream: id=...` 终止诊断，包含协议、平台/模型、
上游事件数、`finish_reason`、正文/推理字节数、工具调用数、completion token、
首包耗时、总耗时及终止原因；达到输出上限时标记为 `terminal=incomplete`。
日志不记录对话正文或凭据。

client2api 是一个**个人自用**的 OpenAI 兼容网关。它把你自己在各个 AI 客户端/平台上拥有的账号和 API Key 聚合起来，对外只提供一套统一的 OpenAI 风格接口和网页管理面板。

> **仅限个人自用、学习使用。**
> 本项目不面向公网、不做共享中转、不提供服务器架设方案。请只在自己机器上运行，不要暴露到互联网，不要拿它当作给他人提供服务的平台。程序默认只监听 `127.0.0.1`。
>
> 使用本项目时，请遵守每个上游平台的账号规则、服务条款和 API 政策。不要用它绕过额度限制、转卖访问权限或架设公开代理服务。

![账号池](docs/images/accounts.png)

## 项目定位

- 聚合你自己拥有的账号和 Key，而不是提供一个公共账号池。
- 用一个入站 API Key 统一调用 `POST /v1/chat/completions` 和 `POST /v1/responses`。
- 为带语音能力的平台提供 `POST /v1/audio/transcriptions`：上传 WAV/FLAC/MP3/MP4/M4A/OGG/WebM 可返回 `{"text": "..."}`、纯文本、SRT、verbose JSON 或 VTT。
- 在平台、账号、模型别名、`Auto/<模型>` 和模型路由组之间统一调度。
- 平台支持会话粘性时，同一会话会尽量落在同一个账号上。
- 统一处理账号优先级、冷却、并发限制、重试和故障切换。
- 通过网页面板管理账号、任务、用量、模型、积分、日志和配置。
- 支持 Windows 安装包、便携发布包和源码本地构建。
- 凭据和运行数据都只保存在本机 `data/` 目录。

## 支持的平台

| 路由 | 客户端 / 平台 | 上游服务 |
|---|---|---|
| `workbuddy/...` | WorkBuddy | 腾讯 WorkBuddy / CodeBuddy（国际版 + 国内版） |
| `trae/...` | Trae | TRAE SOLO / Trae CN |
| `zcode/...` | ZCode | Z.AI coding plan |
| `kimi/...` | Kimi | Kimi Code CLI |
| `qoder/...` | Qoder CN | Qoder CN |
| `qwenwork/...` | QwenWork | 千问办公 / QoderWork CN |
| `tabbit/...` | Tabbit | Tabbit 浏览器 |
| `minimaxcode/...` | MiniMax Code | MiniMax Code |
| `cline/...` | Cline | Cline |
| `lobsterai/...` | LobsterAI | 有道龙虾 LobsterAI |
| `codearts/...` | CodeArts | 华为 CodeArts |
| `loomy/...` | Loomy | 讯飞 Loomy |
| `raccoon/...` | Raccoon | 商汤小浣熊 Raccoon Work |
| `openrouter/...` | OpenRouter | OpenRouter |
| `opencode/...` | OpenCode Zen | OpenCode Zen |
| `kilocode/...` | Kilo Code | Kilo AI Gateway |
| `doubao/...` | Doubao | 字节跳动 豆包（桌面客户端 / 网页） |
| `doubaoime/...` | Doubao IME | 豆包输入法在线语音转写（仅转写） |
| `openai-compat/...` | onmiRoute | 旧兼容来源 |
| `<source-id>/...` | 自定义中转站 | 自己添加的任意 OpenAI 兼容平台 |

## 功能总览

### 网关与路由

- OpenAI 兼容的 `POST /v1/chat/completions` 和 `POST /v1/responses`。
- OpenAI 兼容的 `POST /v1/audio/transcriptions`（multipart 的 `file` + `model`）；
  `GET /v1/models` 会把语音平台的模型一并列出并标记 `extra.kind = "transcription"`。
- `GET /v1/models`、`GET /v1/status`、`GET /healthz`。
- 所有平台共用一个入站 API Key，面板可选本地鉴权。
- 支持 `平台/模型`、裸模型名、别名和 `Auto/<模型>` 聚合路由。
- 支持自定义 `model_groups`，把同一模型在各平台的不同名字绑成一组。
- 平台优先级、账号优先级和按北京时间生效的分时优先级。
- 平台级、账号级、账号内并发限制。
- 会话粘性、重试、冷却和自动故障切换；面板会列出粘性会话使用的模型，可强制解绑账号，也可在强制切换时指定目标平台；不指定时则清除账号和平台粘性并重新按优先级选择。
- 统一把鉴权、额度、WAF、超时、限流等错误映射为 OpenAI 形状。
- 思考档位按“平台 + 模型”解析：优先级为手动配置、模块缓存目录、内置预设；
  “模型与档位”页可以查看和修改支持的档位及默认档，手动值优先于可能过期的模块缓存。
- “封顶对齐”对内置表里所有 V4.1 Flash 条目默认开启，其余模型默认关闭；面板仍可
  按模型手动关闭或恢复默认。开启后，只有请求档位为 `xhigh` 且模型真实最高档更高时
  才会提升（例如 WorkBuddy、Cline、OpenRouter 的 `max`）；较低档位、最高档本来就是 `xhigh`
  的模型（例如 Loomy）都不改，不会下发厂商从未公布的档位。
  用量记录同时保留请求值和实际下发值，不同时显示为 `xhigh → max`。

### 账号与平台

- 账号池支持启用/停用、备注、身份标识、优先级和余额显示。
- 支持手动添加、本地凭据发现、整包导入，以及模块支持时的浏览器/设备登录。
- 支持重新登录、恢复、测试、签到、刷新余额和删除账号。
- ZCode 的 JWT 与 coding-plan API Key 会按同一账号聚合为多条通道。
- 支持动态添加 OpenAI 兼容中转站，每个平台有自己的 API 地址和 Key 池。
- 中转站支持扫描模型；扫描失败时保留上一次成功的模型目录。
- 中转站余额刷新支持 `balance_protocol`：`auto`（默认）依次探测 New API `GET /api/usage/token/`、Sub2API `GET /v1/usage`、New API 控制台 `GET /api/user/self`（仅在配置控制台凭据时），最后是 OpenAI Billing 的 `credit_grants` 与 `subscription`。也可固定为 `newapi`、`sub2api`、`openai_billing`，或设为 `none` 关闭查询。
- `balance_quota_per_usd`（默认 `500000`）用于把 New API 的额度单位换算成美元；Sub2API 和 OpenAI Billing 直接按美元读取。
- 账号高级字段 `balance_access_token` 和 `balance_user_id` 可提供 New API 控制台凭据，用于普通 API Key 读不到余额的情况。

### 网页面板

- 账号池
- 账号池首页可点击「粘性会话」，查看实时的账号/平台绑定及其使用的模型，并可强制解绑账号或指定目标平台切换
- 对话测试
- 用量
- 平台告警
- 积分构成
- 客户端
- 任务中心
- 模型与档位
- 平台配置
- 配置
- 运行日志

### 任务、用量与计费

- 定时任务按平台分 Tab，使用北京时间，可逐平台覆盖；间隔支持秒/分钟/小时和对称随机抖动。
- 每日余额刷新支持按时刻、按间隔或 5/6 段 Cron 表达式。
- 批量执行和任务看板，覆盖模块支持的各种账号任务。
- 恢复探测、每日余额刷新和签到任务。
- 成功率、延迟、输入/输出 tokens、缓存命中率和最近调用。
- 每条调用显示会话 ID，并支持按模型、按账号统计。
- 人民币费用估算、厂商官方价表、models.dev 回退和手动价格覆盖。
- 积分构成按剩余有效期显示，并有套餐/客户端视图。
- 平台告警和结构化运行日志。

### Responses API

`POST /v1/responses` 是给 Codex 使用的原生 Responses 协议入口，支持：

- `instructions`、字符串或数组形式的 `input`、消息、文本和图片。
- 工具/函数调用及函数结果。
- 命名空间工具声明（`type: "namespace"`，Codex 现在用来暴露 MCP 服务器工具的形状）：
  子工具摊平后发给上游，回包时再拆回 Codex 派发所需的 `(namespace, name)` 二元组。
- 推理摘要和加密 agent 内容处理。
- Codex 解析器要求的流式事件顺序。
- 无状态调用和 `previous_response_id` 兼容行为。

完整翻译规则和限制见详细参考文档。

## 界面截图

| 账号池 | 平台配置 |
|---|---|
| ![账号池](docs/images/accounts.png) | ![平台配置](docs/images/platforms.png) |

| 模型与档位 | 用量 |
|---|---|
| ![模型](docs/images/models.png) | ![用量](docs/images/usage.png) |

| 任务中心 | 对话测试 |
|---|---|
| ![任务中心](docs/images/tasks-center.png) | ![对话测试](docs/images/chat.png) |

## 快速开始

### Windows 安装包

从 [Releases](https://github.com/Cvencent/client2api/releases) 下载最新的
`client2api-setup-<version>.exe`，双击安装，然后打开：

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

默认只监听 `127.0.0.1`。不要让两个实例共用同一个 `data/` 目录，也不要在已安装实例使用端口时再从源码目录启动一个开发实例。

## 文档

- [使用教程](TUTORIAL.zh-CN.md)
- [详细参考文档（中文）](docs/REFERENCE.zh-CN.md)
- [Detailed reference (English)](docs/REFERENCE.md)
- [模块接口契约](docs/MODULE-CONTRACT.md)
- [上游协议记录](docs/upstream)
- [更新记录](CHANGELOG.md)

## 许可与使用范围

本仓库面向个人使用、学习和研究。它不授予任何搭建公网或商业中转服务的权利，也不会改变任何上游平台的服务条款。请先阅读 [LICENSE](LICENSE)。
