# repo-executor：Repo Executor 进程

[中文](README.md) | [English](README.en.md)

组合 `internal/repoexec`、`cloneauth` 与 Credential Store 的兑换客户端（`proposed` 的 ADR `cloud/repo-executor/0`）。无状态，
不连数据库。启动时清扫工作根目录；收到 SIGINT/SIGTERM 后取消进行中的任务（它们仍会销毁任务目录与凭据材料并写出结果），
最多等待 30 秒。

| 环境变量 | 默认值 / 用途 |
|---|---|
| `REPO_EXECUTOR_ADDR` | `:8471`，任务监听（mTLS） |
| `REPO_EXECUTOR_CERT_FILE` / `KEY_FILE` | `spiffe://ora.local/repo-executor` 服务证书；用于任务监听、Git Server 与 Credential Store |
| `REPO_EXECUTOR_CLIENT_CA_FILE` | Cloud dispatcher 的 CA |
| `REPO_EXECUTOR_AUTHORIZATION_PUBLIC_KEYS_FILE` | Cloud 执行授权的公钥集合（PEM，可多块） |
| `REPO_EXECUTOR_GIT_SERVER_URL` / `GIT_SERVER_CA_FILE` | `https://ora-git-service:8447`，服务仓库监听及其 CA |
| `REPO_EXECUTOR_CREDENTIAL_STORE_URL` / `CREDENTIAL_STORE_CA_FILE` | `https://credential-store:8461`，兑换监听及其 CA |
| `REPO_EXECUTOR_WORK_ROOT` | `/var/lib/repo-executor/tasks`（tmpfs） |
| `REPO_EXECUTOR_HELPER_PATH` | `/usr/local/bin/git-credential-ora` |
| `REPO_EXECUTOR_GIT_PROGRAM` | `git` |
| `REPO_EXECUTOR_SLOTS` | `4`，并发任务数 |
| `REPO_EXECUTOR_MAX_TASK_BYTES` | 2 GiB，单个任务目录上限 |
| `REPO_EXECUTOR_SYNC_TIMEOUT_MAX` / `PUBLISH_TIMEOUT_MAX` / `PROBE_TIMEOUT_MAX` | `30m` / `5m` / 同 publish |
| `REPO_EXECUTOR_EGRESS_ALLOW` | 逗号分隔的 `host` 或 `host:port`，跳过出口检查（仅验收 fixture） |
| `REPO_EXECUTOR_EGRESS_DENY_HOSTS` | 逗号分隔的内部主机名（精确或父域匹配）；Git Server 与 Credential Store 的主机名总是被拒绝 |
| `REPO_EXECUTOR_EXTERNAL_CA_FILE` | 空；设置后替代系统信任，仅供带自有 CA 的验收 fixture |
| `REPO_EXECUTOR_LOG_LEVEL` | `info` |

以非 root 运行，容器 `init: true`，只接入 `repo-internal` 与 `repo-egress`（ADR D9）。运行 `go test ./cmd/repo-executor/...` 与 `task build`。
