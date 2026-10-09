# spaces：协作空间接入层

## 职责

包装生成的 Cloud 客户端，加载用户在所有租户中的空间，按路由 slug 选中空间并提供对应 `tenantId`。一个协作空间就是一个租户的可见信息；创建空间调用 `POST /api/v1/tenants`。维护空间名称更新、空间内项目列表和 SSE 失效通知。

不负责成员关系（`features/members`）、加入流程（`features/onboarding`）、会话认证或项目业务内容。

## 文件

| 文件 | 说明 |
| --- | --- |
| `api.ts` | 全量分页的已加入空间列表、租户创建、空间改名和空间项目 hooks |
| `current-space.tsx` | 从路由 slug 推导当前空间和该空间的租户 ID |
| `slug.ts` | 与服务端一致的 slug 校验与名称派生 |
| `create-space-dialog.tsx` | 创建新租户及其唯一空间的对话框 |
| `use-space-events.ts` | SSE 解析、重连与 Query 缓存失效（含 Issue run Thread 事件） |
| `*.test.tsx` | 列表、创建、切换和事件行为测试 |

## 依赖与不变量

依赖生成客户端、`features/auth/session`、TanStack Query。布局、onboarding、项目和设置页依赖本模块。

- 只在确认登录后请求 `/me/spaces`；必须读取所有分页，切换器不能漏掉后续租户。
- `tenantId` 必须来自当前路由匹配的空间；未知 slug 不借用第一个租户权限。
- 角色只有 `admin`、`member`，未知角色按普通成员处理。
- SSE 事件只触发重新读取权威 REST 状态；断线后按有上限的退避重连。若重连返回 403/404，则刷新已加入空间列表并停止订阅，以便界面退出已失去权限的空间。

## 测试

MSW 模拟生成客户端的网络边界；纯函数单测覆盖 slug、SSE 帧与重连间隔。

创建表单复用 `CreateFormSubmit` 统一错误与等待反馈；SSE 同时支持租户成员撤权、插件选择和目录更新，刷新相应查询。`issue_run.thread_appended` / `issue_run.thread_changed` 失效该 run 的 Thread 查询（生成客户端的 `getGetApiV1TenantsTidIssuesIidRunsRidThreadQueryKey`，不带参数）和 `['issue-runs', tid, issueId]`；`lastSeq` 只是提示，游标只由 Thread GET 推进。缺少 `issueId` 的 Thread 事件被忽略，`issueId`/`runId` 不是字符串的事件在边界处丢弃。
