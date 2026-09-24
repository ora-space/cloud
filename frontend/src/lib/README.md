# lib：与 React 无关的基础设施

[中文](README.md) | [English](README.en.md)

## 职责

被生成代码和组件共用、但本身不含 React 与业务概念的工具。这里的每个文件都应能在 Node 中独立测试。

## 内容

| 文件 | 说明 |
| --- | --- |
| `api-client.ts` | 共享 axios 实例 `AXIOS_INSTANCE` 与 orval mutator `customInstance`。跨切面 HTTP 策略唯一的落点；同时处理 react-query 的 `AbortSignal` 与 orval 的 `cancel()` 两种取消来源。请求拦截器为 POST/DELETE 补发 `Idempotency-Key`；响应拦截器把任何 401 广播给 `onUnauthorized` 的订阅者（会话拥有者据此结束会话）。认证不是 header：浏览器只持有 Gateway 的 HttpOnly 会话 Cookie，同源请求自动携带，前端从不接触 token；`faultCode` 从被拒绝的请求中取出后端 `Fault.code`（如 `not_found`、`capability_unavailable`），供各 feature 模块把错误映射到 UX。 |
| `api-client.test.ts` | 验证响应体解包、两条取消路径、幂等键策略、不附加任何凭证头、401 监听器的触发与移除，以及 `faultCode` 的错误码提取。 |
| `navigation.ts` | 与其它 origin 接触的唯一出口：`navigateExternal`（登录跳转到 provider）与 `openExternalTab`（在新的 `noopener` 标签页打开 provider 页面，当前页保留）；`replaceExternalNavigation` / `replaceExternalTabOpener` 供测试脚手架替换，因为 jsdom 不允许 spy `location.assign` 也没有 `window.open`。 |
| `navigation.test.ts` | 验证替换与还原语义。 |
| `paths.test.ts` | 验证 `safeReturnTo` 的拒绝规则、`loginPath` 的编码与所有工作区路由的 `/w/` 前缀。 |
| `paths.ts` | 保留的工作区前缀 `WORKSPACE_ROUTE_PREFIX`（`/w`）及其路由模式 `WORKSPACE_ROUTE_PATTERN`、工作区路由构造 `workspacePaths`（问题、项目、小队、智能体、技能、插件、运行时、设置等全部工作区路由）、登录路由 `loginPath`、不受信 `returnTo` 的收窄 `safeReturnTo`（单个前导 `/`、不允许 `//`、反斜杠或控制字符、最长 2048）、以及 `workspaceUrlPrefix`（`host/w/`，显示在 slug 输入框前的固定部分）。 |
| `mock-api-client.ts` | MSW mock 域（`/mock-api/*`）的 axios 客户端，与真实后端生成客户端分离。把真实 space slug 重写为 demo 种子 workspace，使尚无后端的页面在任意 Space 下继续显示演示数据，直到它们接入真实 API。mock 域没有认证。 |
| `utils.ts` | 重新导出 `cn`（Tailwind 感知的类名合并），shadcn 组件通过 `@/lib/utils` 引用。 |

## 依赖方向

只依赖第三方库与本目录内的兄弟模块。**禁止** import `react`、`@/components`、`@/api`（`@/api` 反向依赖这里，否则成环）。

## 不变量

- `customInstance` 的第一个参数类型必须接受 orval 生成的 `signal: AbortSignal | undefined`（`exactOptionalPropertyTypes` 下的显式 `undefined`）。改签名前先跑 `npm run typecheck` 看生成代码是否还能编译。
- 请求拦截器必须保持同步（`synchronous: true`），否则 axios 在拦截器完成前不会把请求交给 adapter，`AbortSignal` 可能输掉竞态。
- 本目录任何文件都不得持有、读取或生成凭证：会话是 Gateway 的 HttpOnly Cookie，前端代码无法也不应触及。
- `safeReturnTo` 只接受单个 `/` 开头且第二个字符不是 `/` 或 `\` 的路径，与 Gateway 的 `returnTo` 规则一致。
