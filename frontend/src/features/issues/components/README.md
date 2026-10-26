# issues/components：任务页子组件

[中文](README.md) | [English](README.en.md)

## 职责

任务看板与详情页的子组件：卡片与行、创建对话框、活动栏（时间线、评论、@ 目标、Workflow 表单配置/审阅/确认）、上下文引用栏与属性栏。数据来自 `features/issues/api`，不直接发 HTTP。

## 文件

| 文件                                                                                                                                            | 说明                                                                                                  |
| ----------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| `task-layout.tsx`                                                                                                                               | 任务可用宽度测量、独立侧栏与窄屏抽屉                                                                  |
| `execution-sidebar.tsx`                                                                                                                         | 当前执行、最近记录与所选运行统计组合                                                                  |
| `pull-request-refs.tsx`                                                                                                                         | 安全 PR 引用展示与显式增删                                                                            |
| `issue-card.tsx` / `issue-row.tsx`                                                                                                              | 看板卡片与列表行                                                                                      |
| `create-issue-dialog.tsx` / `issue-project-select.tsx`                                                                                          | 新建任务及详情的真实项目选择；列表加载或失败不能清空已存关联                                          |
| `issue-properties-panel.tsx`                                                                                                                    | 详情页属性栏：状态、优先级、负责人、父/子任务、项目、标签、属性、上下文引用                           |
| `activity-panel.tsx`                                                                                                                            | 活动栏：时间线与新建评论/目标                                                                         |
| `issue-workspace.tsx`                                                                                                                           | 主区域对话、执行记录、任务动态页签；URL 保存历史 Run 选择，活动可定位详情，完整记录链接复用会话分析页 |
| `target-picker.tsx` / `pending-targets.tsx`                                                                                                     | 真实 @ 目录的加载、空、搜索与重试状态，以及待提交目标                                                 |
| `agent-request-failure.tsx`                                                                                                                     | 安全的任务拒绝文案，项目、目标及个人模型配置引导                                                      |
| `workflow-interaction-composer.tsx` / `dynamic-form-renderer.tsx` / `form-field-renderer.tsx` / `assist-suggestions.tsx` / `confirm-review.tsx` | Workflow 表单的渲染、AI 建议、审阅与确认                                                              |
| `context-refs-panel.tsx`                                                                                                                        | 上下文引用的增删                                                                                      |
| `*.test.tsx`                                                                                                                                    | 组件测试                                                                                              |

## 依赖与不变量

工作区组合 `features/issues/conversation` 的任务对话及 `features/issues/thread` 的会话面板；正文使用 `features/conversation-content`。切换页签与查看历史不会创建运行，成功提交 Agent 任务后回到最新会话。执行记录由真实 Run 列表驱动，未知 Agent 展示保守名称。

依赖 `features/issues/api`、`features/issues/types`、`features/issues/present`、`features/projects/api` 的真实项目列表、`features/spaces/current-space` 与 `components`；由 `features/issues` 的页面使用。草稿（评论、目标、表单）只在用户按下各自按钮时才写入服务端；属性栏的修改都经页面传入的 `onCommit`（带版本号）。项目选择来自当前已加入空间，不能回退到演示数据。

创建草稿按 tenant/space 重挂，活动草稿按 tenant/space/issue 重挂；切到已缓存的其他作用域也不能继承草稿。关联项目以服务端结果为准，版本化更新等待期间禁止再改属性，拒绝保留原值。缺项目提示定位到 `issue-project` 选择器；目标必须来自真实目录，空间合法性仍由服务端校验。

Workflow 确认从创建 interaction 开始锁住重复提交、返回修改与取消；确认失败后仍用同一 interaction 重试，包括切换版本后的重试。确认行通过 `features/workflows/api` 读取 `IssueRun.workflowInvocationRef` 对应的 WorkflowRun 状态，详情链接使用 `executorId`、该引用和当前路由的空间 slug。

初始读取按 `formRef + issueId` 取活稿，但只用来定位平台版本选择器及其默认版本。只有 `snapshotId` select 的全部选项都属于该 Workflow 的已发布快照时，才识别为平台版本选择器；列表读取完成前不开放表单，失败可重试。识别出默认版本后，表单本身按该版本的 snapshot form 渲染，用户不切换版本也一样，所以活稿里未发布的字段不会出现在表单里。作者自定义的同名文本字段或含自定义选项的 select 仍是普通输入，直接从活稿渲染。

选择器由活稿决定，没有它时组件无法告诉服务端执行哪个版本。活稿自己声明了 `snapshotId`（或用启动字段移除它）时表单按活稿渲染，Confirm 按活稿的版本规则执行最新已发布版本，显示的字段可能属于另一个版本。选择器存在、但切到的版本自己的图声明了这个键时，该版本的表单里没有平台字段，组件也不加回选择器；Confirm 仍按活稿判定该键归平台，作者值会被当成版本游标。两种情况都是作者占用保留键的边界，不是可依赖的配置；测试固定了同名变量仍是普通输入，不会伪装成选择器。是否在切换时禁止或提示这类版本留给后续决策（circus#405）。

用户切换平台版本选择器时，按 tenant/workflow/snapshot/issue 读取既有 snapshot form 接口，清空旧字段草稿并初始化新版本默认值；选择器保留响应回显的 snapshotId，不采用它仍指向最新版本的默认值。加载或失败期间不能审阅旧字段，失败可重试。读取表单不代表服务端准入通过，但 Confirm 只按所选版本自身的字段校验，不再与活稿比较：选中的旧版本哪怕字段和当前草稿不同，也按它自己的表单执行，所选版本缺必填字段或收到它未声明的字段时由服务端按该版本拒绝。拒绝会带上字段参数，表单据此提示具体字段；没有字段参数的其他失败仍显示通用提示。

历史运行没有 WorkflowRun 引用时，确认行仍显示 IssueRun 状态，不请求 WorkflowRun，也不显示运行详情链接；有关联的运行始终以 WorkflowRun 状态为准。

## 测试

组件测试用 MSW；属性栏和 Task Mode→Thread 经 `issue-detail-page.test.tsx` 覆盖。测试区分加载、空列表、拒绝、重试；创建、普通评论和目标提交失败保留草稿，作用域切换则清空草稿。

Workflow 测试覆盖双击互斥、确认失败后复用 interaction（含切换版本）、默认版本与切换后的版本都按该版本快照渲染、版本字段/default/回显切换、字段校验拒绝时提示具体字段、作者同名 select 不发快照表单请求、按所选版本自身的字段通过与被拒、读取等待与失败，以及 WorkflowRun 状态和详情链接。

`task-layout.tsx` 管理 1024px 可用宽度断点与独立侧栏；`execution-sidebar.tsx` 按 activeRunId 展示当前执行并链接最近历史；`pull-request-refs.tsx` 只管理已有引用，不推断外部 PR 状态。`issue-workspace.tsx` 保持同一任务输入组件跨视图挂载，草稿及重试身份不丢失。上下文与配置链接收入次级折叠区。

工作流表单也由默认任务对话复用。创建评论的重试键在表单生命周期内保持稳定，interaction 按返回的确切 commentId 查找；确认响应丢失后遇到 interaction_already_confirmed 时刷新已有运行，不能新建交互重复执行。模型配置、未发布、权限和失效目标有具体提示，表单加载失败可取消。测试覆盖这些拒绝与响应丢失恢复；页面重载通过任务运行记录定位已确认执行，未确认字段草稿不持久化。
