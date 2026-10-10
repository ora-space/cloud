# internal/controlgrpc: Controller 内部控制契约的 gRPC 服务端

[中文](README.md) | [English](README.en.md)

`internal/controlgrpc` 把 [`internal/controlpb`](../controlpb/README.md) 的契约暴露为 gRPC 服务。它只是翻译层：
每个 RPC 从 metadata 读出调用方 Controller 的身份，把请求转换为 `core.ControlRequest`，经与 JSON 内部接口相同的
`Store.Control` 事务执行，再把 `Fault` 映射为 gRPC 状态。没有业务规则、缓存或隐式重试。

## 调用方身份

生产入口要求 TLS 1.3 双向认证，即连接两端均验证可信证书。Controller 证书须包含配置的服务 URI，
且 ControllerId 必须与 `x-ora-controller-id` 一致；metadata 不能选择另一个服务身份。
证书缺失或范围不匹配即拒绝，生产没有明文回退。`NewDevelopment` 仅用于显式的进程内测试夹具，
不进入生产监听器。服务身份也不能替代运行时使用权限、操作会话或代次校验。

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
  execution，已退役的 storage/worktree effect 不出现在其中。`ListLiveSandboxes` 没有 JSON 路由：它是租约
  持有者的只读列表，列出应当持有 Node 会话的 sandbox（未在终止、未终止、当前 generation、ensure 已成功），
  附带 ensure 报告的 NodeId、Substrate 标识与未结束的 Node 记录，使重启后的 Controller 无需等待各
  Workspace 的下一个 operation 即可重连。

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
生产监听始终使用经过认证的 TLS。

## 运行时控制

`RuntimeControlService` 交付精确的租户、Workspace、沙盒、运行时代次、Node 宿主代次、用户控制代次
和 Controller 租约代次。控制确认用于核对入口关闭及未完成责任；派发前还须取得新鲜执行许可。
迟到快照返回 `ABORTED stale_runtime_control`，不能续期或复活已撤回的会话。
业务权威与证据边界见 [运行时控制](../../docs/runtime-control.md)。

## 个人模型引用与能力

会话编解码仅在非空时携带 `model_binding_id`，Echo 输入保持兼容。Node 注册将握手模型代理能力
保存到权威记录，记录回放拒绝同一宿主代次的能力改变。个人模型的派发和授权不接受缺能力的
旧 Node；服务不传递 API Key 或模型代理临时令牌。见 [模型连接](../../docs/model-connections.md)。
