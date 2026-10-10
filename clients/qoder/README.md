# `qoder` client

`qoder` 接入的是 **Qoder CN**（`qoder.com.cn` / `qoder.cn`）。它负责浏览器登录、账号池、积分余额、每日签到和本地凭据导入。

账号有两条来源：面板里的浏览器登录（推荐，不需要装客户端），以及从桌面客户端导入的本地凭据。

它同时提供对话能力。Qoder CN 的推理网关 `gateway.qoder.com.cn` 要求逐请求的 COSY 签名；本模块在 `cosy.go` 里实现了这套签名（AES 加密 + RSA 签名 + MD5 摘要），所以模型目录、对话流都能直接调用，不需要客户端常驻。

## 能力

| 能力 | 是否支持 | 说明 |
| --- | --- | --- |
| 账号池 | 支持 | 浏览器登录、导入桌面客户端凭据，或手动粘贴设备令牌 |
| 浏览器登录 | 支持 | 面板生成厂商登录链接，浏览器确认后账号自动落地（设备授权 + PKCE） |
| 接码自动登录 | 支持 | 面板租号后自动驱动阿里云 SSO 手机号表单，读取短信验证码并完成设备授权 |
| 余额查询 | 支持 | 读取 `GET /api/v2/quota/usage` |
| 每日签到 | 支持 | 活动开放时领取每日 Credits |
| 套餐明细 | 支持 | 把 `userQuota` 和 `addOnQuota` 拆成“订阅额度”“活动加赠”两条展示 |
| 活动任务板 | 支持 | 读取厂商活动列表；只把 `CLAIMABLE` 的 `CLAIM_BENEFIT` 标为可在面板领取，其余活动保留并说明需去客户端 |
| 任务领取/自动执行 | 支持 | 直接调用厂商 `claim` 接口，已领取的活动不会重复 POST |
| 池状态 | 支持 | 健康状态、可用/冷却/禁用计数和真实在途请求数 |
| 会话粘性 | 支持 | 同一会话轮次固定走同一账号，避免多轮对话在账号池里来回跳 |
| 模型输出上限 | 支持 | 从在线目录的 `max_output_tokens` 读取，冷缓存不猜值 |
| 测试/启停/恢复 | 支持 | 测试发送一次最小真实对话请求，调通才算可用 |
| 模型目录 | 支持 | 读取 `GET /algo/api/v2/model/list?Encode=1`，失败时回退内置目录 |
| 对话 | 支持 | `POST /algo/api/v2/service/pro/sse/agent_chat_generation` |

## 添加账号

推荐路径是浏览器登录（不需要装桌面客户端）：

1. 打开本项目的账号页，选择 `qoder`。
2. 切到“浏览器登录”，点“获取授权链接”，在浏览器里完成登录。
3. 面板会轮询设备授权；浏览器确认后，账号自动写入账号池，并显示用户 ID、手机号和积分。

如果电脑上已经登录了 Qoder CN 桌面客户端，也可以直接导入：

1. 打开并登录 Qoder CN 桌面客户端。
2. 在账号页选择 `qoder`，切到“导入凭据”。
3. 点击“从 Qoder CN 导入”。模块会读取 `%APPDATA%\com.qodercn.app.stable\auth.v1.dat`。

浏览器登录走的是 Qoder CN 自己的设备授权：面板把浏览器带到 `qoder.cn/users/sign-in`，授权完成后用 `GET /api/v1/deviceToken/poll` 取回同一个设备令牌。机器标识保存在数据目录下的 `machine-id`，所以重启后仍是同一台设备；账号 ID 取厂商用户 ID（`qoder-<user_id>`），同一个人重新登录会就地覆盖，不会多出一条。

也可以手动粘贴设备令牌。设备令牌通常以 `dt-` 开头。刷新令牌只用于展示，本模块不会使用它换新令牌，以免让桌面客户端手里的凭据失效。

Windows 上读凭据的流程是：

- `auth.v1.dat` 使用 Electron safeStorage 的 `v10` AES-256-GCM 格式；
- AES 密钥来自同目录 `Local State` 的 `os_crypt.encrypted_key`；
- `encrypted_key` 前面的 `DPAPI` 前缀去掉后，通过当前 Windows 用户的 DPAPI 解包。

非 Windows 平台不能读取桌面凭据，但仍可使用面板里的浏览器登录，或手动粘贴设备令牌。

## 接码自动登录

在账号页选择 `qoder` 后，可以实现 `core.AutoLoginProvider` 和 `core.SMSProvider`：面板会租一个接码平台号码，模块自动打开 Qoder 登录页，点击“使用阿里云登录”，在阿里云 SSO 手机号表单里填入号码、获取验证码、读取短信并提交，最后完成 Qoder 设备授权。

接码平台的发送方关键字必须使用 `Qoder`。阿里云短信实际签名是 `qoder`，不是“阿里云”；关键字写错会表现为“一直没有收到短信”。模块默认已经使用 `Qoder`，一般不需要改。

平台即使选择“实卡”也可能返回 `162 / 165 / 167 / 170 / 171` 虚拟号段，这些号码通常收不到 Qoder 短信。模块会在新取号时自动把虚拟号段直接归还并重新抽取；重新登录已有账号时，指定的账号绑定号码不会被这个规则过滤。

