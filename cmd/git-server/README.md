# git-server：Project 仓库的 Git Server

[中文](README.md) | [English](README.en.md)

独立于 Cloud 业务进程运行，组合 `internal/gitserver`、Core 和 PostgreSQL（`proposed` 的 ADR
`cloud/git-server/0`）。只读取 Cloud 配置中的数据库和本服务 TLS 的部署文件引用，不保存任何授权状态。
启动先验证迁移；停止时先排空三个监听，再关闭 pre-receive hook 通道。Git 也以 `-pre-receive` 把本程序作为
pre-receive hook 运行。

| 环境变量 | 默认值 / 用途 |
|---|---|
| `GIT_SERVER_REPOSITORY_ADDR` | `:8445`，面向沙盒的 smart HTTP，只接入沙盒网络 |
| `GIT_SERVER_SERVICE_REPOSITORY_ADDR` | `:8447`，面向 Repo Executor 的 smart HTTP，只接入内部服务网络 |
| `GIT_SERVER_REF_ADDR` | `:8446`，Cloud 的只读 ref 查询和 `/healthz`，只接入默认网络 |
| `GIT_SERVER_REPOSITORY_ROOT` | `/var/lib/ora-git/repositories`，bare 仓库所在的持久卷 |
| `GIT_SERVER_GIT_PROGRAM` | `git` |
| `GIT_SERVER_CERTIFICATE_FILE` / `PRIVATE_KEY_FILE` | `/etc/ora-git-management/server.pem` / `server.key` |
| `GIT_SERVER_SANDBOX_CA_FILE` | `/etc/ora-git-management/ca.pem`，git-access 客户端证书的 CA |
| `GIT_SERVER_EXECUTOR_CA_FILE` | `/etc/ora-git-management/repo-executor-ca.pem`，Repo Executor 服务证书的 CA，与沙盒 CA 分开 |
| `GIT_SERVER_CLOUD_CLIENT_CERT_FILE` | `/etc/ora-git-management/cloud-client.pem`，唯一允许查询 ref 的叶证书 |
| `GIT_SERVER_HEALTH_SERVER_NAME` | `git-server`，`-healthcheck` 校验的 TLS 名称 |

`-healthcheck` 访问 ref 监听所配置主机（未配置时为回环地址）上的 `/healthz`，用部署 CA 校验服务端证书，不读取私钥。镜像需要带 `git http-backend` 的
`git`（Alpine 为 `git` 与 `git-daemon`），缺少时启动失败。运行包测试、`task check`、`task test:race` 及 `task build`。
