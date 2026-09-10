# cmd/simulator: 端到端本地执行模拟器

`cmd/simulator` 为 Ora Cloud 第一阶段架构提供完整的本地演示环境。它启动进程内执行替身、临时 Git 仓库夹具和回环 HTTP 服务，无需外部云基础设施即可验证完整的项目生命周期。

## 职责

- **持久化演示夹具**：在 `.local/demo/fixture` 下初始化真实的本地 Git 仓库测试夹具，并包含初始提交与分支。
- **进程内 Cloud 服务**：拉起基于真实 PostgreSQL 数据库的临时 `httptest.Server`，提供完整的 Gin 路由服务。
- **Substrate 模拟**：在 `.local/demo/substrate` 下启动本地 HTTP 服务，提供 Substrate 存储和 Effect 模拟。
- **临时密钥材料**：在内存中为 `gateway`、`controller`、`node` 和 `user` 四种角色生成 Ed25519 密钥对，以便在不依赖外部 IdP 基础设施的情况下签发和验证短期 JWT token。
- **Controller 驱动执行主循环**：
  1. 引导配置演示租户和用户。
  2. 通过 `/internal/v1/controller-lease/acquire` 获取 Controller 独占租约。
  3. 通过公开 API 发起项目创建请求（`POST /api/v1/tenants/{tid}/projects`）。
  4. 模拟 Controller 清空队列：执行 Effect 计划（分配项目存储、创建 Git worktree、调度沙箱并注册节点）。
  5. 验证 Workspace 成功达到 `ready` 就绪状态，并通过公开 API 查询校验。
  6. 正常释放 Controller 租约。
  7. 向 `stdout` 输出 JSON 摘要。

## 边界与不变量

- **仅限测试和演示**：模拟器是工程执行替身。它不与 Kubernetes 交互、不部署真实容器沙箱，也不启动实际的 Deno 或 agent 运行时。
- **保留真实边界**：使用真实的 HTTP 协议封装、PostgreSQL Schema 约束和本地 Git CLI 操作，不绕过领域状态机。

参见 [cmd 入口总览](../README.md)、[模拟器内部实现](../../internal/simulator/README.md) 与 [执行契约与边界](../../docs/execution-contract.md)。
