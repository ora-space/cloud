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
| `executions.proto` | `ExecutionService`：领取工作、派发前登记、Node 事件接管、查询结果保存与恢复读取；执行种类包括 clone、插件安装/移除、Agent 会话与 Revision 交付，会话与交付工作项带目标 Node（`WorkTarget`） |
| `plugin_executions.proto` | 插件安装/移除执行的输入（Cloud 从目录快照拼装的下载与 SHA-256）与逐插件结果 |
| `agent_executions.proto` | Agent 会话与 Revision 交付执行的输入与结果、git 身份、用户轮次，以及只存在于内存的上传授权 `UploadGrant` |
| `agent_runs.proto` | `AgentRunService`：按序批量接管 Thread 事件、领取与登记 Thread 命令、签发 Revision 上传授权 |
| `operations.proto` | `WorkspaceOperationService`：领取、计划 effect、登记 effect 结果、推进与延期 Workspace 生命周期操作 |
| `nodes.proto` | `NodeReportService`：Controller 登记它持有会话的 desktop Node（`node_id` + `node_incarnation_id`），并报告状态、结束与 idle |
| `signals.proto` | `ControlSignalService.Watch`：Controller 发起的服务端流，下发 `WorkAvailable`／`OperationAvailable`／`ThreadCommandAvailable`／`Drain`／`NodeAssignment` |

## 语义要点

- **提交身份**：改变状态的请求携带调用方生成的 `submission_id`。同身份同内容返回原响应且不重新应用；
  同身份不同内容返回 `ABORTED` + `CONFLICT`。回复丢失后用同一身份重传，不换身份。
- **边界顺序**：`RecordDispatch` 成功后 Controller 才可向 Node 派发；`TakeOverNodeEvent` 成功后才可
  向 Node 发送该序号的确认；`RecordQueriedResult` 不产生确认依据。
- **信号流**：至多一次、不持久化、不改变归属；断流后 Controller 退回周期 `ClaimWork`、`ClaimOperation` 与 `ClaimThreadCommands`。
- **上传授权不是输入**：`UploadGrant` 是短期持有者凭据，不进入执行输入、登记或任何日志；执行输入只带对象键。
- **授权来源**：业务授权由 Cloud/PostgreSQL 决定。运行时控制字段携带已确认的租户、用户、会话及目标范围，执行端不得根据未经确认的调用方身份自行扩大权限。

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

## 已批准的运行时控制契约

RuntimeControlService 传递 Cloud 已裁决的目标绑定、关闭确认、即时执行许可和独立强停计划。目标包含 tenant/workspace、实际用户/服务端会话与控制代次；它们用于受信执行端核验范围，不作为客户端自行声明的成员权限。用户控制代次、Controller 租约代次、运行时代次、Node 进程代次及稳定 execution/Node operation ID 分别保存。旧组件未声明 runtime_control 能力时明确拒绝。许可不由幂等响应复活，Controller 派发前重新获取；Node 接受与首次执行入口再次核验。Cloud–Controller 使用双向 TLS。文件/终端/插件/Agent 的新执行入口须具备同样保障后才开放。权威依据为 specs/decisions/cloud/controller-integration/20260927-fenced-runtime-control-delivery.md。

## 个人模型执行

`AgentSessionSpec.model_binding_id` 是 Cloud 冻结的个人模型配置引用，不含密钥或临时令牌。
`RegisterNodeRequest.model_proxy` 与 `NodeRecord.model_proxy` 保存 Node 握手声明的模型代理能力，
旧 Node 默认为 false；个人模型会话派发和授权都要求具备该能力。语义见 [模型连接](../docs/model-connections.md)。
