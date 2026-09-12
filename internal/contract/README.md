# internal/contract: API 契约与 OpenAPI 规范

[中文](README.md) | [English](README.en.md)

`internal/contract` 以代码化方式定义 Ora Cloud 的权威 OpenAPI 3.0 数据模型与 Schema 结构。它是全系统所有 API 请求、响应结构与 Fault 错误定义的单一事实来源（SSOT）。

## 职责

- **编程式生成 OpenAPI 文档**：`contract.Document()` 构建完整的 OpenAPI 3.0 规范树，定义元数据、安全方案（HTTP Bearer JWT）、参数、请求体、状态码和响应 Schema。
- **组件模式（Component Schema）建模**：为领域实体定义严格的 JSON Schema：
  - 核心资源实体：`Tenant`、`TenantMember`、`User`、`Project`、`Workspace`、`Task`、`Operation`、`Effect`、`WorkspaceNode`、`Ticket`。
  - 错误 Schema：包含错误码、参数映射和请求 ID 的标准 `Fault` Schema。
  - 参数类型强校验：包含 `uuid`、`date-time`、`int64` 及字符串枚举等严谨的数据校验格式。
- **契约完整性校验测试**：
  - `TestRoutesCovered`：严格校验 `router.Routes()` 中注册的每一条路由均在生成的 OpenAPI 路径树中得到完整体现。
  - `TestDocumentValid`：依据 OpenAPI 3.0 官方规范，验证生成的 JSON 文件的结构合法性。

## 不变量与工作流

- **禁止手动编辑 JSON**：`api/openapi.json` 由此包通过 `cmd/openapi` 直接生成。开发者在此处修改 Go 定义，运行 `task openapi`，并同时提交代码和生成的 JSON 制品。
- **三方协同严格对齐**：任何 API 路由的改动都必须同步更新：
  1. `internal/api/router`（`router.Routes()`）。
  2. `internal/contract`（`contract.Document()`）。
  3. `api/openapi.json`（通过 `task openapi` 重新生成）。

参见 [OpenAPI JSON 制品文件](../../api/openapi.json)、[HTTP 路由网关](../api/router/README.md) 与 [cmd/openapi 工具](../../cmd/openapi/README.md)。
