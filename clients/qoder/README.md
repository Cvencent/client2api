# `qoder` client

`qoder` 接入的是 **Qoder CN**（`qoder.com.cn` / `qoder.cn`）。它负责浏览器登录、账号池、积分余额、每日签到和本地凭据导入。

账号有两条来源：面板里的浏览器登录（推荐，不需要装客户端），以及从桌面客户端导入的本地凭据。

它**不提供对话接口**。Qoder CN 的推理网关 `gateway.qoder.com.cn` 要求桌面客户端原生安全 SDK 生成逐请求签名；缺少这个签名时，网关会拒绝请求。为了避免把“签名不匹配”伪装成普通的 502 上游故障，本模块的 `Chat` 会明确返回不支持。

## 能力

| 能力 | 是否支持 | 说明 |
| --- | --- | --- |
| 账号池 | 支持 | 浏览器登录、导入桌面客户端凭据，或手动粘贴设备令牌 |
| 浏览器登录 | 支持 | 面板生成厂商登录链接，浏览器确认后账号自动落地（设备授权 + PKCE） |
| 余额查询 | 支持 | 读取 `GET /api/v2/quota/usage` |
| 每日签到 | 支持 | 活动开放时领取每日 Credits |
| 测试/启停/恢复 | 支持 | 测试只读 `GET /api/v1/userinfo` |
| 模型目录 | 内置或配置 | 厂商目录接口受原生签名保护，不能在线刷新 |
| 对话 | 不支持 | 同上，返回 `core.ErrUnsupported` |

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
      "auth_base": "https://qoder.cn",
      "auth_client_id": "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa",
      "client_type": 10,
      "cosy_version": "0.4.3",
      "user_agent": "Qoder",
      "request_timeout": "30s",
      "probe_timeout": "20s",
      "cooldown": "60s",
      "auth_cooldown": "30m",
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
| `CLIENT2API_QODER_TOKEN` | 设备令牌 |
| `CLIENT2API_QODER_REFRESH_TOKEN` | 刷新令牌，仅展示 |
| `CLIENT2API_QODER_USER_ID` | 厂商用户 ID |
| `CLIENT2API_QODER_PHONE` | 手机号 |
| `CLIENT2API_QODER_NAME` | 显示名 |
| `CLIENT2API_QODER_EXPIRES_AT` | 过期时间 |
| `CLIENT2API_QODER_USER_DATA_DIR` | 覆盖 Qoder CN 的 Electron 用户目录 |
| `CLIENT2API_QODER_OPENAPI_BASE` | 覆盖 OpenAPI 根地址 |
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

## 已知限制

- 模型目录只能使用内置列表或 `models` 配置，不能在线刷新。
- 对话不能通过本模块调用。
- 本地凭据解密只支持 Windows，且必须是当前 Windows 用户加密的 DPAPI 数据。
- 厂商活动规则可能变化；签到接口返回的新状态如果不在已知范围内，会按失败报告，而不是强行当作成功。
