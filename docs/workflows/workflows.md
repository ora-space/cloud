# 工作流

[English](workflows.en.md) | 中文

工作流是租户内可编辑的有向图文档：节点描述一次多步骤的 Agent 协作，边描述它们的先后和分支。Cloud 保存工作流文档和它的已发布版本，
在 Cloud 侧调度运行，并把它作为 Issue 上可以 @ 的协作目标。本文说明文档与发布、执行器槽位、运行与调度，以及从插件导入工作流的机制。

## 文档与发布

| 接口 | 作用 |
|---|---|
| `GET`、`POST /tenants/:tid/workflows` | 列出、创建工作流 |
| `GET`、`PUT`、`DELETE .../workflows/:wfid` | 读取、保存（带版本）、归档工作流 |
| `POST .../workflows/:wfid/publish` | 把当前文档冻结成下一个已发布版本 |
| `GET .../workflows/:wfid/snapshots`、`.../snapshots/:snapshotId` | 列出、读取已发布版本 |
| `GET .../snapshots/:snapshotId/form` | 某个已发布版本投影出的表单 |
| `PUT .../snapshots/:snapshotId/restore` | 用某个已发布版本的图替换当前文档 |

- **草稿与已发布分开。** 保存只要求请求体里的图是一个 JSON 对象，不检查完整性：写到一半的草稿是合法的文档。只有**发布**判断图能否执行，
  把问题连同出错的节点或槽位一起返回，让编辑器标出来。被冻结的已发布版本就是 Cloud 同意执行的图。
- **版本不可变。** 已发布版本（`workflow_snapshots`）按工作流内递增的版本号保存，从不修改、从不软删除；运行渲染的图因此不会随文档变化而
  漂移。发布时锁定工作流行，两次并发发布不会拿到同一个版本号。快照名称默认取工作流名称。
- **恢复**把当前文档换成某个版本的图，并像其他编辑一样推进文档版本；它不改动任何已发布版本。
- 工作流名称在租户内未归档的工作流中唯一（`409 workflow_conflict`）。归档是软删除，归档的工作流连同它的全部版本和运行历史一起从读取中隐藏。

**Cloud 能执行的子集。** 发布时只接受 Cloud 当前能执行的节点类型：`start`、`agent`、`condition`、`output`。编辑器里的其他节点类型仍然可以编辑
和保存，但发布会拒绝，而不是冻结一个节点会被静默丢掉的图。Agent 节点里 Cloud 不执行的配置字段同样按字段名拒绝，避免作者以为某个设置生效、
实际却没有运行方会应用它。

发布弹窗会把 `workflow_graph_invalid` 显示为配置检查失败，并列出返回的节点、字段或槽位与修正提示，而不是让作者反复重试。
例如，Cloud 当前不执行结构化输出（`agentConfig.outputContract`，任何非 null 值都会被拒绝）：编辑器在开关旁说明此限制，
发布时提示关闭出错节点的「结构化输出」后重新发布。草稿里的设置仍保留，由作者修改，不会在发布时静默移除。

**保留字段。** `snapshotId` 是 `@` 表单里平台拥有的运行版本键，作者不能把它声明成自己的变量。Start 节点的输入变量或全局变量声明这个名字
时，发布和 workflow 资产导入都会拒绝，问题里点名节点或字段（`reserved_field_name`），不产生已发布版本；保存草稿不受影响。这个规则之前
发布、自己声明了该键的版本不改写、不删除，但不再进入 `@` 表单的版本选择器。仍然声明着该键的活稿在重新发布之前，作者的这个变量也不进入
`@` 表单：只要有一个可选的已发布版本，表单就投影平台的版本选择器；一个可选项都没有时（从未发布，或全部版本都被排除）表单不带选择器。
确认里的 `snapshotId` 始终是版本游标。`launchFields` 撤下这个平台字段是另一回事，仍然有效
（见 [Issue 协作](../collaboration/interactions.md)）。

**首期不支持交互式节点。** `human` 节点和 agent 节点的 `interactive: true` 在发布时被拒绝，运行不会停在等待人工输入的状态。
`workflow_runs.status` 的 CHECK 保留 `awaiting_input` 作为将来的保留值，但首期没有写入者：它不出现在公开 OpenAPI 的状态枚举里，
页面也不会展示它；取消接口仍按不可取消处理，IssueRun 投影也不会因它推进。HITL 留待后续的交互式运行控制决策。
同样在发布时拒绝的还有交互式节点的产出物声明 `agentConfig.completion`（任何非 null 值），以及 `agentConfig.schemaVersion` 不是 `3` 的值
（`4`、旧的 `1`/`2`、非数字）：Cloud 只执行版本 `3`，缺省或 `null` 保持旧行为，其余非 `3` 的值都拒绝，拒绝时点名节点与字段，不静默剥离声明。

