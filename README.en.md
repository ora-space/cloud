# Ora Cloud

[中文](README.md) | [English](README.en.md)

Phase-one implementation of the Go/Gin cloud core, authoritative PostgreSQL persistence, internal authentication and bounded control contracts, plus simulated execution components that use real HTTP, PostgreSQL, disk, and Git boundaries. This repository does not yet include the Rust Controller/Workspace Node split, the Desktop refactor, or Kubernetes deployment.

Requires Go 1.27.1, Git, PostgreSQL 17, optionally Task, and `buf` (≥ 1.73; `task check` lints the contract and regenerates `internal/controlpb` to detect drift, see [`proto/`](proto/README.en.md)). The database is initialized through GORM and injected into the application, while the transaction layer executes parameterized PostgreSQL SQL. There is no global database handle, SQLite/MySQL sample user CRUD, or production-startup AutoMigrate.

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

After `config.toml` and `task setup` the export is unnecessary: `.local/dev.env` provides `TEST_DATABASE_URL` to every task (a value already set in the shell wins). Each test creates an isolated PostgreSQL schema and cleans it up automatically. The test account requires CREATE SCHEMA permission. `task check/test/test:integration/test:race` sets `REQUIRE_POSTGRES=1`; without a real PostgreSQL configuration, these tasks fail instead of silently skipping tests. A direct `go test ./...` explicitly skips integration tests when PostgreSQL is not configured. Use `task test:unit` to run non-PostgreSQL tests separately.

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

`bootstrap` atomically creates a tenant, its first administrator and its `default` space and is a deployment operation; running it again creates another tenant. When `-source` is `huawei-corp`, `-subject` must be the stable `uuid` returned by IDaaS (`uuid~...`), never the employee number or W3 account: otherwise that employee's first login creates a new user, whom the frontend then guides into provisioning a tenant of their own. `credential-ref` stores only an infrastructure reference and never accepts a Git credential value; tenant and owner foreign keys scope the reference. A signed-in user can also provision a tenant for themselves through `POST /api/v1/tenants`: the body carries only the first collaboration space's `name` and `slug`, the tenant borrows that name, and the caller becomes the tenant administrator and the space owner; the tenant is an implicit container the product never shows. Joining an existing tenant still requires the user to access `/api/v1/me` through an authenticated gateway first and an administrator to add them explicitly through the membership API; there is no automatic authorization from external groups.

Before starting production, configure internal verification public keys as described in [Authentication and credentials](docs/authentication.md). Startup fails when the trust configuration is empty:

```sh
go run ./cmd/server -config /path/to/config.yaml
```

The server only checks applied migrations and their checksums; it does not execute DDL. Database, migration, trust, or listen failures cause a non-zero exit. `GET /healthz` checks PostgreSQL reachability.

Local end-to-end development (browser → Gateway → Cloud, see [Authentication Gateway](docs/gateway.md)) needs no GitHub account: the Gateway ships a local-only developer login where any typed identity signs in. To exercise real GitHub login, create an OAuth App (callback `http://localhost:5173/auth/callback/github`) and fill in `[github]` in `config.toml`.

```sh
cp config.toml.template config.toml   # the defaults work as-is; [github] is optional. config.toml is ignored by Git
task setup                            # keys, secret file, .local/dev.env, migrations; rerunnable
task dev                              # Cloud :8080 + Gateway :8081 + frontend :5173
```

`task setup` ([cmd/devsetup](cmd/devsetup/README.en.md)) turns `config.toml` into the `CLOUD_*`/`GATEWAY_*` environment overrides the services already accept (`.local/dev.env`, which `Taskfile.yml` loads for every task and which also provides `TEST_DATABASE_URL`) and the client secret file the Gateway reads; `configs/*.yaml` remain the authoritative configuration. A first-time user belongs to no tenant; the frontend guides them to create their first workspace.

Run the complete creation demo directly; it generates short-lived simulated signing keys independently and uses them only for in-process testing:

```sh
go run ./cmd/cloudctl -command migrate
go run ./cmd/simulator
```

The demo starts isolated loopback HTTP cloud and Substrate services, creates a test tenant, a simulated sandbox and Node, and a real clone into the main Workspace's own data, and then outputs the ready Workspace. Disk state remains under `.local/demo/`, and PostgreSQL records are retained; each subsequent run creates another demo tenant. The simulator has no production infrastructure credentials, does not deploy Kubernetes, and does not start real Agent or Deno runtimes.

## Frontend

