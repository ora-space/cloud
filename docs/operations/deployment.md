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
| cloudctl | `/app/cloudctl` | 迁移、首个租户引导、凭据引用登记；一次性运行，不是服务 |
| PostgreSQL 17 | — | 唯一的业务权威 |
| S3 兼容对象存储 | — | Revision 对象（bundle、会话 JSONL）；可选 |

Cloud 之外，cluster 还部署 Controller（desktop 的 `ora-controller`）、Sandbox Server（cluster 自己的 Substrate 实现）和 Node 镜像（desktop 的
`ora-node`）。

## 镜像

`scripts/Dockerfile` 产出两个镜像：

- **运行镜像**（默认 target）：包含 `server`、`gateway`、`model-gateway`、`git-server`、`cloudctl` 五个静态编译的二进制、`configs/config.yaml` 与
  `configs/gateway.yaml` 样例，以及前端构建产物 `/app/web`；带 `git` 与 `git-daemon`（git-server 需要 `git http-backend`）。默认入口是 server，
  其他进程用 `--entrypoint` 选择；以非 root 用户（UID 10001）运行。
- **devsetup 镜像**（`devsetup` target）：包含 `devsetup`（本地生成密钥与执行迁移）和 `git-server-preset`（只用于验收的 Git Server 预置工具），
  供 cluster 的 Compose 使用，**不**进入服务镜像。

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

cluster 的 Compose 把服务分在三个网络上，生产部署应保持同样的隔离：

| 网络 | 成员 | 用途 |
|---|---|---|
| 默认网络 | Cloud、Gateway、model-gateway、git-server（ref 查询）、Controller、Sandbox Server、PostgreSQL、对象存储 | 服务之间的内部通信 |
| 沙盒网络 | 沙盒、Controller、Sandbox Server、model-gateway（`ora-model-gateway`）、git-server 仓库监听（`ora-git-server`）、对象存储公开地址 | 沙盒能访问的全部入口 |
| Git 服务网络 | git-server 服务仓库监听（`ora-git-service`），以及需要它的内部服务（例如 Repo Executor） | 内部网络，沙盒从不加入，也不需要对外出网 |

沙盒只能到达模型代理、Git Server 的仓库监听和对象存储的公开地址，不能直接访问 Cloud、Gateway 或数据库。

## 部署顺序

1. **迁移**：用同一个运行镜像、`--entrypoint /app/cloudctl`，以拥有建表权限的迁移身份执行 `-command migrate`。迁移在一把 advisory lock 下按
   文件名顺序执行，重复执行是安全的。
2. **首个租户（可选）**：`cloudctl -command bootstrap -name … -source … -subject … -display-name …` 原子创建租户、唯一的空间和首位管理员；
   华为 IDaaS 部署里 `-subject` 必须是该员工的 IDaaS `uuid`。之后用户也可以自助创建租户。
3. **启动服务**：server、Gateway、model-gateway、git-server 启动时各自校验迁移状态：二进制里的迁移必须全部已应用且校验和一致，数据库里不能有
   二进制不认识的迁移，否则拒绝启动。它们从不执行 DDL。

**升级**时先执行新版本的迁移，再启动新版本的二进制。迁移只追加，已应用的文件不可修改；数据库一旦包含新迁移，旧版本的二进制重启时就会因为
"不认识的迁移"而拒绝启动，回滚需要连同数据库一起处理。

## 密钥与证书

| 进程 | 需要的材料 |
|---|---|
| Cloud server | Gateway 的两个验证公钥（`auth.keys`）；控制监听的服务证书、私钥、Controller 客户端 CA；对象存储凭据文件；企业部署的目录机器密钥 |
| Gateway | service 与 user 两把签名私钥；PKCE 密钥；所选 provider 的 client secret；model-gateway 的公开 CA |
| model-gateway | 服务证书与私钥；校验 Node 模型客户端证书的 CA；加密主密钥；Gateway 的验证公钥（经 Cloud 配置） |
| git-server | 服务证书与私钥；沙盒 git-access 证书的 CA；Repo Executor 证书的 CA；Cloud 的 ref 查询客户端证书 |
| Controller | 控制监听认可的客户端证书，URI 为配置的 SPIFFE 身份 |

