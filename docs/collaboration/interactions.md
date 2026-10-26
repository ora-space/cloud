# Issue 协作：@ 目标、交互与执行意图

[English](interactions.en.md) | 中文

在 Issue 的评论里 `@` 一个目标，是"选中一个协作对象"，而不是"执行"。被选中的可以是人、Agent、团队或工作流，不同类型
产生不同的交互模式；只有显式的执行意图（给 Agent 的任务，或者对工作流表单的确认）才会创建运行。本文说明这套模型以及它在
Cloud 里的实现，决策依据是 [Issue 协作 ADR](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-collaboration/0-typed-targets-and-explicit-execution-intent.md)。

## 三层概念

```text
Comment（评论）──< Interaction（一次 @ 选择）──0..1── IssueRun（运行）
   1 条评论              每个被选中的目标一行            只有显式意图才创建
```

- **评论 ≠ 交互 ≠ 运行**：一条评论可以 @ 多个目标，得到多条交互；一条交互至多产生一个初始运行。评论表上没有 `run_id`，
  也不能加：运行的来源记在运行自己的 `trigger_evidence_kind` 和 `trigger_evidence_ref_id` 上（来源是评论、交互或手动创建）。
- 评论正文里的 `@x` 或 `mention://type/id` 只用于显示，不是路由协议；目标通过评论请求体的 `targets` 数组传入：
  `[{"type":"agent","id":"…","task":"…"}]`。
- **工作流不是 ActorRef。** 它只出现在目标一侧，所以工作流的输出写成 `system` 身份的活动，而不是以工作流名义发表的评论。

## 目标类型与交互模式

| 目标 `type` | 模式 | 发表评论时发生什么 |
|---|---|---|
| `user` | Mention | 只记录交互，**不**产生运行。`@人` 永远不会触发执行 |
| `agent` | Task | 必须带非空的 `task`（与评论正文是两回事），同一事务里创建初始运行 |
| `team` | Task | 与 Agent 相同的契约；不假定团队有 leader、成员或委派结构 |
| `workflow` | Form | 只记录一条"已选中、未确认"的交互，不创建运行；用户填写表单并确认后才执行 |

每个被选中的目标写成一行 `issue_interactions`：`mode` 为 `mention`、`task` 或 `form`；交互没有状态列，`run_id IS NULL` 就是
未确认，非空就是已确认。

**目标发现。** `GET /tenants/:tid/collaboration/targets?q=` 返回可以 @ 的目标：人来自租户的当前成员，Agent 来自 Space 的
Agent 名册（`space_agents`，由已安装的 Agent 插件形成，见 [插件市场](../plugins/marketplace.md)），工作流来自租户的工作流
文档。每个目标带一个交互描述 `{mode, requiresTask, formRef}`。团队在任何部署里都没有真实后端，所以不会出现在生产的目标列表里。

## Task Mode：@Agent

发表评论时，Cloud 在同一个事务里：

1. 在事务内按 `space_agents` 解析目标，必须是本租户里**活动**的 Space Agent；解析不到时按目录回退，仍然找不到返回 `404`，不泄露
   目标是否存在。
2. 执行 **Project 关口**：Issue 必须通过 `projectRef` 关联一个存在、且与 Agent 属于同一个 Space 的 Project。没有 Project 就没有
   仓库，运行无从开始，所以整条评论连同它的交互和运行一起回滚，返回 `409 issue_project_required`。
3. 构建上下文快照（Issue 标题与描述、任务、最近的评论、上下文引用），并把 Agent 插件的身份和当时固定的版本写进运行输入，
   之后升级插件不会改变历史运行执行的内容。
4. 创建 `queued` 的 IssueRun 和一条 `run.enqueued` 活动。同一个 Issue 对同一个执行者同时只能有一个排队或已派发的运行。

评论事务提交之后才派发运行；Agent 运行由 Cloud 自己的后台循环推进，见 [Agent IssueRun](../agent-runs/issue-runs.md)。

## Form Mode：@工作流

