# Agent IssueRun：一次 Agent 运行的全过程

[English](issue-runs.en.md) | 中文

用户在 Issue 评论里 @ 一个 Agent 并给出任务，就得到一个 Agent IssueRun（`executor_type = agent`）。Cloud 为每次运行创建
一个一次性的 isolated **运行 Workspace**（沙盒 → Node → clone → 插件），就绪后在里面启动 Agent 会话；会话的记录叫
**Thread**，实时回到 Issue 页，用户可以在 Thread 里继续对话；会话结束后，Node 把代码改动和会话记录保存为 **Revision**，
Cloud 校验并登记之后删除运行 Workspace，释放沙盒和数据。

整条编排的权威状态都在 Cloud。Controller 和 Node 只执行 Cloud 放出的工作，Cloud 在接管它们报回的证据的同一个事务里推进
运行（见 [同一事务里的业务钩子](../core/persistence.md#同一事务里的业务钩子)）。本文是总览，几个环节分别展开在：

| 环节 | 文档 |
|---|---|
| 怎么创建（@Agent、Project 关口、输入快照） | [Issue 协作](../collaboration/interactions.md#task-modeagent) |
| Thread 条目、用户轮次、空闲与结束 | [Thread](threads.md) |
| 启动的等待上限与未知结果收尾 | [有界启动](startup-settlement.md) |
| D4/D5 残留账本、系统清理责任与证据边界 | [残留回收](residual-reclamation.md) |
| 对象存储、上传授权、Revision 登记与续接 | [Revision](revisions.md) |
| 会话列表、归档与模型请求统计 | [会话读取](session-reads.md) |

## 阶段

`issue_runs.phase` 是 Agent 运行的持久状态机；公开的 `status` 保持原义，由阶段推导。阶段只前进，不倒退，每一次转换都由
Cloud 在接管上一阶段证据的事务里写入。

| phase | 进入条件 | Cloud 放出的工作 | `status` |
|---|---|---|---|
| （空） | 评论事务创建运行 | 无 | `queued` |
| `provisioning` | 派发循环为运行建好运行 Workspace 和它的 `create_workspace` 操作 | 运行 Workspace 的创建操作 | `dispatched` |
| `starting` | 创建操作成功，Workspace 就绪，固定的 Agent 插件已按固定版本装上 | Agent 会话执行（`agent_session` 工作项）和 Thread 的首条用户轮次 | `dispatched` |
| `running` | 接管到会话的第一条 Node 记录 | Thread 命令：用户消息、结束请求 | `running` |
| `delivering` | 会话执行有了终态结果，无论结束原因 | Revision 交付执行（`deliver_revision` 工作项） | `running` |
| `releasing` | Revision 已登记，或交付被放弃，或会话开始前就被取消 | 运行 Workspace 的 `delete_workspace` 操作 | 见下文 |
| `done` | 删除操作成功；或删除侧超过上限 | 无 | 不变 |

- **一个 Project 同时只能有一个生命周期操作**，这个约束对运行 Workspace 同样成立。Project 忙时运行留在 `queued`，等下一次派发；
  建删操作都很短，会话本身不占用操作，所以同一 Project 的多个运行只在建删 Workspace 时排队。
- 创建操作失败时运行进入 `releasing`，`status = failed`、`failureReason = workspace_unavailable`，已创建的部分由删除操作清理；
  Workspace 就绪但固定的 Agent 插件没有装上时同样进入 `releasing`，原因为 `agent_plugin_unavailable`。
- 创建操作停在 `blocked` 超过上限时，运行按 `failed`、`done` 收尾并保留残留责任，原创建操作失败、释放 Project 的操作槽（见 [创建 blocked 的有界放弃](startup-settlement.md#创建-blocked-的有界放弃)）。
- **`done` 不代表运行 Workspace 已经删除**，只有删除操作成功才是（见 [删除侧的上限](#上限)）。
- Run 进入 `releasing`、`done` 或被软删除后，未登记的会话和交付工作不再被 `ClaimWork` 返回，避免旧工作挡住后继运行。
  新 `RecordDispatch` 在业务事务锁内重新核验资格，收尾前领取的快照不能绕过检查。工作与执行记录不删除，已有执行仍按原身份
  走既有恢复路径及其检查；这不等于 Node 已终止或 Workspace 已回收。接口语义见 [Controller 契约](../runtime/controller-integration.md#服务)。

运行 Workspace 只属于这一次运行（`workspaces.issue_run_id` 与 `issue_runs.workspace_id` 互相唯一引用；一次 WorkflowRun 的运行 Workspace 同样只属于它，
见 [工作流](../workflows/workflows.md#运行)），以触发运行的用户为创建者
和操作发起者，从 Project 默认分支克隆，不出现在任何公开的 Workspace 列表或接口里，也不接受其他运行或交互式会话。

## 后台循环

D4/D5 业务收尾后的残留由独立账本对账继续处理，证据足够才授予一份持久的系统 owned-delete 责任；重试保持同一 operation，
完成回收不重新结算 Run。部署运维可用 `cloudctl residual` 显式登记受控回收（只终止并保留数据、单独删数据或外部确认），
也可以接管 D8 历史残留，见[残留回收](residual-reclamation.md)。`done` 或对账已运行均不证明资源已回收。

除了接管证据时的同步转换，Cloud server 进程里有一组每 10 秒执行一次的循环，负责那些没有外部事件可跟随的推进和收尾：

| 循环 | 作用 |
|---|---|
| `DispatchQueuedAgentRunsOnce` | 为 `queued` 的运行建运行 Workspace 和创建操作，进入 `provisioning`；Project 忙就留到下一轮 |
| `StartQueuedAgentSessionsOnce` | 为 `starting` 的运行写 Thread 首条轮次并放出会话工作项；同时检查启动等待上限 |
| `EndIdleAgentThreadsOnce` | Thread 空闲超过窗口时放出结束命令 |
| `ReactToCancelledAgentRunsOnce` | 对已登记取消请求的运行放出结束命令 |
| `GiveUpBlockedWorkspaceCreationsOnce` | 运行 Workspace 的创建停在 `blocked` 超过上限时放弃该次创建 |
| `GiveUpStaleDeliveriesOnce` | 交付超过上限时放弃交付，进入 `releasing` |
| `RedeclareRunWorkspaceDeletesOnce` | `releasing` 的运行没有进行中的删除操作时重新声明删除，例如 Node 拒绝停止之后 |
| `GiveUpStaleWorkspaceReleasesOnce` | 删除侧超过上限时把运行结束为 `done` |

它们的形状相同：在全局锁之外做一次只读扫描（每轮至多 100 个，按创建时间与 ID 排序），再为每个运行开一个短事务，在事务里重新
读取并以比较并交换的方式转换。重复的一轮不会产生第二个命令、第二个操作或第二次转换；某个运行暂时卡住时只是回滚、留待下一轮，
不刷日志。

## 执行结果与成果保存分开记录

运行的最终 `status` 只表达执行结果，由会话的结束原因推导，在进入 `releasing` 时与阶段一起写入，并追加对应的时间线活动：

| 会话结束原因 | `status` |
|---|---|
| `user_ended`（用户结束）、`idle_timeout`（空闲期满） | `completed` |
| `cancelled`（运行被取消） | `cancelled`；启动超时引起的策略取消记为 `failed` |
| `agent_failed`（Agent 失败）、`interrupted`（Node 中断或结果未知） | `failed` |

成果是否保存由 `result.deliveryState` 表达：`saved`（登记了有改动的 Revision）、`unchanged`（登记了没有改动的 Revision）、
`failed`（交付被放弃）、`skipped`（部署没有对象存储或 Git Server，或会话开始前就被取消）。`failed` 和 `cancelled` 的运行同样会尽量保存成果，
也可以有 Revision。

时间线上，Agent 运行有一条 `run.enqueued` 活动，进入 `releasing` 时再追加 `run.completed`、`run.failed` 或 `run.cancelled`，
详情里同时带着执行结果和交付状态。

## 取消与结束是请求

用户结束会话、空闲期满、运行被取消，Cloud 都只记录请求，并向 Node 放出 `EndSession{reason}` 命令；会话收尾和交付照常进行，
立即销毁 Workspace 会丢掉 Agent 已经做的修改。

- 用户结束：`POST .../runs/:rid/thread/end`（见 [Thread](threads.md#结束)）。
- 空闲期满：Thread 空闲超过 `issue_runs.thread_idle_timeout`（默认 15 分钟），由数据库时间判定。
- 取消：处理路径已经就位（`issue_runs.cancel_requested_at`），但还没有公开的取消接口。会话开始之前就被取消的运行直接进入
  `releasing`，以 `cancelled` 结束，交付状态为 `skipped`。

## 上限

| 环节 | 规则 | 默认值（配置项） |
|---|---|---|
| 创建 | 运行 Workspace 的创建操作第一次 `blocked` 后等待真实结果 | 30 分钟（`agent_workspace_blocked_timeout`），见 [有界启动](startup-settlement.md#创建-blocked-的有界放弃) |
| 启动 | 运行 Workspace 就绪后等待会话第一条记录；恢复前序 Revision 时使用更长的窗口；结束请求发出后等待确认 | 2 分钟、6 分钟、5 分钟（`agent_start_timeout`、`agent_restore_timeout`、`agent_end_confirm_timeout`），见 [有界启动](startup-settlement.md) |
| 交付重试 | 交付失败时以新的执行重试，等待 30 秒起、每次翻倍、上限 10 分钟；运行保持 `delivering`，沙盒保持运行 | — |
| 交付放弃 | 从第一个交付工作项起连续失败超过上限，或运行 Workspace 的 Node 状态未知超过上限，放弃交付：`deliveryState = failed`，进入 `releasing`，本轮未保存的修改丢失 | 2 小时（`delivery_give_up_after`）、30 分钟（`delivery_unreachable_after`） |
| 删除放弃 | 只以不可达为条件：仍有存活沙盒，且它的 Node 状态未知超过同一个不可达窗口。Node 明确拒绝停止时仍可达，不触发放弃 | 30 分钟（与交付共用） |

删除被放弃时，删除操作以 `node_unavailable` 终结，运行进入 `done`，`status` 保持进入 `releasing` 时的值，另记
`failureReason = workspace_unavailable`。运行 Workspace 行**不**软删除，仍指向这个运行；沙盒和数据不会被自动回收，只能由运维
处置，公开 API 对运行 Workspace 的删除始终是 `404`。所有时间都按数据库时钟比较。

## 交付执行的业务结算与未受理证明

对应规则见[交付执行结算与未受理证明 ADR](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/20261026-delivery-execution-settlement-and-non-acceptance.md)。
交付截止结算独立于 Node 的执行结果。Cloud 在截止事务中记录行政结算并拒绝原 work 的新登记、原
execution 的新许可和上传授权；这不会写 Node result、sequence、receipt 或新的 Revision，也不会把
未知执行算作 idle。产物截止只封闭产物上传，不中断仍有效的 Revision。

Controller 通过专门 RPC 请求原身份的未受理证明。Node 只有在与受理互斥的 SQLite 事务里确认原
execution 不存在，并永久封闭该身份的输入后，才能回复证明；已受理、已完成或新实例的空 ledger
都不能替代它。Cloud 真实持久接管后才解除对应责任；丢失响应由已提交证明事实恢复，不能凭 pending
列表中消失推断成功。quiesce 仍检查其他责任、准入代次及 Node 状态。

原失败 Run 保持历史结论；迟到真实终态按原身份接管，矛盾证明/终态留审计且不能 ACK 或用于 idle。
一个交付执行的矛盾不阻止另一交付执行提交真实结果、回执和 Revision；业务结算可以进入 releasing
并声明删除意图，但实际 quiesce、idle 与物理清理仍受矛盾事实保护，声明意图不等于资源已释放。
存量迁移只补明确来源，不补造原 Node 实例。无可靠原实例证据或原 Node 不可达时保留未知责任，按
既有 D8/受控残留两阶段处理。协议、schema 与迁移须一起升级；本文不授予额外数据删除权限。

## 运行开始前固定的输入

运行的行为由创建和启动时固定的快照决定，之后的变化不影响已经在跑的运行：

- **Agent 插件**：创建时把插件身份和当时的期望版本写进运行输入，之后升级插件不改变这次运行。
- **个人模型**：`official/ora-space.opencode` 的运行在创建事务里按触发用户的默认模型生成一份运行绑定（见
  [个人模型连接](../models/model-access.md)）；没有默认模型、密钥或可用连接时整条评论回滚，返回 `409 model_default_required`、
  `model_credential_required` 或 `model_connection_unavailable`。
- **Git 提交身份**：与模型绑定一起固定触发用户的 Git 作者身份，Agent 以这个身份提交。
- **前序 Revision**：会话启动时选定这个 Issue 最近一个可续接的 Revision 并固定下来（见 [Revision](revisions.md#续接前序-revision)）。

触发用户在运行期间必须一直能使用这个运行时：他不再是当前成员、账号被停用时，之后的派发和 Git 访问都会被拒绝。

## 公开可见的内容

`GET .../issues/:iid/runs` 与 `.../runs/:rid` 返回运行资源。`phase`、运行 Workspace、启动窗口这些控制字段不出现在公开文档里；
Agent 运行额外带两部分投影：

- `preparation`：准备进度，`stage` 为 `waiting`、`environment`、`clone`、`plugin`、`start`、`failed` 或 `cancelled`，另有已用的
  clone 次数与上限（3）、下一次重试时间、安全的错误码；不暴露 Workspace、Node、仓库地址、凭据或原始错误。
- `revision`：已登记 Revision 的元数据（见 [Revision](revisions.md#公开读取)）。

## 相关决策

- [Issue 上 @Agent 的 IssueRun 由 Cloud 编排](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/0-agent-run-in-disposable-isolated-workspace.md)
- [同一 Issue 的新运行先续接最近一次 Revision，再启动 Agent](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/20261010-resume-run-from-latest-issue-revision.md)
- [Agent 运行的对话记为 Thread](https://github.com/ora-space/specs/blob/main/decisions/cloud/thread/0-durable-agent-thread-with-user-turns.md)
- [Workspace 创建与启动增加 plugin 步骤；IssueRun 的运行 Workspace 由 Cloud 发起建删](https://github.com/ora-space/specs/blob/main/decisions/cloud/operation/20260928-plugin-step-and-run-workspace-release.md)
- [用户可在 Cloud 配置 git 提交身份](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/20260928-user-git-commit-identity.md)

## 任务消息与直接启动的顺序

失败或取消发生在准备阶段时，运行可能没有 `result.deliveryState`。这仍然形成失败暂停；待资源释放并进入 `done` 后，发起者或管理员可通过已有的放弃未交付成果操作明确确认，再继续等待中的消息。没有交付结果不等于已保存成果，不自动恢复，也不改写原运行终态。

真实 Space Agent 的手动运行和评论 target 在同一创建事务检查任务暂停、未解决的 failed 消息和已接受的 waiting 消息：暂停/失败返回 `409 task_paused`，待执行输入返回 `409 task_handoff_pending`，拒绝不占用 pending Run 名额。任务队列按已接受的消息顺序取得新运行资格；实际领取和周期扫描再次检查暂停、消息顺序与旧运行的成果/释放屏障，不能靠另一入口绕过。已准入的运行先排空，再由任务队列按消息序号创建后续运行；不以跨事务的创建时间推断消息与运行的接收顺序。
