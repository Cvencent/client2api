# AutoClaw

AutoClaw 是一个独立平台，不是智谱、BigModel、ZCode 或 WorkBuddy 的镜像。它有独立的主机、产品身份、签名、账号目录、协议和模型目录。

## 能力

账号管理、本地导入、JSON 文件导入、手机号验证码登录、接码与一键自动登录、模型目录、余额与积分包、每日积分领取、会话粘性、健康与池统计、复活。

## 余额

主余额是资产网关返回的 `total_balance`，口径是积分；面板同时把每个
`display=true` 的钱包列为一条余额明细，和桌面客户端「积分详情」里列出的
钱包一致。也就是说，厂商返回了第二套钱包（例如名为 Token 的额度）时，它会
作为单独一条显示，网关不会把它折算成积分，也不会在厂商没有返回时编出一个 0。

积分来自 `GET /agent-assetmgr/api/v2/wallets?biz_app_id=autoclaw`，失败时回退
`GET /agent-assetmgr/api/v1/wallet-instances?wallet_type=all&wallet_scope=all`；
流水是只读的 `/agent-assetmgr/api/v1/ledgers_std`。截至当前安装版客户端，只有
这一套钱包接口（另有会员信息 `/agentpay/v1/assistant/subscribe-info`，只返回
是否会员/编码套餐），客户端里没有单独的 Token 余额接口或界面。活动页宣传的
「送 Token」由服务端发放，是否作为一个钱包返回以接口为准；没有返回时本模块
不会伪造 Token 余额。

## 协议

- 基础头：X-Version / X-Tm / X-Product: autoclaw / X-Auth-Appid: 100003 / X-Auth-TimeStamp / X-Auth-Sign / X-Lang / X-Channel。
- 聊天：POST /autoclaw-proxy/proxy/autoclaw/chat/completions，删除 Authorization 与 X-Authorization 后写入 X-Authorization，并补 X-Trace-Id、X-Request-Id、X-Request-Model、X-Client-Type、x_trace_id、X-Session-Id。
- 刷新：POST /userapi/v1/refresh，业务码 400002 时回退 POST /userapi/v1/agent-refresh。
- 登录：POST /userapi/v1/agent-send-code 与 POST /userapi/v1/agent-login。

## 接码与自动登录

登录是一次纯 HTTP 的手机号 + 短信验证码交换，不需要浏览器，所以模块实现了
`core.SMSProvider` 和 `core.AutoLoginProvider`：

- 面板「添加账号」里的接码区可以从一次性短信平台（eomsg）租号、取码、释放号码；
- 「自动添加」一键跑完整流程：取号（避开账号池里已有的号码和本批次已试过的号码）→
  调用 `/userapi/v1/agent-send-code` → 轮询接码平台读出验证码 →
  `/userapi/v1/agent-login` 换取 token → 拉 user-info 补全资料 → 落库，
  结束后把租来的号码还给平台；
- 接码重登用账号自己的手机号重新占用原号码，成功后就地覆盖旧凭据，不会释放它。

接码平台按项目（`keyWord`）过滤短信，AutoClaw 的项目名是「智谱华章」，这是内置
默认值；供应商标注不同时可在配置或面板里改 `sms_keyword`。`sms_token` 留空表示
不启用接码，面板会如实显示未配置。`EOMSG_TOKEN` 是 workbuddy / loomy 共用的同一个
平台环境变量，AutoClaw 也接受它作为 token 的兜底。

相关配置键：`sms_token`、`sms_keyword`、`sms_base`、`sms_provinces`、
`sms_card_type`、`auto_login_timeout_seconds`、`sms_polls`、`sms_interval_seconds`、
`dup_retries`。

## 账号导入

桌面客户端数据目录默认 %APPDATA%\AutoClaw-official，可用 CLIENT2API_AUTOCLAW_USER_DATA_DIR 覆盖。account-credentials.enc 为 Electron safeStorage（v10 加 AES-256-GCM），密钥经 Windows DPAPI 解包；非 Windows 不能解密，但粘贴 token 仍可用。

## 配置

见 config.example.json。关键字段：


access_token、refresh_token、device_id、accounts、auto_discover、base_url、client_version、sms_token、sms_keyword。

## 模型

以服务端目录为准，内置兜底：zai_auto、zai_auto-fast、zaicoding_glm-5.3、zai_glm-5.3-flash。

AutoClaw 没有批量任务接口，本模块不伪造这些能力。
