# internal/api/router: HTTP 路由与传输适配器

[中文](README.md) | [English](README.en.md)

`internal/api/router` 建立 Ora Cloud 的 HTTP 表现层边界。它基于 Gin 构建，负责绑定 HTTP 路由、验证两层 JWT 身份、强制执行严格的请求体解析限制、规范化错误，并将请求分发到 `internal/core`。

## 职责

### 路由白名单与分发
- `Routes()` 显式声明系统支持的端点白名单：
  - **公开 API (`/api/v1/...`)**：共 19 个端点，涵盖用户、租户、成员关系、项目、工作区、操作以及状态查询。
  - **内部控制 API (`/internal/v1/...`)**：共 15 个端点，涵盖 Controller 租约、操作认领/推进、节点注册和 Ticket 准入。
  - **健康检查 (`/healthz`)**：通过 `store.Pool.PingContext` 检查数据库连通性。
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
