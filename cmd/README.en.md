# cmd: Command Entrypoints

[中文](README.md) | [English](README.en.md)

`cmd` hosts the command-line and daemon entrypoints for Ora Cloud. Packages in this directory are
strictly limited to runtime configuration loading, dependency injection, process lifecycle wiring,
operating-system signal handling, and process exit codes.

## Module map

- [server](server/README.en.md) is the primary authoritative HTTP API service daemon.
- [gateway](gateway/README.en.md) is the browser-facing authentication and reverse-proxy boundary: GitHub OAuth login, PostgreSQL sessions, and internal credential issuance.
- [cloudctl](cloudctl/README.en.md) is the restricted deployment and operations CLI for migrations, tenant bootstrap, and credential reference management.
- [credential-store](credential-store/README.en.md) is the independent Credential Service process (no database): it holds user-managed personal HTTPS Git tokens under its own volume and KEK key material, open only to three mutually authenticated service identities.
- [clone-executor](clone-executor/README.en.md) is the credential executor process (no database): under its own identity it redeems a single-use authorization, materializes the token, runs one constrained `git clone`, and destroys the material before answering — sweeping crash leftovers before it opens its listener.
- [git-credential-ora](git-credential-ora/README.en.md) is the deployment-fixed Git credential helper — not a credential store: it reads the one file the executor materialized and enforces the repository scope (the normalized host + path identity, never a prefix) at the point the secret is handed over.
- [devsetup](devsetup/README.en.md) is the development-only one-shot setup (`task setup`): from `config.toml` it generates the Gateway private keys, the public keys Cloud trusts and the PKCE key, writes the GitHub client secret file and `.local/dev.env`, and applies migrations.
- [simulator](simulator/README.en.md) provides an all-in-one local demo environment backed by in-process Substrate and Git execution doubles.
- [openapi](openapi/README.en.md) compiles and synchronizes the canonical OpenAPI 3.0 specification (`api/openapi.json`) from Go contract definitions.
- [checkformat](checkformat/README.en.md) enforces repository Go formatting standards as a strict failing CI gate.

## Boundaries and invariants

- **No domain logic**: `cmd/*` packages contain zero domain policy, state transition algorithms, or transactional logic. All domain behavior belongs to `internal/core`.
- **No direct database queries**: Commands acquire database pools exclusively via `internal/repository` and hand them directly to `internal/core.NewStore`. No raw SQL, GORM models, or queries exist in `cmd/*`.
- **Resource cleanup on exit**: Process termination must flush logging buffers (`logger.Sync`), close database connection pools (`store.Pool.Close()`), and cleanly cancel background contexts.
- **Fail-fast on startup**: Commands immediately abort with non-zero exit codes if configuration loading, schema checksum verification, database ping, or cryptographic trust verification fails.

See the [top-level README](../README.en.md), [internal packages](../internal/README.en.md), and [AGENTS.md](../AGENTS.md).
