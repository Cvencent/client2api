# 任务中心重构设计：账号优先、批次执行、计划驱动

日期：2026-10-08
状态：已确认方向，待实施

## 背景

当前任务中心由三个相对独立的页面组成：

- 定时任务：按平台和批次展示调度配置，只能看到模块通过 `core.BatchPlanner` 声明的批次。
- 批量执行：按平台扫描账号，找出账号当前待执行的成长任务，再形成执行队列。
- 任务看板：按平台读取 `core.TaskProvider.Tasks`，显示任务列表和运行记录。

这三块没有共享统一的数据模型，导致两个明显问题：

1. **Loomy 成长任务没有出现在定时任务里。** Loomy 实现了 `core.TaskProvider`，但没有实现 `core.BatchPlanner`，因此没有 `growth` 批次；调度器看不到它，定时任务页也不显示它。
2. **任务看板的完成状态没有绑定账号。** 服务端 `GET /tasks` 支持 `account` 查询参数，但前端只按平台请求一次；运行记录在前端只按任务 code 过滤，不按账号过滤。结果是某个账号已完成的成长任务，会在看板上看起来像是整个平台都完成了。

用户最终确认的方向是：

- 以**账号**为主轴重新组织任务中心。
- 定时任务和批量执行默认作用于平台的启用账号，同时允许账号级覆盖。
- 三个子页面必须共享同一套账号、批次、任务、运行记录模型。

## 设计目标

1. 任何任务完成状态都必须能明确回答：“这是哪个平台、哪个账号、哪个任务”。
2. Loomy 的成长任务必须自动出现在定时任务、批量执行和任务看板里，不需要在各页重复手工配置。
3. 定时任务、批量执行、任务看板三页使用同一套批次定义和账号范围，不出现“某个页面有、另一个页面没有”的割裂。
4. 默认保持平台统一配置，避免账号多时配置爆炸；只对需要例外的账号做覆盖。
5. 保持现有配置兼容：旧的按平台/批次时间表继续有效，不需要迁移数据。

## 总体模型

任务中心统一采用三层模型：

```text
账号（Account）
  └── 任务状态（AccountTaskState）
        └── 来自任务目录（TaskCatalog）

批次（BatchPlan）
  └── 一组任务 code，例如 growth / checkin / travel
        └── 批量执行时展开到账号

计划（Schedule）
  └── 批次 + 时间 + 账号范围
        └── 到点后按账号范围执行 BatchPlan
```

三个页面的职责调整如下：

- **任务看板**：以账号为主，显示该账号跨平台、跨批次的任务状态。支持“全部账号”矩阵视图和单账号明细视图。
- **批量执行**：以批次为主，先选平台和批次，再选账号范围（全部启用账号或指定账号），展示待执行、执行中、成功、跳过、失败。
- **定时任务**：以计划为主，配置批次在什么时间对哪些账号生效。默认平台全部启用账号，可增加账号级包含或排除。

## 关键决策

### 1. 账号优先

所有任务状态和运行记录都按以下主键关联：

```text
client + account + batch + code
```

服务端现有记录已经包含 `client`、`account`、`code` 字段，主要缺口在 UI 和聚合逻辑：

- 任务看板必须传入 `account` 查询参数。
- 运行记录必须按账号过滤，而不是只按任务 code 过滤。
- 批量执行的队列记录继续使用账号分组，作为任务看板的数据来源之一。

### 2. 批次是执行单元

任务 code 不直接配置定时计划，批次才是调度器的最小单位。批次定义来自模块，模块负责声明：

- 批次名，例如 `growth`、`checkin`。
- 批次包含的任务 code。
- 账号间隔、结算等待时间。
- 是否只运行当前账号尚未完成的任务。

Loomy 需要新增 `Batches()`，声明一个 `growth` 批次，包含当前 onboarding registry 中的全部成长任务 code。

### 3. 计划默认平台级，支持账号覆盖

调度配置保持现有结构：

```text
clients.<client>.<batch>.hours
clients.<client>.<batch>.enabled
```

