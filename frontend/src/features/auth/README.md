# auth：会话与登录边界

[中文](README.md) | [English](README.en.md)

## 职责

前端唯一知道"用户是谁、是否登录"的模块。它负责：

- 通过 Gateway 完成登录与登出（`GET /auth/providers` 询问有哪些登录方式，`POST /auth/login` → 跳转 provider；`POST /auth/logout`），这是 OpenAPI 之外唯一手写的 HTTP 面；
- 用 `GET /api/v1/me` 探测会话并把结果暴露为 `Session`（`loading` / `signed-out` / `disabled` / `unavailable` / `signed-in`；403 是 `disabled`：Gateway 会话有效但 Cloud 拒绝该用户，重新登录也无济于事）；
- 401 策略：任何请求得到 401 即结束会话，页面据此跳转登录而不是逐个查询失败；
- 路由门禁 `RequireSession` 与登录页 `LoginPage`。

不负责：租户与空间解析（`features/spaces`）、任何 token 的持有——会话是 Gateway 的 HttpOnly Cookie，本模块也读不到它。

## 文件

| 文件 | 说明 |
| --- | --- |
| `api.ts` | `fetchLoginProviders`（Gateway 的 provider 列表，过滤为 `huawei-idaas` / `github` / `dev`）、`isExternalProvider`、`startLogin`（拿 `authorizationUrl` 后 `navigateExternal`）、`logoutSession`、`GITHUB_SIGN_OUT_URL`（GitHub 自己的注销页）、`fetchSessionUser`（401 → 未登录，403 → 已停用，其它错误抛出） |
| `providers.ts` | `useLoginProviders`：provider 列表的 query，整个 tab 内缓存；登录页与侧栏共用 |
| `session.tsx` | `SessionProvider`（会话查询 + 订阅 `onUnauthorized`）、`useSession` 提供 `signOut` 与 `signOutOfGitHub`（先退出，再在新标签页打开 GitHub 注销页，当前页留在 Ora）、`Session` 类型、`SESSION_QUERY_KEY` |
| `require-session.tsx` | `RequireSession`：加载中不渲染；未登录跳登录页，私有加入链接先存当前标签、网关只接收无令牌的返回路径；账号停用与后端不可达就地提示 |
| `login-page.tsx` | 只有一个 provider 且是外部 provider 时（生产形态，如 `huawei-idaas`）自动发起一次登录，失败后只显示重试；否则 Gateway 提供几种登录方式就显示几个按钮，存在 GitHub 登录时再加一条"先退出 GitHub"链接（否则 GitHub 会复用浏览器当前账号）；账号已停用时就地提示，不再发起登录："使用 GitHub 登录"，以及本地 Gateway 开启 `login.development_provider` 时的"开发者登录"（Gateway 自己的表单，输入任意身份即可登录）；`?returnTo=` 经 `safeReturnTo` 收窄；已登录直接跳转 |
| `auth.test.tsx` | 上述全部行为的测试 |

## 依赖方向

依赖：`src/api`（`getApiV1Me`）、`src/lib/api-client`（`customInstance`、`onUnauthorized`、`isUnauthorizedError`）、`src/lib/navigation`、`src/lib/paths`、TanStack Query、react-router。

可被依赖：`main.tsx`（挂载 `SessionProvider`）、`routes.tsx`、布局组件、`features/spaces`（用 `useSession` 门控查询）、任何需要显示当前用户的页面。

## 不变量

- `SessionProvider` 在应用里只挂载一次，位于 router 之外、QueryClient 之内。
- `fetchSessionUser` 只把 401 当作"未登录"、只把 403 当作"已停用"；网络错误或 5xx 是 `unavailable`，`RequireSession` 不会把它误判为未登录而丢掉 `returnTo`。
- 私有邀请和申请链接里的令牌不写入 Gateway 的 `returnTo`；外部登录往返只保存 `/join/continue`，原链接暂存在当前浏览器标签。
- 自动登录每次挂载最多发起一次：失败后显示显式重试，绝不形成跳转循环。
- `signOut` 先调 Gateway 再清缓存：会话置空，其余查询全部移除，避免下一个登录者看到上一个人的数据。
- 已登录会话收到 401 时，同样清除之前用户的查询缓存，包括个人模型连接；首次匿名探测的 401 保留登录 provider 的公开查询。
- Ora 无法结束 github.com 的会话：Gateway 从不持有 GitHub token（读完资料立刻丢弃），所以"退出 GitHub"只能打开 GitHub 自己的注销页，且总是先吊销 Ora 会话，并在新标签页打开，让成员在当前页直接回到登录界面。该 URL 是公网 github.com；GitHub Enterprise Server 部署需要把它做成可配置。
- 登录页从不构造 provider URL，也不解析 callback；那是 Gateway 的事。它也从不自行判断开发者登录是否存在：只有 `/auth/providers` 列出 `dev` 时才显示按钮，而 Gateway 只在 loopback 开发 origin 上允许该 provider。

## 测试

`auth.test.tsx` 用 MSW 覆盖 `/auth/*` 与 `/api/v1/me`，用 `installFakeNavigation` 观察跳转目标。基线 MSW 服务器把 `/api/v1/me` 答成 401，未登录场景不需要额外 handler。
