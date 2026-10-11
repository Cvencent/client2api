# AutoClaw

AutoClaw 是一个独立平台，不是智谱、BigModel、ZCode 或 WorkBuddy 的镜像。它有独立的主机、产品身份、签名、账号目录、协议和模型目录。

## 能力

账号管理、本地导入、JSON 文件导入、手机号验证码登录、模型目录、余额与积分包、每日积分领取、会话粘性、健康与池统计、复活。

## 协议

- 基础头：X-Version / X-Tm / X-Product: autoclaw / X-Auth-Appid: 100003 / X-Auth-TimeStamp / X-Auth-Sign / X-Lang / X-Channel。
- 聊天：POST /autoclaw-proxy/proxy/autoclaw/chat/completions，删除 Authorization 与 X-Authorization 后写入 X-Authorization，并补 X-Trace-Id、X-Request-Id、X-Request-Model、X-Client-Type、x_trace_id、X-Session-Id。
- 刷新：POST /userapi/v1/refresh，业务码 400002 时回退 POST /userapi/v1/agent-refresh。
- 登录：POST /userapi/v1/agent-send-code 与 POST /userapi/v1/agent-login。

## 账号导入

桌面客户端数据目录默认 %APPDATA%\AutoClaw-official，可用 CLIENT2API_AUTOCLAW_USER_DATA_DIR 覆盖。account-credentials.enc 为 Electron safeStorage（v10 加 AES-256-GCM），密钥经 Windows DPAPI 解包；非 Windows 不能解密，但粘贴 token 仍可用。

## 配置

见 config.example.json。关键字段：


access_token、refresh_token、device_id、accounts、auto_discover、base_url、client_version。

## 模型

以服务端目录为准，内置兜底：zai_auto、zai_auto-fast、zaicoding_glm-5.3、zai_glm-5.3-flash。

AutoClaw 没有批量任务或接码平台接口，本模块不伪造这些能力。
