# 先放行、被拒再裁剪：上下文守卫的探针式预算 - 设计

**日期：** 2026-10-22
**状态：** 待评审

## 目标

修掉上下文守卫目前最别扭的一处行为：网关只用自己的估算就给请求判死刑，把历史裁掉
再发给厂商。估算偏保守时，本来能被厂商接受的请求被无故缩短，调用方拿不到答案，而且
这个过程对调用方不可见。

改成：**先让厂商裁决，被拒再裁剪重试；成功就记下厂商实际服务过的规模。**

具体要达成的效果：

- 估算超过预算、但从未被厂商实测过的 `(平台, 模型)`，第一次不再主动裁剪，而是原样放行。
- 厂商接受了 → 正常返回，并把这次成功服务过的输入规模记为实测量。
- 厂商以「上下文超限」拒绝 → 沿用现有的学习 + 按实测比例裁剪 + 同一次请求内重试一次。
- 已经有实测量或已有厂商学到的窗口时，恢复现有的主动裁剪，不再每次都去试探。

## 背景与已核实的事实

### 触发这次讨论的现场证据

`D:\client2api\data\recent.json` 里有一条真实记录：

```text
at: 2026-10-22T10:10:35+08:00
model: raccoon/sn-deepseek-v4-1-flash-1017
trimmed: true
trim_dropped_messages: 117
trimmed_from_tokens: 1,043,449
trimmed_to_tokens: 988,062
context_window: 1,000,000
prompt_tokens: 961,759        # 上游实际收到
```

即：网关估算 1,043,449 超预算，丢掉最早的 117 条消息，厂商最终只收到 961,759。
厂商当时并未拒绝，是否本来就接受 1,043,449 无从得知——因为网关没给它机会。

### 现有代码事实

- `internal/gateway/context_guard.go:963`，`prepareCandidateAttempt` 在发请求**之前**调用
  `guardRequestContext`，主动裁剪发生在转发之前。
- `internal/gateway/context_guard.go:83`，`promptBudget` 的算法是：
  `(window - 512 - min(maxOutput, 32768)) * 0.92 / estimateFactor`。
  以 1,000,000 窗口、`raccoon/sn-deepseek-v4-1-flash-1017` 的因子 0.897 计算，
  实际预算约 **888k**，窗口的最后约 11% 从来不用。
- `internal/gateway/server.go:1235` 附近，`openStreamRecorded` **已经实现**了
  「厂商拒绝 → 学窗口 → 按实测比例裁剪 → 同一次请求内重试一次」的完整链路
  （`learnContextWindowFromFailure` + `retryBudgetForMeasuredRefusal` +
  `retryBudgetForUnmeasuredRefusal`）。它的问题不是不存在，而是**够不到**：
  请求在 `prepareCandidateAttempt` 里已经被裁到预算以内，厂商永远不会拒绝。
- `internal/gateway/context_guard.go:540`，`learnContextWindowFromFailure` 的注释写死了
  「只降不升」：厂商报的窗口只用来下调，绝不上调。
- `internal/gateway/context_guard.go:489`，`observedPromptFloor` 已经会扫描用量账本，
  取该模型**成功服务过**的最大 prompt，并在 `applyLearnFloor` 里按 4/5 作为下限，
  用来把厂商报得过小的数字抬回来。它读的是内存里最近 N 条用量环，重启后从磁盘重载。
- `internal/gateway/context_guard.go:653`，`effectiveContextWindow` 的优先级是：
  手工覆盖 > 学习窗 > 厂商/目录发布值。手工覆盖是操作者的最终决定。
- `internal/modelmeta/meta.go:40`，来源枚举里已有 `SourceLearned`（「vendors 自己在拒绝里说的」），
  比 `SourceVendor` 强、比 `SourceManual` 弱。
- `internal/gateway/responses.go:1097`，Responses 端点会在 `responsesWouldTrim` 为真时
  先做 auto-compact，并回带一个 `compaction` 项；裁剪与「折叠」用的是同一个预算数字。
- 用量环容量有限（`internal/gateway/usage.go:388`，`max` 条，`recent.json` 当前 100 条），
  因此 `observedPromptFloor` 只看得到最近的成功样本。

### 结论性的判断

「先放行」站得住脚，因为守卫自己在文件头就写了原则 *Never guess*：窗口未知时宁可原样
转发，也不要静默改写一个本来能成功的请求。现在的问题是这条原则只对「窗口未知」生效，
对「估算说超了」不生效。

