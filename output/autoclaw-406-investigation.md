# AutoClaw 406 排查日志

更新时间：2026-10-13（Asia/Shanghai）

## 当前结论

- 目标接口：`POST https://autoglm-acceleration-api.zhipuai.cn/autoclaw-proxy/proxy/autoclaw/chat/completions`
- 桌面端可正常调用；外部探针稳定得到 `406 Not Acceptable`。
- `406` 返回体长度为 0，常见 `server: nginx`，并可能先下发 `Set-Cookie: acw_tc=...`。更像进入业务鉴权前的网关/WAF/环境路由拒绝。
- 旁证：同一 POST 改用普通 `Authorization` 返回 `401 Invalid token`，说明该形态已进入能读取 token 的业务层；桌面的 `X-Authorization` 形态反而固定命中前置 `406`。
- 部署账号 token 与桌面当前 token 不一致，但 `406` 发生在 token 业务校验前，不能只靠换 token 解释，仍需核实。

## 已确认的桌面端事实

- AutoClaw 桌面版本：`2.1.0`。
- 主程序：`D:\autoclaw\AutoClaw2\AutoClaw2.exe`。
- 主逻辑：`D:\autoclaw\AutoClaw2\resources\app\dist\main.cjs`。
- 托管模型路径：`/autoclaw-proxy/proxy/autoclaw`。
- 托管模型协议：`openai-completions`。
- 最终 chat headers：`X-Authorization`、`X-Trace-Id`、`X-Request-Id`、`X-Request-Model: zai_glm-5.3-flash`、`X-Client-Type: pc`、`x_trace_id: autoclaw-desktop`、`X-Session-Id`；run 场景另有 `X-Query-Id`。
- 公共产品头：`X-Version`、`X-Tm`、`X-Product`、`X-Auth-Appid`、`X-Auth-TimeStamp`、`X-Auth-Sign`、`X-Lang`、`X-Channel`。
- 桌面最终 wire headers 使用 `accept: application/json`，不是 SSE。
- body：`stream: true`、`stream_options.include_usage: true`、`max_tokens`；无思考等级时补 `thinking: {type: "disabled"}`。
- 桌面会删除普通 `Authorization`，最终只保留 `X-Authorization`。
- `X-Agent-Id` 只在特定 run/场景头冻结逻辑中出现，不是所有 chat 的基础头。
- Zhipu `X-Client-*` 签名器只匹配 `bigmodel.cn` / `z.ai`，不匹配 `zhipuai.cn`。
- 成功 run：`1e0b60af-3135-45d4-a79c-76edd3f0cdcd`。
- 成功 session：`0276b84d-e4f3-479a-b863-c5cc06855516`。
- 成功时间：`2026-10-13T01:00:08.011Z` 至 `2026-10-13T01:00:11.777Z`。
- usage：input 5552、output 75、cacheRead 11520、total 17147。

## 已尝试且仍为 406

以下实验已实际跑过，除非出现新证据，不再原样重复：

