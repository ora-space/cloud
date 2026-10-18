# 配置参考

[English](configuration.en.md) | 中文

本仓库的几个进程各有配置来源：

| 进程 | 配置来源 |
|---|---|
| `cmd/server`、`cmd/cloudctl` | `configs/config.yaml` 格式的文件，加 `CLOUD_` 前缀的环境变量 |
| `cmd/gateway` | `configs/gateway.yaml` 格式的文件，加 `GATEWAY_` 前缀的环境变量 |
| `cmd/model-gateway` | Cloud 的配置文件（只读数据库和验证公钥），加 `MODEL_GATEWAY_` 前缀的环境变量 |
| `cmd/git-server` | Cloud 的配置文件（只读数据库），加 `GIT_SERVER_` 前缀的环境变量 |

## 通用规则

- **文件位置**：`-config <path>` 指定；省略时依次在 `./configs`、`../configs`、`.` 下找 `config.yaml`（Gateway 找 `gateway.yaml`）。
- **环境变量覆盖**：键路径的 `.` 换成 `_` 再加前缀，例如 `database.dsn` 对应 `CLOUD_DATABASE_DSN`，`login.provider` 对应
  `GATEWAY_LOGIN_PROVIDER`。即使配置文件里没写某个键，对应的环境变量也会生效。列表和映射（例如 `auth.keys`）不能用单个环境变量设置。
- **时长**使用 Go 的时长写法（`30s`、`15m`、`2h`）。
- **密钥只以文件引用出现**：私钥、证书、凭据都写成文件路径，进程启动时把内容读进内存，不写进配置值、环境变量或日志。
- **启动即校验**：配置不合法、文件读不到、迁移状态不对、证书或信任配置缺失时，进程以非零状态退出，不会带着残缺的配置运行。

## Cloud server

