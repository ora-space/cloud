# integration: PostgreSQL Integration Test Suite

[中文](README.md) | [English](README.en.md)

`integration` houses the automated integration test suite for Ora Cloud. It exercises the complete server stack across real boundaries: real HTTP requests, authoritative PostgreSQL database constraints, real Git repository operations, and filesystem I/O.

## Test categories and coverage

- **`cloud_test.go`**: End-to-end lifecycle verification:
  - Project creation, storage allocation, and Git bare repository cloning.
  - Linked worktree creation and `one_main` constraint enforcement.
  - Workspace state progression (`provisioning` $\rightarrow$ `ready` $\rightarrow$ `stopped` $\rightarrow$ `deleted`).
  - Controller leasing, epoch fencing, and operation claiming/advancing.
  - Idempotency replay and conflict detection.
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