| 方案 | 结果 | 结论 |
| --- | --- | --- |
| `X-Authorization` | `406` | 固定命中前置拒绝 |
| 普通 `Authorization` | `401 Invalid token` | 已进入业务鉴权层 |
| 两种 Authorization 同时带 | `406` | 出现 `X-Authorization` 即命中前置路径 |
| 完全不带 auth | `406` | 前置层先拒绝 |
| `accept: application/json` | `406` | Accept 不是唯一根因 |
| `accept: text/event-stream` | `406` | Accept 不是唯一根因 |
| `X-Agent-Id: main` | `406` | 单独加此头不够 |
| `X-Enterprise: 0` / `1` | `406` | 单独加此头不够 |
| 带 `acw_tc` cookie 后重试 | `406` | 只有第一段 cookie 不够 |
| body 增删 `stream_options` | `406` | body 不是首要分界线 |
| body 增删 `thinking` | `406` | body 不是首要分界线 |
| `thinking` 字符串 / `zai_auto` / `developer` role | `406` | body 变体未突破前置层 |
| HTTP/1.1 | `406` | HTTP 版本不是唯一分界线 |
| HTTP/2 | `406` | HTTP 版本不是唯一分界线 |
| Electron `globalThis.fetch` | `406` | 未复现桌面成功 |
| Electron `net.fetch` | `406` | 未复现桌面成功 |
| Electron `session.fetch` | `406` | 未复现桌面成功 |
| Electron session + Origin/User-Agent 变体 | `406` | 单头变体未复现 |
| 主域名 `autoglm-acceleration-api.zhipuai.cn` | `406` | 当前主域名命中前置拒绝 |
| `autoclaw.zhipuai.cn` | `404` | 非该服务路径 |
| `autoglm-api.zhipuai.cn` | `406` | 非解决方案 |
| `autoglm-pre-api.zhipuai.cn` | `405` | 非解决方案 |
| `autoglm-test-2.zhipuai.cn` | 无法连接 | 非解决方案 |
| 固定 IP `39.107.195.74` | `406` | 不是单一 IP/DNS 选错 |
| 当前 DNS IP `47.109.149.227` / `47.109.25.226` | `406` | 不是单一 IP/DNS 选错 |
| `X-Version: 2.1.0` | `406` | 仅升级版本头不够 |
| 不存在路径 POST `/autoclaw-proxy/proxy/autoclaw/nonexistent-path` | `406` | 前缀级请求先被拦截 |
| 同一不存在路径 GET | `404` | 拒绝与方法 + 前缀组合有关 |
| POST `/autoclaw-proxy/proxy/autoclaw-model-config` | `404` | 不是该接口 |
| POST `/agentdr/v1/assistant/conf-get` | `200` | 同域名其他路由可访问 |
| POST `/userapi/v1/user/web-event/report` | `200` | 同域名其他路由可访问 |

## 已抓包/验证手段

- 抓包：`C:\Users\vencent\AppData\Local\Temp\autoclaw-full.mitm`。
- 桌面主进程 Node fetch 绕过 Chromium `--proxy-server`，抓包中没有桌面真正成功的 chat 请求。
- 抓包里的 chat `401/406` 均来自外部探针。
- 已检查 runtime SQLite、trace、transcript、token usage 日志和 `main.cjs` 静态逻辑。
- 桌面曾连接后端 IP `39.107.195.74`，但固定该 IP 仍为 `406`。

## 新进展

- 2026-10-13：用真实 Edge/Chromium，先访问目标域名根路径，再从同源页面发起
  desktop 形态 POST。结果仍为 `406`，且浏览器上下文只得到 `acw_tc` Cookie，
  没有 `acw_sc__v2`。
- 结论：此前“缺完整风控 Cookie”的假设不成立；真实浏览器网络栈也不能复现桌面成功。
  下一步转向检查桌面 Node/undici 是否使用代理、自定义 dispatcher 或其他 egress 配置。
- `--proxy-server=127.0.0.1:8898` 对照仍为 `406`，代理不是根因。
- 带桌面真实 `Origin: https://autoglm-acceleration-api.zhipuai.cn` 仍为 `406`。
- 对本机入口的结构性验证：`X-Authorization: Bearer invalid` 实际可穿透，返回
  `401 Unauthorized` + JSON 错误体；只有 `acw_tc` 而没有 `X-Authorization`
  时，也返回 `401 Unauthorized`。因此 WAF 并非一概拒绝 POST，而是对带
  `X-Authorization` 且“请求形态不匹配”的请求返回 `406`。
- OPTIONS 预检返回 `204`。

## 尚未完成 / 下一步按顺序验证

1. 用真实 Edge/Chromium 页面上下文，从目标域名同源发 POST，确认真实浏览器网络栈是否仍为 `406`。
2. 只读取得桌面 profile 的完整 cookie，重点检查 `acw_sc__v2`、`acw_tc`；直接复制 SQLite 已被运行中的 Chromium 文件锁拦截。
3. 对照桌面当前账号 token 与部署账号 token，确认是否需要重新导入当前桌面账号。
4. 若浏览器栈仍为 `406`，核对托管 endpoint 的请求路径/环境路由，不再继续堆 headers/body。
5. 找到确定根因后再做最小源码修复，并补 `clients/autoclaw/platform_test.go`。

