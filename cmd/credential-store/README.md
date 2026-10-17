# credential-store：独立的 Credential Store

[中文](README.md) | [English](README.en.md)

独立于 Cloud 运行，组合 [`internal/credentialstore`](../../internal/credentialstore/README.md)（`proposed` 的 ADR
“Git 凭据由独立的 Credential Store 保管”）。不连接 Cloud 的数据库，只读取自己的卷、只读 keyring、TLS 文件和 Cloud 的两组
验证公钥。启动时 keyring 缺失或无效、公钥无法加载、两个监听信任同一身份、或地址重复都会拒绝启动；监听全部绑定成功后才开始
服务。停止时（SIGINT/SIGTERM）排空四个监听，最多 10 秒。启动时和之后每 5 分钟清扫一次超过 1 小时保留期的旧版本密文与
过期的认领文件。

| 环境变量 | 默认值 / 用途 |
|---|---|
| `CREDENTIAL_STORE_ROOT` | 必填，绝对路径；凭据卷，权限必须为 `0700` |
| `CREDENTIAL_STORE_KEYRING_FILE` | 必填；部署挂载的只读 KEK keyring |
| `CREDENTIAL_STORE_CERT_FILE` / `KEY_FILE` | 必填；三个 mTLS 监听的服务证书 |
| `CREDENTIAL_STORE_CLIENT_CA_FILE` | 必填；签发执行器、凭据管理与 KEK 运维客户端证书的 CA |
| `CREDENTIAL_STORE_EXECUTION_PUBLIC_KEYS_FILE` | 必填；Cloud 执行授权的 Ed25519 公钥集合（PEM） |
| `CREDENTIAL_STORE_SUBMISSION_PUBLIC_KEYS_FILE` | 必填；Cloud 提交授权的公钥集合，必须与执行公钥不同 |
| `CREDENTIAL_STORE_REDEMPTION_ADDR` | `:8461`，兑换监听，只接入执行器所在的内部网络 |
| `CREDENTIAL_STORE_MANAGEMENT_ADDR` | `:8462`，管理监听，只供 Cloud |
| `CREDENTIAL_STORE_KEY_ADDR` | `:8463`，密钥监听，只供 KEK 运维 |
| `CREDENTIAL_STORE_INGRESS_ADDR` | `:8460`，浏览器入口，由边缘反向代理直接转发 |
| `CREDENTIAL_STORE_INGRESS_CERT_FILE` / `INGRESS_KEY_FILE` | 必填；浏览器入口 origin 的证书 |
| `CREDENTIAL_STORE_INGRESS_ALLOWED_ORIGINS` | 必填；逗号分隔的前端 origin（`https://`，或回环地址上的 `http://`） |
| `CREDENTIAL_STORE_INGRESS_RATE_PER_MINUTE` / `INGRESS_RATE_BURST` | `10` / `5`，每个来源 IP 的提交限速 |
| `CREDENTIAL_STORE_LOG_LEVEL` | `info`，日志输出到 stdout |

浏览器入口按 TCP 连接的来源地址限速；位于反向代理之后时，所有请求共享代理的地址，需要由代理自己按客户端限速。
运行 `go test -race ./internal/credentialstore/... ./cmd/credential-store/...`。