工作流的执行边界只有一个：显式的**确认**。

```text
选中工作流 ──▶ 读取表单描述 ──▶ 填写（可选 AI 辅助）──▶ 确认 ──▶ WorkflowRun + IssueRun
   不执行        不执行             不执行、不建运行        唯一的执行边界
```

- **表单描述**（`GET /tenants/:tid/collaboration/forms/:formRef?issueId=…`）是面向 Issue 的渲染描述：`formRef`、标题、说明和
  字段列表。它不是工作流的 schema，也不绑定任何 schema 技术。字段分三段：平台字段、工作流 Start 节点声明的变量、图的全局变量。
  前端先读取当前文档，只为定位平台版本选择器及其默认版本；确定默认版本（最新的可选已发布版本）后，初次打开就按该版本通过
  `GET /tenants/:tid/workflows/:wfid/snapshots/:snapshotId/form?issueId=…` 读取表单，切换版本时按新版本重新读取。
  因此显示的表单与 Confirm 校验的版本始终相同，活稿里未发布的字段不出现在默认视图。
  这条同源承诺不依赖活稿：只要还有可选的已发布版本，表单就注入平台版本选择器并默认最新的可选版本，活稿里的同名声明既不渲染、
  也不参与版本游标解析。本次决策之前发布、自己声明了 `snapshotId` 的版本不进入选择器，Confirm 解析到它时返回
  `409 workflow_version_reserved_field`，不改选别的版本（见 [工作流](../workflows/workflows.md)）。用启动字段撤下选择器时界面
  没有选择器：不提交该键时按最新已发布版本解析，提交了仍按版本游标读取；解析到的版本自己声明了保留键时同样返回
  `409 workflow_version_reserved_field`，不会执行。
- **平台字段**只在带着 Issue 打开表单时注入：仓库地址和分支（默认取 Issue 关联 Project 的仓库与默认分支，必填）、提示词、要执行
  的已发布版本 `snapshotId`、附加的上下文引用 `context_refs`。工作流自己声明了同名变量时，以工作流的声明为准；保留字段
  `snapshotId` 例外，这个键始终属于平台，作者的声明不进入表单（见 [工作流](../workflows/workflows.md)）。
- **版本选择器只列出可选的版本。** `snapshotId` 这个平台字段只提供图里不再声明保留字段 `snapshotId` 的已发布版本，选项上限也只
  计这些版本，默认值是最新的一个；一个可选项都没有时表单不带选择器。有版本因此被排除时，选择器的说明会写明原因并请作者从
  不再声明该字段的活稿重新发布（见 [工作流](../workflows/workflows.md)）。
- **AI 辅助**（`POST .../issues/:iid/collaboration/assist`）只返回字段级的建议补丁和建议的上下文引用：从不创建运行，也不写入
  持久的上下文引用。应用建议是用户的动作。它判断提交值的形状所用的是用户正在填的表单：提交了版本游标时用该版本，没有游标时用
  表单默认的最新可选已发布版本，全部版本都不可选时用活稿，所以只接受用户眼前那个版本声明的字段。Confirm 对无游标提交仍解析到
  最新已发布版本并按保留字段规则拒绝，不会改选版本；Assist 不执行，也不替用户改选。
  生产环境没有接入辅助提供方，返回 `503 assist_unavailable`。