## 不再重复

- 不只换 Accept。
- 不只加单个 `X-Agent-Id` / `X-Enterprise`。
- 不只改 body 字段。
- 不只换 HTTP/1.1 或 HTTP/2。
- 不只固定已知 IP。
- 不只把 `X-Version` 改成 `2.1.0`。
- 不重复 Electron 几种 fetch 的相同无 cookie 对照。

## 2026-10-13 继续排查（本轮新增，不重复）

- 桌面运行日志确认成功 chat：`zwork.model.request.issued` -> `zwork.network.request.succeeded 200`，成功物理 ID 为 `4a2cca67-4963-438d-ac49-3499b7f7c27e`，session `0276b84d-e4f3-479a-b863-c5cc06855516`，run `1e0b60af-3135-45d4-a79c-76edd3f0cdcd`。
- 桌面源码确认聊天不是普通 `session.fetch`：
  `createZworkManagedModelRequestRuntime` 的 `createZworkManagedModelRequestResolvers` 未注入 `fetch`，
  最终 `const fetchImpl = input.fetch ?? globalThis.fetch`，即 chat 走 Electron 主进程的 global fetch（Node/undici）。
- 普通目录、通知等请求走 `createElectronHttpTransport(session.defaultSession)`，与 chat 的 transport 不同。
  这解释了为什么 model-config 能 200，而 chat 稳定 406。
- 已重新精确复现 AI SDK wire 头，仍固定 406：
  删除/保留普通 `authorization`、补/不补 `user-agent: ai-sdk/openai-compatible/2.0.81`、
  补/不补 `X-Query-Id` 与 `X-Agent-Id`，均无变化。
- `createOpenAICompatible` 在 `2.0.81` 下通过 `wireHeaders()` 生成：
  `accept`、可选 `authorization`、`model.headers`、`content-type`，随后授权 lease 删除 `authorization` 并写 `X-Authorization`。
- `resolveManagedEndpoint` 用 bundled runtime config；本机 release 环境确认为
  `https://autoglm-acceleration-api.zhipuai.cn`，没有额外 endpoint 或协议。
- `47.109.149.227` / `47.109.25.226` 是正式 `client2api` 进程的连接，不是桌面 chat；
  `180.130.117.48:443` 当前直连探测 TLS alert 40（很可能不是该站点）。因此固定 IP 方向不继续试。
- 运行日志/token usage 不保存完整 wire headers，只保存 request id 和状态。

## 约束

- 不停止、不重启、不杀死正式服务 `D:\client2api\client2api.exe`。
- 不注入、不 patch、不重启正在运行的 AutoClaw。
- 不杀 mitmdump，不清理用户已有抓包。
- 不执行安装包覆盖正式部署。
- 只改 `D:\client2api-src` 源码。

## 2026-10-13 12:28-12:37 最新成功请求对齐（本轮追加）

- 桌面端又成功完成一次 chat：
  - run `131afe3b-e219-4080-b5f0-ad82451c6228`
  - session `8c1edd46-c66a-4a89-9dee-55e6c37fd84c`
  - logical `9477b730-f96d-49c5-8d7c-05ecb2b65c72`
  - physical `93ac9abd-8674-45d7-84db-94cdd22a31ae`
  - issued `2026-10-13T04:30:03.854Z`，success `04:30:07.817Z`
- 连接监控确认 chat 由主进程 PID `64940` 发起：
  - `12:30:04.703` 和 `12:30:04.708` 两次新建到 `47.109.25.226:443`。
  - 成功后 `12:30:14` 的一批 `47.109.25.226/116.162.121.76` 是通知点击后的普通 HTTP，
    不是 chat。
- 复用该成功 run 的真实 `session/run` ID 重放，旧成功 ID、`redirect: manual`、
  `X-Agent-Id` 四个变体仍全部 `406`。因此“旧 ID 失效”和 `redirect` 不是分界线。
- 桌面 bundled runtime：`D:\autoclaw\AutoClaw2\resources\vendor\node\node.exe`，
  Node `24.20.0`、undici `7.29.0`、OpenSSL `3.5.7`。
