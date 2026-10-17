# internal/api/router: HTTP 路由与传输适配器

[中文](README.md) | [English](README.en.md)

`internal/api/router` 建立 Ora Cloud 的 HTTP 表现层边界。它基于 Gin 构建，负责绑定 HTTP 路由、验证两层 JWT 身份、强制执行严格的请求体解析限制、规范化错误，并将请求分发到 `internal/core`。

## 职责

### 路由白名单与分发
- `Routes()` 显式声明系统支持的端点白名单：
  - **公开 API (`/api/v1/...`)**：涵盖用户、租户、成员关系、协作空间及其成员、项目、工作区、操作、状态查询、Issue 协作（Issue、状态、标签、视图、评论、订阅、运行、上下文引用、交互与协作目标），以及工作区插件（`/tenants/:tid/spaces/:spaceId/plugins`：目录快照、选择列表、安装、移除）、Agent 会话列表与完整归档读取。
  - **内部控制 API (`/internal/v1/...`)**：共 15 个端点，涵盖 Controller 租约、操作认领/推进、节点注册和 Ticket 准入。
  - **健康检查 (`/healthz`)**：通过 `store.Pool.PingContext` 检查数据库连通性；附加 `dependencies.objectStore` 与 `dependencies.gitServer` 的 configured/unconfigured 配置状态，不探测 S3、Git Server 或公开 endpoint/凭据。
- 任何未注册的端点均会被 `r.NoRoute` 捕获并返回 `404 not_found`。

### 两层身份认证
- **服务凭据（Service Credential）**：从标准的 `Authorization: Bearer <token>` 请求头读取。必须是由已授权密钥签发且包含 `kind="service"` 的有效 JWT。
- **网关角色授权**：对于公开端点，服务 token 必须拥有 `role="gateway"` 角色声明。
- **用户凭据（User Credential）**：从 `X-Ora-User-Token: Bearer <token>` 请求头读取。必须是包含 `kind="user"` 的有效签名 token。
- **调用方主体绑定（Caller-Subject Binding）**：强制校验 `user.Caller == service.Subject`，杜绝跨网关的凭据冒用行为。

### 严格的请求校验与解码
- **载荷大小上限约束**：通过 `http.MaxBytesReader` 强制对所有入站请求体施加 64 KiB 的严格上限。
- **严格的 JSON 规范性检查**：采用启用了 `UseNumber()` 的 `json.Decoder`。任何尾随字节或多余的 JSON 值均会被拒绝并返回 `400 invalid_json`。
- **未知字段拦截**：JSON 请求体中仅允许包含 `Route.Fields` 显式声明的字段。遇到任何未知属性会立即失败并返回 `400 unknown_field`。
- **类型安全强校验**：字段类型经由 `validField` 进行严格检查，确保时间戳、UUID、整型和布尔属性在流转至领域核心之前完全符合预期的 Schema。

### Fault 映射与关联追踪
- **请求链路关联**：为每个入站 HTTP 请求生成唯一的 UUID `X-Request-Id`，并挂载至请求 Context、响应 Header 以及结构化日志事件中。
- **错误规范化**：捕获 panic 和领域错误，并通过 `core.ErrorCode(err)` 进行处理。将 `*core.Fault` 转换为带有相应 HTTP 状态码的结构化 JSON 响应（`code`、`params`、`requestId`）。内部数据库或系统错误会被屏蔽为 `500 internal_error`，不会披露内部基础设施细节。

## 边界与不变量

- **无业务状态**：路由层不持有任何业务逻辑、领域状态流转或数据库连接。它完全委托给 `core.Store.Public` 与 `core.Store.Control` 执行。
- **契约严格同步**：路由路径、允许字段以及 HTTP 方法必须与 `internal/contract` 及 `api/openapi.json` 保持严格同步。

参见 [api 总览](../README.md)、[核心领域状态机](../../core/README.md)、[OpenAPI 契约](../../contract/README.md) 与 [认证配置与凭据](../../../docs/authentication.md)。

## 个人模型路由

`/me/model-connections` 与 `/me/model-default` 使用已验证用户作用域，严格校验模型数组和版本。
credential 路由只用于公开契约；Cloud HTTP 在读取请求体前拒绝，Gateway 将请求直接交给
model-gateway。Thread 返回发起者、模型摘要及可追加/可结束权限。见 [模型连接](../../../docs/model-connections.md)。

## Agent 会话 Dashboard 路由

- `GET /api/v1/tenants/:tid/agent-sessions`：接受 `projectId`、`status`、`agent`、`from`、`to`、
  `after`、`limit`。未知、重复或空查询参数返回 `400 invalid_session_filter`；领域层继续校验 UUID、单个状态枚举
  与 RFC 3339 时间区间。默认每页 50，最大 100，按 `(createdAt,id)` 倒序；`after` 是当前租户内上一页
  的 UUID 游标，时间筛选按创建时间 `[from,to)`。
- `GET /api/v1/tenants/:tid/issues/:iid/runs/:rid/history`：不接受任何查询参数、对象键或 URL。
  返回 `{source:"archive",records:[...],sizeBytes,sha256}`，不暴露私有存储地址。

两条路由仍需有效 gateway 服务 JWT 与绑定该服务的用户 JWT；Core 使用当前用户/租户成员资格，归档还
校验 Issue/Run 作用域，并在外部读取后再次鉴权。它们不创建身份、会话或业务状态。浏览器继续使用正常
Gateway 会话，不持有 S3 或内部服务凭据；会话响应使用 `Cache-Control: no-store`。

归档最多 32 MiB / 100000 个合法 JSONL 对象，核验真实大小与 SHA-256。未登记/已过期为
`404 history_unavailable`，超限为 `413 history_too_large`，损坏为 `502 history_invalid`，
存储不可用为 `502 history_unavailable`；用户/成员权限和不可见 Issue/Run 使用现有安全错误。
归档数字、字段及文件顺序保持不变。列表只覆盖 Cloud Agent IssueRun；token、cost、父子关系等上游未持久化
数据不会由 API 推测补齐，也不会扫描用户文件系统。实时投影仍使用原 Thread API 与空间 SSE 失效通知。

模型统计另外提供 `.../runs/:rid/statistics`（不接受查询）与 `.../runs/:rid/model-calls`
（只接受单值非空 `limit/after`）。二者同样要求当前成员与 Agent Run 作用域，使用 `private, no-store`，
不依赖归档。模型请求默认 50 条、最多 100 条，按 `(startedAt,id)` 升序，游标必须属于同一 Run。
汇总是全 Run 已登记 HTTP 尝试的统计，缺失指标返回 null 及覆盖信息；不返回正文、grant 或凭据。
