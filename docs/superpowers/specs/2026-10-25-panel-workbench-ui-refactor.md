# 面板「精密仪器」交互与视觉重构（2026-10-25）

## 北极星

把这套面板做成一台**精密仪器**：数据是主角，控件退到环境里，每次操作立刻有回应。

方向来自品类本身：client2api 是一个本地 API 网关的运维控制台，用户每天要扫 100+ 个上游账号、看用量走势、改平台路由、盯任务队列。它不该像展示页，也不该像一堆通用后台组件堆出来的表单。判断标准只有一条：**能不能更快地看出「现在哪里不对」，然后就地处理。**

## 范围

覆盖 `internal/panel/index.html` 的 11 个主视图（账号池 / 对话测试 / 用量 / 平台告警 / 积分构成 / 客户端 / 任务中心 / 模型与档位 / 平台配置 / 配置 / 运行日志），以及共享结构：侧栏、顶栏、统计带、面板、表格、工具条、分页、按钮、输入、标签、二级标签、弹窗、Toast、保存条。

## 非目标

- 不改 API 路径、请求体、字段名、路由策略、权限、业务行为。
- 不新增/删除页面，不改变导航名称与顺序。
- 不引入前端框架、构建步骤、外部字体或网络依赖。
- 不改动 DOM id、`$("#id")` 引用、`aria-*`、`role`、`tabindex` 与现有键盘约定。

## 视觉语言

单一权威层写在 `index.html` 的 `<style>` 末尾（`authoritative workbench layer`），按「令牌 → 壳层 → 控件 → 表格 → 面板 → 状态 → 弹层 → 动效 → 响应式」排列，后写覆盖先写。此前多轮补丁层保留不动，避免一次大改牵动 34 个 UI 回归测试；冲突一律以本层为准。

### 令牌

- 间距 `--sp-1..6`、控件高度 `--ctl-h`、字号 `--type-xs..lg`、圆角 `--radius-ctl/card/pill`。
- 明暗两套独立语义色：`--bg / --surface / --surface-2 / --raise / --line / --line-soft / --ink / --ink-2 / --ink-3 / --accent* / --ok* / --warn* / --bad*`。
- `--accent` 是交互与强调；`--accent-solid` 是承载白字的实心主色。两者分开，是因为亮蓝配白字只有 2.9:1，而主按钮必须 ≥4.5:1。
- 动效 `--dur / --dur-slow / --ease`，只动 `transform / opacity / 颜色`。

### 对比度（实测）

| 组合 | 比值 |
| --- | --- |
| 深色 ink / bg | 17.4 |
| 深色 ink-3 / surface | 5.95 |
| 深色 accent-text / surface | 8.9 |
| 深色 白字 / accent-solid | 4.86 |
| 浅色 ink / bg | 15.9 |
| 浅色 ink-3 / bg | 4.81 |
| 浅色 白字 / accent-solid | 5.32 |

## 关键改动

### 1. 账号池：一屏放得下更多账号（本次最大的可用性修复）

实测改版前 1440px 下每行 **159px**、375px 下 **188px**，一屏只看得到 2 个账号。根因是操作列只占 10% 宽（约 115px），6 个按钮竖着排成 3 行，把整行撑高。

- 操作列放宽到 22.5%，按钮改成固定 3 列网格（窄屏 2 列），行高回落到 **69px / 99px**。
- 13 列宽度重新配平到正好 100%（单条配方，不再分散在多个补丁里）。
- 1200px 以下让表格保持 1040px 可读宽度，横向滚动留在 `.tbl-wrap` 内，粘性首列继续钉住账号名。
- 模型列的「可用 N」改成「N 个」，避免和左侧状态标签「可用」并排时读成同一个意思。

### 2. 壳层与控件

- 侧栏 216px，激活项用 3px 竖条 + 浅色底，不再靠整块高饱和蓝。
- 顶栏 56px，标题 / 元信息 / 动作分三层；窄屏隐藏元信息、动作换行。
- 按钮、输入、下拉统一 34px 高与 9px 圆角；二级标签改成真正的分段控件。
- 焦点环统一 `0 0 0 3px`，覆盖 button / input / a / [tabindex] / summary。

### 3. 表格与面板

- 表头吸顶、首列（账号 / 模型）吸左，数字列 tabular-nums。
- 行 hover 用 5% 强调色底，不再整行变灰。
- 面板统一 14px 圆角 + 一条 hairline + 一层极浅投影；`.box` 继续 `overflow: clip`（`savebar` 的粘底依赖它，测试也钉着）。

### 4. 状态语义

- 计数为 0 的告警格（风控 / 账号异常 / 已停用）不再发红，只有正数才用语义色。
- 能力矩阵里「是 / 就绪」降为安静的勾选文字，只有「未就绪」保留红底，一眼能找到偏差。
- 接口原值 `ready / no` 映射为「就绪 / 未就绪」；模型 id、账号标识等数据值保持原样。

### 5. 弹窗、表单与空态

- 弹窗头部改成「标题 + 分段标签」一行（此前 h3 是块级元素，标签被迫换行并留出大片空白）。
- 补上通用 `.fld`：此前只给 `label.fld` 写了样式，「添加账号」用的是 `<div class="fld">`，标签和控件一直贴在一起。
- 对话测试的空态从通用聊天引导改成「连通性 / 结构化输出 / 报错解释」三个测试意图。
- 用量占比改成一位小数，极小非零值写 `<0.1%`：此前 `Math.round` 把 0.4% 印成 0%、99.6% 印成 100%，看起来和「合计」自相矛盾。

### 6. 动效

