# integration: PostgreSQL 集成测试套件

[中文](README.md) | [English](README.en.md)

`integration` 包含 Ora Cloud 的自动化集成测试套件。它跨越真实边界测试完整的服务器栈：真实 HTTP 请求、权威 PostgreSQL 数据库约束、真实 Git 仓库操作和文件系统 I/O。

## 测试分类与覆盖范围

- **`cloud_test.go`**：端到端完整生命周期验证：
  - 项目创建、存储分配和 Git 裸仓库克隆。
  - Linked worktree 创建与 `one_main` 约束执行。
  - Workspace 状态流转（`provisioning` $\rightarrow$ `ready` $\rightarrow$ `stopped` $\rightarrow$ `deleted`）。
  - Controller 租约获取、Epoch 栅栏隔离以及操作任务认领/推进。
  - 幂等性重放与冲突检测。
- **`endpoints_test.go`**：针对所有公开端点和内部控制端点进行全面的路由与参数矩阵测试。
- **`contract_test.go`**：对照实际 HTTP 响应数据结构，进行端到端 OpenAPI 契约验证。
- **`security_test.go`**：认证、授权与隔离测试：
  - 租户边界隔离与防跨租户数据泄漏校验。
  - 基于角色的访问控制（管理员 admin 与普通成员 member 权限区分）。
  - 双重凭据有效性验证及 `service.Subject == user.Caller` 严格绑定检查。

## 测试隔离性不变量

- **Schema 隔离**：每个测试都在独立、动态创建的 PostgreSQL Schema（`CREATE SCHEMA <unique_name>`）中执行。这些 Schema 会在 `t.Cleanup` 中彻底删除（`DROP SCHEMA <unique_name> CASCADE`），保证测试之间不共享可变数据库状态。
- **强制真实 PostgreSQL**：在 CI 环境下（`REQUIRE_POSTGRES=1`），若未配置 `TEST_DATABASE_URL`，测试将立即报错退出，绝不静默跳过。
- **并行执行与竞态检测**：测试经过设计，可在 `task test:race` 中使用 `go test -race` 安全运行，以验证并发不变量和加锁顺序。

## 运行集成测试

```powershell
# 通过 scripts/postgres.ps1 启动的本地 Windows PostgreSQL：
$env:TEST_DATABASE_URL='host=127.0.0.1 port=55432 user=postgres dbname=ora_test sslmode=disable'
task test:integration

# 带有竞态检测的完整测试门禁：
task test:race
```

参见 [本地验证](../README.md#本地验证)、[核心不变量与契约](../docs/core-contract.md) 与 [Taskfile.yml](../Taskfile.yml)。
