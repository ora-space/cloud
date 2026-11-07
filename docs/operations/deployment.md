# 部署与运维

[English](deployment.en.md) | 中文

本文说明 Cloud 由哪些进程组成、如何打包和部署、每个进程需要哪些密钥与证书，以及健康检查、停机、升级和备份时要注意什么。
完整的多仓库联调环境（Controller、Sandbox Server、Node）由 [ora-space/cluster](https://github.com/ora-space/cluster) 的 Compose 提供；
本文只描述 Cloud 一侧的部件。每个配置项见 [配置参考](configuration.md)。

## 部件

| 部件 | 入口 | 职责 |
|---|---|---|
| Cloud server | `/app/server` | 公开 HTTP API、SSE、Controller 的 gRPC 控制监听，以及 IssueRun、插件、工作流的后台循环 |
| Gateway | `/app/gateway` | 外部登录、浏览器会话、`/api/v1` 代理，可选地提供前端 |
| model-gateway | `/app/model-gateway` | 个人 Key 的加解密与模型请求代理 |
| git-server | `/app/git-server` | Project 仓库的 smart HTTP 与 ref 查询 |
| Repo Executor | `/app/repo-executor` | 唯一访问外部 Git 仓库的进程：同步、显式发布与只读探测；无状态、不连数据库 |
| Credential Store | `/app/credential-store` | 凭据的独立保管：浏览器提交、执行器兑换、Cloud 管理、撤销与过期清扫；只保存密文 |
| cloudctl | `/app/cloudctl` | 迁移、首个租户引导、凭据引用登记；一次性运行，不是服务 |
| PostgreSQL 17 | — | 唯一的业务权威 |
| S3 兼容对象存储 | — | Revision 的会话 JSONL（改走 Git 之前的 Revision 另有 bundle）；可选，没有它时不交付 Revision |

Cloud 之外，cluster 还部署 Controller（desktop 的 `ora-controller`）、Sandbox Server（cluster 自己的 Substrate 实现）和 Node 镜像（desktop 的
`ora-node`）。

## 镜像

`scripts/Dockerfile` 产出四个镜像：

- **运行镜像**（默认 target）：包含 `server`、`gateway`、`model-gateway`、`git-server`、`cloudctl` 五个静态编译的二进制、`configs/config.yaml` 与
  `configs/gateway.yaml` 样例，以及前端构建产物 `/app/web`；带 `git` 与 `git-daemon`（git-server 需要 `git http-backend`）。默认入口是 server，
  其他进程用 `--entrypoint` 选择；以非 root 用户（UID 10001）运行。
- **devsetup 镜像**（`devsetup` target）：包含 `devsetup`（本地生成密钥与执行迁移）和 `git-server-preset`（只用于验收的 Git Server 预置工具），
  供 cluster 的 Compose 使用，**不**进入服务镜像。
- **Repo Executor 镜像**（`repo-executor` target）：`repo-executor` 二进制，加上 Git 唯一允许使用的 credential helper `git-credential-ora`；
  自带 `git` 与 CA 证书。以 UID 10002 运行，任务目录 `/var/lib/repo-executor/tasks` 由部署挂载为 tmpfs，启动时清空；**不**进入运行镜像。
- **Credential Store 镜像**（`credential-store` target）：只有 `credential-store` 二进制，以 UID 10003 运行；持久数据只在
  `/var/lib/ora-credentials` 卷里，权限 `0700`。**不**进入运行镜像。

cluster 的 Compose 让 Repo Executor 与 Credential Store 都以只读根文件系统、`cap_drop: ALL` 和 `no-new-privileges` 运行，
任何密钥都不落在它们的可写层里；生产部署应保持同样的限制。

部署时挂载自己的配置文件、公钥、证书和密钥文件。设置 `GATEWAY_WEB_DIST_DIR=/app/web` 后，Gateway 在与 `/auth`、`/api` 相同的 origin 上
提供前端。

## 华为 IDaaS 现场配置

认证地址与 Client ID 由包外配置提供，Client Secret 通过 `GATEWAY_IDAAS_CLIENT_SECRET_FILE` 指定的只读挂载文件提供。
配置项和端点校验规则见 [配置参考](configuration.md#gateway)。

1. 生产配置关闭 `GATEWAY_PUBLIC_DEVELOPMENT` 和 `GATEWAY_LOGIN_DEVELOPMENT_PROVIDER`，设置 HTTPS public origin，
   并在 IDaaS 平台登记完全一致的 `<public origin>/auth/callback/huawei-idaas`。
2. 更换端点或 Client ID 后，用更新后的配置重新创建 Gateway 容器；不需要重新构建镜像。所有副本使用一致的客户端配置。
3. 原子替换 Client Secret 文件后，重新创建所有 Gateway 容器以刷新文件挂载并读取新 secret；仅替换文件不代表运行进程已生效。

## 网络

cluster 的 Compose 把服务分在四个网络上，生产部署应保持同样的隔离：

| 网络 | 成员 | 用途 |
|---|---|---|
| 默认网络 | Cloud、Gateway、model-gateway、git-server（ref 查询）、`credential-edge`、Controller、Sandbox Server、PostgreSQL、对象存储 | 服务之间的内部通信 |
| 沙盒网络 | 沙盒、Controller、Sandbox Server、model-gateway（`ora-model-gateway`）、git-server 仓库监听（`ora-git-server`）、对象存储公开地址 | 沙盒能访问的全部入口 |
| `git-services`（`internal: true`） | Cloud、git-server 服务仓库监听（`ora-git-service`）、Repo Executor、Credential Store、`credential-edge` | 外部 Git 线的内部通信；没有出网，沙盒从不加入 |
| `repo-egress` | 只有 Repo Executor | 唯一能到达外部 Git 仓库的网络 |

沙盒只能到达模型代理、Git Server 的仓库监听和对象存储的公开地址，不能直接访问 Cloud、Gateway 或数据库。外部 Git 线里，Cloud
同时接入默认网络与 `git-services`，到执行器、Credential Store 和 ref 查询的调用都走内部网络；Credential Store 只接入
`git-services`；Repo Executor 接入 `git-services` 与 `repo-egress`，因此它是唯一有出网能力的进程。

浏览器提交凭据不经过 Cloud：`credential-edge` 是 nginx，只把 Store 的提交入口发布在 `127.0.0.1:8460`（HTTPS、TLS 1.3、请求体上限
16 KiB），清空 `X-Forwarded-For` 与 Cookie 后转发，并按 TCP 来源地址限速。

## 部署顺序

1. **迁移**：用同一个运行镜像、`--entrypoint /app/cloudctl`，以拥有建表权限的迁移身份执行 `-command migrate`。迁移在一把 advisory lock 下按
   文件名顺序执行，重复执行是安全的。
2. **首个租户（可选）**：`cloudctl -command bootstrap -name … -source … -subject … -display-name …` 原子创建租户、唯一的空间和首位管理员；
   华为 IDaaS 部署里 `-subject` 必须是该员工的 IDaaS `uuid`。之后用户也可以自助创建租户。
3. **启动服务**：server、Gateway、model-gateway、git-server 启动时各自校验迁移状态：二进制里的迁移必须全部已应用且校验和一致，数据库里不能有
   二进制不认识的迁移，否则拒绝启动。它们从不执行 DDL。
4. **外部 Git 线（可选）**：准备执行器、Credential Store 与 Git Server 的证书、CA、两把 Ed25519 私钥和 keyring，先启动 Repo Executor 与
   Credential Store，再把配置里的 `repository.enabled` 设为 `true` 启动 Cloud。Cloud 启动时只校验材料、不发起网络请求；有同步或发布任务后，
   后台循环每 2 秒领取一轮。

**升级**时先执行新版本的迁移，再启动新版本的二进制。迁移只追加，已应用的文件不可修改；数据库一旦包含新迁移，旧版本的二进制重启时就会因为
"不认识的迁移"而拒绝启动，回滚需要连同数据库一起处理。

## 密钥与证书

| 进程 | 需要的材料 |
|---|---|
| Cloud server | Gateway 的两个验证公钥（`auth.keys`）；控制监听的服务证书、私钥、Controller 客户端 CA；对象存储凭据文件；Git Server ref 查询的 CA 与 Cloud 的客户端证书、私钥；企业部署的目录机器密钥 |
| Gateway | service 与 user 两把签名私钥；PKCE 密钥；所选 provider 的 client secret；model-gateway 的公开 CA |
| model-gateway | 服务证书与私钥；校验 Node 模型客户端证书的 CA；加密主密钥；Gateway 的验证公钥（经 Cloud 配置） |
| git-server | 服务证书与私钥；沙盒 git-access 证书的 CA；Repo Executor 证书的 CA；Cloud 的 ref 查询客户端证书 |
| Cloud server（外部 Git 线） | 调度方（dispatcher）与凭据管理方（manager）两套 mTLS 客户端证书与私钥；Git Server ref 查询的客户端证书与私钥；执行器 CA、Credential Store CA、Git Server CA；签发执行授权与浏览器提交授权的两把 Ed25519 私钥（必须不同）；浏览器提交入口的 origin |
| Repo Executor | 任务监听、Git Server 与 Credential Store 共用的服务证书与私钥；签发 Cloud 调度方证书的 CA；Cloud 的执行验证公钥；Git Server 与 Credential Store 的 CA |
| Credential Store | 四个监听共用的服务证书与私钥；签发执行器、管理方和 KEK 运维客户端证书的 CA；Cloud 的执行与提交两组验证公钥（必须不同）；KEK keyring 与凭据卷；浏览器入口证书 |
| credential-edge（cluster 的 Compose） | 浏览器入口 origin 的证书与私钥，以及校验 Store 服务证书的 CA |
| Controller | 控制监听认可的客户端证书，URI 为配置的 SPIFFE 身份 |

- 所有密钥都以只读文件挂载，只放在管理卷里，不能进入代码目录或运行时 Workspace。
- 证书只证明身份；三类沙盒证书（WSS 服务、`git-access`、`model-access`）可能链到同一个 CA，靠证书 URI 的用途段区分，不能互相替代。
- Repo Executor 的证书由它自己的 CA 签发：Git Server 的服务仓库监听只接受这张 CA 签发的客户端，沙盒监听只接受沙盒证书，两套身份不能互换。
  执行器的 CA 私钥留在管理卷里，Git Server 只拿到它的证书。
- 两把 Ed25519 私钥只留在 Cloud；对应的两组公钥以只读文件挂给 Repo Executor（执行）与 Credential Store（执行与提交各一组）。浏览器提交
  授权只由 Cloud 签发，Store 用提交公钥验证后落库。
- 轮换 Gateway 签名密钥时，在 Cloud 的 `auth.keys` 里同时配置新旧两个 `kid` 再重启，旧凭据自然过期后移除旧条目。

## 数据库账号

| 账号 | 权限 |
|---|---|
| 迁移账号 | 建表与改表，只给 `cloudctl migrate` 使用 |
| Cloud server、model-gateway、git-server | 业务表的 DML |
| Gateway | 只对 `gateway_login_attempts`、`gateway_sessions` 有 `SELECT/INSERT/UPDATE/DELETE`，对 `schema_migrations` 有 `SELECT` |

Repo Executor 与 Credential Store 不连接 PostgreSQL：前者的任务状态全部由 Cloud 记录，后者的数据在自己的凭据卷里。生产连接使用 TLS，
不要使用样例里的本地 trust 配置。

## 共享出网

Cloud server、全部 model-gateway 与全部 Repo Executor 只读挂载同一份 `deployment-egress.json`。前两者用 `-deployment-egress` 或
`ORA_DEPLOYMENT_EGRESS_FILE` 指向它；Repo Executor 用 `REPO_EXECUTOR_DEPLOYMENT_EGRESS_FILE`（优先）或
`ORA_DEPLOYMENT_EGRESS_FILE`。更换时先停止这三类进程并确认旧进程已退出，再替换文件并启动；只改文件不会改变已经加载的快照。
策略见 [部署方共享出网配置](deployment-egress.md)。

## 健康检查

| 进程 | 检查 |
|---|---|
| Cloud server | `GET /healthz`：数据库不可达返回 `503 database_unavailable`；报告对象存储与 Git Server 是否已配置（`objectStore`、`gitServer`），不代表它们在线。响应头还有 `X-Ora-Egress-Schema` 与 `X-Ora-Egress-Digest` |
| Gateway | `GET /healthz`：PostgreSQL 探活 |
| model-gateway | `model-gateway -healthcheck`：正常校验 HTTPS 和公开 CA，检查数据库就绪，不读取用户 Key 或主密钥，也不报告出网摘要。凭据监听上的 `GET /healthz` 才带与 Cloud 相同的 `X-Ora-Egress-Schema` 与 `X-Ora-Egress-Digest` |
| git-server | `git-server -healthcheck`：访问 ref 监听的 `/healthz`，用部署 CA 校验服务端证书，不读取私钥 |
| Repo Executor | 健康监听（`REPO_EXECUTOR_HEALTH_ADDR`，默认 `:8084`，明文）上的 `GET /healthz`：监听成功即说明证书、CA 与公钥可用；响应头带与 Cloud 相同的 `X-Ora-Egress-Schema` 与 `X-Ora-Egress-Digest` |

Credential Store 没有独立的健康检查端点：监听成功即说明证书、CA、公钥和 keyring 可用；兑换失败通过 Cloud
的任务状态与 Store 日志体现。

健康检查只报告依赖是否就绪，不暴露配置内容。

## 停机

- **Cloud server** 收到 SIGTERM 或 SIGINT 后，先向 Controller 的信号流发 `Drain`，再在 10 秒内优雅停止 gRPC 与 HTTP，最后等待后台循环中正在
  执行的一轮结束。被截断的 Controller 调用会以同一个提交身份重试，不丢失持久状态。
- **Gateway** 在 10 秒内停止 HTTP，并停止会话清理循环。
- **model-gateway** 先取消进行中的模型流，再等待所有监听退出。
- **git-server** 先排空三个监听，再关闭 pre-receive hook 通道。
- **Repo Executor** 取消进行中的任务：Git 进程被终止，任务的暂存目录与凭据材料随之销毁，30 秒内结束。
- **Credential Store** 在 10 秒内排空四个监听，并等待过期清扫循环退出。

## 多实例

所有权威状态都在 PostgreSQL，后台循环在每个事务里重新读取并以比较并交换的方式转换，多个 server 实例同时运行不会重复推进。但 SSE 失效事件和
Controller 信号只在进程内传递：浏览器只能收到它所连实例上提交的事件，Controller 的 `Watch` 也只收到它所连实例的信号。两者本来就是提示，客户端
靠重新读取 REST、Controller 靠周期领取保证不丢工作，但多实例部署会让实时刷新变得更慢。

外部 Git 线不改变这一点：同步与发布的租约、发布核对和凭据核验都在 PostgreSQL 里，不依赖执行器实例的身份；Repo Executor 无状态，
任务目录在启动时清空，重启后由 Cloud 重新派发。Credential Store 的数据在它自己的凭据卷里，扩容与卷的共享方式单独决定，不在 Cloud 的
多实例范围之内。

## 备份

| 数据 | 说明 |
|---|---|
| PostgreSQL | 全部业务状态 |
| model-gateway 主密钥卷 | 没有它，数据库里的 Key 密文无法解密；**必须与数据库一起备份** |
| git-server 仓库卷 | Project 仓库（`GIT_SERVER_REPOSITORY_ROOT`） |
| 对象存储桶 | Revision 的会话 JSONL（以及改走 Git 之前登记的 bundle） |
| Credential Store 凭据卷与 KEK keyring | 外部 Git 凭据的密文和它的密钥；**必须一起备份**，丢失 keyring 就无法解密卷里的凭据 |
| 证书与密钥卷 | 各进程的身份材料；Repo Executor 没有需要备份的数据，任务目录是 tmpfs |

## CI

仓库有两个 GitHub Actions 工作流：

- **Backend**：用 PostgreSQL 17 服务和固定版本的 RustFS 运行格式与静态检查、带 race 检测的完整集成测试、proto 检查和构建。
- **Frontend**：只在 `frontend/`、`api/` 或 `internal/contract/` 变化时触发，检查生成客户端与契约一致，并执行格式、lint、类型、带覆盖率的测试、
  模块文档与测试、死代码、重复代码和构建门禁。

测试门禁的说明见 [测试与门禁](../development/testing.md)。