但「成功就更新模型配置里的上下文长度」不能照用户说的直接写。一次 961,759 成功只证明
**这个账号、这个后端、这个时刻**服务过这么大；raccoon 是多账号池，不同账号可能落到不同
后端。直接写成广告窗口，等于把一次观测当成能力上限宣告出去，和现有的「只降不升」正面冲突。

## 范围

纳入范围：

- 在 `context_guard.go` 增加「探针式放行」决策，并把判定结果记录到用量行。
- 探测状态黏性：每个 `(平台, 模型)` 只探测一次，结果持久化，之后不再探测。
- 成功回写：把「已被厂商接受的最大估算规模」作为持久化的预算下限 `probeFloor`。
- 面板模型列表增加探测状态列与手动清除入口。
- 为上述行为增加开关，默认开启（见第 6 节）。
- 让 Responses 的 auto-compact 与探针式放行保持一致（不能让「会裁剪」的判断落空）。
- 单元测试与端到端测试。
- 文档：`README.md`、`docs/REFERENCE.zh-CN.md`、`CHANGELOG.md`。

明确不纳入：

- 不改 `learnContextWindowFromFailure` 的「只降不升」语义；它学的是厂商报的**上限**。
- 不把实测量写进 `modelmeta.OverrideStore`（那是操作者的手工决定，面板会标成 manual，
  还会被 `-NoData` 打包导出）。
- 不改任何模块（`clients/*`）的协议实现。
- 不碰正在运行的 `D:\client2api` 实例，不重打安装包。

## 组件设计

### 1. 探针式放行（probe）

新增 `probeDecision`，职责单一：回答「这个请求应该原样放行，还是按估算裁剪」。

判定顺序：

1. `promptBudget <= 0`（窗口未知）→ 放行。这是现有行为，不变。
2. 估算 `<= budget` → 放行。这是现有行为，不变。
3. 该 `(平台, 模型)` **已探测过**（见第 1.1 节）：
   - 探测结果是「厂商接受」→ 用探测时记录的**估算规模**当作预算。估算不超过它就原样放行，
     超过它才走现有裁剪路径。**不再探测。**
   - 探测结果是「厂商拒绝」→ 按已有学习窗走正常预算与裁剪。**不再探测。**
4. 其余情况 → **放行并探测**，标记 `rec.Probed = true`。

**没有「超出太多就不试探」的上限。** 估算器正是这里不可信的东西，所以它认为超了多大
都不构成跳过厂商的理由；多付的那一次往返，是换取「不误裁调用方历史」的代价。一次探测
的结果是黏性的，所以这个代价每个 `(平台, 模型)` 只付一次。

### 1.1 探测状态是黏性的

一个模型的窗口不会在两次请求之间改变，所以探测一次就够了。首次探测的出账
（无论厂商接受还是拒绝）都会把该 `(平台, 模型)` 标记为**已探测**，之后这个模型
永远不再走探测分支，除非操作者手动清除。

探测记录在 `observed_context_windows.json` 里，每个 `(平台, 模型)` 一条：

```json
{
  "version": 1,
  "probes": {
    "raccoon": {
      "sn-deepseek-v4-1-flash-1017": {
        "probed": true,
        "outcome": "accepted",
        "estimate_tokens": 1043449,
        "vendor_tokens": 961759,
        "at": "2026-10-22T10:10:35+08:00"
      }
    }
  }
}
```

字段含义：

| 字段 | 含义 |
|---|---|
| `outcome` | `accepted`（厂商原样接受了超预算请求）或 `refused`（厂商以超限拒绝） |
| `estimate_tokens` | 探测那次请求网关的**估算值**，即被证明的规模 |
| `vendor_tokens` | 厂商回报的 `prompt_tokens`，仅在接受时存在，用作估算器校准证据 |
| `at` | 探测完成时间 |

**为什么用 `estimate_tokens` 而不是厂商 token 数做后续判据：** 预算和裁剪都发生在
估算空间里（`applyContextBudget` 度量的是网关自己的估算）。用厂商口径的数字当预算，
还要先靠校准因子换算一次，而校准因子是滚动平均、会滞后——那正是这次误裁的成因之一。
直接记住「这个估算规模厂商接受了」，下次同样的请求就不会再被裁，判据精确、不需要换算。
`vendor_tokens` 保留下来是为了让面板显示真实规模，以及给 `estimate_calibration.go`
提供高质量样本。

### 2. 已接受的规模就是预算下限

一次探测若被厂商接受，就证明了「这个估算规模厂商吃得下」。这个数字本身就是判据：
后续请求的估算只要不超过它，就不再裁剪。