| 键 | 默认 | 说明 |
|---|---|---|
| `server.port` | `8080` | HTTP 监听端口 |
| `server.mode` | — | Gin 模式：`debug`、`release`、`test` |
| `server.read_timeout`、`server.write_timeout` | — | 必须为正 |
| `logger.*` | — | `level`、`filename`、`max_size`（MB）、`max_backups`、`max_age`（天）、`compress`、`enable_console`；结构化 JSON 日志，按大小轮转 |
| `database.dsn` | — | PostgreSQL 连接串；生产使用 TLS 和只有 DML 权限的账号 |
| `database.max_idle_conns`、`max_open_conns`、`conn_max_lifetime` | — | 连接池；`conn_max_lifetime` 必须为正 |
| `auth.audience` | — | 内部 JWT 的 audience |
| `auth.keys[]` | — | 可信公钥：`id`、`issuer`、`kind`（`service` 或 `user`）、`role`（service 才有）、`public_key_file`。为空时拒绝启动，见 [内部凭据](../identity/authentication.md) |
| `control.grpc_addr` | 必填 | Controller 控制监听地址 |
| `control.certificate_file`、`private_key_file`、`client_ca_file`、`controller_identity` | 必填 | 控制监听的 TLS 1.3 双向认证；身份是 `spiffe://<信任域>/controller/<id>` |
| `directory.endpoint`、`hw_id`、`environment`、`app_key_file` | 空 | 企业员工目录；`endpoint` 为空即公网部署，见 [用户、租户与成员](../identity/tenancy.md#加入租户) |
| `collaboration.development_fixtures` | `false` | 开发夹具，生产必须关闭，见 [Issue 协作](../collaboration/interactions.md#端口issue-只拥有消费侧的契约) |
| `plugins.sync_enabled` | — | 是否同步插件市场 |
| `plugins.marketplace_url`、`marketplace_branch`、`sync_interval` | — | 开启同步时必填：`https` Git 地址（不带查询和片段）、合法分支名、正的间隔 |
| `plugins.asset_import_enabled`、`asset_import_interval` | —、`30s` | 工作流插件资产导入 |
| `issue_runs.thread_idle_timeout` | `15m` | Thread 空闲窗口；非正值取默认 |
| `issue_runs.agent_start_timeout`、`agent_restore_timeout`、`agent_end_confirm_timeout` | `2m`、`6m`、`5m` | 启动窗口，显式配置非正值时拒绝启动，见 [有界启动](../agent-runs/startup-settlement.md) |
| `issue_runs.agent_workspace_blocked_timeout` | `30m` | 运行 Workspace 创建停在 `blocked` 的放弃窗口，规则同上 |
| `issue_runs.delivery_give_up_after`、`delivery_unreachable_after` | `2h`、`30m` | 交付与删除的放弃上限；非正值取默认 |
| `object_store.*` | `enabled: false` | 见 [Revision](../agent-runs/revisions.md#对象存储)：`endpoint`、`public_endpoint`、`region`、`bucket`、`path_style`、`access_key_id_file`、`secret_access_key_file`、`upload_grant_ttl` |

`cloudctl` 读取同一份配置，只用到数据库连接；它需要拥有建表权限的迁移账号。

## Gateway

| 键 | 默认 | 说明 |
|---|---|---|
| `server.*`、`logger.*`、`database.*` | — | 与 Cloud server 相同；数据库使用只对两张 Gateway 表有权限的专用角色 |
| `public.base_url` | — | 浏览器看到的 origin；必须是 `https`，只有 `public.development: true` 时允许 loopback 的 `http` |
| `public.development` | `false` | 本地开发：允许 loopback HTTP，Cookie 去掉 `Secure` |
| `session.ttl` | IDaaS `12h`，其他 `30 天` | 绝对有效期，上限 90 天 |
| `session.cleanup_interval`、`retention`、`cleanup_batch` | `10m`、`168h`、`500` | 过期会话的清理 |
| `login.provider` | `github` | 外部 provider：`huawei-idaas` 或 `github`；只读取它的 secret |
| `login.development_provider` | `false` | 注册本地开发登录；只能与 `public.development` 同时开启 |
| `login.attempt_ttl` | `10m` | 登录尝试有效期，1 分钟到 1 小时 |
| `login.rate_limit_per_minute`、`rate_limit_burst` | `30`、同前 | 登录开始与回调的限流 |
| `login.pkce_key_file` | 必填 | 至少 32 字节随机数据；所有副本相同 |
| `cloud.upstream`、`cloud.timeout` | —、`30s` | 固定的 Cloud upstream（不带路径和查询） |
| `tokens.issuer`、`audience`、`service_subject` | — | 内部 JWT 的声明 |
| `tokens.service_key_id`、`service_private_key_file`、`user_key_id`、`user_private_key_file` | — | 两把不同的 Ed25519 私钥（PKCS#8 PEM） |
| `tokens.lifetime` | `1m` | 不超过 5 分钟 |
| `github.client_id`、`client_secret_file` | — | GitHub OAuth App；`authorize_url`、`token_url`、`user_url`、`source` 可指向 GitHub Enterprise Server |
| `idaas.base_url`、`client_id`、`client_secret_file`、`display_name_field`、`timeout` | —、—、—、—、`10s` | 华为 IDaaS |
| `idaas.authorize_url`、`token_url`、`userinfo_url` | 空 | 独立覆盖完整 HTTPS 端点；不允许 URL 用户凭据、query 或 fragment |
| `web.dist_dir` | 空 | 前端构建产物目录；为空时不提供前端 |
| `model_credentials.upstream`、`ca_file` | 空 | 固定的 HTTPS model-gateway 地址与它的 CA；二者必须同时设置 |

`GATEWAY_IDAAS_AUTHORIZE_URL`、`GATEWAY_IDAAS_TOKEN_URL`、`GATEWAY_IDAAS_USERINFO_URL` 分别覆盖上述三个端点。
每个未配置的端点沿用 `GATEWAY_IDAAS_BASE_URL` 加 `/saaslogin1/oauth2/v1/authorize`、`token` 或 `userinfo` 路径。
这些值由部署管理员设置，不能由浏览器选择；认证协议与身份映射见 [Gateway](../identity/gateway.md)。
更改配置和轮换 secret 的操作见 [部署与运维](deployment.md#华为-idaas-现场配置)。

## model-gateway

环境变量列在 [model-gateway](../models/model-gateway.md#启动与停止)。它读取 Cloud 配置文件里的 `database.*` 与 `auth.*`，用后者校验 Gateway
转发来的凭据写入请求。

## git-server

完整说明见 [`cmd/git-server`](../../cmd/git-server/README.md)：

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `GIT_SERVER_REPOSITORY_ADDR` | `:8445` | 面向沙盒的仓库监听，只接入沙盒网络 |
| `GIT_SERVER_SERVICE_REPOSITORY_ADDR` | `:8447` | 面向 Repo Executor 的仓库监听，只接入内部服务网络 |
| `GIT_SERVER_REF_ADDR` | `:8446` | Cloud 的 ref 查询和 `/healthz` |
| `GIT_SERVER_REPOSITORY_ROOT` | `/var/lib/ora-git/repositories` | bare 仓库所在的持久卷 |
| `GIT_SERVER_CERTIFICATE_FILE`、`_PRIVATE_KEY_FILE` | `/etc/ora-git-management/server.pem`、`server.key` | 服务证书 |
| `GIT_SERVER_SANDBOX_CA_FILE`、`_EXECUTOR_CA_FILE` | `/etc/ora-git-management/ca.pem`、`repo-executor-ca.pem` | 两类客户端证书各自的 CA |
| `GIT_SERVER_CLOUD_CLIENT_CERT_FILE` | `/etc/ora-git-management/cloud-client.pem` | 唯一允许查询 ref 的叶证书 |

## 本地开发

本地开发不直接编辑这些文件：`config.toml`（由 `config.toml.template` 复制，被 Git 忽略）经 `task setup` 生成密钥、secret 文件和
`.local/dev.env`，后者以 `CLOUD_*`、`GATEWAY_*` 环境变量覆盖 `configs/*.yaml`，`Taskfile.yml` 为每个 task 自动加载。见
[本地开发](../development/local-development.md)。
