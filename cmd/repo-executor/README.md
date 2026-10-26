# repo-executor：Repo Executor 进程

[中文](README.md) | [English](README.en.md)

组合 `internal/repoexec`、`cloneauth` 与 Credential Store 的兑换客户端（`approved` 的 ADR `cloud/repo-executor/0`）。无状态，
不连数据库。启动时清扫工作根目录；收到 SIGINT/SIGTERM 后取消进行中的任务（它们仍会销毁任务目录与凭据材料并写出结果），
最多等待 30 秒。

| 环境变量 | 默认值 / 用途 |
|---|---|
| `REPO_EXECUTOR_ADDR` | `:8471`，任务监听（mTLS） |
| `REPO_EXECUTOR_HEALTH_ADDR` | `:8084`，健康监听（明文，只在部署网络内），返回出口配置的 schema 与 digest |
| `REPO_EXECUTOR_CERT_FILE` / `KEY_FILE` | `spiffe://ora.local/repo-executor` 服务证书；用于任务监听、Git Server 与 Credential Store |
| `REPO_EXECUTOR_CLIENT_CA_FILE` | Cloud dispatcher 的 CA |
| `REPO_EXECUTOR_AUTHORIZATION_PUBLIC_KEYS_FILE` | Cloud 执行授权的公钥集合（PEM，可多块） |
| `REPO_EXECUTOR_GIT_SERVER_URL` / `GIT_SERVER_CA_FILE` | `https://ora-git-service:8447`，服务仓库监听及其 CA |
| `REPO_EXECUTOR_CREDENTIAL_STORE_URL` / `CREDENTIAL_STORE_CA_FILE` | `https://credential-store:8461`，兑换监听及其 CA |
| `REPO_EXECUTOR_WORK_ROOT` | `/var/lib/repo-executor/tasks`（tmpfs） |
| `REPO_EXECUTOR_HELPER_PATH` | `/usr/local/bin/git-credential-ora` |
| `REPO_EXECUTOR_GIT_PROGRAM` | `git`（需 ≥ 2.37，才有 `http.curloptResolve`） |
| `REPO_EXECUTOR_SLOTS` | `4`，并发任务数 |
| `REPO_EXECUTOR_MAX_TASK_BYTES` | 2 GiB，单个任务目录上限 |
| `REPO_EXECUTOR_SYNC_TIMEOUT_MAX` / `PUBLISH_TIMEOUT_MAX` / `PROBE_TIMEOUT_MAX` | `30m` / `5m` / 同 publish |
| `REPO_EXECUTOR_DEPLOYMENT_EGRESS_FILE`（或 `ORA_DEPLOYMENT_EGRESS_FILE`、`-deployment-egress`） | 共享的 `deployment-egress.json`，须为绝对路径；带前缀的变量优先，启动时读取一次，读取失败拒绝启动 |
| `REPO_EXECUTOR_EGRESS_DENY_HOSTS` | 逗号分隔的内部主机名（精确或父域匹配）；Git Server 与 Credential Store 的主机名总是被拒绝 |
| `REPO_EXECUTOR_DEVELOPMENT` | `false`；`true` 时启用隔离测试栈的精确 host 例外（仅 public 模式，验收 fixture） |
| `REPO_EXECUTOR_DEVELOPMENT_ALLOW_HOSTS` | 逗号分隔的精确 fixture 主机名；未开 `DEVELOPMENT` 时拒绝启动 |
| `REPO_EXECUTOR_EXTERNAL_CA_FILE` | 空；设置后替代系统信任，仅供带自有 CA 的验收 fixture |
| `REPO_EXECUTOR_LOG_LEVEL` | `info` |

出口规则来自共享的 `internal/egress`：public 模式经部署 DNS 解析，要求全部地址都是公网地址；enterprise 模式只接受部署列出的目标。
执行器把每次解析、检查过的地址经 `http.curloptResolve` 交给 Git，每个外部 Git 子进程各自重新解析一次。

`-check-egress` 只校验出口文件并输出 `{"schemaVersion":1,"digest":…}`，不读其他配置、不连数据库，供 `ora apply`/`ora check`
在替换文件前验证候选配置。健康监听的 `X-Ora-Egress-Schema` / `X-Ora-Egress-Digest` 供 `ora verify` 比对。

以非 root 运行，容器 `init: true`，只接入 `git-services` 与 `repo-egress`（ADR D9），`deployment-egress.json` 只读挂载。
运行 `go test ./cmd/repo-executor/...` 与 `task build`。