The official web frontend lives in [`frontend/`](frontend/README.md): React 19 + TypeScript + Vite +
Tailwind CSS 4 + TanStack Query, with the API layer compiled by [orval](https://orval.dev) from
[`api/openapi.json`](api/openapi.json). It installs, develops, and builds independently:

```sh
cd frontend
npm ci                 # Node >= 24, see frontend/.node-version
npm run dev            # Vite dev server (:5173); /api, /internal, /healthz proxied to :8080
npm run build          # tsc -b && vite build
```

Auth reuses Cloud's dual-JWT: the browser holds only a session cookie + user profile; `cmd/ora-web`
signs tokens server-side (the browser is untrusted). Dev requires a real PostgreSQL — start the
database per "Local validation" above and `task run` (:8080), then `npm run dev`.

Demo vs formal: `cmd/demo-issue-board-web` is a single-file HTML Kanban demo (no frontend
dependency, no CORS, server-side JWT signing) for manual API verification; `frontend/` is the
formal product UI. The two coexist and are not interchangeable.

## Contracts and boundaries

- [OpenAPI 3.0](api/openapi.json): all 19 public endpoints, 15 internal endpoints, and health. `task openapi` regenerates it, while tests validate the document, generated output, and actual HTTP response structures.
- [Web frontend](frontend/README.en.md): `frontend/src/api` is generated by orval from the same `api/openapi.json` into typed TanStack Query hooks; `task frontend:generate` runs Go contract → JSON → TypeScript in one step, and CI fails on generated-client drift.
- [Core invariants and state machines](docs/core-contract.md): identity, ownership, idempotency, admission, leases, recovery, and cleanup.
- [Substrate/Node and phase-two boundaries](docs/execution-contract.md): shared-volume layout, maintenance Jobs, container mounts, Git semantics, and migration ownership.
- [Requirements—implementation—validation checklist](docs/acceptance.md): current evidence and incomplete phase-two validation.

## Module architecture and layered documentation

Every subsystem, service command, and tool follows the same rigorous architectural design as Ora Desktop and has its own module-level specification:

- **Command and operations entrypoints (`cmd/`)**: [entrypoint overview (`cmd/`)](cmd/README.en.md)
  - [Service daemon (`cmd/server`)](cmd/server/README.en.md): core production HTTP daemon.
  - [Authentication gateway (`cmd/gateway`)](cmd/gateway/README.en.md): Huawei IDaaS or GitHub OAuth login, PostgreSQL browser sessions, and the `/api/v1` reverse proxy.
  - [Operations CLI (`cmd/cloudctl`)](cmd/cloudctl/README.en.md): migrations, initial tenant bootstrap, and credential-reference configuration.
  - [Local execution simulator (`cmd/simulator`)](cmd/simulator/README.en.md): in-memory and disk-backed execution-double demo.
  - [OpenAPI synchronization tool (`cmd/openapi`)](cmd/openapi/README.en.md): automatically compiles the Go contract into `api/openapi.json`.
  - [Strict formatting gate (`cmd/checkformat`)](cmd/checkformat/README.en.md): static CI formatting gate.
- **Internal core subsystems (`internal/`)**: [subsystem overview (`internal/`)](internal/README.en.md)
  - [Domain state-machine engine (`internal/core`)](internal/core/README.en.md): aggregates, transactions and global locking, optimistic versioning, leases, and idempotency.
  - [PostgreSQL migration catalog (`internal/core/migrations`)](internal/core/migrations/README.en.md): linear migrations 0001–0005 and checksum integrity verification.
  - [HTTP routing gateway (`internal/api/router`)](internal/api/router/README.en.md): Gin dispatch, two-tier JWT validation, allowlisting, and Fault projection.
  - [Authentication boundary (`internal/gateway`)](internal/gateway/README.en.md): login orchestration, Login Attempt/Session store, internal credential issuance, cookie/CSRF policy, and the proxy; [Huawei IDaaS adapter (`internal/gateway/idaas`)](internal/gateway/idaas/README.en.md); [GitHub adapter (`internal/gateway/github`)](internal/gateway/github/README.en.md).
  - [API contract definitions (`internal/contract`)](internal/contract/README.en.md): OpenAPI 3.0 data models and tests.
  - [Database pool management (`internal/repository`)](internal/repository/README.en.md): GORM connection pooling, fail-fast health checks, and security constraints.
  - [Configuration parsing and loading (`internal/config`)](internal/config/README.en.md): strongly typed Viper configuration and environment-variable mapping.
  - [Structured logging (`internal/logger`)](internal/logger/README.en.md): Zap and Lumberjack rotation with platform adaptation.
  - [Execution doubles (`internal/simulator`)](internal/simulator/README.en.md): development-time Substrate, Node, and Controller doubles.
- **Public libraries and integration tests**:
  - [Public export boundary (`pkg/`)](pkg/README.en.md): public-library export policies and constraints.
  - [Integration test suite (`integration/`)](integration/README.en.md): end-to-end integration tests using isolated, real PostgreSQL schemas.

Phase one serializes core transactions with a database-level global advisory lock and permits only one unfinished operation per Project. HTTP, Git, and Substrate calls never hold a database transaction. This choice suits the initial single-cluster, active-singleton deployment and trades write throughput for simpler concurrency invariants. Locks may later be partitioned by tenant or Project, but the existing concurrency tests must continue to pass.

The container packages server, gateway, and cloudctl and runs as a non-root user. Build it with `docker build -f scripts/Dockerfile -t ora-cloud:phase-one .`, mount your own configuration and public keys, run migrations separately from the same image with `--entrypoint /app/cloudctl`, and run the authentication Gateway with `--entrypoint /app/gateway` (see `docs/gateway.md`). The same Dockerfile has two local-stack targets used by the `ora-space/cluster` Compose file: `web` (the built frontend served by nginx, which proxies `/auth`, `/api` and `/healthz` to `GATEWAY_UPSTREAM` for one origin, as the Vite dev server does) and `devsetup` (`cmd/devsetup`, local key generation and migrations only, never part of the service image). Repository CI is split into two workflows: `Backend` runs formatting, static checks and race-enabled integration tests against a PostgreSQL service; `Frontend` runs only when `frontend/`, `api/` or `internal/contract/` change, verifies the generated client matches the contract and runs the format, lint, type, coverage, module docs/tests, dead-code, duplication and build gates (see `frontend/AGENTS.md`). Docker images and real deployment are outside the locally validated scope.
