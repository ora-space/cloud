# issues：任务（Issue）

[中文](README.md) | [English](README.en.md)

## 职责

租户任务的看板、列表与详情页，以及它们共用的 Cloud Issue 数据层（`/api/v1/tenants/:tid/issues*`）。不负责 Agent 会话 Thread（`thread/`）和 MSW 演示数据。

## 文件

| 文件 | 说明 |
| --- | --- |
| `api.ts` | Issue、状态、成员、评论、run、时间线、交互与上下文引用的 Query hooks；新建任务可携带 `projectRef`；`useRuns` 在仍有未结束的 agent 运行时每 5 秒刷新（运行结算没有空间事件） |
| `types.ts` | 与后端契约对应的 Issue 领域类型；运行准备阶段直接引用生成的 `RunPreparation`，不重复维护契约 |
| `present.ts` | 编号、负责人、状态列等显示用纯函数 |
| `issues-page.tsx` / `issues-board.tsx` / `issues-list.tsx` | 任务页、看板（拖拽→`move` 锚点）与列表 |
| `issue-detail-page.tsx` | 详情页：活动、版本化 Project 关联与失败重读、真实 Agent 入口及现有 Thread；为发起者摘要传入成员名称 |
| `*.test.ts(x)` | 页面与 hooks 测试 |

子目录：`components/`（页面子组件）、`thread/`（Agent 会话面板）。

## 依赖与不变量

依赖 `src/api`、`lib`、`components`、`features/spaces`；由 `routes.tsx` 使用。`slug` 属性实为租户 id（由 `CloudScope` 传入），导航链接用路由里的空间 slug。run 列表的 query key 固定为 `['issue-runs', tid, issueId]`，空间事件按它失效。

## 测试

MSW 模拟真实 Cloud 接口；`onUnhandledRequest: 'error'` 下每个测试都要为页面挂载的所有查询提供 handler。新任务测试通过粘贴填写标题，验证真实 API 创建与列表刷新。

恢复重读与属性保存互斥：重读期间锁住属性和重读按钮，处理原保存错误后才解锁；失败重读保留错误，后续保存仍以读取到的版本提交并独立显示失败。

详情测试经过 SessionProvider 和 CurrentSpaceProvider，验证关联/取消关联的版本号、冲突后保留及 GET 重读、延迟 GET/PUT 的互斥与错误可见性，以及普通评论不创建 Run、Task Mode 创建的同 Run Thread 可见。组件证据不替代真实供应商多轮、终态和资源释放的联合验收。

`IssueRun.workflowInvocationRef` 保留服务端返回的 WorkflowRun 引用，供活动栏读取真实运行状态并导航；API 测试检查列表没有丢失该关联。Workflow 确认去重、失败重试和详情链接的交互测试位于 `components/`。

`useWorkflowSnapshotForm` 复用生成的 snapshot form GET，查询键包含 tenant/workflow/snapshot/issue；API 测试逐项切换这些范围，避免不同版本或 Issue 共用表单默认值。

`useWorkflowPublishedSnapshotIds` 按 tenant/workflow 缓存全部已发布版本 id，使用生成的列表接口逐页读取（每页 100，传递取消信号），不缓存图副本。活动表单用它判断下拉选项是否都属于已发布版本；API 测试验证第二页游标、完整 id 列表及租户/Workflow 隔离。