常用配置：

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `sms_token` | 空 | eomsg 接码平台 Token；也可以在面板临时粘贴 |
| `sms_keyword` | `Qoder` | 接码平台发送方关键字 |
| `sms_base` | `https://api.eomsg.com/zc/data.php` | 接码平台地址 |
| `sms_provinces` | 空（内置轮换） | 指定省份池；`["none"]` 表示让平台选择 |
| `sms_card_type` | `全部` | 平台卡类型 |
| `browser_path` | 空 | 指定 Edge / Chrome；空则自动查找 |
| `browser_headless` | `true` | 是否无界面运行；需要观看操作时改为 `false` |
| `auto_login_timeout_seconds` | `300` | 整个自动登录流程超时 |
| `sms_polls` | `12` | 一个号码最多查询多少次短信 |
| `sms_interval_seconds` | `5` | 两次短信查询之间的间隔 |
| `dup_retries` | `6` | 重复号或虚拟号时的最大重抽次数 |

## 签到

签到流程只领取满足以下条件的活动：

1. `actionType` 是 `CLAIM_BENEFIT`；
2. `claimStatus` 是 `CLAIMABLE`。

只读 `GET /sash/api/v1/me/campaigns`，有可领取目标时才调用：

```text
POST /sash/api/v1/me/campaigns/{campaignId}/claim
```

已经领取过的目标不会再次 POST。每日签到通常在 `10:00`（UTC+8）刷新，奖励 Credits 的有效期由厂商决定。

## 配置

配置放在主配置文件的 `clients.qoder` 下。所有字段都可选；一个空对象也是完整可用的默认配置。

```jsonc
{
  "clients": {
    "qoder": {
      "openapi_base": "https://openapi.qoder.com.cn",
      "gateway_base": "https://gateway.qoder.com.cn",
      "auth_base": "https://qoder.cn",
      "auth_client_id": "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa",
      "client_type": 10,
      "cosy_version": "0.4.3",
      "user_agent": "Qoder",
      "request_timeout": "30s",
      "probe_timeout": "20s",
      "chat_timeout": "10m",
      "models_ttl": "30m",
      "models_timeout": "30s",
      "chat_model": "auto",
      "cooldown": "60s",
      "auth_cooldown": "30m",
      "sms_token": "",
      "sms_keyword": "Qoder",
      "sms_base": "https://api.eomsg.com/zc/data.php",
      "sms_provinces": [],
      "sms_card_type": "全部",
      "browser_path": "",
      "browser_headless": true,
      "auto_login_timeout_seconds": 300,
      "sms_polls": 12,
      "sms_interval_seconds": 5,
      "dup_retries": 6,
      "models": ["qmodel_38max", "qfmodel"],
      "accounts": [
        {
          "label": "我的 Qoder CN",
          "token": "",
          "refresh_token": "",
          "user_id": "",
          "phone": "",
          "name": "",
          "expires_at": "",
          "enabled": true
        }
      ]
    }
  }
}
```

字段说明见 [`config.example.json`](./config.example.json)。`expires_at` 接受 unix 秒、unix 毫秒、RFC3339 或 `2006-01-02 15:04:05`；无法解析时按“未知过期时间”处理，交给厂商的 401 判定。

环境变量：

| 变量 | 用途 |
| --- | --- |
| `EOMSG_TOKEN` | 接码平台 Token；低于配置文件，低于面板临时粘贴 |
| `CLIENT2API_QODER_TOKEN` | 设备令牌 |
| `CLIENT2API_QODER_REFRESH_TOKEN` | 刷新令牌，仅展示 |
| `CLIENT2API_QODER_USER_ID` | 厂商用户 ID |
| `CLIENT2API_QODER_PHONE` | 手机号 |
| `CLIENT2API_QODER_NAME` | 显示名 |
| `CLIENT2API_QODER_EXPIRES_AT` | 过期时间 |
| `CLIENT2API_QODER_USER_DATA_DIR` | 覆盖 Qoder CN 的 Electron 用户目录 |
| `CLIENT2API_QODER_OPENAPI_BASE` | 覆盖 OpenAPI 根地址 |
| `CLIENT2API_QODER_GATEWAY_BASE` | 覆盖推理网关根地址 |
| `CLIENT2API_QODER_AUTH_BASE` | 覆盖浏览器登录页根地址 |
| `CLIENT2API_QODER_AUTH_CLIENT_ID` | 覆盖设备授权 OAuth client id |
| `CLIENT2API_QODER_MACHINE_ID` | 覆盖持久化的设备标识（UUID） |
| `CLIENT2API_QODER_COSY_VERSION` | 覆盖客户端版本头 |
| `CLIENT2API_QODER_USER_AGENT` | 覆盖 User-Agent |

## 数据文件

模块只写自己的 `Deps.DataDir`：

| 文件 | 内容 |
| --- | --- |
| `accounts.json` | 账号和凭据、导入来源路径 |
| `state.json` | 冷却、失败次数和最近错误，不含令牌 |

换新令牌时会清空该账号旧的冷却和失效状态。相同令牌重新导入时保留运行时状态，避免重复导入把厂商已经拒绝的账号伪装成可用。

任务板不会伪造任务：`VIEW_DETAILS` 等不能领取的活动只展示说明，标成锁定；只有厂商返回 `CLAIM_BENEFIT` + `CLAIMABLE` 的活动才会真正领取。Qoder CN 没有券码接口，也没有多区服概念，所以不支持券码和区服登录。

## 已知限制

- `models` 配置会覆盖厂商在线目录，作为运营侧的显式白名单使用；留空时以在线目录为准。
- 本地凭据解密只支持 Windows，且必须是当前 Windows 用户加密的 DPAPI 数据。
- 厂商活动规则可能变化；签到接口返回的新状态如果不在已知范围内，会按失败报告，而不是强行当作成功。
