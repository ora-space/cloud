# cmd/server: Ora Cloud HTTP Daemon

`cmd/server` is the main production entrypoint for the authoritative Ora Cloud service. It binds HTTP endpoints, validates database migration checksums, verifies JWT credentials, and manages the daemon lifecycle under operating system signals.

## Responsibilities

- **Configuration & logging**: Loads application settings via `internal/config.Load` (supporting YAML files and `CLOUD_*` environment variable overrides) and initializes the process-wide Zap logger.
- **PostgreSQL pool initialization**: Connects to the authoritative PostgreSQL database via `internal/repository.InitDB` and verifies connectivity with a mandatory ping.
- **Migration integrity gate**: Executes `store.CheckSchema(ctx)` at startup. It strictly verifies that all migrations defined in `internal/core/migrations` have been applied in order with matching SHA256 checksums, and that no unexpected migration versions exist. It never applies DDL or runs `AutoMigrate`.
- **Cryptographic trust setup**: Constructs the `core.Authenticator` using configured trusted verification public keys and the expected audience string.
- **HTTP server composition**: Initializes the Gin engine via `internal/api/router.New`, applies server timeouts (`ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`), and listens on the configured TCP port.
- **Graceful shutdown**: Intercepts `os.Interrupt` and `syscall.SIGTERM`. On receipt of a shutdown signal, it allocates a bounded 10-second shutdown context to finish in-flight requests and cleanly closes the PostgreSQL pool and logger buffers.

## Boundaries and invariants

- **No startup DDL**: The server never mutates the database schema at runtime. Missing or altered migrations immediately cause a fatal startup exit; schema updates must be performed using `cloudctl migrate`.
- **Stateless daemon**: The server process maintains no in-memory mutable business state across requests. Authoritative state resides entirely within PostgreSQL and is synchronized using database-level advisory locking.
- **Zero credential leaks**: The server does not handle or log raw external deployment keys, cloud provider secrets, or infrastructure passwords.

See [cmd overview](../README.md), [HTTP router](../../internal/api/router/README.md), [Authentication](../../docs/authentication.md), and [Core contract](../../docs/core-contract.md).
