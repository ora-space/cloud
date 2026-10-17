# internal: Authoritative Cloud Subsystems

[中文](README.md) | [English](README.en.md)

`internal` hosts the private implementation packages for Ora Cloud. Per the repository architectural boundary rules documented in `AGENTS.md`, all core state, policy, translation, and infrastructure adapters remain private under `internal/`.

## Module map

- [core](core/README.en.md) is the authoritative domain core, owning business state machines, transaction boundaries, database advisory locks, and cryptographic authentication.
  - [migrations](core/migrations/README.en.md) contains ordered, forward-only PostgreSQL schema migration scripts and checksum verification.
- [api](api/README.en.md) is the HTTP presentation layer.
  - [router](api/router/README.en.md) binds HTTP routes, verifies two-tier JWT credentials, parses JSON request bodies, and projects domain errors into stable contracts.
- [gateway](gateway/README.en.md) is the public authentication boundary: PostgreSQL-backed Login Attempts and Browser Sessions, provider-neutral login orchestration, internal JWT issuance, cookie/CSRF/redirect protection, and the `/api/v1` proxy.
  - [idaas](gateway/idaas/README.en.md) is the Huawei IDaaS 2.0 (`client_secret_post`) adapter that produces a `VerifiedIdentity` with `source=huawei-corp`.
  - [github](gateway/github/README.en.md) is the GitHub OAuth App adapter that produces a `VerifiedIdentity`.
  - [devlogin](gateway/devlogin/README.en.md) is the development-only provider: a local form where any typed identity signs in, registered solely on loopback development origins.
- [contract](contract/README.en.md) defines OpenAPI 3.0 schema models, DTO structures, and contract coverage tests.
- [repository](repository/README.en.md) manages PostgreSQL database connection pools and startup health checks via GORM.
- [objectstore](objectstore/README.en.md) is cloud's own S3-compatible object-store client (SigV4-presigned upload grants, HEAD verification of an object's size and SHA-256), serving the Revision path alone.
- [gitpreset](gitpreset/README.en.md) prepares and checks Git Server contract fixtures for acceptance only, writing the repository volume and database directly; no service process imports it.
- [gitserver](gitserver/README.en.md) serves Git access to Project repositories: the sandbox and Repo Executor repository listeners, per-request ref rule sets from Core, `git http-backend`, per-namespace pre-receive rules and Cloud's ref query.
- [gitrefquery](gitrefquery/README.en.md) is Cloud's client for the Git Server's read-only ref query (`GET /internal/v1/refs`), verifying outside any transaction that a ref Cloud is about to register exists and names the declared commit.
- [config](config/README.en.md) loads and validates application configuration files and environment overrides.
- [logger](logger/README.en.md) provides structured, non-blocking JSON logging via Zap and Lumberjack.
- [simulator](simulator/README.en.md) implements in-process doubles for the Substrate execution engine, Controller, and Workspace Node.
- [controlpb](controlpb/README.en.md) holds the gRPC Go code (server stubs and messages) generated from the Controller internal control contract under [`proto/`](../proto/README.en.md); read-only.
- [controlgrpc](controlgrpc/README.en.md) serves that contract over gRPC: the caller-identity interceptor, `Fault` → status mapping, the lease service; translation only, no business rules.

## Layering and architectural rules

1. **Unidirectional dependencies**:
   - `cmd/*` $\rightarrow$ `internal/api/router`, `internal/gateway`, `internal/core`, `internal/config`, `internal/logger`, `internal/repository`, `internal/objectstore`.
   - `internal/gateway` $\rightarrow$ `internal/core` (the `Claims` type only), `internal/config`, `internal/logger`; `internal/gateway/idaas`, `internal/gateway/github` and `internal/gateway/devlogin` $\rightarrow$ `internal/gateway`. The Gateway never queries Cloud business tables.
   - `internal/api/router` $\rightarrow$ `internal/core`, `internal/contract`; `internal/controlgrpc` $\rightarrow$ `internal/core`, `internal/controlpb`.
   - `internal/core` $\rightarrow$ standard library, `gorm.io/gorm`, `internal/core/migrations`.
   - `internal/repository` $\rightarrow$ `internal/config`, `gorm.io/gorm`.
   - `internal/gitpreset` $\rightarrow$ `internal/core` (ref vocabulary), `internal/gitserver` (repository creation); only `cmd/git-server-preset` imports it.
   - `internal/objectstore` $\rightarrow$ `internal/core` (the `UploadGrant` value type only); `internal/core` reaches it through the `RevisionObjects` interface it declares for itself and never imports this package back.
   - `internal/gitrefquery` $\rightarrow$ `internal/core` (`ErrGitRefNotFound` and the commit format); `internal/config` builds `core.GitServer` with it, and `internal/core` reaches it only through the `GitRefResolver` interface it declares and never imports this package back.
   - Lower layers (`core`, `repository`, `objectstore`) never import upper presentation layers (`api`, `router`).
2. **PostgreSQL is authoritative**:
   - All shared state is persisted in PostgreSQL. In-memory caching of authoritative domain state across requests is strictly prohibited.
3. **Transaction boundary**:
   - Database transactions and advisory locks are strictly localized to PostgreSQL operations inside `core.Store.transact`. Transactions must **never** be held across external HTTP requests, Git CLI commands, or filesystem I/O.
4. **Error handling**:
   - Public-facing errors use the stable `Fault` structure (`Code`, `Params`, `Status`). Internal SQL, stack traces, and database errors are logged internally and never returned to clients.

See [AGENTS.md](../AGENTS.md), [Core contract](../docs/core-contract.md), and [Authentication](../docs/authentication.md).
