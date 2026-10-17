# internal/config: Configuration Loader & Validation

[中文](README.md) | [English](README.en.md)

`internal/config` manages configuration parsing, schema validation, and environment variable overrides for Ora Cloud.

## Responsibilities

- **Structured configuration**: Defines strongly-typed Go structs mapping the entire system configuration:
  - `ServerConfig`: Port, Gin mode, read/write timeouts.
  - `LoggerConfig`: Log level, file paths, rotation thresholds (max size, age, backups, gzip compression).
  - `DatabaseConfig`: Driver (must be `postgres`), DSN, and connection pool limits (`max_open_conns`, `max_idle_conns`, `conn_max_lifetime`).
  - `AuthConfig`: Expected token audience and list of `TrustedKey` verification parameters.
- **Hierarchical loading via Viper**:
  - Searches for `config.yaml` in `./configs`, `../configs`, and `.`.
  - Supports explicit file path overriding via `-config <path>`.
  - Maps environment variables with the `CLOUD_` prefix, replacing dots with underscores (e.g., `CLOUD_DATABASE_DSN` overrides `database.dsn`).
- **Startup sanity validation**: Rejects invalid configurations with explicit errors, requiring positive durations for `read_timeout`, `write_timeout`, and `conn_max_lifetime`.

## Boundaries and invariants

Startup windows use database time. Confirmation expiry settles business state while retaining independent residual responsibility; a business terminal does not prove physical release. See [Startup settlement](../../docs/agent-startup-settlement.en.md) for defaults and recovery boundaries.

The optional `object_store` section configures S3 delivery. When enabled, the server loads separately mounted access/secret key files into memory and validates private/public endpoints, region, bucket, addressing mode and a TTL between one second and seven days. All keys support `CLOUD_OBJECT_STORE_*` overrides. Short-lived PUT grants can bind SHA-256; Cloud verifies existence, size and stored checksum through private HEAD before registering Revision. Disabled by default, delivery settles skipped/object_store_unconfigured through the caller's transaction without dispatch; standalone grant requests return unavailable. Health reports configured/unconfigured, not S3 availability.

The optional `git_server` section configures the Git Server: `repository_base_url` (the sandbox-facing repository base, https without userinfo, query, fragment or trailing slash; a Project's repository is `<base>/<tenantId>/<projectId>.git`, written into clone, session and delivery inputs), `ref_query_url` (the ref query listener, `https://host[:port]`), `ca_file`, `client_cert_file`, `client_key_file` (Cloud's own client certificate; the private key stays a file reference) and `ref_query_timeout` (default 10s). All keys support `CLOUD_GIT_SERVER_*` overrides. With `repository_base_url` set the other four are required; startup only validates and loads the files and makes no network call, so an unreachable Git Server only makes verification unavailable. An empty `repository_base_url` means no Git Server: clone steps block as git_server_unconfigured and deliveries settle skipped/git_server_unconfigured. Health reports `gitServer` configured/unconfigured.

- **No secret storage**: Configuration files store only public verification keys and infrastructure references. Plaintext deployment secrets and private keys must never appear in configuration files.
- **Immutable runtime**: Configurations are loaded once at command startup and passed as ready-to-use values. There is no global mutable configuration singleton.

See [config.yaml](../../configs/config.yaml), [cmd/server](../../cmd/server/README.en.md), and [Authentication](../../docs/authentication.md).

Agent startup, restore and end confirmation default to 2m/6m/5m (agent_start_timeout, agent_restore_timeout, agent_end_confirm_timeout). Only omission selects defaults; explicit nonpositive or invalid durations fail. Changes affect new durable windows only, never renew active ones.
