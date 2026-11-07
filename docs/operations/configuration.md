# 配置参考

[English](configuration.en.md) | 中文

本仓库的几个进程各有配置来源：

| 进程 | 配置来源 |
|---|---|
| `cmd/server`、`cmd/cloudctl` | `configs/config.yaml` 格式的文件，加 `CLOUD_` 前缀的环境变量 |
| `cmd/gateway` | `configs/gateway.yaml` 格式的文件，加 `GATEWAY_` 前缀的环境变量 |
| `cmd/model-gateway` | Cloud 的配置文件（只读数据库和验证公钥），加 `MODEL_GATEWAY_` 前缀的环境变量 |
| `cmd/git-server` | Cloud 的配置文件（只读数据库），加 `GIT_SERVER_` 前缀的环境变量 |
| `cmd/repo-executor` | 只有 `REPO_EXECUTOR_` 前缀的环境变量 |
| `cmd/credential-store` | 只有 `CREDENTIAL_STORE_` 前缀的环境变量 |
| `cmd/git-credential-ora` | 无配置；Repo Executor 以一次任务的凭据材料目录作为唯一参数调用它 |

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
| `plugins.personal_agent_target` | 空 | 声明式个人 Agent 的部署 target；空值关闭，支持 `x86_64-unknown-linux-gnu` 或 `aarch64-unknown-linux-gnu`，部署完 adapter 再启用 |
| `plugins.marketplace_url`、`marketplace_branch`、`sync_interval` | — | 开启同步时必填：`https` Git 地址（不带查询和片段）、合法分支名、正的间隔 |
| `plugins.asset_import_enabled`、`asset_import_interval` | —、`30s` | 工作流插件资产导入 |
| `issue_runs.thread_idle_timeout` | `15m` | Thread 空闲窗口；非正值取默认 |
| `issue_runs.agent_start_timeout`、`agent_restore_timeout`、`agent_end_confirm_timeout` | `2m`、`6m`、`5m` | 启动窗口，显式配置非正值时拒绝启动，见 [有界启动](../agent-runs/startup-settlement.md) |
| `issue_runs.agent_workspace_blocked_timeout` | `30m` | 运行 Workspace 创建停在 `blocked` 的放弃窗口，规则同上 |
| `issue_runs.delivery_give_up_after`、`delivery_unreachable_after` | `2h`、`30m` | 交付与删除的放弃上限；`delivery_unreachable_after` 同时是"Node 是否还在"的判定窗口，工作流运行里进行中的会话也按它结算；非正值取默认 |
| `workflow.session_turn_timeout` | `2h` | 工作流会话初始回合的窗口：执行行 `created_at` 超过它、既没有提交结果也没有成功的 `turnEnded` 时，节点以 `session_turn_deadline_exceeded` 失败；按执行行计时，非正值取默认，见 [工作流](../workflows/workflows.md#调度) |
| `object_store.*` | `enabled: false` | 见 [Revision](../agent-runs/revisions.md#对象存储)：`endpoint`、`public_endpoint`、`region`、`bucket`、`path_style`、`access_key_id_file`、`secret_access_key_file`、`upload_grant_ttl` |
| `git_server.repository_base_url` | 空 | 沙盒访问的 Git Server 仓库 base URL（`https`，不带用户信息、查询、片段和末尾斜杠），写进 clone、会话和交付输入；为空即不使用 Git Server：clone 步骤停在 `blocked`（`git_server_unconfigured`），交付以 `skipped` 结算 |
| `git_server.ref_query_url`、`ca_file`、`client_cert_file`、`client_key_file` | 空 | 设置了 `repository_base_url` 时必填：ref 查询监听的 `https://host[:port]`、校验它的 CA，以及 Cloud 的客户端证书和私钥文件。启动时只读入文件、不发网络请求；Git Server 不可达或证书不符只让核验暂不可用，不阻止启动。见 [Git Server](../repositories/git-server.md#ref-查询) |
| `git_server.ref_query_timeout` | `10s` | 单次 ref 查询的超时；非正值取默认 |
| `repository.enabled` | `false` | 打开外部 Git 线与凭据路由；关闭时 Project 仍可创建，但没有同步，凭据创建返回 `503`，见 [外部 Git 线](../repositories/external-git.md) |
| `repository.executor_url`、`executor_ca_file`、`dispatcher_cert_file`、`dispatcher_key_file` | URL 默认 `https://repo-executor:8471`，文件必填 | Repo Executor 的任务监听，以及 Cloud 调度方的 mTLS 客户端身份 |
| `repository.authorization_key_file` | 必填 | 签发执行授权的 Ed25519 私钥（PKCS#8），必须与 `submission_key_file` 不同 |
| `repository.credential_store_url`、`credential_store_ca_file`、`manager_cert_file`、`manager_key_file` | URL 默认 `https://credential-store:8462`，文件必填 | Credential Store 管理接口，以及 Cloud 管理方的 mTLS 客户端身份 |
| `repository.submission_key_file` | 必填 | 签发浏览器提交授权的 Ed25519 私钥（PKCS#8） |
| `repository.credential_ingress_url` | 必填 | 浏览器提交凭据的 Credential Store origin，随每次提交授权返回 |
| `repository.git_refs_url`、`git_refs_ca_file`、`git_refs_cert_file`、`git_refs_key_file` | URL 默认 `https://git-server:8446`，文件必填 | Git Server 的只读 ref 查询与 Cloud 的固定客户端证书 |
| `repository.slots`、`lease`、`sync_timeout`、`publish_timeout` | `4`、`1m`、`30m`、`5m` | 并发任务数、租约、同步上限与发布上限；必须为正 |

启动时 `repository.enabled` 为真会校验两把私钥不同、Ingress URL 非空，并加载三组 mTLS 客户端材料（执行器、Credential
Store、Git Server），不发起任何网络请求。

两组配置都访问 Git Server 的同一个 ref 查询监听（`GET /internal/v1/refs`），各用自己的客户端：`git_server.*` 供 Workspace clone 输入与 Revision 交付核验使用，`repository.git_refs_*` 供仓库 worker、发布来源核对与 Workspace 分支列表使用。

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
| `login.development_provider` | `false` | 注册本地开发登录；配置校验要求 `public.development: true` 且 `public.base_url` 为 loopback HTTP origin（可带端口）；HTTPS 即使开启开发模式也会被拒绝 |
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

## 共享出网

Cloud server、model-gateway 和 Repo Executor 指向同一份包外 JSON。前两者用启动参数 `-deployment-egress <path>` 或环境变量
`ORA_DEPLOYMENT_EGRESS_FILE`，参数优先；Repo Executor 只读环境变量，`REPO_EXECUTOR_DEPLOYMENT_EGRESS_FILE` 优先，否则用
`ORA_DEPLOYMENT_EGRESS_FILE`，路径必须是绝对路径。省略时为公网模式。公网 DNS 仍是 `MODEL_GATEWAY_DNS_HTTPS_URL` 与
`MODEL_GATEWAY_DNS_BOOTSTRAP_IPS`，三个进程必须拿到相同的值。文件格式、企业白名单和更换方式见
[部署方共享出网配置](deployment-egress.md)。这项不写进 `configs/config.yaml`。

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

## Repo Executor 与 Credential Store

完整说明见 [`cmd/repo-executor`](../../cmd/repo-executor/README.md) 与 [`cmd/credential-store`](../../cmd/credential-store/README.md)。
两者都以非 root 运行、不连接 Cloud 的数据库；密钥、证书和公钥都只以只读文件挂载。

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `REPO_EXECUTOR_ADDR` | `:8471` | 任务监听（mTLS），只接受 Cloud 调度方的身份 |
| `REPO_EXECUTOR_CERT_FILE`、`_KEY_FILE` | 必填 | 执行器服务证书；同时用于任务监听、Git Server 与 Credential Store |
| `REPO_EXECUTOR_CLIENT_CA_FILE` | 必填 | 签发 Cloud 调度方客户端证书的 CA |
| `REPO_EXECUTOR_AUTHORIZATION_PUBLIC_KEYS_FILE` | 必填 | Cloud 执行公钥集合（PEM，可多块） |
| `REPO_EXECUTOR_GIT_SERVER_URL`、`_GIT_SERVER_CA_FILE` | `https://ora-git-service:8447`、必填 | Git Server 服务仓库监听及其 CA |
| `REPO_EXECUTOR_CREDENTIAL_STORE_URL`、`_CREDENTIAL_STORE_CA_FILE` | `https://credential-store:8461`、必填 | Credential Store 兑换监听及其 CA |
| `REPO_EXECUTOR_WORK_ROOT` | `/var/lib/repo-executor/tasks` | 任务目录；部署挂载为 tmpfs，启动时清空 |
| `REPO_EXECUTOR_HELPER_PATH` | `/usr/local/bin/git-credential-ora` | Git 唯一允许使用的 credential helper |
| `REPO_EXECUTOR_GIT_PROGRAM` | `git` | Git 可执行文件（需 ≥ 2.37，才有 `http.curloptResolve`） |
| `REPO_EXECUTOR_SLOTS`、`_MAX_TASK_BYTES` | `4`、2 GiB | 并发任务数与单个任务目录上限 |
| `REPO_EXECUTOR_SYNC_TIMEOUT_MAX`、`_PUBLISH_TIMEOUT_MAX`、`_PROBE_TIMEOUT_MAX` | `30m`、`5m`、同 publish | 三类任务的上限 |
| `REPO_EXECUTOR_HEALTH_ADDR` | `:8084` | 明文健康监听，只在部署网络内；`GET /healthz` 带 `X-Ora-Egress-Schema` 与 `X-Ora-Egress-Digest` |
| `REPO_EXECUTOR_DEPLOYMENT_EGRESS_FILE`（或 `ORA_DEPLOYMENT_EGRESS_FILE`） | 空 | 部署方共享的 `deployment-egress.json`，绝对路径，启动时读取一次；带前缀的变量优先。空为公网模式 |
| `REPO_EXECUTOR_EGRESS_DENY_HOSTS` | 空 | 逗号分隔的内部主机名（精确或父域）；Git Server 与 Credential Store 总是拒绝 |
| `REPO_EXECUTOR_DEVELOPMENT`、`_DEVELOPMENT_ALLOW_HOSTS` | `false`、空 | 仅公网模式的精确 fixture 主机例外；未开 `DEVELOPMENT` 时列出主机拒绝启动 |
| `REPO_EXECUTOR_EXTERNAL_CA_FILE` | 空 | 替代系统信任的 CA（仅验收 fixture） |
| `REPO_EXECUTOR_LOG_LEVEL` | `info` | 日志级别 |

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `CREDENTIAL_STORE_ROOT` | 必填 | 凭据卷，绝对路径，权限必须为 `0700` |
| `CREDENTIAL_STORE_KEYRING_FILE` | 必填 | 只读挂载的 KEK keyring；缺失或无效时拒绝启动 |
| `CREDENTIAL_STORE_CERT_FILE`、`_KEY_FILE` | 必填 | 三个 mTLS 监听的服务证书 |
| `CREDENTIAL_STORE_CLIENT_CA_FILE` | 必填 | 签发执行器、管理和 KEK 运维客户端证书的 CA |
| `CREDENTIAL_STORE_EXECUTION_PUBLIC_KEYS_FILE`、`_SUBMISSION_PUBLIC_KEYS_FILE` | 必填 | Cloud 的两组验证公钥，必须不同 |
| `CREDENTIAL_STORE_REDEMPTION_ADDR`、`_MANAGEMENT_ADDR`、`_KEY_ADDR` | `:8461`、`:8462`、`:8463` | 兑换、管理、KEK 运维监听 |
| `CREDENTIAL_STORE_INGRESS_ADDR` | `:8460` | 浏览器入口，由边缘反向代理直接转发 |
| `CREDENTIAL_STORE_INGRESS_CERT_FILE`、`_INGRESS_KEY_FILE` | 必填 | 浏览器入口 origin 的证书 |
| `CREDENTIAL_STORE_INGRESS_ALLOWED_ORIGINS` | 必填 | 逗号分隔的前端 origin，CORS 只回显这些值 |
| `CREDENTIAL_STORE_INGRESS_RATE_PER_MINUTE`、`_INGRESS_RATE_BURST` | `10`、`5` | 每个来源 IP 的提交限速 |
| `CREDENTIAL_STORE_LOG_LEVEL` | `info` | 日志级别 |

浏览器入口按 TCP 连接的来源地址限速；位于反向代理之后时所有请求共享代理的地址，需要由代理自己按客户端限速。

## 本地开发

本地开发不直接编辑这些文件：`config.toml`（由 `config.toml.template` 复制，被 Git 忽略）经 `task setup` 生成密钥、secret 文件和
`.local/dev.env`，后者以 `CLOUD_*`、`GATEWAY_*` 环境变量覆盖 `configs/*.yaml`，`Taskfile.yml` 为每个 task 自动加载。见
[本地开发](../development/local-development.md)。`task setup` 不生成外部 Git 线的证书与密钥，本地默认
`repository.enabled: false`；需要这条线时按上表自行准备执行器、Credential Store 与 Git Server 的材料。
