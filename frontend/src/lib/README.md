# lib：与 React 无关的基础设施

[中文](README.md) | [English](README.en.md)

## 职责

被生成代码和组件共用、但本身不含 React 与业务概念的工具。这里的每个文件都应能在 Node 中独立测试。

## 内容

| 文件 | 说明 |
| --- | --- |
| `api-client.ts` | 共享 axios 实例 `AXIOS_INSTANCE` 与 orval mutator `customInstance`。为写请求补充幂等键（重复提交时识别同一次操作的标识），并统一处理 react-query 的 `AbortSignal` 与 orval 的 `cancel()` 两种取消来源。浏览器不注入认证头。 |
| `api-client.test.ts` | 验证响应体解包、写请求幂等键，以及两条取消路径都会中止请求并以 `CanceledError` 拒绝。 |
| `mock-api-client.ts` | 仅把仍处于模拟阶段的业务请求改写到 `/mock-api`，并把真实空间路径映射到演示数据；认证与 `/api/v1/me` 永远走真实 Gateway。 |
| `utils.ts` | 重新导出 `cn`（Tailwind 感知的类名合并），shadcn 组件通过 `@/lib/utils` 引用。 |

## 依赖方向

只依赖第三方库。**禁止** import `react`、`@/components`、`@/api`（`@/api` 反向依赖这里，否则成环）。

## 不变量

- `customInstance` 的第一个参数类型必须接受 orval 生成的 `signal: AbortSignal | undefined`（`exactOptionalPropertyTypes` 下的显式 `undefined`）。改签名前先跑 `npm run typecheck` 看生成代码是否还能编译。
- 写请求的幂等键在共享客户端集中补齐，但调用方显式提供的值不能被覆盖。
- 不读取 localStorage、Zustand 或模拟 token；Cloud 认证事实只来自 HttpOnly session Cookie 与 `/api/v1/me`。
