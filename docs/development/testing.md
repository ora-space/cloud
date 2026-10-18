# 测试与门禁

[English](testing.en.md) | 中文

Cloud 的正确性主要来自真实边界上的测试：真实的 PostgreSQL 约束和事务、真实的 HTTP、真实的 Git 与磁盘，必要时还有真实的 S3。替身
可以覆盖故障注入，但不能代替跨越真实边界的测试。本文说明测试的分层、运行方式和合并前的门禁；编写测试的规则以仓库根目录的
`AGENTS.md` 为准。

## 分层

| 层 | 位置 | 依赖 | 运行 |
|---|---|---|---|
| 单元测试 | `internal/...`、`cmd/...` 里不需要数据库的测试 | 无 | `task test:unit` |
| core 数据库测试 | `internal/core/*_db_test.go` 等 | 真实 PostgreSQL | `task test` |
| 集成测试 | `integration/` | 真实 HTTP、PostgreSQL、磁盘与 Git | `task test:integration` |
| Revision 与对象存储测试 | `integration/` 中的 Revision、插件与钩子用例 | 真实 PostgreSQL 与 S3 | `task test:revision` |
| 前端测试 | `frontend/` | Node | `task frontend:test`、`task frontend:check` |
| 跨仓库验收 | ora-space/cluster | 真实 Controller、Sandbox Server、Node | cluster 的验收任务 |

集成测试从路由开始驱动完整的服务器栈：Workspace 生命周期、Controller 的 gRPC 契约、所有公开与内部端点的路由和参数矩阵、认证与隔离，以及
用 `api/openapi.json` 逐条校验真实 HTTP 响应的契约测试。各测试文件的覆盖范围见 [`integration/`](../../integration/README.md)。

## 数据库

- 测试通过 `TEST_DATABASE_URL` 连接 PostgreSQL；`task setup` 会把它写进 `.local/dev.env`。账号需要 `CREATE SCHEMA` 权限。
- **每个测试一个 schema**：测试为自己建一个独立的 schema，在里面执行迁移，并在 `t.Cleanup` 里删除，测试之间不共享可变的数据库状态。
- **不能静默跳过**：`task test`、`task test:integration`、`task test:race`、`task check` 都设置 `REQUIRE_POSTGRES=1`，没有配置数据库时直接
  失败。直接运行 `go test ./...` 而没有配置数据库时，需要数据库的测试会显式跳过，此时的通过不能当作完整验证。

## S3

`task test:revision` 设置 `REQUIRE_S3=1`，强制使用真实的 S3 兼容存储：

| 环境变量 | 说明 |
|---|---|
| `TEST_S3_ENDPOINT` | 存储地址；需要预先建好 `revisions` 桶 |
| `TEST_S3_ACCESS_KEY_FILE`、`TEST_S3_SECRET_KEY_FILE` | 凭据文件，只用临时文件 |
| `TEST_S3_PUBLIC_ENDPOINT`、`TEST_S3_SANDBOX_NETWORK`，加 `REQUIRE_S3_SANDBOX=1` | 沙盒网络内上传的用例 |

这组测试覆盖真实上传、校验和与大小、对象缺失、授权过期与刷新、外部核验之后的复核、事务失败与重放、替身重启，以及历史升级。CI 的 Backend
工作流用固定版本的 RustFS 运行它们；cluster 的 `task agent:acceptance` 会自动配置这些环境。完整运行 `task test`、`task test:race` 时也应该
启用这些环境，避免把跳过 S3 当作已经验证。

## 门禁

| 任务 | 内容 | 什么时候必须通过 |
|---|---|---|
| `task format` / `task format:check` | gofumpt 与 goimports（生成的 `internal/controlpb` 除外） | 每次提交 |
| `task lint` | golangci-lint，规则在 `.golangci.yml` | 每次提交 |
| `task proto:lint`、`task proto:breaking`、`task proto:check` | proto 规则、对 `PROTO_BASE_REF`（默认 `main`）的兼容性、生成物漂移 | 修改 `proto/` 时 |
| `task check` | `format:check`、`lint`、`proto:lint`、`proto:check` 与完整的 `task test` | 行为变化或仓库范围的修改 |
| `task test:race` | 带 race 检测的完整测试；Windows 需要可用的 C 编译器 | 修改事务、租约、围栏、幂等、恢复、共享缓存或 goroutine 时 |
| `task build` | 构建所有命令 | 修改命令的装配、启动或停止时 |
| `task frontend:check` | 生成客户端与契约一致，加格式、lint、类型、覆盖率、模块文档与测试、死代码、重复代码、构建 | 修改前端、`api/` 或 `internal/contract/` 时 |
| `task frontend:check:diff` | 自 `BASE`（默认 `origin/main`）以来改动的前端模块必须同时更新 README 和测试 | 前端 PR |

迭代时先跑最小的相关任务，完成之前再跑完整门禁。不能为了让门禁通过而放宽 lint、跳过测试、调大超时或削弱断言与安全检查；需要例外时，在尽可能
窄的抑制旁写明经批准的原因。

## 编写测试的要点

- 每个行为变化都要加或改测试。真正的输入矩阵用表驱动；尽量比较完整的值；失败信息要说清是哪个用例、违反了哪条不变量。
- 测试要确定、封闭：使用独立的 schema 和临时目录，立即注册清理；不用随意的 sleep、墙钟假设、测试顺序依赖或可变的进程全局配置。
- 只有确认测试和它的辅助函数不共享 schema、环境变量、端口、文件路径、日志或其他可变状态时，才使用 `t.Parallel`。并发行为要同步起跑，并断言
  全部允许的结果，而不是只接受最常见的那一个。
- 测试用的密钥只存在于进程内存或临时路径，不进入日志或持久的 fixture；真实用户的会话日志不能作为 fixture。

## 与 specs 核心用例的关系

specs 的 `test-cases/` 为每个领域列出核心用例，并逐项记录证据状态（Covered、Partial、Missing）。一个行为对应某个核心用例时，保持它引用的
测试路径和锚点准确；只有当一个低层测试失败就直接说明该义务被破坏时，它才算这个用例的证据。

## 相关决策

- [Cloud 拥有 Internal Control gRPC 契约并承担权威持久化](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/0-cloud-owned-internal-grpc-contract.md)
- [Cloud 核心用例索引](https://github.com/ora-space/specs/blob/main/test-cases/cloud/README.md)
