# internal/repository: PostgreSQL 连接与连接池管理

`internal/repository` 负责 Ora Cloud 的 PostgreSQL 数据库连接池初始化、参数配置与连通性校验。

## 职责

- **连接建立**：`InitDB` 使用 GORM 的 PostgreSQL 驱动根据配置的 DSN 建立数据库连接。
- **连接池参数调优**：从 `config.DatabaseConfig` 中读取并配置标准的连接池调优参数：
  - `SetMaxOpenConns`：限制最大并发打开的连接数。
  - `SetMaxIdleConns`：维持适当数量的空闲连接。
  - `SetConnMaxLifetime`：设置连接最大存活周期，以适应数据库服务端的连接清理策略。
- **快速失败（Fail-fast）健康检查**：在初始化期间执行 `pool.PingContext(ctx)`。若数据库无法连通，立即关闭连接池并返回包装了上下文信息的错误，杜绝半初始化状态的服务启动。
- **静默 GORM 内部日志**：将 GORM 内部日志记录器设置为静默（`logger.Silent`）。应用程序级别的运维与错误日志完全由 `internal/logger` 和请求生命周期中间件统一管理。

## 边界与不变量

- **仅限 PostgreSQL**：`cfg.Driver` 必须等于 `"postgres"`。不允许使用内存数据库、SQLite、MySQL 或模拟数据库层。
- **禁止修改 Schema**：该包**绝不**调用 GORM 的 `AutoMigrate`，也绝不发送任何 DDL 语句。Schema 结构定义与更新完全归属于 `internal/core/migrations` 与 `cloudctl migrate`。
- **无领域数据查询**：所有具体的数据访问逻辑与事务编排均归属于 `internal/core`。本包的职责仅限于向 `core.NewStore` 交付已完成健康探活的 `*gorm.DB` 实例。

参见 [核心仓储 Store](../core/README.md)、[数据库迁移目录](../core/migrations/README.md) 与 [配置管理](../config/README.md)。
