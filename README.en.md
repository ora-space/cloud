# Ora Cloud

[中文](README.md) | [English](README.en.md)

Phase-one implementation of the Go/Gin cloud core, authoritative PostgreSQL persistence, internal authentication and bounded control contracts, plus simulated execution components that use real HTTP, PostgreSQL, disk, and Git boundaries. This repository does not yet include the Rust Controller/Workspace Node split, the Desktop refactor, or Kubernetes deployment.

Requires Go 1.27.1, Git, PostgreSQL 17, and optionally Task. The database is initialized through GORM and injected into the application, while the transaction layer executes parameterized PostgreSQL SQL. There is no global database handle, SQLite/MySQL sample user CRUD, or production-startup AutoMigrate.

## Local validation

On Windows, PostgreSQL 17.11 can be installed and started in isolation under the project's `.local/` directory without creating a system service:

```powershell
./scripts/postgres.ps1 start
$env:TEST_DATABASE_URL='host=127.0.0.1 port=55432 user=postgres dbname=ora_test sslmode=disable'
task check
```

The script listens only on `127.0.0.1:55432` and uses local-test trust authentication. The binaries come from the [EDB PostgreSQL Windows distribution](https://www.enterprisedb.com/download-postgresql-binaries), with a pinned version and SHA256 checksum. Stop it with `./scripts/postgres.ps1 stop`; the data is retained.

Docker is also supported:

```sh
docker compose up -d --wait
export TEST_DATABASE_URL='host=127.0.0.1 port=55432 user=ora password=ora-local dbname=ora sslmode=disable'
task check
task test:race
```

Each test creates an isolated PostgreSQL schema and cleans it up automatically. The test account requires CREATE SCHEMA permission. `task check/test/test:integration/test:race` sets `REQUIRE_POSTGRES=1`; without a real PostgreSQL configuration, these tasks fail instead of silently skipping tests. A direct `go test ./...` explicitly skips integration tests when PostgreSQL is not configured. Use `task test:unit` to run non-PostgreSQL tests separately.

Race testing on Windows requires a working C compiler:

```powershell
$env:CC='D:\tmp\ora-cloud-test-tools\llvm-mingw-20260908-ucrt-x86_64\bin\x86_64-w64-mingw32-gcc.exe'
$env:PATH=(Split-Path $env:CC)+';'+$env:PATH
task test:race
```

This is the isolated tool path used during the original validation. Other machines should configure their own MinGW/LLVM-MinGW `CC`. Linux CI uses the system C compiler.

## Running

The default configuration file is `configs/config.yaml`. Every existing setting can be overridden with a `CLOUD_` environment variable, such as `CLOUD_DATABASE_DSN`. Production databases should use TLS, a dedicated DML identity, and a separate deployment migration identity; do not use the sample local trust configuration.

```powershell
# The default sample targets the local ora_test database above; specify -config for real deployments.
go run ./cmd/cloudctl -command migrate
go run ./cmd/cloudctl -command bootstrap -name 'Engineering Organization' -source 'huawei-corp' -subject 'stable-account-id' -display-name 'Initial Administrator'
go run ./cmd/cloudctl -command credential-ref -tenant '<tenant UUID>' -owner '<user UUID>' -secret-ref 'infra-secret://git/team/account'
```

`bootstrap` atomically creates a tenant and its first administrator and is a deployment operation; running it again creates another tenant. `credential-ref` stores only an infrastructure reference and never accepts a Git credential value; tenant and owner foreign keys scope the reference. A regular member must first access `/api/v1/me` through an authenticated gateway to create the user, and an administrator must then add that user explicitly through the membership API. There is no self-service organization registration or automatic authorization from external groups.

Before starting production, configure internal verification public keys as described in [Authentication and credentials](docs/authentication.md). Startup fails when the trust configuration is empty:

```sh
go run ./cmd/server -config /path/to/config.yaml
```

The server only checks applied migrations and their checksums; it does not execute DDL. Database, migration, trust, or listen failures cause a non-zero exit. `GET /healthz` checks PostgreSQL reachability.

Run the complete creation demo directly; it generates short-lived simulated signing keys independently and uses them only for in-process testing:

```sh
go run ./cmd/cloudctl -command migrate
go run ./cmd/simulator
```

The demo starts isolated loopback HTTP cloud and Substrate services, creates a test tenant, bare repository, main linked worktree, simulated sandbox, and Node, and then outputs the ready Workspace. Disk state remains under `.local/demo/`, and PostgreSQL records are retained; each subsequent run creates another demo tenant. The simulator has no production infrastructure credentials, does not deploy Kubernetes, and does not start real Agent or Deno runtimes.

## Contracts and boundaries

- [OpenAPI 3.0](api/openapi.json): all 19 public endpoints, 15 internal endpoints, and health. `task openapi` regenerates it, while tests validate the document, generated output, and actual HTTP response structures.
- [Core invariants and state machines](docs/core-contract.md): identity, ownership, idempotency, admission, leases, recovery, and cleanup.
- [Substrate/Node and phase-two boundaries](docs/execution-contract.md): shared-volume layout, maintenance Jobs, container mounts, Git semantics, and migration ownership.
- [Requirements—implementation—validation checklist](docs/acceptance.md): current evidence and incomplete phase-two validation.

## Module architecture and layered documentation

Every subsystem, service command, and tool follows the same rigorous architectural design as Ora Desktop and has its own module-level specification:

- **Command and operations entrypoints (`cmd/`)**: [entrypoint overview (`cmd/`)](cmd/README.en.md)
  - [Service daemon (`cmd/server`)](cmd/server/README.en.md): core production HTTP daemon.
  - [Operations CLI (`cmd/cloudctl`)](cmd/cloudctl/README.en.md): migrations, initial tenant bootstrap, and credential-reference configuration.
  - [Local execution simulator (`cmd/simulator`)](cmd/simulator/README.en.md): in-memory and disk-backed execution-double demo.
  - [OpenAPI synchronization tool (`cmd/openapi`)](cmd/openapi/README.en.md): automatically compiles the Go contract into `api/openapi.json`.
  - [Strict formatting gate (`cmd/checkformat`)](cmd/checkformat/README.en.md): static CI formatting gate.
- **Internal core subsystems (`internal/`)**: [subsystem overview (`internal/`)](internal/README.en.md)
  - [Domain state-machine engine (`internal/core`)](internal/core/README.en.md): aggregates, transactions and global locking, optimistic versioning, leases, and idempotency.
  - [PostgreSQL migration catalog (`internal/core/migrations`)](internal/core/migrations/README.en.md): linear migrations 0001–0004 and checksum integrity verification.
  - [HTTP routing gateway (`internal/api/router`)](internal/api/router/README.en.md): Gin dispatch, two-tier JWT validation, allowlisting, and Fault projection.
  - [API contract definitions (`internal/contract`)](internal/contract/README.en.md): OpenAPI 3.0 data models and tests.
  - [Database pool management (`internal/repository`)](internal/repository/README.en.md): GORM connection pooling, fail-fast health checks, and security constraints.
  - [Configuration parsing and loading (`internal/config`)](internal/config/README.en.md): strongly typed Viper configuration and environment-variable mapping.
  - [Structured logging (`internal/logger`)](internal/logger/README.en.md): Zap and Lumberjack rotation with platform adaptation.
  - [Execution doubles (`internal/simulator`)](internal/simulator/README.en.md): development-time Substrate, Node, and Controller doubles.
- **Public libraries and integration tests**:
  - [Public export boundary (`pkg/`)](pkg/README.en.md): public-library export policies and constraints.
  - [Integration test suite (`integration/`)](integration/README.en.md): end-to-end integration tests using isolated, real PostgreSQL schemas.

Phase one serializes core transactions with a database-level global advisory lock and permits only one unfinished operation per Project. HTTP, Git, and Substrate calls never hold a database transaction. This choice suits the initial single-cluster, active-singleton deployment and trades write throughput for simpler concurrency invariants. Locks may later be partitioned by tenant or Project, but the existing concurrency tests must continue to pass.

The container packages only server and cloudctl and runs as a non-root user. Build it with `docker build -f scripts/Dockerfile -t ora-cloud:phase-one .`, mount your own configuration and public keys, and run migrations separately from the same image with `--entrypoint /app/cloudctl`. Repository CI uses a PostgreSQL service, formatting and static checks, and race-enabled integration tests. Docker images and real deployment are outside the locally validated scope.
