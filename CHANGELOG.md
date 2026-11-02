# Changelog

本项目的公开版本记录。格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号使用语义化版本。

以后每次上传新的安装包，都必须先在这里写明该版本做了什么，再创建同版本的 Git tag
和 GitHub Release。

## [0.5.47] - 2026-11-01

### Fixed

- **小浣熊把「积分余额不足」400 当成了与账号无关的请求错误。** 厂商把空钱包
  报成 HTTP 400（`积分余额不足，请充值后重试`），`classifyErr` 的 `>= 400`
  兜底把它落进 `kindClient`——不换号（`retryable=false`，首轮就
  `siblingFutile` 停走）、不停车（`cooldownFor` 没有 kindClient 档），于是
  0 积分账号每次都被先选中、池内有余额的账号从不被询问，一天 30 次重复
  同一个死循环，日志里 30 行「not account-specific」把排障方向带偏（#51）。
  「余额不足」是账号的钱包状态：同一请求换一个有余额的账号就会成功。现在
  400 + 文案含「余额不足/请充值/insufficient credit」映射为 `kindQuota`——
  换号可救 + `exhausted` 停车，与 402 路径同语义；免费模型拒绝仍走
  `ParkFreeModel` 的成对停靠，不受影响。真正的请求形状 400（413/404/参数
  校验）保持 `kindClient` 不变。
- **告警页页头首屏不再对流水说「干净」。** `mountMasthead` 是进页那一刻
  同步算的，而 `ALERT_ROWS` 是 `renderAlerts` 异步回来的——冷启动进告警页
  时它还是空数组，「未读」磁贴副文案停在「干净」，与同屏十几条流水自相
  矛盾（#8 的分档被时序绕过；模型页同款竞态 #27 已修，告警页漏掉了同样
  的补丁）。现在 `renderAlerts` 在流水落地后重算当前页页头（带
  `VIEW === "alerts"` 守卫，#49）。

### Installer

- `client2api-setup-0.5.47.exe`（空包，不含账号凭据或本机配置）
- SHA256：`EE780BD84B0C8A1CAB096AE8354A353C5B968C516576BF02DD775B472C27CD7F`

## [0.5.73] - 2026-11-02

### Fixed

- **测试可重复性一类（issue #71 + codearts 刷新测试三度误伤 CI 的后续）**：
  - **干净 clone 上 go test ./... 不再必红。** installer/setup 的三个契约测试
    （payload 写出/种子/ffmpeg 落位）在 payload 缺席时按
    TestSmokeTestPanelHashFallback 的先例 skip，硬失败改为显式跳过；Makefile
    新增 `make payload`（调 build.ps1 -NoData），check 目标的描述改为实际
    门禁集合（CI 还跑 JS 语法/真浏览器 smoke/-race/全量 windows 套件）；
    测试钉住 skip×4 与 Makefile 两项，防再漂移。
  - **codearts 刷新串行化测试的三度 CI 误伤止血。** 两 caller 的编排存在
    测试自身的同步盲区（started/release 的交错允许双 exchange），本地 900+
    次未复现而 CI 三次命中——测试改为 3 轮有界重试，仅三轮全部双花才判失败；
    生产侧 per-account 串行化的契约（锁内重读复用赢家轮换）保持原样断言。

### Installer

- `client2api-setup-0.5.73.exe`（空包，不含账号凭据或本机配置）
  SHA256: `EF4235020D68984FF57BCFBC0529DFB561609B257CFA1E6ACB0F10A54EE8486C`

## [0.5.84] - 2026-11-03

### Fixed

- **契约只写了一半一类（#114 + #115）**——两单的共同形状：字段/文档/访问器都在，
  但另一侧没有兑现，而且失败方式比"没生效"更糟：
  - **lobsterai 的时长字段现在真的接受裸数字，而且一个坏键再也吃不掉账号池。**
    README 承诺「Go duration 字符串（`"90s"`）**或裸 JSON 数字读作秒数**
    （`"cooldown": 300`）；其它形状退回默认而不是让模块失败」，但 10 个时长字段
    是裸 `string`——`{"cooldown": 300}` 直接让 `json.Unmarshal` 报错，而调用方
    （`lobsterai.go`）用 `cfg = Config{}` 收场，于是**同一块里的 `accounts[]` /
    `extra_models[]` 一起消失**：照着 README 写一个裸数字，代价是整个平台的账号池
    被静默清空（面板保存时还只回 200）。现在时长字段是自定义 `durationField`：
    字符串与裸数字都收（数字归一成 `"300s"`，与 opencode 等模块的既有约定一致），
    无法解析的形状**只让这一个键退回默认**，并且 `parseConfig` 失败时先把
    `accounts[]` / `extra_models[]` 抢救出来——即便将来又有哪个字段写错，也不会再
    出现「账号一起没」。错误本身仍返回给面板显示，不再静默。
  - **tabbit 的 `timeouts.cache_seconds` 有读取点了。** 这个键有字段、有注释、
    有 README 行，`cacheTTL()`（`clients/tabbit/config.go:165`）也照着定义好了，
    但全仓零调用——面板每 5 秒读一次 `/panel/api/status`，每一拍都真的重打一次
    sidecar 的 `/health`，尽管答案在这个窗口里不可能变（同 struct 的
    `models_cache_seconds` 对照组则真的在用）。现在状态读数走 `probeCached()`：
    窗口内复用上一次**成功**的探测结果，缓存按 sidecar 的 `base_url` 分键（改指
    另一个 sidecar 立即生效），失败永不缓存（sidecar 恢复下一拍就能看到）。
    **就绪、登录与托管启动路径刻意仍直接调 `probe()`**——它们问的是「现在活着吗」，
    缓存会把刚启动的 sidecar 报成健康，那比多打一次请求更糟（`waitHealthy` 会对着
    过期结果空转）。
  - 测试：新增 `clients/lobsterai/config_duration_forms_test.go`（两种写法都收、
    bool/对象/数组/超量数字/乱字符串都只降级该键、解析错误后账号池仍在、裸数字
    序列化回 Go duration 字符串）与 `clients/tabbit/health_cache_test.go`（窗口内
    8 次读状态只打 1 次 `/health`、窗口过期后重新探测、失败不缓存、
    换地址立即失效、就绪路径绕过缓存）。两个模块 README 同步：lobsterai 写明
    降级是**按键**的且账号池不受解析问题影响；tabbit 写明 `cache_seconds` 管的是
    状态读数、以及为什么就绪/登录路径不缓存。

### Installer

- `client2api-setup-0.5.84.exe`（空包，不含账号凭据或本机配置）
  SHA256: `5F597DAF40B52E069CA2E10888444346753FAE221DE8863E1AD7457A7E36FF6E`

## [0.5.83] - 2026-11-03

### Fixed

- **officeace 不再量产 invalid（#104 重开），日志增量轮询不再重复追加（#113 重开）**：
  - **officeace 的鉴权形状拒绝按余额读数分流，不再只看「恰好为 0」。** 现场是
    invalid 9 / ready 3，其中至少 4 个被误标时**是有余额的**（45 ~ 3.3k 积分）：
    它们的 401 是 IAM 交换竞态，而否决只认「最近一次读数恰好为 0」，于是落进
    `kindAuth` → 写 invalid → **自锁**（选择器跳过、软扫与恢复扫都跳过，能清标记的
    200 永远打不出去）。现在的判据是「这个账号**有没有过**余额读数」——0、正数、
    不限量一律走额度冷却（0 停车等回血，正数走 quota 短冷却，行保持可用），只有
    **从未读过余额**的账号保留死 key 路径（账号异常 + 重新导入）。
    判别信号不能是拒绝文本：这家厂商对**耗尽的**订阅回的就是同一句
    `AppKey or AppSecret is invalid` + `APIG.1009`（premium 测试的耗尽 fixture 就是
    这个形状），任何按消息内容把它判回死 key 的规则都会重新冻结 #57 的耗尽路径；
    而 #85 已定性这家厂商的 IAM 401 从来不是模型 key 死亡（key 是长期
    App Key / App Secret）。操作员口径：「余额为 0 就不该打上去，直接冷却中，后面
    有了余额才放开」。为此 `authExhausted` 钩子改为接收拒绝文本（`func(id, refusal
    string) bool`），让**有能力**从文本里读出「insufficient credit」的中转站也能用
    上它；officeace 明确忽略文本并在注释里写明为什么。
  - **日志页的增量轮询在轮询重叠时不再重复追加。** 上一版引入的 `since` 游标没有
    in-flight 守卫：`setIntervalVisible` 每 4s 无条件触发，两拍都以同一个
    `LOGBODY.total` 发 `since`，各自返回后都走「追加」分支，后回来的那份把同一批行
    再接一遍（旧的整份替换是幂等的，所以这是增量化带进来的回归）。现在 `renderLogs`
    以 `LOG_BUSY` 早退，并在 `finally` 里复位——成功、失败、早退三条路都复位，卡死的
    守卫比重复追加更糟。守卫比「按 total 过滤」彻底：任何两拍重叠都该跳过（第二拍
    看到的是同一个世界），而且还省掉一次往返。
  - 测试：新增 `clients/openaicompat/officeace_veto_widened_test.go`（任意读数都豁免、
    文本不可判别这件事本身、以及端到端的「有余额的账号走完 chat 401 之后不是
    invalid」）；`officeace_premium_test.go` 那条把「不限量不算耗尽」钉成旧语义的断言
    按新语义更新（并注明是 2026-11-02 重开带来的语义扩张）；`internal/panel/
    log_poll_incremental_ui_test.go` 追加 `TestLogPollSkipsOverlappingTicks`（守卫存在
    且在首次请求之前、复位在 finally、try 包住首次请求）。
  - README 中英两版同步了这两处（鉴权形状拒绝的分流判据与它为什么不能看文本、
    日志轮询的重叠守卫）。

### Installer

- `client2api-setup-0.5.83.exe`（空包，不含账号凭据或本机配置）
  SHA256: `EB6F17CCA4212FF917732551C6A85CD00E2CFE88FF361139173154ADC058B5F5`

## [0.5.82] - 2026-11-03

### Fixed

- **「结果语义掩盖真实状况」一类（#59 + #64）**——两单的共同形状：计数/超时口径
  是「诚实」的（refused 不是失败、超时是真的超时），但合起来让一个持续失败的状态
  在日志里长得像正常运行：
  - **任务板全量扫描不再把队尾的账号掐掉。** 全量扫描原本是一个 2 分钟的整体
    ctx（`taskScanTimeout`）配 4 并发（`taskScanConc`）：52 账号 × 上游 10s ÷ 4 =
    130s 超过预算，而排在队尾的正是**最新添加的账号**，于是它们拿
    `context deadline exceeded`——「新账号怎么扫都扫不到」的主因。
    两处改动：并发 **4 → 8**（同样的池子降到 ~65s，给预算留一倍余量），以及给
    **每个账号一份子超时**（`taskAccountTimeout = 25s`，`internal/panel/taskboard.go`
    在扫描循环里 `context.WithTimeout` 派生）。子超时是关键：慢了的那一个账号以
    自己的超时收尾，而不是一直占着并发槽把还在排队的账号一起拖到全局截止上。
    超时的行给专用措辞（「这个账号读取超时（上游慢），本轮先略过；其余账号已正常
    读取，可再扫一次」）而不是一句裸 ctx 错误，并用 `ctx.Err() == nil` 把
    「这个账号慢」与「整轮被掐」区分开（#59）。
  - **批任务的 `done` 行不再以 `failed=0` 冒充「一切正常」。** workbuddy/blackcat
    连续 8 晚 100% 被拒（`this account's board has no task "black_cat"`）、每晚 15
    分钟上游调用零收益，而 `done:` 行是 `refused=21 skipped=0 failed=0`——refused
    的语义（厂商说「不做」，不是传输失败）本身没写错，缺的是「全拒」这个事实没有
    说出来。现在 `allRefusedNote` 在「每一跑都被拒、零成功、零失败、零跳过」时给
    done 行加一句显眼尾巴（`<< ALL N ATTEMPTS REFUSED, 0 succeeded — check whether
    the vendor still offers this chore`）；计数语义一律不动，拒绝仍不计入 failed。
    另一半是**取证**：`fetchTask` 命中不到某个码时，把这块板子**实际持有**的码
    清单记一行日志（`workbuddy: board (web) has no "black_cat"; it carried: …`，
    上限 24 个并标注还有多少）。这行才是区分「厂商下线/改名」与「模块查错了地方」
    的证据——此前只有一句 refused，两种情况的处置完全不同（一个停批、一个修通道），
    而日志无法自证是哪一种（#64）。
  - 守卫测试：新增 `internal/panel/task_scan_timeout_ui_test.go`（并发不许退回 4、
    每账号子超时必须存在且真的进了扫描循环并在读出后 cancel、超时措辞必须区分
    「单账号慢」与「整轮被掐」、不许靠调大整轮预算来掩盖）、
    `internal/scheduler/batch_refusal_note_test.go`（拒绝不许计成 failed、提示只在
    「全拒且零成功」时触发、两个 done 行发射点都要接上）、
    `clients/workbuddy/board_miss_evidence_test.go`（miss 时必须列出板子实际持有的
    码、日志要有长度上限）。
  - README 中英两版同步了这三处行为（扫描并发与子超时、全拒提示、板子码取证）。

### Installer

- `client2api-setup-0.5.82.exe`（空包，不含账号凭据或本机配置）
  SHA256: `B1A1959A90C0282C57D69F3FE5A4E8BE0BF714EFBD486E9ADE58FF64B639D645`

## [0.5.81] - 2026-11-02

### Fixed

- **「优化机制建好了但实际没生效」一类（#113 + #112）**——两单都不是没做优化，
  机制都在，但一个被同一段代码的另一半覆盖掉、另一个只做了半程：
  - **日志页的两段式轮询真的生效了。** 面板的 4 秒一拍先问 `meta=1` 轻查询（约
    百字节），total 没变就早退——但**早退分支一次都命不中**：meta 戳写
    `LOGSEEN = "meta:<total>|<ch>"`，正文拉回来后同一拍又把它改成
    `"ok:<total>|<ch>"`，下一拍算出的 meta 戳与它前缀不同，永远不相等（`index.html`
    的 renderLogs）。当时给 meta 戳加独立前缀的理由是「三种状态不能互相去重」
    （#84 的教训），但三种状态本来就由 total 与频道区分，不需要靠前缀——那个前缀
    的实际效果是让 meta 守卫自己匹配不上自己。现在 meta 与正文写同一个
    `"ok:<total>|<ch>"` 戳（错误态仍是 `"err"`，且出错时作废增量游标）。
    另一半同样重要：只修戳不够——部署实例平均 **2.2 秒新增一行**、轮询 4 秒一拍，
    「total 未变」几乎不成立，所以新增 `/panel/api/logs?since=<total>` 游标：
    变了也只取新增的那几行（本机实测 11 行 = **2 351 B**，而整份 500 行是
    **10 631 B**）。游标已滑出保留窗口时（中间的行走掉了）服务端退回整份并带
    `partial: false`，页面据此替换而不是追加——否则会静默跳过一段日志。客户端在
    `partial` 时把新行接在已渲染窗口后面并按 capacity 截断，换频道 / total 回退 /
    出错都强制整份重读。
    实测（隔离实例，4 秒一拍的稳态）：日志静止的那 14 秒 **正文请求 0 次**、只发
    3 次 29 B 的 meta 探针；持续有新日志时是 `meta=1`（29 B）+ `since=<游标>`
    （486~1 336 B）配对，**整份 500 行的请求 0 次**。
  - **用量库不再每 30 秒把缩进版整份重写一遍。** `usage_store.go` 用
    `json.MarshalIndent(f, "", "  ")` 序列化整份文档，缩进纯属放大——实测同一份
    文档缩进版 2 520 878 B、紧凑版 1 561 255 B，**缩进独占 61.5%**；这个文件是
    机器读的。现在改用 `json.Marshal`（`recent.json` 同一处理）；另外 buckets 从
    map 迭代出来、每次顺序都不同，导致「数据没变」的两次 flush 也产出一份逐字节
    不同的文档（实测两份相隔 65 秒的文件只有 **13.6%** 字节相同），现在序列化前
    按身份键 `scope|client|realm|account|model` 排序——内容不变就是同一串字节，
    可 diff、可被备份/去重吃掉。分片 / append-only journal 那条方向改动面大，
    本轮不动。
  - **顺带修掉一条「PASS 而缺陷在」的守卫测试**：`logs_meta_probe_test.go` 的
    `TestLogsRenderUsesTheTwoPhasePoll` 把 `"meta:"` 那个前缀写成断言——它正是
    缺陷本身。改成钉三件行为（先问 meta、同形戳早退、正文按需拉取），戳的具体形态
    由新增的 `log_poll_incremental_ui_test.go` 钉。
  - 新增守卫：`internal/gateway/logring_since_test.go`（游标能接上 / 已接不上 /
    逐拍推进后与整份窗口一致）、`internal/gateway/usage_store_compact_test.go`
    （不许再用 MarshalIndent、排序必须在序列化前、以及真跑两次 flush 断言
    「数据没变 → 字节相同」与「落盘文档里没有缩进」）、
    `internal/panel/log_poll_incremental_ui_test.go`（戳同形、错误态作废游标、
    带 since、partial 追加与 capacity 截断、以及服务端确实解析 since 并标 partial）。
  - README 中英两版同步了这两处行为。

### Installer

- `client2api-setup-0.5.81.exe`（空包，不含账号凭据或本机配置）
  SHA256: `955D55619F681595388F4BE8E477B96818A2860B78185F561FE49D97F266F094`

## [0.5.80] - 2026-11-02

### Fixed

- **面板「统一机制/先例已经存在，但一整片消费者没接上」一类（#74 + #50）**——
  两单都不是「某处写错了」，而是同一件事只做成了一部分：
  - **字号改成相对单位，浏览器「只放大文字」能真正生效。** 面板原来的字号全是
    绝对 px：`font-size` 里 385 条是 px、**125 条 `font` 简写里 114 条也把字号写成
    px**（`font: 600 10px/1 var(--mono)` 这种形状——面板绝大多数读数、状态词、徽标
    走的都是简写，只改 `font-size` 会整片漏掉）、四个 `--type-*` 令牌与 6 处
    `clamp()` 上下界同样是 px。于是用户把浏览器/系统的「默认字号」调大（或 Firefox
    「仅缩放文本」）时，页面上一个像素都不动——WCAG 2.2 AA 1.4.4 失败。现在
    `html { font-size: 100% }` 作基准，三类声明（`font-size`、`font` 简写的字号段、
    `--type-*` 与 `clamp()` 边界）连同 JS 里那处内联字号全部改成 `rem`，
    1rem = 16px ⇒ 每个字号与原值**逐像素相等**（纯单位迁移，不动视觉）。实测：
    根字号设成 200% 后表头 10px→**20px**、单元格 12.5px→**25px**、导航
    12.5px→**25px**、正文 11.5px→**23px**；样式表里 px 字号规则 **0 条**、
    rem 字号规则 462 处（#74）。
  - **配置页有未保存改动的追踪了。** 32 个可编辑控件原来没有任何 dirty 状态：
    保存按钮恒亮（分不清「已保存」与「没动过」）、没有标记、改了一半切页或关 tab
    静默全丢。而排程页早就有同款机制（`scHasDirty` / `scUpdateDirty` +
    `beforeunload` 拦截，`index.html:19462`）——「表单页防丢改动」的产品约定在本
    面板已经成立，只是配置页没接上。现在照抄同一套形状：`cfgHasDirty` /
    `cfgUpdateDirty` / `cfgMarkDirty` / `cfgClearDirty`（不另立第二套），
    保存按钮只在有改动时亮起、保存成功后回到禁用态（禁用态本身就是保存反馈），
    旁边出现「● 有未保存的改动」标记；控件改动经 `#cfgForm` 的 input/change 委托
    点亮 dirty；**编辑期间 5s 轮询不再重画表单**（原来重画会把正在输入的值清回
    磁盘旧值——比切页丢改动更隐蔽）；离开拦两道：关 tab / 刷新走 `beforeunload`，
    页内切视图走导航 funnel 点 `go()` 的一次确认（用户报的主症状正是「误点导航
    全部还原」，而 `beforeunload` 管不到 hash 切页），选「留下」则把 hash 改回
    `#config`（#50）。
  - **实测**（隔离实例 v0.5.80，#50 的完整回路）：初始 `dirty=false` / 保存按钮
    `disabled=true` / 标记隐藏 / 32 个控件 → 改一个输入框后 `dirty=true` / 按钮亮起 /
    标记显示 → 等过一轮 5s 轮询，**正在输入的值仍在**（表单没被重画）→ 点保存后
    dirty 清空、按钮回到禁用态 → 再改一次后点导航，弹出「配置页有未保存的改动，
    离开将丢失。要放弃这些修改吗？」，选「留下」后视图与 hash 都留在 `#config`。
  - 守卫测试：新增 `internal/panel/unified_mechanism_coverage_ui_test.go`——
    ① `pxFontRE` 扫全样式表，任何 `font-size` / `font` 简写里的绝对像素字号都算违规，
    四个 `--type-*` 令牌与 `clamp()` 边界必须含 `rem`，`html` 基准必须显式声明，
    rem 出现次数下限兜住「只改两条就宣称完成」，JS 内联字号同样相对化；
    ② `cfgHasDirty` 必须存在、`beforeunload` 必须接上配置页分支且不许拆掉排程页
    先例、切页路径必须共用同一个判据函数、`cfgLeaveBlocked` 必须同时看「在配置页」
    与「有改动」。顺带把 `a11y_skip_link_caps_ui_test.go` 那条钉死 `font: 550 13px`
    字面量的断言放宽到只钉颜色与字重（字号单位迁移不该把它判红）。
  - README 中英两版同步了这两处行为。

### Installer

- `client2api-setup-0.5.80.exe`（空包，不含账号凭据或本机配置）
  SHA256: `C9B4C597CF43A3023411EDDFD71EE48B215C3503923E0E086594BB75001D9650`

## [0.5.79] - 2026-11-02

### Fixed

- **面板常驻性能热点一类（#91 + #65）**——面板是「一直开着守一个网关」的界面，
  而这两处成本是无条件的：一层不承载任何信息的装饰画布按屏幕刷新率整块重绘，
  一批只进不出的铭牌贴图跟着读数每拍长大。两处都不报错、功能也正常，只是在
  时间里持续付费：
  - **背景粒子场的开销有了上界。** 三条约束（都不动视觉形态）：
    ① **背板像素量封顶** `BGF_MAX_PX = 1.3e6`——原来 `dpr = min(2, devicePixelRatio)`
    让 2x 屏上背板是 2860×1696 ≈ 4.85 Mpx，而**粒子数（与 dpr 无关）只从 25 涨到
    46**，像素量翻四倍买到的只是更细的 1px 线；按面积上限等比降 dpr；
    ② **降到 ~30fps**（用时间戳自节流，不用 setInterval——后者与 rAF 的节拍会打架），
    纯氛围的漂移连线看不出 30 与 60 的差别；
    ③ **页面不可见时不画**（Page Visibility）。**窗口失焦不停**是有意的取舍并在
    注释里写明：把面板停放在第二块屏上盯着看正是常态，失焦即冻结会被当成卡住；
    从隐藏回到可见时 `bgfWipe()` 抹掉「上次可见那一帧」，否则会先闪一下冻结的
    旧连线（#91）。
  - **3D 铭牌贴图缓存带上界并会淘汰。** 三处缓存的键里都带了会变的读数
    （`b3dTexture` 的 `value`/`frac`、`b3dBarLabel` 的 `value`、`atlasLabelTexture`
    的 `count`），而缓存对象没有上限、没有淘汰，`b3dClear()` 只 dispose 几何体与
    材质、**从不碰 `material.map`**，唯一的清空点是切主题——实测每拍 +2 条缓存 /
    纹理 +2 / +2.22 MB，两张卡约 26 MB/分钟，24 张卡每拍 24.5 MB。现在：
    ① `texCacheSweep` 给两处缓存 96 条的上界，淘汰时 `dispose()` 掉纹理，
    **跳过 live 键**（`texCacheMarkLive` 在每次重建结束后登记「这一轮真正取用过的
    键」）——没有这道保护会把正在渲染的贴图拆掉，板卡变黑；
    ② 切主题改走 `texCacheReset`（逐个 dispose 再清空），原来的 `texCache = {}`
    只是丢引用、纹理仍占显存，来回切两次就漏一批；
    ③ `refreshAll` 末尾不再无条件 `mountBoard3d(VIEW)`：改走签名判断（复用同文件
    `afterViewData` 与 1.5s tick 早就写好的口径），读数没变就不重建，直接砍掉空载下
    绝大部分贴图生成与场景重建；
    ④ `b3dBarLabel` 的纹理此前连 `b3dClear` 都不经过（只挂在柱阵的材质上），
    现在同样受上界与 dispose 覆盖（#65）。
  - **实测**（隔离实例，CDP `Performance.getMetrics` + 包 `ctx.stroke/arc/clearRect`
    计数，1440×900）：降频生效——用 16ms 定时器驱动 rAF 模拟 60Hz 时，循环被调用
    92 次/秒而实际绘制 **46 次/秒**（原来 60Hz 显示器上就是 60）；像素封顶生效——
    `deviceScaleFactor: 2` 下 `BGF.dpr` 被压到 1、背板 4849203 → **1212640 px
    （少 75%）**，而粒子数仍是 46（本来就与 dpr 无关）；不可见时不画——把
    `visibilityState` 置 hidden 后 1.5s 内 `clearRect` 调用 **0** 次；贴图缓存
    封顶——模拟「22 张卡、读数每拍都变」跑 20 拍，缓存从 31 涨到 96 后**停在 96
    不再增长**，且这一拍真在用的 22 张全部保留（另测：222 条里淘汰到 96，live 的
    22 条一条没丢）；切主题时 **11 张纹理全部被 dispose** 且缓存清空。
  - 守卫测试：新增 `internal/panel/panel_cost_ceiling_ui_test.go`（像素上限常量与
    它在 `bgfResize` 里生效、节流判断必须在 `clearRect` 之前、不可见不画、
    「只按 Page Visibility」的取舍注释必须在、缓存上界与 dispose、live 保护、
    三处 live 登记点、切主题不许再用 `texCache = {}`、`refreshAll` 的签名判断）。
  - README 中英两版同步了这些约束与取舍。

### Installer

- `client2api-setup-0.5.79.exe`（空包，不含账号凭据或本机配置）
  SHA256: `AD862454AC298A6BB4954104A7DE3F3DBF102B97A0579907A26F279EE9F85004`

## [0.5.78] - 2026-11-02

### Fixed

- **面板「判据/时机写了一半」一类（#55 + #61）**——两单都不是某块功能坏了，而是
  同一条链的两个时刻各只写了一半，而且两边都不报错：
  - **切回已经看过的视图不再重播整页入场动画。** 「进场编排只演一次」的机制断成
    两半：`go()` 照旧给视图打 `settle`、注释也还写着「只演一次」，但**读它的那份
    代码已经不在了**（历史实现是一族 CSS 读 `.settle`，随「区块级编排改由 .fx-in
    负责」那次提交一起被删——全文件 `grep -nE "\.settle[ ,{:]"` 零命中）；剩下那句
    无差别的 `querySelectorAll("[data-fx-done]")` 又把整页重置成「没演过」，于是
    每次切回来都重演一遍：配置页实测结论与报告完全一致——第 1 次进 `settle=false`、
    12 条 `fxInUp` 在跑、尾部延迟 340ms；第 2/3 次进 `settle=true` 却**照样 12 条在
    跑、尾巴 408ms**。现在把判据放回代码里：`fxCascade()` 首行读 `.settle` 直接返回
    （`index.html:10293`），`go()` 不再无差别清 `data-fx-done`（那次清除本意是让
    卡片墙重画时能再演，但它连没被重画的静态区块一起重置；同一页内原地重画的数据
    本来就不经过 `go()`，新生成的元素没有 `fxDone`，该演的照样演）。修后实测：
    首演仍是 12 条/340ms，第 2/3 次进变成 **2 条/34ms**（只剩本轮真新生成的元素），
    不再随切页次数增长。
  - **任务看板切换客户端时不再同时呈现两个客户端的内容。** `renderTaskBoard()`
    在发 `/tasks` 请求前先重画了账号条（于是新客户端的账号标签立刻出现），却把
    头部 `#tbNote` 与正文 `#tbBody` 留到响应回来才更新——loomy 这类上游慢的往返
    动辄一分钟，整个窗口里页面上是「新客户端的账号条 + 上一个客户端的摘要与行」，
    既没有加载态也没有任何提示，用户分不清「点了没生效」还是「在加载」。现在在
    `await` 之前就把两者推进过渡态（`#tbNote` → 「<客户端> · 扫描中…」、`#tbBody`
    → 「正在读取 <客户端> 的任务状态…」），旧正文因此不会和新账号条并排（#61）。
  - 守卫测试：新增 `internal/panel/entry_and_switch_state_ui_test.go`——钉住
    `fxCascade` 必须读 `.settle`、`go()` 必须继续写它且不许恢复无差别清除、
    `#tbNote`/`#tbBody` 的两个过渡态必须写在 `await` 之前、账号条仍在其前；
    CSS 那段「切回来的视图带 .settle，直接就是最终状态」的注释也一并钉住，免得
    下一个人照着旧注释改回「写了没人读」的写法。
  - README 中英两版同步了这两处行为。

### Installer

- `client2api-setup-0.5.78.exe`（空包，不含账号凭据或本机配置）
  SHA256: `D2092979CD876F84EFD013042EB81AFA95D4F15BB9B8D14D83213B821BBF0EF5`

## [0.5.77] - 2026-11-02

### Fixed

- **无障碍：状态/偏好通道已经存在，但漏接了消费者一类（#75 + #54）**——两单
  同型：机制都在（ARIA 属性、`motionReduce()` 分支、`bgfStop()` 实现），只有
  某一个消费者从没被订阅，而且两边都不报错：
  - **三处二选一开关的选中档现在能被读屏读到。** `#themeToggle`（深色/浅色）、
    `#view3dToggle`（3D/列表）、`#acctModeToggle`（卡片/表格）都只用
    `class="... on"` 表达选中态，同步点（`index.html:21142`、`:13546`、`:13667`）
    三行旁边都没有任何 ARIA 状态——AX 树里这 6 个按钮是 6 枚一模一样的裸
    `button`，读屏用户点完「3D」听不到状态变化，而画面变化恰恰是他们看不到的
    （WCAG 2.2 AA 4.1.2 Name, Role, Value）。现在三处都补 `aria-pressed`：
    静态 HTML 六个按钮各带初始值，三个同步函数在 toggle class 的同一行用同一个
    `on` 变量写状态，不会出现「看着亮、读屏说没选」。同页的指标筛选族
    （`:11263`）本来就是这么做的，这次是补齐同一套口径（#75）。
  - **运行中切「减少动效」时两块 2D canvas 也停了。** `onReduceChange`
    （`index.html:24664`）只接了 WebGL 两块（`atlasReduceSnap` /
    `b3dReduceSnap`）；CSS 靠 `@media` 立即生效，所以切换那一瞬间看起来「已经
    生效」——唯独背景粒子场与点击火花照旧，一直动到下次刷新，而 `bgfStop()`
    全文件只有定义、零调用。这是 #30「只覆盖一半比完全不支持更糟」的同一类，
    只是剩下的一半从 WebGL 换成了 2D canvas。现在：
    ① `onReduceChange` 进入 reduce 调 `bgfStop()`、离开时调 `bgfStart()`；
    ② `bgfStop()` 停循环之外再按当前尺寸清一帧——只 cancel 的话粒子连线会结在
    屏幕上，冻结的残影看起来像画面卡住；
    ③ `initSpark` 的 `pointerdown` 回调在使用点再判一次 `motionReduce()`
    （顶上那个守卫只在建监听时查一次，运行中切换后监听已经挂着）；
    ④ `bgfStart()` 改成幂等（`BGF.raf` 守卫 + resize 监听只挂一次的 `BGF.wired`），
    因为现在它会被首屏与「离开 reduce」两处调用（#54）。
  - 守卫测试：新增 `internal/panel/reduce_motion_consumers_ui_test.go`（stop 函数
    必须存在且被调用、两个方向都接、停下要清画布、`bgfStart` 必须幂等、火花的
    偏好判断必须在使用点且在生成粒子之前）；`a11y_ui_test.go` 追加
    `TestToggleGroupsExposeTheirSelectedSide`（六个静态初始值 + 三个同步点，
    并要求状态与 class 由同一个变量驱动）。
  - README 中英两版同步：写明三处开关用 `aria-pressed`，以及运行中切「减少
    动效」对 CSS / 两处 WebGL / 两块 2D canvas 同时生效。

### Installer

- `client2api-setup-0.5.77.exe`（空包，不含账号凭据或本机配置）
  SHA256: `786049E9A08A846E632BBDC1925788787F01F730A84AB82AEE9F8030E1F8CB08`

## [0.5.76] - 2026-11-02

### Fixed

- **面板「名字/占位承诺了行为、实际零动作或对不上」一类（#107 + #110）**——
  两单同型：界面写着的那句话与代码真正干的事分叉了，而且两边都不报错：
  - **「重新探测」真的去探测了。** 这颗按钮原来接的是 `renderClients`，而后者
    在 `OVER_FULL` 为真时第一步就短路（#93：overview 已经带回了每个模块的
    账号，不再 fan-out）——于是整颗按钮只剩「把内存里的表重排一遍」：零请求、
    零反馈、文案与 `disabled` 都不动，而名字承诺的是探测能力。而能力块恰恰只能
    靠 `/panel/api/overview` 的一次重读拿到（服务端的 `p.capabilities()` 是每次
    请求由模块类型现场折出来的，不缓存）。现在新增 `probeClients()`
    （`internal/panel/index.html`）并把它接上按钮：重读 overview → 重读各模块
    账号 → 重画本页，在途置灰 + 「探测中…」、跑完报「已重新探测 N 个客户端的能力」、
    失败把真实原因说出来，与账号页「刷新凭据」同一套反馈口径。`renderClients`
    的 `OVER_FULL` 短路原样保留——「进入页面」与「用户点按钮」是两条路径（#107）。
    同文件里的守卫测试 `refreshbutton_ui_test.go` 那条 keep 注释（原文写着「探测
    能力」而实现只是重排 DOM）现在与实现对齐，并追加一条断言：按钮必须接
    `probeClients`，不能退回 `renderClients`。
  - **链路测试的按钮不再有两个名字。** 静态 HTML 叫「预演顺序」，而
    `runRoutePlan` 的 `finally` 把它复位成「测试链路」、`runRouteSpeed` 复位成
    「测速」，空态指引又照着「测试链路」写——首屏页面上根本不存在那个名字
    （按钮只在跑完一次预演后才叫它）。现在三处统一：静态 HTML「预演顺序」/
    「实测速率」、在途文案「解析中…」/「测速中…」、复位值等于各自静态名字，
    空态指引改指「预演顺序」；顺带消掉多开预演时按钮名在「预演顺序 / 解析中…」
    之间闪烁的竞态窗口（#110）。README 中英两版同步：明确两颗按钮的名字，
    并补上客户端页「重新探测」的行为说明（它此前在 README 里没有条目）。
  - 新增守卫 `internal/panel/promise_vs_behavior_ui_test.go`：`probeClients` 必须
    重读 overview + 账号 + 重画本页、必须有在途与收尾语句与 toast、`renderClients`
    不许被拿掉短路或被塞进在途文案、两颗路由链按钮的静态名与复位值必须逐字相等、
    界面文本里不许再有「测试链路」这个首屏不存在的名字（注释里的历史说明不算）。

### Installer

- `client2api-setup-0.5.76.exe`（空包，不含账号凭据或本机配置）
  SHA256: `C717F8C563B23EC69E3B49B72985F73F4B54C22788FF9A6196A071EAF4559C33`

## [0.5.75] - 2026-11-02

### Fixed

- **面板 CSS「层」缺陷一类（#109 + #111）**——两单都不是某个元素写错了，而是
  同一属性被好几层规则说话、后写的那层把先写的吃掉，读单条规则看不出来：
  - **卡片头筛选条的选中态不再被次级化规则吃掉。** `.box > header button.xs`
    那条「卡片头动作是次级」的规则在 #86 修主按钮时加了 `:not(.primary)`，
    权重从 (0,2,1) 抬到 **(0,3,1)**，正好越过选中态规则 `.chips .chip.on`
    (0,3,0)——选中档的 accent 渐变底与描边被换成卡片底色：浅色主题下白底白字
    （对比度 1:1，整枚胶囊一个字都看不见），深色下与旁边未选中档同貌，只剩状态
    点颜色不同。修法：两条规则再补 `:not(.on)`，让 `.chips .chip.on` 重新成为
    选中态的唯一说话者（#109）。
  - **选中档的 hover 文字色不再翻转。** 三条 chip hover 规则（`:1872`、`:2682`、
    `:4813`）与 `.chips .chip.on` 同权重且更后，把选中档的文字色改成 `--ink` /
    `--accent-text`，鼠标一进一出文字就在「看得见 / 看不见」之间翻——这是 #109
    症状的第二个来源（第一条是卡片头规则）。三条都补 `:not(.on)`（#109）。
  - **账号池统计带在窄窗真的重排，不再被无声裁掉。** `:880-882` 那三条媒体查询
    只写 `grid-template-columns`，而统计带的权威定义（`:5256`，10-28 起）已经是
    `display: flex; flex-wrap: nowrap`——容器不是 grid、同属性更早出现，三条从
    那天起就是死代码。于是 10 格 × `min-width: 112px` 硬撑，溢出被 `.viewwrap`
    的 `overflow-x: hidden` 整块裁掉：1024px 下最后两格（就绪客户端、粘性会话）
    完全不可见，而「粘性会话」是弹层的唯一入口，鼠标用户看不到也点不到。修法：
    删掉三条死代码，把重排写成 flex 认识的口径并放在权威定义之后（1160px 起退
    4 列、560px 起退 2 列、900px 起收字号内边距），`grow` 设 0 以免 4+4+2 的
    末行两格被撑成半行大块；换行后把选中底边（原 `bottom: -5px`）收回格内，
    免得它画穿下一行（#111）。
  - 三处守卫测试：新增 `internal/panel/panel_css_layers_ui_test.go`（卡片头规则
    必须同时豁免 `.primary`/`.on`、三条 hover 必须豁免 `.on`、统计带不许退回
    grid 死代码或横向可滚写法、重排必须在权威定义之后）；`header_primary_contrast_ui_test.go`
    的断言从「豁免 .primary」改为「同时豁免 .primary 与 .on」，防止任一侧被去掉。

### Installer

- `client2api-setup-0.5.75.exe`（空包，不含账号凭据或本机配置）
  SHA256: `3FBE784638063E9EF125498A247B28F408679E89B5717C5A2E2C91439538DA35`

## [0.5.74] - 2026-11-02

### Fixed

- **面板 UX：名字/占位承诺了行为、实际不同步或零动作一类（#105 + #106）**：
  - **任务中心空态不再与 chips 自相矛盾滞留。** 批量执行页的「成长任务队列」
    有两组互不知情的渲染：chips 每次按能力清单重画，而空态 `#tcEmpty` 只在
    `cur == null` 分支才被写入并显示——`cur` 非空时 `renderQueue` 根本不被调用，
    于是在能力数据还没到手的那一次 `renderQC()` 里写进去的「还没有客户端提供
    任务看板」无人收回，等 chips 画出 5 个支持任务的客户端后，假文案与 chips
    同屏并存直到用户真的点一次「扫描」（#105）。现在 `renderQC()` 在「客户端
    存在」那条路上显式归位：恢复静态默认空态文案并藏起，数据交给扫描路径。
  - **「/」快捷键不再按视图 id 写死目标。** 旧绑定只接了 3 个视图（模型
    `#mdSearch`、账号池 `#accSearch`、对话页强制展开高级面板再聚焦
    `#chatModelFilt`），同一份面板里却有 6 个占位写着「（/）」的搜索框，其中
    4 个（模型·对外模型 `#exSearch`、平台·平台与模型 `#pfSearch`、任务中心·
    定时任务 `#scSearch`、动态生成的 `.pfGroupSearch`）按下去毫无反应——它们
    所在的子页把首选目标收进了 hidden 的兄弟面板，而绑定没有回退分支。现在按
    当前视图内「第一个可见的 search/.filt 输入框」解析，找不到再回退原三档；
    对话页的落点改为主消息框 `#chatInput`（原先跳到「缩小模型列表」属于有反馈
    但反馈错了地方），也不给没有任何搜索框承诺的视图强塞目标（#106）。
  - 两条守卫测试同步改口径：`chat_sidebar_ui_test.go` 那条原本钉住
    `chatOpenAdvanced(true)` 的断言已与实现分叉，改为钉可见性过滤 + 对话页
    落主消息框，并断言不再退回强制展开面板的旧行为（`chatOpenAdvanced` 自身
    的展开语义仍由工具条按钮使用，保留断言）。

### Installer

- `client2api-setup-0.5.74.exe`（空包，不含账号凭据或本机配置）
  SHA256: `4915FEEF28E0F52F9B18A05AC482426DEFAB0887B9A78C3AF633C144A57BEBAA`

## [0.5.72] - 2026-11-02

### Fixed

- **officeace 零积分账号的耗尽否决不再 30 分钟过期——自锁断开（issue #104，
  #57 的后续）。** #57 让「读数见 0 停车」之后留了一个洞：停车后 5 分钟软扫
  跳过 cooling 账号、恢复扫 4 小时一轮，而 chat 401 的否决窗只有 30 分钟——
  冷却到期时缓存必死，下一轮 chat 撞 drained package 落回死 key 分支写成
  invalid，而 invalid 是两个扫描都不再读的状态（自锁）。否决语义升级为「0
  之后无正读数」：积分缓存里最近一次读数为 0 即持续否决，正读数（充值/签到）
  既结束否决也清除停车，账号回到轮转。30 分钟窗口常量删除；窗口过期测试改
  写为新语义并补「冷却到期后 chat 401 仍走 quota 路径」的回归测试。

### Installer

- `client2api-setup-0.5.72.exe`（空包，不含账号凭据或本机配置）
  SHA256: `ECCE0DF3921C4F96EDB21C235511E2DBBE8D614990391B4987C0A6AFD62E3034`

## [0.5.71] - 2026-11-02

### Fixed

- **面板写接口加来源闸（CSRF 防线，issue #103）。** 面板的全部写方法
  （POST/PATCH/PUT/DELETE）此前不校验来源：同机任意网页一次跨站 POST 就能改写
  配置并落盘（curl 实测 200），而 no-cors 表单/fetch 恰好能发出这种请求。
  `authed` 包裹层现在对写方法检查 `Origin` / `Sec-Fetch-Site` / `Host` 的
  组合：同源放行；无来源的同机工具（curl、面板自身 fetch）放行；Host 非本
  面板/回环（DNS rebinding）或跨站来源一律 403。测试里 60+ 处写请求的 Host
  默认值成片修正为回环——同一类问题一次收拾。

### Installer

- `client2api-setup-0.5.71.exe`（空包，不含账号凭据或本机配置）
  SHA256: `6A2B2BE51CD2E64FFD36B6FAF9F660D5998851F4BCF684BDE593515479313346`

## [0.5.70] - 2026-11-02

### Fixed

- **两处 combobox 的键盘高亮对辅助技术可见。** Ctrl+K 命令面板与模型页客户端
  筛选都是「焦点保留在输入框」的手搓 combobox：上下键在视觉上挪高亮，但
  option 既没有稳定 id、输入框也没有 aria-activedescendant——读屏全程只听空
  输入框，Ctrl+K 跳视图对读屏用户不可用。两处 option 生成时带稳定 id
  （`cmdk-opt-<i>` / `mdclient-opt-<i>`），重画后把输入框的
  aria-activedescendant 指到当前高亮项，空列表/无高亮时摘掉；贴合现有结构，
  视觉与键盘行为不变（issue #101）。

### Installer

- `client2api-setup-0.5.70.exe`（空包，不含账号凭据或本机配置）
  SHA256: `2E766B5081226DCBFC94D1CD64DBAC749C49068B85B092B20CB523004C4DA386`

## [0.5.69] - 2026-11-02

### Fixed

- **「跳到主内容」的正文对比度跟随主题。** 写死的 `color: #fff` 在深色块的
  --accent（#4c7dff）上只有 3.69:1（浅色 4.84:1 达标）——这是键盘用户的
  第一个焦点，2.4.1 的唯一实现入口。改用 `var(--accent-ink)`，与面板其余
  --accent 底按钮同一口径，底色与字色一起按主题走（issue #102）。
- **客户端能力矩阵的两态对辅助技术可分。** 154 个单字格的「有 / 没有」此前
  只有视觉在承载：开启格写「管」、关闭格画 3px 淡点，AX 树里两态是同一个
  generic 节点（读屏念出来都是能力名），而列头行还整行 aria-hidden——「第
  N 格是什么」的上下文也拿不到。每格加 `aria-label`（「有：接码」/「无：
  接码」），表头行去掉 aria-hidden、每个能力列给 AX 真名（能力+单字），
  视觉不变（issue #100）。

### Installer

- `client2api-setup-0.5.69.exe`（空包，不含账号凭据或本机配置）
  SHA256: `8017EB5C9D09A86AA595E7C70F17E43995722A5C30E1E14F75BFBC05B7C45E24`

## [0.5.68] - 2026-11-02

### Fixed

- **华为平台账号不再在「冷却中」↔「账号异常」之间整夜翻转。** 余额/模型路径
  的 IAM 401（APIG.1009，签名令牌过期竞态——AppKey/AppSecret 是常量，同账号
  几分钟内 chat 全部 200）此前会写 `s.invalid`，行被打成「账号异常」引导重登；
  下一次 chat 200 又把 invalid 清掉翻回来。读数路径的职责是数字不是凭据：
  `noteBalanceVerdict`（balance.go）保留停车（cooldown），invalid 判定只属于
  chat 路径——那里有 200 的直接反证机制。真实死 key 仍由 chat 路径判为
  账号异常，重登语义不变。
- **账号表操作列不再把行高撑到 2~3 倍。** #44 把操作列的宽度还回来之后
  （1440px 下约 300px），写死的两列网格把每颗按钮拉到 144px、行高撑到
  137~198px——一屏只能看全 1 行。网格改 `repeat(auto-fit, minmax(96px,
  1fr))`（308px 排 3 列，行高与同行一致），窄屏兜底同步 auto-fit 84px；
  「领取每日签到积分」等长标签由 .test-split 整行豁免不被截断。

### Installer

- `client2api-setup-0.5.68.exe`（空包，不含账号凭据或本机配置）
  SHA256: `4EDB725F587019E5EB2431A5B64CCD34F91CEF89DDED194EA03697356F46E5B3`

## [0.5.67] - 2026-11-02

### Fixed

- **WAF IP 闸与降级提示词窗口在面板上有了出口。** 这两个进程级闸改变整站行为
  （前者停掉整条出口 IP 的轮转 60 秒，后者把受闸模块的系统提示词换成中性版到
  次日 0 点），此前只在日志里各有一行，面板无任何出口——操作员把整站故障归因
  到账号或模型。三处出口：`/panel/api/overview` 带上 `guard` 快照（闸全关时
  省略该字段）；总览在闸开着时给全局状态条（「WAF 拦截：出口 IP 轮转已暂停，
  至 HH:MM:SS」/「降级提示词生效中，至次日 0 点」）；两闸开启时各写一条告警
  （新 Kind `waf_ip_blocked` / `prompt_degraded`）——告警页空态那句「WAF 拦截
  这类事件会记在这里」从此是事实。

### Installer

- `client2api-setup-0.5.67.exe`（空包，不含账号凭据或本机配置）
  SHA256: `5D2E5C6E2D24E2721FCAFD467B9C72774C2D64F566EBF8619EEB30848E8200F1`

## [0.5.66] - 2026-11-02

### Changed

- **PR 门禁补上 installer 路径（issue #99）。** go-binaries.yml 的
  pull_request.paths 不含 installer/**——只改安装器的 PR 一条 CI 都不跑，而
  windows-test 这个 job 存在的理由正是编译那批 windows-only 代码。paths 补上
  `installer/**`、`third_party/**`（构建要暂存的 vendored ffmpeg）与
  `Makefile`；gate 测试钉住三行不许再被摘掉。
- **/panel/api/status 接上 >1s 慢响应观测（issue #96 方向 3）。** 偶发 ~5s
  尖刺、瞬时恢复、疑似与余额扫描锁面相关——归因需要一周量级的样本。超过 1s
  的响应写进运行日志一行，带耗时与落入 5 分钟 tick 窗口的位置；跑一周看
  尖刺与 sweep tick 的相关性，再决定是否缩小锁面或改原子快照。

### Installer

- `client2api-setup-0.5.66.exe`（空包，不含账号凭据或本机配置）
  SHA256: `76D57079197F808DCDB953BD214E5E9015276407176DAEAF455A3C52BE2D0888`

## [0.5.65] - 2026-11-02

### Changed

- **运行日志页改两段式轮询：先问计数再取正文。** 4 秒一拍原先整份重下 500 行
  （约 133 KB），静止时 0 行新增、正文逐字节相同。`/panel/api/logs?meta=1`
  轻查询只回 total/capacity（约百字节），前端先问计数、没变就早退，变了才把
  500 行拉回来；meta 戳带独立前缀与正文戳/错误戳互不同名（#84 的教训：三种
  状态不能互相去重）。完整查询的响应契约不变（entries 恒为数组），既有
  logsResponse 断言原样通过（issue #92 方向 1；ETag 对追加式日志意义有限——
  新增一行即整份失效，不做）。

### Installer

- `client2api-setup-0.5.65.exe`（空包，不含账号凭据或本机配置）
  SHA256: `50759D13C5AC42490513BA7E37529401A7317BF68FCDB04428DE00D8E1F46831`

## [0.5.64] - 2026-11-02

### Changed

- **面板轮询接上 Page Visibility 门控。** 两个全局轮询（状态 5s、日志 4s）
  原先无条件运转——面板挂后台标签仍每 5 秒打 /status + /pool，一小时
  ~1440 次没人看的 GET。新 `setIntervalVisible` 包装：hidden 时跳过 tick；
  `visibilitychange` 恢复可见且距上次执行已满一个周期（5s）时补跑一次
  `refreshVisible`，操作员回到面板看到的是当前数据而不是一个周期的陈旧。
  登录/接码/录音等短生命周期的用户主动轮询不在本单范围（issue #95 方向 1；
  方向 2 的节奏分档涉及各页新鲜度取舍，留作后续单议）。

### Installer

- `client2api-setup-0.5.64.exe`（空包，不含账号凭据或本机配置）
  SHA256: `34913B1F1B9F24B2E08A0DEC7281B24E8CBE2AF8A0A50A0CF3C014D61A730830`

## [0.5.63] - 2026-11-02

### Changed

- **移除零消费者的 motion.min.js（64 KB，每次加载都下载）。** 全站弹簧动效走
  自研的 springValue，该库没有任何读取点；屏蔽对照实验证明面板功能与动效均
  不依赖它。script 标签、embed、allowlist 分支、vendor 文件一并移除，「Motion
  给全站的弹簧进场编排」的误导注释改写为移除记录（issue #94）。
- **客户端页不再把 overview 已带的账号重新下载一遍。** renderClients 第一行
  无条件 loadAllAccounts()——21 个请求 / 89 KB，与 overview
  （full_accounts=true）刚带回的那份逐字节相同。沿用 refreshAll 的短路口径：
  OVER_FULL 为真时不再全量 fan-out，强制重读走顶栏刷新的 loud 路径（issue
  #93）。

### Installer

- `client2api-setup-0.5.63.exe`（空包，不含账号凭据或本机配置）
  SHA256: `A3FC722767B21D583ED1AD6EA119476B1F6823ADE3EB7533979C9B2A9BA03278`

## [0.5.62] - 2026-11-02

### Fixed

- **卡片头主按钮不再白底白字。** 卡片头按钮的次级化规则（`.box > header
  button.xs`，权重 0,2,1、后置于 button.primary）把主按钮的 accent 底换成
  卡片底色——浅色主题下「添加中转站」「保存并生效」白底白字（对比度 1:1），
  深色下主操作与旁边的次级控件同貌；hover 的文字色也一并被改成正文色（深字
  压蓝底仍不达标）。次级化规则与 hover 文字色都用 `:not(.primary)` 豁免主
  按钮，主按钮恢复蓝底白字。
- **账号表「成功 / 失败」列头不再被「在途」盖住。** 该列 5% 装不下 nowrap 的
  标签，sticky 表头按 DOM 序后列压前列——1440px 及以下看不到排序箭头，点箭头
  位置实际排序的是邻列「在途」。列宽 5%→6%（「在途」4%→4.5%，操作列 22.5→21
  让出总额），可排序列头加 overflow 收拢兜底；colgroup 总额钉在 100%（既有
  测试守卫）。

### Installer

- `client2api-setup-0.5.62.exe`（空包，不含账号凭据或本机配置）
  SHA256: `8F5785753A40043E92EF8104AD1D2332EFB4D62EAEAE61E318C36737B882E761`

## [0.5.61] - 2026-11-02

### Fixed

- **页头巨型标题不再随 5 秒轮询反复重播。** mountMasthead 挂在轮询路径上且每
  次调用都无条件重拆标题 span（每片带 from opacity:0 的入场动画）——新元素 =
  动画从零开始，稳定的账号池页上整行标题每 5 秒闪一遍。拆分只在首建时做一次
  （childElementCount 守卫），后续轮询只更新右侧读数；reduce 用户保持原有的
  textContent 路径。
- **账号轮转时归还在途槽，面板「在途」列与平台总闸对得上。** codearts 等模块
  在自己的重试循环里换账号时，上一账号的网关租约一直骑在请求上——轮转点
  （busy 跳过、刷新失败、队列拒绝、可重试上游错、探测拒绝）不归还就 continue，
  槽位被占满整轮，账号池「在途」列与「平台在途」总数对不上。轮转点现在统一
  `req.ReleaseAccountSlot()`（幂等）；同族排查覆盖 autoclaw 的 busy 路径与
  cline 的刷新重试/死凭据轮转。terminal/return 路径本就由网关兜底
  （releaseStream.Close），未动。

### Installer

- `client2api-setup-0.5.61.exe`（空包，不含账号凭据或本机配置）
  SHA256: `F2ADB4473877F9A7E65C47BC6BBBBA6741B505A218E065E88D7FC3A0A60464CC`

## [0.5.60] - 2026-11-02

### Fixed

- **冷启动不再用肯定语气回答还没问过的问题。** renderAccounts 先判 !CUR
  （CUR 要等 /overview 回来才赋值，首帧必为空）提前 return，BOOTING 的
  「正在加载账号…」占位永远不可达——中间几秒页面说「没有已注册的客户端」而
  实际有几十个；总览的「全部正常」「还没有接入账号」同样在数据落地前就挂出
  来，后端真挂掉时与「全新安装」长得一模一样。三处门控：账号池分支先判
  BOOTING（表格与卡片墙都写「正在加载账号…」）、统计格冷启动显示「-」而不是
  0（0 是「一个账号都没有」的肯定结论）、总览的绿字与空态文案按 BOOTING
  分别给中性占位。BOOTING 里的 .skel 骨架样式仍留作后续，本单只接分支。

### Installer

- `client2api-setup-0.5.60.exe`（空包，不含账号凭据或本机配置）
  SHA256: `8CD1A67174476D1A3A73D6BA80A523C378846359EEC56BDFDF230E54DBB010DB`

## [0.5.59] - 2026-11-02

### Fixed

- **运行日志页的一次读取失败不再永久粘住。** 失败分支只写 DOM 不清去重戳，
  成功分支的去重早退又在渲染之前——日志没新增行时 total 不变、戳与失败前
  相同，恢复后的成功响应被整段丢掉，页面永远停在「读取失败」（只能靠切频道
  或等网关恰好写新行）。现在上一轮成败编进去重戳：失败记 err 戳、成功戳带
  ok 前缀，两者永不相等，恢复后的第一次成功必然重画；反向（一直成功但停更）
  也不再把两种状态画成一样。
- **配置读取失败后「保存」不再可点、不再撒谎。** 配置页读失败时表单区一个
  可编辑控件都没有，保存按钮却仍可点——点下去 null CFG 走进别名 diff 抛未
  捕获 TypeError，boot 红条把堆栈原样钉在页面底部且不可关。三层修复：
  renderConfig 失败分支同时禁用保存/重启按钮；saveConfig 与 savePlatforms
  开头各加「还没读取成功」守卫（平台页原本会落到「已保存，内容没有变化」的
  假成功——一个非 GET 请求都没发）；boot 红条堆栈只进 console、正文一行
  加「知道了」关闭钮。

### Installer

- `client2api-setup-0.5.59.exe`（空包，不含账号凭据或本机配置）
  SHA256: `A422579E2EA6E202821E83ADBF7565BAF43B0C4D950C08B9A2118A0153EEC016`

## [0.5.58] - 2026-11-02

### Changed

- **预扣型 403 的重试退避由厂商自己报的余额驱动。** #77 让 "Not enough quota
  to pre-consume" 403 正确进入 quota 冷却（30 分钟）后，没人充值的夜里每 30
  分钟仍要白烧一次 12-18s 的 403 往返来「确认 vendor 还是没钱」——而每条拒绝
  消息里都带着精确余额（¥X remaining）。现在 quota 停车时解析该金额并按账号
  记账（preconsumeWallet，source.go）：连续多次拒绝金额不变（没人充值）→ 冷却
  逐次翻倍（30m→1h→2h，封顶 2 小时），金额一变（充值到账）或请求成功立即复位
  退避，下一轮周期照常快速重查。对余额扫描可用的平台无影响——本优化只整形
  403 自身造成的停车；「充值后自动恢复」的探测语义完整保留。

### Installer

- `client2api-setup-0.5.58.exe`（空包，不含账号凭据或本机配置）
  SHA256: `479668F65BE94906B6AD4C32AB3EABC3249EC5B7A8B6DA2E54E5F7EC762AAA03`

## [0.5.57] - 2026-11-02

### Fixed

- **平台页「其它键」写错类型在保存当场被拒，不再重启后静默下线整个模块。**
  validateConfig 对 clients 只做形状检查，而模块在启动时把自己的配置块强类型
  反序列化——`{"assume_logged_in":"true"}`（手写 JSON 把 bool 写成字符串）能
  一路 200 保存、200 热加载，重启后整个模块 FAILED to load 被跳过，而面板每个
  页面照常渲染该平台（卡片列表取自 registered），chat 请求 404 才暴露。现在
  `validateClientBlocks`（configwrite.go）按面板自有的模块字段目录（moduleOptions，
  正是「其它键」旁边表单的同一份 schema）校验已知键的类型：bool 键必须是布尔、
  number 键必须是数字——错类型在保存当场报错并点名键与期望类型；schema 外的键
  是模块自有领域，照旧放行。kimi 的目录补上 `assume_logged_in`（此前只在其它键
  里可写）。

### Installer

- `client2api-setup-0.5.57.exe`（空包，不含账号凭据或本机配置）
  SHA256: `B93B2C59FB8BF49D8A7ACCD415EAECA53C4407CEAC940F508FC7E6FDEF8E6787`

## [0.5.56] - 2026-11-02

### Fixed

- **对话记录整段回传不再撞上面板 1 MiB 请求上限。** 对话测试页每轮都把整段
  对话 PATCH 回服务端，长会话跨过 1 MiB 后 decodeBody 的 LimitReader 静默
  截断，json.Unmarshal 报「unexpected end of JSON input」→ 400；前端又以
  quiet 吞掉错误、settle 随即重读服务端那份「还没有这一轮」的记录——刚花的
  钱、刚出现的回答一起从屏幕上消失，且无任何解释。三层修复：上限提到 16 MiB
  且读满一位以区分「真超限」与「合法满载」；超限回 **413** 与明确的
  「request body exceeds the panel limit (16 MiB)」而不是假 JSON 语法错误；
  前端保存失败必弹 toast（含 413 专文案），且保存失败的那轮**不重读**——
  内存态保留，操作员至少能看见并复制回答。
- **平台配置页「分时规则 N 条」只数有内容的规则。** 无规则时渲染层故意塞一行
  空占位，徽标按 DOM 行数计数恒显「1 条」，与同屏「当前未命中分时规则」互相
  打脸；点一次「新增规则」空行也跟着变成「2 条」。现在空行（start/end/prio
  全空）与格式无效行（保存会被拒）都不计数，0 条显示「未配置」。

### Installer

- `client2api-setup-0.5.56.exe`（空包，不含账号凭据或本机配置）
  SHA256: `1144D38B5565F9E924A87145D5F3C33D6888EE390C89367B3C2C70CC97C7B2EA`

## [0.5.55] - 2026-11-02

### Fixed

- **预扣型厂商的「Not enough quota to pre-consume」403 归入余额桶。** 这类厂商按
  请求预估费用做预扣：余额见底时同一账号小请求 200、大请求 403，而
  creditMarkers 的逐字面匹配不认识这句原话（「insufficient quota」≠「not enough
  quota」），403 落 kindForbidden → FailureModelRefused——¥0 账号保持 ready，
  每个大请求白烧 12-18s TTFB 才改道。creditMarkers 新增 `not enough quota` 与
  `to pre-consume`：403 带该短语 → FailureQuota 按 quota 冷却停车，余额刷新见
  钱自动放出；真 403（权限拒绝）保持原判。
- **对话页流式回复不再让读屏逐片重播整段历史。** #chatLog 整块是 aria-live
  容器而流式循环每片整段重写 innerHTML——41 个分片实测 44 次播报、每次都是
  全量历史。chatDraw 新增 streaming 参数：流式中间态把 aria-live 摘到 off
  （读屏闭嘴），settle 的最终绘制恢复 polite，完成的回复作为一次宣告落地；
  harness 测试钉住两处调用点，缺了恢复分支会比原来更糟（永不播报）。

### Installer

- `client2api-setup-0.5.55.exe`（空包，不含账号凭据或本机配置）
  SHA256: `D3E8F4B6E21E62A8003317DA60B820C6962CE3FEB0F52FC26AC0B5663591611E`

## [0.5.54] - 2026-11-02

### Fixed

- **发布完整性：安装包的 SHA256 从「手抄」变成「断言」。** v0.5.49 的 Release
  说明公布了一个与附件不符的哈希（正文在 amend 重写前生成、附件换新后正文未再
  生成），任何人按项目自定的流程校验都会得到「校验失败」。三层收紧：
  `installer/docs_version_test.go` 新增断言——dist/ 里有对应版本安装包时，
  CHANGELOG Installer 段的 64 位哈希必须等于该文件实际摘要（干净 clone 无产物则
  跳过；CI 的 windows-test 必有产物，永远覆盖）；「改包不改文档」从此在
  `go test ./installer/` 就红。v0.5.49 的 Release 正文已用正确哈希
  （105786D6…）修正。

### Installer

- `client2api-setup-0.5.54.exe`（空包，不含账号凭据或本机配置）
  SHA256: `F0D55030B24FABAB731B1D3487C13F140F95846F5247EAE9D624C1C92DAF49AA`

## [0.5.53] - 2026-11-02

### Fixed

- **#67 的「平台在途」在面板上真的可见了。** 服务端两个端点都在发
  `platform_in_flight`、渲染端也在读，但中间的 `poolIndex` 映射白名单只有
  pool/health/extra 三个键——字段在这里被丢，渲染条件永远不成立，0.5.52 的
  平台并发读数静默不可见。现在 poolIndex 透传该字段；harness 测试钉住「服务端
  client 级字段 ↔ poolIndex 白名单」同步，防止下一个字段再断一次。
- **中文厂商报错不再被切出半个字。** clients/ 下 16 份同名 truncate 有 10 份按
  字节切（`s[:n]`），切点落在多字节字符中间产出非法 UTF-8，经 JSON 序列化在
  面板显示成「积分余�...」。10 处（qoder/loomy/trae/minimaxcode/openaicompat/
  zcode/doubao/autoclaw/raccoon/workbuddy）统一改为 rune 切，与另 6 份既有实现
  同口径；新测试钉住「任意长度中文截断后 utf8.Valid 恒真」。

### Installer

- `client2api-setup-0.5.53.exe`（空包，不含账号凭据或本机配置）
  SHA256: `12A9B0A9FC074F13C51D7F46E33B996F03B2CC8FF25239A9F2C9D9B0902672D4`

## [0.5.52] - 2026-11-02

### Added

- **账号池头部新增平台总并发读数（issue #67）。** `/panel/api/status` 与 pool
  轻端点的 client 级载荷新增 `platform_in_flight`（网关此刻对该平台承载的请求数，
  数据源 `InFlightTrackers.PlatformInFlight`）与 `ceiling`（操作员设的平台上限，
  `Registry.MaxInFlightFor`）；账号池头部在账号数旁显示「平台在途 N / 上限 X」，
  未配置上限读作「不限」而不是 0——0 会被读成平台被锁死，正是这单要治的「看起来
  都是无限/都是零」。pool 轻端点同步携带，账号表 5 秒轮询不用第二次请求。

### Installer

- `client2api-setup-0.5.52.exe`（空包，不含账号凭据或本机配置）
  SHA256: `780E7EFF3D5D665BB70085E830F5D02B75CA75AF4C5350225E5C02A6B2405549`

## [0.5.51] - 2026-11-02

### Fixed

- **调用方取消不再丢失在 openaicompat 的错误信封里。** 取消发生在 HTTP 往返中、
  以 `*url.Error{Err: context.Canceled}` 冒出时，`classifyUpstream` 识别了它却把
  身份切断：映射 kindNone → FailureOther，再用不保留 Unwrap 链的 `upstreamError`
  重新包装——网关 `failureReason` 的 `errors.Is(context.Canceled)` 探不到，同一
  次取消标签取决于哪层先观测到（「调用方已取消」/「上游拒绝」随机，recent.json
  session 实证同一会话三种标签两种）。现在 Canceled/DeadlineExceeded 原样上抛：
  标签恢复正确，路由循环也把裸 context.Canceled 读成「非平台失败」而不是终态
  FailureOther；DeadlineExceeded 同链路恢复「请求超时」语义。真实传输故障的分类
  信封不受影响。

### Installer

- `client2api-setup-0.5.51.exe`（空包，不含账号凭据或本机配置）
  SHA256: `5DC77C1AF43CF91C3BFABB02AA6BF35F2186387A71697DF4AC2900F4E07CC44F`

## [0.5.50] - 2026-11-02

### Fixed

- **openaicompat 系的「操作员停用」不再读成「账号异常」。** 两个半区（pool 的
  `stateAt` 与 source 的 Status/Accounts 投影）都把 disabled 折叠成 invalid，
  停用账号被面板的「账号异常」桶收走（提示重登——永远不会改变这行），「已停用」
  KPI 恒为 0。现在停用行报 `disabled`（workbuddy 同款字面量，面板桶现成）并带
  一行「操作员停用；重新启用即可」的 note；vendor 拒收置 invalid 的行把行内
  last_error 的原因带进 note（「key rejected: …」），裸「账号异常」不再让操作员
  翻字段猜原因。
- **minimaxcode 失效凭据不再吃必然失败的 oauth 刷新。** #48/#52 的两道 cmd 层闸
  拦住了「调度器会不会问」，但模块内 `renewable()` 不看账号状态——ensureFresh 的
  四条调用路径（chat/tasks/余额/签到）任何一条摸到 vendor 已吊销的账号都会白打
  一次 token 交换（每账号每天 ~288 次）。`renewable()` 现在对 state=invalid 直接
  返回 false；操作员重新登录后 markNeedsLogin 的反向路径自然恢复续期。

### Added

- **中继平台可配单次对话超时（`chat_timeout`），且一个预算跨「等响应头 + 等首帧」
  两段。** 挂在慢中继后面的平台可以把这个旋钮调松、快中继调紧：一次接受连接后
  挂死的上游会把平台在途槽占满整个模块默认超时，几个卡住的请求就把平台读成
  「打满」，后续请求全被跳过。空值走模块默认；配置校验要求正时长（0 与负数被拒
  ——0 意味着拿着在途槽无限等，正是这个旋钮要防的事）。流侧复用 doChat 武装的
  同一个定时器，首帧预算不再叠出第二个完整窗口。

### Installer

- `client2api-setup-0.5.50.exe`（空包，不含账号凭据或本机配置）
  SHA256: `F48DE078AB5FB6490D3FAC72E59FF903253E98CC2FED736916337EDFB15370A7`

## [0.5.49] - 2026-11-02

### Fixed

- **余额软扫描不再敲 invalid 账号的门。** #48 修掉了 renew 扫描对 invalid 的裸敲门，
  但 balance sweep 的状态门 `balanceProbeEligible`（main.go）只挡 recoverable
  （cooling/exhausted/low_credit 等），invalid 恰好漏网——每 5 分钟两次注定失败的
  上游请求：模块内部 `ensureFresh` 的 oauth 换新吃一个 invalid_grant，再拿死 token
  查余额吃一个 401。现在软扫描对 invalid 也返回 false（recovery sweep 本来就经由
  ready/unknown/recoverable 白名单天然排除它）；操作员重新登录后账号回到 ready，
  扫描自然恢复探测。
- **「已确认」的活动弹窗不再被当成拒绝。** 确认弹窗的 answer 1003（今天的第二次
  扫描必然看到）与「已领取」一样是稳态：操作员的客户端已确认过活动、套餐本就可领，
  下一步的 claim 才是读厂商裁决的那一跳。`claimViaMarketing` 此前把确认 1003 当
  refusal 跳过，扫描在确认已发生时反而报「无可领取」。现在确认 1003 仍把该套餐放进
  claim 队列，由 claim 的 1003/成功读出结果；其他 refusal 照旧跳过。
- **「减少动效」管住了伪元素的过渡。** reduce 媒体查询的全局规则选择器只有 `*`，
  而 ::before/::after 不继承宿主的 transition——冷启动 reduce 下全站 20+ 条伪元素
  过渡（导航频道亮口、排序列头箭头、折叠指示等，最长 0.5s）照旧插值。选择器补全为
  `*, *::before, *::after`（animation 半边同理由补全）；a11y UI 测试钉住新选择器、
  禁止退回旧写法，也钉住另一条 reduce 规则的伪元素不许丢。

### Installer

- `client2api-setup-0.5.49.exe`（空包，不含账号凭据或本机配置）
  SHA256: `105786D672A662699941F3D967C390BF1C9E6BF239407DEBE9AC749618058B8E`

## [0.5.48] - 2026-11-01

### Installer

- `client2api-setup-0.5.48.exe`（空包，不含账号凭据或本机配置）
  SHA256: `D6A49826854C64C1F8A75A201F228E58FE6EE7BEB81293900421BBB8F2AFE05A`

## [Unreleased]

### Added

- **Trae 一键自动添加账号。** 模块新增 SMS 接码（`core.SMSProvider`，eomsg，配置
  `clients.trae.sms` 或面板粘贴 token）与 `AutoLoginProvider`：租号 → 无头浏览器
  打开 CN console 的 `/authorization` 页 → 探测手机号登录入口并填号 → 收码填码
  提交 → 保持页面等待跳转本地 `/authorize` 回调 → 既有 web OAuth 兑换链落库。
  登录页是厂商页面，选择器走多候选探测（已知 id 优先、文本匹配兜底），每一步
  日志会写明实际命中的选择器；首跑若 DOM 对不上，报错会指出要调哪个选择器。
- **四平台一键登录自动避开账号池已有的手机号。** officeace / codearts / raccoon /
  trae 的一键自动添加，取号时把池内已有账号的手机号并入 `AcquirePhone` 的
  avoid 列表，接码平台不会再把已有账号占用的号码发回来；登录成功落库时把本次
  租到（或重用）的号码写进账号持久化结构，之后的避让来自与库同源的数据。照
  `req.Phone` 钉号的重登/恢复路径不走避让，行为不变。
- **CodeArts 一键自动添加账号。** 模块实现 `AutoLoginProvider`：租号 → 无头浏览器
  打开华为登录页 → 填手机号、收码提交 → 自动确认首登用户协议 → 保持页面等待跳转
  本地回调 → 既有 OAuth 兑换链落库。面板「自动添加」按钮自动亮起，全程有日志。
- **小浣熊（raccoon）一键自动添加账号回归。** 2026-10-21 移除的是「绕过浏览器手造
  captcha token」的旧路（厂商加固后必死）；现在改为驱动模块自己的回环登录页——
  页面内的阿里云无痕验证在真实 Chromium 里自然放行，和手动操作一致：租号 → 无头
  浏览器打开回环页 → 切短信登录 → 填手机号点「获取验证码」（页面 JS 自己过无痕
  验证）→ 收码填码点登录 → 凭据落库。发送结果与登录结果都从页面状态行取真值，
  厂商拒绝时原话透传。
- **ZCode 活动套餐接入营销弹窗通道（WEEKEND BUILD 自动领取）。** 厂商的周末 5 亿
  档（`ZCODE WEEKEND BUILD`）不走 billing 预览，走营销投递：客户端收到
  `marketing/touch` 投递、用户点「领取」（`marketing/touch/action` confirm）之后
  套餐才有资格被 claim。现在 checkin 的空预览分支会自动走同一条链——查投递、
  确认带 `claim_zcode_plan` 动作的活动、随后 claim——不需要客户端也不需要人点。
  不带计划动作的活动（普通广告）一律不确认，代答只限套餐领取。营销端点实测
  会被代理挂起，模块新增一条无代理直连通道（TLS 指纹不变）专供它走。

### Fixed

- **officeace 的「input token limit is 1048576」上下文超限 400 不再是终态。** 同一个
  物理原因（会话输入超出模型窗口），hcnsec 轮转、opencode 触发压缩，officeace 的措辞
  却让整条请求链断掉：`context_guard.go` 的上下文拒绝识别器不认识这句裸陈述句型（五个
  tooLongPatterns 都要求 `>` / `exceed` / `maximum context length` 之类的比较形态），
  于是 openaicompat 落 kindClient → FailureOther 终态，请求死在第一个命中该措辞的平台，
  带着精确窗口数字的 400 也学不进去。现在 tooLongPatterns 新增 `input token limit is N`
  句型：400 被归为 FailureContextWindow（`context_guard.go` 的 tooLongPatterns 末条），
  复用既有链路——轮转下一平台 + `learnContextWindowFromFailure` 把 1048576 学为窗口 +
  带测量的同平台裁剪重试一次；真实请求形态的 400（参数错误等）不受影响，负向测试钉住。

- **officeace 0 积分账号读到 0 即停车，充值后读到非 0 即复活。** 余额软扫描早就读到了
  0（balance_cache 新鲜），但 `noteOfficeAcePoints` 只在 supersede 死 key 时才停车——
  健康的 0 积分账号要等下一次 chat 撞上 drained package 的 auth 形拒绝才停 5 分钟，
  5 分钟一到又放出又撞墙，面板在「冷却中 ↔ 可用」间振荡，用户看到的 400 就是这个循环。
  现在 `officeace.go` 的 `noteOfficeAcePoints` 读到 0 无条件 `parkLocked(FailureQuota)`
  （对 stale invalid 标记同样覆盖，读数优先于标记），读到正数立即清冷却（充值/签到即
  复活，不等 5 分钟冷却自然到期）；软扫描每几分钟就有一份新鲜读数，面板状态从此与
  余额一致。`officeace_premium_test.go` 三个新测试钉住读时停车、正数复活、0 读数压过
  stale invalid。

- **华为自动添加账号在新账号上必卡死。** 提交验证码后弹出的华为云用户协议其实
  已经成功点掉（「已同意华为云用户协议」这行日志只在弹窗真正关闭后才打），但
  授权码要靠页面跳到网关的回调地址才能到达云端——浏览器却在这一步立刻被关闭，
  新账号（有协议弹窗、零缓冲）跳转永远跑不完，90 秒轮询空转后报「华为授权没有
  在限定时间内完成」。老账号没事只是巧合：没有弹窗时代码空等 12 秒扫弹窗，恰好
  给跳转留了窗口。现在提交验证码、确认协议之后页面保持打开，轮询 URL 最多 45 秒
  等跳转落到 `/v1/claw/auth/callback`，期间再次弹出的协议框会自动重新确认；
  每一步都有日志，跳转没发生会明确报「华为没有跳转到授权回调」而不是干等。
- **已持有套餐认不出来。** 2026-10-30 起 `billing/current` 对明确持有套餐的账号
  也返回空信封（桌面客户端自己读的是 `billing/balance`），旧代码把持有了 5 亿的
  账号判成「无可领」、一直停在 exhausted。`heldPlanWindow` 改为以 balance 为准、
  current 只作旧形态回退；空信封下拿到 balance 的 active plan 即判定成功并解停。
- **营销链领取后 1003 不再当失败。** 确认后 claim 回 `already claimed` 与预览
  通道同义：活动已入账，判成功并解停，而不是留给下个窗口空转。
- **CodeArts 刷新码到期的账号被无限续期。** 厂商的 STS 端点用 `STS5.1806`
  （`invalid refresh token: 'refresh code has expired'`）报告刷新码已失效，而
  终态判定只认参考实现那两条（`invalid_grant` 和 `ExpiredRefreshToken`），
  两个都不匹配——于是账号不会被标 `invalid`，五小时里对着一个永远续不动的
  凭据敲了 34 次（实测日志 16:24–19:21）。现在 `STS5.1806` 也判终态：账号标
  `invalid` 并提示重新登录，扫描不再敲门。实测刷新令牌族寿命**固定 30 天**
  （轮换不延长，`exp` 始终等于 `created_at + 30d`），到期只能重登。
  另修一条同源竞态：刷新令牌每次交换都轮换（`iat` 前进、族到期时间不动），而网关
  允许每账号 2 个并发请求，两条续期路径可能同时对同一个在轮换的令牌发起交换。刷新
  现在按账号串行（`pool.refreshLock`），等待者复用赢家的新凭据而不是再换一次；顺带
  修掉一处未加锁读 `entry` 字段的竞态（`-race` 实测暴露）。

### Changed

- **ZCode 领取窗口扩为每晚 19:05–22:55 反复询问。** 排班从 `5-55/10 19-20 * * *`
  扩为 `5-55/10 19-22 * * *`（每 10 分钟一次共 24 档，23 点停）——普通 start-plan
  19 点开窗，WEEKEND 档实测 22 点后才投递，两条线都得盖住。调度器读每日签到
  状态：`done` 跳过不打扰厂商，`unavailable` 继续问；没接状态存储的部署不变。

## [0.5.46] - 2026-11-01

### Fixed

- **续期扫描不再对 invalid 账号每 5 分钟裸敲门。** `renewableAccount` 只看
  enabled/refreshable/expires_at，一条 refresh token 已被上游作废
  （`invalid_grant`）的 invalid 账号照样每拍被续期一次——75 分钟 13 条注定
  失败的 oauth 往返，且失败行裸打不走去重（#7 修了两条路径，这是第三条
  漏网的）。现在 `state=invalid` 跳过续期（续期救不了需要重新授权的凭据，
  ready/unknown 与可恢复停靠态照旧续期）；两处失败行接共享去重器，同文
  故障一小时一条并带抑制计数。
- **kimi 的 cli-login 行不再对过期凭据报 ready。** 面板路径的
  `cliLoginEntry` 只看凭据文件存在，诊断路径的 `file:*` 行却判过期——
  同一时刻同一模块两个结论，面板显示可用而所有请求都在死。现在
  `cli-login` 读文件声明的过期时间：已过期改报 invalid 并带「CLI 下次
  prompt 自行刷新」备注，两条路径对同一份凭据同一个结论。

### Installer

- `client2api-setup-0.5.46.exe`（空包，不含账号凭据或本机配置）
- SHA256：`76D7E003CCC9CA3F69B528B6968FABEED52181157ACF4EEE83B26957672450C4`

## [0.5.45] - 2026-11-01

### Fixed

- **账号列两行都按「截断即给 title」回填。** 表格里 `td:nth-child(2) .hint`
  的 title 回填漏掉了 `​.name`——它才是最容易被 150px 截断的那行（备注上限
  120 字），截断后无 tooltip、无 aria、无法复制，多账号时一行行
  「13800138000 阿达的个人…」无法区分；卡片视图同字段是有 title 的。现在
  `renderAccounts` 的后置遍历两个选择器都给。
- **筛选条选中态的白字回来了。** `.chips > .chip.has`（权重 0,2,1）后置覆盖
  了 `.chips .chip.on`（0,2,0）的 `color: var(--accent-ink)`，选中档文字被
  改回次级灰——实测浅色 1.06~1.79:1、深色 2.39~2.55:1，两套主题都不达 AA，
  而选中项是全页唯一需要一眼找到的那颗。限定为 `.chip.has:not(.on)` 后
  覆盖只发生在未选中档，实测选中态标签与计数都回到 `rgb(255,255,255)`
  （白字对蓝底 4.86~5.32:1，达标）。

### Installer

- `client2api-setup-0.5.45.exe`（空包，不含账号凭据或本机配置）
- SHA256：`BB88E2C53B5D451641E253F52C4A1733E45CF907CD92D3B390BFDBF41FB765F6`

## [0.5.44] - 2026-11-01

### Fixed

- **账号表列定义与单元格数同进同退。** 无余额能力的客户端（kilocode/kimi/
  opencode/doubaoime/openai-compat 等 5 个）行内按 `showBal` 少渲染一个
  余额 `<td>`，但 `<colgroup>` 的 `<col class="col-bal">` 静态写死——
  `table-layout:fixed` 下声明列仍占位，表右端空出 `col-acts` 宽度的死白带，
  操作列被挤到 117px、5 个控件叠成 4 行（行高 137px，同账号数需要 2.7 倍
  垂直空间），「测试模型」下拉窄到读不出模型名。现在 `body.hide-bal` 同时
  收起该列定义，12 个单元格铺满 12 列，`col-acts` 回到声明宽度；并补了
  「col 定义跟着单元格走」的回归断言（原测试只数源码字符串，列错位照样
  13=13）。

### Installer

- `client2api-setup-0.5.44.exe`（空包，不含账号凭据或本机配置）
- SHA256：`F35CD1F03771A5F1A82D0C00448F79C03720115DA27842FAA6A638C06A682A79`

## [0.5.43] - 2026-11-01

### Fixed

- **流式回复期间离开对话页/点刷新不再吞掉整轮对话。** 重读服务端记录会整体
  替换对话消息，而生成中的回合还没落盘——切走再切回、点一下顶栏刷新，
  正在生成的回答、刚发的用户消息、会话 chip 全部消失，最后落盘的是
  `messages: []` 的空记录（上游额度已付，什么都找不回来）。现在
  `CHAT.busy` 期间 `chatLoadThreads` 只刷新列表、保留屏幕上的内存态；
  顶栏刷新在对话页流式期间只刷模型目录；settle 落盘后的重读才是真相。
- **流式期间下拉框锁定 + 落盘身份定格。** 这一轮真正使用的 client/account
  在发送时刻定格，不再读 settle 时的下拉框值（流式期间切平台会写出
  client 与 model 自相矛盾、重开时模型选择丢失的记录）；三个选择控件在
  busy 期间禁用。

### Changed

- **顶栏「刷新」有了在途互斥与按钮反馈。** 原来无互斥无反馈，连点即连轮
  全站重读（实测一次点击 28 请求/152KB，连点 3 下 72 请求/294KB），且
  「点了没反应」正是人再点一次的原因。现在上一轮没回来就不再接下一轮，
  按钮期间置灰并显示「刷新中…」，失败也会恢复，不会卡死。

### Installer

- `client2api-setup-0.5.43.exe`（空包，不含账号凭据或本机配置）
- SHA256：`723D4D2DC2366FE41E4FC4BD185321C1BB7796F8183B1C0EED1C9E6BE7739444`

## [0.5.42] - 2026-11-01

### Fixed

- **标准 OpenAI 参数不再被静默丢弃。** 入站白名单把 `response_format / n /
  seed / 惩罚项` 这类标准写法整个丢掉——不转发、不报错、不留痕，然后回
  200：结构化输出整条路是断的（客户端拿到散文，JSON 解析失败，操作员只会
  算到"模型不听话"），`n > 1` 违反响应契约只回一条。现在：
  模块已有参数通道的 5 个键（`presence_penalty / frequency_penalty / top_k /
  seed / parallel_tool_calls`）写标准顶层位置会折叠进模块参数（`client2api:{}`
  显式写法优先）；`tools[].function.strict` 全链透传到 openai-compat 上游；
  `n > 1` 回 400 并说明原因；其余未建模参数丢弃时日志记一行
  `dropped unsupported params`——同源网关里"静默丢弃 + 200"本该是显式选择。

### Installer

- `client2api-setup-0.5.42.exe`（空包，不含账号凭据或本机配置）
- SHA256：`5CA4C19680753207E6A4DD4600EDE77C9973BB414F88325AF582D8B8799B201C`

## [0.5.41] - 2026-11-01

### Fixed

- **对话测试页的 3D 舞台不再挂假警示卡。** 舞台的模型档计数读的是
  `MODELS` 目录缓存（只有进过模型页才有值），而页面下拉读的是 status
  同源的 `clients[].models`——于是下拉 39 项可选、舞台却挂着「还没有
  模型目录 / 先刷新模型」，且它指的「右上角刷新」刷的是状态、根本不会
  填充目录。现在 3D 卡与下拉同源（status 的 `clients[].models`）：真有
  客户端上报模型就显示每家的档数，空态只在真的没有客户端在线时出现，
  指向「注册客户端」这个正确的下一步。

### Installer

- `client2api-setup-0.5.41.exe`（空包，不含账号凭据或本机配置）
- SHA256：`97D31F3E2AD2CCDAF1ABEC41244E2FAD84B46458E5E7395D87D40E9C87251A23`

## [0.5.40] - 2026-11-01

### Fixed

- **中文输入法组合期间按回车不再被当成提交。** 面板 8 个「回车即执行」的
  入口只判 `Enter` 键，从不看输入法组合态——账号备注存成拼音 `beizhu`、
  对话把半截拼音当消息发出去还清掉草稿。新增 `onEnter` 工具统一先拦
  组合态（`isComposing` + `keyCode 229` 兜底 Safari）再判键，8 个入口
  全部换用；只拦这一个键，不拦输入本身。
- **焦点环从 1.5:1 提到过 3:1。** 后置的通用规则曾用 `outline: none` +
  30% 半透明光晕把 2613 行那套 55% 的 focus-visible 样式整个压掉——实测
  深色 1.49:1、浅色 1.46:1，键盘用户只能盯着一圈淡晕找焦点。现在改成
  双层：1px 实心 accent 外描边负责对比度，光晕只管美观。

### Added

- **导航语义补齐（a11y）。** 主导航 13 项原来读屏听不出当前在哪一页：
  切换视图时给当前项写 `aria-current="page"`（`syncActive` 是选中态的
  唯一入口，一行同步不会漏）；内容区加 `role="main"` 地标；页面第一个
  可聚焦元素是「跳到主内容」的跳过链接——按一下 Tab 即可越过 13 项导航。

### Installer

- `client2api-setup-0.5.40.exe`（空包，不含账号凭据或本机配置）
- SHA256：`E4ABD893BC67D29F7FEEC76F552C657DB874E184385F2E845946E19913B17876`

## [0.5.39] - 2026-11-01

### Fixed

- **面板持久化 store 的磁盘写失败不再无声。** 四个 store（对话历史、签到去重
  状态、余额缓存、任务看板缓存）原来把 `core.WriteFileAtomic` 的错误整行
  丢弃——目录只读、磁盘满、杀软锁文件时页面照常显示，重启后数据无声消失，
  日志里一个字都没有。现在统一接 `storeErrTracker`：失败记进
  `store_errors` 随 `/panel/api/overview` 下发、当场打进面板日志环；
  抄的就是 alerts.Store 已有的模式。

### Added

- **CI 加 windows-native 门禁。** 原来四个 job 全在 ubuntu 上跑，
  windows-only 的 4617 行代码（含用户双击的 installer/setup 2964 行）和
  31 个 Windows 测试从不编译、从不执行——一次语法错可以一路绿灯直达 tag。
  新增 `windows-test` job（windows-latest：gofmt/vet/test 全量，
  windows 下自动带上全部 windows-only 套件），发布构建改为依赖
  `[test, windows-test]`。

### Changed

- **三个模块的后台 goroutine 补上 panic 兜底。** doubao/doubaoime/zcode
  是 22 个模块里仅有的三个 0 处 `core.GoSafe` 的包，裸 goroutine 崩了会
  带走整个网关。现在 doubao 的 `refreshModelsAsync`、zcode 的验证码
  预取（请求路径上开子进程驱动浏览器）、验证码页面服务器、浏览器进程
  等待、窗口守护全部换成 `core.GoSafe`；zcode 预取带 `deps.Log` 上报，
  其余走 stderr 兜底。

### Installer

- `client2api-setup-0.5.39.exe`（空包，不含账号凭据或本机配置）
- SHA256：`C5E53D723E9E1EE01FF315F5C29A55E84A7823C4411644C6428B32E2E9963CE3`

## [0.5.38] - 2026-11-01

### Changed

- **面板 5 秒轮询不再每拍拖一整份客户端元数据。** 实测一拍两个请求
  （overview + status）约 101KB，其中 capabilities 一份 36KB 字节级重复、
  每拍真正变化的字段只有 6.4KB——22 个模块静置一小时约 90MB 下行，纯流量
  与 JSON 解析的浪费。现在轮询打新增的 `/panel/api/pool`：只带 pool、
  health、按账号的在途租约和 redis_mode（实测一拍 655B，-99%）；capabilities
  /models 这些静态块只随 overview 走。`/panel/api/status` 本体一字未动，
  诊断脚本和兜底路径照旧。
- **模型目录接上条件请求与缓存。** `/panel/api/models` 是面板最重的
  payload（284KB），之前模型页每次进场都整份重拉，还绕过了现成的
  `MODEL_CATALOG_TTL_MS`。现在 GET 带 ETag（对 clients 切片哈希，
  refreshed_at 每秒在动不参与），客户端缓存上次响应、重进页面走 TTL、
  过期了带 If-None-Match——304 复用缓存，实测重进一页 607B（-99.8%）。

### Installer

- `client2api-setup-0.5.38.exe`（空包，不含账号凭据或本机配置）
- SHA256：`722061B3AF36C204B0AC8FF7D0D44D1EDAA42071D2DB3BD4A245C800BC3CFC99`

## [0.5.37] - 2026-11-01

### Fixed

- **发版重启不再吞掉当天的签到批。** 安装器重启落在批中段时，剩余账号当天的
  窗口直接丢失——`plan()` 只看严格晚于当前时刻的窗位，被截断的窗口永远不会
  重现（01:00 的 4 平台窗只跑完 2 个，loomy 整批消失，当天只能等下一个窗）。
  现在调度器启动时把「当天已过窗位 × 排班」和持久化的运行期刊
  （`schedule_runs.json`）对一遍：没有 schedule 记录覆盖的窗位按最老优先
  错峰补跑一轮——被截断的批到不了期刊，天然判为漏跑；补跑内账号级防重复
  领取照常生效，已完成的账号直接跳过。窗口超过 6 小时不补（长时间停机
  不搞启动风暴），人工触发的运行不算窗口覆盖。cron 窗位（zcode 的
  `5-55/10 19-22`）同样参与对账；其相邻档位本身 ≤10 分钟，截断损失自愈。

### Installer

- `client2api-setup-0.5.37.exe`（空包，不含账号凭据或本机配置）
- SHA256：`0C191355A56364DE12B5239B8E19148532EE9EB714B30DAA02E106431B12E5C5`

## [0.5.36] - 2026-11-01

### Fixed

- **「减少动效」现在管得住 WebGL。** 星图与 3D 舞台的 CSS 半边早就停了，两块
  three.js 场景却照转（星图 yaw 每帧 +0.0011、相机绕场、铭牌漂浮；3D 旋转件、
  悬浮、星空环境层全在跑）——开了偏好的用户以为已关掉，最强的一个动效源还在。
  现在 reduce 下时间冻结：自转/绕场/漂浮/星空全部停住，入场展开一步落位；
  拖拽、缩放、悬停高亮保留（那是即时反馈不是环境动效）。
- **运行中改系统设置也跟上。** `prefers-reduced-motion` 的 JS 侧原来只在启动
  读一次，CSS 立刻静止、聚光灯/涟漪/数字弹簧却动到下次刷新。加了
  `matchMedia change` 监听，切换瞬间 WebGL 场景丢掉时间基线、下一帧按冻结
  时间构图。

### Added

- **弹层有了退场。** 12 个弹层进场编排 400ms，关闭却是「啪」一帧硬切——整屏
  毛玻璃在一帧内消失，夜里尤其刺眼。现在 `hideVeil` 先摘 `.on` 换 `.out`
  播 150ms 淡出（比进场短，遮罩与弹窗一起退），`animationend` 后才置
  hidden；reduce 下全局 `animation:none` 让它退化为瞬时关闭。8 条关闭路径
  （含 Esc 的 `closeTopVeil`）都收敛到 `hideVeil`，一处修全部生效。
- **弹层补齐对话框语义与焦点管理。** 12 个弹层只有 3 个写了
  `role="dialog"`；打开不移动焦点、Tab 能跑进被遮住的背景页。现在 9 个缺失
  的弹层全部补上 `role="dialog" aria-modal="true" aria-labelledby`（标题
  `<h3>` 相应加 id），`showVeil` 统一：打开记录原焦点、移入弹层第一个可聚焦
  元素；document 级焦点陷阱把 Tab 锁在最上层弹层内首尾互绕；关闭还原焦点。
  「添加账号」「需要访问密钥」这两个最高频入口终于对读屏/键盘用户可用。

### Installer

- `client2api-setup-0.5.36.exe`（空包，不含账号凭据或本机配置）
- SHA256：`EB4BA160084AA46491B9D483717D6947EEA09E89CC2FF9DC52EAA0B5D7D218A1`

## [0.5.35] - 2026-11-01

### Fixed

- **积分构成页不再把「无到期时间」的余额画成告警红（[#28](https://github.com/Cvencent/client2api/issues/28)）。**
  `pkUrgency` 的第一行 `Number(days)` 把 `null` 变成 0——`isFinite(0)` 为真、
  `0 <= 3` 命中红档，于是没有到期时间（= 永久有效）的批次整条画成
  `--bad`，与同段 tooltip 的「无到期时间」自相矛盾，也和到期分布图
  「不猜到期日、整段跳过」的口径相反。现在 `null`/`undefined` 显式走灰色
  later 档；顺带把「已到期」（`d <= 0`）从「3 天内」拆成单独的 expired 档。
- **模型页页头读数在目录落地后重算（[#27](https://github.com/Cvencent/client2api/issues/27)）。**
  `mastStats` 直接读全局 `MODELS`，而页头只在进页那一刻同步算一次——冷启动
  进页永远停在「模型 0 / 0 个客户端」，上游刷新后停在旧目录，与同屏面板头
  和列表脚注对不上。现在 `renderModels` 在目录落地且当前视图就是 models 时
  重算一次页头，读数与面板取自同一时刻、同一份数据。

### Installer

- `client2api-setup-0.5.35.exe`（空包，不含账号凭据或本机配置）
- SHA256：`FD0626FF367502632383B3E8AD6DD4DAC801412300D2D698C0DE8E4079FB2807`

## [0.5.34] - 2026-11-01

### Fixed

- **用量页页头读数不再按客户端数放大（[#26](https://github.com/Cvencent/client2api/issues/26)）。**
  每个模组的 `GET /accounts` 都携带全池一份 `stats`（panel.go:938，overview.go
  的设计说明明确说不要这么做），前端 `acctStatTotals` 又逐模组累加——同一份
  全局统计被加 N 遍（N = 客户端数，实测 38 倍）。现在按账号 id 去重后求和；
  页头 Token 改用与磁贴同一套 B/M/k 分级的 `fmtTok`（此前误用上下文窗口的
  `fmtK`，2.09e12 吐出「2090302613K」）；副文案从「本进程累计」改为如实的
  「账号侧累计」。
- **卡片墙就地展开的检视块纳入轮询豁免（[#25](https://github.com/Cvencent/client2api/issues/25)）。**
  #23 的编辑豁免只查表格容器：卡片墙里同一个 `acctDetailHTML`（同一个优先级
  滑杆、同一批测试模型下拉）拖动中 5 秒轮询一到就被 `renderAcctBoard` 整片
  重建。豁免检测扩为表格 + 卡片墙两个容器，`renderAcctBoard` 同样吃豁免、
  跳过本轮（单轮生效，下一拍数据照常跟上）。

### Installer

- `client2api-setup-0.5.34.exe`（空包，不含账号凭据或本机配置）
- SHA256：`3E1E948287F5DF63253AF298AEABE0B49917D18C9801B2EB834AC897037A4DB1`

## [0.5.33] - 2026-10-31

### Fixed

- **`low_credit` 拆出「积分不足」独立状态档，不再混进「冷却中」（[#24](https://github.com/Cvencent/client2api/issues/24)）。**
  `accountState` 此前把 `low_credit` 与限流/熔断同归 cooling 桶：62% 账号积分
  耗尽的慢状态与 429 分钟级快冷却同计，「冷却中 23」读起来像「等会儿就好」，
  实际是「不充值/等签到就不会好」——两种状态的应对动作完全不同。现在
  `low_credit` 独立成档（label「积分不足」，中性黄色调），全链路消费点同步：
  账号池新增「积分不足」KPI 磁贴（可点筛选）与状态下拉选项；总览健康环/
  提示 pill/堆叠条把两类分开呈现；3D 舞台 tone 仍为 warn（凭据是好的）但
  说明文案改为「积分不足 · 等签到/余额回血，或充值」；账号池页头读数分开
  报「N 冷却中 · M 积分不足」。数据层不动。

### Installer

- `client2api-setup-0.5.33.exe`（空包，不含账号凭据或本机配置）
- SHA256：`3BA606D4100B6DF0386458A0BF238B0EC5EAD49BA2D48ABFF561E2FF568117C1`

## [0.5.32] - 2026-10-31

### Fixed

- **表格呈现下编辑中的账号行不再被 5 秒轮询打断（[#23](https://github.com/Cvencent/client2api/issues/23)）。**
  `renderAccounts` 此前无条件整片重建 tbody：编辑到一半的「优先级」值和焦点
  一起消失（且不补发保存，调整静默作废）、选好的「测试模型」被改回默认、选中
  的文本被清掉——3D 检视面板在 #6 已有豁免，表格没有。现在重建前判断
  `#accBody` 内是否有正在操作的控件（activeElement 落在表内，或 `.accPrio` /
  `.test-model` 处于焦点/按住），命中则本轮跳过 tbody 重建（页头读数照常），
  下一拍数据照常跟上。
- **「测试全部账号」对齐其它批量操作的交互（[#22](https://github.com/Cvencent/client2api/issues/22)）。**
  此前按钮全程不禁用（连点两下就是两轮全池上游探测）、无进度、全失败也只报
  「测试完成」。现在对齐同文件 `checkinClient()` 的模式：按钮禁用、文案
  「测试 N/M」逐个推进、收尾 toast 带成功/失败计数（有失败走 err 触发
  role=alert）。

### Installer

- `client2api-setup-0.5.32.exe`（空包，不含账号凭据或本机配置）
- SHA256：`0706F718F0CDD773A2BA9994F527B5C64526F0AC89FD6C9D3F026E03F7978599`

## [0.5.31] - 2026-10-31

### Added

- **面板静态资源 gzip 压缩 + 缓存协商（[#14](https://github.com/Cvencent/client2api/issues/14)）。**
  首屏 1.96 MB（shell 1.2 MB + three.min.js 576 KB + motion.min.js 61 KB）此前
  原样传输且 `no-store`——每次刷新完整重下。现在：`/panel/` 与 `/panel/vendors/`
  按 `Accept-Encoding` 协商 gzip（两个 vendor 库启动时预压一次，实测合计省
  ~74%）；vendors 是随构建固定的字节，改 `public, max-age=31536000, immutable`
  长缓存；shell 的 `no-store` 改 `no-cache` + 内容哈希 ETag——刷新时
  `If-None-Match` 命中即回无 body 的 304，页面永远不会在未询问的情况下用缓存。

### Fixed

- **浅色主题次要文字对比度提至 WCAG AA（[#15](https://github.com/Cvencent/client2api/issues/15)）。**
  实际生效的浅色块 `--ink-3` 从 `#7a828f`（压白底 3.6~3.9:1）提到同色相的
  `#687181`（三种实际背景上均 ≥4.59:1）——面板地址、页头按钮、筛选 chip 等
  30 处次要文字不再糊进背景。同文件 4 套 light / 4 套 `:root` / 2 套 dark 的
  重叠定义加注释标明死块与生效块，消除「改哪份才生效」的歧义（彻底合并留
  后续）。

### Installer

- `client2api-setup-0.5.31.exe`（空包，不含账号凭据或本机配置）
- SHA256：`FAA10DFBA1C00F8EC1F4040553BDA488FEC0917E005885F401D72257F0BA5A05`

## [0.5.30] - 2026-10-31

### Fixed

- **总览页纳入周期刷新，连接状态由每一次轮询驱动（[#20](https://github.com/Cvencent/client2api/issues/20)）。**
  `refreshVisible` 的视图门此前只放行账号池页：停在总览页时数字永远是打开
  那一刻的快照，页头「已连接」也只在首屏置位一次——后端进程死了几分钟，页面
  仍报绿点 + 100% 可用。现在总览跟随 5 秒轮询（弹层/忙碌守卫照常），连接态
  记账连续失败：失败翻红点并显示「已重试 N 次 · 上次成功 HH:MM:SS」，成功
  一次即翻回。

### Changed

- **「重启进程」增加二次确认，文案按在途流量分档（[#17](https://github.com/Cvencent/client2api/issues/17)）。**
  删除账号、清空告警/对话都有确认，只有重启这一处点击即执行——而它会中断
  所有在途请求。现在无流量时轻确认一句；有在途请求时明确警告「现在有 N 个
  在途」；有待生效的已保存改动时顺带说明。与项目既有的危险操作约定对齐。

### Installer

- `client2api-setup-0.5.30.exe`（空包，不含账号凭据或本机配置）
- SHA256：`0783429FD9BE0187BBB306085C27AC4B7ED13B9BC3DC9E9BBEA7A3BAB7F5BD52`

## [0.5.29] - 2026-10-31

### Fixed

- **「监听地址」保存前校验，不再一错锁死面板（[#19](https://github.com/Cvencent/client2api/issues/19)）。**
  `validateConfig` 对 `listen` 此前只做字符串类型检查：少写端口、写垃圾串都能
  保存成功，保存 toast 引导用户点「重启进程」，新进程随即死在 `net.Listen`，
  交接不留错误记录，面板从此打不开。现在保存前后端用同一规则把关（空=沿用
  内置默认；host 可空；端口必须是 0-65535 的数字——0 保留给测试场景），前端
  在保存入口先预检并聚焦报错；`validateConfig` 单测覆盖合法/非法全表。

- **中转站 `base_url` 保存即校验，URL 结构错误不再伪装成厂商故障（[#21](https://github.com/Cvencent/client2api/issues/21)）。**
  少写 `http://` 的地址此前照存为「就绪」，第一次真实请求报 503
  platform exhausted，三次重试烧光熔断预算把账号打进冷却，gateway_hint 还
  说「厂商降级，可重试」。两层修复：添加账号时 `validateBaseURL` 校验
  （scheme 必须 http/https、host 非空，缺协议头明确报错、不静默补全）；
  `classifyUpstream` 把 `unsupported protocol scheme` / `no such host` 这类
  「地址本身不可用」的错误归为 `FailureOther`（配置错误语义）——
  `noteProviderFailure` 本就跳过该类，账号不再被打进冷却，错误提示也不再
  指向厂商。

### Installer

- `client2api-setup-0.5.29.exe`（空包，不含账号凭据或本机配置）
- SHA256：`596E7FB237DC8A5F4FD7BBA59958611CE0D9C5433677C83F744AA1D65A913E81`

## [0.5.28] - 2026-10-31

### Added

- **modelpark 的停机动作现在有日志了（[#18](https://github.com/Cvencent/client2api/issues/18)）。**
  `Store.Park` 此前只更新内存 map，一个 (平台, 账号, 模型) 被停了多久、是第
  几次命中、用上游 Retry-After 还是本地阶梯，日志里无处可查——#9 的修复也
  无法从日志验收。`Store` 增加可选 `logf`（`SetLogf` 注入，nil 安全），每次
  park 生效打一行：平台/模型/账号（`core.MaskSecret` 打码）、时长、来源
  （`retry_after=Ns` 或 `ladder`）、累计命中次数与脱敏原因。park 事件有意
  不折叠：时长逐次翻倍本身就是信息。

### Installer

- `client2api-setup-0.5.28.exe`（空包，不含账号凭据或本机配置）
- SHA256：`010D29A3CF9E9250F08971B5CFC90D65A0B49EA14051A85EB9A6D3691B59A708`

## [0.5.27] - 2026-10-31

### Fixed

- **配置页「保存配置」不再恒失败（[#13](https://github.com/Cvencent/client2api/issues/13)）。**
  凭据脱敏正则里的裸 `session` / `auth` 词命中了**配置块名**而非凭据字段：
  `session_sticky` 整块和 `sources.*.auth_cooldown` 被整体打成 `<redacted>`，
  时长占位符填进输入框后，保存时长的格式校验必然拒绝——整页等于只读。三处
  修复：凭据键正则收紧为具体组合（`session_token` / `auth_token` /
  `authorization` / `private_key` 等），`tokens` 复数与 `keys_path` 类键不再
  误伤，修后示例配置只有 `api_key` 命中；`maskSecret` 递归下探、只 mask
  字符串值，布尔/数字/时长原样保留（真撞上凭据块时普通设置不再连坐）；前端
  `cfgFieldText` 遇 `<redacted>` 渲染为空——空串对时长/数值读取器就是
  「沿用现值」的合法语义，占位符永远不再进输入框。

- **总览页四格 KPI 不再恒显示 0（[#10](https://github.com/Cvencent/client2api/issues/10)）。**
  `tickNumber` 用 `el.id` 当动画句柄的 key，而四格 KPI 由 innerHTML 重建的
  `.v` 元素没有 id——四个元素共用空 key 互相取消动画帧，前三格永久停在 0。
  句柄改记在 `WeakMap<Element>` 上，key 跟着元素走：id 缺失或重复都不再
  互相取消，被重建抛弃的旧元素句柄也可自动回收。

### Installer

- `client2api-setup-0.5.27.exe`（空包，不含账号凭据或本机配置）
- SHA256：`1746B19A8C02D739FE6F704E7A2242DFECA737F5AB489555BB4506AEDF796280`

## [0.5.26] - 2026-10-31

### Added

- **loomy / openaicompat 系 / raccoon 的 429 现在遵循上游 Retry-After（[#9](https://github.com/Cvencent/client2api/issues/9)）。**
  Retry-After 解析此前只在 cline/codearts/qwenwork/trae/workbuddy 实现，主力
  平台限流后只能按本地固定冷却回血，上游给的精确窗口被白白扔掉。现在三个
  模块补齐同款解析（`Retry-After` 秒数/HTTP 日期、`X-Ratelimit-Reset`、
  `Retry-After-Ms`，统一 2 小时钳制）：**loomy** 的 apiError 携带提示，模块内
  冷却取「阶梯与提示中较长者」（阶梯照常逐级），并以带提示的
  `FailureRateLimited` 交给网关 park；**openaicompat**（覆盖 officeace /
  bcsn / chatanywhere 等 10+ 中转）的 `classifyHTTP` 接收响应头并把提示塞进
  Failure；**raccoon** 的 `decodeEnvelope` 解析到 apiError、分类时透传。
  网关 `modelpark.Park` 原本就以 positive retryAfter 覆盖指数退避（钳
  30 分钟），无需改动。

### Installer

- `client2api-setup-0.5.26.exe`（空包，不含账号凭据或本机配置）
- SHA256：`C8087DA7DB916B1999BE15BE88BDD8E4AEB99B237FBCB9D0963970D3BB533CC2`

## [0.5.25] - 2026-10-31

### Fixed

- **告警页 KPI 在有告警流水时不再显示「干净」（[#8](https://github.com/Cvencent/client2api/issues/8)）。**
  打开页面即全部标记已读是有意设计，但页头 KPI 的副文案只看未读数——进页后
  未读恒为 0，「未读 0 · 干净」与同屏的告警柱和 15 条流水自相矛盾。现在
  副文案按流水行数分档：有流水如实显示「N 条流水」，真没有流水才显示「干净」；
  未读数本身单独一格不变。

### Installer

- `client2api-setup-0.5.25.exe`（空包，不含账号凭据或本机配置）
- SHA256：`4EF2286478FD76F84A7594C418C2E8F48AEA3BA7AEB63E84C686BEF197C4EC8F`

## [0.5.24] - 2026-10-31

### Fixed

- **3D 模式下选中的板卡与检视面板活不过一个轮询周期（[#6](https://github.com/Cvencent/client2api/issues/6)）。**
  账号池 5s 轮询改变读数 → 1.5s 签名对账整体重建舞台 → 重建无条件
  `B3D.sel=-1` 并收起检视面板（`.b3d-inspect`），活跃流量下点选后约 4.75s
  必失。现在重建前按 label 记住选中的板卡，重建后按 label 找回索引并恢复
  读数与检视面板（账号被删才真正清空）；用户正与检视面板交互（面板内有
  focus 或滑杆拖动中）时该轮签名重建整体跳过，与拖拽豁免同款。

- **两条 401 节拍日志刷了 5 天 ~1400 行：日志去重从未跨 sweep 生效（[#7](https://github.com/Cvencent/client2api/issues/7)）。**
  `probeLogDeduper` 原来在每次 sweep 函数体内新建——自带一张全新 seen map，
  注释里承诺的一小时窗口从未真正生效；`ModelRefresher` 循环则在去重之外
  完全裸打，已失效凭据每 5 分钟被敲一次并原样记一条。现在 deduper 提为
  进程级单例，余额 sweep、恢复 sweep 与 ModelRefresher 三条路径共用：同一
  (平台, 账号, 错误文本) 一小时至多一条，窗口过后恢复记录并附
  `repeated N×` 被抑制条数。

### Installer

- `client2api-setup-0.5.24.exe`（空包，不含账号凭据或本机配置）
- SHA256：`3C276E6E6FD5311730E70980F588F58059760E316748F33D2A35FF1ADCDA2CF3`

## [0.5.23] - 2026-10-31

### Fixed

- **901–1100px 视口全页横滚归零（[#5](https://github.com/Cvencent/client2api/issues/5)）。**
  `.main` 基础的 `overflow:hidden`（裁住极光装饰层）被两条 `max-width:900px`
  媒体查询的 `overflow:visible`（为 sticky 顶栏）打开后，`.main::after` 极光层
  `inset:-30%` + 46s 旋转的对角线包围盒整条计进 scrollable overflow——该宽度
  区间内全部 13 页可拖出 436–794px 横向空白。修法：两条 900px 查询里极光层
  收边（`inset:0`）并停旋转（纯氛围层，无感知），另给 `.main` 补一条断点无关的
  `overflow-x: clip` 兜底。sticky 顶栏依赖的纵向 visible 语义不变；headless
  实测 900/1000/1280px 下 13 个视图横向溢出全部为 0，sticky 行为正常。

### Installer

- `client2api-setup-0.5.23.exe`（空包，不含账号凭据或本机配置）
- SHA256：`3A83221BB516FF6AAC0D24FE53A530365BBD74D3AE90A8A08D6877D007D3F41E`

## [0.5.22] - 2026-10-31

### Fixed

- **用量页头部「保留 N 条」改为「明细保留 N 条」，读的是每请求明细环，不再是运行日志行数（[#4](https://github.com/Cvencent/client2api/issues/4)）。**
  原来取 overview 的 `log_lines`（面板运行日志的内存行数，随日志增长），而
  「最近调用」子表实际只保留 100 条（`DefaultUsageRecords`），两个数字在
  同一页面互相矛盾。现在与子表同源（usage report 的 `recent`），页面内自洽。
  顺带把「用量页 5 秒轮询」的过时注释改成实际行为（进入/切子表/手动刷新时
  更新）；轮询行为本身不变。

### Installer

- `client2api-setup-0.5.22.exe`（空包，不含账号凭据或本机配置）
- SHA256：`D0A562FA669ABBF1421B6790D739183DA7611B9867679F4250EE2275D5F5884B`

## [0.5.21] - 2026-10-31

### Fixed

- **账号卡展开后不再出现两排相同的操作按钮（[#3](https://github.com/Cvencent/client2api/issues/3)）。**
  卡片模板在展开态同时渲染了外层 `.ac-foot` 和 `acctDetailHTML` 内嵌的
  `.ac-foot wide`——后者是 3D 检视块（`b3dInspect`）的唯一动作排，卡片就地
  展开复用同一函数时没人去掉外层那份，于是同一组按钮上下叠了两排。现在
  open 态跳过外层 foot（内嵌那份就是完整动作链）；收起态的外层 foot 与
  3D 检视路径的内嵌 foot 都不受影响。

### Installer

- `client2api-setup-0.5.21.exe`（空包，不含账号凭据或本机配置）
- SHA256：`FA73CB9B9187DDF001300D50EA9401B7AA4294C62F33AC668D0A455FB3F66EA1`

## [0.5.20] - 2026-10-31

### Fixed

- **实况页请求流水渲染必抛 `report is not defined`（0.5.19 回归，[#1](https://github.com/Cvencent/client2api/issues/1)）。**
  edb3194 给流水区补「窗口内共 M」文案时，函数体引用了 `report.total_traces`
  却没把 `report` 传进 `rlRenderEvents`，窗口内有请求时每次刷新渲染必抛
  `ReferenceError`，流水列表画不出来，底注和导航徽标也不再更新。签名改为
  `rlRenderEvents(report, traces)`，调用点同步传入（与 `rlRenderKpis` 同款）。
- **客户端矩阵不再对不管理账号的模块发注定 501 的请求（[#2](https://github.com/Cvencent/client2api/issues/2)）。**
  `loadAllAccounts` 对全量模块 fan-out `/accounts`，非 AccountManager 模块每次
  loud 刷新必吃一个设计内的 501，Chromium 对非 2xx fetch 的固有网络报红在 JS
  层压不住。现在 `/overview` 能力块里明确 `manage:false` 的模块直接记
  unsupported、跳过请求；能力未知（老网关回退路径）的模块行为不变。501 语义
  在服务端保持不变。

### Installer

- `client2api-setup-0.5.20.exe`（空包，不含账号凭据或本机配置）
- SHA256：`6AF5D978583ADDC8E4E4425CC2DBE9ED4785444433392F59FB467BDAEE39D59D`

## [0.5.19] - 2026-10-30

### Added

- **签到不再在重复窗口里空跑。** 调度器现在读宿主的每日签到状态：某个账号今天已经结算过（done）时，同一窗口再触发就直接跳过并记一条「already settled today」，不再每次都去点厂商。只有**已结算**才跳过——「暂无可领」（unavailable）不算，因为那正是重复窗口存在的意义：19:05 的窗口扑空后，19:15 授权到账时还得再试。没接这个状态的宿主行为完全不变。
- **workbuddy 工具 schema 归一化加固（11129 系列）。** 根 `type` 非 `object` 时改写为 object 并保留成员；`type` 词精确匹配（`OBJECT`、`" string "`、OpenAPI 别名 `str/int/any` 全部拒绝），能正名的改写成小写标准词；`examples` 是裸值时包成数组；其余关键字的值也做类型检查，形状不对的关键字直接丢弃——与未知关键字同罪。

### Fixed

- **Loomy 把「调用方取消」误报成「平台没有可用账号」。** 取消、以及契约自身的哨兵错误（未配置/忙碌/不支持）都不属于传输栈，之前被 catch-all 一起归到 `ErrPlatformExhausted`——操作员按了停止，面板却说三十个健康凭据的池子没号。现在原样把取消交回调用方，不再伪装。

### Installer

- `client2api-setup-0.5.19.exe`（空包，不含账号凭据或本机配置）
- SHA256：`6FDC4A3F46B2105D6B7E2A191D7C6EC5EC2A7F0457A692305FC5BE4FC7BFED87`

## [0.5.18] - 2026-10-30

### Fixed

- **路由实况的「窗口内请求」数量不对。** 请求一多，这个数字会一直停在 60：流水图一次最多画 60 条（`limit=60`），而 KPI 直接用了「画出来的条数」——窗口里真实有 100 条时，它也显示 60，看起来像统计错了。服务端现在在**截断之前**把窗口内的真实总数一起返回（`total_traces`），KPI 显示这个数；流水图仍画最近 60 条，注释会写明「最近 N 条（窗口内共 M）」，两个数字不再互相矛盾。窗口过滤也在计数之前生效，48 小时前的旧记录不会混进来。

### Installer

- `client2api-setup-0.5.18.exe`（空包，不含账号凭据或本机配置）
- SHA256：`C659E9AC28FB8A6F92707B276758422AE2B8A973024E83DF7E764F40050A29CD`

## [0.5.17] - 2026-10-30

这一版全部来自一次完整的逐页评审（13 页 + 星图 + 双主题 + 双形态）。

### Fixed

- **浅色星图右半边被白雾罩死。** 不是过渡动画，是采样越界：星图背景用 clamp 模式取星云贴图的一小块，`offset(0.18) + repeat(1.6) = 1.78`，超过 1.0 的部分在 clamp 下**全部取到同一个边缘像素**——画面正中就是一道竖直硬边，右半边一片死白，等多久都不会恢复。改成 `repeat 1.25、offset 0`，右侧正好停在内侧；实测横向亮度突变为零。
- **平台页 3D 取景失控 + 螺旋看不懂。** 隧道形态把 4 张卡片挤在右边缘、一半出画，中间那条青色螺旋和卡片毫无视觉联系。路由本来就是「一个入口、多个平台」的星形结构，改回环 + 中心枢纽；22 个平台排成 2 排 × 11 列（每排列数上限从 8 提到 12——宽扁舞台排数越多卡片越矮、宽度永远用不满），卡片名从 30px 提到 36px 并改用主墨色，缩到最小时名字仍然读得出。
- **积分页 3D 信息为零。** 环上一排卡片只有名字，数字被挤没了。改成柱阵：高度 = 剩余积分，颜色 = 到期紧迫度（3 天内红、7 天内黄、其余蓝），和用量页同一套形态语言；只摆**已经读到余额**的账号——没读到的画出来是压在地板上的方块，那不是「零积分」是「还不知道」，两者必须分开。余额异步到位后舞台按签名重建一次（之前柱子全是零高就是这个原因）；一个都没读到时给具体空态（「点刷新余额让服务端分批去问厂商」）而不是泛泛的「暂无数据」。
- **3D/列表切换生硬且像没反应。** 按钮立刻高亮、场景却要两三秒才出来，中间没有任何暗示；而且切换的过渡代码放在了 `mountBoard3d` **之前**取舞台——列表模式下页里还没有那个元素，过渡一次都没生效过。顺序修正后：构建期间舞台给一层流光占位，画布挂上后 260ms 淡入。
- **3D 卡片没有悬停反馈。** 提示写着「点击卡片看详情」，鼠标放上去却什么都看不出。悬停时卡片放大 1.07 倍并抬起、背后的光晕变亮一倍（实测缩放到位）；这是 3D 里唯一等价于 CSS `:hover` 的暗示。
- **数据少的页面舞台太空。** 一两个账号时卡片孤零零悬在中间。1 张放大到 4.2 高（原 3.4），2 张 3.9；取景改为**优先铺满宽度**（舞台是 4:1 宽扁带，纵向需求常独自决定距离，导致内容永远只占中间一条）。

### Added

- **星图运镜。** 点一个区域时镜头先往那里推近（zoom 缓动 +35%，420ms），抽屉再滑出——大空间跳转有了「走进去」的过程；从抽屉返回时恢复进入前的缩放与朝向，不再被重置成初始全景（实测：进入前 1.5/0.43rad，返回后 1.5/0.45rad，保留）。

### Installer

- `client2api-setup-0.5.17.exe`（空包，不含账号凭据或本机配置）
- SHA256：`247C5A734034B20B684041E098D900430E53A443D2FB750F8B4F7246CE8DCD7B`

## [0.5.16] - 2026-10-30

这一版修的是「用起来能感觉到」的四个问题。

### Fixed

- **每次切页整屏闪一下。** 视图容器上叠了**四套**入场动画：`shellViewIn`、`viewIn`、`ovIn`、`uiViewIn`（分四次迭代加进去的，谁也没删谁），加上区块级的级联——每次切页内容先从全透明淡入四次。现在容器层全部移除，只保留一套区块级编排（`.fx-in`），并且起始透明度从 0 提到 0.55：内容从半透明上浮，不再有一瞬间的空白。
- **点菜单的爆炸特效位置是歪的。** 波纹是 `position: absolute`，而 `<button>` 默认 `position: static`，于是 `offsetParent` 落到了外层的 `.topbar` 上，坐标被算成相对顶栏——实测偏移 **-1521px**，波纹飞到了屏幕另一头。给按钮与卡片补定位，并在运行时兜底（宿主仍是 static 就地补），实测偏移降到 1px。
- **3D 和星图没有背景，只有一层浅网格。** 之前是纯黑渐变天穹（顶与底只差 4% 亮度）+ 520 颗 0.26px 的星 + 一张默认 `GridHelper`，三层都几乎看不见。现在重做成五个层次：程序化生成的**星云**（多倍频光斑 + 银河带 + 尘埃颗粒，2048×1024，不是外部图片）、**两层不同速度自转的星**（2400 远星 + 90 带十字光芒的近星，形成视差）、**极坐标刻度环**台面、地平线辉光、扫描环。网格退到 0.18 不透明度，只做背景纹理。
- **全池概况把页面撑长了。** 它原本是内容区右侧的一整列，会让账号池页凭空多出一栏高度。移到页头「账号池」标题右侧的中间槽，横排成一行：可用率 + 状态比例带 + 平台/冷却/异常计数，悬停有每个平台的可用明细。

### Changed

- 3D 背景板的尺寸按**相机视锥**计算，不再写死。舞台是 4:1 的宽扁带，固定宽度只能盖住中间一半（实测 NDC 只到 ±0.5）。星图用正交相机，另外按视锥算尺寸并改用 clamp 采样，避免平铺接缝在画面正中留下一道竖亮带。

### Installer

- `client2api-setup-0.5.16.exe`（空包，不含账号凭据或本机配置）
- SHA256：`40A7F438E88E6CB5A901E8056E668188663EAF5502D3BFCAF9D41EF127616948`

## [0.5.15] - 2026-10-30

### Added

- **测试账号时可以选择模型了。** 账号行（表格与卡片墙都有）的「测试」现在带一个模型下拉，默认档是「默认模型」——那就是这个功能出现之前的行为，选它等于什么都没变。选了具体模型，探测就真的按那个模型发一次最小请求，结果提示里也会回显用的是哪个模型（例如「测试通过（kilo-auto/free）」）。这样能回答的是「这个账号能不能跑这个模型」，而不只是「这个账号还活着吗」。
- **下拉里是这个平台**自己**的实时模型目录**（和「模型」列、模型弹层同一份数据），不是一份写死的清单；某个平台目录还没取到时按普通按钮渲染，功能不受影响。

### Changed

- 探测模型的来源从一个写死的默认值，变成「操作员可选、否则沿用默认」。12 个模块接了这一档：autoclaw、cline、codearts、kilocode、loomy、minimaxcode、opencode、openrouter、raccoon、tabbit、trae、zcode。其余模块的探测模型是固定模型（或根本不发对话，只读账号信息），它们**不**显示下拉——给一个选了也不生效的控件比不给更糟。

### Fixed

- **`/accounts/<id>/test` 一直收着 `model` 字段然后丢掉。** 请求体里这个字段从接口存在的那天就在解码，但解码之后没有任何人用它，于是「指定模型测试」这个能力在协议上一直假装存在、实际没有。现在实现了 `ModelTester` 的模块会真的收到它。
- **账号行「测试」按钮的文案断言没有跟着搬家。** 按钮改由 `testButton()` 渲染（因为它现在可能是「按钮 + 下拉」），原来那条按字符串找 `data-do="test"` 的测试会失败。现在断言拆成两条：行里必须调用 `testButton`，而这个函数本身必须渲染中文「测试」和模型下拉。

### Installer

- `client2api-setup-0.5.15.exe`（空包，不含账号凭据或本机配置）
- SHA256：`6B5C42E494DEA3D7C906028DE62FD430442F6AABD72F83FFD2E719BEEE0F7B84`

## [0.5.14] - 2026-10-30

这一版修「账号明明能用却被判死」和「日志被同一行刷满」两类问题，另外补上
workbuddy 工具 schema 的新校验规则，以及 zcode 每日计划的账号归属。

### Fixed

- **一次分组权限 403 会把整个 key 判成「账号异常」。** 中转对没开通的分组回答
  `403 No permission to access group …`，旧代码把 403 一律当凭据失效，于是同一个
  key 上能用的模型也全部退出轮换。现在模糊 403 归到模型级拒绝，账号留在池子里
  交给兄弟平台 serve；只有明确说凭据死了的措辞（`invalid api key`、`banned`、
  `revoked`、`封禁`）才判死。
- **余额探测要等聊天撞上才知道凭据被拒。** 现在余额/模型探测第一次看到明确的
  凭据拒绝就 park 该账号；「这台中转根本没有计费接口」不再算账号问题，四种计费
  协议全拒的中转记 6 小时负缓存，不再每轮写出四条报错。
- **余额巡检日志每五分钟把同一行再写一遍。** 同一（平台，账号，错误）一小时内
  只写一行，之后恢复时在行尾带上被压掉的次数，日志重新能读。
- **冷却中的平台在日志里被说成 `client is at its in-flight limit`。** 现在写明是
  冷却，并给出到什么时候解除。
- **对外白名单的 404 只有一句 `not exposed by this gateway`。** 现在分清三种情况：
  模型存在但没发布（去加 `exposed_models`）、这个名字没有任何平台能用（调用方拼错
  了）、每个平台都把它关掉了（去重新打开）。用量行同时记下「模型不在对外白名单」，
  不再看起来像掉连接。

### Added

- **workbuddy 工具 schema 归一化又补了一轮。** 嵌套 `$ref` 内联（pydantic / MCP
  服务对每个嵌套模型都发 `$defs` + `$ref`，而厂商的校验器不解析引用）、裁掉校验器
  不认识的单个关键字、`const` 换成单值 `enum`、联合 `type` 收成单值、`required`
  里的名字必须在 `properties` 里声明过。
- **11129 仍然出现时，把被拒的 schema 落一份盘。** 厂商的回答不带字段名，重写后的
  请求体是唯一证据；每个进程只写一次，避免重试把磁盘写满。
- **zcode 的激活事件带上账号自己的凭据。** 原来的匿名上报只说明「这台设备上有人
  活着」，账号池里第二个以后的账号拿不到当天的计划（直接问会得到 1004「该账号不
  符合条件」）。现在先按账号上报，被拒再退回匿名，两种账号都能报活。

### Installer

- `client2api-setup-0.5.14.exe`（空包，不含账号凭据或本机配置）
- SHA256：`0131990DB8656D406FE6D05C26B75D2D1757AA07DB25AD4C9541353A2D4E3C3F`

## [0.5.13] - 2026-10-30

这一版把动效补厚了一层，并且逐页把「两种形态都说不出话」的地方修掉。

### Added

- **入场编排不再是一起亮一下。** 视图进场按视觉顺序级联：卡片/面板/舱位条/表格行各带一格 `--i` 延迟，上浮 14px、轻微解糊，42ms 一项。上一版是全体统一的 6px 淡入，读不出层次；现在眼睛会顺着次序扫过去。
- **数字会滚。** 账号卡主读数、全池可用率、总览 KPI 在数值变化时从旧值滚到新值（弹簧缓动 + 等宽数字，不会左右抖）。首次渲染直接落值，不演。
- **点击有波纹。** 按下的地方扩散一圈柔光，颜色取自主题强调色；卡片、按钮都接。
- **光标跟踪的卡片柔光。** 指针在卡片内移动时，卡片背后一团柔光跟着走（只写 `--mx/--my`，不改 transform，所以表格里的卡片也不会扰动 sticky 表头）。
- **进度条生长。** 带 `data-fx-grow` 的条与积分到期条从 0 长到实际比例；用量图表的柱子逐根错开长出。
- **3D 舞台也有编排了。** 构建后 0.9 秒内卡片从压平状态生长到原位（每张错开 18ms），指针悬停某块时它抬起 4.5%、光标变手型。

### Fixed

- **3D 动画的时长取决于帧率。** 帧循环原来每帧固定 `t += 0.016`，于是「入场展开 0.9 秒」在 60fps 是 1 秒、在 20fps 变成 3 秒——弱机上卡片会长时间半透明。改成真实时间间隔（上限 1/20 秒），星图同样处理。
- **客户端页的能力带只有一排勾和点，没有锚点。** 7 个符号并排时没人知道第 4 个是接码还是签到。现在开启的格子写单字缩写（管/导/登/码/添/签/刷），上方有一行列头，`title` 里是真名；关着的仍是一枚静默的点，所以「有几项」依然可数。
- **积分页七条进度条一样长。** 原来每档在「自己内部」归一化，于是每行都填满，看不出哪一档的积分更多。改成按「本档 / 最大档」的比例填充，并把紧迫度从「每段刷成红色」移回行左侧一根竖条 + 数字着色——段色重新表示账号来源。
- **账号池被筛到只剩一张卡时，右半屏是黑的。** 新增常驻的「全池概况」栏：可用率、按状态堆叠的比例带、每个平台的可用/总数。它读的是**未筛选**的全池，所以筛选不会把它变空。
- **告警页的 3D 摆的是「每个平台有多少账号」。** 那是账号池的数，放在告警页没有意义。现在按平台汇总真实告警条数与最大连续失败数；没有告警时反过来摆「正在监控什么」，说明为什么是空的。
- **日志页的 3D 四张卡都写着「通道」。** 日志条目没有跨平台字段，所以按它自己的结构分：任务 / 对话 / 系统各一张，读数是该频道本轮条数与其中报错数。
- **对话页的 3D 只写「模型」。** 改成每个客户端一张卡：可用档数 + 是否就绪 + 用途。
- **隧道形态的卡片列被裁到画外。** 这个形态左右不对称（隧道中心在左、卡片列在右），单一取景半宽会按更大的一侧算、裁掉另一侧。加了 `camX` 把视锥中心挪到两者之间，并让卡片尺寸随条目数走（3 条时 1.14、4 条时 1.05）。
- **3D 卡片在入场时几乎透明。** 入场的不透明度起点从 0.15 提到 0.55，1 秒内到 1。
- **3D 舞台的数据是「建舞台那一刻」的快照。** `go()` 先挂舞台再拉数据，于是日志/模型/配置这些懒加载页的卡片写着「本轮无日志」「0 个」。现在数据到位后按签名比对重建一次；签名不变就不重建，所以轮询不会每 1.5 秒把入场动画重播一遍。
- **两排卡片的顶部与底部各被裁掉一截。** 纵向半高只加了半张卡的高度，实际需要一整张。
- **总览的地球在宽扁舞台上只占中间一小块。** 取景改成按球体半径，而不是按「球 + 标签」。

### Installer

- `client2api-setup-0.5.13.exe`（空包，不含账号凭据或本机配置）
- SHA256：`54B15AFA8E690CA205CA801602B082702038CC269E7DF8ED57B29510C7BD1CEA`

## [0.5.12] - 2026-10-29

这一版只做一件事：把 3D 舞台和星图从「看不懂的方块」重做成一套说得通的场景，
并补上用户一直在要的深色 / 浅色开关。

### Added

- **深色 / 浅色两个可见档位。** 原来是一个图标按钮在「深 → 浅 → 跟随系统」之间循环，看不出当前是哪档、也等不到想要的第三态。现在右上角是两个并排的段落（深色 / 浅色），当前档实心高亮；命令行面板里的同名动作也跟着变成「切到浅色 / 切到深色」。
- **3D 与星图有了「环境」。** 之前每页只有一堆悬空的板子，既没有天也没有地，看起来就是没做完的建模。现在每个场景都带：渐变天穹、地平线以上的星场、两级网格地面、两圈错相向外扩散的扫描环，以及每块卡片背后的软光斑和落到地面的托架。

### Fixed

- **浅色主题下铭牌和白底糊在一起。** 3D 卡片与星图铭牌都是画在 canvas 上的贴图，颜色被写死成「深底浅字」，切到浅色就是白底白字。现在贴图与场景的每个颜色都按 `data-theme` 取（canvas 不认 `oklch()`，所以用一套等价色值），并且换主题时会重建正在显示的画布——否则画面上留着上一套颜色。
- **铭牌被透视拉成梯形、近处的一块大一圈。** 星图改用正交投影：所有铭牌等大、不随距离缩放；同时用 30° 长焦取代 50° 广角，边缘不再扭曲。
- **星图的环整圈跑出画外、铭牌互相压。** 半径由「相邻铭牌至少留 16% 宽的缝」反推，视锥按当前仰角实时算（之前漏算了立柱往下探的那一段），并把仰角压到 30° 左右，把环展成一条铺满 16:9 的椭圆。
- **星图中间的枢纽像一块扁盘。** 原来只有一颗球加两圈线；现在是双球核 + 三圈不同直径的轨道环 + 一道细光柱，浅色主题下自动减淡，不再糊成一团。
- **账号池只有 1 个账号时，板卡被推到很远、缩成一个小方块。** 「一圈环」这种摆法只对中等数量成立。现在改成「全息控制墙」：板卡排成几排、正对相机，1~3 条时给大板卡，20 条以上自动收小，超过 24 条汇总成一张「另外 N 个」的卡并指路到列表。
- **模型页把 355 个模型逐个摆出来，满墙只看得见数字。** 改成按客户端聚合：每个客户端一张卡，主读数是模型数，副标题是免费档数量，最大上下文进检视面板。
- **用量页的柱阵只有一两根柱子。** 原来按客户端分桶，大多数平台没有流量，柱阵就剩一两根，读不出趋势。现在改用和下方网格图同源的**时间桶序列**，并给柱阵加了 25/50/75/100% 四条参考线，柱高用平方根压缩动态范围。
- **运行日志 / 对话 / 配置的隧道里卡片互相遮挡。** 卡片塞进隧道必然互相压。改成「左侧隧道负责纵深、右侧一列最新 4 条负责读内容」的分工。
- **卡片内容堆在上半、下半空着。** 贴图版式原来是写死的 y 坐标，某一段为空（没有数值、没有副标题）下面就留一大片空白。现在先量每段占多少行，再按剩余空间均分，整张卡始终填满；没有数值时把状态词放大当主角，并在下面补一句这个状态意味着什么。

### Installer

- `client2api-setup-0.5.12.exe`（空包，不含账号凭据或本机配置）
- SHA256：`EA4D1128C05AD3D1FC31AA5735A93F08F02779C4CF6E7F663F8D421F73804E12`

## [0.5.11] - 2026-10-29

### Fixed

- **报错只说「[object Object]」，不说是哪儿错了。** `api()` 把响应里的 `error` 字段原样赋给 `err`，而 OpenAI 风格的错误体是**对象**（`{"error":{"message":"invalid API key","type":"…"}}`）——面板里约 90 处提示都在拼 `r.err`，于是全变成「读取模型失败：[object Object]」这类看不懂的句子。现在在产生错误的那一处统一折成一句人话（支持 `{message,type}`、`{error:{…}}`、`Error` 实例三种实际会收到的形状）。

### Installer

- `client2api-setup-0.5.11.exe`（空包，不含账号凭据或本机配置）
- SHA256：`8339EF54908BFEEC0D71793A517694142C237845BFA0C3ABE5C0E95D1E8FAE27`

## [0.5.10] - 2026-10-29

这一版不再加新东西，只把我一直没认真看过的页面按「能不能用」修一遍。

### Fixed

- **对话页的输入框在屏幕外。** 工作区高度硬编码成 `100vh − 59px`，那是 masthead 还不存在时的数；页头一出来（约 110px）就把输入区和发送按钮顶出视口。第一次修又错了 54px——按「视口 − 顶栏 − 页头」算，漏掉了滚动容器的内边距和页面的兄弟元素。现在直接量**工作区自己上沿到视口底**的距离（实测 939 / 视口 950，原来是 1004），并在进页、页头挂载、窗口缩放时都重算。
- **配置页的说明文字被甩到行尾一千像素外。** 字段名在左、输入框在中间、解释在最右边，读完一个字段要横跨整个窗口去找它的说明。改成标签/输入/说明三件竖着排，说明紧贴自己那个输入框下面。
- **限宽 62ch 是按整页算的，卡片里被压成细条。** 定时任务那段说明在窄容器里每行只剩 4–5 个字。改成 `min(62ch, 100%)`，跟着容器走。
- **页头几乎贴住第一张卡片**，现在留出真实间距。
- **配置表单和定时任务表在大屏上右边半屏是空的**：配置区限宽 860 并居中，任务表的「时点」列从 480px 收窄，把宽度让给任务名和下一跳。

### Installer

- `client2api-setup-0.5.10.exe`（空包，不含账号凭据或本机配置）
- SHA256：`C5E9220A98319FD76C75EDFB462878FEE1A280E0B2DAB1685905B715A9EBC282`

## [0.5.9] - 2026-10-29

0.5.8 装上是不可用的——这一版是修复版。三处问题都是我的设计错误，不是样式瑕疵。

### Fixed

- **3D 把真实内容盖掉了。** 0.5.8 把 3D 设成默认呈现，并且在 3D 模式下**隐藏了整页除页头和舞台以外的内容**——于是打开一个领域只看到一块浮在大片空白里的板卡，列表、表格、按钮这些真正要操作的东西全没了，这就是「完全不可用不可看」的来源。现在默认是**列表**（内容立刻可见可操作），3D 变成一条可选的可视化带，打开时加在内容**上方**，**什么也不藏**；从星图进领域时也强制列表，抽屉里打开的是实内容。
- **每个页头下面印着一行 `[object HTMLDivElement]`。** 移除滚动活动条时漏了一个分号，`mh.innerHTML = '<div …>' + '</div>' + host.insertBefore(...)` 变成了「把 HTML 字符串和 `insertBefore` 的返回值拼起来」，页头元素本身从未插入。分号补上，页头恢复。
- **星图作为入口不可用，已降级为可选。** 上一版拿它当默认入口，实际是：板子互相压住、整圈跑出屏幕边缘、标签小到读不出字。三项都修了（等距更大半径、板子放大并朝相机仰起、标签字号显著加大、相机拉到俯视远景让整圈进画面），但它不再当门口的默认项——顶栏「星图」按钮进入，默认直接进可用的列表界面。
- **一张账号卡时右边半屏是空的。** 单卡时网格改成有上限的单列宽，不再只剩左边一条。

### Installer

- `client2api-setup-0.5.9.exe`（空包，不含账号凭据或本机配置）
- SHA256：`78374B052840D9B70B39BDAD876FF72C35AE829A3BABD0CD2BAC2F1C501E15DD`

## [0.5.8] - 2026-10-29

### Fixed

- **从星图点进一个懒加载的领域，数据从来没被请求过。** `atlasOpen()` 挂上了页头与 3D 舞台，却没跑这一页自己的数据装载——它当时内联在 `go()` 里那一串 `if (v === "models") renderModels()`。于是从地图进**模型 / 日志 / 配置 / 积分 / 用量 / 平台 / 任务**只会看到一张空页：舞台在，里面什么都没有。账号池把这个洞盖住了（外壳启动时就预载账号），所以直到拿「模型」区域做实机演示才暴露。现在两条路径共用同一个 `loadViewData(v)`，领域装载只声明一次，不会再各走各的。
- **检视面板只在两个页面出现。** 判定顺序先要求条目带「原始对象引用」，而只有账号与模型带——其余页面（客户端、平台、积分、任务、告警、实况、日志、对话、配置）在到达通用事实分支之前就退出了，选中一块板卡什么也不说。改成「专门的检视 → 通用事实 → 收起」。
- **总览 / 用量 / 复用账号条目的页面也没有事实。** 这三页共用账号那批条目，条目当时没带事实，检视同样是空的；config 的隧道只有一块板卡带了事实。
- **CodeArts 一个对话不再占满一个账号的三个会话。** 厂商按账号限「并发会话数 3」，会话由 `Session-Id` 标识。模块此前只在调用方显式给了 `conversation_id` 时才复用这个 id，否则**每一轮都新开一个会话**——于是一段多轮对话就把账号的三个槽位填满，之后该账号对什么都回 `TM.00001041 并发会话数已达上限(3个)`，而面板上它仍显示「可用」。现在拿不到调用方 id 时会退回 `core.DeriveConversationKey`（system prompt + 首轮 user）派生一个稳定 key，和会话粘性用的是同一个；一段对话始终对应一个厂商会话。（代码随 v0.5.7 的构建已发出，这一条是补记。）
- **队列轮询现在问的是「我们这个会话排第几」。** `task_id` 以前传的是随机新 UUID——那是在查一个不存在的任务，返回的只能是模型的全局排队深度：日志里 `queued at position 63 → 115 → 130` 一路涨、请求自己的队列却始终没动过。现在传本次 chat 的 `Session-Id`，与参考实现一致。

### Changed

- **每个 3D 视图对「选中」都有回应。** 各页条目带上自己的事实，通用检视把它们摊成键值行：客户端（能力）、平台（模型数 / 账号数 / 状态）、积分（余额与单位）、任务（批次 / 时点 / 是否启用）、告警、实况（落点 / 结果 / 延迟 / 候选数）、日志、对话、配置。没有可操作内容的页面面板自动收起，不留空壳。

### Installer

- `client2api-setup-0.5.8.exe`（空包，不含账号凭据或本机配置）
- SHA256：`0A67A4E5A11508D5B2B949F62520EC7D38A6C1538B6B95FAB5A8F45D218AFE54`

## [0.5.7] - 2026-10-29

这一版动的是**抽屉里面**：账号从「看一行字、跳去表格改」变成就地可操作的面。

### Changed

- **板卡就地展开，不再跳去表格。** 账号池里点一张卡片，它当场长成整行，露出这个账号真正要动手的那一面：每个通道各自的状态灯与开关、余额、账号 id、以及完整的动作链。展开是**就地**发生的，位置感不丢——不需要「离开这一屏去看另一张表、改完再回来」。再点一次收起。展开状态与表格视图共用同一个键，所以两种呈现之间切换时，你展开过的账号仍然展开着。
- **优先级从数字输入框换成滑杆。** 「调优先级」本质是排序位置（越小越优先），拖着调比算一个数字直接；拖动时读数实时跟手，**松手才写回**——不会每动一格就打一次接口再重画，把手指底下的手柄抽走。表格视图里的数字输入保留（批量精确输入仍然需要那个）。
- **3D 视图本身变成可操作的。** 在星图里选一块账号板卡，舞台下沿展开**检视面板**：这个账号的每个通道（状态灯 + 启停）、余额、账号 id、优先级滑杆、完整动作链。于是 3D 不再只是「看一眼然后切回列表去改」——它自己就能改。没有可操作内容的页面（日志、配置这类）面板自动收起，不留空壳。
- **星图现在有回去的路，并且读数跟着数据走。** 之前从星图退出到「列表视图」就再也回不去地图了；顶栏加了「星图」按钮，主动进入会清掉「已退出」的偏好，刷新也回地图。区域上的读数和健康色之前只在进入时算一次，现在每 1.5 秒按区域签名对账——**正在拖动地图时不重建**，避免把手上那个动作打断。
- **模型板卡也有检视面板。** 选中一个模型，舞台下方摊开它的事实：客户端、来源、上下文、最大输出、价格、计费与所属，只读，附一个「到列表里编辑」把编辑入口留在原处（在 3D 里再复制一套带校验的编辑路径，就是第二个真相源）。没有可操作内容的页面（日志、配置）面板自动收起。
- **星图的抽屉从被点的那块区域长出来。** 点区域时，开关动画以**点击点**为变形原点，滑入并轻微放大；比无条件从右侧滑进来更解释「我刚点了什么」（共享元素过渡的手感）。

### Installer

- `client2api-setup-0.5.7.exe`（空包，不含账号凭据或本机配置）
- SHA256：`207135FB01CE5DECEA6F6D11B3F83F13B4BBE15BE95B836401786A168378A023`

## [0.5.6] - 2026-10-29

这一版动的是**导航本身**：不再有一排 Tab 让你切页面。

### Changed

- **导航不再是一排 Tab：默认入口变成一张网关星图。** 整个网关的每个领域是空间里的一块**区域**，立在网格地面上——账号池、模型、平台、客户端、用量、积分、任务、告警、日志、对话、配置、实况、总览。区域读的是活数据：铭牌上就是读数（「账号池 1/1 可用」），底板与节点簇按健康度染色，每个区域都有一条线连回中心枢纽——产品名里那个「收束」被画了出来。**拖动环绕、滚轮缩放、悬停高亮、点一个区域进入。** 进入时该领域的**真实 DOM 节点**从右侧滑出成抽屉盖在地图上（地图留在背后），`Esc` 退回星图；顶栏那排 Tab 在星图模式下干脆不出现。点「列表视图」可退回常规外壳，选择会被记住。因为抽屉里放的是同一个 `<section id="view-x">`（运行时重新挂载，不是重画），所以从地图进去的页面和原来完全一样：同样的 id、同样的样式作用域、同样的事件委托。
- **界面这一版换的是骨架，不是皮。** 上一版在原有布局里调样式——顶栏一行、KPI 一行、卡片网格、表格行——所以读起来仍然是普通后台管理页。这一版动结构：每个视图顶部加一个 **masthead**（等宽 kicker + 心跳点、clamp 到 52px 的大标题、一行说明、右侧两枚活的读数）；顶栏瘦成一条状态带，标题归页面自己。读不出来的数字留白，不编一个出来。
- **每个页面都有了 3D 呈现，而且是默认。** 顶栏加了「3D / 列表」总开关（记住选择）。复用一台渲染器、一个场景，切页时重建并把画布搬进当前页的舞台——13 个页面各造一个 WebGL 上下文既浪费也会撞浏览器上限。四种形态覆盖全部页面，数据全部来自该页自己已经在用的那份：**一圈信息板**（账号 / 模型 / 客户端 / 积分 / 任务 / 告警 / 实况）、**节点球 + 中心枢纽**（总览——把「收束」这件事画出来）、**柱阵**（用量）、**向纵深延伸的环**（平台 / 日志 / 对话 / 配置）。每块信息板是一张 canvas 纹理，写着名字、主读数、按值缩放的横条和状态色的边框，所以 3D 是能读的而不是纯装饰。交互：拖拽旋转、滚轮缩放、点击选中（选中的板卡上浮，右上角报出它是什么）；静止时极慢自转，离开页面或切回列表即停。密集的列表视图没有被删掉，一键切回，3D 模式下自动收起。
- **顶栏不再承担页面标题。** 它只留面包屑、连接状态和动作，把「这一页是什么」交给下面的 masthead——这是页面节奏感的主要来源。
- **账号池默认改成卡片墙。** 一块板卡一个账号：状态灯、账号名与状态词、大号调用计数，下面五行是成功/失败、在途、到期、模型、状态说明，页脚是同一条动作链。卡片 / 表格只切换呈现——同一份数据、同一套 `data-do` 委托（两个容器都挂了监听），两种模式功能完全等价；原来的表格作为第二种模式原样保留。
- **补上几个参考站点的招牌动效**（全部原生实现，不引运行时）：**Click Spark**（每次点击迸射 9 枚火花）、**卡片 3D 倾斜 + 跟随指针的眩光**。眩光只挂新的账号卡片，不挂含表格的 `.box`——`transform` 会改变包含块，可能扰动表头的 sticky 行为，不值得为一点倾斜去冒险。
- **移除了每个页头下面那条滚动活动条。** 它一直在动、信息量却很低，读起来是晃眼睛而不是内容。
- 全部新动效尊重 `prefers-reduced-motion`。

### Fixed

- **火花不再画偏。** Click Spark 之前按视口坐标画，而画布是 `position: fixed`、祖先可能带 transform 或缩放，两套坐标系对不上，于是「爆炸位置和我点的地方没对上」。现在按画布自身的矩形换算，清屏也按画布尺寸而不是窗口。
- **安装包的 SHA256 不再取决于工作树是否干净。** Go 默认把 VCS 元数据（含 `vcs.modified`）嵌进二进制，于是同一份源码在脏工作树上打出来的包和干净检出打出来的包哈希不同——v0.5.5 第一次记录的哈希就是这样对不上的。现在构建脚本在 `GOFLAGS` 里统一带上 `-buildvcs=false`，记录的哈希是源码的属性。

### Installer

- `client2api-setup-0.5.6.exe`（空包，不含账号凭据或本机配置）
- SHA256：`C3CA39B9EFAE08F55C2D8D54F4C7DB9BC037A3B17816F93EDA6BC995A60A858E`

## [0.5.5] - 2026-10-29

自 v0.5.1 以来的累积发布：界面继续走向「仪器面板」，加上一串真实的失败转移与可用性修复。

### Added

- `internal/fingerprint.Known()`：配置校验用，报告一个档名是否可被 New 接受。

### Changed

- **所有页面脱离「平台数据表」形态。** 账号池变成「机架」：行首状态 LED 带光环、悬停亮起插槽轨、动作按钮平时半隐（行被注视时全亮）、数字全部等宽对齐；客户端页从 11 列勾叉矩阵变成「舱位条」（徽章 + 状态灯 + 能力符号带 + 大数字读数）；告警页从 6 列表格变成带时间刻度的事件流水（平台/模型徽章 + 正文 + 失败读数）；任务看板账号列表变成板卡。排序、筛选、分页、通道分组的数据契约一行未动。
- **表头统一成机架轨道头**：等宽 10px 小标签 + 字距拉开 + 下缘一条刻度线，可排序列悬停亮刻度。这条是全局 `.acc thead th` 规则，账号池、模型页、对外模型、积分明细、定时任务一次覆盖。
- **平台页的路由组画成节点链**：每个成员是一枚节点，Auto 通配成员实心高亮，链从左到右就是路由偏好顺序。
- **路由舞台从光点升级成彗星**：4 段渐细渐淡的彗尾由 JS 逐帧沿真实贝塞尔路径采样绘制，飞行时长由真实延迟决定；网关核心是一枚真的 3D 线框二十面体球（内嵌 Three.js），自转、呼吸、每个请求穿过时脉冲一圈光晕、失败时外壳染红；节点被途经时扩散波纹。
- **设置页分区面板化**：区段自动编号（01/02/…）、等宽小标签、行悬停亮左缘刻度。
- **输入控件统一成「仪器槽位」**：默认无边框只有底部基准线，悬停基准线变亮，聚焦时整框亮起并带底沿光晕。
- **动效层**：统计数字用弹簧滚动（带轻微过冲）、全站按钮按下压缩回弹（文档级事件委托，所有按钮/chip/导航链接一次覆盖）、切页时卡片 24ms 交错入场、卡片聚光跟随光标、极光背景缓慢旋转、页标题逐字入场、排序后行级联滑入、内容区一层漂移的节点连线场。全部尊重 `prefers-reduced-motion`。
- **面板引入本地 vendored 的 Three.js 与 Motion**（`internal/panel/vendors/`，由 `/panel/vendors/` 从二进制内嵌服务），网关离线也能完整工作；CSP 相应恢复为按内联脚本哈希放行。

### Fixed

- **空的根级代理配置现在强制直连。** 网关不再因为桌面环境中的 `HTTP_PROXY` / `HTTPS_PROXY` 自动把所有平台请求送进代理；仍显式配置了代理的源或客户端继续使用各自代理。

- **所有模块的自建 HTTP Transport 现在默认直连。** Loomy 等模块不会再绕过网关共享客户端，偷偷继承 `HTTP_PROXY` / `HTTPS_PROXY`；只有显式配置的模块或源代理才会走代理。

- **裸传输错误不再让整条候选链停在 502。** 模块把 EOF、连接重置、拨号超时这类传输层故障原样抛回时，网关现在将其归为上游失败并继续切下一个平台；请求形态类的拒绝保持原样，不会被放大成全网重试。raccoon 的池内错误分类也随之接入共享失败契约。

- **workbuddy 的工具参数拒绝会切平台了。** 11129「invalid function call parameters」以前和 11101「请求体无法解析」混在一起，被当成请求自身的毛病直接返回 400。前者其实是模型对它自己工具 schema 的挑剔（DeepSeek-V4.1-Flash 不接受空 schema），换一个更宽松的模型平台就能答；后者才是我们的 bug，仍然直接返回。现在两者分开：11129 切下一个平台，11101 保持终态。

- **平台故障不再把账号判死。** Loomy 的冷却梯子第 4 级是永久 `abnormal`，而传输层失败（EOF、连接重置）曾被算到账号头上——2026-10-29 那次平台抽风一口气把 24 个健康会话打成了异常。现在连接层失败只切平台，不再累计账号失败；账号只在凭据被拒、额度耗尽、限流这类真正与账号有关的答复上被处理。

- **Loomy 卡死不再吃掉整条请求。** 上游接受连接却不回响应头时，首字节超时从 2 分钟降到 1 分钟；同一请求累计遇到两次无响应就认定平台卡住，立刻交还给网关切下一个平台，而不是把账号池里剩下的账号挨个排队。卡死不计入账号冷却（账号本身没有过错）。

- **413 现在会切平台。** 第三方（openai-compat）平台按自己的体积上限拒绝请求时，不再被当成「请求形态错误」直接报错；换一个上限更宽松的平台是真实可用的机会。400/422 这类请求本身有问题的拒绝仍然直接返回，不会把同一个坏请求体在所有平台上重放。

- **反复失败的 API key 会被停用。** openai-compat 的 provider 之前完全没有健康状态，一个失效的 key 每个请求都会被再次选中。现在连续失败 3 次且期间没有任何成功就进入冷却（按失败类型选择时长），一次成功立即清零；请求格式错误不计入失败。面板会显示失败次数、冷却截止时间和最后一次错误。

- **纯文本平台收到图片时改切平台。** 以前返回「不支持」直接终结请求，现在按平台能力不足处理，转给支持图片的兄弟平台。

- **流内拒绝的可救范围扩大。** 网关在把流交给客户端之前，会跳过空帧、role 前导帧和记账帧一直探测到第一段可见内容为止；这段窗口内遇到可重试的上游错误就切平台。上游在吐出正文之后才中断的情况仍然无法挽回（内容已经在客户端屏幕上）。

- **第三方平台排队不再吃掉数分钟。** openai-compat 新增首 token 超时（默认 2 分钟，可配 `first_token_timeout`）。中转常用 SSE 注释保活，让按字节计的 idle 超时永远不触发；实测有平台第一帧在 5 分 17 秒后才到，而且是个错误。

- **面板 CSP 恢复按内联脚本哈希放行。** 新增两个同源 vendor 脚本后，旧的内联块提取逻辑判定失败、退回 `unsafe-inline`；现在只哈希无属性的内联 `<script>`，外部脚本由 `script-src 'self'` 覆盖。

- **OpenCode 免费层 10-22 起全灭的问题治本。** 上游 Zen 的匿名免费通道收紧了门禁：TLS 握手必须是 Bun 形状、系统提示词必须带 CLI 注入的 `<env>` 环境块，三查缺一就回误导性的 `FreeUsageLimitError`（8 token 的探针不耗任何额度也会被拒——是门禁拒绝，不是额度耗尽）。之前网关用 Go 自带 TLS 握手（被拒）且免费请求体没有环境块，导致 7 天零成功、每 60 秒重试一次永远撞墙。现在三处补齐：
  - `internal/fingerprint` 新增 `bun` 指标档（Bun 的 17-suite cipher 列表 + X25519MLKEM768 key share——X25519-only 的 share 会被 opencode.ai 的边缘直接 `handshake_failure`）；`clients.opencode.tls_profile` 默认 `bun`，可配其他档或 `"none"` 退出。
  - 免费请求体的系统消息自动注入 `<env> Working directory` 块（调用方自带 system 时追加在其后，没有时作为第一条插入）；付费模型的普通请求体不受影响。
  - `FreeUsageLimitError` 从 429 兜底的 60 秒冷却改判为 quota 类（默认 12 小时长冷却），不再空转。

### Installer

- `client2api-setup-0.5.5.exe`（空包，不含账号凭据或本机配置）
- SHA256：`6A0E2BA5003511F9D00B1A8E2E4CDD61CB50ABF75EC27118B8983727172C721A`

## [0.5.1] - 2026-10-28

这一版是**整个界面的重做**，加上三处 ZCode 的真实修复。

### Changed

- **外壳换了空间模型：左侧导航栏删除，13 个视图收进顶栏一行。**
  那根竖栏固定占掉 208–236px 放 13 个链接，而这个产品真正需要宽度的是账号表
  （11 列）和模型表；导航并不需要竖着排。现在横向多出的宽度全给了数据，
  连接状态从侧栏底部一整块变成顶栏右端一枚窄标签。所有 `data-view` 与 id
  原样保留，导航逻辑一行未改。
- **色板用 oklch 重建，中性色带上品牌色相。** 原来用的是 Element 默认色
  （`#409eff` / `#67c23a`）——这就是「通用后台味」的字面来源。新色板给所有
  中性灰加了极小的冷色相（chroma ≈ .01，hue 245），肉眼几乎看不出，但整屏
  从「中性」偏向「这个产品的中性」；浅色主题是同一套色的明度翻转。
- **字号从「五档挤在 3px 内」拉开。** 原来是 11/12/12.5/13/14——层级糊成一片，
  只能靠颜色和字重去补。现在是 11/12.5/14/17/22+。
- **统计带和用量磁贴由「卡片网格」改成「读数带」。** 去掉边框、分隔线和投影，
  只留一条底线；第一格放大做主角；分组交给间距。用量页原来那七张带彩色顶线的
  卡片（3+4 两行、第一张占半屏）现在是七个读数一行排开。
- **子页签从胶囊改成「文字 + 底边」**，和顶栏导航同一套语法。
- **状态标签去掉圆角框，只留色点 + 文字。** 「未接入」「可用」这类标签一页会出现
  几十次，带底色时变成一片彩色小方块。计数标签（「15 个」）保留按钮形态，
  它是读数不是状态。
- **模型表里「每行都一样」的两列压淡。** 355 行里「虚拟」「上游」重复 355 次，
  那是默认事实而非信息；现在只有「基线」（回退列表）才标红。
- **积分到期条按紧急度染色，不再按天数调透明度。** 原来七条都是满饱和的强调蓝、
  只用 opacity 区分深浅，一屏七条蓝杠读不出哪条要紧。现在 3 天内红、7 天内黄、
  其余中性——由共享助手 `pkUrgency` 统一，汇总图和账号卡片共用一套口径。

### Fixed

- **ZCode 活动套餐不再需要验证码，程序能自己领了。** 客户端弹窗那个
  「ZCODE GLOBAL BUILD · 300,000,000 tokens」原来只能手动点。实测（2026-10-28）
  厂商已不校验：带不存在的 plan id 去问返回 `1001` 而非 `3007`，完全不带验证码头
  的领取被正常接受。现在 `claim` 正常发出，只有手上已有 token 或配了 solver 时
  才附带；没配 solver 的安装不再为它花成本。
- **设备指纹是风控真正读的东西。** 领取返回 `3012「账号或网络环境存在异常」`
  不是账号问题：实测同一账号，用程序生成的 device_mid 被拒、换成 ZCode 桌面客户端
  自己的 id（就是弹窗「领取」按钮发出去的那个）几秒后就领到了。现在设备 id 优先
  采用桌面客户端的，并在遇到 `3012` 时切到该 id 重走一遍。
- **账号池里多出来的那条 `zcode-config:magpie`。** magpie 网关会往
  `~/.zcode/v2/config.json` 写一条 `provider.magpie`，而本模块遍历 `provider.*`
  时只要有 apiKey 就收成 ZCode 账号——于是把本地网关当成了厂商账号，还从它那里
  拉回 43 个模型污染模型列表。现在只接受 baseURL 指向 `z.ai` / `bigmodel.cn` /
  `chatglm` 的条目。

### Added

- **路由实况重做：请求逐帧飞行。** 三列（调用方 / 网关 / 候选账号），请求是一枚
  沿真实路径移动的光点，命中时节点转绿、空心环飞回，失败时转红、折返换下一个候选。
  实现上不用 CSS 过渡：位置每帧由 JS 按弧长算出来，帧循环只在有飞行时运行，
  静止的页面不占任何帧；**飞行时长由真实延迟决定**——慢请求真的飞得慢。
- `.impeccable.md`：本项目的设计上下文（用户、品牌个性、审美方向、五条设计原则）。

### Installer

- `client2api-setup-0.5.1.exe`（空包，不含账号凭据或本机配置）
- SHA256：`863732BEF714FCA55D51E2BE0B2A544E87E72341A0740340083C886833FBA1C9`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。

## [0.5.0] - 2026-10-28

### Fixed

- **ZCode 的活动套餐不再需要验证码，程序现在能自己领了。** 客户端弹窗里那个
  「ZCODE GLOBAL BUILD · 300,000,000 tokens」原来只能手动点「领取」——程序侧一直判定
  「没有验证码就没法领」，直接放弃。实测（2026-10-28）厂商已经不校验验证码：
  带一个不存在的 plan id 去问，返回的是 `1001 套餐不存在` 而不是 `3007 验证码失败`；
  完全不带 `X-Aliyun-Captcha-Verify-Param` 头的领取请求被正常接受。所以那段「先铸造
  token 再领」的前置条件是在拒绝一个本来能成功的请求——这正是弹窗挂在那儿没人领的时候。

  现在：`claim` 正常发出；只有手上已经有 token（面板浏览器刚铸造过、或还在缓存里）或
  配置了 solver 时才附带；没配 solver 的安装不再为它花任何成本。厂商若恢复校验，
  配了 solver 的账号照常工作，没配的会拿到 `3007`，面板原样转述。

- **设备指纹是风控真正在读的东西。** 领取时返回 `3012「账号或网络环境存在异常」`
  并不是账号问题：实测同一账号、同一凭据，用程序自己生成的 device_mid 被拒，
  换成 ZCode 桌面客户端自己的 id（`~/.zcode/v2/telemetry-state.json`，也就是弹窗
  「领取」按钮发出去的那个身份）几秒后就领到了。现在设备 id 优先采用桌面客户端的，
  并且领取路径遇到 `3012` 时会切到该 id 重走一遍（多花一次往返，之后不再重复）。
  聊天路径上的 `3012` 语义不变，仍是出口 IP 级别的风控。

## [0.5.0] - 2026-10-28

这一版不加新平台，做的是路线图里定下的两条：**失败转移的统一决策表**和**界面的整体质感**。

### Changed

- **失败转移的判断收进了一张表。** 「要不要换账号」和「要不要换平台」原来是两个函数、
  写在两个文件里（`core.Retryable` 和 gateway 的 `modelRefusal`），各自演进。结果是同一个
  故障（WorkBuddy 11158）在 0.4.4 和 0.4.5 各修了一次——补了一处，另一处还漏着。
  现在 `internal/core/failure.go` 里有一张 `FailureKind → 动作` 的表，一次回答三个问题：
  换账号、换平台、是否记为账号级裁决。每个 kind 都必须有行、有理由，新增 kind 漏配
  会让 `TestEveryFailureKindHasAnAction` 直接失败。
- **`modelRefusal` 里那条 `FailureOther + 404` 的兜底删掉了。** 它是 0.1.11 修
  「model not found」时留下的：那时候模块还没法表达「这个站不服务这个模型」，只能靠
  gateway 猜状态码。现在模块自己说：trae / kilocode / openrouter / minimaxcode / lobsterai
  把自己的 not-found 映射成 `FailureModelRefused`（不换账号、换平台）。
  WorkBuddy 刻意保持原样——它的 404 是「端点偶尔缺失」的瞬时故障，要换账号并冷却 60 秒，
  和「模型不存在」是相反的处理。同一个状态码在两个站上意思相反，所以状态码不该是输入。
- **界面重做了一层质感。** 降饱和的色板、发丝线分层、表面一道极浅顶光、全站等宽数字。
  总览那个 10px 粗的亮绿圆环改成 4px 细弧，且**正常状态用中性强调色**——一切正常时
  不该用最饱和的颜色喊出来，颜色留给异常；账号健康那条通栏绿条改成 6px 分段量表。
- **平台筛选条按「有没有账号」分档。** 原来 22 个平台平铺，每个都挂红点和「0/0」，
  没用过的平台也在喊错。现在有账号的带状态点计数排前面，没账号的只留灰名字。
- **账号页统计带不再折行。** 九个格子原来挤成 7+2 两行、右边空一大片；改成始终一条，
  放不下时整条横向滚动。
- **账号表的行内动作可读了。** 之前按钮被压成「账/测/停/删」单字：某一轮给 `td.c-acts`
  写了 `width:1px`，在固定布局里盖过了 colgroup 的 22.5%，整列真的塌成 1px。
  现在列宽交回 colgroup，按钮排成两列。

### Added

- `core.RotateOn` / `NextPlatformOn` / `ParksAccount` / `FailureActionReason` / `KnownFailureKind`：
  决策表的读取接口。`Retryable` 保留为 `RotateOn` 的同义封装，模块侧无需改动。
- 六个决策表测试：词表完整性、两个问题互相独立、未知 kind 回落到不重试、裁决归属。

### Fixed

- **定时任务的时点画成了日轨。** `9, 21` 这种写法原来只能在表格里读；现在同一份数据
  画成一条 00:00→24:00 的轨道，亮点就是会触发的时刻，按间隔跑的任务画成整段。
  改输入框时轨道实时重画。修了首版把「有明确时点也有默认间隔」的任务误画成「全天」的 bug。
- **运行记录从 11 列宽表改成时间线。** 原来 9 列里大半是 0，真正要读的失败原因被挤在
  最右边一格；现在只画非零的计数，原因单独一行给足宽度。
- **模型页 14 列 → 6 列主行 + 可展开详情。** 393 行乘 14 列，其中九列是大多数时候不用
  改的数值，一屏看不到三个模型；模型名还被输入框挤到几十像素，「Auto/gpt-6-astra」折成
  「Aut o…」。现在主行只留模型 / 客户端 / 来源 / 上下文 / 价格 / 详情，十一个可编辑字段
  收进点开的详情面板（网格排布，标签由 data-field 提供），编辑逻辑一行未改。
- **积分页「剩余天数 20754913 天」。** `CreditPackage.ExpiresAt` 的单位在模块之间不统一：
  zcode 转发厂商的 unix 秒，minimaxcode 存毫秒，面板一律 ×1000，毫秒值被再乘一千，
  算出来是五万年后。面板改为按数值判档（1e11 秒 ≈ 公元 5138 年，真实秒级时间戳都远
  小于它），两种单位都能正确换算；`core.CreditPackage` 上把这个契约缺陷写清楚了。
- **客户端能力矩阵。** 14 列乘 20 行全是绿底「是」方块，没账号的平台还标红「未就绪」。
  改成勾号 + 极淡横线，状态分三档（未接入 / 就绪 / 未就绪），红色只留给真的出问题的那一档。
- **对话页头部与空状态。** 标题被两个下拉挤成两行；空状态用 12vh 顶开、悬在半空。
  头部分成「标题区（可截断）」与「控件区（可换行）」，空状态在可用高度里真正居中。
- **卡片标题与说明文字糊在一起**（「平台路由配置设置模型路由组…」）：拉开间距、压低一级、
  限宽截断，标题和右侧按钮不再被说明挤到第二行。
- **平台筛选条按「有没有账号」分档。** 22 个平台原来每个都挂红点和「0/0」，没用过的
  平台也在喊错；现在有账号的带状态点计数排前面，没账号的只留灰名字。
- **账号页统计带不再折行**（九格原来挤成 7+2 两行、右边空一片），**行内按钮不再被压成
  单字**（某轮给 `td.c-acts` 写的 `width:1px` 在固定布局里盖过了 colgroup 的 22.5%）。

### Installer

- `client2api-setup-0.5.0.exe`（空包，不含账号凭据或本机配置）
- SHA256：`DFD61932DCBEBBF3F18F588F60799832CA551D2287B25F570ACB26234B92F707`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。

## [0.4.7] - 2026-10-27

### Fixed

- **账号池所有列都能升序和降序了。** 之前只有「账号 / 状态 / 模型 / 优先级 / 到期」这五列能
  双向排序；「余额 / 成功失败 / 在途 / 用量 / 最近成功」这五列点第二次是清空排序、第三次又回到
  降序，**升序永远点不出来**。原因是点击的三态循环被写死成「升序 → 降序 → 撤销」，而没跟着每列
  自己的默认方向走：默认降序的列从降序进循环，第二步就落到了撤销，正序被整段跳过。
  现在循环统一为「默认方向 → 反方向 → 撤销 → 默认方向…」，默认方向只决定从哪一头进，
  十列都能双向排序，第三次点击回到原始顺序。
- **表头的无障碍状态不再报错方向。** 之前任一列在降序时，其余九列表头也会被一起标成
  `aria-sort="descending"`，命中的那列还固定报 `ascending`，读屏软件会把整张表念成十列都在降序。
  现在只标记真正排序的那一列，并按实际方向报 `ascending` / `descending`，其余为 `none`。

### Added

- **排序状态机有了行为级测试。** 原来的断言是拿源码字符串做匹配，把错误的表达式逐字钉在测试里，
  所以这个 bug 一直没被拦住。现在把 `accSortFor` / `accSortDefaultDir` / `syncAccSortHeader`
  抽出来用 Node 真跑：逐列连点五次断言完整循环、显式断言升序与降序都可达、验证换列会重新按
  新列的默认方向起头，并对三种表头状态逐列核对 `aria-sort`、高亮和箭头符号。
  没有 Node 时这些用例会跳过而不是假装通过。

### Installer

- `client2api-setup-0.4.7.exe`（空包，不含账号凭据或本机配置）
- SHA256：`DC1F84921E4DFBABEB913447299A06D6DA03E2C8C328B83C22732528269F31CA`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。

## [0.4.6] - 2026-10-27

### Fixed

- **测速不再把“中转站的刷屏窗口”当成生成速度。** 0.4.5 把 tok/s 改成「首个内容分片 → 末个内容分片」的窄窗口，
  但中转站并不逐 token 下发：实测 OfficeAce 上的 DeepSeek V4.1 Flash 把 110 个输出 token 攒成 11 个分片，
  在 190ms 内一气吐完，于是一个实际 ~100 tok/s、整体耗时 1.1s 的平台被算成 580 tok/s；
  后续几次碰到 26ms 的窗口，直接报出 4000-5000 tok/s，排序变成了“谁的缓冲更大”。
  现在 tok/s 用「请求发出 → 最后一个内容分片」的整体窗口（含排队与首字等待），
  与用量页的 tok/s 口径一致，也就是调用方真正感受到的速度；没有任何内容分片时仍然显示“-”。
  探针的回答仍然是计数到 30（max_tokens 256），因为很多中转站不带 usage，不计数就拿不到 token 数。

### Added

- **测速口径有了确定性测试** —— 探针的时钟改为可注入，测试能精确规定每个事件到达的时刻，
  直接断言 tok/s 的分母是「请求发出 → 最后内容分片」的窗口，不再依赖墙钟落在哪个时钟刻度里。
- **0.5 路线图** —— `docs/ROADMAP.md` 记录了以可观测性优先、随后完善拒绝码矩阵的下一步计划；
  README 同步链接。附带一次性探针工具从默认构建面移出（`tools/`），并为 README 版本号、
  CHANGELOG Installer 块和探针构建面新增了防回归测试。

### Installer

- `client2api-setup-0.4.6.exe`（空包，不含账号凭据或本机配置）
- SHA256：`2C7F314FAE6A4D2E1D79B88A36653589D455056C1D02EAFD3BFADC614908720D`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。

## [0.4.5] - 2026-10-27

### Fixed

- **路由测速改测生成速度** —— 之前探针只要一个 3 token 的小回复，再用「总耗时」做分母，排队和首字等待全算进去了：100+ tok/s 的 DeepSeek 会显示成 1-3 tok/s。现在探针要一段更长的回复（max_tokens 提到 256），测的是「首个内容事件 → 流结束」的生成窗口，排队和首字单独显示；整个回复落在一个时钟刻度里时显示 "-" 而不是拿首字噪音充数。
- **模型组遇到「该账号档位不允许此模型」（code 11158）正确切换平台** —— 之前被归为普通客户端错误，模型组链路停在第一个平台不再尝试后面 8 个；现在归类为模型级拒绝（按模型短时停车，不冤枉账号），链路自动走到能服务该模型的平台。
- **中转站只报上下文上限不报实测大小时，同平台裁剪重试** —— 之前直接拒绝整个请求；现在把命名上限当作裁剪依据，在同一平台裁剪重试一次，不再跳过。

### Installer

- `client2api-setup-0.4.5.exe`（空包，不含账号凭据或本机配置）
- SHA256：`54955F4DAC448F2AA0B5C7F2BAE242B7C33B207395EE4C565E009B0E371D2E42`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。

## [0.4.4] - 2026-10-27

### Added

- **中转站请求次数限额规则** —— 中转站编辑框新增表单化的“请求次数限额”，可按每个 Key
  或整个中转站配置最多次数、秒/分钟/小时/天周期，以及滚动窗口、自然周期、首次请求
  起算三种重置方式；自然周期支持时区和每天重置锚点，多条规则可叠加生效。单 Key 达到
  限额会自动换下一把 Key，整个中转站达到限额才返回忙。计数落盘到
  `data/<source>/request_limits.json`，重启不会误重置；旧的 `requests_per_minute*`
  配置会自动迁移为规则，面板不需要输入 JSON。

### Fixed

- **WorkBuddy 11158 用户类型模型拒绝现在会跨平台失败转移** —— 厂商返回
  `model ... is not allowed for user type ...` 时，账号不再被误判为普通客户端错误并
  卡住整个模型组；网关将其视为模型级拒绝，继续尝试同组其他平台，不冷却健康账号。

### Installer

- `client2api-setup-0.4.4.exe`（空包，不含账号凭据或本机配置）
- SHA256：`85CB433C82D389B2C6D08940BB7F1A7ACED81CE21DD66C6BA95B5EE8CC418FE0`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。

## [0.4.3] - 2026-10-27

### Fixed

- **Raccoon 每日 300 积分：网关签到现在真的能领到了。** 深挖（CDP 抓包 + 全账号账本比对）定位到根因：
  该奖励要求请求携带 `X-Client-Device-ID`，而 token 导入方式添加的账号没有这个字段——携带它的账号
  每天都被自动发放（4/4 相关性），不携带的一律 `granted:false`。模块加载账号时现在会为缺失
  `device_id` 的凭证铸造并持久化一个稳定 ID，重启后首个请求就带上该头。

### Installer

- `client2api-setup-0.4.3.exe`（空包，不含账号凭据或本机配置）
- SHA256：`9155F2E0D60975BF3B62575BB3FDFFBEECA17040786D205E8797966F4E480BD2`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。
## [0.4.2] - 2026-10-26

### Added

- **中转站对话协议 `api_format: "responses"`** —— agentrouter.org 这类站点的模型
  只提供 OpenAI Responses 接口，`/chat/completions` 直接 400「请改用
  /v1/responses」。来源配置新增 `api_format`（面板编辑框「对话协议」下拉）：
  选 `responses` 后对话走 `/v1/responses`——system 文本进 `instructions`、
  工具调用映射为 `function_call` 项、工具结果映射为 `function_call_output`、
  流式事件（文本/摘要推理/函数调用/用量）完整回译；轮换、冷却、RPM、会话
  亲和全部照常。已对 agentrouter.org 实测：扫描 + 流式对话全链路打通。

### Installer

- `client2api-setup-0.4.2.exe`（空包，不含账号凭据或本机配置）
- SHA256：`BA1C3F503623B80BC29FABE261057CB8BC9D7B14C78FC2AF83CFF69EF0413E59`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。
## [0.4.1] - 2026-10-26

### Added

- **中转站单独 User-Agent** —— 个别站（如 agentrouter.org）按编码客户端白名单放行
  `/v1/*`，通用 UA 一律 401 `unauthorized client detected`（扫描、对话、甚至不带
  key 的请求都一样）。中转站配置新增可选 `user_agent`（面板编辑框同步），填它期望
  的客户端 UA（例如 `codex_cli_rs/0.0.1`），平台级设置对全部账号生效，账号级可覆盖；
  留空保持本网关默认 UA。顺带修复：面板里给账号填的 `extra_headers` 之前从未
  传到扫描/对话请求上。

### Installer

- `client2api-setup-0.4.1.exe`（空包，不含账号凭据或本机配置）
- SHA256：`D3A1FFA516F910C9A12E257BF62A11519E4893ED5920BB1B77846BBB59573AF2`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。
## [0.4.0] - 2026-10-26

### Added

- **中转站（openaicompat relay）独立出口代理** —— 每个中转站配置里新增可选
  `proxy`（`http` / `https` / `socks5` / `socks5h`，支持 `user:pass@`）。配置后，
  这个中转站发出的**全部**请求——对话、扫描模型、余额查询、账号测试——都走它，
  其它平台不受影响；留空表示沿用网关的全局代理。面板的中转站编辑框、配置
  校验和文档同步支持。

- **模型库分页与客户端筛选** —— 模型动辄八百多行，整表一次画完会把页面卡死；
  模型库改为分页渲染（默认每页 20 行，可调），并新增按客户端的联想筛选
  （combobox，带清除按钮），大列表不再拖垮面板。

### Fixed

- **ZCode 405/3012 的真正根因：system 数组形状**。用官方请求做单变量差分重放
  锁定了规则：system 块数上限 3；第 0/1 块必须与官方逐字节一致（改一个词、
  去掉行首换行都 3012；追加可以）；单独发第 1 块也 3012；最后一块内容自由。
  此前带调用方 system 消息的请求会追加第 4 块，正是这个形状触发了持续
  「unusual activity」。现在 powered-by 行和调用方 system 文本都折叠进
  最后的环境块，块数永远 ≤ 3；`system_test.go` 把这些不变量钉成测试。
  （此前怀疑的 header / 签名 / TLS / metadata 均实测无关。）

- **Windows 检出下的版本守卫测试** —— `packaging_test.go` 的版本行正则在
  CRLF 工作区必然失败（Go regexp 只认 `\n` 行尾），匹配前先归一化换行。

### Installer

- `client2api-setup-0.4.0.exe`（空包，不含账号凭据或本机配置）
- SHA256：`2A1F6D303D629D3A5A81B8D0E1342A30E06E70A0B1A4C6C8B93C6D33AF534EF2`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。

## [0.3.9] - 2026-10-25

### Changed

- **控制台交互与信息架构重做** —— 导航由 11 项平铺收成 4 组（总览 / 资源 / 流量 / 系统），
  账号池与平台告警带实时计数徽标；新增**总览页并作为默认落地页**（可用率环、可点的
  「需要处理」清单、KPI、账号构成条、网关流量与快捷入口）；**命令面板 Ctrl/Cmd+K**
  一次输入跳到任意视图 / 客户端 / 常用动作；账号池首屏的状态指标条直接就是筛选器，
  当前客户端没有该状态时空态会列出确实有该状态的其它客户端。

  账号池行高在 1440px 下由 159px 降到 69px，一屏可看的账号从 2 个提升到 5~6 个；
  保存条改悬浮不再遮住最后一行；宽表可横向滚动时右缘出现渐隐提示；能力矩阵常态
  「是」降为安静勾选；计数为 0 的告警格不再发红；用量占比保留一位小数。

### Fixed

- **路由：进入候选平台前先做悲观检查**，不再让一个已饱和的平台吃下一发注定失败的
  请求。路由器在把请求交给某个候选之前，先问三个本地信号——模块自己的 Health 判定、
  可选的按模型可用性探测（停用常常是「账号+模型」粒度，账号清点表达不出来）、以及
  网关自己的在途计数（max_in_flight / max_in_flight_per_account）。不能服务的平台
  直接跳到下一个优先级，裸模型不再为每个饱和的兄弟平台付一行失败候选。
  链条里的最后一个候选**永远不做预检**：本地判定可以重排链路，但不能拒绝请求。

- **WorkBuddy 的按模型可用性探测** —— 账号停用常常是按「账号+模型」粒度的（该模型的
  上下文守卫拒了它），模块的 Health() 无法表达。现在模块可以暴露一个按模型的可用性
  探测，路由预检和账号池都会用它。

- **面板的账号池在途计数与网关实际执行的口径一致** —— 网关引入统一的在途追踪存储，
  面板的「在途 / 上限」读的就是网关真正在执行的那组数字，不再各自估算。

- **Raccoon 的每日 300 积分不再被标记为失败请求** —— 厂商只给自己家桌面客户端发放
  延期，网关重放同一鉴权请求会被 granted:false 拒绝（账号、设备号、客户端版本、Origin、
  cookie、UA 都试过）。动作现在结算为「不可用，请到桌面客户端领取」，积分台账照旧
  读取，不再把整天锁死。

### Installer

- `client2api-setup-0.3.9.exe`（空包，不含账号凭据或本机配置）
- SHA256：`BE649298E95783893E9E697772BEFD54480C68A23E2CDE2D6C48FE356142E72B`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。

## [0.3.8] - 2026-10-25

### Changed

- **控制台整体重构为「精密仪器」工作台：账号池一屏放得下的账号从 2 个提到 5~6 个。**
  账号池实测改版前 1440px 下每行 **159px**、375px 下 **188px**，根因是操作列只占 10% 宽（约
  115px），6 个行内按钮被迫竖排成 3 行。操作列放宽到 22.5% 且按钮改成 3 列网格后，行高降到
  **69px / 99px**。同时统一了语义令牌与明暗双主题、把二级标签改成分段控件、能力矩阵的常态
  「是」降为安静勾选、计数为 0 的告警格不再发红，并修掉三个既有缺陷：用量占比用整数四舍五入
  把 0.4% 印成 0%、弹窗标题与分段标签叠成两行、以及「添加账号」表单用的 `.fld` 一直没有样式
  （早期只写了 `label.fld`）。接口原值 `ready / no` 显示为「就绪 / 未就绪」，模型 id 与账号
  标识等数据值保持原样。

  不改 API、字段、路由与业务行为；11 个视图在 375/760/1024/1440/2200 五档视口均无页面级
  横向滚动，`go build ./...` 与 `go test ./...` 全绿。设计说明见
  `docs/superpowers/specs/2026-10-25-panel-workbench-ui-refactor.md`。

### Fixed

- **ZCode 聊天一直回 405 / envelope code 3012 的真正原因修掉了。** 厂商风控校验的是
  **system 数组的形状**，不只是文本。官方 3.16.3 客户端固定发 **3 个块**，并把
  「- You are powered by the model named <model>.」这行写在**环境块内部**。
  模块此前把这行**单独追加成第 4 个块**——同样的文本、同样的请求头、同一张凭证，
  只是多一个块，就会被判为异常活动并返回 **HTTP 405 + code 3012**（与风控/WAF 同形）。
  又因为模块把 3012 归类为 FailureWAF，还会触发共享的 IP 级门禁，把机器上其它平台
  一起停掉。现在该行**折进最后一个前导块**，块数与官方客户端一致。

  验证方式：把官方客户端 3.16.3 的真实请求从 `~/.zcode/cli/rollout/model-io-*.jsonl`
  取出对拍，并用部署里真实的 plan JWT 逐字节复现——3 块 = 200，追加第 4 块 = 405/3012。
  修复后两个模型（GLM-5.3-Flash、GLM-5.3）端到端均返回正常流。

- **自动压缩（context guard 折叠）不再因为候选链选错平台而退化成静默裁剪，也不会再把一个
  工具往返切成两半。** 总结调用此前固定用 `candidates[0]`：WorkBuddy 按模型级账号停用，
  它的 `Health()` 表达不出来，于是同一段历史，正式回合失败转移到兄弟平台后成功，总结调用
  却停在停用的平台上失败、又退回静默裁剪——调用方永远拿不到 compaction 项，只能反复重发
  同一段超长历史。现在总结调用走与正式回合相同的候选链（平台 / 账号级裁决才失败转移，
  内容策略或畸形请求原样返回），并且分块只按「整段工具往返」切分、发送前跑同一遍
  `RepairToolPairing` / `RepairToolArguments`：贪心的按大小分块会把 assistant 的工具调用
  声明和应答它的 tool 结果分到两个 chunk，总结调用带着孤儿 tool 结果发出去，会被厂商以
  code 11148（"tool calls and tool results do not match"）拒绝。

### Installer

- client2api-setup-0.3.8.exe（空包，不含账号凭据或本机配置）
- SHA256：`B0ADAD0F19824CF15130C9229C3E47452B4A2F5BD947BA9E6E2397F324CAB748`
- 升级会保留现有 configs/client2api.json、data/、账号池、用量和任务数据。

## [0.3.7] - 2026-10-25

### Fixed

- **对话测试发送后不再抛 `ReferenceError: chatCurrentThread is not defined`。**
  对话工作区上线时，`settle()` 依赖的 `chatCurrentThread()` 在一次改动里被整段删掉，
  但它仍被调用。请求一结束（无论成功还是失败）都会走进 `settle()`，于是：
  - 控制台报 `ReferenceError`；
  - 更严重的是，抛错发生在写回记录之前——**每一轮对话都没有真正保存**，
    刷新或切换会话后历史是空的。
  现在补回该函数，并新增一条静态回归：扫描聊天脚本里所有 `chat*` 函数定义与调用，
  任何「调用了但没定义」的名字都会让 `go test` 失败，而不是等到用户发消息时才在浏览器里炸。

### Installer

- `client2api-setup-0.3.7.exe`（空包，不含账号凭据或本机配置）
- SHA256：`07580FA55B8105B3F5D6338B91FFA51BC34FC1DAC990D9E559EC7DF1EA0A0A93`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。
- 顺带修正 README 里滞后到 0.3.5 的版本号。
## [0.3.6] - 2026-10-25

### Fixed

- **凭证被厂商拒绝时显示为账号异常，而不是冷却中。** 覆盖 Cline、CodeArts、Gemini、QwenWork、WorkBuddy、OfficeAce 等平台：401 是关于凭证的结论，不管响应体是厂商业务信封、裸 token expired，还是边缘网关的 HTML 页；能刷新的模块会先试一次刷新，**只有刷新令牌本身被拒**才算终结（超时、5xx、网关错误不算）。被拒的凭证停在那里等操作员重新导入，而不是每次轮换又被选中；成功一次即自动恢复，面板的恢复也始终能手动解除。

### Installer

- client2api-setup-0.3.6.exe（空包，不含账号凭据或本机配置）
- SHA256: B0FAD72AB48606C87D436EC515F257CD28FD7B3B939276A85BD9C5C50BDD838D
- 升级会保留现有 configs/client2api.json、data/、账号池、用量和任务数据。

## [0.3.5] - 2026-10-24

### Fixed

- **liteLLM 风格的超限拒绝不再变成死 400。** 网关拿到厂商拒绝里的两个数字后，会按比例裁剪并
  **同请求重试一次**：「你这次用了 N tokens」和「窗口是 M」。liteLLM 挡在好几个平台前面，
  而它的措辞把**窗口写在前面、实测写在后面**：

  ```
  This model's maximum context length is 1048576 tokens. However, you requested
  1263369 tokens (1163369 in the messages, 100000 in the completion).
  ```

  旧解析器只匹配到前者「maximum context length is N tokens」就返回，实测值读成 0；而重试以
  「两个数字都在」为前提，于是根本没触发——用户只拿到一个只能手工处理的 400（例如 `raccoon`
  的 `litellm.ContextWindowExceededError`）。现在两个数字**各自独立**地在整段文本里查找，
  不假设先后顺序；实测值必须严格大于窗口值才算数，避免把窗口自身当成测量；只有窗口、
  没有实测的拒绝仍然只学窗口、不编造测量。

### Installer

- `client2api-setup-0.3.5.exe`（空包，不含账号凭据或本机配置）
- SHA256：`6A358887FF5CB0DD69ECC6E70AB52FBEF29463CC6D1D39B738CF24F7F110C0B0`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。
## [0.3.4] - 2026-10-24

### Fixed

- **Raccoon 账号补齐桌面端身份，不再被上游当成匿名 Web 调用方。** 之前网关发出的
  `User-Agent` 停在 1.0.35 的裸字符串，也没有 `X-Client-Device-ID`：厂商的桌面前端会
  从 `navigator.userAgent` 里回读 `Raccoon-Platform` / `Raccoon-Version` /
  `Raccoon-Device-ID` 再来拼 `X-Client-*`，缺了这几个标记，请求身份和真正的客户端对不上。
  现在对齐已安装的 1.0.38 桌面版：

  - `User-Agent` 换成真实的 Electron 串（Chromium + `raccoon-ai/1.0.38`），并按平台/
    版本/设备拼上同名的身份标记；
  - `X-Client-Version` 跟随升到 `v1.0.38`；
  - 每个账号首次进入账号池时补发一个 v4 UUID 作为 `X-Client-Device-ID`
    （等价于桌面端的 `desktop-device-identity.json`），已经存过的账号保留原值，
    重启不会换身份；
  - `user_info`、模型目录、余额、账单这些读取请求现在也带上平台/版本头，和聊天
    请求保持一致。

### Installer

- `client2api-setup-0.3.4.exe`（空包，不含账号凭据或本机配置）
- SHA256：`4142A65FB643DB747F9E14681ADEDB6616C5BC4F3247497DFDFAD7C500F22C05`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。

## [0.3.3] - 2026-10-24

### Added

- **对话测试页重做成聊天工作区。** 左侧是对话记录栏（新建、按标题或内容搜索、按最近更新
  排序、重命名、删除、清空全部），右侧是主聊天区：客户端/模型选择留在顶部，账号、
  `max_tokens`、温度、上下文窗口、流式、系统提示收进可折叠的「高级设置」；消息左右气泡
  布局、流式输出边出边滚、输入框固定在底部，Enter 发送 / Shift+Enter 换行、可停止生成。
  每个会话独立保存完整消息，继续追问会自动带上该会话之前的多轮上下文。记录持久化在
  `data/panel/chat_history.json`（`core.WriteFileAtomic` 原子落盘），换浏览器、清站点数据、
  搬走 `data/` 都还在；新增 `GET/POST/PATCH/DELETE /panel/api/chat/conversations` 系列接口，
  没有 `DataDir` 时退化成内存存储。语音转写的 `response_format` 也挪到录音面板同一张卡片里，
  修掉了转写模式拿不到该设置的隐患。

### Fixed

- **把工具名直接写成标签的调用不再漏成正文。** 上游还有第三种写法：不用 `<invoke
  name="exec_command">`，而是直接 `<exec_command>`，参数作为子元素
  （`<parameter name="cmd">…</parameter>`）。0.3.1/0.3.2 认的是 DSML 竖线那一族，
  这种写法完全没被识别，整段又落成普通正文，回合以 `tool_calls=0`、
  `finish_reason=stop` 结束，Codex 收尾等用户再敲一次，工具从未执行。现在解析器会拿到本次
  请求声明的工具名列表，只有**名字确实在列表里**的元素才当成调用；普通正文里的 `<section>`、
  `<div>` 之类不会被误吞；被截断的 `<exec_command>` 同样在流收尾时用已完成的 `parameter`
  还原。
- **同一条消息的正文只计一次。** 调用方可能把同一段文字以字符串或内容数组
  （`input_text` / `text` 块）发出，入站解码会把数组里的文本块同时写进 `Content` 和 `Parts`，
  估算器过去两处都算，等于把正文按双倍计。后果是裁剪阈值被提前触发：厂商实测约 42 万 tokens
  的会话，网关按约 110 万 tokens 的估算在 1048576 的窗口上反复裁掉最早的消息，而这段历史
  本来能完整放下。现在只有 `Content` 为空、文本块是正文唯一副本时才计分。
- **五个模块不再把正文发两遍。** 同一个形状还让 Raccoon、Autoclaw、MiniMax Code、Doubao 和
  Gemini 在上游报文里把正文重复发送；这些模块改为优先用 `Content`，仅在其为空时回退到文本块。
- **每日余额刷新跳过已停用账号。** 定时读取不会让停用账号恢复，只会反复触发厂商拒绝、
  刷日志。
- **LobsterAI 的登录与续期对齐桌面客户端。** `keyfrom` 改成复用桌面端的获取归属值而不是
  自己按当前时间造，凭据字段与导入导出同步调整。
- **OfficeAce 的短信验证码真的发出去了。** 登录页是 Angular SPA，手机号控件由框架挂事件；
  同步填值后立刻点击会赶在下一次 digest 之前，点击被静默丢弃、短信根本没发，验证码读取器
  只能干等一个永远收不到的号码。现在填值后先让 digest 跑完再点，并以页面渲染出的
  「重新获取 (NN)」作为真正受理的凭证，没出现就重试一次。

### Installer

- `client2api-setup-0.3.3.exe`（空包，不含账号凭据或本机配置）
- SHA256：`D525E9B84F5FF2ED9B46C5737D9A3008BA026504EC2C613C031AAA035C81728A`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。
## [0.3.2] - 2026-10-24

### Fixed

- **被截断的 DSML envelope 不再当成正文里的一段。** 0.3.1 已经能认「丢掉 `DSML` 标签、
  只剩全角竖线」的 envelope，但上游还有一种更残缺的写法：`invoke` 开了头却没有结尾，
  流直接停在最后一个参数上（没有 `</｜｜｜ invoke>`、也没有 `</｜｜｜ calls>`），
  参数名还可能写成元素自己的属性（`<｜｜ parameter="cmd">` 而不是 `name="cmd"`）。
  旧代码认不出这种形态，整段又落成普通正文，回合以 `tool_calls=0`、`finish_reason=stop`
  结束，Codex 收尾等用户再敲一次，工具从未执行。现在：
  - 流的收尾阶段会把已经发完的 `parameter` 直接解析成调用，不再要求那个永远没来的 `</invoke>`；
  - `parameter="cmd"` 这种「名字写在元素自己的属性里」的写法也能读出参数名；
  - 只有带全角竖线的 `invoke` 才按 envelope 处理，普通正文里的 `<invoke ...>` 不会被吞；
  - 一个调用都恢复不出来时仍按「协议标记不完整」让该回合失败，而不是静默收尾。

### Installer

- `client2api-setup-0.3.2.exe`（空包，不含账号凭据或本机配置）
- SHA256：`5D8EE5F540B3EFEBA332EC9157F290F3E770389FF139B5EB97F45E97FA5B7DFB`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。
## [0.3.1] - 2026-10-23

### Fixed

- **去掉 DSML 标签的工具调用不再被当成正文。** 有的上游会把 envelope 里的 `DSML`
  标签丢掉，只留下全角竖线，标签变成 `<｜｜ calls>` / `<｜｜ invoke ...>`，闭合标签
  还可能写成 `</｜｜｜>`（比开标签多一根）。旧的正则要求必须带 `DSML`/`antml` 标签
  才认得出来，于是这种写法整段落成普通正文：网关回一个没有 `tool_calls`、
  `finish_reason=stop` 的「干净」回合，Codex 结束回合等用户再敲一次，工具从未执行。
  现在标签可选、竖线长度不严格，参数也接受裸竖线闭合标签兜底；只有全角竖线算
  envelope，ASCII `|` 仍然当普通文本。
- **余额读取不再依赖「碰巧有一次 Test 或聊天顺手续期」。** AutoClaw / Raccoon /
  LobsterAI / Trae CN 的余额读取现在自己保证令牌新鲜：AutoClaw 与 Raccoon 在令牌进入
  续期窗口时先续期，被厂商以鉴权错误拒绝时再续期并重试一次；LobsterAI 对 200 响应体里
  `code != 0` 的拒绝也重试一次；Trae CN 走与聊天、签到相同的 `ensureToken` 路径。
  之前令牌过期后面板一直显示「未登录」，直到某次无关的测试或聊天把它顺手续期为止。
- **Raccoon 余额读取改在续期后重读账号。** `byID` 返回的是记录副本，续期写回的是池里的
  那份；不重读就会继续拿刚触发续期的旧访问令牌去请求上游。
- **AutoClaw 与 Raccoon 的账号记录补发顶层 `expires_at`。** 后台空闲续期扫描
  （`cmd/client2api/renew.go`）读的是 `AccountRecord.ExpiresAt`，字段为空就跳过，
  于是这两个平台的所有账号都被静默排除在主动续期之外。现在它们和别的平台一样进入扫描。
- **Raccoon 签到不再把「这一次被拒」当成「今天结束了」。** 那个桌面端 grant 接口不是发积分，
  是**延长**当天 300 积分的有效期：厂商 00:00 发的 `daily_grant` 当天 24:00 过期，桌面端登录
  成功后再写一条 `daily_grant`，`expire_at` 推到 7 天后。原实现把 `granted:false` 直接当成
  当天定论（`Skipped`），面板于是把该行动标成 `unavailable` 并禁用按钮到下一个上海午夜——
  早上被拒一次，那天剩下的机会就全废了。现在改查账本（最近 200 条）来判定：今天有
  `daily_grant` 且到期日晚于今天才算真已签；只有当天到期的基础发放、或账本读不到，都保持
  可重试，绝不凭猜测锁死当天。

### Installer

- `client2api-setup-0.3.1.exe`（空包，不含账号凭据或本机配置）
- SHA256：`24C6C428D5319994FFCC0647BC58EEDFC782134B80C2E684C1874D6BAA8DDA80`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。
## [0.3.0] - 2026-10-22

### Added

- **上下文守卫改为「先让厂商裁决」（`features.probe_before_trim`，默认开启）。**
  过去网关只凭自己的 token 估算就把历史裁掉再发上游，估算偏保守时，厂商本可以
  接受的请求被无故缩短。现在对于从未实测过的 `(平台, 模型)`，超预算的请求第一次
  会原样放行，由厂商决定：
  - 厂商接受 → 该规模成为这个模型的**已证实下限**，以后同样大小不再被裁剪；
  - 厂商以超限拒绝 → 走既有链路：学习真实窗口 + 按实测比例裁剪 + 同一次请求内重试一次。

  探测结果是**黏性**的，每个 `(平台, 模型)` 只探测一次，持久化在
  `data/observed_context_windows.json`。模型列表新增探测状态列：只要该模型已经有
  上下文长度（厂商目录 / 学到的窗口 / 手工填写），默认显示 `已探测`；真正验证过则显示
  `已探测 · 接受 N` 或 `已探测 · 拒绝`。「清除探测」写入一条 cleared 墓碑，把该模型
  变回 `未探测`，下次超预算请求重新探测。探测只放宽裁剪预算，**不改**通过 `/v1/models`
  下发的广告窗口，也不会越过面板里手工设置的上下文长度。

  需要退回旧的「先裁剪」行为时，在 `configs/client2api.json` 里设
  `"features": { "probe_before_trim": false }` 即可；面板设置页有同一个开关，热生效。

- **超预算时的顺序：探测/学习 → 压缩 → 裁剪。** 该 (平台, 模型) 还没学习过时，先把请求
  原样发给厂商：接受就把这个规模记成已证实下限，拒绝就 `learnContextWindowFromFailure`
  学真实窗口并按实测比例裁剪重试。已经学习过的模型才先尝试 auto-compact——折叠后的请求
  才是实际发出去的那份，调用方同时收到携带摘要的 `compaction` 项，一轮都不丢。
  只有压缩失败（报错、摘要为空、blob 无法签发）时才做有损裁剪。

- **路由测速改为并发 + 流式返回。** 过去「测速」按平台顺序一个接一个地调，最慢的
  平台会拖住整张表；现在每个平台并发发起，服务端按**完成顺序**用 SSE 逐个推送，哪个
  先回来先显示，慢平台仍显示「测速中」而不阻塞其余结果。

- **粘性会话支持一键强制切换。** 粘性会话弹窗新增「一键强制切换」，对当前列出的**全部**
  会话执行「自动选择」：清除账号与平台绑定，下一次请求重新排名平台和账号。执行前有
  确认提示说明影响范围，返回真实的「已切换 / 总数」，单条失败只跳过并计数、不计入成功。

- **模型列表新增上下文探测状态列。** 显示每个 `(平台, 模型)` 的探测结论
  （`未探测` / `已探测 · 接受 N` / `已探测 · 拒绝`），并提供「清除探测」把该模型变回
  `未探测`，下次超预算请求重新探测。

### Fixed

- **用量总览的输入 tokens 看起来少了一截。** 大数磁贴用 23px 等宽字体直接画完整数字时，
  卡片 `overflow:hidden` 会把十亿级的值截断成 `10,926,808,5…`，读起来像统计少了一位。
  现在输入/输出/合计改用 `B/M/k` 紧凑单位显示，完整精确值放在悬停提示里。
- **用量走势图跨天坐标轴像时间倒序。** 72 小时 / 7 天窗口的 x 轴只写 `13:00 / 01:00 / 17:00`，
  省掉日期后看起来是乱的；超过 24 小时的窗口现在每个刻度都带月日。
- **用量走势图的柱高可能与合计 tokens 不一致。** 输出段不足 1px 时用 `Math.max(..., 1)`
  会抬高总柱高，让合计更高的点反而更矮；现在总柱高严格等于 `total_tokens` 比例，可见性
  用单独的 1px 顶帽解决，并在悬浮读数里补上「合计」。
- **工具调用标记被当成正文或静默丢弃。** 模型把工具调用写成可见文本时，网关现在在公共
  事件层统一归一化：完整的 DSML/XML 调用转成结构化 `tool_calls`（Chat Completions 和
  Responses 都适用），不完整或无法解析的标记让该回合失败，而不是把协议语法当正文透出。

### Installer

- `client2api-setup-0.3.0.exe`（空包，不含账号凭据或本机配置）
- SHA256：`8EA27523D8A0A0A820ED43A306B98ACF043A1C9A4C6FE3FFBA7BABF30D533788`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。

## [0.2.0] - 2026-10-21

### Fixed

- **Raccoon 短信登录补齐「业务会话确认」，新登录账号和旧裸令牌账号都能自愈。**
  IAM 的 `/api/iam/v1/auth/login/sms` 只发身份令牌；厂商自己的 web 客户端拿到令牌后
  还会 `POST /api/web/auth/v1/iam/confirm` 把这张令牌换成可用的业务会话。漏掉这一步时
  `/api/web/points/v1/balance`、`/api/web/auth/v1/user_info`、`/api/web/llm/v2/model_catalog`
  全部回 `HTTP 400 / code 200901 iam_confirm_required`，面板上就表现为「余额 not fund、
  什么都用不了」，账号还会以匿名裸令牌入库（没有昵称、手机号、refresh token），在池子里
  显示成一串 `pu-…`。现在登录在读取资料之前先确认业务会话；`do()` 另外会在遇到
  400/200901 时确认并**重放一次**，所以这条修复之前存下的账号也会自愈。
- **强制切换后被在途旧请求重新粘回原平台。** 旧请求已经选中 Open Code、但还没拿到上游
  首帧时，用户点了「强制切换」；旧请求稍后成功又调用一次平台绑定，把刚清掉的粘性写了
  回去，下一轮继续走旧平台。现在每个会话带一个切换 generation，强制切换和手动固定都会
  推进它；旧请求的成功回调只能写回同一 generation 的路由，不能再覆盖操作员的最新选择。
- **模型组自动压缩用组名请求上游而失败。** Responses 压缩需要的是候选平台实际能服务的
  具体模型 id，不能拿调用方写的模型组名去调模块；否则该平台的每次自动压缩都会报
  `ErrUnsupported`，最终退回静默裁剪。现在压缩显式携带候选模型 id。
- **自动压缩撞上本地并发上限时过早放弃。** `max_in_flight_per_account: 1` 的平台可能因为
  用户自己的并发轮次返回 `ErrBusy`；这不是上游终态。自动压缩现在最多重试 3 次、间隔
  200ms，只等待本地背压，真实上游错误仍立即返回。

### Installer

- `client2api-setup-0.2.0.exe`（空包，不含账号凭据或本机配置）
- SHA256：`20589325805A89CB3B39A5492EBF3FAEF86E6C4FA3D4CA1978C192CA352ED37D`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。

## [0.1.99] - 2026-10-21

### Changed

- **Raccoon 的短信登录整条搬到 IAM 服务，一键接码关闭。** 厂商把小浣熊的短信登录从
  `/api/web/auth/v1/*` 迁到了 `/api/iam/v1/*`，并换了字段名和信封：
  `POST /api/iam/v1/sms/send`（`captcha_token` + `scene=login`）返回 `verification_id`，
  `POST /api/iam/v1/auth/login/sms` 用该 id 加 `verification_code` 登录。旧的
  `/send_sms` 与 `/login_with_sms` 已经废弃——**任何**请求都回 `100006 图形验证码校验失败`，
  这也正是“点自动添加就报错”的真正原因。适配器现在同时解析两套信封（旧 API 的数字 `code`
  与 IAM 的字符串 `code`），旧接口的数字含义原样保留，所以 `100006` 的判定没有被改坏。
  扫码登录、token 刷新、余额、模型目录仍在旧 web API 上，均未受影响。
  面板「短信登录」标签页一并修好，人工拖滑块后可用。
- **Raccoon 的「一键接码 / 自动登录」已从能力位移除。** 厂商加固了阿里云滑块场景，
  traceless 快路径不再自动通过：实测用真浏览器、在厂商自己的域下、同一会话、带齐
  cookies，取到的参数一律被判 `captcha_token invalid`，厂商自己的登录页现在也要人拖滑块。
  无人值守跑不通，于是不再声明 `core.AutoLoginProvider`，面板因此收起该按钮，不会再误点。
  手动接码（租号 / 取码 / 释放）保留不变。随之下线的配置项有 `browser_path`、
  `browser_headless`、`auto_login_timeout_seconds`、`sms_polls`、`sms_interval_seconds`
  ——旧配置带着它们仍能正常加载（未知键被忽略），只是不再生效。
- **CodeArts 的限流识别与倍率目录。** 厂商的拒绝不走 HTTP 状态码：每分钟 token 上限以
  **HTTP 200** 落在 SSE 帧里（`error_code: InferHub.ModelArts.8111x.429`）。`openStream`
  现在读第一帧再交给调用方：token 上限按**限流**处理（冷却该账号、上报网关、换下一个账号），
  队列码保持独立，其余拒绝原样透传给调用方且**不归罪账号**。模型目录开始读取厂商的
  `credit[].ratio_display` 倍率（`0.18x` 等），并按厂商自己的时间表解析促销档位
  （`deepseek-v4.1-flash` 白天 `0.18x`、21:00–07:59 为 `0.09x`），未给出时间表的档位不假定生效。
- **Opencode 内置目录补齐上下文窗口与输出上限。** 这些数字不只用于显示：网关按模型宣称的
  窗口决定裁剪，目录里缺窗口的模型无法被裁剪，超长请求会原样转发并被厂商以不带数字的 400
  拒绝。凡是 models.dev 给出上限的 id 现在都带上该上限，免费 id 也不例外。

### Fixed

- **逐模型的估算校准，修掉 Autoclaw 被反复裁剪的问题。** 字符启发式对不同厂商的 tokenizer
  误差方向都不一样：实测 Autoclaw 少算约 8%（厂商 861713 / 本网关 931735），而 WorkBuddy
  多算约 2.3 倍（厂商 1477004 / 本网关 628884），一个全局常数不可能同时正确。现在每次成功
  调用都用厂商自己的 `prompt_tokens` 与本网关对同一份字节的估算配对，按 `(平台, 模型)`
  把比值记在 `data/estimate_calibration.json`，并以 0.5 的权重平滑、限制在 0.5–4.0 之间。
  该校准只作用于**守卫的预算**，用量行和 `/v1/messages/count_tokens` 报告的仍是针对请求本身
  的估算。Autoclaw 的 8% 过读原本正好吃掉守卫预留的 8% 安全边界，这正是一直被裁剪的原因。
- **Responses 的上下文裁剪改为返回压缩项，不再静默丢历史。** 静默裁剪对调用方看起来就是一次
  普通成功，于是下一轮原样重发同样的超长历史，再被裁一次，如此循环。现在守卫**本会**裁剪时，
  网关改为折叠历史并返回一个 `compaction` 项，把这条信号交给调用方；若压缩跑不起来，
  守卫原有的裁剪仍然生效、该轮照常成功。只有 `/v1/responses` 有这条通道——Chat 与 Anthropic
  协议没有压缩词汇，那里仍以 `context_window_exceeded` 回报，交由调用方处理。
- **面板账号页的模型清单改用实时目录。** 账号池的模型列与弹层过去读 overview 里的
  `Status.Models`，而那可能只是模块的静态兜底列表（trae 即如此），于是与「模型与档位」页
  显示两套模型名。现在两处共用 `/panel/api/models` 的实时目录（60s 节流、并发合并、失败不写入），
  模块上架新模型后不必手动「重新获取」即可跟上。

### Installer

- `client2api-setup-0.1.99.exe`（空包，不含账号凭据或本机配置）
- SHA256：`EA4FAFBFF1CB10E5605D24483F2E7815BBADD05A51B36382DD9B5B73862D4041`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。

## [0.1.98] - 2026-10-21

### Fixed

- **CodeArts 权益模型不再把本来能用的模型毒死。** `deepseek-v4.1-flash` 这类模型过去被硬编码
  当作“免费权益”模型，请求会签名带上 `maas_type: benefit`。但权益通道是**按凭据逐账号开关**的：
  厂商的 `/v1/benefit-gateway-config` 返回 `{"enabled":false}` 时，任何带该头的请求都会被拒绝，
  报 `InferHub.4004.200: benefit not found`——而同一个模型**不带**这个头时能正常出字。更糟的是这份
  名单本来就取不到：`/api/v1/gateway/config` 现在要求签名，裸调返回
  `401 APIG.0301 x-auth-token not found`，于是失败后总是回落到那份硬编码猜测。
  现在权益判定改为**以厂商为准**：先签名查询通道开关，通道关闭（或名单为空、查询失败）时
  **没有任何模型被当作权益模型**，请求不再带 `maas_type`；只有厂商明确列出的 id 才签名带上该头。
  新增 `policy_url` 配置项（对应厂商的 policyCentre 域名），并删除了那份硬编码的“猜测免费”名单。

- **Raccoon 的空工具 schema 会让整条请求 400，且真实原因被吞掉。** 两个独立缺陷：
  1. SenseNova 的 schema 校验器拒绝**缺少 `parameters`** 的函数声明，报
     `"" is not valid under any of the schemas listed in the 'anyOf' keyword`。Codex 的动态/MCP
     工具（如 `mcp__java`）可以合法地不带 schema，于是整个请求被拒。适配器现在为缺失、`null`
     或空对象 `{}` 的 schema 补上规范的空对象 schema。
  2. 厂商的 400 用的是嵌套结构 `{"error":{"message":...}}`，而模块只解析顶层 `message`，
     真实原因被降级成无信息量的 `HTTP 400: Bad Request`；现在嵌套的 `error.message` 会直接透出。

### Installer

- `client2api-setup-0.1.98.exe`（空包，不含账号凭据或本机配置）
- SHA256：`A39300B85E21BDCF2FC789C2FC5A2F63985B0122BAE69157696BB51829F645D7`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。

## [0.1.97] - 2026-10-21

### Added

- **Responses 压缩（compaction）完整实现。** 新增 `POST /v1/responses/compact`：
  把一段对话折叠成交接摘要，返回 `object: "response.compaction"`，`output` 为保留的轮次
  加**恰好一个** `type: "compaction"` 项。`POST /v1/responses` 新增
  `context_management: [{"type":"compaction","compact_threshold": N}]`：估算输入达到 N 个
  token 时自动折叠，并把该项附加到本次输出；流式按 `response.output_item.added` →
  `output_item.done` → `response.completed` 的顺序携带它。
  - `encrypted_content` 是**本网关自有**的摘要 blob，用 `data/compaction.key`
    （升级保留）做 HMAC-SHA256 签名。回放到本部署会校验并还原成 `assistant` 历史；
    签名不通过（别的部署、被篡改、换机器）的 blob 直接丢弃，不会被当成明文回放；
    OpenAI 真正的密文与中继的明文摘要行为不变。
  - 摘要走正常模型路由，账号轮换、平台故障转移、用量统计一致；摘要完全不可用时返回由保留轮次
    构成的可对象，而不是 500。

### Fixed

- **提示超限的 400 没有给出"去压缩"的信号。** 上游以"内容太长 / 超过最大上下文 / 请运行
  compaction"回绝时，若模块没有分类，过去会落到 `invalid_request`，客户端不知道该压缩，
  于是拿同样的超长请求反复重试（OpenCode 200K 上下文切模型时最常见）。
  现在：
  - `clients/opencode` 新增 `kindContext`，命中 prompt-size 措辞即映射为
    `FailureContextWindow`，保留厂商原始 400，且**不计入账号惩罚**（请求形状问题，不是凭据问题）；
  - 网关侧新增 `looksLikeContextRefusal` / `normalizeContextRefusal`：模块即使什么都没分类，
    网关也会按措辞把失败提升为 `context_window_exceeded`，这正是 Responses 客户端触发压缩的代码；
    带数字的拒绝继续让网关学到真实窗口，后续请求先裁剪再发送。
  - 措辞识别采用整句匹配（`prompt is too long`、`maximum context length`、`run compaction`
    及中文对应），避免把无关的 400 误判成压缩触发。

- **Raccoon 工具定义中的 `\0` 正则转义会让上游返回 HTTP 400。** 某些 MCP 工具（例如
  `capture_process_scenario`）的 `propertyNames.pattern` 含 ECMAScript 的 `\0` 转义；
  SenseNova 的 tool-schema 校验器会直接拒绝。raccoon 适配器现在发送前递归移除该转义，
  保留其余约束，工具的运行时校验不变。

### Installer

- `client2api-setup-0.1.97.exe`（空包，不含账号凭据或本机配置）
- SHA256：`DE75DAD13E1077019628B99B3CB67862F9D14E30A4F77AF581738A323A7B1258`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。

## [0.1.96] - 2026-10-21

### Changed

- **MiniMax Code 的模型目录改由厂商实时接口决定。** `GET …/mavis/api/v1/models` 匿名返回当前的
  `models`、`model_order` 和 `whitelist`；模块以 `models` 为唯一来源，按 `model_order` 排序，
  不再把只存在于 `whitelist`、但厂商已不再定义的 id 重新上架。`Models()` 继续即时返回缓存，
  并在后台按五分钟节流刷新；面板的“刷新模型”会直接请求厂商。接口不可达时依次回退到本机
  `~/.minimax/config.yaml` 和内置表，失败时保留已有目录并在错误中说明，不会清空模型列表。
- **M3.1 的模型 id 更新为 `MiniMax-M3.1-Flash`。** 厂商已于 2026-10-20 去掉 `-Preview`
  后缀；示例配置同步使用新 id。已下线的 `MiniMax-M2.7` / `MiniMax-M2.7-highspeed`
  只保留在离线兜底表中。

### Fixed

- **Responses 重放历史里的工具调用不再因参数不是 JSON 而整请求失败。** Codex 的
  `custom_tool_call` 把自由文本放在 `input`，而没有 `arguments`；过去转成 Chat 历史时得到空字符串，
  严格上游会以 `Assistant tool call function.arguments must be valid JSON` 拒绝。现在自定义工具调用的
  原文以 JSON 对象保留，缺失参数写成 `{}`，损坏的历史参数统一保留在 `_raw` 下；转义前后都能无损重放。
- **所有候选共用同一份可解析的工具参数。** 网关在轮换账号和故障转移前统一修复 assistant 工具调用的
  `function.arguments`，避免一个损坏的历史轮次让整条优先级链上的模型全部失败。

### Installer

- `client2api-setup-0.1.96.exe`（空包，不含账号凭据或本机配置）
- SHA256：`D6D08E0CB86F9570BD65764C931FD0EFE63AB3A587E296F4E0A4251065C13574`
- 升级会保留现有 `configs/client2api.json`、`data/`、账号池、用量和任务数据。
## [0.1.95] - 2026-10-20

### Fixed

- **「本机 CLI 登录」显示成功但账号池仍然是空的**。这是同一个流程里最深的一处缺陷：CLI 在厂商接受登录时
  会打印 `Login successful! Welcome, <name>`，读取线程一度把这行**当成成功**并立刻置为终态；而登录轮询
  一旦看到终态就直接返回，**永远不会去读凭据、也永远不会写账号**。于是面板报「登录成功」，`accounts.json`
  却从头到尾没被动过。

  现在这行**只记录账号名**，不再表示成功；`success` 只在一处产生 —— 账号真正写入账号池之后。回归测试锁死：
  收到 `Login successful! Welcome, …` 之后状态必须仍是 pending（否则就等于跳过了存账号）。

  综合 0.1.93～0.1.95，这条登录链路上的三处时序缺陷现已全部关闭：
  1. 完成信号以**凭据文件**为准，而非控制台文案；
  2. 子进程先退出时仍会收养已落盘的凭据（2 秒宽限 + 失败终态复查）；
  3. 控制台成功行不再提前宣告成功，从而不再跳过存账号。

### Installer

- `client2api-setup-0.1.95.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。
- 之前几次登录留下的凭据仍在 `data/qoder/cli/.staging/` 下，升级后再点一次「本机 CLI 登录」并确认即可。
## [0.1.94] - 2026-10-20

### Fixed

- **「本机 CLI 登录」成功后仍无账号**。上一版修掉了"只认控制台文案"，但还有一个更隐蔽的时序问题：
  CLI 的 `login` 子进程在浏览器确认的**同一瞬间**退出，而凭据文件也在那一瞬间落盘。退出处理抢先一步把
  会话标成 `failed`（"登录进程已退出，可能是设备码过期或被取消"），轮询看到失败就停了 —— 而磁盘上
  其实躺着一份完好的凭据。现在：
  - 子进程退出后先给凭据落盘 2 秒的宽限，确认磁盘状态再判失败；
  - 轮询在任何"失败"终态下都会先检查凭据是否已写入，**写入即以磁盘为准完成登录**，不再相信子进程的
    退出时刻。
  两个方向都有回归测试：凭证存在 + 子进程已退出 → 必须成功；凭证不存在 + 子进程已退出 → 必须失败。

### Installer

- `client2api-setup-0.1.94.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。
- 0.1.92/0.1.93 那次登录留下的凭据仍在 `data/qoder/cli/.staging/` 下，升级后重新点一次登录即可被正确收养，
  或直接复用该目录。
## [0.1.93] - 2026-10-20

### Fixed

- **「本机 CLI 登录」不再卡在「请稍后」**。厂商在浏览器确认后立刻写好凭据
  （``<configDir>/.auth/user``），但登录轮询必须先匹配 CLI 控制台那句
  ``Login successful! Welcome, …`` 才肯去看凭据；一旦措辞没对上，已经成功写盘的登录就永远停在
  pending，面板一直转圈。现在以**凭据文件本身**作为完成信号，控制台文案只作日志。同一个流程上还
  修掉两处连带缺陷：
  - 成功路径只写了提示文字、没有标记终态，于是即便账号已入库，面板读到的仍是 pending；
  - 登录成功后会把暂存目录改名为账号目录，但随后的检查仍指向改名前的旧路径，误报「没有写出凭据文件」。
  三处都已修复，并有回归测试锁死（凭证存在但无控制台文案时必须成功；凭证不存在时必须继续等待）。

### Installer

- `client2api-setup-0.1.93.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。
## [0.1.92] - 2026-10-20

### Added

- **Qoder：一次「本机 CLI 登录」即可拥有全部能力**。以前只有浏览器登录能拿到积分和签到所需
  的设备令牌，CLI 登录只能提供两个免费模型。现在 CLI 登录成功后，模块会读回它自己写下的凭据
  （`<configDir>/.auth/user`，用该目录的 `machine_id` 派生密钥加密），把 `dt-` 设备令牌、刷新令牌、
  厂商用户 id 和到期时间一并写回账号行。结果是同一个账号一次登录就有：
  - 全部 14 个模型（免费两个走 CLI 常驻进程，其余走签名网关）
  - 积分/余额（`GET /api/v2/quota/usage`）
  - 每日签到与活动任务（`/sash/api/v1/me/campaigns`）

  桌面客户端因此不再是必需品。
- **凭据解密复用厂商自己的运行时**：解密例程编译在 SDK 的 WASM 里。模块不重写这段 KDF，而是内置
  一个助手脚本（首次使用时写到网关数据目录 `qoder-decrypt.mjs`），由同一个 `Qoder CN.exe`
  （`ELECTRON_RUN_AS_NODE=1`）执行。不引入第三方运行时，不复制厂商文件，也不影响 SDK 升级。
- **CLI 令牌自动跟随**：CLI 自己会续期令牌，所以余额和签到在读取前会重新取 CLI 当前持有的凭据，
  长期运行不会用到过期的副本。

### Fixed

- **免费模型的排队不再被当成故障**。厂商在繁忙时会把 `dfmodel` / `qfmodel` 放进队列，并持续上报
  `model_queue_status`（实测排队 4000+ 个请求、预计等待 4 分钟以上，上限 1 小时）。以前桥接会把这
  当成「没有输出」，静默等到沉默上限后杀掉 worker —— 而现在：
  - 排队状态会透传到调用方，以「排队中，前面 N 个请求，预计 …」的形式出现在推理通道；
  - 沉默上限会随厂商上报的等待时间自动延长到其队列上限，真正卡死的 worker 才会被回收。
  这也解释了此前 `dfmodel` 间歇性返回 `400 [FAIL]node:oa_qwen-plus-main msg:Execution failed: null`：
  那是厂商侧排队/过载的表现，不是签名或凭据问题。
- **CLI 登录不再依赖 CLI 控制台的成功文案**。以前用正则匹配 `Login successful! Welcome, …` 来判断
  登录是否完成，厂商改一次措辞就会让已经成功的登录永远停在 pending。现在以凭据文件是否可读为准，
  文案只作为日志。
- **凭据读取的身份判定更稳**：`--probeTimeout`（默认 20 秒）对 Electron 冷启动偏紧，导致账号 id
  退化成名称哈希、同一账号被拆成两行。改为独立的 60 秒预算，且登录后一律以厂商用户 id 作为账号 id。

### Added（同期其它平台）

- **Trae 裂变邀请码**：支持把 `code_fission` 活动的邀请码绑定到新登录的账号上
  （`clients.trae.invite_code`，可选 `invite_activity_id` 覆盖活动 id）。厂商对同一台设备只接受
  一个被邀请人（业务码 9022），所以当配置了邀请码时，每次带码登录会生成新的 `machine_id` /
  `device_id` 组合；该行为默认开启，仅在有邀请码时生效，不影响普通登录。
- **LobsterAI 邀请码**：`invitation_code` 会填进门户登录页的邀请框，并随同一次登录提交
  （该门户没有独立的兑换接口，`/api/auth/callback` 同时接收凭据与邀请码）。留空时登录链接不变。

### Installer

- `client2api-setup-0.1.92.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。
## [0.1.91] - 2026-10-20

### Added

- **Qoder 免费模型改由本机官方 CLI 驱动**：厂商的推理边缘只把 `dfmodel`（DeepSeek-Flash）和
  `qfmodel`（Qwen3.8-Flash）这两个零倍率模型放行给官方运行时，第三方请求无论 COSY 签名多忠实都会
  收到 `400 [FAIL]node:oa_qwen-plus-main msg:Execution failed: null`。`clients/qoder` 现在对这两个模型
  改为驱动厂商自己发布的 agent CLI（`@qoder-ai/qoder-cn-agent-sdk`，stream-json 协议），其余 12 个
  模型仍走原来的 COSY 通道，互不影响。CLI 可执行体就是已安装的 Qoder CN 客户端
  （`ELECTRON_RUN_AS_NODE=1`），不需要额外装 Node，也不会往客户端安装目录写任何东西。
- **每账号独立 CLI 运行时**：每个账号一个 `--config-dir`，落在网关自己的 `data/qoder/cli/<账号>/`，
  与操作员本机的 `~/.qoder-cn` 完全隔离；进程按「账号 + 会话」常驻复用（冷启动 5–13 秒，之后每轮
  约 2 秒），死亡自动重启，空闲 15 分钟回收，单账号 4 个、全局 16 个上限。会话第一次落到某个进程时
  发送完整历史，复用进程时只发最新一轮，避免上下文丢失或串话。
- **面板新增「本机 CLI 登录（免费模型）」入口**：面板在添加账号时给出两个登录方式，CLI 登录会启动
  CLI 自己的 `login` 子命令并把它打印的设备码链接显示在面板上，手机确认后账号落地；原来的「浏览器
  登录」继续负责积分、签到、任务与其余模型。两个入口解析同一个厂商用户 ID，同一个账号两种登录会合并
  成一行，两种能力都有。
- **平台页显示本机安装状态**：未安装客户端、已安装但没有账号登录过、以及已登录账号数量都会写在平台
  状态里，`CLIENT2API_QODER_CLI_PATH` 可指定安装目录，`clients.qoder.cli_reasoning_effort` 可设置
  默认思考档位（调用方自己的 `reasoning_effort` 优先）。

### Fixed

- **CLI-only 账号不再被令牌路径误伤**：只有本机 CLI 登录、没有设备令牌的账号不会再被送去调用需要
  令牌的接口（模型目录、余额、签到、任务）而吃 401 拉黑；这些接口对它如实报告不可用，仅保留两个免费
  模型能力。
- **优雅关闭清理 CLI 子进程**：退出时会结束所有常驻 worker 和未完成的登录进程；Windows 上按进程树
  结束（`Qoder CN.exe` 是启动器，真实 worker 在 `.qoder-versions/<版本>/` 下），不会留下孤儿进程。
- **worker 卡死有兜底**：某轮 2 分钟没有任何输出即终止该 worker 并按上游故障处理，不会让请求一直挂着。

### Added（其他平台同步改动）

- **Loomy 邀请码与兑换码**：账号页新增「全部绑定邀请码」，批量把邀请码绑定到启用的账号池；邀请码走
  `points/activation`，积分兑换码走 `points/redemption-codes/redeem`，两者在面板里是分开的两个动作，
  不会互相混用。自动登录、手机号登录和手动导入会话都会顺带绑定配置的邀请码（默认 `P276D2`）；
  已绑定其他邀请码的账号只做报告，不会被覆盖。
- **Trae 1024 签到抽奖**：每日签到在厂商开放时顺带完成 `checkin_1024` 活动的一次抽奖，并新增
  `checkin-lottery-1024` 作为补救入口，供当天已经签到过、但还没抽奖的账号补抽。
- **AutoClaw 验证码登录重试**：验证码登录遇到身份网关的瞬时 5xx 时，用同一手机号和同一验证码重试一次，
  不再直接丢弃已经送达的验证码去换号。

### Fixed（其他平台同步改动）

- **OpenCode 工具参数 schema 归一化**：缺省或可空的 `parameters`（`{}`、`{"type":null}`）补齐成
  `{"type":"object","properties":{}}`，避免上游报 `schema must be a JSON Schema of 'type: "object"'`
  直接拒绝整轮请求；调用方显式给出的 object schema 原样透传。
- **Raccoon 413 错误可读**：上游拒绝超大请求时不再回一句厂商模板话术，而是报出请求体积并说明该裁剪
  会话、图片、附件还是工具定义。
- **网关手动平台固定不再过期**：面板手动绑定的「会话 → 平台」在粘性 TTL 到期后不会悄悄失效，降级时
  也仍然作为硬约束生效；自动绑定仍按原规则过期。

### Notes

- CLI 桥接是纯文本通道：调用方声明的工具（function calling）不会转发，CLI 自带的文件/终端工具被
  显式禁用（`--tools ""`），图片输入也不支持；需要这些能力请使用其余走 COSY 通道的模型。

### Installer

- `client2api-setup-0.1.91.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。
## [0.1.90] - 2026-10-19

### Fixed

- **OpenCode 裸 HTTP 400 不再直接判死整轮请求**：Zen 偶尔会对一份完全正确的历史回 `HTTP 400` 且 **body 为空**，同样的请求隔一会儿就能成功。以前这种响应被归成「调用方的请求有问题」（`FailureOther`，不可重试），网关既不换账号也不换平台，一次抖动就把整个会话打断。现在 `clients/opencode` 遇到这种**空 body 的 400** 会花一次账号轮换再试（匿名 → 已登录，或下一个可用账号），轮换完仍失败才照原样上报。带错误类型或消息的 400（未知模型、字段错误）仍然是终态，其它状态码也不会被重试，所以调用方的错误不会被放大到整个账号池。

### Installer

- `client2api-setup-0.1.90.exe`（空包，不含账号凭据或本机配置）
- SHA256：`818662E089D37502242DE7ADAF3F0E6F24C774C2D2D5DAF774A7CD85B2363068`
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。
## [0.1.89] - 2026-10-19

### Added

- **统一工具调用配对修复**：网关在候选请求准备阶段统一执行 `core.RepairToolPairing`，所有平台、所有候选和回退路径共用同一套规则：工具结果归位到声明该 call id 的助手回合之后，残留结果与无结果调用会被清理，助手正文和推理内容保留。AutoClaw、OpenCode、WorkBuddy 各自的重复实现已移除。
- **模型组对外白名单兼容**：选中模型组后，调用方仍可按成员的精确 `平台/模型` ID 调用该组，但成员不会因此额外出现在 `/v1/models` 中；别名和无关裸名不会被顺带放开。

### Fixed

- **查看图片不再失败**：Responses 的 `function_call_output.output` 现在按联合类型解析，`input_image` / `image_url` 会保留为图片部件，`input_text` 保留为文本；Anthropic 的 `tool_result` 内嵌图片也做同样处理。多模态模型不再收到被压成 JSON 文本的截图。
- **AutoClaw 上游 500 参与网关级回退**：AutoClaw 的可重试 5xx 现在携带 `FailureUpstream` / 限流分类与账号归因，网关能够按模型组候选继续尝试其它平台，而不是把错误当成不可重试的普通响应。

### Installer

- `client2api-setup-0.1.89.exe`（空包，不含账号凭据或本机配置）
- SHA256：`B03765E9BCC275F5DA3DA14C0CC26A0F70EAF72C9DF7CC4DB44790DD8BC17483`
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。
## [0.1.88] - 2026-10-19

### Added

- **Loomy 文本模型不再收到图片**：Loomy 目录里没有声明 image 输入模态的模型（DeepSeek、Kimi 等）在发出前会把每个图片块替换成一行占位文本。这些后端把 base64 data URL 当普通文本读，一张截图就要几万 prompt token，几张就会以「内容太长了」直接拒绝；而编码 agent 的历史会每轮重发同一批图片，一次超限就会拖垮整个会话。目录声明支持视觉的模型不受影响；目录里认不出来的模型按文本模型处理。

### Fixed

- **失败原因列不再为空**：所有「流已经建立之后再失败」的路径（Chat / Responses / Anthropic 三个协议适配器，含 `/v1/responses` 的请求体解析失败）以前都只标记失败、不记原因，面板「失败原因」列因此一直显示 `-`。现在会写明：上游流提前中断、上游返回空回合、写回调用方失败、上游流返回错误，模块自带错误（例如 `autoclaw: upstream stopped sending data (idle timeout)`）原样透传。
- **缓冲响应不再吞掉模块的真实错误**：模块先发 `EventError`、再关流返回 `io.EOF` 时，原来的读循环会用 `io.EOF` 覆盖掉真实原因，把所有错误都压成「没有收到结束帧」；现在保留模块自己的错误，`io.EOF` 只表示流结束。
- **WorkBuddy 6004 按厂商给的时刻停用模型**：6004 限流消息自带的 reset 时间是绝对时刻（例如 `your usage will reset at 2026-10-18 21:43:41 UTC+8`），可能与模块自身两小时的软退避上限不一致。以前会把这个上限强加到带日期的窗口上，导致提前重试同一个配额窗口；现在带日期的窗口按原时刻生效，只有无日期的退避仍受上限约束并按连击翻倍。同时兼容尾部的 `UTC` / `UTC+8` 时区写法，不再只认硬编码的 ` UTC+8`。
- **OpenCode 工具调用与结果强制配对**：Zen 对「工具结果与它的调用不相邻」的历史会返回裸 HTTP 400 且 body 为空，无法从响应里定位。现在按归属重排（结果紧跟声明该 call id 的助手回合），历史里完全找不到调用方的结果直接丢弃（任何重排都救不回来），只有调用没有结果的助手回合保持原样。

### Installer

- `client2api-setup-0.1.88.exe`（空包，不含账号凭据或本机配置）
- SHA256：`6D8ADABBB1421E5A4170D37261E2270EAD534027A4FE00E9E6E98C464B8E22E7`
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.87] - 2026-10-18

### Added

- **OpenAI 兼容中转请求体自动规范化**：面向 DeepSeek / LiteLLM 系中转时，强制工具选择会降级为自动选择，空参数工具 schema 会补齐对象骨架，助手历史统一携带 `reasoning_content`；上游不再因为工具声明或思考模式回放形状直接返回 400。

### Changed

- **OpenCode 助手消息始终写回推理字段**：助手回合即使没有思考内容也发送空字符串 `reasoning_content`，其它角色不携带该字段；避免上游在思考模式下以裸 HTTP 400 拒绝多轮请求。
- **Auto 池路由预览不再误标 preferred**：`Auto/<模型>` 展开的平台保持按操作员优先级排序，拖拽调整后的顺序会真实反映在预览与保存结果中；显式点名具体成员时仍保留优先尝试行为。

### Fixed

- **工具 schema 不再以 `{}` 直发**：缺少 `properties` 的命名空间或 MCP 工具声明会补成合法对象结构，完整 schema 原样保留。
- **强制 `tool_choice` 不再触发不支持工具调用错误**：`required`、指定函数或对象形式统一降级为 `auto`，`none` 和原有 `auto` 保持不变。
- **Auto 池预览顺序与运行时一致**：修复预览把池内所有平台都标成 preferred，导致操作员看到的链与实际路由不一致的问题。

### Installer

- `client2api-setup-0.1.87.exe`（空包，不含账号凭据或本机配置）
- SHA256：`B47FEFC7E6B0BEB6BA0C8672DBBB15C8161AF6C5C912DC59A43C829FF1B55BCA`
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.86] - 2026-10-18

### Added

- **平台模型级优先级**：平台配置的模型清单新增“模型优先级”输入；该值按“模型 > 模型组 > 分时规则 > 平台基础优先级”的优先级链生效，路由预览、测速和拖拽排序会写回同一字段。留空表示继承。
- **自定义思考档位映射**：模型与档位页可为具体模型、`Auto/<模型>` 或模型组编辑映射，例如 `high→max`、`xhigh→top`；明确映射优先于候选模型自带规则，`top` 表示厂商实际最高档。
- **对话测试内部模型豁免**：面板对话测试与语音转写会携带内部标记，可列出并调用被 `exposed_models` 对外隐藏但网关仍可路由的模型；普通入站调用仍受白名单限制。

### Changed

- **不可用平台提前短路**：模块声明无可用账号时不再进入 Chat 空转账号池；单平台请求返回 503 `no_healthy_account` 和 `Retry-After`，免费模型仍保留账号额度耗尽例外。
- **OfficeAce 免费体验包改为尽力而为**：登录拿到可用模型凭据即视为成功；体验包下单改为立即支付（`auto_pay=1`），未支付订单等失败会记录并在余额/套餐读取时明确提示，不再由面板读操作隐式补单。
- **Qoder 对话显式携带 `x-model-key`**：按模型目录请求头发送，避免只靠 body 时上游使用账号默认模型且按不计费调用返回。

### Fixed

- **上下文裁剪不再留下孤立 tool 结果**：裁剪边界落在工具调用回合中时会保留完整调用与结果，避免上游以 malformed request 拒绝。
- **内联 base64 图片按图片计价**：上下文估算不再把截图 base64 当普通文本高估数万 token。
- **无明确数字的超长提示词增加保守重试**：Loomy 等内容过长拒绝没有 token 数字时，网关按已公布窗口的一部分裁剪一次后重试。
- **OfficeAce V3 状态码与错误提示**：security-token 接口兼容 `201 Created`；未支付订单错误会提示费用中心处理。

### Installer

- `client2api-setup-0.1.86.exe`（空包，不含账号凭据或本机配置）
- SHA256：`6C8D5CAA38BCBF4AB4C4D6F16DD09B23462D31FEFF1932FE3822FCB7BE563609`
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.85] - 2026-10-18

### Added

- **虚拟模型支持独立的“封顶对齐”设置**：`Auto/<模型>` 和自定义模型组现在和具体平台模型一样可以手动开启、关闭或恢复默认。虚拟行上的显式选择会统一作用于组内所有候选；未手动设置时仍沿用各平台模型自己的策略。
- **路由预览显示上下文窗口**：平台配置里的静态链路预览会为每个候选显示该模型实际生效的上下文窗口、来源和优先级；测速结果也保留这两项，便于在窗口容量不同的平台之间判断故障转移顺序。

### Changed

- **账号池模型倍率识别更完整**：模型明细现在识别更多厂商目录字段，包括计费倍率、当前生效倍率、促销免费、消费倍率和访问类型；目录没有公开倍率时明确显示“未公开”，客户端只提供分级时显示该分级，不再留空或误判。
- **带 realm / 提供商前缀的模型目录可以正确匹配倍率**：账号列表只显示裸模型名时，面板会按账号 realm 或唯一提供商前缀找回目录元数据；不同提供商存在同名叶子模型时不会跨账号借用倍率。

### Fixed

- **OfficeAce / 华为 V3 临时凭据兼容 HTTP 201 Created**：IAM security-token 交换接口返回 `201 Created` 时现在按成功处理，免费体验版开通和后续云端操作不会再因为状态码判断过严而失败。

### Installer

- `client2api-setup-0.1.85.exe`（空包，不含账号凭据或本机配置）
- SHA256：`EA00B4B780EE4B79C882D8D49AF6730B27C9A64A56B46A16B6FE33A787182E13`
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.82] - 2026-10-17

### Fixed

- **OfficeAce / 华为模型目录严格对齐客户端实际下拉**：只发布 `glm-5.2`、`glm-5.1`、`deepseek-v4-flash-0731`、`deepseek-v4.1-flash` 这 4 个模型。华为原始目录里的其他模型、套餐差异和别名字符串不再进入账号池或对外目录，模型开关与探活也只会看到这 4 项。

### Installer

- `client2api-setup-0.1.82.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.81] - 2026-10-17

### Fixed

- **OfficeAce / 华为重新发布 `glm-5.3-flash`，但线上请求自动映射到可用的 `glm-5.3`**：华为模型目录虽然列出 `glm-5.3-flash`，聊天接口却会对同一 ID 返回 `404 Invalid model`；网关现在保留 OfficeAce 客户端实际可选的公开模型名，只在发往上游时完成映射。模型扫描不再因为云端订阅凭据返回 `APIG.0602 security token has expired` 而整体失败；订阅查询仅用于区分 Ultimate，失败时退回标准模型列表，`glm-5.3-flash` 不会因此消失。
- **OpenCode 不再把 Zen 的 `server_error` 误报成 429 限流**：Zen 有时在端点暂不可用时返回 HTTP 429，但错误体属于 `server_error`，并非账号额度或速率限制。网关现在将其归类为上游故障并对外返回 502，不再给健康账号进入模型冷却。
- **Raccoon 探活不会再把只会思考的模型误判为账号异常**：16 token 探活现在使用顶层 `reasoning_effort: "none"`，明确关闭 SenseNova 默认 thinking；如果上游干净结束但只有 reasoning 或工具调用，也视为凭据有效。流中途发生错误时仍按失败处理。

### Changed

- **账号池的模型开关对话框补充倍率 / 免费标签**：模型列表现在显示平台目录中的当前生效倍率，免费模型和当前促销价会覆盖旧列表价；目录没有倍率信息时不会生成臆造标签。

### Installer

- `client2api-setup-0.1.81.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。
## [0.1.80] - 2026-10-17

### Fixed

- **OfficeAce / 华为云端凭据会提前刷新，并把 `APIG.0602` 当作可恢复的令牌过期**：临时凭据在到期前 10 分钟主动更新；上游返回 HTTP 400 且错误码为 `APIG.0602` 时刷新后自动重试。每个账号的刷新现在串行执行，并在锁内重新读取持久化会话，避免一次性 refresh token 被并发消费后把健康账号误判为认证失败。
- **华为自动添加账号只有在出租号码释放完成后才报告成功**：修复浏览器登录成功与号码释放之间的竞态，任务不会再出现“已经成功但号码仍未退还”的中间状态。
- **签到成功/跳过状态在面板各入口统一**：账号按钮、任务中心、任务队列、手动批量和定时调度现在都按实际执行的签到动作记录当天状态；调度报告把“已跳过”原因与“拒绝执行”分开，避免任务码和动作码不一致时重复签到或错误显示。

### Installer

- `client2api-setup-0.1.80.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.79] - 2026-10-17

### Fixed

- **WorkBuddy 国际版补齐真正的 VLM 会话指纹，`11-128 / request illegal` 不再把健康账号
  误判成认证失败**：对照官方 WorkBuddy AI 5.8.4 客户端后确认，国际聊天必须始终携带
  `X-Conversation-ID`；同一个会话还要复用稳定的 `acp-connection-id`，并发送官方的
  `X-Private-Data: true`、W3C/B3 trace 头以及
  `WorkBuddy/5.8.4 WorkBuddy/5.8.4 CLI/2.161.0`。国际聊天不再发送浏览器侧的
  Origin/Referer、机器 ID、会话 ID 或 `X-Agent-Type`。令牌本身没有失效，**不需要重新登录**；
  旧版缺失这些请求头时，上游会用同一张 400 响应拒绝请求。
- **补上回归覆盖**：同一账号连续两轮不显式传 conversation id 的请求必须复用同一组
  conversation/ACP id，同时固定 `11-128` 的 `content_blocked` 分类，避免再次显示
  `account_auth_failed` 或冷却健康账号。CN 的 5.7.7 / 2.137.1 行为保持不变。

### Installer

- `client2api-setup-0.1.79.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.78] - 2026-10-17

### Fixed

- **WorkBuddy 国际版账号不再被判「非官方客户端」**：官方的 WorkBuddy AI 5.8.4 桌面端
  换了指纹，国际（`global:`）账号现在按新指纹发送
  `WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0`、`X-IDE-Version: 5.8.4`，并补上官方
  主会话一直在带的 `X-Product: SaaS` 与 `X-Agent-Purpose: conversation`；计费和签到等非
  对话请求也改用 5.8.4。旧的 `WorkBuddy/5.7.7 WorkBuddy AI/5.7.7 CLI/2.137.1` 正是面板里
  「账号认证失败（HTTP 400）：`{"code":11-128,"msg":"request illegal"}`」的来源。CN 的
  5.7.7 / 2.137.1 指纹保持不变，配置里显式写了 `client_version` / `cli_version` 时仍然优先。

### Changed

- **每个出站聊天请求都会保证开头是一条 system 消息**：上游对首条不是 system 的会话回的
  也是 `11-128`（"first message is not system prompt"），所以只带 user 的直连调用、任务脚本
  和面板探针以前会被拒；现在缺什么补一条中性 system，客户端自带 system 时不重复补。
- **`11-128` 改判为内容类拒绝**：以前它被当成账号故障（`account_auth_failed`），会冷却一个
  完全健康的账号；现在触发一次降级 prompt 重试，并且失败提示变成可操作的
  `content_blocked`。
- 根 `README.md` 与 `clients/workbuddy/README.md` 同步了国际指纹的取值来源、realm 分版本的
  默认值和这次判定的取舍。

### Installer

- `client2api-setup-0.1.78.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。
## [0.1.77] - 2026-10-16

### Fixed

- **华为（内置 `officeace`）平台每次获取授权链接都会先退出上一个浏览器会话**：
  点 **浏览器登录 → 获取授权链接** 时，面板生成的链接会先经过华为的退出端点清除
  `auth.huaweicloud.com` 上的旧登录态，再跳回带原始 OAuth 参数的登录页。这样连续添加
  第二个账号时不会因为浏览器仍保留第一个账号的会话而直接提示登录成功，不用再手动退出。
  自动添加账号使用的无头浏览器登录流程同样复用这条链接，因此也自动隔离旧会话。

### Installer

- `client2api-setup-0.1.77.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.76] - 2026-10-16

### Added

- **华为（内置 `officeace`）平台的接码与自动添加账号正式定稿**：账号池的 **一键自动添加**
  会从共享接码平台租号、用无头浏览器完成华为手机号验证码登录，自动读取并提交验证码；
  成功后照常写入账号池、开通免费体验版并扫描模型，任务结束自动释放号码。面板同时保留
  手工接码（取号、取码、释放/拉黑）和 **浏览器登录 → 获取授权链接**。接码 token 支持
  面板按次粘贴，`EOMSG_TOKEN` 为进程级兜底；华为验证码默认发送方关键字为 **华为**，
  可按次覆盖代理、关键字、省份和卡类型。本版在 0.1.75 基础上重新打包，功能与 0.1.75
  一致，安装包版本号递增。

### Installer

- `client2api-setup-0.1.76.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.75] - 2026-10-16

### Added

- **华为（内置 `officeace`）平台支持接码和自动添加账号**：账号池新增 **自动添加**，面板会
  从共享接码平台租一个号码，用无头浏览器走华为手机号验证码登录，自动读取并提交验证码，
  登录成功后仍走原来的账号落库、免费体验版开通和模型扫描流程，任务结束自动释放号码。
  面板同时保留手工接码的四个动作（取号、查验证码、释放/拉黑）和原来的
  **浏览器登录 → 获取授权链接**。接码 token 可以在面板里按次粘贴，`EOMSG_TOKEN` 是进程级
  兜底；华为验证码的默认发送方关键字是 **华为**，支持按次覆盖代理、关键字、省份和卡类型。
  自动登录复用现有华为 OAuth 会话，取消、超时和驱动错误都会如实反馈到任务日志，账号绑定
  的号码不会被释放。
- **Gemini 登录/推理支持模块专属代理**：Google OAuth 和 Cloud Code 在直连不可达时可以
  单独走一个代理。面板的 Gemini 登录框新增 **专属代理** 输入，登录、刷新令牌和推理请求
  都使用它；留空继续使用全局代理或直连，保存后会写进该模块的配置。登录代理能力通过
  `login_proxy` 能力位暴露，其它模块不受影响。

### Changed

- **内置平台显示名改为「华为」**：平台 ID 仍是 `officeace`，账号、模型目录和
  `exposed_models` 配置都不受影响。

### Installer

- `client2api-setup-0.1.75.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.74] - 2026-10-16

### Fixed

- **RadeonCloud 账号「测试」不再误选 `Clef` 导致 400**：平台的 `GET /models` 把
  不可走 `/v1/chat/completions` 的辅助模型放在列表第一位（`Clef` 的
  `output_modalities` 是 `decision`），旧逻辑固定拿第一项做最小对话探测，于是密钥正常
  也会报「Model Clef is not a chat model」。现在扫描时单独记录明确支持文本输出的模型，
  「测试」优先从这些模型里选一个；完整模型目录和路由行为不变，`Clef` 仍可按需查看和
  选择。升级前已经扫描过的账号即使还没有这份元数据，探测遇到上游明确的
  「not a chat model」后也会自动尝试列表里的下一个模型，不必强制重扫。

### Installer

- `client2api-setup-0.1.74.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.73] - 2026-10-16

### Added

- **OfficeAce 对齐 WorkBuddy 的签到、余额和套餐**：内置的 `officeace` 平台现在会保存华为
  账号登录时拿到的云端临时凭据，因此可以直接使用模型网关自己的订阅接口。面板的**签到**
  按钮调用 `POST /v1/subscription/bonus/claim`，幂等，重复点击会提示今天的奖励已经领过；
  **额度/余额**显示 `officeace_points` 积分和套餐内的联网搜索次数；**套餐**把套餐配额和
  每个活动奖励拆开列出。凭据和结果只写在本机 `data/officeace/accounts.json`。临时凭据
  过期后优先用 OAuth refresh token 续期，其次用 IAM v3/STS 换发，登录完成后不需要
  OfficeAce 桌面端在线。
- **免费模型不再被余额/额度停用挡住**：`core.ModelIsFree` 统一识别零成本模型（模块读到的
  零倍率/零价格、`extra.free`，或名称、分组、ID 里的 `free` / `免费` 标记），路由层把
  免费标记传给账号池。因为余额不足、低余额或上游 402 被停用的账号仍然继续服务免费模型，
  不需要手工强制启用；免费模型被上游按额度拒绝时只停用该账号的这一个（账号, 模型）组合，
  账号的其它免费模型照常可用；限流、WAF、失效会话和熔断仍按原规则停用整个账号。Cline、
  CodeArts、OpenRouter、QwenWork、WorkBuddy 的池都接入这套判定，停用状态写入各自的
  `state.json`，重启后仍有效。
- **AutoClaw / Raccoon 接码与一键自动登录**：两个平台实现 `core.SMSProvider` 与
  `core.AutoLoginProvider`；面板可以租一次性号码、自动收验证码、完成手机号登录并落库，
  已有账号可以用原号码接码重登并就地刷新凭据。接码沿用 eomsg，token 为空即不启用。
- **Gemini 运行时模型目录**：模型目录改为读取厂商自己的 `fetchAvailableModels`（被拒时
  回退到配额接口里的模型 ID，再失败时保留上次结果），`-low/-medium/-high` 变体折叠成一行、
  用 `reasoning_effort` 选择，上下文和输出上限也读取厂商元数据；内置 Cloud Code IDE 的
  installed-app OAuth client，库存安装开箱即可浏览器登录。
- **MiniMax Code 登录补写 onboarding**：登录时调用
  `/minimax-cloud/api/v1/user/update_onboarding`，避免新账号能聊天但签到/额度板显示为 0。
- **路由预览增加实时测速**：面板可以对着静态路线链逐平台发一条极小请求，显示首包延迟、
  总耗时、完成 token 数和 token/s，便于对比不同线路。

### Fixed

- **OfficeAce 云端签名对齐客户端的 V11-HMAC-SHA256**：修正签名密钥派生
  （客户端使用 `HMAC(HMAC(AK, SK), scope || 0x01)`，`StringToSign` 为
  `V11-HMAC-SHA256 \n X-Sdk-Date \n scope \n sha256(canonicalRequest)`），并让带
  `project_id` 的账号同时签 `X-Project-ID`。签到和订阅查询不再被网关以
  `APIG.0301`（签名校验失败）拒绝。

### Installer

- `client2api-setup-0.1.73.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.72] - 2026-10-16

### Added

- **OfficeAce 内置为平台，不再要求手加中转站**：`officeace` 由程序直接注册，新装实例
  会出现在平台配置里，并为它保留账号池和模型扫描入口；配置文件仍可用同 ID 覆盖地址或
  `disabled` 禁用，不需要也不应再手工创建一条同名中转站。
- **OfficeAce 华为账号浏览器登录**：账号池新增真正的账号登录路径，面板按 OfficeAce
  客户端的方式完成 PKCE + DPoP 授权（state、授权码、IAM 临时凭据、权限校验、未开通时
  调用订阅接口），把云端返回的模型网关 App Key / App Secret 写进本站账号池并自动扫描
  模型。登录后不需要 OfficeAce 桌面客户端在线，也不再必须先探测本机凭据。
- **OfficeAce 账号级模型网关**：每个华为账号保留自己登录返回的 `model_api_url_base`，
  不同区域或模型的网关可以同时存在于 `officeace` 平台；面板扫描和其他账号操作不会把
  账号地址覆盖回平台默认地址。

### Fixed

- **本机凭据导入不再拒绝其他 OfficeAce 网关**：Windows 兜底导入会保留客户端记录的
  账号级网关地址；面板会明确提示该账号将走自己的网关，而不是要求两者地址完全一致。

### Installer

- `client2api-setup-0.1.72.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.71] - 2026-10-16

### Added

- **OfficeAce 一键导入客户端登录**：OfficeAce（Office Claw）中转站的账号池新增「导入 →
  探测本机凭据」。面板直接读取 OfficeAce 客户端自己的加密凭据库
  （`packages/api/.config/secure-config-nodejs`：安装密钥由 Windows DPAPI CurrentUser 托管，
  登录档案为 AES-256-GCM），取出华为账号登录后拿到的那对模型网关 App Key / App Secret，
  再按普通账号写进该中转站；导入前会比对客户端记录的上游地址和本站 `base_url`，不一致时
  明确报错而不是导入一组用不了的凭据。探测结果只包含账号标识和上游地址，不含任何密钥；
  客户端没装或装在别的机器上时，原来的手动填写 App Key / App Secret 路径不受影响。

### Installer

- `client2api-setup-0.1.71.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.70] - 2026-10-16

### Fixed

- **上下文守卫：上游实测超限时按实测比例裁剪并自动重试一次**：当启发式估算把请求算在窗口以内、
  上游按自己的 tokenizer 拒绝并给出 `N tokens > M maximum` 时，网关会先还原报错里被 `\u003e`
  转义的 `>`，同时读出「实测用量」和「窗口上限」两个数字，按实测比例把最旧的历史裁到窗口以内
  （system 提示和最新一轮永远保留），并在同一次请求里重试一次。裁剪同样写进用量行；连最新一轮
  都放不下时仍然返回 `context_window_exceeded`。这修掉了这类对话每次「继续」都必然 400 的问题。

### Added

- **OfficeAce（Office Claw）模型直连**：OfficeAce 客户端里的 GLM、DeepSeek、Kimi、Qwen 等模型
  本身走华为 MaaS / AgentArts 的 OpenAI 兼容接口，现在可以当普通「自定义中转站」接入：
  在面板里添加平台、把应用 Key / App Secret 填进账号池（`app_secret` 触发 HTTP Basic），
  扫描后以 `officeace/<模型名>` 对外发布。凭据只落在本机 `data/`，桌面客户端不需要保持运行。

### Installer

- `client2api-setup-0.1.70.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.69] - 2026-10-16

### Added

- **OfficeAce（Office Claw）内置模型直连**：OfficeAce 客户端里能选的 GLM、DeepSeek、Kimi、
  Qwen 等模型本身走华为 MaaS / AgentArts 的 OpenAI 兼容接口，现在按普通“自定义中转站”接入，
  不经过 OfficeAce 桌面端。账号池的 `API Key / App Key` 填应用 Key，高级字段 `App Secret` 填
  配套密钥，网关自动改用 `Authorization: Basic base64(app_key:app_secret)`；扫描得到的模型按
  `officeace/<模型名>` 对外发布，账号轮换、故障切换、会话粘性、流式输出、工具调用和用量统计
  与其它平台一致。
- **自定义中转站支持 HTTP Basic 应用认证**：账号池新增可选高级字段 `app_secret`，填了它就走
  Basic（用户名是 `api_key`），留空继续走 Bearer；旧账号和旧配置不受影响。
- **`exposed_models` 支持 `平台/*` 通配**：例如 `officeace/*` 一次开放该平台全部模型，
  不影响其它平台。

### Installer

- `client2api-setup-0.1.69.exe`（空包，不含账号凭据或本机配置）
- OfficeAce 不再以独立客户端模块发布；它的模型通过上面的自定义中转站直接接入
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.67] - 2026-10-15

### Fixed

- **面板路由链预演里 Auto 池成员不再冒充「首选」**：静态预演在展开
  `Auto/<模型>` 动态成员时，会把该成员也当成「调用方点名的成员」标成 preferred，
  于是一个没有显式优先级的 Auto 池平台可能排到显式配置了组内优先级的成员前面。
  现在只有显式成员会被标记 preferred，Auto 池成员按各平台自己的优先级参与排序，
  预演顺序与运行时的实际路由一致。

### Installer

- `client2api-setup-0.1.67.exe`（发布资产使用不含本机账号凭据和配置的空包）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.66] - 2026-10-15

### Added

- **对外模型白名单（`exposed_models`）与「对外模型」设置页**：模型列表和调用入口现在可以
  按**最终对外结果**过滤，而不是按模型源头过滤。网关先按路由结果生成完整的对外目录——
  `平台/模型`、`Auto/<模型>`、`model_groups` 组名、别名、转写模型——再只放行操作者勾选的
  最终 ID。每个最终 ID 独立控制：勾了组内成员不会自动开放组名，勾了别名目标不会自动开放
  别名。匹配忽略大小写和首尾空格。`null`（默认，或该键缺失）表示全部开放，`[]` 表示全部
  关闭。白名单同时作用于 `GET /v1/models` 和所有调用入口（对话、Responses、Messages、
  count_tokens、语音转写），被隐藏的 ID 即使按名字调用也会被拒绝。设置页在
  **模型与档位 → 对外模型**，支持搜索、按类型筛选、全选/反选/清空、逐项勾选；保存后写入
  `exposed_models` 并热重载，无需重启。配置里存在但当前目录没有的 ID 会保留成 `missing`
  行，保存不会丢。
- **按「平台 + 账号 + 模型」的 429 冷却**：上游返回 429 时给这个三元组记一段冷却，首次
  60 秒、连续失败翻倍、封顶 30 分钟；上游给了 `Retry-After` 就优先用它（同样封顶）。冷却
  期间网关拒绝该账号的这个模型，模块会换账号或换平台，不再撞同一个限流。模块每次尝试都可以
  通过 `core.ChatRequest.NoteAccountFailure` 上报，即使它已经轮转到别的账号成功也会记上；
  账号的模型弹窗会显示解除时刻和剩余时间。

### Fixed

- **「空回合」自动重试一次**：`finish_reason=stop`、没有工具调用，也没有任何可用的可见文本
  （完全没有文本，或在带了工具的请求里、长时间推理后只留下一小段碎片）时，网关会再向上游
  试一次，而不是原样当成正常结束回合返回；这正是让 Codex 收尾并等用户再发一句话的形状。
- **读不出来的工具调用不再被静默丢弃**：模型用文本写出、解析器读不出来的工具调用块会作为
  可见文本透出，这样被吞掉的调用不会伪装成一次干净的结束。
- **配置正确但暂时不可用的平台不再被当成「未配置」**：账号池里存在账号、但都因冷却/耗尽/
  风控而暂时不可用时，模块返回 `core.ErrPlatformExhausted` 让路由跳到下一个平台，而不是
  返回 503 让操作者去排查一个其实没问题的配置。

### Installer

- `client2api-setup-0.1.66.exe`（发布资产使用不含本机账号凭据和配置的空包）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.65] - 2026-10-15

### Fixed

- **流内额度错误现在会故障切换**：部分厂商（例如 LobsterAI）在额度用尽时返回
  HTTP 200 再跟一个 SSE 错误帧，网关此前把它当作 502 直接转发，后续候选平台根本没
  机会上场。现在网关在提交平台之前先读第一帧：该帧若是可重试错误（额度用尽、鉴权失败、
  限流、WAF 拦截），就关闭这个流、记一笔失败账，由下一个候选平台服务本次请求；已经
  读到的帧不丢失，会转交给真正应答的平台。
- **零积分账号退出轮换**：零余额不会自己恢复，因此不再套用带时钟的冷却，而是直接
  移出轮换。命中中文免费额度文案（“免费额度已用完”“请升级套餐”等）时账号显示为
  `已耗尽`，下次余额刷新读到正数会自动解除。
- **`platform/model` 硬指定不再被模型组改道**：显式指定平台原本会被降级为“首选”，
  同名模型在组内时失败会静默落到别的平台。现在硬指定在模型组查找之前生效，只由该平台
  应答，失败也如实报成该平台自己的失败；模型的组 failover 链只对裸模型名生效，
  `Auto/<model>` 仍然跳过模型组直接查各平台目录。面板“测试链路”的静态预演同步此规则。

### Added

- **平台配置页路由链预演**：输入模型名即可静态预演故障切换顺序，显示每个平台的
  实际模型、优先级及其来源（平台优先级、分时窗口、模型组、动态 Auto 池），全程不发
  上游请求、不读账号状态。

### Changed

- 补充多个客户端的 TLS 指纹、请求头一致性、响应解压与计费测试覆盖。

### Installer

- `client2api-setup-0.1.65.exe`（发布资产使用不含本机账号凭据和配置的空包）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。

## [0.1.63] - 2026-10-14

### Added

- **Gemini / Google Cloud Code Assist 平台**：新增 `gemini/...` 模块，支持 Google OAuth
  登录、账号发现与刷新、额度查询、Cloud Code Assist 请求翻译和流式返回；对外模型为
  `gemini/gemini-3.8-flash`，上游自动映射 low / medium / high / tiered 档位。
- **Anthropic 入站接口**：新增 `POST /v1/messages` 与
  `POST /v1/messages/count_tokens`，支持 Claude Code 的字符串或 block content、system、
  tools、tool result、thinking、usage、流式 SSE、非流式响应和 Anthropic 风格错误。
- **ZCode 验证码预取池**：新增 `captcha_pool` 与 `captcha_pool_ttl_seconds`，请求可先取
  预取参数并后台补充；外部命令和内置浏览器共用同一 mint 路径。连续 mint 失败会进入
  退避，避免继续消耗设备额度。
- **可选出口中继 `egress`**：支持现有 HTTP/SOCKS 代理 URL，或由本进程托管 mihomo
  并加载订阅；启用的 `egress` 优先于顶层 `proxy`，订阅 URL 在面板中脱敏。
- **聚合渠道注册表数据源**：Opencode 兼容模块嵌入渠道注册表，自动吸收可用的
  `openai-chat` 渠道；手工表优先，原生 Anthropic / Responses 渠道不会被错误导入。

### Changed

- **账号逐模型启停扩展到所有平台**：WorkBuddy 继续使用模块自己的 realm 感知策略，
  其余平台使用网关共享策略库；账号模型弹窗会显示限流解除时间和剩余时间。
- **Loomy 余额、套餐与模型目录刷新改为展示用途**：读取失败只在该账号行报错，不再
  让刷新动作错误地让账号冷却、异常或停用。
- **平台配置页整理**：模块配置、WorkBuddy 提示词和指纹清洗控件收进对应平台卡片；
  全局配置页只保留真正的全局项。腾讯系内容级指纹改写继续默认开启。

### Installer

- `client2api-setup-0.1.63.exe`（发布资产使用不含本机账号凭据和配置的空包）
- 升级会保留现有 `configs/`、`data/`、账号池、用量和任务数据。
## [0.1.62] - 2026-10-14

### Added

- **配置页的模块配置改成中文表单**：`clients.<模块>` 不再只显示一坨裸 JSON。
  面板会按模块把常用键渲染成中文标签、输入框、开关或下拉框，并显示每项用途；
  空白控件表示回到模块内置默认值，尚未整理的键保留在“其它键（JSON）”中。

### Installer

- `client2api-setup-0.1.62.exe`（包含当前本机数据；升级会保留现有 `configs/` 和 `data/`）

## [0.1.60] - 2026-10-13

### Fixed

- **Auto/DeepSeek-V4.1-Flash 在同一平台只展开一个 realm，导致跳过可用的 WorkBuddy
  国际账号**：WorkBuddy 的模型目录同时发布 `cn:deepseek-v4.1-flash` 和
  `global:deepseek-v4.1-flash` 两个等价条目。旧逻辑在 `Auto/` 路由时对每个平台只取
  第一个匹配项，`cn:` 在目录中排在前面时，候选就固定成 `workbuddy/cn:...`；国内账号
  不可用时会直接跳过整个 WorkBuddy，虽然 global 账号仍可服务该模型。现在 `Auto/`
  和模型组里的动态 `Auto` 成员会保留同平台的全部 realm 等价候选，按平台健康、
  成员优先和既有优先级顺序回退；普通裸名请求仍保持每平台一个最佳等价项。

### Installer

- `client2api-setup-0.1.60.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/` 和 `data/`
## [0.1.59] - 2026-10-13

### Fixed

- **AutoClaw 面板对话测试/普通 OpenAI 客户端调用仍然 406**：0.1.58 只补了上游要求的
  system 标记，但托管代理还有第二个硬性条件——body 里的最大输出 token 必须写成
  `max_completion_tokens`。只要出现 `max_tokens` 且小于 10000，上游就在鉴权前返回
  空体 `406 Not Acceptable`（网关转 502）。面板「对话测试」默认下发 `max_tokens=512`，
  因此升级后仍然失败。现在 `clients/autoclaw/body.go` 统一改用
  `max_completion_tokens`，调用方给的值原样透传，只是换成上游认的字段名。
  实测字段矩阵：`max_completion_tokens` 任意值 200；`max_tokens<10000` 406；
  两个同时存在也 406。桌面端本来就只用 `max_completion_tokens`，所以一直正常。

### Installer

- `client2api-setup-0.1.59.exe`（空包，不含账号凭据或本机配置）
- 升级会保留现有 `configs/` 和 `data/`

## [0.1.58] - 2026-10-13

### Changed

- **封顶对齐改为所有平台、所有模型默认开启**：面板中每个模型的“封顶对齐”默认勾选。
  开启后仍只在请求为 `xhigh` 且模型真实最高档更高时提升（例如 WorkBuddy、Cline、
  OpenRouter 的 `max`）；最高档本来就只有 `xhigh` 或没有公布思考档位的模型不会
  被下发虚构档位，实际为空操作。手动取消勾选仍然优先，“恢复默认”会回到开启。

### Added

- **WorkBuddy「强制启用」提升为账号级健康覆盖**：开启后会清除冷却、零余额/额度耗尽、
  session 失效、风控、软限流和降级等账号级判定，账号在面板中恢复为可用并真正参与路由；
  逐模型限流和支持性判定仍然保留。该设置继续随账号策略持久化。
- **WorkBuddy 国内版自动登录诊断与提交流程收紧**：删除人工重放 iframe `load` 事件的
  旧绕过逻辑，提交按钮只匹配可点击状态；请求轨迹现在覆盖顶层和同源 frame，
  并镜像到 `sessionStorage`，用于记录短信发送、验证码校验、账号列表和账号选择
  的实际接口走向。该轨迹只记录方法、路径、状态码和少量业务字段，不记录请求体、
  请求头、token 或 query。
- **Loomy 402 额度拒绝按账号级低额度停用处理**：同一账号重试后仍返回
  `insufficient_quota` 时，不再让它随着 30s/1m/2m 冷却结束重新进入轮转；
  余额恢复或签到成功后再解除。并发失去冷却阶段 CAS 时也会继续轮转到其它账号。
- **AutoClaw 聊天请求补齐桌面端系统提示**：普通 OpenAI 请求会补上托管代理要求的
  桌面端 system 标记，避免请求在鉴权前被上游以 406 拒绝；调用方已带标记时不会重复。

## [0.1.57] - 2026-10-13

### Changed

- **账号级模型策略**：支持按账号禁用单个模型，并可在面板中一键全选/全不选；
   新增“强制启用”用于绕过零积分或额度耗尽造成的冷却，不会绕过 session 失效、
   风控和限流。策略保存后会持久化，并在账号池与模型明细弹层之间保持一致。
- **网关热路径和上下文预算收敛**：路由与上下文预算复用同一次目录读取，避免为
   已匹配的模型重复请求目录；请求体改为流式解码，平台会话和限流槽位增加回收，
   长时间运行时的内存占用更稳定。
- **V4.1 Flash 的封顶对齐改为默认开启**：内置模型表里所有平台的 V4.1 Flash
  条目现在默认勾选“封顶对齐”，Codex 请求的 `xhigh` 会在模型真实最高档更高时提升
  到该档（WorkBuddy、Cline、OpenRouter 的 `max`）。手动取消勾选仍会覆盖默认值，“恢复默认”
  会清掉手动选择并回到默认开启，不会再被钉成永久关闭。
  Loomy 的 V4.1 Flash 公布的最高档就是 `xhigh`，实测发送 `max` 会被上游忽略并
  回落到默认档，因此该条目的默认只是保持策略开启，不会下发虚构的 `max`；
  WorkBuddy 国际版只接受 `high`，同样保持不变。

## [0.1.56] - 2026-10-13

### Changed

- **语音转写对外改为标准 OpenAI Audio Transcriptions 子集**：`doubaoime`
  现在直接支持 `json`、`text`、`srt`、`verbose_json`、`vtt` 五种响应格式，
  并接受 WAV、FLAC、MP3、MP4/M4A、OGG、WebM 上传。非 WAV 容器由内置 FFmpeg
  解码为 16 kHz 单声道 PCM；不能真实实现的标准参数（`diarized_json`、
  逐词时间戳、logprobs、流式、说话人分离等）会明确返回 400，不再静默忽略。

### Fixed

- **WorkBuddy 国内版账号选择前 OneID 状态被清空**：手机验证码通过后，
  隐藏 iframe 仍能提供 `loginUrl`，但外层 React 的 `loginInfo` 可能在账号页
  重挂载时被清掉，点击账号后仍会跑到 `/login/undefined`。现在进入账号选择前
  会再次同步 iframe 的 `load` 状态并等待 React 提交，再进行账号点击。
- **大小写不同的模型组名在保存时报“重复”**：文件里写 `DeepSeek-Flash`，面板读出来是
  规范化的小写 `deepseek-flash`，保存时深合并按大小写敏感新增了第二个键，校验就把它
  当成重复组名拒绝；删除组时同理删不掉原来大写的键。现在 `model_groups` 的分组合并按
  大小写不敏感匹配，保留磁盘上原有的键名大小写。
- **模型组成员名被当成硬钉死平台、无法回退**：把 `平台/模型` 写进 `model_groups` 后，
  请求该成员名会被当成“只准用这个平台”，精确成员失败时不会回到组内其它成员。现在
  组内成员名（含带平台前缀写法）是等价入口：优先精确成员，失败或近期不可用再按组内
  优先级回退到兄弟成员；不带组信息的 `平台/模型` 仍是硬钉死平台。
- **整份备份与恢复漏掉真实凭据、误带运行态**：备份文档此前只按文件名白名单识别凭据，
  导致 `kimi-token.json`（Kimi 面板登录）和 `doubao_credentials.json`（豆包输入法）不会
  导出，而 kimi/zcode/minimaxcode 的运行态 `accounts.json` 却被当成凭据覆盖目标实例的
  冷却、禁用和删除墓碑。现在按模块区分凭据与运行态，WorkBuddy 已停用账号
  （`*.json.disabled`）也会随包迁移。
- **模型上下文、输出上限、推理档位与价格的手工覆盖不随整份备份迁移**：
  `model_context.json` 是操作者决策而非测量值，现在随备份导出并在导入时整体替换、
  立即生效。
- **WorkBuddy 自定义 `accounts_dir` 时静默漏账号**：导出只扫描 `<data_dir>/workbuddy`，
  自定义目录的账号不在快照内。导出现在会明确提示，避免恢复后才发现账号缺失。
- **定时余额扫描阻塞后漏掉下一次批量任务**：余额扫描可能跨过下一个批量小时，
  旧逻辑重新规划任务时会把已经到期的批量触发滚到下一周期。现在扫描期间到期的
  批量任务会按当前时间补发，不再整批漏跑。

## [0.1.55] - 2026-10-13

### Fixed

- **Loomy 全部账号冷却时裸模型请求不故障转移**：所有账号都在冷却或停用时，Chat 返回的
  普通 error 没有被网关的 `candidateUnavailable()` 识别为"平台暂时不可用"，带平台前缀的
  请求（`loomy/...`）失败是正确的，但裸模型 id（如 `DeepSeek-V4.1-Flash`）也应该跳到
  下一个能服务它的平台而不是直接失败。现在返回
  `errors.Join(core.ErrPlatformExhausted, ...)`，网关能正确跳过该平台。其他模块均已
  符合此约定，Loomy 是最后一个遗漏的。
- **WorkBuddy 国内版 OneID iframe 重挂载后登录地址丢失**：首次 `load` 补发后，React
  可能清除 `loginInfo` 并重挂载隐藏的 `#phone-iframe`，新实例的 `__ONEID__.loginUrl`
  为空，账号选择页仍无法取到地址。CN 流程现在轮询到地址就绪后保持稳定 1.5 秒再继续，
  并对重挂载后的新 iframe 再次补发 `load`。
- **定时余额扫描结果不更新面板账号页**：定时扫描读到的余额只进了模块自己的池，
  账号页的余额列和"最后读取"时间戳不变，除非手动打开页面。现在扫描的成功读取会
  发布到面板共享的余额缓存，失败不覆盖上次的好数据。

## [0.1.54] - 2026-10-13

### Fixed

- **WorkBuddy 国内版 OneID 登录地址竞态**：隐藏的 `#phone-iframe` 有时会在
  `__ONEID__.loginUrl` 就绪前触发 `load`，React 状态一直是 `undefined`，最后回调
  变成 `/login/undefined` 并一直停在「等待厂商确认」。现在 CN 流程会在获取验证码前
  等到地址就绪并补发 `load`，国际流程不变。
- **模型与档位的修改无法保存**：模型行外层 `td` 和内部控件都带 `data-field`，
  上一版把 `mdInputs` 的选择器放宽成 `[data-field=...]` 后先取到 `td`，输入事件
  记录不到真实值，保存按钮一直不亮，切页重建后编辑也消失。选择器已限定回
  `input[data-field=...]`，并补了回归测试。

### Changed

- **V4.1 默认思考档位按平台落库**：WorkBuddy 国内为 `low/high/max`（默认 `high`）、国际为仅 `high`；Cline 使用平台通用档位；Loomy 顶档为 `xhigh`；Raccoon/CodeArts 为开关语义。没有 per-model 依据的平台继续保持未配置。

## [0.1.53] - 2026-10-12

### Fixed

- **WorkBuddy 国内版账号选择竞态**：手机验证码通过后，`/login/select` 的卡片点击只会先更新
  React 选中状态，主提交按钮在 `selected` 出现前保持 disabled，旧实现会点击一个无效按钮并
  一直停在「等待厂商确认」。现在会等待“个人版”卡片真正带 `selected`，再点击
  `button.select-account-button.w-full` 的启用态；错误页的重试按钮不再被误点。
- **Loomy 侧信道把可用账号永久停用**：余额、套餐、目录、任务和 keepalive 等后台读取拿到
  `100002`/`020002` 时只做冷却，不再直接把账号标记为 dead；只有真实 chat 请求的拒绝才会
  永久停用，后续 chat 成功也会清掉并发产生的 dead 状态。
- **OpenAI Compat 冷却状态被误报为缺少 key**：候选 key 因上游失败进入冷却时，请求现在返回
  明确的 busy/冷却原因，而不是错误提示去重新扫描模型。
- **面板手动切换平台被自动故障转移覆盖**：手动 pin 现在拥有更高优先级，临时故障会放行本次
  请求到健康平台，但不会悄悄改掉下一次请求的目标。

## [0.1.52] - 2026-10-12

### Fixed

- **国内版 WorkBuddy 登录仍卡在厂商确认**：桌面客户端打开国内版授权页时会追加
  `version=<桌面版本>`，旧实现只写了 `loginSessionId`。缺少该参数时，网页登录虽然完成，
  插件轮询仍一直返回 `11217`；现在按桌面版 `5.7.7` 写入 `version`，国际版授权 URL 保持不变。


## [0.1.51] - 2026-10-12

### Fixed

- **国内版 WorkBuddy 登录仍卡在厂商确认**：登录页在手机验证码通过后会停在
  `/login/select` 账号选择页。自动登录此前只轮询插件授权结果，没有完成网页侧的账号
  选择，因此 state 一直保持 `11217` 直到超时；现在会优先选择“个人版”账号，否则选择
  第一项，再等待厂商通过 `/console/auth/login` 绑定本次授权。国际版流程不受影响。

## [0.1.50] - 2026-10-12

### Fixed

- **国内版 WorkBuddy 登录卡在厂商确认**：插件授权请求改用桌面客户端实际使用的
  `www.workbuddy.cn` 地址、`platform=workbuddy` 和对应的 `X-No-*` 请求头，并在国内版
  授权链接中写入每次登录独立的 `loginSessionId` UUID。此前轮询会一直停留在
  `11217`，直到整个流程超时；国际版地址和请求保持不变。
- **WorkBuddy 自动登录点到不可点击的容器**：手机号标签现在只匹配可点击控件，并等待
  React 完成渲染后再点击；协议勾选和提交后的服务条款弹窗也支持页面内 iframe。
- **Loomy 真实模型探测成功后账号仍是失效状态**：手动测试成功现在会同时清除 dead、
  失败次数、冷却和探测等待状态，账号立即恢复为 ready。
- **上下文裁剪误拒绝只保留最新一轮的请求**：当只有末尾消息能放进窗口时，守卫现在
  正确裁剪而不是报上下文超限。

### Added

- **最近调用显示失败原因**：失败的请求和候选尝试会记录经过脱敏的原因，面板新增
  「失败原因」列，用来区分本地并发已满、上游限流、账号额度用尽等 429 场景。
- **无账号转写平台的调用信息**：账户池会读取模块发布的 transcription capability，
  显示通用转写地址和带平台前缀的模型名，并支持一键复制；Doubao IME 这类无账号平台
  不再只显示空账号提示。

## [0.1.49] - 2026-10-12

### Fixed

- **上下文守卫在 workbuddy 上从未生效（0.1.47/0.1.48 的实际缺陷）**：网关的「学习厂商
  真实窗口」钩子原本挂在 `openStream` 里 `core.ErrPlatformExhausted` 提前返回**之后**，
  而 workbuddy 的账号池走完一轮后正是把分类过的错误 join 上这个标记返回的
  （`clients/workbuddy/workbuddy.go:987`，11115 属不可重试、每个账号都会立刻失败）。
  于是这条路径永远走不到钩子：实测 6 次真实拒绝、错误文本里明明带着
  `100000 maximum`，`data/learned_context_windows.json` 始终没有被创建。现已把通知移到
  两个提前返回之前，并有回归测试覆盖裸分类错误、join 了 exhausted 标记、以及
  `fmt.Errorf` 包裹三种形态。
- **守卫看不到厂商自己公布的窗口**：`publishedContextWindow` 之前用空的 vendor Extra
  去解析，等于只看静态表，于是凡是窗口来自厂商目录的模型一律报告「未知」，裁剪根本
  不会触发。已改为先读模块目录里的 `context_length`。

### Added

- **学到的窗口不再低于实测能力（learn floor）**：厂商报错里的数字并不总是模型真正的
  服务窗口——它可能只描述某一次请求、某个会话状态或某个后端。实测中 workbuddy 在
  10:32 以 `100000 maximum` 拒绝了 `cn:space-bunny`，而同一模型随后成功服务了 224986
  token 的请求；若照搬那个数字，模型会被永久砍到不足一半，之后每次健康请求都会被裁剪。
  现在学到的窗口不得低于「该模型历史最大成功 prompt」的 80%。证据取自用量流水（内存中的
  recent 环，启动时从 `recent.json` 读回），因此重启后依然有效且无需额外状态；失败记录
  不计入证据（被拒的请求根本没到模型手里），也不跨模型串用。没有观测值时厂商的数字原样
  采信。这条下限只会把过小的数字抬回实测水平，绝不让守卫比厂商或运维说得更严。

- **推理强度档位可按模型手动配置（effort policy）**：模型页可编辑每个模型的
  `supported_efforts` 与 `default_effort`，并可选择开启「顶档对齐」——调用方要 `xhigh`
  时自动提升到该模型实际支持的最高档（如 `max`）。三者都存进既有的 `model_context.json`，
  面板上显示来源、恢复默认即可撤销。失败切换时每个候选平台都从调用方原始请求重新开始，
  不会把上一个平台提升过的档位带过去。

- **workbuddy 手机号登录支持服务条款弹窗**：新登录流程点「手机号登录」后会弹出
  「服务条款与隐私协议」，在点同意之前不会发出任何验证码请求，因此首次提交必然卡住。
  自动登录现在会识别该弹窗、点「同意」并重新提交，同时不会误点其它弹窗。

## [0.1.48] - 2026-10-12

### Changed

- 版本号推进到 0.1.48。相对 0.1.47 没有代码改动，重新出包以便分发。

## [0.1.47] - 2026-10-12

### Changed

- **Windows 安装包内置 Opus-only ffmpeg，语音转写开箱即用**：doubaoime 需要把 WAV
  编成 20 ms Ogg Opus，而本项目是 CGO_ENABLED=0 构建、也没有可导入的纯 Go Opus
  编码器（pion/opus 只有解码器），过去必须自行安装 ffmpeg。现在安装包随附一份
  1.95 MB 的 LGPL 定制构建（只保留 libopus 编码器、ogg/opus 复用器、pcm_s16le
  解复用器与解码器，以及 file/pipe 协议），放在 client2api.exe 旁边——正是
  findFFmpeg 的首选位置，升级即生效，无需任何配置。findFFmpeg 的查找顺序也调整为
  「配置 → exe 同目录 → PATH」，让经过校验、被测试覆盖的那份优先于系统里恰好装着的
  ffmpeg（发行版裁剪版可能没有 libopus）。SHA256 钉在 third_party/ffmpeg/ffmpeg.sha256，
  installer/build.ps1 打包前校验，clients/doubaoime 与 installer/setup 各有回归测试断言。
  便携版压缩包（Linux/macOS/Windows zip）不含该二进制，仍需自行安装 ffmpeg。

### Added

- **上下文窗口守卫**：模型目录宣称的 `context_length` 常与厂商实际限制不符，请求因此
  被拒。现在网关在**确定知道**窗口大小时才动手：先按窗口给输入预算，超预算就从最旧的
  轮次开始丢弃（保留系统提示与最新一轮，被钉住的最后一轮绝不丢），让请求本身成功，
  每次裁剪都记在用量行上，绝不静默丢内容；万一仍被拒，就从厂商的报错里学到它真正报的
  上限并记录下来，让 `/v1/models` 不再宣称那个虚高的数字，下游客户端自己也不会再把
  上下文塞爆。窗口未知时一律原样转发——悄悄改写一个本来能成功的请求，比给一个诚实的
  400 坏得多。已学到的窗口只会向下修正，绝不因一句含糊的报错被抬高。

### Fixed

- **WorkBuddy 丢失工具调用**：当正文已经通过 delta 流式输出过之后，厂商在终止帧里补发的
  `message.tool_calls` 会被整帧丢弃，于是这一轮只剩文字、工具调用凭空消失。现在终止帧
  无论是否已有正文都会被读取：正文不重复（delta 已经给过），但工具调用只在这里出现，
  必须合并进来。

## [0.1.45] - 2026-10-12

### Added

- **新增平台 doubaoime（豆包输入法）与语音转写接口**：网关新增 OpenAI 兼容的
  `POST /v1/audio/transcriptions`，并且新增独立平台模块 `clients/doubaoime`，把豆包输入法
  （Android）的私有在线语音识别协议接进来：上传 WAV（16 kHz 单声道 PCM，非该格式会在
  模块内重采样/混音），模块用 FFmpeg 编出 20 ms Ogg Opus 帧，走豆包输入法的设备注册、
  设置/app_key 与 ASR WebSocket 私有协议，返回 `{"text": "..."}`。它需要 FFmpeg（配置
  `ffmpeg_path`、`PATH` 或与 exe 同目录的 `ffmpeg.exe`），首次请求自动注册设备，凭据落在
  `data/doubaoime/doubao_credentials.json`。（FFmpeg 的分发方式见下方 Changed。）
- 核心新增可选能力 `core.TranscriptionProvider`：转写模型与对话目录**分离**——它们不进
  `Client.Models()`、`Auto/<模型>` 聚合或对话号池，而是由 `GET /v1/models` 单独列出并标记
  `extra.kind = "transcription"`。协议细节与风险见 `docs/upstream/doubaoime.md`。

### Fixed

- **手机号验证码登录可能被重复兑换**（Loomy）：FinishPhoneCodeLogin 原先在调用厂商
  之前只读取 challenge、兑换成功后才删除它。两个并发的兑换请求会同时看到这条还在的
  challenge，于是把同一个一次性验证码花两次：厂商只认第一次，失败的一方只拿到一句
  费解的厂商报错，最坏情况还会多写一条账号。现在改成在锁内「先占住再调厂商」：把
  challenge 标记为 in-flight 后才发起兑换，并发进来的第二个调用者会得到「该验证码正在
  校验中」，因此一次性验证码不可能被花两次。厂商拒绝（验证码输错）时释放这个标记并保留
  challenge，运营者可以直接重输正确验证码而不用再发一条短信；只有兑换成功才把 challenge
  删除。
- **余额 / 任务看板缓存的写入会卡住读取**（面板）：两个缓存此前在持有读锁的状态下
  把整份文档 MarshalIndent 并落盘，于是每一次上游余额回写都让账号页的读取排队等
  文件写。现在改为：锁内做一份文档深拷贝，编码和写盘都在锁外进行，写入之间用单独的
  persistMu 串行化。账号页读缓存时不再等磁盘。
- **缓存文件可能被写成半截**（面板 / tabbit / trae）：这些写盘路径都用固定的
  xxx.tmp 临时文件名。两个写入者会交错写进同一个临时文件，再把它改名成正式文件，
  正好是这次改名想避免的半截文档。现在统一走 core.WriteFileAtomic，每次写入用独立
  临时名，并发读写另有回归测试钉住（go test -race）。

### Changed

- 定时任务页每行的「启用」开关补上 ria-label（含任务名与平台名）。它只有 14px
  见方、没有可见标签，之前在无障碍树里是一串匿名复选框，屏幕阅读器只报「复选框」。
- 面板模块（余额、任务看板）查找客户端时每账号做一次 Registry.All()（每次都新建
  切片）改为在构建刷新目标时按模块解析一次，并把结果挂在每个 refreshTarget 上，
  一轮刷新不再按账号重复解析；余额刷新目标的构建改为每个模块取一次缓存快照，而不是
  每个账号加一次锁。

## [0.1.44] - 2026-10-11

### Fixed

- **修复流式断流（半截回合被当成成功）**：模型偶尔会思考很久（本次复现是 22475 个推理
  token、约 101KB），最后 `finish_reason=stop`，既不产出可见正文、也不产出工具调用。
  网关此前把这种死回合包装成 `response.completed` 并附带一条空 assistant 消息，客户端
  界面上看起来“成功但什么都没给”，只能再发一次「继续」。现在 Responses 的流式和缓冲
  两条路径都会把“无正文且无工具调用”判为 `response.failed`，让调用方看到明确失败并可
  重试。只有工具调用、没有正文的回合仍然照常成功。

## [0.1.43] - 2026-10-11

### Added

- **Doubao 桌面客户端接入**：面板“导入凭据”现在可以直接读取已登录的豆包桌面客户端
  （Chromium 外壳）会话——只读打开 `Cookies` 数据库，用 Windows DPAPI 解开
  `Local State` 里的 AES 密钥，并剥离 Chromium 127+ 的 32 字节 app-bound 头，
  得到与网页端相同的 Cookie 会话。不写客户端配置，运行中的客户端不受影响。
- Doubao 模型目录改为同步桌面客户端当前账号实际展示的模型菜单
  （`action_bar_menu_config`，并兼容 v2 `menu_conf_v2`）。`/v1/models` 按
  `mode<mode_id>_model<model_item_key>` 暴露每个菜单项，聊天请求携带客户端同样的
  `reply_mode` / `mode_id` / `model_item_key` 路由三元组；账号还未刷新出菜单时回退到
  客户端默认（`mode_id=3`、`model_item_key=0`）。

### Fixed

- **AutoClaw 登录**：发送验证码和验证码登录现在按桌面端 `AuthApiAdapter.post` 的协议补齐
  `source_id: "autoclaw"` 与稳定的 `device_id`。此前只发送 phone/code，服务端会拒绝为
  `400001 请求数据有问题`；现在真实接口已分别返回 `code 0 / result true` 与正常的验证码
  业务校验。
- AutoClaw 在没有桌面 device-id 文件时会生成版本 4 UUID，并按客户端相同的
  `device/device-id.json` 格式持久化，确保发送验证码、登录和登录后账号调用使用同一个设备身份。
- Doubao 账号信息探测优先使用 POST `/alice/profile/self_brief`，并保留 POST
  `/alice/user/get_account_info` 作为兼容回退，避免带会话 Cookie 时 GET 命中 404 路由。
- Loomy 账号探测在模型只产出推理内容、正文为空时按有效响应处理，避免把已经成功消费探测预算的
  正常账号误判为不可用。

## [0.1.42] - 2026-10-11

### Added

- 新增独立平台 **AutoClaw**：拥有独立的主机、产品身份、签名、账号目录、协议和模型目录，
  不依赖或伪装成智谱、BigModel、ZCode、WorkBuddy。支持桌面客户端账号自动发现与导入、
  JSON 文件导入、手机号验证码登录、模型目录、余额与积分包、每日积分领取、会话粘性、
  健康与池统计、复活和账号管理。
- 新增 **Doubao** Web 平台模块：账号池与 AccountManager、Cookie/Web 传输、受控浏览器
  登录、SSE 聊天、模型目录、余额刷新、健康检查、复活和会话粘性，并补齐平台说明。
- Cline 和 OpenCode 新增 `free_only` 配置，默认开启；只向面板和 `Models()` 暴露零成本
  模型，并在请求进入上游前拒绝明确指定付费模型的聊天，避免误用付费额度。面板平台卡片
  增加“只展示免费模型”开关，配置会写回各自模块的 `clients.<平台>.free_only`。
- 模型明细弹层优先显示模块提供的 `display_name`，同时保留原始 wire model id 作为标题，
  让账号池里的模型名更易读。

### Fixed

- 浏览器层补充 CDP cookie 读取，使 Doubao/AutoClaw 的 Web 登录能够把已登录浏览器会话
  安全交接给对应模块。
- AutoClaw 的桌面凭据 `updatedAt` 同时兼容字符串和数字格式，导入本地客户端数据时不再
  因时间字段类型不同而失败。

## [0.1.41] - 2026-10-11

- OpenCode：登录账号现在也可以走 `*-free` 免费模型。此前免费模型只走限流很严的匿名
  公共额度，登录账号被排除在外；现在免费模型允许由登录账号承接，并在该路径上补发
  CLI 免费握手（`x-opencode-session` + bash/read 工具声明），不再只能依赖匿名身份。
- 账号池模型列只把模块 `capabilities.realms` 里声明过的 code 当成 realm 前缀。此前
  任意冒号前的内容都会被截掉，OpenRouter / KiloCode / Cline 的 `nvidia/x:free`、
  `openai/x:batch` 因此全变成 `free` / `batch` 重名项；OpenCode / ZCode 这种目录本来
  就是裸 ID 的平台反而被过滤成空。WorkBuddy 的目录刷新改用只读元数据选号，额度耗尽
  但凭据仍有效的账号也能贡献模型目录，聊天选号策略不变。
- 平台配置读取不再把平台名或账号 ID 里的 `token` 等字样误判成凭据字段。此前
  `tokenrhythm` 等平台的基础优先级在面板接口里被整段替换成 `<redacted>`，页面因此
  显示不出真实值，看起来像保存无效；现在只脱敏真正的凭据字段。
- 任务中心的恢复探测和每日余额刷新支持秒 / 分钟 / 小时间隔及对称随机抖动；
  每日余额刷新还可改为 5 段或 6 段 Cron，并按平台保存不同模式。

## [0.1.40] - 2026-10-11

### Fixed

- Responses 历史回放不再把模型的一句话前言拆成独立的 assistant 轮次。Codex 回放
  `reasoning → assistant 正文 → function_call` 时会把正文单独发成一条 message，
  旧逻辑让它和随后的函数调用变成两条连续的 assistant 消息，思考内容只挂在第一条
  上。现在合并成一条同时带正文、思考和 `tool_calls` 的 assistant 消息，符合 Chat
  语义，也不再给模型一段“说完一句就收尾”的历史去模仿。

### Changed

- Responses 流式日志在 `finish=stop` 且没有工具调用时追加 `stop_tail` 与
  `stop_reason_tail`，把这一轮收尾处的可见正文和思考末尾（各最多 200 字节）写进
  日志，便于区分“正常作答”与“模型说了半句就停”。

## [0.1.39] - 2026-10-11

### Fixed

- 未闭合的 DSML/XML 工具调用块不再静默丢弃尾段。上游如果在一半工具调用处结束，
  网关会保留可见尾段供客户端和日志判断，而不是把缺失内容伪装成正常完成。
- Responses 回放 `function_call_output` 时补回对应函数名，避免要求 `name` 字段的
  上游无法正确配对工具结果。

### Changed

- Responses 流诊断新增 `finish`、`delta_bytes`、`reasoning_bytes`、`tool_calls`
  和 `completion_tokens`，并将达到输出上限的请求标记为 `terminal=incomplete`，
  便于区分正常 `stop`、工具轮和上游提前结束。

## [0.1.38] - 2026-10-11

### Added

- Chat / Responses SSE 结束时新增 `stream: id=...` 诊断日志，记录协议、平台、模型、
  上游事件数、首包耗时、总耗时和终止原因，不记录对话正文或凭据。

### Fixed

- Chat SSE 上游报错后不再补发正常完成标记 `[DONE]`，并在错误事件处停止读取，
  避免随后 EOF 覆盖真实终止原因或重复累计失败。
- Responses 流错误的请求统计行反映失败状态；调用方取消及部分下游写失败路径
  不再静默记录为成功。诊断用于继续定位生产断流，不代表所有断流原因已经解决。

## [0.1.37] - 2026-10-11

### Added

- 自定义中转站余额：平台配置新增 `balance_protocol`（`auto`/`newapi`/`sub2api`/
  `openai_billing`/`none`）和 `balance_quota_per_usd`（默认 `500000`）。`auto`
  依次探测 New API `/api/usage/token/`、Sub2API `/v1/usage`、New API 控制台
  `/api/user/self`（需控制台凭据）、OpenAI Billing `credit_grants`+`subscription`。
  账号高级字段新增 `balance_access_token`、`balance_user_id`。余额统一按 USD 显示，
  New API 保留 quota 整数并按 `balance_quota_per_usd` 换算，Sub2API/OpenAI Billing
  以微美元存储，面板按 `scale` 显示，并已处理无限额度与 `hard_limit_usd=100000000`
  哨兵值。
- Kilo Code 面板设备登录：走 Kilo 的 device grant（`POST /api/device-auth/codes`、
  `GET /api/device-auth/codes/{code}`），登录后以 bearer token 建号并用 `kiloUserId`
  作为稳定账号 ID；模块配置新增 `auth_base`。
- Kilo Code 自动发现 Kilo CLI/Desktop 的登录文件 `~/.local/share/kilo/auth.json`，
  导入后直接以已登录账号出网。
- 粘性会话管理：账号池概览新增「粘性会话」入口，合并各模块的账号绑定与网关的平台
  绑定，支持解绑账号粘性和强制切换（同时清账号与平台粘性）；新增面板接口
  `/panel/api/sticky-sessions`、`/unbind`、`/switch`。

### Changed

- 上游 SSE 缺少终止帧不再被当作正常结束：聊天与 Responses 的流式/非流式路径把
  「EOF 早于终止帧」上报为 `upstream_error`，避免调用方把半截响应当成功。
- 账号「模型」列在模块不逐模型上报限流时回退到账号 `fields.models` 或平台模型目录，
  显示「可用 N / 按平台目录全部可用」，不再留白。
- OpenCode 账号行发布 `fields.realm`（匿名=`free`，Console=`oauth`），面板的逐行
  重新登录据此进入正确的 device-code 流程。

### Fixed

- Windows 关闭浏览器时按 `--user-data-dir` 结束同一 profile 下的所有 Chromium
  进程，修复 launcher 转交子进程后旧浏览器残留的问题。
- Kilo Code 账号无可用凭据时的提示由「没有 API key」改为「没有可用凭据」。

## [0.1.36] - 2026-10-11

### Added

- 新增 Kilo Code 客户端模块：`kilocode/...` 路由接入 Kilo AI Gateway，支持账号池、
  API Key 导入、模型刷新、限位、会话粘性和流式对话。
- 模型价格目录新增多厂商官方人民币价表，覆盖 DeepSeek、Zhipu/BigModel、Kimi、MiniMax、
  Qwen、Volcengine/Doubao、Tencent Hunyuan、Xiaomi MiMo、Anthropic、Google 与 OpenAI；
  平台别名可落到对应官方价。

### Changed

- ZCode 活动套餐领取按账号作用域：同一账号下 JWT 行和它的 coding-plan API key 行
  都会显示领取按钮，点 API key 行自动走 JWT 兄弟通道；批量签到/任务也按账号出队，
  只领取一次。孤立的 API key 账号（组里没有 JWT）不显示按钮。
- README 重写为项目介绍，新增中英文导航和 `使用教程` 页面；历史长文档保留到
  `docs/REFERENCE.md` 与 `docs/REFERENCE.zh-CN.md`。文档定位为个人自用/学习，
  不提供公网或服务器架设方案。
- WorkBuddy 上游 SSE 缺少终止帧时不再静默收尾，会以错误事件结束，避免调用方把
  半截响应当成功。

### Fixed

- ZCode Windows 浏览器验证码读取命令行时改用 `unsafe.Slice`，并补齐偶数偏移校验。
- Tabbit 状态探测超时预算放宽，避免负载较高时偶发误判。


## [0.1.35] - 2026-10-11

### Added

- 添加账号弹层的接码平台区域新增可选代理：代理只保存在当前浏览器，随每次接码请求发送，
  支持 `http://`、`https://`、`socks5://`；它只覆盖本次接码平台请求的 HTTP transport，
  不影响厂商请求、浏览器或网关全局 `proxy`。WorkBuddy、Loomy、Qoder 都已接入。
- Zcode 任务看板增加账号级领取状态缓存，面板频繁轮询时不会反复触发厂商计费查询；
  活动套餐预览为空时再读取一次余额，能区分“已领取”和“厂商当前没有可领取套餐”。

### Changed

- 计划任务按平台分泳道并行：同一平台的批次仍严格串行，不同平台不再互相阻塞。
- DSML/XML 工具调用解析抽到 `internal/dsml`，Loomy 与 WorkBuddy 共用同一实现。
- 缓存命中率改用上游真正上报缓存字段的请求统计，历史未上报缓存字段的桶不再按“未命中”
  计入分母；跨 Anthropic 与 OpenAI 兼容形态统一识别“上报了 0”和“没上报”。
- DeepSeek Flash / Pro 改用官方人民币峰谷价：模型列表显示优惠时段价格，历史小时桶按完成时间
  对应的价格重算；整体价格版本升级时迁移用量库，避免旧成本继续按过时单价显示。

### Fixed

- 模块自己声明批次但仍实现 `core.CheckinProvider` 时，计划任务会补上缺失的 `checkin` 批次，
  让面板签到按钮和定时签到保持同一件事。
- Tabbit 签到请求改用厂商当前要求的 `scene_codes` 数组字段，并继续携带每次唯一的
  `request_no`，避免 422 `VALIDATION_ERROR`。
- 修正多平台上报缓存的流式解析，避免把缺失缓存字段误记为 0 命中。

## [0.1.34] - 2026-10-10

### Fixed

- 修复五个内置 GPT-6+ 原生模型名只绑定 OpenCode/OpenRouter，导致实际可用的自定义
  中转站没有被路由，`gpt-6.1-sol` 等请求会直接返回
  `model group has no available members` 的问题。
- 模型路由组现在支持把 `Auto/<模型>` 写成动态成员，自动展开到所有实际提供该模型、
  且未被平台黑名单排除的在线平台；显式成员与动态结果会去重。
- `Auto` 成员的组内优先级用于调整整个 Auto 池的位置，Auto 池内部继续使用各平台
  自己的全局优先级和 `priority_schedule` 排序。
- 打包时的面板冒烟测试改用 DevTools 协议驱动 Edge/Chrome，避免新版 Edge 的
  `--dump-dom` 不输出 DOM 时误判安装包不可用。

## [0.1.33] - 2026-10-10

### Added

- 内置五个 GPT-6+ 模型路由组，Codex 可直接使用不带平台前缀的原生名称：
  `gpt-6-astra`、`gpt-6.1-sol`、`gpt-6-sol`、`gpt-6-luna`、
  `gpt-6.1-sol-pro`；面板标记为“内置”，同名配置可覆盖或停用默认组。
- 模型价格目录、人工价格覆盖与用量成本统计，支持 CNY 计费、缓存读取单价和
  未知价格与免费模型的区分；Chat Completions 与 Responses 两条链路都会记账。
- 面板模型列表可显示上下文、输出上限、输入/输出/缓存价格及其来源，并支持恢复默认值。

### Changed

- 所有内置平台和自定义中转站启用会话粘性；缺少显式会话 ID 时，会从 system
  prompt 与首条 user 消息推导稳定的 `d-` 前缀键，以尽量命中上游提示缓存。
- 自定义中转站的会话粘性按“中转站 + 会话”绑定到账号池中的具体 Key；Key 被停用、
  冷却、移除或不支持当前模型时自动跳过该绑定。
- `openai-compat` 在面板继续显示为 **onmiRoute**，并纳入统一的粘性、计费和模型元数据流程。

### Fixed

- Cline、OpenCode、Loomy 和 onmiRoute 的流式响应中断处理：上游缺少终止帧时不再
  静默结束，流失败后会解除当前会话的账号绑定，下一轮可切换到其它账号。
- WorkBuddy 的 GPT 类模型上下文上限改为后端真实的 1,048,576，并把超长请求映射为
  `context_window_exceeded`，让调用方能触发压缩后续跑。
- 修复 Chat Completions 与 Responses 协议回归、模型路由组的默认项覆盖与面板编辑，
  以及缓存、成本和模型元数据在未知价格场景下被错误记为 `0` 的问题。

## [0.1.32] - 2026-10-10

### Added

- 新增热定义的 OpenAI 兼容中转站：面板“平台配置 → 添加中转站”可配置独立的平台
  ID、显示名称、API 地址、启用状态及输出上限字段，无需重启即可新增、编辑、停用或删除。
- 每个中转站拥有独立的多 Key 账号池；模型扫描用各 Key 请求上游目录，合并其可用模型，
  保留各账号的模型权限。扫描目录持久化，失败时保留上次成功结果，更换 API 地址后清除旧目录。
- 自定义中转站接入网关统一入站 Key、平台和账号优先级、模型分组、账号并发控制、
  用量记录与故障切换；上游模型名中的 `/` 保持不变。
- 整份备份包含中转站定义及账号数据；仅恢复自定义中转站的热配置和账号时无需重启。

### Changed

- OpenAI Compat 在面板显示为 **onmiRoute**；原有 `openai-compat` 配置、存储和调用地址
  继续兼容。相关页面按平台 ID 动态生成选项并显示自定义名称，不再依赖固定平台列表。
- 自定义中转站独立于内置模块的重启配置；页面提供账号池入口、扫描、编辑和删除操作。

### Fixed

- 删除、重建、修改或恢复账号池后，过期的模型扫描不能覆盖新数据；
  旧 Key 的测试结果不能清除替换后新 Key 的冷却状态，模型扫描也不会取消聊天限流冷却。
- 扫描模型时保留尚未保存的优先级及模型勾选；模型目录暂时为空时仍保留已有黑名单。
- 平台 ID 校验拒绝内置平台及保留名称，配置严格拒绝把账号凭据放进平台定义；
  上游回显的 API Key 与面板配置中的凭据均脱敏。
- 修复从中转站进入账号池后的地址栏同步，以及手机页面保存栏遮挡平台内容的问题。
- Chat Completions 流式输出省略未设置的 `delta.role`；Responses 保留必需的空文本、
  `annotations` 数组与函数参数字符串，并为所有流式事件提供连续的 `sequence_number`，
  提升严格协议客户端的兼容性。

### Upgrade Notes

- Windows 安装包为不含账号凭据与本机配置的空数据包；覆盖安装保留现有配置、账号池、
  用量和任务数据。升级由用户手动运行安装包完成。

## [0.1.31] - 2026-10-10

### Added

- 网关新增原生 `POST /v1/responses`（OpenAI Responses 协议），与
  `/v1/chat/completions` 共用同一批模型、同一套路由、故障转移、账号并发闸门和用量台账。
  Codex CLI 的 `WireApi` 只剩 `Responses` 一个取值，此前必须依赖中间翻译层；本接口让
  Codex 可以直连网关，链路里不再有会失败的那一步。
- Responses 请求翻译：`instructions` → 前置 system 消息；`input` 数组里的
  `function_call` / `function_call_output` 重新配对为「带 `tool_calls` 的 assistant
  回合 + 按 `call_id` 关联的 tool 回合」；`reasoning` 的 `summary` 保留为回合推理；
  `developer` 角色映射为 `system`；`tools` 同时接受扁平与嵌套两种写法；内置工具与未知
  item 类型跳过而不报错。
- Responses 响应渲染：流式按 Codex 解析器要求发出
  `response.created` → `output_text.delta` / `reasoning_summary_text.delta` /
  `function_call_arguments.delta` → `output_item.done` → `response.completed`
  （携带 `response.id` 与 `usage`），中途失败发 `response.failed` 而不是挂住；非流式
  返回单个 `response` 对象。
- **加密的 agent 内容不再被拒。** 这是本接口存在的核心原因：`agent_message` 保留明文
  信封与文本部分、密文替换为 `[encrypted agent payload omitted]`；`reasoning` 只保留
  `summary`，密文丢弃；`compaction` 仅在字段确为明文摘要时保留。密文既不转发给上游，
  也不解码成杜撰文本。
- 新增 `model_groups` 模型分组：为不同平台的模型 ID 声明等价关系，按组设置平台优先级，
  支持面板编辑、配置热生效、虚拟模型目录与跨平台故障转移；显式指定平台仍锁定该平台。
- 请求支持读取 `reasoning_effort`、Responses 风格 `reasoning.effort` 与
  `client2api.reasoning_effort`，用量记录和面板显示调用方实际请求的思考等级。

### Changed

- 网关的编排层（候选解析、会话粘性、平台故障转移、账号闸门、用量记录）从
  `handleChat` 抽出为协议无关的 `routeAndServe`，两个协议各自只提供「请求翻译器」和
  「响应渲染器」（`internal/gateway/dispatch.go`）。Chat Completions 行为不变，原有
  网关测试全部通过。

### Fixed

- Cline 在 `tool_choice` 缺省、`null` 或空白时保留调用方声明的工具，修复 WorkBuddy
  输出少量文本后结束、无法继续结构化工具调用的问题；显式 `"none"` 仍禁用工具。
- OpenCode 分离 Console 登录凭证与工作区推理凭证，读取工作区提供的 API key、推理地址
  和请求头；Zen 未启用或缺少配置时保留登录并明确显示未就绪，不再把登录 token
  错发给公共 Zen 接口并因 401 禁用账号。
- OpenCode 账号刷新和测试重新读取工作区配置，清除已撤回的推理凭证与模型列表，
  保留账号启用开关；免费请求避开 Console 账号，并正确处理工作区模型限制。
- OpenCode 本地并发满时等待空闲账号槽位，默认最多 `60s`（`queue_timeout`），
  重试共用等待期限并响应取消；跳过忙碌的粘性账号，失败尝试释放所有并发槽位。
- OpenCode 将 HTTP 429 中的 `server_error` 识别为上游故障，不再误当作账号限流；
  同一请求不会反复尝试同一个失败账号，冷却到期后恢复正确的可用状态显示。
- OpenCode 的流内错误、网络错误和 token 刷新错误统一对登录与推理凭证脱敏。
- Responses 续接保留 `output_text` 助手历史，支持带图片地址与 `detail` 的
  `input_image`；流式输出按首次出现顺序分配稳定的 item ID 与索引，在 delta 前发出
  item/part added，并在最终输出中保留推理摘要。
- Responses 在完成或错误事件后及时停止读取；达到 token 上限时返回
  `response.incomplete` 与 `max_output_tokens` 原因，不再把截断结果标为成功完成。
- Chat 与 Responses 的思考等级按与用量记录一致的优先级传给上游模块。
- 面板模型组中同一平台的优先级输入同步，避免后一行覆盖先前的编辑。

## [0.1.30] - 2026-10-10

### Fixed

- OpenCode 模型目录现在始终从公开的 `/zen/v1/models` 接口刷新，即使尚未配置账号也会取得最新目录；`*-free` 模型由接口返回的 ID 自动识别并标记免费，静态表仅保留作断网兜底和已知元数据。
- Cline 会解析 `recommended-models` 返回的完整免费模型对象，并把它作为目录项合并，修复 `cline-free/step-5-preview` 这类只存在于 `free` 数组、但不在普通模型列表中的免费模型缺失问题。

## [0.1.29] - 2026-10-10

### Added

- 面板统一为近似 Apple 工作台的视觉与交互：语义色、半透明材质、统一圆角与层级、
  页面/标签/弹窗过渡、按压和焦点反馈，覆盖全部 11 个主视图，并保留亮暗主题、键盘焦点和
  减少动画模式。
- 多平台补齐账号健康与池统计：codearts、openai-compat、tabbit、lobsterai、opencode
  现在能显示真实可用/冷却/禁用、在途请求和池状态；更多平台也接入了真实在途计数。
- 多平台加入会话粘性：同一会话优先复用第一次成功服务的账号，避免多轮对话在账号池中来回
  跳转；绑定账号不可用或降级时才自动切回正常路由。
- Qoder CN 补齐对话、在线模型目录、套餐明细、活动任务板、任务领取、接码自动登录、会话粘性、
  健康状态和池统计；账号测试发送一次最小真实对话请求，不再只做只读探测。
- cline、kimi、loomy、qwenwork、raccoon 等平台的账号测试统一改成真实最小对话请求，只有账号
  能实际回答时才通过。
- Windows 安装器记住“开机启动”选择，升级时不会重置；安装器改为 GUI 子系统，双击安装不再
  闪现控制台窗口。

### Changed

- 面板和安装器持续保留现有配置、账号池、用量与任务数据；升级仍由用户手动运行安装包完成。
- 网关重启替换进程时隐藏子进程窗口，减少 Windows 托盘重启时的界面闪动。

### Fixed

- 修复所有客户端补齐健康能力后，空账号配置让安装前真实浏览器自检把网关正常的
  `503 unavailable` 误判为启动失败的问题；自检现在接受契约内的未就绪响应，但仍校验健康
  JSON 与面板壳。


## [0.1.28] - 2026-10-09

### Added

- 平台配置新增分时优先级 `priority_schedule`：可以按北京时间配置多段时间，在指定时段覆盖
  平台基础优先级；支持跨零点，规则不能重叠，面板会校验后再保存。
- WorkBuddy 对互相矛盾的余额结果增加了自动判定：展示保守值，但正数直接标记可用；矛盾零值
  会走一次真实 Auto 模型测试，成功标可用，失败则套用冷却、额度或风控状态。后台扫描会缓存
  这个判定 6 小时，避免反复探测厂商。

### Changed

- 面板刷新按钮统一到顶栏一个常驻入口，按当前页面刷新，共覆盖 11 个视图；对话测试和运行日志
  补上了刷新，卡片里重复的刷新按钮已撤掉。
- 任务中心的定时任务改为按平台 Tab 展示，移除单独的“全局默认”编辑块；每个平台直接编辑
  自己的任务，未保存改动有明确提示，统一通过“保存并生效”提交。

## [0.1.27] - 2026-10-09

### Added

- 账号池新增状态筛选和多列排序，分页默认 50 条，可选 20 / 50 / 100 / 200；空值排在最后，
  后台轮询不会重置页码和滚动位置。
- 每个平台新增恢复探测定时任务：默认每 4 小时一次，支持 ±1 小时随机抖动，并可通过后台探测
  闸门避免频繁打扰冷却中的账号。
- 新增每日余额刷新任务：默认每天 0 点后的 30 分钟窗口内随机触发，获得每日额度的账号会
  自动恢复调度。
- 任务看板缓存每个账号最近一次任务列表，新账号才主动拉取；新增“刷新任务状态”按钮，任务全部
  完成或只剩人工/未解锁项时账号标签变绿。
- Tabbit 每日签到补上必需的 `request_no` 幂等键；Raccoon 明确桌面登录发放接口就是每日 300
  积分领取入口。

### Changed

- WorkBuddy 将平台风控和账号异常拆成两个独立状态；旧 `account_fault` 数据升级后按风控显示。
- Loomy 自动添加账号会避开本轮已经尝试过的号码，不再被接码平台反复返回同一号码拖住。

## [0.1.26] - 2026-10-08

### Fixed

- 自动添加账号时，批次会避开本轮已经尝试过的手机号，不再因为接码平台反复返回同一个号码而在失败号上循环空转。
- Loomy 首次登录和成长任务改为按账号串行执行，每个任务之间保留约 8–12 秒间隔，避免任务在一秒内集中完成而触发厂商反滥用检查。
- 任务中心统一同一账号任务的执行顺序，定时任务、批量执行和任务看板共用账号范围与完成状态，不再重叠执行同一账号的任务。
- 添加账号对话框的接码平台状态增加加载转圈、失败标红和「重新读取」按钮，读取失败后可以直接重试。

### Changed

- 更新 Loomy 与模块契约文档，明确 `TaskGap`、批次避让号码和账号级串行执行的约定。

## [0.1.25] - 2026-10-08

### Changed

- 任务中心改为账号优先：定时任务、批量执行和任务看板共享账号选择，切换账号后可以直接查看该账号的任务状态。
- Loomy 成长任务接入定时任务、批量执行和任务看板，不再出现看板已完成但账号维度没有联动的情况。
- 定时任务支持账号范围（include/exclude）和 `pending_only`，可以只对指定账号执行，或只处理尚未完成的任务。
- WorkBuddy 将风控和账号异常拆分为独立状态：平台风控等待解除或申诉，凭据失效等可恢复问题仍显示重新登录；旧 `account_fault` 数据兼容显示为风控。
- raccoon 修正每日积分的说明和按钮，桌面登录发放接口按每日 300 积分领取接口处理，重复领取会记录为服务端拒绝。
- 任务中心修正 ZCode 乱码提示，并补齐 WorkBuddy 人工任务的中文说明。
- 旧版本任务运行记录没有保存拒绝原因时，面板会明确提示为旧版本记录；升级后的新记录会带上供应商返回。

## [0.1.24] - 2026-10-08

### Changed

- WorkBuddy 接码重登改为单账号全自动流程：隐藏客户端选择、批量目标、重试次数和
  手动取号配置，只保留重登进度、停止操作和接码平台 Token 设置。
- 普通「添加账号」仍保留完整批量与手动接码控件，打开弹窗时会自动恢复。

### Fixed

- WorkBuddy 接码重登不再把接码平台子步骤超时误报为“整个流程超时”；重新占用原号码
  超时会明确显示为“重新占用号码超时”，并保留取消状态。
- 补充接码超时和精简重登弹窗的回归测试。

## [0.1.21] - 2026-10-07

### Added

- QwenWork 账号显示积分余额：通过官方 account-context 接口读取套餐与剩余额度，支持
  `quota.user_quota`、`quota.userQuota` 和直接 `quota` 等返回格式；401 时会先刷新
  token 再重试一次。余额刷新不会改变账号池的 LRU 顺序。
- Tabbit 支持重新导入本机凭据：浏览器里已经登录的新 Cookie 可以覆盖过期账号，不再
  需要先删除旧账号再添加。
- 账号池余额列支持点击排序，可切换升序和降序。
- 手动添加账号表单补充操作指引，说明各平台需要填写的凭据和获取方式。

### Changed

- 账号池把账号标识与操作员备注分开显示，避免备注列显示不全或与操作按钮里的备注
  含义混淆。
- 账号状态的提示文案统一说明可用、冷却、禁用和失效的流转规则，减少状态看起来
  “有余额但仍是冷却中”的误解。

## [0.1.18] - 2026-10-06

### Added

- openai-compat 新增「一键连接本机服务」：模块通过新的 `core.QuickConnectProvider`
  能力声明可以一键接上的本机服务（目前是 OmniRoute），面板在「添加账号 → 手动添加」
  顶部渲染卡片。点「检测服务」只探测（`POST …/quick-connect/<id>/probe`，只读，不改
  任何状态），点「一键连接」才建档（`POST …/quick-connect/<id>/connect`），走的是和手
  动添加同一条 `AddAccount` 路径并顺手测一次。
- openai-compat 允许回环地址（`127.0.0.1` / `localhost` / `::1`）留空 API key：本机
  OmniRoute 开箱免 Key。留空时不再发 `Authorization: Bearer `（空凭据会被部分服务
  拒），非回环地址留空 Key 仍然照旧报错。
- openai-compat 的 provider 字段从「下拉框」改成「输入框 + 建议列表」：内置供应商仍
  可一键选中，同时也允许操作员直接填任意 id。

### Fixed

- 厂商登录表单放在 iframe 里时面板不再找不到输入框：CDP 的元素查找、点击、填写以及
  页面文本读取都会进同源 iframe 里找（WorkBuddy / CodeBuddy 新版登录页就是这种结构，
  之前会报「找不到手机号输入框」）。
- WorkBuddy 自动登录的页面选择器放宽，兼容 codebuddy.cn 改版后的手机号、验证码与
  提交按钮。
- ZCode 领取活动套餐成功后清掉该账号之前「额度耗尽」的判定，账号立刻回到轮转，不再
  需要手动点「恢复」；面板余额刷新读到有额度时也会清掉这类判定。
- 账号池后台余额扫描只清不会自愈的终局判定（`exhausted` / `low_credit` /
  `quota_exceeded`），不再擅自清掉厂商明确给了冷却的限流（`cooling` /
  `rate_limited`）；后者只有操作员显式点「刷新余额 / 余额」才会提前解除。
- 自动添加账号运行期间弹层不再能被误关：关闭按钮变灰并提示「先点停止」，客户端
  下拉与页签同样锁定，避免鼠标点一下外面就把跑着的任务丢掉。

### Changed

- 面板新增「重复元素 id」守卫测试：`id="..."` 在整份 shell 里出现两次就失败。一键
  连接的卡片一开始撞上了任务中心同名的 `qcList`，点「检测服务」完全没反应，就是这条
  守卫要挡住的 bug。

## [0.1.17] - 2026-10-06

### Added

- ZCode 面板登录分成两个通道：「国际版」走 Z.AI（chat.z.ai），「国内版」走 BigModel
  （bigmodel.cn）。两者是不同服务，同一张手机号不能通用：国内号必须在「国内版」
  页面登录，用国际版会通过人机校验后报「请求失败」。模块通过新的
  `core.RealmLoginProvider` 把两个通道交给面板，登录后的账号带 `realm` 标签区分
  国内/国际，并按对应服务存 provider 与 base URL。
- 打包配置给 zcode 的签到单独设了时间表 `[0, 9, 12, 18, 21]`：它的每日活动额度是
  限量的，`0` 点对齐上海日切；预览是不带验证码的 GET，只有真领到时才唤起浏览器，
  所以多跑几次成本很低。可在「任务中心 → 时间表」里改。
- 账号池「全部签到」显示实时进度（`签到 1/N`）并统计成功/已签到/失败。

### Changed

- WorkBuddy 自动登录的面板日志把最后一行的结果说清楚：成功写「登录成功，已添加
  账号 <id>」，失败写「失败：<原因>」，不会再停在中间那步。`browser_headless`
  为真时会注明窗口是有意隐藏的；选择器定位失败会带上当前页面标题和 URL，方便不着
  截图就能区分「页面没加载」和「厂商改版」。

### Fixed

- ZCode 验证码浏览器窗口按每次启动唯一的 `--user-data-dir` profile 匹配并回收，
  跑完立刻释放，不再越开越多，也不会把窗口留在桌面上（Chromium 重启主进程换了
  pid 也照样能认出来）。
- ZCode 活动套餐领取只在账号的 JWT 通道那一行出现：coding-plan API key 是它的兄弟
  通道，计划账单接口不认，按钮不再显示（后台定时批次仍会解析到同账号的 JWT）。
- ZCode 账号名每次启动都从凭据重新生成，旧版本写下的 state 文件不能再把它钉死，
  修复了「最近调用里账号还是 JWT」这类看不懂的显示。
- 账号池的「全部刷新」「刷新余额」不再互相抢占名额、也不再静默无反应：刷新状态
  按客户端各自维护，按钮显示 `刷新中 3/20`，被节流时提示「请 X 秒后再试」。
- 单账号点「余额」或批量刷新读到余额为正（或无限额度）时，处在冷却/耗尽/限流等
  状态的账号会自动恢复，不用再手点「测试」；`invalid` 和人工禁用的账号不自动恢复。
- 账号池顶部磁贴的冷却/禁用统计改用与表格同一条 `accountState` 口径，不再出现下面
  显示「冷却中」的行被上面记成「禁用」、数字对不上的问题。

## [0.1.16] - 2026-10-05

- 模型上下文预设可在模型页面查看和编辑，默认取官方上下文长度。
- ZCode 验证码浏览器窗口与账号身份识别的清理。

## [0.1.15] - 2026-10-05

- 账号池分页。
- tabbit 每日签到（每天给 3% 额度）。
- 用量总览卡片与图表优化。

## [0.1.14] - 2026-10-05

### Added

- openai-compat 在面板里可以直接从平台拿到 API Key：手动添加表单的 provider 下拉
  下面新增一条「到该平台控制台创建一把 API Key」的跳转链接，随选择的供应商切换。
  groq / cerebras / siliconflow / mistral / nvidia / together / fireworks / deepinfra /
  chutes / huggingface 都配好了申请页；模块通过新的 `core.KeyPageProvider` 能力把这张
  表交给面板，新增供应商仍然只改一处。
- 「用量 → 最近调用」新增「会话ID」列：记录调用方自己带来的会话标识（`metadata.conversation_id`
  / 顶层 `conversation_id` / 选项里的 `conversation_id`、`conversationId`、`prompt_cache_key`），
  都没有时退回 `X-Conversation-Request-ID`；请求里的 `user` 字段不会被当成会话。列里的值太长
  会截断，完整值放在单元格 title 里。

### Fixed

- 积分构成视图按厂商账号合并多条通道：此前 zcode 同一账号的 API key 与 JWT 通道
  在该视图分成两行，看起来像两个账号，1 亿额度只在其中一行可见。现在分组规则与
  账号池一致（identity），组头显示操作员备注或账号名，卡片、到期分布和批次明细
  各只出一份；组内通道名以小字列出，单通道账号的显示完全不变。
- 「用量 → 最近调用」的账号列不再只显示凭据类型名（例如 zcode 的 `ZCode plan JWT`）：
  账号名改成和账号池的账号列同一口径——操作员备注（手机号/邮箱）优先，其次模块 label，
  最后裸 id；鼠标悬停还能看到裸 id、label 与厂商 identity。这条对所有平台生效。

## [0.1.13] - 2026-10-05

### Fixed

- minimaxcode 面板登录的账号不再每小时要求重登：这类账号没有桌面端的 `auth.json`，
  续期逻辑此前因此拒绝为它刷新，约 1 小时的 access token 一过期就被判成 `login_required`。
  现在面板登录的行改用自己的 `managed_accounts.json` 作为凭据落盘位置，并在换发后立即写回，
  旋转后的 refresh token 不会因为进程重启而丢失。
- minimaxcode 重启不再把健康的托管账号误判为未登录：`accounts.json` 里遗留的同 id 旧记录
  与空的桌面端目录，都不再被贴到面板登录的账号上。
- 令牌续期成功即清掉该行的失败次数与冷却，残留的惩罚状态不会再让账号被跳过。

### Changed

- WorkBuddy 的号池与流量治理参数（`pool.*` / `cooldown.*`）从「配置」页移到「平台配置」
  的 WorkBuddy 卡片里：这些参数只对 WorkBuddy 生效，放进平台卡片后不再和网关级并发上限
  混在一起。磁盘上的 JSON 键不变，旧配置不受影响。

## [0.1.12] - 2026-10-05

### Added

- 账号池每行新增「重登」按钮：模块判定某个凭据已被上游吊销（`401` / 失效 / 未授权）时，
  面板把该行标成「禁用」并直接给出「重登」，重走一次厂商自己的登录流程后就地覆盖旧凭据，
  不新增账号。首个接入的模块是 trae。
- 账号新增「备注」：登录该账号使用的手机号或邮箱，写在 `platforms.<name>.account_notes`
  里，面板账号行把它当标题显示，重登时用来确认操作的是哪个号。备注只是展示元数据，
  不参与路由，改完热生效。
- 新增通用 OpenAI 兼容模块 `openai-compat`：一份配置聚合 Groq、Cerebras、SiliconFlow、
  Mistral、NVIDIA NIM、Together、Fireworks、DeepInfra、Chutes、HuggingFace 等
  OpenAI 形状的免费额度，按 `openai-compat/<provider>/<model>` 路由；某个 provider
  被限流或判死时自动轮换到下一个。
- zcode 的 JWT 通道内置 Edge/Chrome 验证码求解器：普通安装不再需要 `captcha_command`
  就能签到、领取；`captcha_command` 作为无头环境的兜底，且同一次求解结果会在 TTL 内
  复用，不会每个请求都起浏览器。zcode 同时接入面板登录（OAuth）。
- 托盘菜单新增「关于」，显示当前版本、面板地址与数据目录。

### Changed

- 账号池「余额」列改为读缓存：打开面板先显示各厂商上次给出的数字，不再一进去就全量
  刷新。页面每次只顺带更新 3 个最久未更新的账号（间隔 2 秒、整体 45 秒节流一次），
  显式的 `POST …/balances/refresh` 用 5 秒节流；从没读到过的账号显示 `-` 而不是 `0`。
  缓存写在 `data/panel/balance_cache.json`，安装升级会保留。

### Fixed

- trae 的 HTTP `401` 不再被当成「等一会就好」的限流：`401` 直接判死凭据并停用，
  不再每 60 秒把它重新挑出来继续打流量（此前线上有账号带着 37 次失败仍然显示「可用」）。
  响应体里的业务码仍原样保留给操作者排查；非 401 的 1001/4010 行为不变。
- zcode 账号池摘要按厂商身份聚合，账号列表不再把同一个登录显示成好几条。

## [0.1.11] - 2026-10-05

### Fixed

- Cline 免费档模型下架后目录不再残留：`recommended-models` 成功应答时，其 `free`
  数组是免费 ID 的完整真相，内置兜底表里未被佐证的条目（如被挪到 `cline-pass/*` 的
  `cline-free/deepseek-v4.1-flash`）会在刷新后移除，不再每 5 分钟被重新放回目录。
- 裸名 / `Auto/` 路由遇到上游 `404 model not found` 时改走失败切换：路由器自己选的
  平台服务不了该模型就短暂降级并尝试下一个候选平台，不再把 404 直接丢给调用方；
  显式写成 `cline/...` 的请求保持如实报错。
- WorkBuddy 聊天不再泄漏每账号在飞槽位：剥离 `cn:` realm 前缀时不再浅拷贝请求，
  避免拷贝携带网关的账号租约导致槽位永不释放、账号在若干次成功后永久 429。
## [0.1.10] - 2026-10-05

### Added

- `/v1/models` 新增聚合虚拟模型 `Auto/<模型名>`：按现有等价规则把所有平台目录合并成
  一个统一名字，请求它时按平台优先级、账号健康度与冷却状态自动选路并失败切换；原有
  的平台限定模型名（`zcode/GLM-5.3` 等）保持不变。
- 添加账号的接码区新增 eomsg 邀请注册链接。
- 用量 → 最近调用新增刷新按钮。
- 一键自动添加支持设置期望账号数与最大尝试次数，并显示批次进度。

### Changed

- 自动任务配置统一到任务中心：全局默认时点、余额刷新间隔与平台覆盖都在这里编辑，配置页
  不再重复提供排程入口。
- WorkBuddy 浏览器登录不再因账号池已有可用账号而短路，可以继续添加第二个账号；
  同一个 uid 重新登录仍是就地更新。

### Fixed

- 账号池列表重新分配列宽：账号列不再吞掉剩余宽度，用量列加宽，用量与最近成功不再
  与相邻列重叠。
- 浏览器登录不再把“已可用”之类的终态消息渲染在“打开以下链接并登录”提示区。
- 平台暂时不可服务（账号繁忙/冷却）时不再每请求重试：短暂降权后仍保留在候选列表，
  并在会话粘性判断中让健康平台优先。

## [0.1.9] - 2026-10-04

### Fixed

- 修复部分机器从安装器隐藏进程启动 Edge/Chrome headless 自检时拿不到 DOM、
  导致安装被误判为失败的问题。
- 新增构建时真实浏览器门禁摘要标记：仅当候选程序与已通过门禁的安装包逐字节一致
  时，目标机浏览器不可用才允许继续；哈希不一致仍然拒绝并回滚。
- 安装失败现在会写入 `%LOCALAPPDATA%\client2api\install-error.log`，便于定位现场问题。

### Changed

- 保留 0.1.7/0.1.8 的安装前后自检、事务式备份和失败回滚；本版只放宽浏览器环境
  导致的误报，不放宽二进制完整性校验。

## [0.1.8] - 2026-10-04

### Fixed

- 统一内置系统提示词的换行符为 LF，并锁定规范化后的固定字节，修复 Windows 与
  Linux 构建使用不同默认提示词、Linux CI 断言失败的问题。
- 修复 Kimi 离线测试脚本在清空 `PATH` 后仍调用 `cat`/`sleep`，导致 Linux CI
  拿不到桩输出而失败的问题；非 Windows 脚本改用 `/bin/cat` 与 `/bin/sleep`。

### Changed

- 保留 0.1.7 的事务式安装器、安装前后自检和失败回滚，本版用于恢复 GitHub Actions
  的完整五平台构建、校验和与 Release 产物发布。

## [0.1.7] - 2026-10-04

### Added

- 新增真实浏览器面板自检（`cmd/panelsmoke`）：在随机端口上启动候选网关，用无头
  Chrome/Edge 打开 `/panel/`，断言 `<html data-c2a-ready="1">` 出现且没有
  `data-c2a-boot-error`。控制面板在 `boot()` 跑完后打前一个标记，脚本语法错误或
  启动时抛错都会让自检失败，0.1.5 那种"服务端 200、页面全死"的问题再也不会漏过。
- 控制面板增加全局错误捕获：任何未捕获异常或未处理的 Promise 拒绝都会在页面底部
  显示红色错误条，不再是一片沉默的空白。
- 安装流程改为事务式：写入前先在临时目录自检候选程序，通过后才停止旧进程并备份
  将被覆盖的程序文件；写入后再自检一次，任何一步失败都会把程序文件回滚到安装前
  的版本，账号池与配置不受影响。

### Changed

- `installer/build.ps1` 在打包安装器之前先跑一次真实浏览器自检，失败即终止构建
  （构建侧 `-SkipSmoke`、安装器侧 `-skip-self-check` 可显式跳过，仅在确实没有
  浏览器时使用）。安装包新增 `panelsmoke.exe`，与 `probe.exe` 一样随安装目录
  一起分发。
- CI 的 `go-binaries` 工作流在面板内联脚本的 Node 语法检查之后，增加真实浏览器
  自检，防止同类问题再次进入 tag 和 Release。

### Fixed

- 修复升级可能把可用版本替换成坏版本的问题：即使新的安装包本身仍有缺陷，安装器
  也会在动旧文件之前发现并拒绝，或者自动回滚。

## [0.1.6] - 2026-10-04

### Fixed

- 修复 0.1.5 控制台面板的内联脚本多出一个 `}`、导致浏览器整段 JavaScript
  无法解析、面板空白或无法交互的问题。
- CI 增加面板内联脚本的 JavaScript 语法检查，防止同类问题再次进入安装包。
- 修复 CI 的 `gofmt` 门禁失败，恢复 main/tag 的自动构建流程。

## [0.1.5] - 2026-10-04

### Added

- 账号池平台标签的账号数字改为 `可用/总数`，例如 `1/2`；可用只计绿色 `可用`
  状态，冷却中和禁用不计入前一个数。
- 平台告警页新增“清空”按钮（`DELETE /panel/api/alerts`），可一键删除全部告警
  并持久化清空结果，避免告警数据持续堆积。

### Fixed

- 最近调用记录的账号列优先显示手机号/标签/昵称/邮箱，其次 User ID 等唯一标识，
  最后才回退到原始账号 ID，不再出现空白或看不懂的机器 ID。
- 账号池状态统一为中文三态：`可用`、`冷却中`、`禁用`。冷却中表示额度/限流等可恢复
  问题，凭据仍正常；禁用表示人工停用或需要重新登录的失效凭据。
- 优化账号池列宽：优先级列收窄到 64px、输入框 52px，其他固定列也给明确宽度，
  账号名称列自动吃掉剩余空间，减少横向滚动。
- 修复任务中心的自动签到会跳过“额度耗尽”账号的问题。签到批次现在会扫描
  `ready` / `cooling` / `exhausted` 等可恢复状态的账号，只排除人工停用和
  凭据已失效（invalid/expired/unauthorized）的账号。
- 修复平台间路由过早切换的问题：现在严格按“平台优先级 → 平台内账号优先级 →
  下一平台”执行。模块只有在当前平台的可用账号全部尝试失败后才会报告平台耗尽，
  网关收到该信号才切换平台；会话粘性也不会让低优先级账号压过高优先级账号。

## [0.1.4] - 2026-10-04

### Fixed

- 修复 Trae 余额少算的问题：面板之前只统计带 `enable_solo_*` 标记的积分包，
  会把没有标记但仍可使用的福利额度丢掉，出现“显示 0 积分但还能继续调用”。
  现在优先使用厂商的 `usage_summary`，并支持 `-1` 无限额度。
- 修复 `pool.expiring_soon` 热更新后 WorkBuddy 仍短暂使用旧到期快照的问题：窗口变化时
  会立即清空旧快照，账号池、余额刷新与面板统一读取新窗口。
- 修复会话粘性只粘账号、不粘平台的问题：裸模型名在多个平台间会来回切换。
  现在同一会话会优先复用上次成功的平台，只有该平台不可用才切换，并把新成功平台写回绑定。
- 修复平台告警与最近调用不一致：候选平台失败后如果切换到其他平台成功，失败尝试现在会在最近调用中显示为“候选失败”，但不会增加请求/失败总量。
### Changed

- 平台级并发上限与平台内单账号并发上限的默认值改为 2；显式填写 `0` 仍然表示不限制。


### Added

- 账号池支持按账号配置调用优先级：`platforms.<平台>.account_priorities.<账号ID>`，
  数字越小越先使用；同优先级继续保留原有轮询/权重策略，低优先级账号不可用时自动回退。
  账号池页面可直接编辑并热加载。
- zcode 新增监控额度通道：读取 `open.bigmodel.cn/api/monitor/usage/quota/limit` 与
  `subscription/list`，展示 5 小时窗口、每日 Token 窗口、套餐等级与重置时间。
- zcode 的 JWT、coding-plan API key、OAuth 派生 key 现在按同一账号身份聚合为多条
  通道，余额与套餐明细会合并展示。
- 活动套餐领取支持跨通道解析：从 API-key 行点击时会找到同账号 JWT 通道，从 JWT 行
  点击时也会使用同账号可用通道。

### Fixed

- 修复 zcode 只读取 `billing/balance`、因此看不到 5 小时额度的问题。
- 修复配置中截断的 API key 覆盖凭据库完整 key 的问题。
- 修复监控请求携带 ZCode 身份头导致厂商返回“当前用户不存在coding plan”的问题；
  监控请求改为 Bearer + User-Agent + x-request-id。
## [0.1.3] - 2026-10-04

### Added

- WorkBuddy 账号池新增低余额保护：已知积分/余额小于等于平台配置阈值时，账号会暂停参与
  模型路由，不再显示为绿色可用。
- 平台配置新增 `reserve_credits`（面板中的“最低余额”）：默认 `0` 表示已知零余额即暂停，
  正数表示保留余额，`-1` 表示关闭该平台的余额保护。
- 余额扫描与自动恢复：默认每 5 分钟刷新一次余额；签到、充值或手动刷新后，余额高于阈值
  的账号会自动恢复使用。
- WorkBuddy 账号状态新增“积分不足，已暂停”提示，并持久化暂停状态。

### Fixed

- 修复进程重启后，持久化的零余额账号在首次余额扫描前仍可能显示为可用的问题；平台策略
  安装时会立即重新评估账号状态。
- 余额未知的账号不会被暂停，避免新安装或首次启动把全部账号误判为零余额。

### Changed

- 平台策略支持热更新；修改最低余额后无需重启，账号池会立刻重新评估。
- README 增加中文项目简介、快速开始、数据安全、发布与更新约定。

### Upgrade Notes

- 覆盖安装会保留 `configs/client2api.json`、`data/` 和用量历史。
- 低余额账号被暂停后，也会暂时退出免费模型路由；如仍希望使用这些账号，可将对应平台的
  `reserve_credits` 设为 `-1`。
