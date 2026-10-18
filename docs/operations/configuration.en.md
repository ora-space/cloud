# Configuration Reference

English | [中文](configuration.md)

Each process in this repository has its own configuration sources:

| Process | Configuration sources |
|---|---|
| `cmd/server`, `cmd/cloudctl` | A file in the `configs/config.yaml` format, plus environment variables prefixed `CLOUD_` |
| `cmd/gateway` | A file in the `configs/gateway.yaml` format, plus environment variables prefixed `GATEWAY_` |
| `cmd/model-gateway` | Cloud's configuration file (only the database and verification keys are read), plus environment variables prefixed `MODEL_GATEWAY_` |
| `cmd/git-server` | Cloud's configuration file (only the database is read), plus environment variables prefixed `GIT_SERVER_` |

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
| `plugins.marketplace_url`, `marketplace_branch`, `sync_interval` | — | Required when sync is on: an `https` Git URL (no query or fragment), a valid branch name, a positive interval |
| `plugins.asset_import_enabled`, `asset_import_interval` | —, `30s` | Workflow plugin asset import |
| `issue_runs.thread_idle_timeout` | `15m` | The Thread idle window; non-positive values take the default |
| `issue_runs.agent_start_timeout`, `agent_restore_timeout`, `agent_end_confirm_timeout` | `2m`, `6m`, `5m` | Startup windows; an explicit non-positive value stops startup, see [Bounded startup](../agent-runs/startup-settlement.en.md) |
| `issue_runs.agent_workspace_blocked_timeout` | `30m` | The give-up window for a run Workspace creation stuck in `blocked`; validated like the startup windows |
| `issue_runs.delivery_give_up_after`, `delivery_unreachable_after` | `2h`, `30m` | Give-up limits for delivery and deletion; non-positive values take the defaults |
| `object_store.*` | `enabled: false` | See [Revisions](../agent-runs/revisions.en.md#object-storage): `endpoint`, `public_endpoint`, `region`, `bucket`, `path_style`, `access_key_id_file`, `secret_access_key_file`, `upload_grant_ttl` |

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
| `login.development_provider` | `false` | Registers the local development login; only allowed together with `public.development` |
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

## Local development

Local development does not edit these files directly: `config.toml` (copied from `config.toml.template` and ignored by
Git) is turned by `task setup` into keys, secret files and `.local/dev.env`, which overrides `configs/*.yaml` with
`CLOUD_*` and `GATEWAY_*` environment variables and is loaded by `Taskfile.yml` for every task. See
[Local development](../development/local-development.en.md).
