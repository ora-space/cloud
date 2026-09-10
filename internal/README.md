# internal: 权威云端子系统

`internal` 存放 Ora Cloud 的私有内部实现包。遵循 `AGENTS.md` 中记录的架构边界规范，所有核心业务状态、策略逻辑、协议转换与基础设施适配器均保持在 `internal/` 作用域内私有化。

## 模块概览

- [core](core/README.md)：权威领域核心，拥有业务状态机、事务边界、数据库级咨询锁以及密码学身份认证。
  - [migrations](core/migrations/README.md)：包含按顺序执行、仅向前的 PostgreSQL Schema 迁移脚本和校验和验证。
- [api](api/README.md)：HTTP 表现层与协议接入层。
  - [router](api/router/README.md)：绑定 HTTP 路由、验证两层 JWT 凭据、在大小限制下解析 JSON 请求体，并将领域错误转换为稳定契约。
- [contract](contract/README.md)：定义 OpenAPI 3.0 Schema 模型、DTO 结构体与契约覆盖率测试。
- [repository](repository/README.md)：基于 GORM 管理 PostgreSQL 连接池与启动时快速探活。
- [config](config/README.md)：加载并校验应用程序配置文件及环境变量覆盖。
- [logger](logger/README.md)：基于 Zap 和 Lumberjack 提供结构化、非阻塞的 JSON 日志记录。
- [simulator](simulator/README.md)：实现 Substrate 执行引擎、Controller 与 Workspace Node 的进程内替身。

## 分层与架构规则

1. **严格单向依赖**：
   - `cmd/*` $\rightarrow$ `internal/api/router`, `internal/core`, `internal/config`, `internal/logger`, `internal/repository`。
   - `internal/api/router` $\rightarrow$ `internal/core`, `internal/contract`。
   - `internal/core` $\rightarrow$ 标准库、`gorm.io/gorm`、`internal/core/migrations`。
   - `internal/repository` $\rightarrow$ `internal/config`, `gorm.io/gorm`。
   - 底层包（`core`、`repository`）严禁反向导入上层表现层包（`api`、`router`）。
2. **PostgreSQL 为唯一权威持久化**：
   - 所有共享业务状态必须持久化在 PostgreSQL 中。严禁跨请求在内存中缓存权威领域状态。
3. **事务边界约束**：
   - 数据库事务与咨询锁严格限制在 `core.Store.transact` 内的 PostgreSQL 操作中。**绝对禁止**跨外部 HTTP 请求、Git CLI 命令或文件系统 I/O 持有事务。
4. **错误处理**：
   - 面向客户端的公开错误统一使用稳定的 `Fault` 结构体（包含 `Code`、`Params`、`Status`）。内部 SQL 细节、错误堆栈与数据库异常仅记录于内部日志中，绝不对外暴露。

参见 [AGENTS.md](../AGENTS.md)、[核心不变量与契约](../docs/core-contract.md) 与 [认证配置与凭据](../docs/authentication.md)。
