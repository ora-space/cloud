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

## 执行器槽位

Agent 节点不在图里写死 Agent CLI 或模型 ID，而是引用一个**执行器槽位**：

- `default` 是每个图都可以直接引用的保留槽位，指运行发起者个人的默认模型；Agent 节点不指定槽位时就用它。图不能重新声明 `default`。
- 图可以声明具名槽位，带给运行者看的说明和作者的建议（`hint`）。槽位名有固定的语法，说明按字符数限制长度。
- 运行开始时，按**发起者自己**的配置解析每个被引用的槽位，不回退到 `default`、不采用作者的建议、也不借用别人的配置。任何一个槽位解析
  不了，启动就以 `409 workflow_executor_unconfigured` 拒绝，参数里逐个列出槽位、依赖它的节点、原因（例如 `slot_not_configured`、
  `default_model_missing`、`credential_missing`）以及槽位说明，前端可以一次性展示所有需要补的配置。
- 个人的具名槽位映射还没有实现，所以目前引用了具名槽位的图都会报告为未配置；只引用 `default` 的图按发起者的默认模型判断。

## 运行

工作流有三个启动入口，它们共用同一道准入：

| 入口 | 产生 |
|---|---|
| `POST .../workflows/:wfid/runs` | 一个 WorkflowRun（`workflow_runs`） |
| `POST .../issues/:iid/runs`，`executorType = workflow` | 一个工作流 IssueRun |
| 在 Issue 上确认 @工作流 的表单 | 一个工作流 IssueRun（见 [Issue 协作](../collaboration/interactions.md#form-mode工作流)） |

**准入**在调用方的事务里一次判定：

1. 解析要执行的已发布版本：调用方显式指定的版本，否则是最新的已发布版本。没有任何已发布版本时返回 `400 workflow_no_published_snapshot`，
   因为执行草稿会让之后的编辑改变一个已经创建的运行"执行过"的内容。
2. 用该版本的图校验输入：Start 节点声明的必填变量必须有值、类型必须匹配；图没有声明的键不检查（运行输入里还有平台自己写入的内容）。
3. 检查执行器槽位（见上文）。

任何一项不通过都不会留下运行、已认领的交互或活动。被执行的版本冻结在运行上，运行之后工作流再被编辑或发布，它仍然知道自己执行的是哪个版本。

## 调度

WorkflowRun 由 Cloud server 里的调度循环推进（`AdvanceWorkflowRunsOnce`，每 10 秒一轮）：

- 每一轮只从**已提交的状态**重新计算可执行的节点：运行行上已持久化的节点状态，加上冻结的已发布图。进程在一轮中间崩溃也不会丢失任何东西，
  重复的一轮什么都不做。每个运行在自己的短事务里推进，节点结果以整行版本比较并交换的方式提交，所以同一个运行至多一个推进者，迟到或重复的
  推进不产生效果；没有提交结果的节点不会被记成成功。
- `start`、`condition`、`output` 是已提交状态的纯函数，在调度事务里直接计算并提交结果。`condition` 只激活与所选分支对应的出边，没有标注分支的
  出边视为 `else`；未被选中的分支上的节点标为未激活，永远不会执行。运行的图是有向无环图，没有循环和迭代。
- **Agent 节点目前会让运行以明确的原因失败。** Agent 节点需要一个运行 Workspace 和一次真实的 Node 会话，这条执行链路还没有接入；在此之前，
  调度到 Agent 节点的运行立即失败并说明原因，而不是停在 `running` 或报告没有发生的成功。

开发夹具（`collaboration.development_fixtures`）会装入一个确定性的运行模拟器，在创建运行时就填好执行轨迹，并在输出里说明没有真实执行；生产环境
不装它。工作流 IssueRun 在生产中目前没有执行端，保持 `queued`（见 [Issue 协作](../collaboration/interactions.md#端口issue-只拥有消费侧的契约)）。

## 插件资产导入

`kind = workflow` 的插件把工作流模板作为资产随插件发布。管理员为空间安装这样的插件时，Cloud **自己**下载插件包，把其中每一份
`assets/workflows/*.json` 导入为租户的一个可编辑工作流，并同时创建它的第一个已发布版本：

- 导入由空间的期望状态触发，与 Node 是否安装无关：没有任何存活运行时的空间也能拿到模板。
- 后台循环每 `plugins.asset_import_interval`（默认 30 秒）一轮，下载和解析都在事务之外完成，只把结果写进一个短事务。下载使用与
  model-gateway 相同的受检出网客户端和公网地址策略，不跟随重定向（见 [出网策略](../models/model-gateway.md#出网策略)）。
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
- [插件资产导入产出已发布快照：资产生命周期不改写快照，运行只冻结 snapshotId](https://github.com/ora-space/specs/blob/main/decisions/cloud/workflow/20261016-asset-import-and-published-snapshots.md)
- [Workflow 插件资产由 Cloud 自行取包、逐文档导入为可编辑副本](https://github.com/ora-space/specs/blob/main/decisions/cloud/plugin-marketplace/20261016-workflow-plugin-assets.md)
- [个人槽位映射为工作流解析执行器，每次运行按槽位锁定多组模型绑定](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261015-executor-slot-mappings-and-per-slot-run-bindings.md)（approved）
