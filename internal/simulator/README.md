# internal/simulator: 开发与测试执行替身

`internal/simulator` 为 Ora Cloud 阶段一的开发与验收测试提供基于进程内与本地磁盘的执行替身（Execution Doubles），涵盖 Controller、Workspace Node 以及 Substrate 存储系统。

## 职责

### Substrate 执行替身 (`substrate.go`)
- 通过本地 HTTP 模拟外部存储交互与 Effect 日志执行。
- 在本地磁盘管理 Effect 执行日志 JSON 文件（`<root>/effects/<effect-id>.json`）。
- 执行模拟基础设施操作：
  - **存储管理（Storage）**：准备本地项目目录（`<root>/projects/<project-id>`）。
  - **工作区检出（Worktree）**：调用本地 Git CLI 执行真实的 Git Clone 与 Worktree 检出操作。
  - **沙箱模拟（Sandbox）**：模拟 Sandbox 实例的分配与终止生命周期。
- 支持确定性故障注入（`SetFault`），用于验证错误恢复与重试策略。

### Controller 执行替身 (`controller.go`)
- 模拟外部控制平面工作进程：
  - 定期通过 `/internal/v1/controller-lease/acquire` 获取并续约 Controller 独占租约。
  - 通过 `/internal/v1/operations/claim` 认领待处理的操作任务。
  - 针对 Substrate 规划并执行所需的 Effect。
  - 通过 `/internal/v1/operations/{oid}/effects/{eid}/result` 上报 Effect 执行结果。
  - 在严格的单调递增 Epoch 栅栏保护下推进或延期操作。

### 临时凭据签发器
- `NewCredentials()` 在内存中为四种不同的角色（`gateway`、`controller`、`node`、`user`）生成 Ed25519 密码学密钥对。
- 在模拟器测试运行期间按需签发短期 JWT token；其密码学 token 结构与生产环境一致，但不需要外部身份认证基础设施。

## 边界与不变量

- **仅限开发与测试**：本包属于工程执行替身。绝对禁止部署到生产环境，生产环境守护进程二进制也绝不导入此包。
- **契约保真度**：模拟器严格通过标准 HTTP API 与 Cloud 核心服务器交互，并遵守所有租约、栅栏和幂等契约。

参见 [cmd/simulator 工具](../../cmd/simulator/README.md)、[执行契约与边界](../../docs/execution-contract.md) 与 [集成测试套件](../../integration/README.md)。
