# repo-executor: the Repo Executor process

[中文](README.md) | [English](README.en.md)

Wires `internal/repoexec`, `cloneauth` and the Credential Store redemption client (proposed ADR `cloud/repo-executor/0`). Stateless,
no database. It sweeps the work root at startup; on SIGINT/SIGTERM it cancels in-flight tasks (which still destroy their task
directories and credential material and write their results) and waits up to 30 seconds.

| Variable | Default / purpose |
|---|---|
| `REPO_EXECUTOR_ADDR` | `:8471`, task listener (mTLS) |
| `REPO_EXECUTOR_CERT_FILE` / `KEY_FILE` | `spiffe://ora.local/repo-executor` service certificate, for the task listener, the Git Server and the Credential Store |
| `REPO_EXECUTOR_CLIENT_CA_FILE` | CA of Cloud's dispatcher |
| `REPO_EXECUTOR_AUTHORIZATION_PUBLIC_KEYS_FILE` | Cloud's execution-authorization public key set (PEM, several blocks allowed) |
| `REPO_EXECUTOR_GIT_SERVER_URL` / `GIT_SERVER_CA_FILE` | `https://ora-git-service:8447`, the service repository listener and its CA |
| `REPO_EXECUTOR_CREDENTIAL_STORE_URL` / `CREDENTIAL_STORE_CA_FILE` | `https://credential-store:8461`, the redemption listener and its CA |
| `REPO_EXECUTOR_WORK_ROOT` | `/var/lib/repo-executor/tasks` (tmpfs) |
| `REPO_EXECUTOR_HELPER_PATH` | `/usr/local/bin/git-credential-ora` |
| `REPO_EXECUTOR_GIT_PROGRAM` | `git` |
| `REPO_EXECUTOR_SLOTS` | `4` concurrent tasks |
| `REPO_EXECUTOR_MAX_TASK_BYTES` | 2 GiB per task directory |
| `REPO_EXECUTOR_SYNC_TIMEOUT_MAX` / `PUBLISH_TIMEOUT_MAX` / `PROBE_TIMEOUT_MAX` | `30m` / `5m` / same as publish |
| `REPO_EXECUTOR_EGRESS_ALLOW` | Comma-separated `host` or `host:port` that bypass the egress check (acceptance fixtures only) |
| `REPO_EXECUTOR_EGRESS_DENY_HOSTS` | Comma-separated internal host names (exact or parent domain); the Git Server and Credential Store hosts are always denied |
| `REPO_EXECUTOR_EXTERNAL_CA_FILE` | Empty; when set it replaces system trust, for fixtures with their own CA only |
| `REPO_EXECUTOR_LOG_LEVEL` | `info` |

Runs as non-root with `init: true`, on `repo-internal` and `repo-egress` only (ADR D9). Run `go test ./cmd/repo-executor/...` and `task build`.
