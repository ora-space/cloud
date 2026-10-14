# internal/contract: API 契约与 OpenAPI 规范

[中文](README.md) | [English](README.en.md)

`internal/contract` 以代码化方式定义 Ora Cloud 的权威 OpenAPI 3.0 数据模型与 Schema 结构。它是全系统所有 API 请求、响应结构与 Fault 错误定义的单一事实来源（SSOT）。

## 职责

- **编程式生成 OpenAPI 文档**：`contract.Document()` 构建完整的 OpenAPI 3.0 规范树，定义元数据、安全方案（HTTP Bearer JWT）、参数、请求体、状态码和响应 Schema。
- **组件模式（Component Schema）建模**：为领域实体定义严格的 JSON Schema：
  - 核心资源实体：`Tenant`、`TenantMember`、`User`、`Project`、`Workspace`、`Task`、`Operation`、`Effect`、`WorkspaceNode`、`Ticket`。
  - clone 工作项：`CloneOperation` 与带 `kind` 标签的 `CloneState`（`pending` / `succeeded{path, commit}` / `failed{reason, retainedPath?}`），字段与 Controller 过渡 DTO 逐字对应。
  - 错误 Schema：包含错误码、参数映射和请求 ID 的标准 `Fault` Schema。
  - 参数类型强校验：包含 `uuid`、`date-time`、`int64` 及字符串枚举等严谨的数据校验格式。
- **契约完整性校验测试**：
  - `TestPublishedOpenAPIIsValidAndCurrent`：已提交的 `api/openapi.json` 必须与 `Document()` 逐字节一致，并通过 kin-openapi 的 OpenAPI 3.0 结构校验。
  - `TestRequiredIsOmittedWhenEmpty`：遍历原始 JSON 树，任何 object schema 的 `required` 只能是非空数组。kin-openapi 会容忍 `null` / `[]`，但规范和前端生成器（orval）不会。
  - `integration/contract_test.go`：集成测试用同一份文档校验真实 HTTP 响应结构。

## 不变量与工作流

- **禁止手动编辑 JSON**：`api/openapi.json` 由此包通过 `cmd/openapi` 直接生成。开发者在此处修改 Go 定义，运行 `task openapi`，并同时提交代码和生成的 JSON 制品。
- **`required` 为空时省略**：`object()` 仅在存在必填字段时写入 `required`。OpenAPI 3.0 要求该数组至少一项；严格的下游消费者会拒绝 `null` 或 `[]`。
- **四方协同严格对齐**：任何 API 路由的改动都必须同步更新：
  1. `internal/api/router`（`router.Routes()`）。
  2. `internal/contract`（`contract.Document()`）。
  3. `api/openapi.json`（通过 `task openapi` 重新生成）。
  4. `frontend/src/api`（通过 `task frontend:generate` 重新生成，CI 检测漂移）。

参见 [OpenAPI JSON 制品文件](../../api/openapi.json)、[HTTP 路由网关](../api/router/README.md)、[cmd/openapi 工具](../../cmd/openapi/README.md) 与 [Web 前端](../../frontend/README.md)。

## 模型与 Thread 扩展

`ModelDefinition`、`ModelConnection` 和 `ModelDefault` 定义个人配置。API Key 字段仅写，
credential 接口由 Gateway/model-gateway 承接，Cloud HTTP 不读取其请求体。
Thread 增加发起者、可空模型摘要及可追加/可结束权限；OpenAPI 和客户端必须由生成器更新。
