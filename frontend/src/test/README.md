# test：测试脚手架

[中文](README.md) | [English](README.en.md)

## 职责

所有测试共用的环境设置与替身。目标是让每个测试都能在无网络、无真实后端的 jsdom 中确定性运行，并且不在用例之间泄漏状态。

## 内容

| 文件 | 说明 |
| --- | --- |
| `setup.ts` | vitest `setupFiles`：每个用例后卸载 Testing Library 渲染的树。 |
| `http.ts` | `installFakeHttp(body, status)`：替换 `AXIOS_INSTANCE` 的 adapter，记录请求并返回固定响应；测试结束自动还原。 |
| `http.test.ts` | 验证假适配器本身的记录与错误状态语义，其它测试依赖这些行为。 |
| `issue-fixtures.ts` | `makeIssue` / `makeStatus` / `makeProject` / `makeAgentTarget`：构造真实契约形状的任务、状态列、项目及 Task Mode 目录夹具。 |
| `issue-fixtures.test.ts` | 验证夹具的默认值与 overrides 优先级，其它测试依赖这些行为。 |
| `msw-server.ts` | MSW node server：mock 域 handler 加上基线 `GET /api/v1/me → 401`（任何渲染默认未登录）与 `GET /auth/providers → ['github']`（生产形态的网关；开发者登录的测试会覆盖它）。 |
| `cloud-handlers.ts` | 云流程共享 MSW 替身：会话探测及 `/me/spaces` 返回的 `cloud-dev` 租户空间，并提供 `TEST_USER` 等 fixture。 |
| `navigation.ts` | `installFakeNavigation()`：替换外部跳转与新标签页打开，记录 `destinations` 与 `openedTabs`；测试结束自动还原。 |
| `render.tsx` | `renderWithProviders` / `renderAtRoute` / `renderRoutes`：包好 QueryClient、`SessionProvider`、Sidebar 与（前两者）`CurrentSpaceProvider` 的渲染入口；`renderAtRoute` 用应用的 `WORKSPACE_ROUTE_PATTERN`（`/w/:workspaceSlug`）从初始路径解析 slug。 |

## 依赖方向

依赖 `@/lib/api-client`（替换其 adapter）、`@/lib/navigation`（替换外部跳转）、`@/features/auth/session` 与 `vitest`。业务代码**禁止** import 本目录。

## 约定

- 替身只替换边界（HTTP adapter、外部跳转、网络 handler），不 mock 内部模块；需要新边界替身时在这里加文件并附测试。
- 需要已登录成员的测试先调用 `installSignedInSession`（或 `installCloudSpaceHandlers`）；不调用即为未登录，这是与真实浏览器无 Cookie 时一致的默认。
- 覆盖率统计排除本目录（见 `vite.config.ts`）。
