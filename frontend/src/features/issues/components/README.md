# issues/components：任务页子组件

[中文](README.md) | [English](README.en.md)

## 职责

任务看板与详情页的子组件：卡片与行、创建对话框、活动栏（时间线、评论、@ 目标、Workflow 表单配置/审阅/确认）、上下文引用栏与属性栏。数据来自 `features/issues/api`，不直接发 HTTP。

## 文件

| 文件 | 说明 |
| --- | --- |
| `issue-card.tsx` / `issue-row.tsx` | 看板卡片与列表行 |
| `create-issue-dialog.tsx` | 新建任务对话框 |
| `issue-properties-panel.tsx` | 详情页属性栏：状态、优先级、负责人、父/子任务、项目、标签、属性、上下文引用 |
| `activity-panel.tsx` | 活动栏：时间线与新建评论/目标 |
| `target-picker.tsx` / `pending-targets.tsx` | @ 目标选择与待提交目标 |
| `workflow-interaction-composer.tsx` / `dynamic-form-renderer.tsx` / `form-field-renderer.tsx` / `assist-suggestions.tsx` / `confirm-review.tsx` | Workflow 表单的渲染、AI 建议、审阅与确认 |
| `context-refs-panel.tsx` | 上下文引用的增删 |
| `*.test.tsx` | 组件测试 |

## 依赖与不变量

依赖 `features/issues/api`、`features/issues/types`、`features/issues/present`、`components`；由 `features/issues` 的页面使用。草稿（评论、目标、表单）只在用户按下各自按钮时才写入服务端；属性栏的修改都经页面传入的 `onCommit`（带版本号）。

## 测试

组件测试用 MSW；属性栏经 `issue-detail-page.test.tsx` 覆盖。
