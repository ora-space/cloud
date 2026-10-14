# internal/config: Configuration Loader & Validation

[中文](README.md) | [English](README.en.md)

`internal/config` manages configuration parsing, schema validation, and environment variable overrides for Ora Cloud.

## Responsibilities

- **Structured configuration**: Defines strongly-typed Go structs mapping the entire system configuration:
  - `ServerConfig`: Port, Gin mode, read/write timeouts.
  - `LoggerConfig`: Log level, file paths, rotation thresholds (max size, age, backups, gzip compression).
  - `DatabaseConfig`: Driver (must be `postgres`), DSN, and connection pool limits (`max_open_conns`, `max_idle_conns`, `conn_max_lifetime`).
  - `AuthConfig`: Expected token audience and list of `TrustedKey` verification parameters.
  - `ObjectStoreConfig`: The optional S3 delivery configuration.
  - `CredentialIngressConfig`: Cloud's `ingress-writer` client (endpoint, certificate/key/CA file paths, timeout).
  - `CredentialStoreServiceConfig`: The independent Credential Service process's own configuration (`listen`, `volume`, `kek_file`, server certificate, client authority, the three SPIFFE identities, superseded-version retention), loaded by `LoadCredentialStoreConfig` under the `CREDENTIAL_STORE_*` prefix.
  - `GitServerConfig`: The internal Git Server's sandbox-facing base URL (one key), plus the "exactly one content channel" deployment check between it and `ExecutorDispatchConfig`.
- **Hierarchical loading via Viper**:
  - Searches for `config.yaml` in `./configs`, `../configs`, and `.`.
  - Supports explicit file path overriding via `-config <path>`.
  - Maps environment variables with the `CLOUD_` prefix, replacing dots with underscores (e.g., `CLOUD_DATABASE_DSN` overrides `database.dsn`).
- **Startup sanity validation**: Rejects invalid configurations with explicit errors, requiring positive durations for `read_timeout`, `write_timeout`, and `conn_max_lifetime`.

## Boundaries and invariants

The optional `object_store` section configures S3 delivery. When enabled, the server loads separately mounted access/secret key files into memory and validates private/public endpoints, region, bucket, addressing mode and a TTL between one second and seven days. All keys support `CLOUD_OBJECT_STORE_*` overrides. Short-lived PUT grants can bind SHA-256; Cloud verifies existence, size and stored checksum through private HEAD before registering Revision. Disabled by default, delivery settles skipped/object_store_unconfigured through the caller's transaction without dispatch; standalone grant requests return unavailable. Health reports configured/unconfigured, not S3 availability.

The `credentials` section is the **optional** half of Cloud's credential ingress (`credentials.enabled`, `credentials.endpoint`, `credentials.certificate_file`/`private_key_file`/`ca_file`, `credentials.timeout`). When disabled no client is built and every credential write is refused with `credential_store_unavailable` — the approved fail-closed state: no credential may exist without a service that can hold it, and a raw token is never written into Cloud's own database as a substitute. Enabled but incomplete or unusable is a startup error, because D6 requires a configured capability that cannot be established to fail at startup rather than per request. Certificates and private keys are file paths only and never appear in a configuration file.

The `credential-store` section belongs to a **separate process**: `LoadCredentialStoreConfig` honours only the `CREDENTIAL_STORE_*` prefix, so neither Cloud nor the service can inherit the other's values. `Store()`, `ServerTLS()` and `Identities()` form the D6/D8 startup gate — the volume must exist and not be readable by other local accounts, the KEK must be delivered by the deployment and usable, the server certificate/private key/client authority must all be present, and the three identities must be distinct; any one missing refuses startup. The listener always requires and verifies a client certificate, with TLS 1.3 as the floor.

The `git_server` section is Cloud's only configuration about the internal Git Server, and it holds one key: `git_server.sandbox_base_url` (`CLOUD_GIT_SERVER_SANDBOX_BASE_URL`), the origin a sandbox reaches it at. It exists because Cloud has to write that address into a Workspace's clone input, and only the deployment knows which address a sandbox can reach: the read listener resolves the caller's container from the connection's **peer address**, so a wrong value does not bypass authorization — it makes every read look like it came from somewhere else. The mirror address itself is stored in no configuration and no column; it is derived per clone from the mirror's key (Project, canonical upstream repository, pinned commit), and this section supplies only the base.

Leaving it empty — the default — is serving no mirror at all: a credentialed Workspace stops at the `mirror_not_ready` gate and is never pointed back at its upstream. Naming it requires two deployment facts to hold as well, and violating either is a **startup error** rather than a runtime downgrade: it may not be set alongside `clone_dispatch.enabled`, because there is exactly one content channel (content-transport D1 — with the base URL named, the mirror serves the content and `enabled` would become a switch that is never dispatched to); and it may not be set with `clone_dispatch.mirror_enabled` off, because nothing would publish a mirror and every credentialed Workspace would wait for one that never appears. The reverse combination is not refused: mirror preparation with no base URL is a coherent deployment, where the preparation machinery is live and the channel is still closed, so every credentialed clone stays refused.

- **No secret storage**: Configuration files store only public verification keys and infrastructure references. Plaintext deployment secrets and private keys must never appear in configuration files.
- **Immutable runtime**: Configurations are loaded once at command startup and passed as ready-to-use values. There is no global mutable configuration singleton.

See [config.yaml](../../configs/config.yaml), [cmd/server](../../cmd/server/README.en.md), and [Authentication](../../docs/authentication.md).