```text
budget = max(promptBudget, probeFloor)
```

`probeFloor` 是该 `(平台, 模型)` 已被证明可接受的**最大估算规模**。它与探测记录是同一条
数据：探测时接受 → 记下当次的估算值；之后每次成功返回，若当次估算值更大，就抬高它。

**为什么不做 4/5 折减：** 4/5 是为了给「厂商报的上限」留余量，因为那句话可能说窄了。
而这里记录的是**实际被服务过的规模**，是观测事实而不是声明，不需要再打折。打折反而会
让曾经成功的规模又被裁一次，正是这次要修的问题。

**为什么用估算口径而不是厂商 token 口径：** 预算和裁剪都发生在估算空间里
（`applyContextBudget` 度量的是网关自己的估算）。用厂商数字当预算还要靠滚动平均的校准
因子换算一次，会引入滞后——那正是本次误裁的成因之一。

**为什么仍然不写广告窗口：** 广告窗口会通过 `/v1/models` 下发给所有下游，包括 Codex
自己的上下文管理。一次账号级观测不该被当成全平台的能力宣告。放宽预算已能拿到全部好处
（不再误裁），又不承担误报风险。

探测结果为 `refused` 时不设 `probeFloor`：厂商已经说了吃不下，按学习窗走正常裁剪。

### 3. 手工覆盖的优先级

`effectiveContextWindow` 现有优先级不变：**手工覆盖 > 学习窗 > 目录/厂商值**。

实测量只参与「预算放宽」，不参与「窗口来源判定」。当操作者手工设了窗口时：

- 探针式放行仍然生效（否则手工设小了就永远没机会纠正）。
- 但实测量不得把预算抬到手写值之上。手写值是操作者的最终决定。

### 4. 与 Responses auto-compact 的一致性

顺序是 **探测/学习 → 压缩 → 裁剪**，前两步都不丢消息：

- **该 (平台, 模型) 还没学习过**：`responsesWouldTrim` 返回假，不压缩，让探针先把请求
  原样发给厂商。厂商接受 → 记 accepted floor；厂商拒绝 → `learnContextWindowFromFailure`
  学真实窗口并记 refused，然后按实测比例裁剪重试。学习是这一步的副产品，不能跳过。
- **已经学习过**：`responsesWouldTrim` 返回真，先尝试 auto-compact。`autoCompact` 成功时
  会把 `req.Messages` 换成「系统消息 + 摘要 + 最近若干轮」，并回带一个 compaction 项，
  请求随后按折叠后的规模发送。只有压缩失败（报错、摘要为空、blob 无法签发）才裁剪。

为什么不先压缩未学习的模型：摘要调用本身要用同一段完整历史发给同一个模型，厂商刚刚
或即将因为这段历史太长而拒绝，摘要大概率撞同一堵墙；而且先折叠会丢掉探针本来要验证的
那段历史。探测既便宜又能换来真实窗口，所以排在压缩前面。

### 5. 用量与可观测性

`UsageRecord` 增加探针相关字段（omitempty，默认不出现在正常行里）：

- `Probed bool` — 本次是探针式放行。
- `ProbedFromTokens int64` — 放行时网关的估算值。

调用记录沿用现有「已裁剪」标记的做法（`internal/panel/index.html:7265`），增加
「探针放行」标记，悬停显示估算规模与模型窗口。这样「网关为什么这次没裁」在面板上能直接看到。

另外在**模型列表**（面板的模型表格，每行一个 `(平台, 模型)`）增加一列探测状态：

| 显示 | 含义 |
|---|---|
| `已探测` | 该模型已经有上下文长度（厂商目录 / 学到的窗口 / 手工填写），默认就算已探测 |
| `已探测 · 接受 961,759` | 厂商原样接受了该规模，之后不再探测 |
| `已探测 · 拒绝` | 厂商以超限拒绝，已转为学习窗 + 裁剪，之后不再探测 |
| `未探测` | 既没有上下文长度，也没有探测记录；或操作者点过「清除探测」 |

每行提供一个**清除探测**按钮。它写入一条 `cleared` 墓碑而不是删除记录：模型即使已有
上下文长度，也会因此显示成 `未探测`，下一次超预算请求重新探测。清除只影响探测状态，
不动学习窗、不动实测量、不动手工覆盖。

### 6. 开关与默认值

新增配置项 `context_guard.probe_before_trim`，默认 **true**（探针放行）。设置位置：

