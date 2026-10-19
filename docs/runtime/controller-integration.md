# Controller 内部控制契约

[English](controller-integration.en.md) | 中文

Controller（desktop 仓库的 `ora-controller`）是 Cloud 与沙盒里各个 Node 之间唯一的执行通道。它只主动拨出到 Cloud 的
控制监听，Cloud 是全部 RPC 的服务端并拥有权威持久化；Controller 不向 Cloud 暴露服务，也不持有数据库连接。本文说明这份
gRPC 契约的通用语义和各个服务的职责，字段以 [`proto/`](../../proto/README.md) 为准。

```text
proto/ora/cloud/internal/v1/*.proto     契约的唯一来源（package ora.cloud.internal.v1）
internal/controlpb                       生成的 Go 代码，只读
internal/controlgrpc                     gRPC 服务端：调用方身份、Fault → 状态码，只翻译不含业务
internal/core  Store.Control             与其他入口共用的业务事务
```

`controlgrpc` 的每个 RPC 都转换成一个 `core.ControlRequest`，经 `Store.Control` 在一个短事务里执行，再把结果和错误翻译
回 gRPC。没有业务规则、缓存或隐式重试留在 gRPC 层。

## 身份与连接

- 监听地址 `control.grpc_addr`，只接受 TLS 1.3 双向认证；Controller 证书须带配置的 SPIFFE URI，每个调用带
  `x-ora-controller-id` metadata（见 [内部凭据与服务信任](../identity/authentication.md#controller-的-grpc-身份)）。
- 监听接受间隔不短于 5 秒的客户端 HTTP/2 PING，没有活动流时也接受，使 Controller 的保活不会被 `GOAWAY(too_many_pings)`
  断开；Cloud 自己不发 PING。
- Cloud 停机时先向信号流发 `Drain`，再优雅停止 gRPC，与 HTTP 使用同一个截止时间。被截断的调用可以用同一个提交身份重试，
  不会丢失持久状态。

## 通用语义

### 租约与 epoch

整个部署只有一个全局协调租约（`ControllerLeaseService`）：

- `AcquireLease`：租约空闲或已过期时取得，**epoch 加一**；仍有效且由别人持有时返回 `lease_held`。持有者重复取得只是确认。
- `RenewLease` 延长 30 秒；约定每 10 秒续一次。`ReleaseLease` 立即让租约过期。过期由数据库时钟判定。
- **epoch 是所有写操作的 fencing token。** 领取、登记、推进、计划 effect、取得许可等写入都校验调用方持有当前租约且 epoch 一致，
  否则返回 `stale_controller`。被新持有者取代的旧 Controller 即使还活着，它的每次写入也会失败。
- 恢复读取（`GetDispatch`、`ListPendingDispatches`）不要求租约：接替者需要先找到原来的执行，才能决定接下来做什么，读取本身
  不会改变任何归属。

### 提交身份

改变状态的请求携带调用方生成的 `submission_id`：

- 同一身份、同样内容的请求返回第一次记录的响应，**不重新应用**；同一身份、不同内容返回 `ABORTED` 加 `CONFLICT`。
- 回复丢失后必须用**同一个**身份重传，不能换新身份。重放发生在版本检查之前，所以版本已经前进也能拿回原响应。
- 许可类读取（`GetEffectPermit`、`GetExecutionPermit`）故意不参与重放：每次都现场判定，已撤回的许可不会被旧响应复活。

### 边界顺序

- `RecordDispatch` 成功之后，Controller 才可以把这个执行派发给 Node。
- `TakeOverNodeEvent` 成功之后，Controller 才可以向 Node 确认这个序号的事件；Cloud 在提交接管之后才算收到。
- `RecordQueriedResult` 保存 Controller 主动查询到的结果，但不构成向 Node 确认的依据。

### 错误映射

gRPC 状态码是主分类，`ErrorDetail{ErrorCode}` 作为 `google.rpc.Status` 的 detail 细分：

| Cloud `Fault` | gRPC 状态 | `ErrorCode` |
|---|---|---|
| `lease_held` | `FAILED_PRECONDITION` | `LEASE_HELD` |
| `stale_controller`、`stale_operation` | `FAILED_PRECONDITION` | `STALE_CONTROLLER` |
| 其他 409 | `ABORTED` | `CONFLICT` |
| 400 | `INVALID_ARGUMENT` | `INVALID_INPUT` |
| 403 | `PERMISSION_DENIED` | `SERVICE_FORBIDDEN` |
| 404 | `NOT_FOUND` | `NOT_FOUND` |
| 数据库失败 | `UNAVAILABLE`（不带数据库细节） | `UNAVAILABLE` |

### 信号流

`ControlSignalService.Watch` 是 Controller 打开的服务端流。打开时校验一次 epoch（只读，不续期），之后转发 Cloud 进程内的
控制信号：

| 信号 | 何时发出 |
|---|---|
| `WorkAvailable` | 提交了新的执行工作项（Agent 会话、Revision 交付） |
| `OperationAvailable` | 创建或重试了 Workspace 生命周期操作 |
| `ThreadCommandAvailable` | 提交了新的 Thread 命令 |
| `Drain` | 这个 Cloud 实例即将停止，持有者应停止从它领取 |

- 信号**至多一次、不持久化、不改变归属**，慢订阅者会丢信号。断流后 Controller 退回周期性的 `ClaimWork`、`ClaimOperation`
  和 `ClaimThreadCommands`，所以丢信号只会延迟，不会丢工作。`retry_wait` 到期这类没有提交可跟随的变化，本来就只靠周期领取。
- 流只在排空时正常结束（`Drain` 之后 EOF），其他结束都带错误状态；停机期间新的 `Watch` 返回 `UNAVAILABLE`。`Drain` 不要求
  持有者释放租约。
- proto 里还定义了 `NodeAssignment`，Cloud 目前不发送它。

## 服务

| 服务 | 职责 |
|---|---|
| `ControllerLeaseService` | 全局租约：`AcquireLease`、`RenewLease`、`ReleaseLease` |
| `WorkspaceOperationService` | Workspace 生命周期操作：`ClaimOperation`、`PlanEffect`、`RecordEffectResult`、`AdvanceOperation`、`DeferOperation`、`RecordProvisioningFact`、`ListLiveSandboxes` |
| `NodeReportService` | Controller 代报它持有会话的 Node：`RegisterNode`、`ReportNodeStatus`、`EndNode`、`ReportNodeIdle` |
| `ExecutionService` | 执行登记：`ClaimWork`、`RecordDispatch`、`TakeOverNodeEvent`、`RecordQueriedResult`、`GetDispatch`、`ListPendingDispatches` |
| `AgentRunService` | Agent 运行：`TakeOverThreadEvents`、`ClaimThreadCommands`、`RecordThreadCommandDelivered`、`GrantRevisionUpload`、`GrantRevisionDownload` |
| `RuntimeControlService` | 运行时控制：`ListBindings`、`AcknowledgeBinding`、`GetExecutionPermit`、`GetEffectPermit`、`ListForceStops`、`ConfirmForceStop` |
| `ControlSignalService` | `Watch` 信号流 |

**`WorkspaceOperationService`** 推进 [Workspace 生命周期](workspaces.md) 里的操作。

`ListPendingProvisioningEffects` 是独立于普通 operation 领取的恢复读取：当前租约持有者按 effect UUID 分页获取未清除的 D5 残留责任，每页最多 100 项，返回原 operation、effect 身份与查询所需的原请求。缺少持久终态回执的 effect 始终可读，absent/running 的 ACK 不删除后续查询责任；真实 succeeded/failed 事实经 `RecordProvisioningFact` 提交后才退出列表。读取不领取 operation、不改变业务版本，也不授予重新执行许可。消费方应周期遍历所有页，并在重启、租约切换或结果未知后按原身份继续查询和补交；这份列表不证明消费方已经完成该接线，也不提供自动回收或物理终止。

- `ClaimOperation` 领取最早的一个可执行操作（`queued`、到期的 `retry_wait`，或已经 `running` 的），把它置为 `running` 并记录
  当前 epoch，**版本加一**，从而让还拿着旧快照的工作者失效；它是一次写入，不是纯读。返回的快照包含该操作所需的全部内容：
  Project（含凭据引用）、全部相关 Workspace、沙盒、Node、effect，以及这个操作已有的 clone 和插件执行，恢复中的 Controller
  据此找到原来的执行，而不是登记第二个。
- `RecordProvisioningFact` 在运行 Workspace 的创建被放弃之后，按原操作、effect、执行身份补记迟到的真实事实，不重开任何东西（见
  [创建 blocked 的有界放弃](../agent-runs/startup-settlement.md#创建-blocked-的有界放弃)）。
- `ListLiveSandboxes` 是租约持有者的只读列表，列出应当持有 Node 会话的沙盒（属于当前代次、未在终止、ensure 已成功），附带
  `nodeId`、Substrate 标识和未结束的 Node 记录，让重启后的 Controller 不必等下一个操作就能重连所有 Node。

**`NodeReportService`** 是 Node 状态进入 Cloud 的唯一入口。`RegisterNode` 按（沙盒实例、`node_incarnation_id`）幂等，
`node_id` 必须等于 `sandbox_ensure` 返回的值，同一沙盒上一个实例结束后才接受新的；Node 握手时声明的能力（例如是否具备模型
代理）记入权威记录，同一实例重放时不能改变。所有报告都绑定 Workspace 当前代次和存活沙盒。

**`ExecutionService`** 是一份通用的执行登记表，覆盖 Node 上的几类执行：

| 执行种类 | 来源 | 登记表 |
|---|---|---|
| `clone` | Workspace 操作的 `clone` 步骤 | `clone_executions` |
| `install_plugins`、`remove_plugins` | Workspace 操作的 `plugin` 步骤 | `node_executions` |
| `agent_session`、`deliver_revision` | Agent 运行声明的执行工作项（`execution_work`） | `node_executions` |

- `ClaimWork` 是**纯读**：返回最早的一个可领取的工作项，归属在 `RecordDispatch` 的事务里才决定，所以 Controller 在领取和
  登记之间崩溃不会留下需要恢复的东西。
- `RecordDispatch` 在派发之前登记执行：Cloud 而不是 Controller 决定输入和目标 Node，登记时核对输入与操作或工作项冻结的输入
  一致、目标就是当前 Node、没有未决的同类执行。同一执行、同样输入的重放返回原登记，矛盾的输入返回冲突。
- `TakeOverNodeEvent` 按执行身份和**连续序号**接管 Node 事件并保存收据，重复事件必须与原收据逐字节一致；会话和交付的终态事件
  也走同一个序列，不能越过尚未接管的事件。

**`AgentRunService`** 承载 Agent 运行的会话面：批量接管 Thread 事件、领取并确认投递 Thread 命令、为交付执行签发对象存储的
上传和下载授权。语义见 [Thread](../agent-runs/threads.md) 与 [Revision](../agent-runs/revisions.md)。

**`RuntimeControlService`** 送达运行时控制绑定、签发执行和 effect 许可、执行强停，见 [运行时控制](runtime-control.md)。

## 修改契约

- `buf.yaml` 使用 `STANDARD` lint 和 `FILE` 级 breaking 检查：`v1` 只接受非破坏性修改，破坏性变更用并列的 `v2` package 表达。
- 修改 `.proto` 后运行 `task proto:lint` 和 `task proto:breaking`，再运行 `task proto:generate`，把生成物和改动放在同一个提交里。
  `task check` 与 CI 都执行 lint、breaking 和生成物漂移检查。
- 合入 main 的那个提交就是消费方可以锁定的契约版本。desktop 以 submodule 锁定 Cloud 的提交并生成自己的客户端，不复制
  `.proto`；desktop 编译失败就意味着契约没有对齐。
- 契约的语义由 specs 的 ADR 拥有，本仓库的 `proto/` 只承载字段。

## 相关决策

- [Cloud 拥有 Internal Control gRPC 契约并承担权威持久化](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/0-cloud-owned-internal-grpc-contract.md)
- [Controller 控制监听允许 HTTP/2 保活](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260924-control-listener-keepalive.md)
- [Internal Control gRPC 增加只读 ListLiveSandboxes](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260926-list-live-sandboxes.md)
- [Internal Control gRPC 增加 Workspace 操作与 Controller 代报 Node](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260926-workspace-operations-and-controller-reported-nodes.md)
- [运行时控制由 Cloud 授权、Controller 派发、Node 在真实入口持久隔离旧资格](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260927-fenced-runtime-control-delivery.md)
- [Internal Control gRPC 增加插件、Agent 会话与 Revision 交付三类执行](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260928-agent-run-executions-thread-and-upload-grants.md)
- [会话与交付执行输入增加前序 Revision，新增 `GrantRevisionDownload`](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20261010-revision-restore-contract.md)
