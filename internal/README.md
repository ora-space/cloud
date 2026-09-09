# internal: Authoritative Cloud Subsystems

`internal` hosts the private implementation packages for Ora Cloud. Per the repository architectural boundary rules documented in `AGENTS.md`, all core state, policy, translation, and infrastructure adapters remain private under `internal/`.

## Module map

- [core](core/README.md) is the authoritative domain core, owning business state machines, transaction boundaries, database advisory locks, and cryptographic authentication.
  - [migrations](core/migrations/README.md) contains ordered, forward-only PostgreSQL schema migration scripts and checksum verification.
- [api](api/README.md) is the HTTP presentation layer.
  - [router](api/router/README.md) binds HTTP routes, verifies two-tier JWT credentials, parses JSON request bodies, and projects domain errors into stable contracts.
- [contract](contract/README.md) defines OpenAPI 3.0 schema models, DTO structures, and contract coverage tests.
- [repository](repository/README.md) manages PostgreSQL database connection pools and startup health checks via GORM.
- [config](config/README.md) loads and validates application configuration files and environment overrides.
- [logger](logger/README.md) provides structured, non-blocking JSON logging via Zap and Lumberjack.
- [simulator](simulator/README.md) implements in-process doubles for the Substrate execution engine, Controller, and Workspace Node.

## Layering and architectural rules

1. **Unidirectional dependencies**:
   - `cmd/*` $\rightarrow$ `internal/api/router`, `internal/core`, `internal/config`, `internal/logger`, `internal/repository`.
   - `internal/api/router` $\rightarrow$ `internal/core`, `internal/contract`.
   - `internal/core` $\rightarrow$ standard library, `gorm.io/gorm`, `internal/core/migrations`.
   - `internal/repository` $\rightarrow$ `internal/config`, `gorm.io/gorm`.
   - Lower layers (`core`, `repository`) never import upper presentation layers (`api`, `router`).
2. **PostgreSQL is authoritative**:
   - All shared state is persisted in PostgreSQL. In-memory caching of authoritative domain state across requests is strictly prohibited.
3. **Transaction boundary**:
   - Database transactions and advisory locks are strictly localized to PostgreSQL operations inside `core.Store.transact`. Transactions must **never** be held across external HTTP requests, Git CLI commands, or filesystem I/O.
4. **Error handling**:
   - Public-facing errors use the stable `Fault` structure (`Code`, `Params`, `Status`). Internal SQL, stack traces, and database errors are logged internally and never returned to clients.

See [AGENTS.md](../AGENTS.md), [Core contract](../docs/core-contract.md), and [Authentication](../docs/authentication.md).
