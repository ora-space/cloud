# integration: PostgreSQL Integration Test Suite

[中文](README.md) | [English](README.en.md)

`integration` houses the automated integration test suite for Ora Cloud. It exercises the complete server stack across real boundaries: real HTTP requests, authoritative PostgreSQL database constraints, real Git repository operations, and filesystem I/O.

## Test categories and coverage

- **`cloud_test.go`**: End-to-end lifecycle verification:
  - Project and Workspace creation through sandbox → node → clone, with a real Git clone of a Git Server double snapshot into each Workspace's own data, and `one_main` constraint enforcement.
  - Workspace state progression (`provisioning` $\rightarrow$ `ready` $\rightarrow$ `stopped` $\rightarrow$ `deleted`).
  - Controller leasing, epoch fencing, and operation claiming/advancing.
  - Idempotency replay and conflict detection.
- **`workspace_lifecycle_test.go`**: retained effect kinds per step, clone failure/retry and unknown-outcome blocking, and Workspace data deletion waiting for termination.
- **`clone_snapshot_test.go`**: the clone step consuming the Binding's current snapshot (git-server-preset fixtures stand in for the external line): waiting for the first sync then freezing the input, failing with the sync's failure, claims passing a parked operation, frozen input outliving snapshot updates, isolated `baseRef` limited to the synced branch, run Workspaces never syncing, delete_project waiting for a repository Operation, blocking without a Git Server, and tenant-level clones being gone.
- **`git_server_grants_test.go`**: Git Server authorization: a clone execution reads only during its step, and an Agent session writes only its own Workspace's branches.
- **`workspace_operations_grpc_test.go`**: the Controller driving a Workspace through `WorkspaceOperationService` / `NodeReportService`, Node incarnation replacement, and `OperationAvailable` delivery.
- **`endpoints_test.go`**: Comprehensive route and parameter matrix testing for all public and internal control endpoints.
- **`contract_test.go`**: End-to-end OpenAPI contract validation against live HTTP responses.
- **`personal_agent_admission_http_test.go`**: With a usable global default, absent and cleared Claude model selections still fail through real HTTP readiness, manual Run, task mention and task message entry points. Public reads prove no leftover resources; an explicit plugin selection restores admission and freezes its revision. Credentials are an isolated ciphertext-boundary double, not evidence of browser credential entry or real Node execution.
- **`gateway_authorization_test.go`**: Tenant authorization through the real Gateway and Cloud path: external identities gain no automatic membership, creators become administrators, invitations grant ordinary membership, and disabling membership immediately denies access on both replicas. Another login cannot restore access. The provider is a protocol double, not browser evidence for real external login.
- **`gateway_callback_test.go`**: When the browser cookie remains valid but the database attempt has expired, callback rejection precedes provider code exchange, creates no session, and clears the attempt cookie.
- **`security_test.go`**: Authentication, authorization, and isolation tests:
  - Tenant boundary isolation and cross-tenant data leak prevention.
  - Role-based access control (admin vs member permissions).
  - Two-tier credential validation and `service.Subject == user.Caller` binding checks.

## Hermetic testing invariants

For leftover template and copy cleanup after a killed process, see [the testing guide](../docs/development/testing.en.md#cleaning-up-leftover-test-databases); never clean up while tests are running.

- **Ordinary fixture isolation**: `fixture_template_test.go` migrates a fresh template database once per test process, closes its migration pool and disallows connections to the source. `setup`, Gateway and gRPC fixtures use PostgreSQL `CREATE DATABASE … TEMPLATE` for independent copies containing tables, indexes, constraints, triggers and migration records; `t.Cleanup` drops each copy. The test role needs `CREATEDB`. The template receives no business writes and is dropped at package shutdown.
- **Migration fixtures keep their original path**: `testSchema` still creates a fresh isolated schema. `TestMigration*`, older-snapshot upgrades and skeleton migration tests continue to execute migrations and replays. Serial controlled-reclamation matrices retain their existing business-state reset while reusing one independent copy; every case still creates its own Store and service stack.
- **Template regression coverage**: `fixture_template_isolation_test.go` checks data, DDL, migration-history, later-copy and parallel-copy isolation, and proves migration fixtures remain empty. Package shutdown prints cumulative template migration and copy timings to distinguish preparation costs.
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

See [Local development](../docs/development/local-development.en.md), [Authoritative persistence](../docs/core/persistence.en.md), and [Taskfile.yml](../Taskfile.yml).

Upgrade and authorization regression coverage: `tenant_upstream_migration_test.go` verifies 0016-to-0017 row preservation for runtimes, clones and plugins. `project_space_test.go` checks a different member can create and list ready Node-cloned runtimes. Plugin tests use separate tenants for isolation and reject reads immediately after membership revocation.

## Revision and business-hook acceptance

`task test:revision` requires real PostgreSQL and S3. Set `TEST_S3_ENDPOINT`, `TEST_S3_ACCESS_KEY_FILE` and `TEST_S3_SECRET_KEY_FILE` and create the `revisions` bucket; credentials are temporary file references. `REQUIRE_S3=1` makes missing storage fatal. Sandbox acceptance additionally needs `TEST_S3_PUBLIC_ENDPOINT`, `TEST_S3_SANDBOX_NETWORK` and `REQUIRE_S3_SANDBOX=1`; the cluster companion's `task agent:acceptance` configures these automatically.

Revision tests directly cover real uploads, checksum/size/missing objects, expired-grant refresh, post-I/O fencing, transaction failures, replay, both doubles restarting and historical upgrades. Hook tests cover ready/failed, Thread, session end, delivery and deletion committing/rolling back with control evidence. Enable mandatory storage/network environments for full `task test` and `task test:race` too; skipped S3 is not acceptance. See [Testing and gates](../docs/development/testing.en.md#s3) for the environment.

When restart acceptance replaces the Controller/Node, it preserves the same Git Server double to verify frozen history uploads and grant refresh after the delivery ref has been pushed.

## Test-scoped contract validation

Each test process loads, validates and compiles the OpenAPI router once, then shares it read-only.
Every fixture retains its own HTTP transport, requests/responses, PostgreSQL schema, signing keys
and Git data. Every real response is still strictly validated; validation outcomes are never cached.
`contract_suite_test.go` covers reuse across two real fixtures, concurrent reads, malformed-response
rejection and schema immutability. This cache is test-only and does not add production global state
or relax the existing test timeout.
