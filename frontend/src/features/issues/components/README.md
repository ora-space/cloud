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

## 测试

组件测试用 MSW；属性栏和 Task Mode→Thread 经 `issue-detail-page.test.tsx` 覆盖。测试区分加载、空列表、拒绝、重试；创建、普通评论和目标提交失败保留草稿，作用域切换则清空草稿。
