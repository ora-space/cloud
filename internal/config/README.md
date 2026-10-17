# internal/config: 配置加载与校验

[中文](README.md) | [English](README.en.md)

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

启动窗口使用数据库时间，确认耗尽后业务结算并保留独立残留责任，不能以业务终态声称物理释放。配置默认值与恢复边界见 [启动结算](../../docs/agent-startup-settlement.md)。

`object_store` 为可选的 S3 交付配置。`enabled=true` 时 server 在启动阶段读取独立挂载的 `access_key_id_file` 与 `secret_access_key_file`，只保存在进程内存，检查私网 endpoint 与 Node 可达的 public_endpoint、region、bucket、path_style 和 1 秒至 7 天的 upload_grant_ttl。每个字段可由 `CLOUD_OBJECT_STORE_*` 覆盖。短期 PUT 授权可绑定 SHA-256；Cloud 经私网 HEAD 验证存在、大小与存储摘要后登记 Revision。默认关闭：不派发交付工作，以同事务业务钩子结算 skipped/object_store_unconfigured；独立授权请求返回不可用。健康信息报告 configured/unconfigured，不检查 S3 在线状态。

`git_server` 为可选的 Git Server 配置：`repository_base_url`（沙盒可达的仓库 base，https，不带 userinfo、query、fragment 或末尾斜杠；Project 仓库为 `<base>/<tenantId>/<projectId>.git`，写入 clone、session 与交付输入）、`ref_query_url`（ref 查询监听，`https://host[:port]`）、`ca_file`、`client_cert_file`、`client_key_file`（Cloud 自己的客户端证书，私钥只作为文件引用）与 `ref_query_timeout`（默认 10s）。每个字段可由 `CLOUD_GIT_SERVER_*` 覆盖。设置了 `repository_base_url` 时其余四个文件/地址字段必填；启动只校验并加载文件，不访问网络，Git Server 不可达只让核验暂时不可用。`repository_base_url` 为空即没有 Git Server：clone 步骤 blocked/git_server_unconfigured，交付结算 skipped/git_server_unconfigured。健康信息报告 `gitServer` 的 configured/unconfigured。

- **不存储机密信息**：配置文件只存储公开验证密钥和基础设施引用。明文部署机密信息和私钥绝不得出现在配置文件中。
- **运行时不可变**：配置在命令启动时加载一次，并作为就绪可用的值传递。不存在全局可变配置单例。

参见 [config.yaml](../../configs/config.yaml)、[cmd/server](../../cmd/server/README.md) 与 [认证配置与凭据](../../docs/authentication.md)。

Agent 启动、恢复和结束确认默认分别为 2m/6m/5m（agent_start_timeout、agent_restore_timeout、agent_end_confirm_timeout）；省略才选默认，显式非正/非法时长拒绝。配置只影响新进入的持久窗口，已开始的窗口不续期。
