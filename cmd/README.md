# cmd: 命令入口

`cmd` 包含 Ora Cloud 的命令行工具和守护进程入口。该目录下的包严格限于加载运行时配置、注入依赖、编排进程生命周期、处理操作系统信号和设置进程退出码。

## 模块概览

- [server](server/README.md)：核心的权威 HTTP API 服务守护进程。
- [cloudctl](cloudctl/README.md)：受限的运维与部署管理 CLI，用于数据库迁移、租户初始化引导以及凭据引用管理。
- [simulator](simulator/README.md)：基于进程内 Substrate 和 Git 执行替身提供一体化的本地演示环境。
- [openapi](openapi/README.md)：根据 Go 契约定义生成并同步权威 OpenAPI 3.0 规范（`api/openapi.json`）。
- [checkformat](checkformat/README.md)：作为严格的 CI 校验门禁，强制执行仓库 Go 代码格式化规范。

## 边界与不变量

- **无领域逻辑**：`cmd/*` 包不包含领域策略、状态转换算法或事务逻辑。所有领域行为都属于 `internal/core`。
- **无直接数据库查询**：各命令只能通过 `internal/repository` 获取数据库连接池，并将其直接交给 `internal/core.NewStore`。`cmd/*` 中不得出现原生 SQL、GORM 模型或数据库查询。
- **退出时清理资源**：进程终止时必须刷新日志缓冲区（`logger.Sync`）、关闭数据库连接池（`store.Pool.Close()`），并取消后台 context。
- **启动时快速失败**：如果加载配置、验证 Schema 校验和、检查数据库连通性或验证密码学信任失败，命令会立即以非零状态码退出。

参见 [根目录总览](../README.md)、[内部子系统](../internal/README.md) 与 [AGENTS.md](../AGENTS.md)。
