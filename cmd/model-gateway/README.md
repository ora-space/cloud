# model-gateway：个人模型访问服务

[中文](README.md) | [English](README.en.md)

独立于 Cloud 业务进程运行的可信服务，组合 `internal/modelgateway`、Core 和 PostgreSQL。
读取 Cloud 配置中的数据库和 Gateway 公钥，仅以部署文件引用读取本服务 TLS 和加密主密钥。
服务启动先验证迁移与旧密文的加密身份，随后开放三个监听；停止时取消模型请求并等待全部服务退出。

| 环境变量 | 默认值 / 用途 |
|---|---|
| `MODEL_GATEWAY_CREDENTIAL_ADDR` | `:8083`，只接收 Gateway 凭据写入 |
| `MODEL_GATEWAY_RUNTIME_ADDR` | `:8443`，临时授权的模型数据 |
| `MODEL_GATEWAY_GRANT_ADDR` | `:8444`，Node 专用 mTLS 授权 |
| `MODEL_GATEWAY_PUBLIC_ORIGIN` | `https://ora-model-gateway:8443` |
| `MODEL_GATEWAY_CERTIFICATE_FILE` / `PRIVATE_KEY_FILE` / `CA_FILE` | `/etc/ora-model-management/` 的材料引用 |
| `MODEL_GATEWAY_MASTER_KEY_FILE` / `MASTER_KEY_ID` | `/etc/ora-model-secrets/master-key` / `v1` |

`-healthcheck` 正常验证 HTTPS 和公开 CA，只检查数据库就绪，不读取用户 Key 或主密钥。
public 模式的私网 fixture 例外需同时设置 `MODEL_GATEWAY_DEVELOPMENT=true` 和精确的
`MODEL_GATEWAY_DEVELOPMENT_ALLOW_HOSTS`；enterprise 模式按部署目标准入。
出网文件与 DNS 配置见 [共享出网配置](../../docs/operations/deployment-egress.md)，内部实现与测试见 [modelgateway](../../internal/modelgateway/README.md)。
