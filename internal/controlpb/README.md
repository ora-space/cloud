# internal/controlpb: 内部控制契约的生成代码

[中文](README.md) | [English](README.en.md)

`internal/controlpb` 是 [`proto/`](../../proto/README.md) 下 `ora.cloud.internal.v1` 契约的 Go 生成物：
消息类型、`ControllerLeaseService`／`ExecutionService`／`WorkspaceOperationService`／`NodeReportService`／`AgentRunService`／`RuntimeControlService`／`ControlSignalService` 的服务端桩与客户端。
Cloud 是这些服务的服务端；客户端类型只供进程内测试与替身使用。

## 边界与不变量

- **只读**：目录内全部为生成代码，不手工编辑；行为变更只能通过修改 `proto/` 并重新生成。
- **无业务语义**：生成类型承载契约声明的身份与作用域字段，不判定租户或成员权限；服务实现位于 `internal/controlgrpc`，业务校验位于 Core。
- **无漂移**：`task proto:check` 与 CI 用固定版本插件重新生成并比较；生成物与 `proto/` 不一致即失败。
