# internal/controlgrpc: Controller 内部控制契约的 gRPC 服务端

[中文](README.md) | [English](README.en.md)

`internal/controlgrpc` 把 [`internal/controlpb`](../controlpb/README.md) 的契约暴露为 gRPC 服务。它只是翻译层：
每个 RPC 从 metadata 读出调用方 Controller 的身份，把请求转换为 `core.ControlRequest`，经与 JSON 内部接口相同的
`Store.Control` 事务执行，再把 `Fault` 映射为 gRPC 状态。没有业务规则、缓存或隐式重试。

## 调用方身份

- 当前阶段不认证 Controller：每次调用以 `x-ora-controller-id` metadata 声明 ControllerId，缺失、为空或超过
  256 字节时返回 `INVALID_ARGUMENT`。
- 该值只作为租约与提交记录的持有者进入 `Store.Control`（以 `role=controller` 的 principal 表示），不能由请求字段指定。
- unary 与 stream 拦截器共用同一读取；后续的服务端流沿用。因为没有认证，`control.grpc_addr` 只应监听回环或私网。

## 错误映射

状态码是主分类，`ErrorDetail{ErrorCode}` 作为 `google.rpc.Status` detail 附上，两者在 `fault.go` 一处决定：

| `Fault` | gRPC 状态 | `ErrorCode` |
|---|---|---|
| `lease_held` | `FAILED_PRECONDITION` | `LEASE_HELD` |
| `stale_controller`、`stale_operation` | `FAILED_PRECONDITION` | `STALE_CONTROLLER` |
| 其他 409 | `ABORTED` | `CONFLICT` |
| 400 | `INVALID_ARGUMENT` | `INVALID_INPUT` |
| 403 | `PERMISSION_DENIED` | `SERVICE_FORBIDDEN` |
| 404 | `NOT_FOUND` | `NOT_FOUND` |
| 数据库失败 | `UNAVAILABLE`（不带数据库细节） | `UNAVAILABLE` |

## 已实现的服务

- `ControllerLeaseService`：`AcquireLease`／`RenewLease`／`ReleaseLease` 直接对应 `lease_acquire`／
  `lease_renew`／`lease_release`；全局单租约、30 秒过期、`epoch` 单调递增，过期由数据库时钟判定。

- `ExecutionService`：clone 闭环的执行登记，对应 `core` 的 `clone_*` 动作。`ClaimWork` 是纯读（归属在
  `RecordDispatch` 事务中决定）；`RecordDispatch`／`TakeOverNodeEvent`／`RecordQueriedResult` 携带
  `submission_id`，同身份同内容回放记录的响应、不同内容 `ABORTED+CONFLICT`；`GetDispatch`／
  `ListPendingDispatches` 是恢复读取，不要求持有租约。输入与结果以固定 JSON 形状落库
  （`{kind, repositoryUrl, branch}`；`{node, outcome, path, commit | reason, retainedPath}`），冲突按该形状比较。

- `WorkspaceOperationService`：Workspace 生命周期操作（specs
  `decisions/cloud/controller-integration/20260926-workspace-operations-and-controller-reported-nodes.md`）。
  `ClaimOperation`／`PlanEffect`／`RecordEffectResult`／`AdvanceOperation`／`DeferOperation` 与
  `/internal/v1/operations` JSON 路由使用相同的 control 动作，两个入口共享租约、operation version 与步骤
  fencing。`ClaimOperation` 会领取（重新领取递增 version），不是纯读。快照携带该 operation 的 clone
  execution，已退役的 storage/worktree effect 不出现在其中。

- `NodeReportService`：Controller 报告它持有会话的 desktop Node。`RegisterNode` 按（sandbox 实例、
  `node_incarnation_id`）幂等，`node_id` 必须等于 sandbox_ensure 返回值；上一个 incarnation 结束后才接受新的。
  `ReportNodeStatus`／`EndNode`／`ReportNodeIdle` 与 Node 凭据路由共享事务与 fencing。

- `ControlSignalService.Watch`：Controller 发起的服务端流。打开时以 `lease_check` 校验 epoch（只读，不续期）；
  之后从进程内 `core.ControlHub` 转发信号：`clone_requests` 提交后的 `WorkAvailable{operation_id}`、Workspace operation 创建或重试后的 `OperationAvailable{operation_id}`、
  服务关停前的 `Drain`。至多一次、不持久化、慢订阅者丢信号；`Drain` 后流干净结束（EOF），
  关停期间新的 `Watch` 返回 `UNAVAILABLE`。流只在排空时以 OK 结束，其他结束都带错误状态，持有者可以把
  干净结束当作 `Drain`；`Drain` 只表示本实例即将停止，不要求持有者释放租约。监听接受间隔不短于 5 秒的
  客户端 HTTP/2 PING（连接上没有活动流时也接受），使 Controller 的保活不会被 `GOAWAY(too_many_pings)`
  断开；服务端自身不发起 PING。监听地址由 `control.grpc_addr` 配置，
在 TLS 落地前只应绑定回环或私网地址。