## 执行器槽位

Agent 节点不在图里写死 Agent CLI 或模型 ID，而是引用一个**执行器槽位**：

- `default` 是每个图都可以直接引用的保留槽位，指运行发起者个人的默认模型；Agent 节点不指定槽位时就用它。图不能重新声明 `default`。
- 图可以声明具名槽位，带给运行者看的说明和作者的建议（`hint`）。槽位名有固定的语法，说明按字符数限制长度。
- 运行开始时，按**发起者自己**的配置解析每个被引用的槽位，不回退到 `default`、不采用作者的建议、也不借用别人的配置。任何一个槽位解析
  不了，启动就以 `409 workflow_executor_unconfigured` 拒绝，参数里逐个列出槽位、依赖它的节点、原因（例如 `slot_not_configured`、
  `default_model_missing`、`credential_missing`）以及槽位说明，前端可以一次性展示所有需要补的配置。
- 具名槽位的映射是每个用户自己的配置（`GET`、`PUT`、`DELETE /me/executor-slot-mappings/:slot`：槽位名到 agent CLI、模型连接与模型）。`default` 不能被
  存成一条映射——它就是个人默认模型本身，单独在个人模型设置里维护。

## 运行

工作流有三个启动入口，它们共用同一道准入：

| 入口 | 产生 |
|---|---|
| `POST .../workflows/:wfid/runs` | 一个 WorkflowRun（`workflow_runs`） |
| `POST .../issues/:iid/runs`，`executorType = workflow` | 一个 WorkflowRun 和关联它的工作流 IssueRun |
| 在 Issue 上确认 @工作流 的表单 | 一个 WorkflowRun 和关联它的工作流 IssueRun（见 [Issue 协作](../collaboration/interactions.md#form-mode工作流)） |

**准入**在调用方的事务里一次判定：

1. 解析要执行的已发布版本：调用方显式指定的版本，否则是最新的已发布版本。没有任何已发布版本时返回 `400 workflow_no_published_snapshot`，
   因为执行草稿会让之后的编辑改变一个已经创建的运行"执行过"的内容。
2. 用该版本的图校验输入：Start 节点声明的必填变量必须有值、类型必须匹配；图没有声明的键不检查（运行输入里还有平台自己写入的内容）。
3. 检查执行器槽位（见上文）。

任何一项不通过都不会留下运行、已认领的交互或活动。被执行的版本冻结在运行上，运行之后工作流再被编辑或发布，它仍然知道自己执行的是哪个版本。

**运行的身份在创建时冻结。** 三个入口共用创建函数：直接启动的 Project 来自请求体 `projectId`，Confirm/manual 来自 Issue 的 `project_ref`。
Project 缺失时统一返回 `409 workflow_run_project_required`，不可用、已归档或属于其他空间的 Project
按 `404` 拒绝，正在删除的返回 `409 resource_unavailable`。运行把发起人（`created_by`）和这个 Project（`project_id`）写进运行行，之后不再变化——工作流文档
属于租户而不是某个 Project，没有可以默认的对象，而运行的 Workspace、它的 clone 和整份检出都落在被指定的 Project 里。运行 Workspace 归属于创建它的那次运行：
`workspaces.workflow_run_id` 与 IssueRun 的 `issue_run_id` 互斥，运行时控制里为它预留的 `maintenance` 同样按运行身份记录（见
[运行时控制](../runtime/runtime-control.md)）。

**取消是请求，不是状态切换。** `POST .../workflows/:wfid/runs/:rid/cancel` 在运行行上记录请求，返回 `202` 和记录的时刻；重复请求（即使换一个幂等键）
返回同一个时刻。请求只写 `workflow_runs.cancel_requested_at`，不改动运行的状态、节点状态、会话或 Workspace——结算由调度器在下一轮完成（见下文）。
`pending` 和 `running` 的运行接受请求；已经进入终态的运行返回 `409 run_not_cancellable`。授权按运行上冻结的模型绑定判断：带绑定的运行只有发起人和
租户管理员可以取消，其他成员返回 `403 workflow_run_cancel_forbidden`；没有绑定的运行沿用 Issue 线程的成员规则。

**恢复接口已注册，但本阶段不可用。** `POST .../workflows/:wfid/runs/:rid/resume` 按最终契约校验请求体：`mode` 可选，取值
`keep`（默认）、`node_files`、`checkpoint`，未知字段返回 `400 unknown_field`，非法取值返回 `400 invalid_field_value`；授权与取消相同，
带冻结绑定的运行只有发起人和租户管理员可以调用，其他成员返回 `403 workflow_run_resume_forbidden`。任何合法请求都返回
`501 workflow_run_resume_unavailable`，不改动运行状态，也不写幂等记录——重复请求得到同样的拒绝，而不是重放一条记录下来的成功。运行的
Workspace 在失败时已经声明删除，`keep` 没有可恢复的状态，所以前端不提供恢复入口。

## 调度

WorkflowRun 由 Cloud server 里的调度循环推进（`AdvanceWorkflowRunsOnce`，每 10 秒一轮）：

- 每一轮只从**已提交的状态**重新计算可执行的节点：运行行上已持久化的节点状态，加上冻结的已发布图。进程在一轮中间崩溃也不会丢失任何东西，
  重复的一轮什么都不做。每个运行在自己的短事务里推进，节点结果以整行版本比较并交换的方式提交，所以同一个运行至多一个推进者，迟到或重复的
  推进不产生效果；没有提交结果的节点不会被记成成功。
- `start`、`condition`、`output` 是已提交状态的纯函数，在调度事务里直接计算并提交结果。`condition` 只激活与所选分支对应的出边，没有标注分支的
  出边视为 `else`；未被选中的分支上的节点标为未激活，永远不会执行。运行的图是有向无环图，没有循环和迭代。
- **Agent 节点在一次真实会话里执行。** 运行第一次调度到 Agent 节点时，Cloud 在运行冻结的 Project 里创建这次运行的 Workspace（isolated，属于该运行），
  等它有连上的 Node 后，按该节点引用的槽位取出创建时冻结的模型绑定，把会话工作项交给 Controller；会话的终态就是该节点的终态。同一运行同一时刻只有一个会话，
  下一条边上的节点等它结束才开始，会话开始前的等待有上限。运行 Workspace 的 `plugin` 步骤装的是创建运行时冻结的 Agent 插件 release，不是 Space 当前的
  选择：创建之后的在途升级不改变这次运行执行的内容；创建之后插件被移除时不补装，交给下面的启动判定以 `agent_plugin_unavailable` 失败。运行 Workspace
  建不出来时节点以 `workspace_unavailable` 失败，会话开始时发现冻结的插件版本或槽位绑定
  缺失时以 `agent_plugin_version_unfrozen`、`workflow_executor_unconfigured` 失败；Workspace 已经就绪、但这次运行冻结的 Agent 插件没有可用实例
  （缺失、安装失败或版本不是冻结的那个）时，节点在交给会话之前就以 `agent_plugin_unavailable` 失败，运行同样进入终态。这些情况都不会停在 `running` 或报告没有发生的成功。**会话已经开始、但它的 Node 再也报不出消息时**，
  节点以 `node_unavailable` 结算失败，运行随之进入终态：依据是 Node 的可达性，而不是会话已经跑了多久——心跳超过 `issue_runs.delivery_unreachable_after`（默认 30 分钟），
  或者沙箱连同其上的 Node 一起消失（强制停止留下的正是这个状态），都表示 Node 已经不在了。这个判断和它的窗口与 IssueRun 交付、释放路径用的是同一个
  （`runWorkspaceNodeUnknown`），两种运行不会对"Node 还在吗"给出不同答案；窗口未配置时不会退化成零长度窗口，会话保持等待而不是被立刻误判为失败。
  `node_unavailable` 是 Cloud 自己下的判断（也是 Node 不可达时公开的操作失败码），不是 Node 上报的会话结束原因。
  **会话已经开始、Node 仍然可达，但既没有提交结果、也没有成功的 `turnEnded` 回执时**，执行行的 `created_at` 超过
  `workflow.session_turn_timeout`（默认 2 小时）后节点以 `session_turn_deadline_exceeded` 失败，运行进入终态。这也是 Cloud 自己下的
  判断，按执行行计时（重试产生的新执行行重新计时），只使用数据库时钟，回放、重启和副本推进都不会重置。判断顺序是结果优先：
  已提交的结果按它自己的原因结算，Node 不可达仍报 `node_unavailable`，两者都不成立时才轮到窗口；窗口把运行带入终态之后才到达的结果
  保留在执行行上，不改写已经终态的运行。运行进入终态后先进入成果保存屏障，保存与释放分别记录，见下文。
- **Agent 节点重试由冻结在节点上的 `agentConfig.retry` 决定。** 只有会话以 `agent_failed` 结束、策略启用且预算未用完时才重试：预算是
  `maxRetries + 1` 次尝试（`maxRetries` 取值 0–5，默认 2），下一次尝试在 `initialDelaySeconds × 2^(n-1)` 之后开始（`initialDelaySeconds`
  取值 0–300，默认 10，单次等待上限 600 秒）。等待期间节点提交 `retrying`：运行仍是 `running`，节点保留失败尝试的 `startedAt`、
  `error` 和 `output.reason`，写入数据库时钟计算的 `retryAt` 并清空 `finishedAt`；它继续占用运行唯一的会话位置，直到 `retryAt` 到达才启动
  下一次尝试。新尝试使用自己的执行身份（`workflow_node_attempt`），上一次尝试的结果和结束请求不会被它复用。预算用完、策略关闭或原因不在
  `agent_failed` 内时，节点以最后一次的原因失败，运行进入终态并进入成果保存屏障。发布时 `agentConfig.retry` 必须是完整对象，缺失或为 `null`
  表示默认策略，未知键忽略；已冻结的历史快照不重新校验，无法解析的策略按默认值执行。
- **工作流会话是单回合的，结束由 Cloud 决定。** 初始回合的 `turnEnded` 回执（`stop_reason` 不是 `cancelled`）到达后，调度器通过既有的 `EndSession`
  命令让 Node 结束会话；Node 上报的会话执行结果仍是节点终态的唯一证据。结束请求放出后超过确认窗口仍没有会话终态时，节点以
  `session_end_deadline_exceeded` 失败，运行进入终态并进入成果保存屏障——不会再有永久 `running` 的运行。
- **取消请求由调度器结算，并且有界。** 记录请求后，`pending` 的运行不再启动：不写 `started_at`、不校验图、不创建 Workspace，直接以 `cancelled` 结束；
  `running` 的运行不再派发新节点。正在执行的会话，已提交的结果保留为节点自己的终态（那个结果就是实际跑出来的东西），运行仍以 `cancelled` 结束；还没有结果时，
  调度器复用该节点已有的结束请求（没有就放出 `end_session{cancelled}`），等待上限仍是 `issue_runs.agent_end_confirm_timeout`，超过后节点和运行都以
  `cancelled` 结束；Node 已不可达时取消优先于 `node_unavailable`，同样以 `cancelled` 收尾。每种终态都先保存整次成果，再声明正常释放；请求记录之后才到达的结果
  保留在执行行上，不改写已经终态的运行。
- **Workspace 释放以成果保存为前提。** 未保存的工作区保留；保存、执行与实际释放分别记录，详见下节。

## 成果保存与历史归档

执行结果、`delivery` 和 `workspaceRelease` 分别记录，列表和详情均返回这三个事实。运行进入终态后，先撤销会话写资格并等待所有已登记会话停止；确认没有写入会话的运行记 `not_required`，不伪造 Revision。其余运行整次交付一份 Revision，保存成功或核验无改动后才声明正常释放；保存不改变原执行结果。

交付输入冻结全部节点尝试、原 execution ID、同一 checkout、每个历史对象键和不可变交付 ref。Node 保存各会话的原始字节，在主历史对象里存储 `workflow_manifest_v1` 清单；Cloud 在事务外核验 ref、清单身份以及全部对象的大小和 SHA-256，再在一个短事务内登记 Revision、回执和交付结果。历史缺失、错误清单或上传失败不会被当作成功，也不会删除未保存的 Workspace。

失败重试有独立交付工作项和 ref，但不创建第二个逻辑成果。达到已有交付时限后记 `failed` 并保留工作区；未配置存储或 Git Server 时记 `skipped` 并保留工作区。只有 `released` 表示实际删除完成。旧 Node 未声明 `workflow_history_delivery` 时保持等待，升级 Node/Controller 后才能派发新输入。

工作流会话目录含同 Project 的工作流 Workspace、冻结运行名称和归属；目录不参与授权。任务调用和独立运行都从 Project 快照初始化，后续节点与重试复用本次 checkout，不恢复任务前序成果。完整节点日志界面和跨任务交接不属于此保存链路。

契约依据：[整次成果与历史归档 ADR](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/20261025-run-owned-git-delivery-and-history.md)。ADR 在实现和真实多节点验收完成前保持 `approved`。

开发夹具（`collaboration.development_fixtures`）会装入一个确定性的运行模拟器，在创建运行时就填好执行轨迹，并在输出里说明没有真实执行；生产环境
不装它。工作流 IssueRun 通过 `workflow_invocation_ref` 关联同一事务里创建的 WorkflowRun，执行由上面的调度循环推进。
WorkflowRun 的实际状态更新成功后，在同一事务里将状态、时间和错误投影回关联的 IssueRun；未更新到行时不投影，契约冲突使事务回滚。
`awaiting_input` 不改变 IssueRun，未关联 IssueRun 的直接运行也不产生协作记录。输出摘要作为 `system` 的 `run.message` 活动写入，
最多 20000 字节且保持合法 UTF-8，不创建工作流作者评论（见 [Issue 协作](../collaboration/interactions.md#端口issue-只拥有消费侧的契约)）。

## 插件资产导入

`kind = workflow` 的插件把工作流模板作为资产随插件发布。管理员为空间安装这样的插件时，Cloud **自己**下载插件包，把其中每一份
`assets/workflows/*.json` 导入为租户的一个可编辑工作流，并同时创建它的第一个已发布版本：

- 导入由空间的期望状态触发，与 Node 是否安装无关：没有任何存活运行时的空间也能拿到模板。
- 后台循环每 `plugins.asset_import_interval`（默认 30 秒）一轮，下载和解析都在事务之外完成，只把结果写进一个短事务。下载使用与
  model-gateway 相同的受检出网客户端和同一份部署出网策略，不跟随重定向（见 [部署方共享出网配置](../operations/deployment-egress.md)）。
- 包的位置来自目录快照里被选定的那个版本，每次领取时解析，不单独保存。导入按**被领取的修订**加以隔离：下载期间管理员重新安装，旧的这次尝试
  什么都不写，下一轮按新的选择重新解析。
- **逐文档的结果**：成功导入的路径成为跳过键，以后不再重复导入；失败的路径（解析失败、发布校验不通过、与已有工作流重名 `name_conflict`、
  PostgreSQL 无法原样存储的 JSON 等）记录有界的失败码，等管理员下次触发时重试。整包下载或解压失败只记录包级的失败，不写任何文档结果。
- **管理员触发重试**：再次安装同一版本会重试失败的文档，安装新版本会重试所有尚未成功导入的路径；已经导入的副本不会被改动。在这个功能之前
  安装的插件不会被回填，重新安装就是管理员的显式触发。
- 导入结果通过 `GET .../spaces/:spaceId/plugins/assets` 单独读取：安装结果和资产导入结果是两件不同的事。

导入之后的工作流就是普通的租户工作流，资产的生命周期（插件升级或移除）不会改写已经导入的文档或已发布版本。

## 相关决策

- [工作流由 Cloud 侧调度器执行：WorkflowRun 是执行聚合](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/0-cloud-side-scheduler-owns-workflow-execution.md)（approved）
- [工作流 agent 节点按执行器槽位引用执行器，图里不再存 agent CLI 与模型 ID](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/20261015-executor-slots-replace-agent-model-ids.md)（approved）
- [工作流会话在初始回合结束后由 Cloud 调度器结束，结束请求有界等待](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/20261019-workflow-session-end-after-initial-turn.md)
- [工作流 agent 节点按重试策略重试，恢复接口按最终契约注册但在首期不可用](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/20261021-node-retry-and-resume-contract.md)（approved）
- [插件资产导入产出已发布快照：资产生命周期不改写快照，运行只冻结 snapshotId](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/20261016-asset-import-and-published-snapshots.md)
- [Workflow 插件资产由 Cloud 自行取包、逐文档导入为可编辑副本](https://github.com/ora-space/specs/blob/main/decisions/cloud/plugin-marketplace/20261016-workflow-plugin-assets.md)
- [个人槽位映射为工作流解析执行器，每次运行按槽位锁定多组模型绑定](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261015-executor-slot-mappings-and-per-slot-run-bindings.md)（approved）
- [`snapshotId` 是保留字段：发布门禁拒绝作者声明，存量版本不可选](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-collaboration/20261021-snapshot-id-is-a-reserved-field.md)（approved）