- 裸 `tls.connect` 对照：显式 `ALPNProtocols: ["http/1.1"]` 时远端
  `47.109.25.226` 协商 TLS 1.3 / `TLS_AES_128_GCM_SHA256` / `X25519`；
  不指定 ALPN 时 TLS 版本、套件和曲线相同，但 `alpn=false`。
- 当前源码默认 `http.Transport` 使用 `ForceAttemptHTTP2: true`；部署配置没有给
  autoclaw 配 `tls_profile`/`tls_protocol`，所以正式服务当前走 Go 原生 TLS。
- TLS 指纹实验（不改正式服务和正在运行的 AutoClaw）已实际执行：
  `golang/h2`、`chrome/http1.1`、`chrome/h2`、`chrome-133/http1.1`、
  `chrome-133/h2`、`edge/http1.1`、`node/http1.1` 全部仍为 `406`。
  因此“只切 TLS 指纹”已排除，后续不要再重复。
- 桌面凭证与部署账号凭证再次解密核对：长度都为 `341`，SHA256 都是
  `737d0dc72ca18aafc9f7b7b39185c97288a2eca7494053a28dbbe762f4deb9d4`。
- 进程内先 GET `autoclaw-model-config`（200，拿 `acw_tc`）再连续 POST chat，
  仍全部 `406`，因此“同进程连接池热连接/先取 cookie”也已排除。

## 已尝试但不要再重复（本轮追加）

- 不要再用旧/新真实 `sessionId`、`runId` 重放 chat。
- 不要只为 `redirect: "manual"` 单独试一轮。
- 不要再把 `12:30:14` 的普通 HTTP 连接当成 chat 连接。
- 不要只重复 `X-Agent-Id`、`X-Query-Id`、`Accept`、`Authorization` 单头组合。
- 不要再重复 TLS fingerprint/profile 组合。
## 2026-10-13 13:00 左右继续排查（本轮新增）

- 用户说“我发了”后，桌面日志仍没有出现新的 `zwork.model.request.issued`；
  最后一次模型请求仍是 `131afe3b-e219-4080-b5f0-ad82451c6228`。
  这只说明该次操作没有真正发出模型请求，不能据此判定桌面端当前失败。
- 桌面 `main.cjs` 的实际发送点进一步确认：
  `overridingFetch(input, downstream)` 最终执行
  `downstream(url4, { ...init, headers: wireHeaders(input), body: ... })`。
  目前没有证据表明它传的是 `Request` 对象；AI SDK 先构造 `Request` 的说法
  需要重新验证，不能作为根因假设继续堆实验。
- `wireHeaders(input)` 的 managed `openai-completions` 分支实际会生成：
  `accept: application/json`、可选 `authorization`、`model.headers`、
  可选的 session affinity 头、`input.headers` 覆盖项，最后 `content-type`。
- `completionsCompat` 的默认 `sendSessionAffinityHeaders` 是 `false`；
  managed 的 `zai_glm-5.3-flash` 目前没有证据带 `x-client-request-id`、
  `x-session-affinity` 或 `session_id`。所以这些不是本轮候选根因。
- `createZworkManagedModelRequestResolvers` 的授权 lease 返回：
  `headers` 含 `Authorization: null` 和 `X-Authorization: <token>`；
  `wireHeaders` 遇到 `null` 会删除对应头，随后 `model.headers` 注入
  `X-Authorization`。这与“最终只带 X-Authorization”一致。
- 已确认 `globalThis.fetch` 在普通 Node 和 Electron
  `ELECTRON_RUN_AS_NODE=1` 两种运行方式下，都能稳定产生 `406`；
  但桌面 Electron 主进程的真实 `globalThis.fetch` 仍需单独确认是否是
  Electron `net.fetch` 或带 Electron session 的实现。
- `pktmon` 在当前权限下无法启动：`无法与 PktMon 驱动程序通信。拒绝访问。`
  因此本轮不能直接抓取桌面成功请求的 TLS/HTTP 明文。
