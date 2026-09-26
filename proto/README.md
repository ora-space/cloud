# proto: Controller 内部控制契约的唯一来源

[中文](README.md) | [English](README.en.md)

`proto/ora/cloud/internal/v1/` 是 Cloud 与 Controller 之间内部控制契约（package `ora.cloud.internal.v1`）
的唯一来源。Cloud 是全部 RPC 的服务端并拥有权威持久化；Controller 只拨出，不向 Cloud 暴露服务。
消费方（`ora-space/desktop` 的 `ora-controller`）以本仓库的 commit 锁定契约并生成自己的客户端，
不复制 `.proto`。

| 文件 | 内容 |
|---|---|
| `errors.proto` | `ErrorDetail{ErrorCode}`：附在 `google.rpc.Status` detail 上的错误分类；gRPC 状态码是主分类，枚举细分 |
| `lease.proto` | `ControllerLeaseService`：全局协调租约，`epoch` 作为所有写操作的 fencing token |
| `executions.proto` | `ExecutionService`：领取工作、派发前登记、Node 事件接管、查询结果保存与恢复读取；第一版只覆盖 clone 闭环 |
| `operations.proto` | `WorkspaceOperationService`：领取、计划 effect、登记 effect 结果、推进与延期 Workspace 生命周期操作 |
| `nodes.proto` | `NodeReportService`：Controller 登记它持有会话的 desktop Node（`node_id` + `node_incarnation_id`），并报告状态、结束与 idle |
| `signals.proto` | `ControlSignalService.Watch`：Controller 发起的服务端流，下发 `WorkAvailable`／`OperationAvailable`／`Drain`／`NodeAssignment` |

## 语义要点

- **提交身份**：改变状态的请求携带调用方生成的 `submission_id`。同身份同内容返回原响应且不重新应用；
  同身份不同内容返回 `ABORTED` + `CONFLICT`。回复丢失后用同一身份重传，不换身份。
- **边界顺序**：`RecordDispatch` 成功后 Controller 才可向 Node 派发；`TakeOverNodeEvent` 成功后才可
  向 Node 发送该序号的确认；`RecordQueriedResult` 不产生确认依据。
- **信号流**：至多一次、不持久化、不改变归属；断流后 Controller 退回周期 `ClaimWork`。
- **无租户字段**：契约只携带 Cloud 已授权的 opaque 身份；评审以此拒绝携带 tenant／user／membership 的变更。

## 生成与检查

- `buf.yaml`：`STANDARD` lint，`FILE` 级 breaking；`v1` 只接受非破坏性修改，破坏性变更以并列的 `v2` package 表达。
- `buf.gen.yaml`：固定版本的 `protocolbuffers/go` 与 `grpc/go` 远程插件，生成到 [`internal/controlpb`](../internal/controlpb/README.md)（需要网络，不需要本机 `protoc`）。
- `task proto:lint`、`task proto:breaking`（以 `PROTO_BASE_REF`，默认 `main` 为基线）、`task proto:generate`、
  `task proto:check`（重新生成并在有 diff 时失败）；`task check` 与 CI 执行 lint、breaking 与漂移检查。

## 变更流程

1. 修改 `.proto`，运行 `task proto:lint` 与 `task proto:breaking`。
2. 运行 `task proto:generate`，把生成物与改动一起提交；合入主干后该 commit 即消费方可锁定的契约版本。
3. desktop 更新 submodule 指针到该 commit、重新生成客户端；编译失败即契约不对齐。

契约的语义由 specs 的
`decisions/cloud/controller-integration/0-cloud-owned-internal-grpc-contract.md` 拥有；本目录只承载字段。
