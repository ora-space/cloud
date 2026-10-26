# internal/api/router：HTTP 路由适配

[中文](README.md) | [English](README.en.md)

`Routes()` 是路由、动作和允许请求字段的显式清单。`New` 装配生产 Gin 引擎，`NewDevelopment` 另行打开模拟器使用的旧管理 JSON 入口。生产对已退役的 `/internal/v1/...` 路由返回 `410 retired_management_transport`，Controller 使用 [gRPC 服务](../../controlgrpc/README.md)。

## 包内处理顺序

- 为请求建立 `X-Request-Id` 与日志上下文；健康检查读取数据库就绪与对象存储、Git Server 的配置状态，不探测 S3 或 Git Server。
- 验证服务/用户凭据及 caller 绑定；公开字段由 `Route.Fields` 白名单与 `validField` 校验。
- 默认请求体上限为 64 KiB；`ThreadMessagePath` 与 `TaskConversationMessagesPath` 单独使用 256 KiB，以容纳合法消息的 JSON 封装；正文仍由领域核心限制为 64 KiB。
- JSON decoder 使用 `UseNumber`，拒绝未知字段、额外 JSON 值和非法类型；查询解析按对应读取入口约束。
- 普通请求交给 `Store.Public`；归档、统计和外部服务核对等走专用 Store 方法。开发管理请求交给 `Store.Control`。
- 领域错误通过 `core.ErrorCode` 映射为安全 Fault；未注册路径返回 `404 not_found`。

任务对话新增 `GET/POST /issues/:iid/conversation`、显式 Agent 消息、待交接消息取消/重试、成功成果恢复与未交付成果显式放弃入口，使用同源成员鉴权和幂等键。URL 中的 `:mid` 只作为待交接消息身份，不由客户端指定作者或状态。`POST .../conversation/stop` 返回 202，原子请求取消并暂停后续队列；读取返回当前执行、最早未解决屏障、手动暂停及 `canStop`。历史详情的选择不会改变停止目标，恢复必须明确指向最早屏障。

`thread_preview.go` 提供运行级快照 SSE；每次发送和定期心跳都重新核对凭据与成员权限，断开仅取消订阅。慢浏览器只接收最新快照，不阻塞 Agent 或持久记录。

恢复请求的 `acknowledgeLostWork` 严格接受 JSON 布尔值；`false` 由领域层拒绝，字符串、数字及 null 在 HTTP 层拒绝。真实 HTTP 测试覆盖确认、类型拒绝和同键重放。

`tianzhou.go` 是部署目录查询适配；领域成员权限仍由 Core 决定。测试在本目录覆盖请求校验与传输边界，完整真实数据库/API 契约验证在 [integration](../../../integration/README.md)。

## 契约入口

字段和状态码以 [OpenAPI](../../../api/openapi.json) 为准；生成规则见 [contract](../../contract/README.md)。跨模块语义见 [HTTP API](../../../docs/core/http-api.md)、[会话读取](../../../docs/agent-runs/session-reads.md) 和 [个人模型访问](../../../docs/models/model-access.md)。API Key 写入由 Gateway/model-gateway 承接，Cloud 路由在读取凭据请求体前拒绝，不能回退到普通 JSON 分派。

定时产物列表与下载按只读会话边界处理：拒绝任何查询参数，成功响应使用 `private, no-store`，调用者只能通过路由选择运行与序号。

单产物响应是经校验的原始字节，强制安全文件名的附件、二进制类型、nosniff 与私有禁缓存；错误仍用 JSON Fault。
