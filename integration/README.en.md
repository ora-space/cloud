# integration: PostgreSQL Integration Test Suite

[中文](README.md) | [English](README.en.md)

`integration` houses the automated integration test suite for Ora Cloud. It exercises the complete server stack across real boundaries: real HTTP requests, authoritative PostgreSQL database constraints, real Git repository operations, and filesystem I/O.

## Test categories and coverage

- **`cloud_test.go`**: End-to-end lifecycle verification:
  - Project and Workspace creation through sandbox → node → clone, with a real Git clone into each Workspace's own data, and `one_main` constraint enforcement.
  - Workspace state progression (`provisioning` $\rightarrow$ `ready` $\rightarrow$ `stopped` $\rightarrow$ `deleted`).
  - Controller leasing, epoch fencing, and operation claiming/advancing.
  - Idempotency replay and conflict detection.
- **`workspace_lifecycle_test.go`**: retained effect kinds per step, clone failure/retry and unknown-outcome blocking, and Workspace data deletion waiting for termination.
- **`workspace_operations_grpc_test.go`**: the Controller driving a Workspace through `WorkspaceOperationService` / `NodeReportService`, Node incarnation replacement, and `OperationAvailable` delivery.
- **`endpoints_test.go`**: Comprehensive route and parameter matrix testing for all public and internal control endpoints.
- **`contract_test.go`**: End-to-end OpenAPI contract validation against live HTTP responses.
- **`security_test.go`**: Authentication, authorization, and isolation tests:
  - Tenant boundary isolation and cross-tenant data leak prevention.
  - Role-based access control (admin vs member permissions).
  - Two-tier credential validation and `service.Subject == user.Caller` binding checks.

## Hermetic testing invariants

- **Schema isolation**: Every test executes within an isolated, dynamically provisioned PostgreSQL schema (`CREATE SCHEMA <unique_name>`). Schemas are completely destroyed in `t.Cleanup` (`DROP SCHEMA <unique_name> CASCADE`), guaranteeing tests do not share mutable database state.
- **Mandatory PostgreSQL**: When running under CI (`REQUIRE_POSTGRES=1`), tests fail immediately if `TEST_DATABASE_URL` is unset rather than silently skipping.
- **Parallel execution & race detection**: Tests are designed to run safely with `go test -race` under `task test:race` to verify concurrency invariants and lock ordering.

## Running integration tests

```powershell
# Local Windows PostgreSQL running via scripts/postgres.ps1:
$env:TEST_DATABASE_URL='host=127.0.0.1 port=55432 user=postgres dbname=ora_test sslmode=disable'
task test:integration

# Full test gate with race detector:
task test:race
```

See [Local setup](../README.en.md#local-validation), [Core contract](../docs/core-contract.md), and [Taskfile.yml](../Taskfile.yml).

Upstream reconciliation coverage: `tenant_upstream_migration_test.go` verifies 0016-to-0017 row preservation for runtimes, clones and plugins. `project_space_test.go` checks a different member can create and list ready Node-cloned runtimes. Plugin tests use separate tenants for isolation and reject reads immediately after membership revocation.

## Revision and business-hook acceptance

`task test:revision` requires real PostgreSQL and S3. Set `TEST_S3_ENDPOINT`, `TEST_S3_ACCESS_KEY_FILE` and `TEST_S3_SECRET_KEY_FILE` and create the `revisions` bucket; credentials are temporary file references. `REQUIRE_S3=1` makes missing storage fatal. Sandbox acceptance additionally needs `TEST_S3_PUBLIC_ENDPOINT`, `TEST_S3_SANDBOX_NETWORK` and `REQUIRE_S3_SANDBOX=1`; the cluster companion's `task agent:acceptance` configures these automatically.

Revision tests directly cover real uploads, checksum/size/missing objects, expired-grant refresh, post-I/O fencing, transaction failures, replay, both doubles restarting and historical upgrades. Hook tests cover ready/failed, Thread, session end, delivery and deletion committing/rolling back with control evidence. Enable mandatory storage/network environments for full `task test` and `task test:race` too; skipped S3 is not acceptance. See the [control-plane report](../docs/agent-run-control-plane-review.en.md).

## Test-scoped contract validation

Each test process loads, validates and compiles the OpenAPI router once, then shares it read-only.
Every fixture retains its own HTTP transport, requests/responses, PostgreSQL schema, signing keys
and Git data. Every real response is still strictly validated; validation outcomes are never cached.
`contract_suite_test.go` covers reuse across two real fixtures, concurrent reads, malformed-response
rejection and schema immutability. This cache is test-only and does not add production global state
or relax the existing test timeout.
