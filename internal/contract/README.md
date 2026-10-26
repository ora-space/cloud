# internal/contract：OpenAPI 定义

[中文](README.md) | [English](README.en.md)

`Document()` 以 Go 值构建 OpenAPI 3.0 文档；`openapi.go` 组织路径与通用 schema，`repository.go` 和 `session_statistics.go` 提供对应领域的 schema。生成物是 [api/openapi.json](../../api/openapi.json)，字段不在 README 再维护一份表。

## 修改与生成

1. 对齐 [router.Routes](../api/router/router.go) 的路径、字段、请求/响应与状态码。
2. 修改本包定义，运行 `task frontend:generate`，同时生成 OpenAPI 和 `frontend/src/api` 客户端。
3. 提交定义与生成物；不手工编辑 JSON 或生成客户端。

`TaskConversationMessage`、任务对话的双向序号游标、发送/取消/重试/恢复/显式放弃未交付成果，以及 `issue.conversation_changed` 失效事件从本包生成；任务读取的 `activeRunId`、`blockedRunId`、`pausedRunId`、`canStop` 和停止的 202 响应也由同一契约声明，旧客户端可忽略新增状态字段。实时旁路由 `ThreadPreviewSnapshot`、Thread `historySequence`、快照读取和 `getThreadPreviewEvents` SSE 声明；快照为完整替换且可缺失，不是持久 Thread 或执行成功证据。

`object()` 只有非空时才写 `required`，避免严格 OpenAPI 消费方拒绝 null/空数组。

## 验证与边界

- `TestPublishedOpenAPIIsValidAndCurrent` 验证规范有效且生成物无漂移。
- `TestRequiredIsOmittedWhenEmpty` 遍历 JSON 树验证 required 形状。
- [integration/contract_test.go](../../integration/contract_test.go) 对真实 HTTP 响应校验同一契约。
- 本包只定义公开传输形状；授权、生命周期、数据库和外部 I/O 由各实现包拥有。浏览器凭据写入契约不代表 Cloud 接收原始 Key。

生成命令见 [cmd/openapi](../../cmd/openapi/README.md)；跨模块语义见 [HTTP API](../../docs/core/http-api.md) 和 [文档地图](../../docs/README.md)。

产物列表与单文件下载各自声明响应 schema，必须先于通用 `/runs` 分支匹配；定时产物失效事件同时进入公开 SSE 枚举。`scheduled_artifact_test.go` 固定这两处边界，防止生成客户端与实际返回不一致。

单产物响应是经校验的原始字节，强制安全文件名的附件、二进制类型、nosniff 与私有禁缓存；错误仍用 JSON Fault。
