# git-server：沙盒 Git Server

[中文](README.md) | [English](README.en.md)

独立于 Cloud 业务进程运行，组合 `internal/gitserver`、Core 和 PostgreSQL（`proposed` 的 ADR
`cloud/git-server/0`）。一个进程、两个面：**仓库面**向沙盒提供 Project 仓库，并为 Cloud 提供只读的交付 ref 查询；
**镜像面**向 Workspace 的 Node 提供已发布的私仓镜像。两面各有监听与信任模型，互不可达，理由见
`internal/gitserver/README.md`。

只读取 Cloud 配置中的数据库、本服务 TLS 的部署文件引用与镜像卷，不保存任何授权状态。启动先验证迁移；停止时先排空
所有监听，再关闭 pre-receive hook 通道。Git 也以 `-pre-receive` 把本程序作为 pre-receive hook 运行。

仓库面默认开启，而且这个默认方向是 fail-closed 的：没被明确关掉的部署必须提供它的材料，否则在启动时被拒。
`GIT_SERVER_REPOSITORY_ENABLED=false` 是一个部署在说「这里没有任何 git-access 签发方」时该说的话，它不放松任何
保留面上的东西。镜像面默认关闭，用 `-mirror-root` 命名镜像卷就是打开它；打开后它拒绝半配置启动——原点解析器、
调用解析器所用的身份、以及本面自己的 TLS 证书对必须同时齐备，缺一不出。

| 环境变量 | 默认值 / 用途 |
|---|---|
| `GIT_SERVER_REPOSITORY_ENABLED` | `true`；为 `false` 时本进程不提供服务仓库：不为它开监听，也不建 hook 通道 |
| `GIT_SERVER_REPOSITORY_ADDR` | `:8445`，面向沙盒的 smart HTTP，只接入沙盒网络 |
| `GIT_SERVER_REF_ADDR` | `:8446`，Cloud 的只读交付 ref 查询和 `/healthz`，只接入默认网络 |
| `GIT_SERVER_REPOSITORY_ROOT` | `/var/lib/ora-git/repositories`，bare 仓库所在的持久卷 |
| `GIT_SERVER_GIT_PROGRAM` | `git`；两个面跑同一个二进制，由本进程解析一次 |
| `GIT_SERVER_CERTIFICATE_FILE` / `PRIVATE_KEY_FILE` | `/etc/ora-git-management/server.pem` / `server.key` |
| `GIT_SERVER_SANDBOX_CA_FILE` | `/etc/ora-git-management/ca.pem`，git-access 客户端证书的 CA |
| `GIT_SERVER_CLOUD_CLIENT_CERT_FILE` | `/etc/ora-git-management/cloud-client.pem`，唯一允许查询 ref 的叶证书 |
| `GIT_SERVER_HEALTH_SERVER_NAME` | `git-server`，`-healthcheck` 校验的 TLS 名称 |

| 参数 | 用途 |
|---|---|
| `-config` | Cloud 配置，提供两面判定所用的数据库 |
| `-pre-receive` | Git 以 hook 身份运行本程序；由 `git` 自己调用，不手工传 |
| `-healthcheck` | 访问 ref 监听的 `/healthz` 并校验服务端证书；仓库面被关掉时本参数直接失败，因为那个监听不存在 |
| `-mirror-addr` / `-mirror-root` | 镜像监听地址（默认 `127.0.0.1:18002`）与镜像卷；命名卷即打开镜像面 |
| `-origin-url` / `-identity-dir` / `-origin-server-name` | 原点解析器地址、本服务调用它所用的身份目录（`client.pem`/`client.key`/`ca.pem`/`origin.pem`）、其证书必须携带的名字 |
| `-mirror-tls-cert` / `-mirror-tls-key` | 镜像监听出示的证书对；本面的唯一客户端只认 https，因此两者必填 |
| `-mirror-timeout` / `-origin-timeout` | 一次 clone、一次原点解析的时间上界 |
| `-max-mirror-bytes` / `-max-volume-bytes` | 单个镜像与镜像卷的容量上界；触界使该镜像失败而不是发布半份内容 |
| `-mirror-grace` | 不再被引用后仍保留多久才可回收 |
| `-mirror-reclaim` / `-mirror-reclaim-interval` | 是否运行回收循环及其周期；默认关闭 |

`-healthcheck` 访问 ref 监听所配置主机（未配置时为回环地址）上的 `/healthz`，用部署 CA 校验服务端证书，不读取私钥。
镜像需要带 `git http-backend` 的 `git`（Alpine 为 `git` 与 `git-daemon`），缺少时启动失败；只服务镜像面的部署不检查
这一项，因为它不跑 `http-backend`。运行包测试、`task check`、`task test:race` 及 `task build`。
