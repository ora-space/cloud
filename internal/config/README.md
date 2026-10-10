# internal/config: 配置加载与校验

[中文](README.md) | [English](README.en.md)

`internal/config` 负责 Ora Cloud 的配置解析、Schema 校验和环境变量覆盖。

## 职责

- **结构化配置定义**：定义强类型的 Go 结构体以映射全系统配置项：
  - `ServerConfig`：端口号、Gin 运行模式、读写超时时间。
  - `LoggerConfig`：日志级别、输出文件路径、轮转阈值（最大单文件体积、保留天数、备份数、gzip 压缩）。
  - `DatabaseConfig`：驱动类型（必须为 `postgres`）、连接串 DSN 以及连接池上限参数（`max_open_conns`、`max_idle_conns`、`conn_max_lifetime`）。
  - `AuthConfig`：预期的 token 受众和 `TrustedKey` 验证参数列表。
  - `ObjectStoreConfig`：可选的 S3 交付配置。
  - `CredentialIngressConfig`：Cloud 侧 `ingress-writer` 客户端（endpoint、证书/私钥/CA 文件路径、超时）。
  - `CredentialStoreServiceConfig`：独立凭据服务进程自己的配置（`listen`、`volume`、`kek_file`、服务证书、客户端 CA、三个 SPIFFE 身份、旧版本保留期），由 `LoadCredentialStoreConfig` 用 `CREDENTIAL_STORE_*` 前缀加载。
- **基于 Viper 的分层配置加载**：
  - 依次在 `./configs`、`../configs` 和 `.` 目录下检索 `config.yaml`。
  - 支持通过 `-config <path>` 显式指定配置文件路径。
  - 自动映射带有 `CLOUD_` 前缀的环境变量，将点号替换为下划线（例如 `CLOUD_DATABASE_DSN` 覆盖 `database.dsn`）。
- **启动期合理性校验**：对不合法的配置返回明确错误并拒绝启动；例如，`read_timeout`、`write_timeout` 和 `conn_max_lifetime` 必须是正时长。

## 边界与不变量

`object_store` 为可选的 S3 交付配置。`enabled=true` 时 server 在启动阶段读取独立挂载的 `access_key_id_file` 与 `secret_access_key_file`，只保存在进程内存，检查私网 endpoint 与 Node 可达的 public_endpoint、region、bucket、path_style 和 1 秒至 7 天的 upload_grant_ttl。每个字段可由 `CLOUD_OBJECT_STORE_*` 覆盖。短期 PUT 授权可绑定 SHA-256；Cloud 经私网 HEAD 验证存在、大小与存储摘要后登记 Revision。默认关闭：不派发交付工作，以同事务业务钩子结算 skipped/object_store_unconfigured；独立授权请求返回不可用。健康信息报告 configured/unconfigured，不检查 S3 在线状态。

`credentials` 是 Cloud 侧凭据入口的**可选**一段（`credentials.enabled`、`credentials.endpoint`、`credentials.certificate_file`/`private_key_file`/`ca_file`、`credentials.timeout`）。关闭时不产生客户端，凭据写入一律以 `credential_store_unavailable` 拒绝，这正是 approved 的 fail-closed 状态：没有能保存它的服务，就不允许存在凭据，也绝不把原始 Token 写进 Cloud 自己的库作为替代。开启但配置不完整或不可用则在启动时报错（D6 要求"配置了却建立不起来的能力"启动失败，而不是每个请求失败）。证书与私钥只以文件路径给出，绝不写进配置文件。

`credential-store` 段属于**独立进程**：`LoadCredentialStoreConfig` 只认 `CREDENTIAL_STORE_*` 前缀，Cloud 无法设置它的监听地址或卷，它也无法继承 Cloud 的值；`Store()`/`ServerTLS()`/`Identities()` 组成 D6/D8 的启动闸门——卷必须存在且不可被其他本机账户读取、KEK 必须由部署交付且可用、服务证书/私钥/客户端 CA 齐备、三个身份互不相同，缺一即拒绝启动；监听始终要求并校验客户端证书（TLS 1.3 起）。

- **不存储机密信息**：配置文件只存储公开验证密钥和基础设施引用。明文部署机密信息和私钥绝不得出现在配置文件中。
- **运行时不可变**：配置在命令启动时加载一次，并作为就绪可用的值传递。不存在全局可变配置单例。

参见 [config.yaml](../../configs/config.yaml)、[cmd/server](../../cmd/server/README.md) 与 [认证配置与凭据](../../docs/authentication.md)。
