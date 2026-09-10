# internal/config: 配置加载与校验

`internal/config` 负责 Ora Cloud 的配置解析、Schema 校验和环境变量覆盖。

## 职责

- **结构化配置定义**：定义强类型的 Go 结构体以映射全系统配置项：
  - `ServerConfig`：端口号、Gin 运行模式、读写超时时间。
  - `LoggerConfig`：日志级别、输出文件路径、轮转阈值（最大单文件体积、保留天数、备份数、gzip 压缩）。
  - `DatabaseConfig`：驱动类型（必须为 `postgres`）、连接串 DSN 以及连接池上限参数（`max_open_conns`、`max_idle_conns`、`conn_max_lifetime`）。
  - `AuthConfig`：预期的 token 受众和 `TrustedKey` 验证参数列表。
- **基于 Viper 的分层配置加载**：
  - 依次在 `./configs`、`../configs` 和 `.` 目录下检索 `config.yaml`。
  - 支持通过 `-config <path>` 显式指定配置文件路径。
  - 自动映射带有 `CLOUD_` 前缀的环境变量，将点号替换为下划线（例如 `CLOUD_DATABASE_DSN` 覆盖 `database.dsn`）。
- **启动期合理性校验**：对不合法的配置返回明确错误并拒绝启动；例如，`read_timeout`、`write_timeout` 和 `conn_max_lifetime` 必须是正时长。

## 边界与不变量

- **不存储机密信息**：配置文件只存储公开验证密钥和基础设施引用。明文部署机密信息和私钥绝不得出现在配置文件中。
- **运行时不可变**：配置在命令启动时加载一次，并作为就绪可用的值传递。不存在全局可变配置单例。

参见 [config.yaml](../../configs/config.yaml)、[cmd/server](../../cmd/server/README.md) 与 [认证配置与凭据](../../docs/authentication.md)。