新增账号范围字段：

```json
{
  "enabled": true,
  "hours": [12],
  "accounts": {
    "include": ["loomy-account-1"],
    "exclude": ["loomy-account-2"]
  },
  "account_mode": "platform"
}
```

`account_mode` 语义：

- `platform`：默认，作用于该平台全部启用账号。
- `include`：只作用于 include 列表中的账号。
- `exclude`：作用于全部启用账号，但排除 exclude 列表中的账号。

不填写 `accounts` 时完全保持旧行为。

### 4. 只执行待执行任务

对成长任务这类一次性任务，调度器不应反复上报已经完成的任务。批次支持 `pending_only` 语义：

```json
{
  "name": "growth",
  "codes": ["first_message", "share_soul"],
  "pending_only": true
}
```

执行前先调用 `Tasks(ctx, accountID)`，只运行满足以下条件的 code：

```text
存在 && !Claimed && !Locked && Auto
```

这个规则与批量执行页现有的待办判断保持一致。已经完成的成长任务不会继续出现在定时执行中，也不会污染运行记录。

## 服务端设计

### 任务状态聚合

保留现有 `GET /panel/api/clients/<client>/tasks?account=<id>` 作为单账号任务看板数据源。

新增聚合读取能力，用于“全部账号”矩阵：

```text
GET /panel/api/clients/<client>/tasks?all=1
```

返回：

```json
{
  "client": "loomy",
  "accounts": [
    {
      "account": "loomy-account-1",
      "label": "16286204683",
      "state": "ready",
      "tasks": [
        {
          "code": "share_soul",
          "title": "分享 Soul",
          "claimed": true,
          "auto": false,
          "credit": 1000
        }
      ],
      "runs": [
        {
          "account": "loomy-account-1",
          "code": "share_soul",
          "state": "done",
          "started_at": "2026-10-08T12:00:00+08:00"
        }
      ]
    }
  ]
}
```

实现可以复用现有 `scanTasks` 的并发控制，避免一个平台的账号全部同时请求上游。

### 定时任务行

`GET /panel/api/schedule` 返回的每个 `scheduleRow` 增加：

```json
{
  "account_mode": "platform",
  "account_scope": {
    "include": [],
    "exclude": ["loomy-account-2"]
  },
  "pending_only": true,
  "pending_accounts": 2,
  "completed_accounts": 1
}
```

`pending_accounts` 和 `completed_accounts` 按批次中的任务动态计算，而不是仅统计启用账号数。

### 运行记录

现有运行记录分为：

- `taskRuns`：单任务运行，已经有 `account` 字段。
- `batchRuns`：批量 sweep，`Steps` 中每一步都有 `Account`。
- `scheduler.History`：调度历史，记录 `Client` 和 `Batch`。

面板聚合时统一输出：

```json
{
  "client": "loomy",
  "account": "loomy-account-1",
  "batch": "growth",
  "code": "share_soul",
  "trigger": "schedule",
  "state": "ok",
  "message": "厂商返回的余额为 10000 积分"
}
```

看板按 `client + account + code` 过滤，计划页按 `client + batch` 汇总。

## 前端设计

任务中心保持三个子页签，但重命名和重排：

1. **账号任务**：原“任务看板”，账号优先。
2. **批量执行**：原“批量执行”，批次和账号范围优先。
3. **定时计划**：原“定时任务”，批次和计划优先。

### 顶部公共筛选

三页共享一组顶部筛选项：

- 平台选择 chips。
- 账号范围：`全部账号` / 具体账号。
- 批次筛选：`全部批次` / `growth` / `checkin` 等。
- 状态筛选：`全部` / `待执行` / `执行中` / `成功` / `跳过` / `失败`。

切换页面时保留平台和账号选择，避免用户每切一次页都要重新选账号。

### 账号任务页

默认显示选中账号的任务列表：

