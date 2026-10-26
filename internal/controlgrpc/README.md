# internal/controlgrpc：Controller gRPC 适配

[中文](README.md) | [English](README.en.md)

把 [controlpb](../controlpb/README.md) 请求翻译为 Core 调用，将 Fault 翻译为 gRPC 状态；业务规则、提交去重、租约与恢复状态都在 Core。

| 文件 | 职责 |
|---|---|
| `server.go` | `NewSecure` / `NewDevelopment`、身份拦截器、服务注册与 keepalive |
| `lease.go`、`nodes.go`、`operations.go` | 租约、Node 报告、Workspace operation 与 effect 转换 |
| `executions.go`、`execution_codec.go` | clone、插件、Agent 会话与 Revision 交付的领取、登记、恢复及存储形状转换 |
| `agent_runs.go` | Thread 事件、命令、上传授权和可选的实时预览 |
| `runtime_control.go` | 控制确认、派发许可与强停 |
| `signals.go` | 转发 ControlHub 的 Watch 信号，排空时结束流 |
| `fault.go` | 安全状态与 ErrorDetail 分类，不携带数据库细节 |

生产 `NewSecure` 要求双向认证 TLS，匹配证书 URI 与 `x-ora-controller-id`；metadata 不能切换身份。`NewDevelopment` 只供进程内夹具，生产没有明文回退。服务身份不代替用户权限和运行时代次。

`AgentRunService.PublishThreadPreview` 是可选旁路：调用 `Store.PublishThreadPreview` 核对相同身份、租约与活执行，写入有界内存快照。它不进入持久 submission/receipt，不返回 Node Ack。ThreadEvent 的 `history_sequence` 只作为最终记录对应预览的位置元数据转交核心。

Watch 不持久化、不赋予工作所有权，慢订阅者可能丢信号。排空时发送 Drain 并结束流，关停期间拒绝新 Watch。监听接受间隔至少 5 秒、包括无活动流时的客户端 PING，服务端不主动发 PING。

字段和生成见 [proto](../../proto/README.md)；跨模块语义见 [Controller 契约](../../docs/runtime/controller-integration.md)、[运行时控制](../../docs/runtime/runtime-control.md) 与 [模型访问](../../docs/models/model-access.md)。本包测试覆盖转换、错误、TLS、信号与 keepalive，真实数据库链路见 [integration](../../integration/README.md)。
