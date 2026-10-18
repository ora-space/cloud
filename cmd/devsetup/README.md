# cmd/devsetup：本地开发一键配置

[中文](README.md) | [English](README.en.md)

`cmd/devsetup`（`task setup`）只服务于本地开发：把仓库根目录的 `config.toml` 变成真实的 Gateway 与 Cloud 在一台笔记本上启动所需的一切，开发者不必手工执行 `openssl`、拼环境变量或分别写密钥文件。生产密钥与配置由部署基础设施提供，本命令不参与。

## 职责

按 `config.toml`（从 `config.toml.template` 复制，被 Git 忽略）执行：

- 在 `.local/gateway/`（`-dir` 可改）生成两对用途分离的 Ed25519 密钥：`gateway-service.key`/`.pem` 与 `user-identity.key`/`.pem`。私钥为 PKCS#8 PEM 交给 Gateway（`configs/gateway.yaml`），公钥为 PKIX PEM 交给 Cloud（`configs/config.yaml` 的 `auth.keys`）；另生成 32 字节随机 `gateway-pkce.key`。
- 把所填外部 provider（`[github]` 或 `[idaas]`，至多一个）的 `client_secret` 写入 `.local/gateway/<provider>-client-secret`（Gateway 对应 `client_secret_file` 指向的文件），删除另一个残留的 secret 文件；两者都留空时 Gateway 只提供本地开发登录。
- 写出 `.local/dev.env`（`-env` 可改）：`CLOUD_DATABASE_DSN`、`GATEWAY_DATABASE_DSN`、`TEST_DATABASE_URL` 取自 `[database].dsn`，`GATEWAY_LOGIN_PROVIDER` 取自所填的段（`github` / `huawei-idaas` / 空），`GATEWAY_GITHUB_CLIENT_ID` 取自 `[github]`，`GATEWAY_IDAAS_BASE_URL` / `GATEWAY_IDAAS_CLIENT_ID` / `GATEWAY_IDAAS_DISPLAY_NAME_FIELD` 取自 `[idaas]`。`Taskfile.yml` 通过 `dotenv` 为每个 task 加载它，因此 `task dev`、`task migrate`、`task test`、`task check` 都用同一套值；shell 中已有的环境变量优先。
- 用同一个 DSN 应用数据库迁移，与 `cloudctl migrate` 走同一条路径。

`configs/config.yaml` 与 `configs/gateway.yaml` 仍是服务配置的权威来源；`config.toml` 只提供每台机器不同的值，并且只通过服务本来就接受的环境变量覆盖与密钥文件生效。

## 边界与不变量

- **严格解析**：`config.toml` 里未知的键会报错并指出位置；`database.dsn` 必填；`[github]` 与 `[idaas]` 各自的 `client_id` / `client_secret` 要么同时填、要么同时留空，且两段不能同时填（Gateway 只有一个外部 provider）。
- **可重复执行**：已存在的密钥一律保留；一对密钥的私钥与公钥必须同时存在，否则报错要求删除后重生成，避免 Gateway 与 Cloud 信任的密钥漂移。secret 文件与 `.local/dev.env` 每次都按 `config.toml` 重写，改完 `config.toml` 重跑即可。迁移是幂等的。
- **不输出机密**：只打印“已生成/已保留/已写入”的文件名，不打印私钥、client secret 或可能含密码的 DSN。
- **只写 `.local/`**：该目录与 `config.toml` 都被 Git 忽略；所有文件权限为 `0600`，任何生成物都不进入仓库、日志或数据库。
- **禁止部署**：没有任何轮换、托管或分发机制。

参见 [cmd 入口总览](../README.md) 与 [Gateway 文档](../../docs/identity/gateway.md)。