- 视图切换 260ms 淡入 + 6px 上移；二级标签 150ms；弹窗 260ms 缩放淡入；按钮/标签 150ms。
- `prefers-reduced-motion: reduce` 下全部动画关闭（沿用并扩充原有规则）。

## 响应式

375 / 760 / 1024 / 1440 / 2200 五档实测无页面级横向滚动。窄屏统计带保持两列（改版前 375px 是单列，9 个格子会把账号表推到折叠线以下）；导航变成横向工具带；宽表滚动限定在 `.tbl-wrap`。

## 验证

- `go build ./...` 通过；`go test ./...` 45 个包全绿（含 34 个面板 UI 回归测试）。
- 自建只读预览（`127.0.0.1:8799`，代理到运行中实例的只读 API），40 项交互检查全过、0 个运行时异常：11 个视图切换、搜索过滤与清空、表头排序、Esc 关弹窗、添加账号三个标签页、用量二级标签与 ARIA、刷新、主题切换。
- 截图：11 个视图桌面版 + 账号池/日志浅色 + 375px 窄屏 + 200% 缩放 + 改版前后并排对比。
- 已知与画面无关的环境现象：本机 `/panel/api/alerts` 返回 403、`/v1/models` 404、`/panel/api/clients/doubaoime/accounts` 501，均为既有接口行为，不是本次改动引入。

## 未做

- 模型表在 1440px 下的多列裁切与模型 ID 三行折行是改版前既有问题，未在本轮改动表格列结构。
- 顶部 KPI 是「全部客户端」口径、状态筛选是「当前客户端」口径，本轮只补了空态提示（「上方计数含全部客户端」），没有改统计口径。
- 任务中心平台筛选胶囊、平台配置底部常驻说明条的收纳，属于信息架构调整，留待后续。


---

# 第二轮：信息架构与交互重做

用户明确要求「交互、层级、布局都可以完全重来，不必参考现在的形态，只要功能还在」，
于是这一轮不再只是换皮，重新决定了导航模型、落地页和几个交互方式。

## 与上一版的差别

| 维度 | 上一版 | 这一版 |
| --- | --- | --- |
| 导航 | 11 项平铺一级列表 | 4 组收纳（总览 / 资源 / 流量 / 系统）+ 计数徽标 |
| 落地页 | 直接落到账号表 | **总览页**：健康度环 + 待处理清单 + KPI + 构成条 + 快捷入口 |
| 顶栏 | 标题 / 分隔符 / 长串元信息 | 标题+副标题 / 动作区；新增命令面板入口 |
| 账号状态筛选 | 只能去「状态」下拉 | **指标条本身就是筛选器**，再点一次取消 |
| 全局跳转 | 逐个点侧栏 | **命令面板 Ctrl/Cmd+K**：12 视图 + 全部客户端 + 常用动作 |

## 新增的交互

1. **总览页（默认落地）** —— 一个圆环回答「现在健不健康」，旁边就是「需要处理」清单；
   清单里的每一项都可点，直接跳到对应客户端的账号表。
2. **指标条即筛选器** —— 首屏那排状态数字不再只是读数；点「账号异常」就按该状态过滤。
   筛选后当前客户端没有匹配时，空态会列出**确实有这个状态的其它客户端**，一点直达。
3. **命令面板 Ctrl/Cmd+K** —— 12 个视图、全部客户端、刷新/切主题/添加账号都能一次输入到达；
   ↑↓ 选择、Enter 打开、Esc 关闭。只做导航与既有动作，不新开写数据的路径。
4. **宽表横向提示** —— 能往右滚的表格右侧出现渐隐，滚到底自动收起。
5. **保存条改为悬浮** —— 长表单滚到底时最后一行不再被保存条盖住。

## 修掉的既有缺陷

- 总览最初读了 `/overview` 里并不存在的 `usage` 字段（那个接口只回网关累计），
  已改成只画真实存在的请求/失败/日志行，不假装有一个 24 小时窗口切片。
- 命令面板与密钥弹窗同时存在时，DOM 顺序会让密钥弹窗盖住面板，Esc 也会被
  `data-lock` 的密钥框吞掉；已显式抬高层级并让 Esc 优先关面板。

## 验证

- `go build ./...` 通过；`go test ./...` 45 个包全绿。
- 真浏览器 smoke（panelsmoke + 无头 Edge）：面板干净启动，无 boot error。
- 自建只读预览 42 项交互 + 34 项命令面板/新交互检查全过、0 运行时异常。
- 375/760/1024/1440/2200 五档无页面级横向滚动。

## Review fixes (round 2, independent review 7/10)

1. The account pool expiry column collided with the status-note column: at 1440px it
   had 57px but needed 82px. Expiry goes 5% -> 7.5%, usage and note each give up a
   little, so the total is still 100% and nothing overlaps.
2. The overview read cooling as an alarm. Severity is now decided only by genuinely
   unavailable accounts: some unavailable = red, cooling only = neutral blue, clean = green.
3. Wide tables were hard-clipped on the right with no affordance. Added
   scrollbar-gutter, a right pad on the last column, a two-line clamp on model names,
   and a right-edge fade that appears only when the table can actually scroll.
4. The usage input/output series were nearly the same colour; input is now the accent
   and output a mix of accent and neutral, in both bars and legend.
5. Top bar no longer shows the per-second uptime; client filter chips are quieter.

Also fixed a defect I introduced earlier: the command palette and the API-key prompt
could both be open, and DOM order let the key prompt cover the palette while its
data-lock swallowed Esc. The palette now sits above it and Esc closes the palette first.
