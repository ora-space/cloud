# internal/contract: API 契约与 OpenAPI 规范

[中文](README.md) | [English](README.en.md)

`internal/contract` 以代码化方式定义 Ora Cloud 的权威 OpenAPI 3.0 数据模型与 Schema 结构。它是全系统所有 API 请求、响应结构与 Fault 错误定义的单一事实来源（SSOT）。

## 职责

- **编程式生成 OpenAPI 文档**：`contract.Document()` 构建完整的 OpenAPI 3.0 规范树，定义元数据、安全方案（HTTP Bearer JWT）、参数、请求体、状态码和响应 Schema。
- **组件模式（Component Schema）建模**：为领域实体定义严格的 JSON Schema：
  - 核心资源实体：`Tenant`、`TenantMember`、`User`、`Project`、`Workspace`、`Task`、`Operation`、`Effect`、`WorkspaceNode`、`Ticket`。
  - clone 步骤：`CloneInput`（`{kind, repositoryUrl, snapshotRef, baseCommit, workspaceRef}`，内部控制快照的 `cloneInput` 与 `OperationRequest.clone`）与 `CloneExecution`。
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

## Agent 会话与归档契约

`AgentSessionSummary` 是 `GET /api/v1/tenants/:tid/agent-sessions` 的列表元素，包含 Run/Issue 标识、Issue
标题与编号、可空项目与插件信息、运行和 Thread 状态、时间、可空模型摘要与 `historyAvailable`。
此布尔值仅表示未过期归档登记存在，不能代替一次成功归档读取。当前数据源只覆盖 Agent IssueRun。

列表允许 `projectId`、`status`、`agent`、`from`、`to`、`after`、`limit`：时间按创建时间的 `[from,to)`
筛选，状态是单值枚举，Agent 是精确插件标识；页大小默认 50、最大 100。顺序为 `(createdAt,id)` 倒序，
`nextCursor` 是排他 UUID 游标，与 Thread 的整数 `seq` 游标不同。未知、空、重复查询项均被拒绝。

`SessionHistory` 对应 `GET /api/v1/tenants/:tid/issues/:iid/runs/:rid/history`，字段为
`source="archive"`、`records`、`sizeBytes`、`sha256`。`records` 的每项是保留未知字段的 JSON 对象，
维持文件顺序和原始数字，不将 provider/history 结构硬编码进 Cloud 公共 Schema。消费方负责处理同一
history `seq` 的后续更新；Cloud Thread `seq` 不替代 history `seq`。

归档路由不接受查询参数、路径或 URL；现有双层凭据与当前成员/Issue/Run 授权适用。最大 32 MiB /
100000 条记录，下载前后核验权限和登记。公开错误为 `404 history_unavailable`（未登记/过期）、
`413 history_too_large`、`502 history_invalid` 或 `502 history_unavailable`；错误中没有私有存储细节。
模型、工具耗时、token、cost 和父子会话数据的可得性取决于已持久化证据，不能用默认值伪造缺失数据。
Schema、路由、生成 OpenAPI 和前端客户端必须一并更新；真实用户日志不得成为契约测试 fixture。

## 模型请求指标契约

`session_statistics.go` 定义 `MetricAggregate`、`ModelAttemptUsage`、`SessionModelAttempt` 和
`SessionModelStatistics`。`/statistics` 无查询，`/model-calls` 仅 `limit/after`；统计范围固定为
`recorded_http_attempts`，来源为 `model_gateway`，不是历史全量补算或账单。汇总独立于请求分页。
input/output/total/cache read/cache write/reasoning 分别可空并携带覆盖率，不将未知变成 0。
OpenAI input 包含缓存读取，Anthropic input 不含缓存读写；reasoning 为输出子集，不重复加总。
`firstOutputMs` 是网关首个有效输出观测，不是模型 TTFT；状态 `started` 不保证请求仍存活。
