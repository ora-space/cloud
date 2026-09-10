# internal/api: HTTP 表现层与协议接入层

`internal/api` 包含 Ora Cloud 的 HTTP 协议转换和路由组件。它负责将入站 HTTP 请求转换为 `internal/core` 的领域请求，强制执行传输安全和载荷大小限制，并按照 OpenAPI 契约格式化响应和错误。

## 模块概览

- [router](router/README.md)：配置 Gin HTTP 引擎、注册路由、验证调用方凭据、在严格限制下解析请求体，并将领域错误映射为公开 Fault 契约。

## 职责与边界

- **仅限传输协议转换**：该层纯粹作为 HTTP 与核心领域之间的适配器。不包含任何业务逻辑、状态机或 SQL 查询。
- **严格输入校验**：以 64KB 为大小上限解码 JSON 请求体，并在调用领域方法之前拒绝含有未知字段或格式错误的请求。
- **契约一致性保障**：所有路由路径、查询参数、请求体字段及响应状态码均严格遵循 `internal/contract` 与 `api/openapi.json` 中的规范定义。
- **数据库隔离**：该层中的 Handler 绝不直接接触数据库连接句柄或 GORM 实例；所有数据库交互均由 `core.Store` 统一封装与调度。

参见 [HTTP 路由网关](router/README.md)、[契约定义包](../contract/README.md) 与 [API 规范文件](../../api/openapi.json)。
