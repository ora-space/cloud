# Configuration Reference

English | [中文](configuration.md)

Each process in this repository has its own configuration sources:

| Process | Configuration sources |
|---|---|
| `cmd/server`, `cmd/cloudctl` | A file in the `configs/config.yaml` format, plus environment variables prefixed `CLOUD_` |
| `cmd/gateway` | A file in the `configs/gateway.yaml` format, plus environment variables prefixed `GATEWAY_` |
| `cmd/model-gateway` | Cloud's configuration file (only the database and verification keys are read), plus environment variables prefixed `MODEL_GATEWAY_` |
| `cmd/git-server` | Cloud's configuration file (only the database is read), plus environment variables prefixed `GIT_SERVER_` |
| `cmd/repo-executor` | Environment variables prefixed `REPO_EXECUTOR_` only |
| `cmd/credential-store` | Environment variables prefixed `CREDENTIAL_STORE_` only |
| `cmd/git-credential-ora` | No configuration; the Repo Executor invokes it with one task's credential material directory |

## General rules

- **File location**: set with `-config <path>`; when omitted, `config.yaml` (`gateway.yaml` for the Gateway) is looked
  up in `./configs`, `../configs` and `.` in that order.
- **Environment overrides**: replace the `.` in a key path with `_` and add the prefix, e.g. `database.dsn` becomes
  `CLOUD_DATABASE_DSN` and `login.provider` becomes `GATEWAY_LOGIN_PROVIDER`. The variable takes effect even when the
  file does not mention the key. Lists and maps (such as `auth.keys`) cannot be set from a single variable.
- **Durations** use Go duration syntax (`30s`, `15m`, `2h`).
- **Secrets appear only as file references**: private keys, certificates and credentials are written as file paths;
  processes read the contents into memory at startup and never put them into configuration values, environment
  variables or logs.
- **Validated at startup**: an invalid configuration, an unreadable file, a wrong migration state, or missing
  certificates or trust configuration make the process exit with a non-zero status instead of running half-configured.

## Cloud server