- 顶部显示账号标签、平台、余额、冷却状态。
- 任务按批次分组，例如“Loomy 成长任务”。
- 每一行显示：任务名、code、进度、状态、上次运行、最近余额变化。
- 操作按钮：执行、加入计划、查看运行记录。

提供“全部账号”矩阵视图：

- 行：任务。
- 列：账号。
- 单元格：已完成 / 可执行 / 未解锁 / 执行中 / 失败。
- 适合快速发现“有的账号做了，有的账号没做”。

### 批量执行页

页面结构：

- 顶部：平台、批次、账号范围。
- 批次卡片：显示批次名、任务数、待执行账号数、上次执行结果。
- 执行列表：按账号分组，每个账号下列出待执行任务。
- 操作：扫描待办、执行全部待办、执行选中账号、执行选中任务。

Loomy 的 `growth` 批次会直接出现在批次卡片中，不需要在页面里单独硬编码。

### 定时计划页

每个计划行展示：

- 平台。
- 批次。
- 计划时间，例如 `每天 12:00`。
- 账号范围，例如 `全部启用账号` 或 `排除 1 个账号`。
- 下次执行时间。
- 上次执行结果。
- 待执行账号数 / 已完成账号数。

操作：

- 编辑时间。
- 编辑账号范围。
- 立即执行。
- 查看运行记录。

账号覆盖编辑器支持：

- 平台全部账号。
- 只包含指定账号。
- 全部账号但排除指定账号。

## Loomy 集成

Loomy 新增 `core.BatchPlanner`：

```go
func (c *Client) Batches() []core.Batch {
    return []core.Batch{{
        Name:        "growth",
        Codes:       onboardingAutoCodes(),
        PendingOnly: true,
    }}
}
```

`onboardingAutoCodes()` 返回 registry 中所有可自动上报的成长任务 code。

运行时行为：

- 调度器先读取账号的 `Tasks`。
- 只对未完成、未锁定、支持自动化的成长任务调用 `RunTask`。
- 任务成功后继续刷新账号余额缓存。
- 已完成的成长任务不重复上报，不产生无意义的运行记录。

## 兼容与迁移

- 旧配置不写 `accounts` 时，默认行为与现在完全一致。
- 旧的任务运行记录没有 `batch` 时，看板显示在“未归入批次”分组。
- 旧记录 `account` 为空时，显示在“默认账号”分组，不伪造账号 ID。
- 现有 `schedule` 配置文件结构保持向后兼容。
- 不需要迁移历史运行记录。

## 测试计划

服务端：

- `Batches()` 对 Loomy 返回 `growth` 批次，且 code 列表稳定、只包含可自动执行任务。
- `PendingOnly` 批次只执行未完成任务，已完成后不重复执行。
- 调度配置支持 `account_mode=platform/include/exclude`，旧配置无字段时行为不变。
- `GET /tasks?account=<id>` 返回该账号的 tasks 和 runs。
- `GET /tasks?all=1` 返回按账号分组的任务状态。
- 运行记录按 `client + account + code` 过滤。

前端：

- 切换账号时，任务状态和运行记录一起变化。
- 从定时计划点击“立即执行”后，批量执行页能看到对应账号的进度。
- Loomy 的 growth 计划出现在定时计划中。
- 账号任务页能切换“单账号”和“全部账号矩阵”。

回归：

- `go test ./...`
- `go build ./...`
- 面板 UI 测试覆盖三个页签的账号维度。

## 非目标

本次不做：

- 多平台账号合并为一个逻辑主体的跨平台编排。
- 任意用户自定义任务脚本。
- 把调度器改造成通用 cron 表达式系统。
- 为每个账号维护独立的定时数据库；账号覆盖仍通过现有配置热加载。

## 实施顺序

1. Loomy 实现 `Batches()`，让 growth 批次进入调度器和批量执行页。
2. 扩展 `core.Batch` 和调度配置，加入 `pending_only` 与账号范围。
3. 服务端补任务聚合和运行记录过滤。
4. 前端重构任务中心顶部筛选和三个子页。
5. 补测试并跑全量验证。
