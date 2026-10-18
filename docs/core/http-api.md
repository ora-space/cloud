# HTTP API 约定

[English](http-api.en.md) | 中文

Cloud server 对外只有一个 HTTP 面：浏览器经 [Gateway](../identity/gateway.md) 转发到 `/api/v1/*`。
本文说明这个 HTTP 面的公共约定：路由、凭据、请求解码、错误、分页、SSE，以及契约文件如何生成。
每个接口的字段、状态码和响应结构以 [`api/openapi.json`](../../api/openapi.json) 为准，本文不逐个列出。

Controller 使用的内部契约是 gRPC，见 [Controller 内部控制契约](../runtime/controller-integration.md)。

## 路由白名单

`internal/api/router` 的 `Routes()` 是显式白名单：每条路由声明方法、路径和允许出现的请求体字段。
不在白名单里的路径返回 `404 not_found`。同一份白名单也驱动 OpenAPI 覆盖测试，保证文档和实现一致。

路由分两类：

| 类别 | 路径 | 处理 |
|---|---|---|
| 公开接口 | `/api/v1/...` | 校验双凭据后进入 `Store.Public` |
| 已退役的内部 JSON 接口 | `/internal/v1/...` | 生产 server 一律返回 `410 retired_management_transport`；只有进程内模拟器用 `router.NewDevelopment` 打开它们 |

另外有两条不在白名单里的路由：`GET /healthz`（检查数据库连通性，报告对象存储是否已配置，不泄露
配置内容）和 SSE 事件流 `GET /api/v1/tenants/:tid/spaces/:spaceId/events`。

## 凭据

每个公开请求都必须带两份独立签名的内部凭据，由 Gateway 为每个请求现签（见 [内部凭据与服务信任](../identity/authentication.md)）：

| 请求头 | 内容 | 要求 |
|---|---|---|
| `Authorization: Bearer <service JWT>` | 调用方服务的身份 | `kind=service`，公开接口要求 `role=gateway` |
| `X-Ora-User-Token: <user JWT>` | 最终用户的身份 | `kind=user`，且 `caller` 必须等于 service JWT 的 `sub` |

任一凭据缺失或无效分别返回 `401 invalid_service_credential`、`401 invalid_user_credential`；服务角色
不对返回 `403 service_forbidden`。凭据只证明"谁在调用"；授权一律由 core 根据当前 PostgreSQL 状态判定
（租户成员、角色、资源归属），不接受请求头或请求体里自称的身份和作用域。

## 请求解码

非 `GET` 请求的请求体在进入 core 之前严格校验：

- 请求体上限 64 KiB；追加 Thread 消息的路由放宽到 256 KiB，使超长消息由契约返回
  `400 content_too_large`，而不是被截断成 `invalid_json`。
- 必须是一个 JSON 对象，后面不能再跟其他 JSON 值；`null` 也不接受。数字按 `json.Number` 解码，不丢精度。
- 只允许路由声明的字段，其他字段返回 `400 unknown_field`（`params.field` 指出字段名）。服务端拥有的身份和
  作用域字段因此无法从请求体注入。
- 字段类型按名称固定：`version`、`position` 等是整数，`ids` 是字符串数组，`filter`、`input`、`graph`
  等是对象，`targets`、`content`、`models` 等有固定的元素形状，其余字段默认是字符串。类型不符返回
  `400 invalid_field_type`。
- 少数布尔字段（例如强停的 `impactConfirmed`）必须显式出现，缺失返回 `400 missing_field`。

查询参数同样受约束。分页参数 `limit` 只能是 1 到 100 的整数（Thread 读取上限是 500），会话类只读接口
只接受文档列出的单值参数，未知、重复或空的参数返回 `400`。

有两条路由在读取请求体之前就拒绝：个人模型连接的 credential 写入和清除只属于
[model-gateway](../models/model-gateway.md)，即使有人绕过 Gateway 直接请求 Cloud，明文密钥也进不了 Cloud 进程。

## 错误

所有错误使用同一个外形：

```json
{"code": "version_conflict", "params": {}, "requestId": "8c1f…"}
```

- `code` 是稳定的机器可读错误码，客户端据此本地化提示；`params` 只放安全的标识和枚举值（例如出错的
  字段名），不放自然语言，也不放调用方本无权读取的内容。
- 每个响应都带 `X-Request-Id` 头，与错误体里的 `requestId` 一致，用于在服务端日志里定位。
- 内部细节（SQL、堆栈、凭据、基础设施信息）不进入响应。core 之外的未预期错误统一映射为
  `500 internal_error`，详情只写结构化日志。

常见状态码的含义：

