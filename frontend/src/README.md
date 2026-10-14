# src：应用源码根

[中文](README.md) | [English](README.en.md)

## 职责

浏览器入口与组合根。这里只做"把各部分接起来"：挂载 React、装配全局 Provider、渲染根页面。业务逻辑、HTTP 细节、样式原语都不放在这一层。

## 内容

| 文件 | 说明 |
| --- | --- |
| `main.tsx` | 浏览器入口：启动 MSW（仅 `/mock-api/*`）、创建 `QueryClient`、挂载 `SessionProvider` 与路由。无导出，不做单元测试。 |
| `routes.tsx` | 路由表：`/login`、受 `RequireSession` 保护的 `/onboarding` 与保留前缀下的 `/w/:workspaceSlug/*`（工作区 slug 不会与顶层路由冲突）；`/` 落到 `/onboarding`。 |
| `index.css` | Tailwind 入口与设计令牌（颜色、圆角）。只放全局主题变量，组件样式写在组件里。 |

## 子模块

| 目录 | 说明 |
| --- | --- |
| `api/` | **生成物**（orval），禁止手改，见 [`../README.md`](../README.md)。 |
| `components/ui/` | 无业务语义的展示原语（shadcn/ui）。 |
| `features/auth/` | 会话与登录边界：Gateway 登录/登出、`/api/v1/me` 探测、401 策略、路由门禁。 |
| `features/clones/` | 「仓库」页：经公开 clones API 提交 clone、跟踪 Node 回报的结果，并保证待确认请求不会被重复提交。 |
| `features/onboarding/` | 首个工作区创建页。 |
| `features/model-connections/` | 当前用户私有的模型连接、写入凭据与新会话默认模型；路由为 `settings/model-connections`。 |
| `features/spaces/` | 协作空间接入层：租户/空间解析、空间 API、SSE 订阅。 |
| `lib/` | 与 React 无关的基础设施：HTTP 客户端、外部跳转、路径。 |
| `mocks/` | MSW 演示数据：尚未接入后端的页面的 `/mock-api/*` handler 与种子。 |
| `test/` | 测试脚手架：jsdom 清理、假 HTTP 适配器、会话与空间 fixture。 |

## 依赖方向

`main.tsx → routes.tsx → (features, components)`；`features → (api, lib)`；`api → lib`。下层永远不 import 上层。

## 不变量

- `main.tsx` 是唯一有副作用的顶层模块（挂载 DOM）。
- 所有真实后端 HTTP 只经过 `lib/api-client.ts` 的 `AXIOS_INSTANCE`；认证是 Gateway 的 HttpOnly Cookie，前端不持有 token。