| Key | Default | Description |
|---|---|---|
| `server.port` | `8080` | HTTP listen port |
| `server.mode` | — | Gin mode: `debug`, `release`, `test` |
| `server.read_timeout`, `server.write_timeout` | — | Must be positive |
| `logger.*` | — | `level`, `filename`, `max_size` (MB), `max_backups`, `max_age` (days), `compress`, `enable_console`; structured JSON logs rotated by size |
| `database.dsn` | — | PostgreSQL connection string; production uses TLS and an account with DML rights only |
| `database.max_idle_conns`, `max_open_conns`, `conn_max_lifetime` | — | Connection pool; `conn_max_lifetime` must be positive |
| `auth.audience` | — | The audience of internal JWTs |
| `auth.keys[]` | — | Trusted public keys: `id`, `issuer`, `kind` (`service` or `user`), `role` (service only), `public_key_file`. Startup fails when empty; see [Internal credentials](../identity/authentication.en.md) |
| `control.grpc_addr` | Required | The Controller control listener address |
| `control.certificate_file`, `private_key_file`, `client_ca_file`, `controller_identity` | Required | TLS 1.3 mutual authentication for the control listener; the identity is `spiffe://<trust domain>/controller/<id>` |
| `directory.endpoint`, `hw_id`, `environment`, `app_key_file` | Empty | The corporate employee directory; an empty `endpoint` means a public deployment, see [Users, tenants and members](../identity/tenancy.en.md#joining-a-tenant) |
| `collaboration.development_fixtures` | `false` | Development fixtures; production must keep it off, see [Issue collaboration](../collaboration/interactions.en.md#ports-issues-own-only-the-consuming-side-contract) |
| `plugins.sync_enabled` | — | Whether to sync the plugin marketplace |
| `plugins.personal_agent_target` | Empty | Declared personal-Agent deployment target; empty disables it. Supports `x86_64-unknown-linux-gnu` or `aarch64-unknown-linux-gnu`; enable only after adapter rollout |
| `plugins.marketplace_url`, `marketplace_branch`, `sync_interval` | — | Required when sync is on: an `https` Git URL (no query or fragment), a valid branch name, a positive interval |
| `plugins.asset_import_enabled`, `asset_import_interval` | —, `30s` | Workflow plugin asset import |
| `issue_runs.thread_idle_timeout` | `15m` | The Thread idle window; non-positive values take the default |
| `issue_runs.agent_start_timeout`, `agent_restore_timeout`, `agent_end_confirm_timeout` | `2m`, `6m`, `5m` | Startup windows; an explicit non-positive value stops startup, see [Bounded startup](../agent-runs/startup-settlement.en.md) |
| `issue_runs.agent_workspace_blocked_timeout` | `30m` | The give-up window for a run Workspace creation stuck in `blocked`; validated like the startup windows |
| `issue_runs.delivery_give_up_after`, `delivery_unreachable_after` | `2h`, `30m` | Give-up limits for delivery and deletion; `delivery_unreachable_after` is also the window that decides whether a Node is still there, and an in-flight workflow session settles on it; non-positive values take the defaults |
| `workflow.session_turn_timeout` | `2h` | The initial turn's window for a workflow session: once the execution row's `created_at` is older than this with neither a committed result nor a successful `turnEnded`, the node fails as `session_turn_deadline_exceeded`; timed per execution row, non-positive values take the default, see [Workflows](../workflows/workflows.en.md#scheduling) |
| `object_store.*` | `enabled: false` | See [Revisions](../agent-runs/revisions.en.md#object-storage): `endpoint`, `public_endpoint`, `region`, `bucket`, `path_style`, `access_key_id_file`, `secret_access_key_file`, `upload_grant_ttl` |
| `git_server.repository_base_url` | empty | The Git Server repository base URL sandboxes reach (`https`, no userinfo, query, fragment or trailing slash), written into clone, session and delivery inputs; empty runs without a Git Server: clone steps stay `blocked` (`git_server_unconfigured`) and deliveries settle as `skipped` |
| `git_server.ref_query_url`, `ca_file`, `client_cert_file`, `client_key_file` | empty | Required with `repository_base_url`: the ref query listener's `https://host[:port]`, the CA to verify it, and Cloud's client certificate and private key files. Startup only reads the files and makes no network call; an unreachable Git Server or a mismatched certificate only makes verification unavailable and never stops startup. See [Git Server](../repositories/git-server.en.md#ref-queries) |
| `git_server.ref_query_timeout` | `10s` | Timeout of one ref query; a non-positive value takes the default |
| `repository.enabled` | `false` | Enables the external Git line and the credential routes; with it off Projects are still created, but no sync runs and credential creation answers `503`; see [The external Git line](../repositories/external-git.en.md) |
| `repository.executor_url`, `executor_ca_file`, `dispatcher_cert_file`, `dispatcher_key_file` | URL defaults to `https://repo-executor:8471`; files required | The Repo Executor's task listener and Cloud's dispatcher mTLS client identity |
| `repository.authorization_key_file` | Required | The Ed25519 (PKCS#8) key signing execution authorizations; must differ from `submission_key_file` |
| `repository.credential_store_url`, `credential_store_ca_file`, `manager_cert_file`, `manager_key_file` | URL defaults to `https://credential-store:8462`; files required | The Credential Store management interface and Cloud's manager mTLS client identity |
| `repository.submission_key_file` | Required | The Ed25519 (PKCS#8) key signing browser submission grants |
| `repository.credential_ingress_url` | Required | The Credential Store origin for browser submission, returned with every submission grant |
| `repository.git_refs_url`, `git_refs_ca_file`, `git_refs_cert_file`, `git_refs_key_file` | URL defaults to `https://git-server:8446`; files required | The Git Server's read-only ref query and Cloud's fixed client certificate |
| `repository.slots`, `lease`, `sync_timeout`, `publish_timeout` | `4`, `1m`, `30m`, `5m` | Concurrent tasks, lease, sync bound and publish bound; all must be positive |

With `repository.enabled` true, startup checks that the two private keys differ, that the ingress URL is set, and loads
three sets of mTLS client material (executor, Credential Store, Git Server) without making any network call.

Both sections reach the same Git Server ref query listener (`GET /internal/v1/refs`), each with its own client: `git_server.*`
serves Workspace clone inputs and Revision delivery verification, and `repository.git_refs_*` serves the repository worker,
publish source checks and the Workspace branch list.

`cloudctl` reads the same configuration but only uses the database connection; it needs a migration account with rights
to create tables.

## Gateway

| Key | Default | Description |
|---|---|---|
| `server.*`, `logger.*`, `database.*` | — | As for the Cloud server; the database uses a dedicated role with rights on the two Gateway tables only |
| `public.base_url` | — | The origin browsers see; must be `https`, with loopback `http` allowed only when `public.development: true` |
| `public.development` | `false` | Local development: allows loopback HTTP and drops `Secure` from cookies |
| `session.ttl` | `12h` for IDaaS, `30 days` otherwise | Absolute lifetime, at most 90 days |
| `session.cleanup_interval`, `retention`, `cleanup_batch` | `10m`, `168h`, `500` | Cleanup of expired sessions |
| `login.provider` | `github` | The external provider, `huawei-idaas` or `github`; only its secret is read |
| `login.development_provider` | `false` | Registers local development login; configuration validation requires `public.development: true` and a loopback HTTP `public.base_url` origin (with an optional port); HTTPS is rejected even with development mode enabled |
| `login.attempt_ttl` | `10m` | Login attempt lifetime, 1 minute to 1 hour |
| `login.rate_limit_per_minute`, `rate_limit_burst` | `30`, same as the former | Rate limits for login start and callback |
| `login.pkce_key_file` | Required | At least 32 random bytes; identical on every replica |
| `cloud.upstream`, `cloud.timeout` | —, `30s` | The fixed Cloud upstream (no path or query) |
| `tokens.issuer`, `audience`, `service_subject` | — | Claims of internal JWTs |
| `tokens.service_key_id`, `service_private_key_file`, `user_key_id`, `user_private_key_file` | — | Two different Ed25519 private keys (PKCS#8 PEM) |
| `tokens.lifetime` | `1m` | At most 5 minutes |
| `github.client_id`, `client_secret_file` | — | The GitHub OAuth App; `authorize_url`, `token_url`, `user_url` and `source` can point at GitHub Enterprise Server |
| `idaas.base_url`, `client_id`, `client_secret_file`, `display_name_field`, `timeout` | —, —, —, —, `10s` | Huawei IDaaS |
| `idaas.authorize_url`, `token_url`, `userinfo_url` | Empty | Independent full HTTPS endpoint overrides; URL credentials, queries and fragments are rejected |
| `web.dist_dir` | Empty | The frontend build directory; when empty no frontend is served |
| `model_credentials.upstream`, `ca_file` | Empty | The fixed HTTPS model-gateway address and its CA; both must be set together |

`GATEWAY_IDAAS_AUTHORIZE_URL`, `GATEWAY_IDAAS_TOKEN_URL` and `GATEWAY_IDAAS_USERINFO_URL` override those three endpoints
respectively. Each omitted endpoint retains `GATEWAY_IDAAS_BASE_URL` plus its `/saaslogin1/oauth2/v1/authorize`, `token`
or `userinfo` path. These values are deployment-owned, never selected by the browser; see
[Gateway](../identity/gateway.en.md) for the authentication protocol and identity mapping. For configuration changes and
secret rotation, see [Deployment and operations](deployment.en.md#huawei-idaas-site-configuration).

## model-gateway

Environment variables are listed in [model-gateway](../models/model-gateway.en.md#startup-and-shutdown). It reads
`database.*` and `auth.*` from Cloud's configuration file, using the latter to verify credential writes forwarded by the
Gateway.

## Shared egress

The Cloud server, model-gateway and the Repo Executor point at one out-of-package JSON file. The first two take
`-deployment-egress <path>` or `ORA_DEPLOYMENT_EGRESS_FILE`, and the flag wins; the Repo Executor reads the environment
only, where `REPO_EXECUTOR_DEPLOYMENT_EGRESS_FILE` wins over `ORA_DEPLOYMENT_EGRESS_FILE`, and requires an absolute path.
Omitting it keeps public mode. Public DNS remains `MODEL_GATEWAY_DNS_HTTPS_URL` and
`MODEL_GATEWAY_DNS_BOOTSTRAP_IPS`, and all three processes must receive the same values. The file format, the enterprise
allowlist and how to replace the file are in [Shared deployment egress](deployment-egress.en.md). This is not a key in
`configs/config.yaml`.

## git-server

The full description is in [`cmd/git-server`](../../cmd/git-server/README.en.md):

| Environment variable | Default | Description |
|---|---|---|
| `GIT_SERVER_REPOSITORY_ADDR` | `:8445` | The sandbox-facing repository listener, on the sandbox network only |
| `GIT_SERVER_SERVICE_REPOSITORY_ADDR` | `:8447` | The Repo Executor-facing repository listener, on the internal service network only |
| `GIT_SERVER_REF_ADDR` | `:8446` | Cloud's ref queries and `/healthz` |
| `GIT_SERVER_REPOSITORY_ROOT` | `/var/lib/ora-git/repositories` | The persistent volume holding the bare repositories |
| `GIT_SERVER_CERTIFICATE_FILE`, `_PRIVATE_KEY_FILE` | `/etc/ora-git-management/server.pem`, `server.key` | The service certificate |
| `GIT_SERVER_SANDBOX_CA_FILE`, `_EXECUTOR_CA_FILE` | `/etc/ora-git-management/ca.pem`, `repo-executor-ca.pem` | The CAs for the two kinds of client certificate |
| `GIT_SERVER_CLOUD_CLIENT_CERT_FILE` | `/etc/ora-git-management/cloud-client.pem` | The only leaf certificate allowed to query refs |

## Repo Executor and Credential Store

The full descriptions are in [`cmd/repo-executor`](../../cmd/repo-executor/README.en.md) and
[`cmd/credential-store`](../../cmd/credential-store/README.en.md). Both run as non-root and do not connect to Cloud's
database; keys, certificates and public keys are mounted only as read-only files.

| Environment variable | Default | Description |
|---|---|---|
| `REPO_EXECUTOR_ADDR` | `:8471` | The task listener (mTLS), accepting only Cloud's dispatcher identity |
| `REPO_EXECUTOR_CERT_FILE`, `_KEY_FILE` | Required | The executor service certificate, used for the task listener, the Git Server and the Credential Store |
| `REPO_EXECUTOR_CLIENT_CA_FILE` | Required | The CA that issued Cloud's dispatcher client certificate |
| `REPO_EXECUTOR_AUTHORIZATION_PUBLIC_KEYS_FILE` | Required | Cloud's execution public keys (PEM, one or more blocks) |
| `REPO_EXECUTOR_GIT_SERVER_URL`, `_GIT_SERVER_CA_FILE` | `https://ora-git-service:8447`, required | The Git Server service repository listener and its CA |
| `REPO_EXECUTOR_CREDENTIAL_STORE_URL`, `_CREDENTIAL_STORE_CA_FILE` | `https://credential-store:8461`, required | The Credential Store redemption listener and its CA |
| `REPO_EXECUTOR_WORK_ROOT` | `/var/lib/repo-executor/tasks` | The task directory; deployments mount a tmpfs and it is cleaned at startup |
| `REPO_EXECUTOR_HELPER_PATH` | `/usr/local/bin/git-credential-ora` | The only credential helper Git may use |
| `REPO_EXECUTOR_GIT_PROGRAM` | `git` | The Git executable (≥ 2.37, for `http.curloptResolve`) |
| `REPO_EXECUTOR_SLOTS`, `_MAX_TASK_BYTES` | `4`, 2 GiB | Concurrent tasks and the per-task directory bound |
| `REPO_EXECUTOR_SYNC_TIMEOUT_MAX`, `_PUBLISH_TIMEOUT_MAX`, `_PROBE_TIMEOUT_MAX` | `30m`, `5m`, same as publish | Bounds for the three task kinds |
| `REPO_EXECUTOR_HEALTH_ADDR` | `:8084` | Plaintext health listener, deployment network only; `GET /healthz` carries the egress schema and digest |
| `REPO_EXECUTOR_DEPLOYMENT_EGRESS_FILE` (or `ORA_DEPLOYMENT_EGRESS_FILE`) | Empty | The deployment-owned `deployment-egress.json`, an absolute path read once at startup; the prefixed variable wins. Empty keeps public mode |
| `REPO_EXECUTOR_EGRESS_DENY_HOSTS` | Empty | Comma-separated internal host names (exact or parent domain); the Git Server and Credential Store are always denied |
| `REPO_EXECUTOR_DEVELOPMENT`, `_DEVELOPMENT_ALLOW_HOSTS` | `false`, empty | Exact fixture-host exception in public mode only; listing hosts without `DEVELOPMENT` refuses to start |
| `REPO_EXECUTOR_EXTERNAL_CA_FILE` | Empty | A CA replacing system trust (acceptance fixtures only) |
| `REPO_EXECUTOR_LOG_LEVEL` | `info` | Log level |

| Environment variable | Default | Description |
|---|---|---|
| `CREDENTIAL_STORE_ROOT` | Required | The credential volume, an absolute path that must have mode `0700` |
| `CREDENTIAL_STORE_KEYRING_FILE` | Required | The read-only KEK keyring; startup fails when it is missing or invalid |
| `CREDENTIAL_STORE_CERT_FILE`, `_KEY_FILE` | Required | The service certificate of the three mTLS listeners |
| `CREDENTIAL_STORE_CLIENT_CA_FILE` | Required | The CA that issued executor, management and KEK-operator client certificates |
| `CREDENTIAL_STORE_EXECUTION_PUBLIC_KEYS_FILE`, `_SUBMISSION_PUBLIC_KEYS_FILE` | Required | Cloud's two verification key sets, which must differ |
| `CREDENTIAL_STORE_REDEMPTION_ADDR`, `_MANAGEMENT_ADDR`, `_KEY_ADDR` | `:8461`, `:8462`, `:8463` | Redemption, management and KEK-operator listeners |
| `CREDENTIAL_STORE_INGRESS_ADDR` | `:8460` | The browser ingress, forwarded directly by the edge reverse proxy |
| `CREDENTIAL_STORE_INGRESS_CERT_FILE`, `_INGRESS_KEY_FILE` | Required | The certificate of the browser ingress origin |
| `CREDENTIAL_STORE_INGRESS_ALLOWED_ORIGINS` | Required | Comma-separated frontend origins; CORS echoes exactly these |
| `CREDENTIAL_STORE_INGRESS_RATE_PER_MINUTE`, `_INGRESS_RATE_BURST` | `10`, `5` | Per-source-IP submission rate limit |
| `CREDENTIAL_STORE_LOG_LEVEL` | `info` | Log level |

The browser ingress rate-limits by the TCP source address; behind a reverse proxy every request shares the proxy's
address, so the proxy has to rate-limit per client itself.

## Local development

Local development does not edit these files directly: `config.toml` (copied from `config.toml.template` and ignored by
Git) is turned by `task setup` into keys, secret files and `.local/dev.env`, which overrides `configs/*.yaml` with
`CLOUD_*` and `GATEWAY_*` environment variables and is loaded by `Taskfile.yml` for every task. See
[Local development](../development/local-development.en.md). `task setup` does not generate certificates or keys for the
external Git line, and `repository.enabled` is `false` locally; enabling it means preparing the executor, Credential
Store and Git Server material described above.
