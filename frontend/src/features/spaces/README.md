# spaces：云协作空间接入层

## 职责

把生成的 orval 客户端包装为页面可用的领域 hooks，并维护"当前空间"上下文与 SSE 订阅。它负责：

- Gateway 会话通过布局验证后，从 `GET /api/v1/me/tenants` 解析租户，再加载 `GET /spaces` 空间列表；
- 把路由的 `:workspaceSlug` 解析为真实空间（未加入的 slug 解析为空）；
- 空间成员 / 空间内项目的查询与变更（创建/改名/归档/成员 upsert），成功后失效对应查询；
- 订阅 `/spaces/:sid/events` SSE 流，事件到达时只做 query invalidation（事件不携带业务状态）。

不负责：页面 UI、路由定义、身份签发（由 Gateway 承担）、mock 数据。

## 文件

| 文件 | 说明 |
|---|---|
| `api.ts` | 领域 hooks（useSpaces / useSpaceMembers / useSpaceProjects / useCreateSpace / useUpdateSpace / useArchiveSpace / useUpdateSpaceMember） |
| `current-space.tsx` | `CurrentSpaceProvider` + `useCurrentSpace`：在已认证 Cloud 空间列表中解析当前空间 |
| `use-space-events.ts` | `parseSSEFrames`（纯函数）+ `useSpaceEvents`（fetch 流订阅与失效） |
| `create-space-dialog.tsx` | 新建工作区对话框：slug 自动小写、创建后回调 slug 供跳转 |
| `spaces.test.tsx` | 上述行为的测试 |

## 依赖与使用

依赖：`src/api`（生成客户端）与 TanStack Query。浏览器凭据由同源 Gateway 的 HttpOnly Cookie 承载，本模块不可读取。

可被依赖：页面与布局组件（`projects`、`members`、`settings`、`dashboard-layout`、`app-sidebar`）。

## 不变量

- 租户级查询全部以 `tenantId` 为开关（`enabled: !!tenantId`），无租户时绝不发请求；
- 不读取或拼装认证 token；普通请求与 SSE 都只依赖浏览器自动携带的同源 HttpOnly Cookie；
- SSE 事件仅触发失效，任何状态都以 REST 重新拉取为准；
- `parseSSEFrames` 是纯函数，坏帧跳过不抛异常。

## 测试

`spaces.test.tsx` 用 MSW 动态 handler 模拟真实 API，并验证无需 JavaScript token 即可解析租户和空间。测试不依赖网络。
