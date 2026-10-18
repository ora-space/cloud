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
选中工作流 ──▶ 读取表单描述 ──▶ 填写（可选 AI 辅助）──▶ 确认 ──▶ IssueRun
   不执行        不执行             不执行、不建运行        唯一的执行边界
```

- **表单描述**（`GET /tenants/:tid/collaboration/forms/:formRef?issueId=…`）是面向 Issue 的渲染描述：`formRef`、标题、说明和
  字段列表。它不是工作流的 schema，也不绑定任何 schema 技术。字段分三段：平台字段、工作流 Start 节点声明的变量、图的全局变量。
- **平台字段**只在带着 Issue 打开表单时注入：仓库地址和分支（默认取 Issue 关联 Project 的仓库与默认分支，必填）、提示词、要执行
  的已发布版本 `snapshotId`、附加的上下文引用 `context_refs`。工作流自己声明了同名变量时，以工作流的声明为准。
- **AI 辅助**（`POST .../issues/:iid/collaboration/assist`）只返回字段级的建议补丁和建议的上下文引用：从不创建运行，也不写入
  持久的上下文引用。应用建议是用户的动作。生产环境没有接入辅助提供方，返回 `503 assist_unavailable`。
- **确认**（`POST .../issues/:iid/interactions/:ixid/confirm`）是唯一把工作流交互变成执行意图的转换：
  - 前端校验只是体验，Cloud 在确认时**重新**解析目标，并按**被确认的那个已发布版本**的表单校验提交的值；没有任何已发布版本的
    工作流不能确认，因为不能执行未发布的草稿。
  - 用户看到的表单来自当前文档；如果它与被确认版本的字段已经不一致，确认被拒绝并列出变化的字段
    （`409 workflow_version_fields_changed`），而不是用用户没见过的 schema 校验他填的值。
  - 提交的值作为交互的输入保存，被执行的版本冻结进运行输入；运行之后工作流再被编辑或发布，它仍然知道自己执行的是哪个版本。
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
| `ExecutionDispatcher`、`ExecutionObserver` | 把排队的运行交给外部执行者，并观察其进度 | 未接入：团队运行和从 Issue 确认的工作流运行在生产中保持 `queued` |

Agent 运行不走 `ExecutionDispatcher`，由 Cloud 自己的调度推进（见 [Agent IssueRun](../agent-runs/issue-runs.md)）。直接从工作流
发起的运行是另一个聚合 WorkflowRun，见 [工作流](../workflows/workflows.md)。

**开发夹具。** `collaboration.development_fixtures: true`（环境变量 `CLOUD_COLLABORATION_DEVELOPMENT_FIXTURES`）在 Store 上装入
`internal/collab` 的进程内替身：固定 ID 的 Agent 和团队目标、确定性的上下文构建器（最近至多 10 条评论，不用 AI）、给出固定输出的
派发器和 AI 辅助。工作流目标仍然来自数据库。这个开关只用于开发和演示，生产默认关闭；它与认证完全独立：开启 GitHub 登录不会打开
夹具，认证失败也绝不会退回到夹具身份。替身不建任何模拟领域表。

## 相关决策

- [Issue 协作：`@` 只选择类型化目标，执行只来自显式意图](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-collaboration/0-typed-targets-and-explicit-execution-intent.md)
- [Issue 上 @Agent 的 IssueRun 由 Cloud 编排](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/0-agent-run-in-disposable-isolated-workspace.md)
- [工作流由 Cloud 侧调度器执行](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/0-cloud-side-scheduler-owns-workflow-execution.md)（approved）