- 所有密钥都以只读文件挂载，只放在管理卷里，不能进入代码目录或运行时 Workspace。
- 证书只证明身份；三类沙盒证书（WSS 服务、`git-access`、`model-access`）可能链到同一个 CA，靠证书 URI 的用途段区分，不能互相替代。
- 轮换 Gateway 签名密钥时，在 Cloud 的 `auth.keys` 里同时配置新旧两个 `kid` 再重启，旧凭据自然过期后移除旧条目。

## 数据库账号

| 账号 | 权限 |
|---|---|
| 迁移账号 | 建表与改表，只给 `cloudctl migrate` 使用 |
| Cloud server、model-gateway、git-server | 业务表的 DML |
| Gateway | 只对 `gateway_login_attempts`、`gateway_sessions` 有 `SELECT/INSERT/UPDATE/DELETE`，对 `schema_migrations` 有 `SELECT` |

生产连接使用 TLS，不要使用样例里的本地 trust 配置。

## 健康检查

| 进程 | 检查 |
|---|---|
| Cloud server | `GET /healthz`：数据库不可达返回 `503 database_unavailable`；报告对象存储是否已配置，不代表存储在线 |
| Gateway | `GET /healthz`：PostgreSQL 探活 |
| model-gateway | `model-gateway -healthcheck`：正常校验 HTTPS 和公开 CA，检查数据库就绪，不读取用户 Key 或主密钥 |
| git-server | `git-server -healthcheck`：访问 ref 监听的 `/healthz`，用部署 CA 校验服务端证书，不读取私钥 |

健康检查只报告依赖是否就绪，不暴露配置内容。

## 停机

- **Cloud server** 收到 SIGTERM 或 SIGINT 后，先向 Controller 的信号流发 `Drain`，再在 10 秒内优雅停止 gRPC 与 HTTP，最后等待后台循环中正在
  执行的一轮结束。被截断的 Controller 调用会以同一个提交身份重试，不丢失持久状态。
- **Gateway** 在 10 秒内停止 HTTP，并停止会话清理循环。
- **model-gateway** 先取消进行中的模型流，再等待所有监听退出。
- **git-server** 先排空三个监听，再关闭 pre-receive hook 通道。

## 多实例

所有权威状态都在 PostgreSQL，后台循环在每个事务里重新读取并以比较并交换的方式转换，多个 server 实例同时运行不会重复推进。但 SSE 失效事件和
Controller 信号只在进程内传递：浏览器只能收到它所连实例上提交的事件，Controller 的 `Watch` 也只收到它所连实例的信号。两者本来就是提示，客户端
靠重新读取 REST、Controller 靠周期领取保证不丢工作，但多实例部署会让实时刷新变得更慢。

## 备份

| 数据 | 说明 |
|---|---|
| PostgreSQL | 全部业务状态 |
| model-gateway 主密钥卷 | 没有它，数据库里的 Key 密文无法解密；**必须与数据库一起备份** |
| git-server 仓库卷 | Project 仓库（`GIT_SERVER_REPOSITORY_ROOT`） |
| 对象存储桶 | Revision 的 bundle 与会话 JSONL |
| 证书与密钥卷 | 各进程的身份材料 |

## CI

仓库有两个 GitHub Actions 工作流：

- **Backend**：用 PostgreSQL 17 服务和固定版本的 RustFS 运行格式与静态检查、带 race 检测的完整集成测试、proto 检查和构建。
- **Frontend**：只在 `frontend/`、`api/` 或 `internal/contract/` 变化时触发，检查生成客户端与契约一致，并执行格式、lint、类型、带覆盖率的测试、
  模块文档与测试、死代码、重复代码和构建门禁。

测试门禁的说明见 [测试与门禁](../development/testing.md)。