- 最新进程连接：
  `64940`（AutoClaw 主进程）有一条到 `59.80.45.41:443` 的已建立连接；
  `50340` 有一条到 `47.109.149.227:443` 的连接。
  这两条现在只是连接层证据，尚未证明是最近一次 chat。

### 当前真正需要验证的下一项

1. 只读确认 Electron 主进程的 `globalThis.fetch` 是 Electron `net.fetch`
   还是未修改的原生 Node `fetch`。
2. 若仍是原生 fetch，下一步不再试 header/body，转去比对桌面进程的
   DNS/代理/egress 环境与独立探针是否不同。
3. 在拿到新的成功 chat wire 证据前，不改 `clients/autoclaw` 的业务源码。

## 2026-10-13 16:18 继续排查（本轮新增）

- 已按桌面端真实过滤规则重算成功 run `131afe3b-e219-4080-b5f0-ad82451c6228` 的 wire tools：
  `snapshotToolCount=297`，过滤 loop-executed、registration-only 与 deferred 后，goal 是 43 个。
  但按运行库快照实际重算时，always-on 9 个 + bridge 4 个 = 13；其余 284 个 governed 工具中，
  日志仅列出前 20 个 deferred 名称，完整 deferredNames 未落日志，不能只凭这 20 个名称推出全部 254 个。
- 已验证一次直连高保真请求（不改源码、不改运行中的服务）：
  - `X-Authorization` + 真实 session/run + `X-Session-Id` / `X-Query-Id`
  - `X-Request-Model: zai_glm-5.3-flash`、`X-Client-Type: pc`、`x_trace_id: autoclaw-desktop`
  - body: `stream:true`、`stream_options.include_usage:true`、`thinking:{type:"disabled"}`、`max_tokens:131072`
  - 13 个真实名称/描述/schema 的 tools（always-on 9 + bridge 4）
  - 结果仍为 `406 Not Acceptable`，空体，带 `acw_tc` Cookie。
- 同一形态去掉 `tools` 后回到业务鉴权层的 `401 Invalid token`（此前矩阵已记录）。
  因此当前证据指向：`tools` 字段（或其触发的请求形状/大小分类）是 406 的关键分界，而不是
  `X-Authorization`、session/run、Accept、TLS 或代理。
- 不再重复：只换 `X-Query-Id` / `X-Session-Id`、只补 always-on/bridge 工具、只换 TLS/代理。
  下一步要拿到 254 个 deferred 的完整名单或真实桌面 wire 请求体；否则只能继续盲猜。

## 2026-10-13 17:55 根因确认（已解决）

**根因：AutoClaw 托管代理要求 chat 请求的第一条消息必须是含固定文本的 system 提示。**

服务端在鉴权之前就检查消息形状：

- 第一条消息 `role` 不是 `system`，或第一条 system 内容里
  **不含这段精确子串**，一律返回空体 `406 Not Acceptable`：

  ```
  You are AutoClaw. Answer the user directly and concisely
  ```

- 判定要点（全部实测）：
  - 子串匹配，不要求整条等于；前后都能带额外内容。
  - **大小写敏感**：`YOU ARE AUTOCLAW` / `autoclaw` 小写 → 406。
  - 拼写错误 → 406。
  - marker 必须在**第一条消息**；放到第二条 system 后面 → 406。
  - 只有 system、没有 user → 200。
  - 纯 `xxxx...` 或任意自然语言 system → 406，所以不是长度阈值。
  - 与 header（Accept/UA/X-Version/X-Agent-Id/X-Query-Id/X-Session-Id）、
    `max_tokens` vs `max_completion_tokens`、`store`、`thinking`、`tools`
    都无关：逐个改回桌面形态的变体全部 200；唯独裁掉 system marker 立刻 406。

**验证链**

1. `wire-51688.jsonl` 的完整 body + 完整 headers 原样重放（普通 Node 22）→ 200，
   真实流式回复。此前所有探针失败，是因为都在用简化/部分的 header 或 body。
2. 受控差分：逐项改动基线，只有 message 维度会翻成 406。
3. 最小复现：`messages=[{user:"测试"}]` → 406；
   `messages=[{system: 含 marker}, {user:"测试"}]` → 200。
