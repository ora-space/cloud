# issues：任务（Issue）

[中文](README.md) | [English](README.en.md)

## 职责

租户任务的看板、列表与详情页，以及它们共用的 Cloud Issue 数据层（`/api/v1/tenants/:tid/issues*`）。不负责 Agent 会话 Thread（`thread/`）和 MSW 演示数据。

## 文件

| 文件 | 说明 |
| --- | --- |
| `api.ts` | Issue、状态、成员、评论、run、时间线、交互与上下文引用的 Query hooks |
| `types.ts` | 与后端契约对应的 Issue 领域类型 |
| `present.ts` | 编号、负责人、状态列等显示用纯函数 |
| `issues-page.tsx` / `issues-board.tsx` / `issues-list.tsx` | 任务页、看板（拖拽→`move` 锚点）与列表 |
| `issue-detail-page.tsx` | 详情页：描述与活动栏、属性栏（`components/issue-properties-panel.tsx`）和有 agent run 时的「Agent 会话」栏（`thread/`） |
| `*.test.ts(x)` | 页面与 hooks 测试 |

子目录：`components/`（页面子组件）、`thread/`（Agent 会话面板）。

## 依赖与不变量

依赖 `src/api`、`lib`、`components`、`features/spaces`；由 `routes.tsx` 使用。`slug` 属性实为租户 id（由 `CloudScope` 传入），导航链接用路由里的空间 slug。run 列表的 query key 固定为 `['issue-runs', tid, issueId]`，空间事件按它失效。

## 测试

MSW 模拟真实 Cloud 接口；`onUnhandledRequest: 'error'` 下每个测试都要为页面挂载的所有查询提供 handler。
