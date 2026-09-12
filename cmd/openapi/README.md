# cmd/openapi: OpenAPI 文档生成工具

[中文](README.md) | [English](README.en.md)

`cmd/openapi` 是一个代码生成和同步工具，用于根据 Go 契约定义输出权威 OpenAPI 3.0 规范。

## 职责

- **契约编译**：调用 `internal/contract.Document()`，构建覆盖全部 19 个公开端点、15 个内部控制端点以及健康检查端点的权威 OpenAPI 3.0 文档对象。
- **制品同步**：将文档序列化为带缩进的 JSON，并写入 `api/openapi.json`。
- **单一事实来源（SSOT）**：保证 `api/openapi.json`、`router.Routes()` 和 `internal/contract` 始终严格一致。

## 不变量

- **禁止手动编辑**：不得手动修改 `api/openapi.json`。对路由、请求体、查询参数或状态码的任何修改都必须在 `internal/contract` 和 `internal/api/router` 中进行，然后执行以下命令重新生成：
  ```sh
  task openapi
  ```
- **CI 验证**：CI 会检查已提交的 `api/openapi.json` 是否与 `cmd/openapi` 的输出完全一致；如果存在任何未提交的 Schema 漂移，检查将失败。

参见 [cmd 入口总览](../README.md)、[契约定义包](../../internal/contract/README.md) 与 [HTTP 路由网关](../../internal/api/router/README.md)。