| 状态 | 典型错误码 | 含义 |
|---|---|---|
| 400 | `invalid_json`、`unknown_field`、`invalid_input`、`idempotency_key_required` | 请求本身不合法 |
| 401 | `invalid_service_credential`、`invalid_user_credential` | 凭据缺失或无效 |
| 403 | `membership_required`、`admin_required`、`user_disabled`、`runtime_use_forbidden` | 身份有效但无权执行 |
| 404 | `not_found` | 资源不存在，或调用方无权知道它存在 |
| 409 | `version_conflict`、`idempotency_conflict`、`operation_in_progress` | 与当前状态冲突 |
| 410 | `retired_management_transport` | 已退役的入口 |
| 428 | `version_required` | 缺少必需的资源版本 |
| 503 | `database_unavailable`、`directory_unavailable` | 依赖暂时不可用 |

对不可见资源返回 `404` 而不是 `403`，是为了不把"存在但无权访问"泄露给非成员。

## 一致性约定

这几条约定在 core 里实现，见 [权威持久化与事务模型](persistence.md#一致性手段)：

- **幂等**：`POST` 与 `DELETE` 必须带 `Idempotency-Key`（不超过 200 字节）。同 key 同内容重放原响应，
  同 key 不同内容返回 `409 idempotency_conflict`。
- **资源版本**：修改类请求带当前 `version`，缺失返回 `428`，不一致返回 `409`。
- **异步接受**：创建 Project、Workspace 生命周期动作、重试操作、强停等请求返回 `202`，响应里带持久
  操作；客户端通过读取资源或操作跟踪进度，不等待外部执行完成。

## 分页

列表接口返回：

```json
{"items": [...], "nextCursor": "<uuid 或空串>"}
```

- 默认按资源 UUID 升序；`after` 是排他游标（上一页的 `nextCursor`），`limit` 默认 50、最大 100。
  `nextCursor` 为空串表示没有下一页。
- 身份和作用域过滤在分页之前完成，所以游标不会越过调用方看不到的行。
- 少数接口有自己的顺序，以 OpenAPI 为准：`GET /api/v1/me/tenants` 按租户创建时间升序；会话列表按
  `(createdAt, id)` 倒序；Thread 使用整数 `seq` 游标。

## SSE 失效事件流

`GET /api/v1/tenants/:tid/spaces/:spaceId/events` 是 Space 级的服务器推送事件流
（`text/event-stream`）。它只发**失效通知**，从不携带业务状态；收到事件后，客户端通过 REST 重新读取
相关资源。REST 是权威，SSE 只是"该刷新了"的提示。

- **授权与 REST 相同**：打开时按 `GET /spaces/:spaceId` 的路径校验凭据和成员身份。每推送一条事件之前
  都会**重新**校验凭据和成员身份，成员被停用后，连"成员已变化"这条事件本身也不会发给他。
- **连接会过期**：重新校验使用的是建立连接时的请求头，而用户凭据的有效期不超过 5 分钟。凭据过期后到达
  的第一条事件会使服务端结束连接，客户端需要重新连接。
- **不保证送达**：事件在事务提交后才发出，至多一次；没有重放，慢订阅者会丢事件，只有连到同一个 Cloud
  进程的订阅者能收到。客户端不能依赖事件做状态推导，重连后应主动刷新。

事件形如 `data: {"type":"project.created","spaceId":"…","projectId":"…"}`，类型包括：

| 类型 | 触发 |
|---|---|
| `space.updated`、`space.member_updated` | Space 资料、成员变化 |
| `project.created`、`project.updated`、`project.archived` | Project 创建、改名、进入删除 |
| `space.plugins_updated`、`plugins.catalog_updated` | 空间插件选择或安装进度变化；插件目录刷新（发给所有订阅者） |
| `issue_run.thread_appended`、`issue_run.thread_changed` | Thread 有新内容或 REST 表示变化，`lastSeq` 只是高水位提示，不是游标 |

## 契约文件的生成链

公开 HTTP 契约只有一个来源，四处必须同步：

```text
internal/api/router  Routes()         路由白名单与请求字段
internal/contract    Document()       OpenAPI 3.0 模型（Go 代码）
        │  task openapi（go run ./cmd/openapi）
        ▼
api/openapi.json                      生成物，不手改
        │  task frontend:generate（orval）
        ▼
frontend/src/api                      前端类型化客户端，生成物，不手改
```

- 改路由、字段、状态码或响应时，修改 Go 源码后运行 `task frontend:generate`（它会先运行 `task openapi`），
  把两份生成物和源码放在同一个提交里。
- `TestPublishedOpenAPIIsValidAndCurrent` 要求提交的 `api/openapi.json` 与 `Document()` 逐字节一致并通过
  结构校验；集成测试用同一份文档校验真实 HTTP 响应，任何未声明的响应字段都会失败。CI 的前端任务检查
  生成客户端有无漂移。
- OpenAPI 文档要通过严格校验器：对象 schema 的 `required` 只在非空时出现（`TestRequiredIsOmittedWhenEmpty`）。

## 相关决策

- [Space API 与 Space 级失效事件流：REST 为权威，SSE 只做失效通知](https://github.com/ora-space/specs/blob/main/decisions/cloud/api-boundary/0-space-api-and-event-stream.md)
- [外部身份由 Gateway 归一，Cloud 保持授权权威](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/0-gateway-mediated-external-identity.md)
