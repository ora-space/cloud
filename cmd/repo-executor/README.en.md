# repo-executor: the Repo Executor process

[中文](README.md) | [English](README.en.md)

Wires `internal/repoexec`, `cloneauth` and the Credential Store redemption client (approved ADR `cloud/repo-executor/0`). Stateless,
no database. It sweeps the work root at startup; on SIGINT/SIGTERM it cancels in-flight tasks (which still destroy their task
directories and credential material and write their results) and waits up to 30 seconds.

| Variable | Default / purpose |
|---|---|
| `REPO_EXECUTOR_ADDR` | `:8471`, task listener (mTLS) |
| `REPO_EXECUTOR_HEALTH_ADDR` | `:8084`, plaintext health listener (deployment network only) reporting the egress schema and digest |
| `REPO_EXECUTOR_CERT_FILE` / `KEY_FILE` | `spiffe://ora.local/repo-executor` service certificate, for the task listener, the Git Server and the Credential Store |
| `REPO_EXECUTOR_CLIENT_CA_FILE` | CA of Cloud's dispatcher |
| `REPO_EXECUTOR_AUTHORIZATION_PUBLIC_KEYS_FILE` | Cloud's execution-authorization public key set (PEM, several blocks allowed) |
| `REPO_EXECUTOR_GIT_SERVER_URL` / `GIT_SERVER_CA_FILE` | `https://ora-git-service:8447`, the service repository listener and its CA |
| `REPO_EXECUTOR_CREDENTIAL_STORE_URL` / `CREDENTIAL_STORE_CA_FILE` | `https://credential-store:8461`, the redemption listener and its CA |
| `REPO_EXECUTOR_WORK_ROOT` | `/var/lib/repo-executor/tasks` (tmpfs) |
| `REPO_EXECUTOR_HELPER_PATH` | `/usr/local/bin/git-credential-ora` |
| `REPO_EXECUTOR_GIT_PROGRAM` | `git` (≥ 2.37, for `http.curloptResolve`) |
| `REPO_EXECUTOR_SLOTS` | `4` concurrent tasks |
| `REPO_EXECUTOR_MAX_TASK_BYTES` | 2 GiB per task directory |
| `REPO_EXECUTOR_SYNC_TIMEOUT_MAX` / `PUBLISH_TIMEOUT_MAX` / `PROBE_TIMEOUT_MAX` | `30m` / `5m` / same as publish |
| `REPO_EXECUTOR_DEPLOYMENT_EGRESS_FILE` (or `ORA_DEPLOYMENT_EGRESS_FILE`, `-deployment-egress`) | The shared `deployment-egress.json`, an absolute path; the prefixed variable wins, it is read once, and a load failure keeps the process from starting |
| `REPO_EXECUTOR_EGRESS_DENY_HOSTS` | Comma-separated internal host names (exact or parent domain); the Git Server and Credential Store hosts are always denied |
| `REPO_EXECUTOR_DEVELOPMENT` | `false`; `true` enables the isolated test stack's exact-host exception (public mode only, acceptance fixtures) |
| `REPO_EXECUTOR_DEVELOPMENT_ALLOW_HOSTS` | Comma-separated exact fixture host names; listing them without `DEVELOPMENT=true` is a startup error |
| `REPO_EXECUTOR_EXTERNAL_CA_FILE` | Empty; when set it replaces system trust, for fixtures with their own CA only |
| `REPO_EXECUTOR_LOG_LEVEL` | `info` |

The egress rules come from the shared `internal/egress`: public mode resolves through the deployment DNS and requires every
address to be public; enterprise mode admits only the deployment's listed targets. The executor hands each resolved, checked
address set to Git through `http.curloptResolve`, and every external Git subprocess resolves again.

`-check-egress` validates only the egress file and prints `{"schemaVersion":1,"digest":…}`, reading no other configuration and
touching no database, so `ora apply`/`ora check` can validate a candidate file before replacing it. The health listener's
`X-Ora-Egress-Schema` / `X-Ora-Egress-Digest` headers are what `ora verify` compares.

Runs as non-root with `init: true`, on `git-services` and `repo-egress` only (ADR D9), with `deployment-egress.json` mounted
read-only. Run `go test ./cmd/repo-executor/...` and `task build`.
