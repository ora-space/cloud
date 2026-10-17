# internal/simulator: 开发与测试执行替身

[中文](README.md) | [English](README.en.md)

`internal/simulator` 为 Ora Cloud 阶段一的开发与验收测试提供基于进程内与本地磁盘的执行替身（Execution Doubles），涵盖 Controller、Workspace Node 以及 Substrate 存储系统。

## 职责

### Substrate 执行替身 (`substrate.go`)
- 通过本地 HTTP 模拟外部存储交互与 Effect 日志执行。
- 在本地磁盘管理 Effect 执行日志 JSON 文件（`<root>/effects/<effect-id>.json`）。
- 执行模拟基础设施操作：
  - **沙箱模拟（Sandbox）**：模拟 Sandbox 实例的分配与终止；sandbox_ensure 挂载该 Workspace 自己的数据（`<root>/workspaces/<workspace-id>/home`）并返回 Node 的 `nodeId`。
  - **Workspace 数据**：`workspace_data_delete` 删除单个 Workspace 的数据目录。
  - **Node clone**（`node.go`）：`PUT/GET /clones/<execution-id>` 模拟 desktop Node 从 Git Server 替身把快照 ref 取进不带任何 remote 的 `home/checkout`，核对取到的提交等于 `baseCommit`，再以 `-B <branch>` 检出 Workspace 分支的短名，按 execution 落日志；从不访问外部仓库。`NewSubstrate(root, git *GitServer)` 注入 Git Server 替身。
- 支持确定性故障注入（`SetFault`），用于验证错误恢复与重试策略。

### Git Server 替身 (`gitserver.go`)
- `GitServer` 在磁盘上按 Git Server 卷的布局保存 Project 仓库，仓库 base URL 为 `https://git-server.simulator.invalid:8445`（无人监听，替身把它映射到磁盘）；不执行 ref 规则。
- `Config()` 交给 Store 作为 `core.GitServer`，Cloud 的 ref 查询直接从磁盘回答。
- `SeedSnapshot` 经 git-server-preset 夹具代替外部线登记 Project 的快照。

### Controller 执行替身 (`controller.go`)
- 模拟外部控制平面工作进程：
  - 定期通过 `/internal/v1/controller-lease/acquire` 获取并续约 Controller 独占租约。
  - 通过 `/internal/v1/operations/claim` 认领待处理的操作任务。
  - 针对 Substrate 规划并执行所需的 Effect。
  - 通过 `/internal/v1/operations/{oid}/effects/{eid}/result` 上报 Effect 执行结果。
  - 在严格的单调递增 Epoch 栅栏保护下推进或延期操作。
  - 通过 gRPC `ExecutionService` 驱动 clone 步骤（`clone.go`）：原样转发 Cloud 冻结的 `cloneInput` 登记 execution、交给模拟 Node 执行、登记查询结果，失败时以 `clone_failed` 延期到 retry_wait；推进后 operation 不再 running（已停放或失败）时放下它。

### Agent 会话替身 (`agent.go`)

Controller 每次有界 Step 都领取运行工作、投递 Thread 命令并接管 Node 证据，避免 quiesce 饿死结束命令。`AgentNode` 用磁盘日志固定输入、去重 command_id，回显并结束会话；交付先把最终提交推送到 Git Server 替身上的交付 ref（changed 与 unchanged 都推送），再尝试快进 `workspaceRef`，然后只上传封存的 JSONL：取得 checksum 签名头后真实 PUT，授权过期则刷新同键。终态按 protojson 写日志，上传授权不落盘；替换 Controller/Node 后重放原证据。真实 PostgreSQL + S3 测试证明 A 闭环，不能替代生产 Rust relay 或 D 真实 Agent 验收。

上传携带签名的 `If-None-Match: *`，不能覆盖首次对象。回复丢失后的 412 保留对象并把声明交给 Cloud 核对，不能仅凭 412 推断摘要相同。Controller 取消会返回取消原因且不写交付终态，重启后仍可继续原执行；实际 Node 的终态失败继续持久化。

首个 PUT 前，Node 在日志根目录内持久固定 JSONL 字节和元数据；部分上传后重启复用原内容，不重新生成带新时间戳的快照。终态证据原子写入后清理对象副本，此后重放原终态。旧 pending 日志中的 `Result: null` 按未终结处理；新日志省略空结果。授权从不写入准备计划或终态日志。

### 临时凭据签发器
- `NewCredentials()` 在内存中为四种不同的角色（`gateway`、`controller`、`node`、`user`）生成 Ed25519 密码学密钥对。
- 在模拟器测试运行期间按需签发短期 JWT token；其密码学 token 结构与生产环境一致，但不需要外部身份认证基础设施。

## 边界与不变量

- **仅限开发与测试**：本包属于工程执行替身。绝对禁止部署到生产环境，生产环境守护进程二进制也绝不导入此包。
- **契约保真度**：模拟器严格通过标准 HTTP API 与 Cloud 核心服务器交互，并遵守所有租约、栅栏和幂等契约。

参见 [cmd/simulator 工具](../../cmd/simulator/README.md)、[执行契约与边界](../../docs/execution-contract.md) 与 [集成测试套件](../../integration/README.md)。