4. client2api 现有 header/body 形态 + 注入 marker → 200。
   同一形态不注入 → 406（复现现网故障）。
5. 修复后 Go 真实路径端到端（`buildChatBody` + `chatHeaders`）→ 200。

**修复**

- `clients/autoclaw/body.go`：`buildChatBody` 末尾调用
  `ensureDesktopSystemPrompt`。若第一条消息不是含 marker 的 system，
  就在最前面补一条 `You are AutoClaw. Answer the user directly and concisely.`
  调用方已自带 marker 时不重复注入。
- 测试：`clients/autoclaw/models_test.go`
  `TestDesktopSystemPromptInjected` / `TestDesktopSystemPromptNotDuplicated`，
  并更新 `TestDeveloperRoleMapsToSystem` 的消息计数。

**未改动的项**：版本号仍 `2.0.5`（实测 200 不受影响），Accept 仍
`text/event-stream`，`max_tokens` 等保持原样。本次只修阻塞根因，改动最小。

**此前的 406 猜测全部作废**：TLS 指纹、代理、DNS/IP、Cookie、浏览器栈、
`X-Authorization` 拼写、`tools` 字段、body 大小等都不是根因。
## 2026-10-13 20:35 第二个根因：max_tokens 过小（面板对话测试 406 的真凶）

升级到 0.1.58 后 system marker 已生效（连续 52 次 200），但用户在**面板「对话测试」**
里仍然 406→502。复现并定位：

- 面板对话测试的默认参数是 `max_tokens=512`、`temperature=0.7`。
- 只带 `temperature` 或只带大 `max_tokens` → 200。
- 只带 `max_tokens<=9000` → 上游 406（网关转 502）。

**上游要求 `max_tokens` 必须 >= 10000（实测 9000 失败、10000 成功）。**
这是与 system marker 并列的第二个硬性前置条件。

这与桌面端一致：桌面每次都发 `max_completion_tokens=131072`（该模型上限），
从不下发小值，所以桌面永远碰不到这个坑。

**当前网关的缺陷**：`buildChatBody` 直接采用调用方的 `req.MaxTokens`，调用方
（面板默认 512，或任何 OpenAI 客户端给个小值）会把小值原样转发，命中 406。

**待修**：调用方给的 `max_tokens` 低于阈值时抬到安全下限（取该模型上限更稳妥），
阈值 `10000`，安全值用模型公布上限或至少 10000。需要同时覆盖 `max_tokens`
与 `max_completion_tokens` 语义。
## 2026-10-13 20:40 结论修正：第二个根因是 body 字段名 max_tokens

上一节把 `max_tokens` 写成"数值太小"，不对。对原始上游做字段矩阵后确认：

| body 字段 | 结果 |
| --- | --- |
| `max_completion_tokens`（任意值 1..131072） | 200 |
| 不带任何 max 字段 | 200 |
| `max_tokens` < 10000 | **406** |
| `max_tokens` >= 10000 | 200 |
| `max_tokens` 与 `max_completion_tokens` 同时存在 | **406**（只要带 max_tokens ） |

所以规则是：**上游只接受 `max_completion_tokens`；只要 body 里出现 `max_tokens`，
且它小于 10000，就 406。** 桌面端从不发 `max_tokens`，所以永远没事。

**面板「对话测试」的默认 `max_tokens=512` 正好踩中这个 406**，这就是升级到 0.1.58
后用户仍看到 406/502 的原因。上一轮只补了 system marker，没换字段名。

**修复**（`clients/autoclaw/body.go`）：
`chatBody` 的 `MaxTokens *int json:"max_tokens"` 改为
`MaxCompletionTokens *int json:"max_completion_tokens"`，构造处同步改名。
调用方的额度仍原样透传，只是换成上游认的字段名。

**验证**
- 上游字段矩阵：见上表。
- Go 真实路径（`buildChatBody`+`chatHeaders`，`max_tokens=512` 输入）
  产出 `"max_completion_tokens":512`，上游 200。
- `clients/autoclaw` 单包测试与全量 `go build ./...` / `go test ./...` 全绿。
- 新增 `TestChatBodyUsesCompletionTokenField` 防止字段名回归。