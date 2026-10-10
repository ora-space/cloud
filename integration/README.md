# integration: PostgreSQL 集成测试套件

[中文](README.md) | [English](README.en.md)

`integration` 包含 Ora Cloud 的自动化集成测试套件。它跨越真实边界测试完整的服务器栈：真实 HTTP 请求、权威 PostgreSQL 数据库约束、真实 Git 仓库操作和文件系统 I/O。

## 测试分类与覆盖范围

- **`cloud_test.go`**：端到端完整生命周期验证：
  - Project 与 Workspace 经 sandbox → node → clone 创建，真实 Git clone 到各 Workspace 自己的数据中，以及 `one_main` 约束执行。
  - Workspace 状态流转（`provisioning` $\rightarrow$ `ready` $\rightarrow$ `stopped` $\rightarrow$ `deleted`）。
  - Controller 租约获取、Epoch 栅栏隔离以及操作任务认领/推进。
  - 幂等性重放与冲突检测。
- **`workspace_lifecycle_test.go`**：各步骤只计划保留的 effect、clone 失败重试与结果未知时阻塞、Workspace 数据删除等待终止。
- **`workspace_operations_grpc_test.go`**：Controller 通过 `WorkspaceOperationService`／`NodeReportService` 驱动 Workspace、Node incarnation 替换以及 `OperationAvailable` 投递。
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

合入上游后的回归覆盖：`tenant_upstream_migration_test.go` 校验 0016 升级至 0017 不改写运行时、克隆或插件记录；`project_space_test.go` 验证非创建者新建运行时后可见且完成 Node 克隆；插件测试使用不同租户验证空间隔离，停用成员后立即拒绝读取。

## Revision 与业务钩子验收

`task test:revision` 强制真实 PostgreSQL 和 S3。设置 `TEST_S3_ENDPOINT`、`TEST_S3_ACCESS_KEY_FILE`、`TEST_S3_SECRET_KEY_FILE`，预建 `revisions` 桶；凭据仅为临时文件引用。设 `REQUIRE_S3=1` 时缺少存储立即失败。sandbox 用例还需 `TEST_S3_PUBLIC_ENDPOINT`、`TEST_S3_SANDBOX_NETWORK` 及 `REQUIRE_S3_SANDBOX=1`；集群配套 `task agent:acceptance` 自动配置这些环境。

Revision 测试直接覆盖真实上传、checksum/大小/缺失、授权过期刷新、外部校验后围栏、事务失败与重放、双替身重启及历史升级。业务钩子测试验证 ready/failed、Thread、会话结束、交付、删除与控制证据同提交/回滚。完整 `task test`、`task test:race` 也应启用这些强制环境，避免把跳过 S3 当作已验收。范围见[控制面报告](../docs/agent-run-control-plane-review.md)。

## 契约验证的测试作用域

同一测试进程只解析、验证和编译一次 OpenAPI 路由表，随后只读共享；每个 fixture 仍持有自己
的 HTTP transport、请求/响应、数据库 schema、认证密钥与 Git 数据。所有真实响应仍逐条严格
验证，不缓存验证结果。`contract_suite_test.go` 覆盖两个真实 fixture 的复用、并发读取、非法
响应拒绝和契约不可变性；此缓存仅存在于测试，不增加生产全局状态，也不放宽测试超时。
