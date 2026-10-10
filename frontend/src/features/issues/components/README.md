# issues/components：任务页子组件

[中文](README.md) | [English](README.en.md)

## 职责

任务看板与详情页的子组件：卡片与行、创建对话框、活动栏（时间线、评论、@ 目标、Workflow 表单配置/审阅/确认）、上下文引用栏与属性栏。数据来自 `features/issues/api`，不直接发 HTTP。

## 文件

| 文件 | 说明 |
| --- | --- |
| `issue-card.tsx` / `issue-row.tsx` | 看板卡片与列表行 |
| `create-issue-dialog.tsx` / `issue-project-select.tsx` | 新建任务对话框与当前真实协作空间的可选项目选择，保留「未关联项目」 |
| `issue-properties-panel.tsx` | 详情页属性栏：状态、优先级、负责人、父/子任务、项目、标签、属性、上下文引用 |
| `activity-panel.tsx` | 活动栏：时间线与新建评论/目标 |
| `target-picker.tsx` / `pending-targets.tsx` | @ 目标选择与待提交目标 |
| `agent-request-failure.tsx` | 安全的任务拒绝文案，缺少个人模型配置时链接至设置页 |
| `workflow-interaction-composer.tsx` / `dynamic-form-renderer.tsx` / `form-field-renderer.tsx` / `assist-suggestions.tsx` / `confirm-review.tsx` | Workflow 表单的渲染、AI 建议、审阅与确认 |
| `context-refs-panel.tsx` | 上下文引用的增删 |
| `*.test.tsx` | 组件测试 |

## 依赖与不变量

依赖 `features/issues/api`、`features/issues/types`、`features/issues/present`、`features/projects/api` 的真实项目列表、`features/spaces/current-space` 与 `components`；由 `features/issues` 的页面使用。草稿（评论、目标、表单）只在用户按下各自按钮时才写入服务端；属性栏的修改都经页面传入的 `onCommit`（带版本号）。项目选择来自当前已加入空间，不能回退到演示数据。

## 测试

组件测试用 MSW；属性栏经 `issue-detail-page.test.tsx` 覆盖。目标提交拒绝保留草稿并重新启用按钮，缺少默认模型时提供当前工作区的个人设置链接。
