# cmd: Command Entrypoints

`cmd` hosts the command-line and daemon entrypoints for Ora Cloud. Packages in this directory are
strictly limited to runtime configuration loading, dependency injection, process lifecycle wiring,
operating-system signal handling, and process exit codes.

## Module map

- [server](server/README.md) is the primary authoritative HTTP API service daemon.
- [cloudctl](cloudctl/README.md) is the restricted deployment and operations CLI for migrations, tenant bootstrap, and credential reference management.
- [simulator](simulator/README.md) provides an all-in-one local demo environment backed by in-process Substrate and Git execution doubles.
- [openapi](openapi/README.md) compiles and synchronizes the canonical OpenAPI 3.0 specification (`api/openapi.json`) from Go contract definitions.
- [checkformat](checkformat/README.md) enforces repository Go formatting standards as a strict failing CI gate.

## Boundaries and invariants

- **No domain logic**: `cmd/*` packages contain zero domain policy, state transition algorithms, or transactional logic. All domain behavior belongs to `internal/core`.
- **No direct database queries**: Commands acquire database pools exclusively via `internal/repository` and hand them directly to `internal/core.NewStore`. No raw SQL, GORM models, or queries exist in `cmd/*`.
- **Resource cleanup on exit**: Process termination must flush logging buffers (`logger.Sync`), close database connection pools (`store.Pool.Close()`), and cleanly cancel background contexts.
- **Fail-fast on startup**: Commands immediately abort with non-zero exit codes if configuration loading, schema checksum verification, database ping, or cryptographic trust verification fails.

See the [top-level README](../README.md), [internal packages](../internal/README.md), and [AGENTS.md](../AGENTS.md).
