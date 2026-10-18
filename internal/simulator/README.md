# internal/simulator: 开发与测试执行替身

[中文](README.md) | [English](README.en.md)

`internal/simulator` 为 Ora Cloud 的开发与集成测试提供基于进程内与本地磁盘的执行替身（Execution Doubles），涵盖 Controller、Workspace Node 以及 Substrate。

## 职责

### Substrate 执行替身 (`substrate.go`)
- 通过本地 HTTP 模拟外部存储交互与 Effect 日志执行。
- 在本地磁盘管理 Effect 执行日志 JSON 文件（`<root>/effects/<effect-id>.json`）。
- 执行模拟基础设施操作：
  - **沙箱模拟（Sandbox）**：模拟 Sandbox 实例的分配与终止；sandbox_ensure 挂载该 Workspace 自己的数据（`<root>/workspaces/<workspace-id>/home`）并返回 Node 的 `nodeId`。
  - **Workspace 数据**：`workspace_data_delete` 删除单个 Workspace 的数据目录。
  - **Node clone**（`node.go`）：`PUT/GET /clones/<execution-id>` 模拟 desktop Node 用本地 Git CLI clone 到 `home/checkout`，按 execution 落日志。
- 支持确定性故障注入（`SetFault`），用于验证错误恢复与重试策略。

#### HTTP 接口

Substrate 替身只监听 loopback，供模拟器和测试使用，不是生产 Substrate 的接口定义：

| 方法与路径 | 请求 | 返回与不变量 |
|---|---|---|
| `GET /effects/{effectId}` | Cloud 事先分配的 effect UUID | `404` 表示确知没有这个 intent；否则返回原请求、`externalId`、`state` 与 `result`；查询从不创建 effect |
| `PUT /effects/{effectId}` | `kind`、`projectId`、`workspaceId`，terminate 还带 `sandboxInstanceId` | 先写日志再执行；同 ID 同请求幂等，同 ID 不同请求 `409`；已成功的 effect 直接返回原结果 |
| `PUT /clones/{executionId}`、`GET /clones/{executionId}` | `workspaceId`、`repositoryUrl`、`branch` | 模拟 Node 执行 clone：同一 execution 只执行一次并按 execution 写日志；返回 `clone_ready` 与 `commitId`，或 `clone_failed` 与原因 |

| `kind` | 结果 |
|---|---|
| `sandbox_ensure` | `sandboxInstanceId`、`nodeId`；沙盒已终止时失败 |
| `sandbox_terminate` | `terminated`；沙盒必须属于请求里的 Workspace |
| `workspace_data_delete` | `removed` |

`SetFault` 的模式：`fail` 与 `unconfirmed` 写下 `running` 日志后返回 `503`，`lose_response` 执行成功但返回 `504`，用于验证"结果不明时按稳定身份查询"；
clone 的 `fail` 返回 `clone_failed`。仓库只能来自显式的 repository URL → 本地 fixture 映射，不接入真实的 Git 凭据。插件安装已改为 Node execution，
Cloud 拒绝计划插件 effect，替身里残留的 `plugin_ensure` 分支不会被 Cloud 触发。

### Controller 执行替身 (`controller.go`)
- 模拟外部控制平面工作进程。租约、操作与 Node 注册走已退役的 `/internal/v1/...` JSON 接口，只有 `router.NewDevelopment` 打开它们，生产 server 一律返回
  `410`；execution 与 Agent 运行走 gRPC：
  - 定期通过 `/internal/v1/controller-lease/acquire` 获取并续约 Controller 独占租约。
  - 通过 `/internal/v1/operations/claim` 认领待处理的操作任务。
  - 针对 Substrate 规划并执行所需的 Effect。
  - 通过 `/internal/v1/operations/{oid}/effects/{eid}/result` 上报 Effect 执行结果。
  - 在严格的单调递增 Epoch 栅栏保护下推进或延期操作。
  - 通过 gRPC `ExecutionService` 驱动 clone 步骤（`clone.go`）：登记 execution、交给模拟 Node 执行、登记查询结果，失败时以 `clone_failed` 延期到 retry_wait。

### Agent 会话替身 (`agent.go`)

Controller 每次有界 Step 都领取运行工作、投递 Thread 命令并接管 Node 证据，避免 quiesce 饿死结束命令。`AgentNode` 用磁盘日志固定输入、去重 command_id，回显并结束会话；交付从封存会话和真实 Git checkout 创建累计 bundle/JSONL，取得 checksum 签名头后真实 PUT，授权过期则刷新同键。终态按 protojson 写日志，上传授权不落盘；替换 Controller/Node 后重放原证据。真实 PostgreSQL + S3 测试证明 A 闭环，不能替代生产 Rust relay 或 D 真实 Agent 验收。

上传携带签名的 `If-None-Match: *`，不能覆盖首次对象。回复丢失后的 412 保留对象并把声明交给 Cloud 核对，不能仅凭 412 推断摘要相同。Controller 取消会返回取消原因且不写交付终态，重启后仍可继续原执行；实际 Node 的终态失败继续持久化。

首个 PUT 前，Node 在日志根目录内持久固定 bundle/JSONL 字节和元数据；部分上传后重启复用原内容，不重新生成带新时间戳的快照。终态证据原子写入后清理对象副本，此后重放原终态。旧 pending 日志中的 `Result: null` 按未终结处理；新日志省略空结果。授权从不写入准备计划或终态日志。

### 临时凭据签发器
- `NewCredentials()` 在内存中为四种不同的角色（`gateway`、`controller`、`node`、`user`）生成 Ed25519 密码学密钥对。
- 在模拟器测试运行期间按需签发短期 JWT token；其密码学 token 结构与生产环境一致，但不需要外部身份认证基础设施。

## 边界与不变量

- **仅限开发与测试**：本包属于工程执行替身。绝对禁止部署到生产环境，生产环境守护进程二进制也绝不导入此包。
- **契约保真度**：模拟器严格通过标准 HTTP API 与 Cloud 核心服务器交互，并遵守所有租约、栅栏和幂等契约。

参见 [cmd/simulator 工具](../../cmd/simulator/README.md)、[Workspace 生命周期](../../docs/runtime/workspaces.md) 与 [集成测试套件](../../integration/README.md)。
