# issues/components：任务页子组件

[中文](README.md) | [English](README.en.md)

## 职责

任务看板与详情页的子组件：卡片与行、创建对话框、活动栏（时间线、评论、@ 目标、Workflow 表单配置/审阅/确认）、上下文引用栏与属性栏。数据来自 `features/issues/api`，不直接发 HTTP。

## 文件

| 文件 | 说明 |
| --- | --- |
| `issue-card.tsx` / `issue-row.tsx` | 看板卡片与列表行 |
| `create-issue-dialog.tsx` / `issue-project-select.tsx` | 新建任务及详情的真实项目选择；列表加载或失败不能清空已存关联 |
| `issue-properties-panel.tsx` | 详情页属性栏：状态、优先级、负责人、父/子任务、项目、标签、属性、上下文引用 |
| `activity-panel.tsx` | 活动栏：时间线与新建评论/目标 |
| `target-picker.tsx` / `pending-targets.tsx` | 真实 @ 目录的加载、空、搜索与重试状态，以及待提交目标 |
| `agent-request-failure.tsx` | 安全的任务拒绝文案，项目、目标及个人模型配置引导 |
| `workflow-interaction-composer.tsx` / `dynamic-form-renderer.tsx` / `form-field-renderer.tsx` / `assist-suggestions.tsx` / `confirm-review.tsx` | Workflow 表单的渲染、AI 建议、审阅与确认 |
| `context-refs-panel.tsx` | 上下文引用的增删 |
| `*.test.tsx` | 组件测试 |

## 依赖与不变量

依赖 `features/issues/api`、`features/issues/types`、`features/issues/present`、`features/projects/api` 的真实项目列表、`features/spaces/current-space` 与 `components`；由 `features/issues` 的页面使用。草稿（评论、目标、表单）只在用户按下各自按钮时才写入服务端；属性栏的修改都经页面传入的 `onCommit`（带版本号）。项目选择来自当前已加入空间，不能回退到演示数据。

创建草稿按 tenant/space 重挂，活动草稿按 tenant/space/issue 重挂；切到已缓存的其他作用域也不能继承草稿。关联项目以服务端结果为准，版本化更新等待期间禁止再改属性，拒绝保留原值。缺项目提示定位到 `issue-project` 选择器；目标必须来自真实目录，空间合法性仍由服务端校验。

Workflow 确认从创建 interaction 开始锁住重复提交、返回修改与取消；确认失败后仍用同一 interaction 重试，包括切换版本后的重试。确认行通过 `features/workflows/api` 读取 `IssueRun.workflowInvocationRef` 对应的 WorkflowRun 状态，详情链接使用 `executorId`、该引用和当前路由的空间 slug。

初始表单按 `formRef + issueId` 读取活稿。只有 `snapshotId` select 的全部选项都属于该 Workflow 的已发布快照时，才识别为平台版本选择器；列表读取完成前不开放表单，失败可重试。作者自定义的同名文本字段或含自定义选项的 select 仍是普通输入。

用户切换平台版本选择器时，按 tenant/workflow/snapshot/issue 读取既有 snapshot form 接口，清空旧字段草稿并初始化新版本默认值；选择器保留响应回显的 snapshotId，不采用它仍指向最新版本的默认值。加载或失败期间不能审阅旧字段，失败可重试。读取表单不代表服务端准入通过，Confirm 的 `workflow_version_fields_changed` 门禁保持不变；收到该拒绝时，界面提示所选版本字段与当前草稿不一致，并引导先发布草稿或选择其他版本。

历史运行没有 WorkflowRun 引用时，确认行仍显示 IssueRun 状态，不请求 WorkflowRun，也不显示运行详情链接；有关联的运行始终以 WorkflowRun 状态为准。

## 测试

组件测试用 MSW；属性栏和 Task Mode→Thread 经 `issue-detail-page.test.tsx` 覆盖。测试区分加载、空列表、拒绝、重试；创建、普通评论和目标提交失败保留草稿，作用域切换则清空草稿。

Workflow 测试覆盖双击互斥、确认失败后复用 interaction（含切换版本）、版本字段/default/回显切换、作者同名 select 不发快照表单请求、字段差异拒绝的操作提示、读取等待与失败，以及 WorkflowRun 状态和详情链接。
