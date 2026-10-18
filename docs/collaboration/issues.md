# Issue 看板

[English](issues.en.md) | 中文

Issue 是租户内的任务卡片，Issue 看板是 Cloud 里第一个纯业务资源：它不依赖运行时，只读写 PostgreSQL。在 Issue 上
`@` 人、Agent 或工作流发起协作的部分见 [Issue 协作](interactions.md)，Agent 运行本身见 [Agent IssueRun](../agent-runs/issue-runs.md)。

看板对租户的全部当前成员共享：任何成员都可以读写租户内的 Issue、状态列、标签和评论；只有保存的视图属于个人。

## 资源

| 资源 | 表 | 说明 |
|---|---|---|
| Issue | `issues` | 标题、描述、状态、优先级、负责人、父 Issue、关联 Project、看板位置、租户内编号、自定义属性 |
| 状态列 | `issue_statuses` | 看板的列，租户可以自定义 |
| 标签 | `labels`、`issue_labels` | 租户级标签，与 Issue 多对多 |
| 订阅者 | `issue_subscribers` | 关注某个 Issue 的成员 |
| 评论 | `issue_comments` | 支持回复（`parentId`），作者是一个 ActorRef |
| 活动 | `issue_activities` | 只追加的时间线投影，没有公开的修改或删除接口 |
| 上下文引用 | `issue_context_refs` | 指向外部资源的不透明引用，只记录、不复制内容 |
| 视图 | `issue_views` | 个人保存的筛选条件，只对创建者可见 |

## Issue

- **编号**：每个租户内从 1 开始递增的 `number`，在全局锁保护的事务里分配。
- **优先级**：`urgent`、`high`、`medium`、`low`、`none`，默认 `none`。
- **状态**：必须是租户状态目录里一个未删除的列，默认 `todo`。
- **负责人**是一个多态的 ActorRef：`assigneeType` 为 `user`、`agent` 或 `team`，加上 `assigneeId`。用户负责人必须是
  已有的活动用户；Agent 和团队负责人目前只校验 UUID 形状，不解析。为兼容旧客户端，仍接受单独的 `assigneeUserId`。
  指派只改变"谁负责"，**不触发任何执行**。
- **父 Issue**：同一租户内一个存在的 Issue，不能指向自己。删除一个 Issue 时，它的子 Issue 变为无父级。
- **关联 Project**（`projectRef`）：只校验 UUID 形状，不检查 Project 是否存在；没有外键，Project 删除后这个引用不会
  报错，只是不再生效。在 Issue 上 @Agent 运行要求它指向本 Space 里存在的 Project（见 [Issue 协作](interactions.md)）。
- **描述**最长 20000 字符，标题最长 200 字符；`properties` 是一个任意 JSON 对象。

列表 `GET /tenants/:tid/issues` 一次返回整个看板（不分页），按状态列的位置、卡片位置、ID 排序；`?q=` 按标题和描述做
不区分大小写的子串匹配，用户输入里的 `%` 和 `_` 按字面处理。`GET /tenants/:tid/issue-groups?groupBy=` 按状态、优先级
或负责人分组返回同样的内容。

## 看板位置与移动

每张卡片有一个浮点 `position`，同一列内按它排序：

- 新建的卡片和改变状态的卡片放在目标列的顶部（当前最小位置减一）。
- `POST .../issues/:iid/move` 可以同时修改状态和位置：给出 `beforeId` 与 `afterId` 时取两者的中点，只给一个时放在它的
  紧邻处，都不给但状态变了时放到新列顶部。锚点之间没有空隙时返回 `409 position_conflict`。
- `POST .../issues/batch` 对至多 100 个 Issue 原子地应用同一个状态、优先级或负责人修改。

## 状态列

每个租户第一次用到看板时，会自动补齐 7 个**系统列**：

| key | 名称 | 类别 |
|---|---|---|
| `backlog` | Backlog | `unstarted` |
| `todo` | Todo | `unstarted` |
| `in_progress` | In Progress | `started` |
| `in_review` | In Review | `started` |
| `blocked` | Blocked | `started` |
| `done` | Done | `done` |
| `cancelled` | Cancelled | `closed` |

租户可以新增自定义列（`key` 为小写字母、数字和下划线，最长 32 个字符，类别为 `unstarted`、`started`、`done`、`closed`
之一），修改名称、描述、类别、颜色、图标和位置。系统列不能删除（`409 system_status_required`）；自定义列删除是软删除。

## 评论、活动与时间线

- **评论**的作者是 ActorRef：用户发表的评论为 `user`；Agent 或团队运行的回复以 `agent` 或 `team` 身份写入；`system`
  在 schema 里允许，但目前没有写入路径。回复的父评论必须属于同一个 Issue 且未删除。评论正文最长 20000 字符，支持编辑
  和软删除。发表评论时可以附带 `targets`，即 `@` 选择的协作目标（见 [Issue 协作](interactions.md)）。
- **活动**由服务端在业务变化时追加，例如运行进入队列、开始、完成、失败、取消，工作流的输出也以 `system` 身份的活动
  出现。活动没有公开的修改或删除接口，业务状态也从不由活动重建。
- **时间线** `GET .../issues/:iid/timeline` 把评论和活动合并成一个列表，按**同一个每 Issue 序号 `seq`** 排序，而不是
  按创建时间。评论和活动共用这个序号空间，同一个 Issue 里不会重复。时间线是高层投影，不是执行日志，也不是事件源；
  Agent 会话的完整记录在 [Thread](../agent-runs/threads.md) 里。

## 上下文引用

`.../issues/:iid/context-refs` 记录这个 Issue 关联的外部资源：`refType` 为 `parent_issue`、`run`、`timeline_message`、
`pull_request`、`project`、`workspace`、`acceptance_criteria` 之一，`refId` 是一个 UUID。引用只存指针，不复制、不展开
内容；同一个引用不能重复添加（`409 context_ref_exists`），删除是硬删除。运行创建时，这些引用会随上下文快照写入运行的输入。

## 标签、订阅与视图

- **标签**属于租户，名称在未删除的标签中唯一；给 Issue 添加或移除标签是关联表的硬增删。
- **订阅**：请求体的 `userId` 指定一位当前成员，把他加入或移出 Issue 的关注者，重复添加是幂等的。
- **视图**保存一个名称和一个 `filter` 对象，只属于创建者：其他成员既看不到也改不了，访问别人的视图返回 `404`。

## 相关决策

- [Issue 协作：`@` 只选择类型化目标，执行只来自显式意图](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-collaboration/0-typed-targets-and-explicit-execution-intent.md)
- [协作 Space 是租户内的共享、可见性与授权边界](https://github.com/ora-space/specs/blob/main/decisions/cloud/workspace/0-collab-space-boundary.md)
