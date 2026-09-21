# test：测试脚手架

[中文](README.md) | [English](README.en.md)

## 职责

所有测试共用的环境设置与替身。目标是让每个测试都能在无网络、无真实后端的 jsdom 中确定性运行，并且不在用例之间泄漏状态。

## 内容

| 文件 | 说明 |
| --- | --- |
| `setup.ts` | vitest `setupFiles`：每个用例后卸载 Testing Library 渲染的树。 |
| `http.ts` | `installFakeHttp(body, status)`：替换 `AXIOS_INSTANCE` 的 adapter，记录请求方法、地址、取消信号、幂等键和敏感认证头是否存在，并返回固定响应；测试结束自动还原。 |
| `http.test.ts` | 验证假适配器本身的记录与错误状态语义，其它测试依赖这些行为。 |
| `cloud-handlers.ts` | 云空间流程的共享 MSW 替身：测试租户与 `cloud-dev` 空间（`installCloudSpaceHandlers`）；认证 Cookie 不在 JavaScript 测试数据中模拟。 |
| `render.tsx` | `renderWithProviders` / `renderAtRoute`：包好 QueryClient、Sidebar 与 `CurrentSpaceProvider` 的渲染入口；需测试真实 Cloud 空间时显式传 `authenticated: true`。 |

## 依赖方向

依赖 `@/lib/api-client`（替换其 adapter）与 `vitest`。业务代码**禁止** import 本目录。

## 约定

- 替身只替换边界（HTTP adapter），不 mock 内部模块；需要新边界替身时在这里加文件并附测试。
- 覆盖率统计排除本目录（见 `vite.config.ts`）。