- 配置文件：`configs\client2api.json` 的 `features.probe_before_trim`（布尔，缺省即 true）。
- 面板：设置页的「特性」分组里加一个开关，热生效（走现有 reload 路径）。

理由：

- 用户的判断是「估算不该比厂商更有权威」，默认走厂商裁决。
- 探测是黏性的（每模型一次）+ 实测量 + 学习窗三道闸仍在，误放行的最坏结果是一次多余往返，不是无限放行。
- 需要退回保守裁剪的部署，把该项设为 false 即可，不必回滚版本。

注意：改这一项需要重启或面板 reload；本设计不要求重启进程。

## 数据流

```text
请求进入（Responses）
  -> responsesWouldTrim
       ├─ 该模型未学习 -> 返回假（不压缩），交给探针
       └─ 该模型已学习 -> 返回真 -> autoCompact
                            ├─ 成功 -> req.Messages 换成折叠版 + 回带 compaction 项
                            └─ 失败 -> 继续，交给守卫
  -> prepareCandidateAttempt
       -> probeDecision
            ├─ 未学习       -> 原样转发，rec.Probed = true（学习从这里开始）
            └─ 已学习/预算内 -> applyContextBudget（压缩失败且仍超预算时才裁）
  -> openStreamRecorded
       ├─ 成功   -> probeStore.recordAccepted(prompt_tokens)
       └─ 超限拒绝 -> learnContextWindowFromFailure（学习）
                    + probeStore.recordRefused
                    -> 按实测比例裁剪 -> 重试一次
```

## 错误处理

- 实测量存储读取失败：与 `learnedWindowStore` 一致，塌缩为空存储并告警，绝不因为
  缓存文件损坏而让请求失败。
- 探针放行后被拒、但拒绝里没有数字：走现有的
  `retryBudgetForUnmeasuredRefusal`（按广告窗口一半重试），仍失败则把厂商原始 400
  返回给调用方，行为与今天完全一致。
- 探针放行后被拒、且重试仍然超限：`contextRetried` 已置位，不会无限重试；返回最后一次错误。
- 探针放行后厂商返回非超限错误：按现状分类处理，不做特殊处理。

## 测试

单元测试：

- `probeDecision` 的六个分支各一个用例，尤其是「有实测量 → 不探针」与
  「已探测过 → 不再探针」。
- `observedWindowStore` 的加载 / 写入 / 损坏文件塌缩 / 只取最大值。
- 预算放宽：实测量抬高有效窗口，但不超过手工覆盖值。
- `responsesWouldTrim` 与 `probeDecision` 结论一致（探针放行时不产生 compaction 项）。

端到端测试（httptest 起的假厂商）：

- 估算超预算、厂商接受 → 请求原样到达厂商（断言消息条数未被裁剪），用量行标记 probed。
- 估算超预算、厂商按实测数字拒绝 → 消息被裁剪且重试一次后成功，行为与现有测试一致。
- 已探测过的模型 → 不再试探，按已有结论走。
- 第二次请求（已有实测量）→ 不再试探，直接按放宽后的预算处理。

回归：现有 `context_guard_*_test.go` 全绿；`responsesWouldTrim` 相关测试全绿。

## 文档

- `README.md`：配置项 `context_guard.probe_before_trim` 与「探针放行」标记说明。
- `docs/REFERENCE.zh-CN.md`：守卫的决策顺序，以及实测量与广告窗口的区别。
- `CHANGELOG.md`：新增条目。

## 风险与缓解

| 风险 | 缓解 |
|---|---|
| 每次试探多一次往返 | 每个 (平台, 模型) 只探测一次；成功后即转为正常路径 |
| 多账号池下 probeFloor 被高估 | 只放宽预算、不写广告窗口；探测结果可在面板清除后重测 |
| 厂商按账号不同窗口 | 实测量按 `(平台, 模型)` 记录，不跨平台共享；后续可细化为按账号 |
| 默认开启影响面大 | 黏性探测 + 实测量 + 学习窗三道闸；需要保守时设 `features.probe_before_trim=false` |
| 用量环滚动导致证据丢失 | 探测与 probeFloor 单独持久化，不依赖 `recent.json` 环 |

## 本设计已确定的取舍

1. **默认开启**探针；需要退回保守裁剪时设 `features.probe_before_trim=false`。
2. `probeFloor` **只放宽预算**，不改广告窗口；广告窗口仍由手工覆盖 / 学习窗 / 目录值决定。
3. **不设**「超出太多就不试探」的上限；估算器不可信，任何超预算规模都先交给厂商。
4. 探针状态记录在用量行并在面板显示，不静默。