- **确认**（`POST .../issues/:iid/interactions/:ixid/confirm`）是唯一把工作流交互变成执行意图的转换：
  - 前端校验只是体验，Cloud 在确认时**重新**解析目标，并按**被确认的那个已发布版本**的表单校验提交的值；没有任何已发布版本的
    工作流不能确认，因为不能执行未发布的草稿。
  - Cloud 只按所选发布版本自身的字段校验，不再与当前活稿比较。选择旧版本就是按那份快照执行：即使它的字段与活稿不同，
    提交符合该版本表单即可通过；缺少该版本必填字段或提交它未声明的字段时，按该版本的 schema 拒绝。
  - `snapshotId` 始终由平台拥有，提交里的这个键始终是版本游标，不看活稿是否还声明着同名变量。解析到的已发布版本如果在自己的图里声明了
    它，确认返回 `409 workflow_version_reserved_field`，参数带上版本和字段；带不带版本游标都一样，既不会静默换成别的版本，也不会留下
    运行或已认领的交互。
  - 提交的值作为交互的输入保存，被执行的版本冻结进运行输入；运行之后工作流再被编辑或发布，它仍然知道自己执行的是哪个版本。
  - 同一事务里创建一个 WorkflowRun 和一个 IssueRun，IssueRun 的 `workflowInvocationRef` 指向该 WorkflowRun（与租户一起的复合外键，且唯一）；直接运行、manual 工作流 IssueRun 与确认共用同一个创建函数。已关联的 IssueRun 不交给 `ExecutionDispatcher`。
  - 用 `run_id IS NULL` 的比较并交换认领交互，重复确认返回 `409 interaction_already_confirmed`；对非表单交互确认返回
    `409 interaction_not_confirmable`。

## 端口：Issue 只拥有消费侧的契约

Issue 协作通过一组端口向外部能力请求"做一件事"，而不定义那个能力内部是什么：

| 端口 | 作用 | 生产环境 |
|---|---|---|
| `CollaborationDirectory` | 列出、解析非人类目标 | Space Agent 名册在前、工作流目录在后的链式目录 |
| `FormDescriptorProvider` | 按 `formRef` 给出表单描述 | 工作流文档投影成的表单 |
| `InputAssistProvider` | 表单的 AI 辅助 | 未接入（503） |
| `ContextBuilder` | 组装运行的上下文快照 | 未接入，退回到"任务、表单值、目标、原始上下文引用"的确定性快照 |
| `ExecutionDispatcher`、`ExecutionObserver` | 把排队的运行交给外部执行者，并观察其进度 | 未接入：团队运行在生产中保持 `queued`；工作流 IssueRun 不经过这里，由关联的 WorkflowRun 执行 |

Agent 运行不走 `ExecutionDispatcher`，由 Cloud 自己的调度推进（见 [Agent IssueRun](../agent-runs/issue-runs.md)）。直接从工作流
发起的运行是另一个聚合 WorkflowRun，见 [工作流](../workflows/workflows.md)。

关联工作流的 IssueRun 由 WorkflowRun 的实际状态更新在同一事务内推进：未更新到行时不投影，保留值 `awaiting_input`（首期没有写入者，
见 [工作流](../workflows/workflows.md)）不改变 IssueRun，契约冲突使事务回滚。输出摘要写成 `system` 的 `run.message` 活动，最多 20000 字节且保持合法 UTF-8，不创建工作流作者评论。
没有关联 IssueRun 的直接工作流运行不产生这些协作记录。

**开发夹具。** `collaboration.development_fixtures: true`（环境变量 `CLOUD_COLLABORATION_DEVELOPMENT_FIXTURES`）在 Store 上装入
`internal/collab` 的进程内替身：固定 ID 的 Agent 和团队目标、确定性的上下文构建器（最近至多 10 条评论，不用 AI）、给出固定输出的
派发器和 AI 辅助。工作流目标仍然来自数据库。这个开关只用于开发和演示，生产默认关闭；它与认证完全独立：开启 GitHub 登录不会打开
夹具，认证失败也绝不会退回到夹具身份。替身不建任何模拟领域表。

## 相关决策

- [Issue 协作：`@` 只选择类型化目标，执行只来自显式意图](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-collaboration/0-typed-targets-and-explicit-execution-intent.md)
- [Issue 上 @Agent 的 IssueRun 由 Cloud 编排](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/0-agent-run-in-disposable-isolated-workspace.md)
- [工作流由 Cloud 侧调度器执行](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/0-cloud-side-scheduler-owns-workflow-execution.md)（approved）
- [`snapshotId` 是保留字段：发布门禁拒绝作者声明，存量版本不可选](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-collaboration/20261021-snapshot-id-is-a-reserved-field.md)（approved）
