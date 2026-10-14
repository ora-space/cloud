# cmd/credential-store: 独立凭据服务进程

[中文](README.md) | [English](README.en.md)

`cmd/credential-store` 运行独立的 Credential Service。它是**与 Cloud 分开的部署单元**:独立进程、独立卷、
独立密钥材料、独立信任根(Secret Store D1)。除了经过双向认证的入口,Cloud 拿不到它持有的任何东西;
它与 Cloud 业务库不共享数据库、连接、卷或凭据(`cmd/credential-store` 不连接任何 PostgreSQL)。

## 职责

- **配置**:`config.LoadCredentialStoreConfig` 读取本服务自己的配置文件,只认 `CREDENTIAL_STORE_*`
  环境前缀,因此 Cloud 的配置无法设置本服务的监听地址或卷,本服务也不会继承 Cloud 的值。
- **启动闸门**:在监听端口之前完成 `CredentialStoreServiceConfig.Store()`(卷必须存在且不可被其他本机
  账户读取;KEK 必须由部署交付且可用)、`ServerTLS()`(服务证书、私钥与客户端 CA,始终
  `RequireAndVerifyClientCert`、TLS 1.3 起)与 `credentialstore.NewServer`(三个固定身份必须存在且互不相同)。
  D6/D8 要求"没有可用 KEK"是启动拒绝,绝不能表现为"空存储"。
- **审计日志**:D7 把每次操作当作审计记录,因此没有日志目的地(既未启用控制台又未配置文件)时进程拒绝启动,
  而不是静默丢弃;日志只写操作、凭据 id、服务身份与结果,从不写 Token、密文、被封装 DEK 或 KEK。
- **生命周期**:`signal.NotifyContext` 处理 `SIGINT`/`SIGTERM`,`http.Server.Shutdown` 在 10 秒内优雅退出,
  日志缓冲区在退出时 `logger.Sync`。

## 不变量

- **不含领域逻辑**:进程只做配置、装配、生命周期与退出码。顺序、幂等、保留窗口与失败映射都在
  `internal/credentialstore`;身份表与启动校验在 `internal/config`。
- **监听即双向认证**:`tls.NewListener` 直接包住监听器并强制客户端证书,不存在匿名请求能到达处理器的路径。
- **只记录位置,不记录秘密**:启动日志只写监听地址、卷路径与 KEK 标识符(`keyId`),三者都是位置而不是秘密。

## 配置

`configs/credential-store.yaml` 或 `CREDENTIAL_STORE_*` 环境变量,字段见
`internal/config.CredentialStoreServiceConfig`:`listen`、`volume`、`kek_file`、`certificate_file`、
`private_key_file`、`client_ca_file`、`ingress_writer_identity`、`broker_resolver_identity`、
`kek_operator_identity`、`superseded_retention`(0 取默认 1 小时)与 `logger`。证书与 KEK 一律以**文件
路径**给出,绝不写进配置文件或命令行。

参见 [cmd 入口总览](../README.md)、[凭据服务包](../../internal/credentialstore/README.md) 与
[配置加载](../../internal/config/README.md)。
