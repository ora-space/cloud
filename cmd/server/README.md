# cmd/server: Ora Cloud HTTP 守护进程

`cmd/server` 是 Ora Cloud 权威服务的主要生产环境入口。它负责绑定 HTTP 端点、校验数据库迁移的校验和、验证 JWT 凭据，并根据操作系统信号管理守护进程生命周期。

## 职责

- **配置与日志加载**：通过 `internal/config.Load` 加载应用程序配置（支持 YAML 文件与 `CLOUD_*` 环境变量覆盖），并初始化全进程 Zap 日志记录器。
- **PostgreSQL 连接池初始化**：通过 `internal/repository.InitDB` 建立与权威 PostgreSQL 数据库的连接，并执行强制 Ping 探活以验证连通性。
- **迁移完整性门禁**：在启动时执行 `store.CheckSchema(ctx)`。严格校验 `internal/core/migrations` 中定义的所有迁移都已按顺序应用且 SHA256 校验和匹配，同时检查数据库中不存在任何意外的迁移版本。此过程从不应用 DDL 或运行 `AutoMigrate`。
- **密码学信任配置**：使用配置的受信任验证公钥和预期受众字符串构建 `core.Authenticator`。
- **HTTP 服务装配**：通过 `internal/api/router.New` 初始化 Gin 引擎，设置服务端超时参数（`ReadHeaderTimeout`、`ReadTimeout`、`WriteTimeout`、`IdleTimeout`），并监听配置的 TCP 端口。
- **优雅停机**：捕获 `os.Interrupt` 和 `syscall.SIGTERM`。收到停机信号后，创建一个最长 10 秒的有界停机 context，以完成处理中的请求，然后关闭 PostgreSQL 连接池并刷新日志缓冲区。

## 边界与不变量

- **启动时不执行 DDL**：服务器运行时不会更改数据库 Schema。如果迁移缺失或被修改，服务器会立即停止启动并退出；Schema 更新必须使用 `cloudctl migrate` 执行。
- **无状态守护进程**：服务器进程不会在请求之间保留可变的内存业务状态。权威状态完全位于 PostgreSQL 中，并通过数据库级咨询锁协调并发访问。
- **零凭据泄漏**：服务器不处理或记录原始的外部部署密钥、云提供商机密信息或基础设施密码。

参见 [cmd 入口总览](../README.md)、[HTTP 路由网关](../../internal/api/router/README.md)、[认证配置与凭据](../../docs/authentication.md) 与 [核心不变量与契约](../../docs/core-contract.md)。
