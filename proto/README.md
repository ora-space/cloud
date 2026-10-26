# proto：Controller 内部契约源码

[中文](README.md) | [English](README.en.md)

`ora/cloud/internal/v1/` 是 `ora.cloud.internal.v1` 的唯一字段来源。Cloud 提供所有 RPC 服务，Controller 拨出连接。消费方锁定本仓库 commit 后生成客户端，不复制 proto 文件。

实时预览使用独立 `PublishThreadPreview` RPC，按执行和版本替换有界快照，不参与 Ack；`ThreadEvent.history_sequence` 将已定型记录关联到预览位置。Controller 消费方升级生成代码后才可协商启用该旁路。

| 文件 | 内容 |
|---|---|
| `errors.proto` | ErrorDetail / ErrorCode |
| `lease.proto` | ControllerLeaseService |
| `executions.proto` | ExecutionService、领取/登记/恢复 |
| `plugin_executions.proto` | 插件安装/移除输入与结果，含冻结的 Agent 声明版本与摘要关联（不含静态副本） |
| `agent_executions.proto` | Agent 会话（含定时运行的 verdict 与产物声明）、Revision 与定时产物的输入/结果、上传授权 |
| `agent_runs.proto` | AgentRunService、Thread 事件与命令、产物上传授权 |
| `operations.proto` | WorkspaceOperationService |
| `nodes.proto` | NodeReportService、身份与能力 |
| `runtime_control.proto` | RuntimeControlService、快照/确认/许可/强停 |
| `signals.proto` | ControlSignalService.Watch |

## 生成与变更

- `buf.yaml` 使用 STANDARD lint 和 FILE 级 breaking；v1 仅接受兼容修改，破坏性变化用并列 v2。
- `buf.gen.yaml` 固定远程 Go/gRPC 插件，生成至 [controlpb](../internal/controlpb/README.md)，需要网络但不需本机 protoc。
- 修改后执行 `task proto:lint`、`task proto:breaking`（`PROTO_BASE_REF` 默认 main）、`task proto:generate`；定义与生成物一起提交。
- `task proto:check` 重新生成并检查漂移；`task check` 包含 lint 与漂移，CI 还检查 breaking。消费方更新锁定 commit 并重新生成。

跨模块提交身份、派发/确认顺序、流恢复和凭据边界见 [Controller 契约](../docs/runtime/controller-integration.md)；控制许可见 [运行时控制](../docs/runtime/runtime-control.md)，模型绑定见 [模型访问](../docs/models/model-access.md)。服务实现见 [controlgrpc](../internal/controlgrpc/README.md)。
