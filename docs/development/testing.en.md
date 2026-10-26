# Testing and Gates

English | [中文](testing.md)

Cloud's correctness rests mainly on tests at real boundaries: real PostgreSQL constraints and transactions,
real HTTP, real Git and disk, and real S3 where needed. Doubles can cover fault injection, but they never
replace a test that crosses the real boundary. This document describes the test layers, how to run them and
the gates before merging; the rules for writing tests are in the repository root's `AGENTS.md`.

## Layers

| Layer | Location | Dependencies | Run with |
|---|---|---|---|
| Unit tests | Tests in `internal/...` and `cmd/...` that need no database | None | `task test:unit` |
| Core database tests | `internal/core/*_db_test.go` and similar | Real PostgreSQL | `task test` |
| Integration tests | `integration/` | Real HTTP, PostgreSQL, disk and Git | `task test:integration` |
| Revision and object storage tests | The Revision, plugin and hook cases in `integration/` | Real PostgreSQL and S3 | `task test:revision` |
| Frontend tests | `frontend/` | Node | `task frontend:test`, `task frontend:check` |
| Cross-repository acceptance | ora-space/cluster | Real Controller, Sandbox Server and Nodes | cluster's acceptance tasks |

Integration tests drive the full server stack from the router: the Workspace lifecycle, the Controller's gRPC
contract, the route and parameter matrix of every public and internal endpoint, authentication and isolation, and the
contract test that validates every real HTTP response against `api/openapi.json`. What each test file covers is listed
in [`integration/`](../../integration/README.en.md).

## Database

- Tests connect to PostgreSQL through `TEST_DATABASE_URL`, which `task setup` writes into `.local/dev.env`. The account
  needs `CREATE SCHEMA`; integration and core template copies also require `CREATEDB` for the test role.
- **Independent database state**: ordinary integration and core fixtures each migrate a template once per test process and
  receive independent database copies; the source has no open connections and accepts no business writes. Migration/upgrade
  cases still execute migrations from empty or older isolated schemas. Core's `dispatcherDB` receives an independent database
  copy; controlled-reclamation matrices retain their existing per-case resets. Each test owns its cleanup without sharing
  mutable business state; see [integration fixtures](../../integration/README.en.md#hermetic-testing-invariants).
- **No silent skipping**: `task test`, `task test:integration`, `task test:race` and `task check` set `REQUIRE_POSTGRES=1` and
  fail outright when no database is configured. Running `go test ./...` directly without a database skips the tests that need
  one explicitly, and a pass in that mode does not count as full verification.

### Cleaning up leftover test databases

A killed test process may leave its template and copies behind. **Run this only when no tests are running**, against the local test PostgreSQL selected by `TEST_DATABASE_URL`: list leftovers first, then drop them. These commands match only the complete names used by `integration/fixture_template_test.go`, `internal/core/fixture_template_test.go` and their callers: one of the listed prefixes followed by a 32-character lowercase hexadecimal UUID. Other names and legacy test schemas are excluded. Use the test role that created the databases (or an administrator with permission to drop them), connected to the base test database, not a copy being dropped. `WITH (FORCE)` disconnects sessions in matching databases.

```sh
psql -X -v ON_ERROR_STOP=1 "$TEST_DATABASE_URL" <<'SQL'
SELECT datname FROM pg_database WHERE datname ~ '^(core_template_|core_run_|test_template_|test_|grpc_|test_gateway_|test_controlled_|test_copy_first_|test_copy_second_|test_copy_later_|test_copy_parallel_)[0-9a-f]{32}$' ORDER BY datname;
SQL

psql -X -v ON_ERROR_STOP=1 "$TEST_DATABASE_URL" <<'SQL'
SELECT format('DROP DATABASE %I WITH (FORCE);', datname) FROM pg_database WHERE datname ~ '^(core_template_|core_run_|test_template_|test_|grpc_|test_gateway_|test_controlled_|test_copy_first_|test_copy_second_|test_copy_later_|test_copy_parallel_)[0-9a-f]{32}$' ORDER BY datname
\gexec
SQL
```

## S3

`task test:revision` sets `REQUIRE_S3=1` and requires a real S3-compatible store:

| Environment variable | Description |
|---|---|
| `TEST_S3_ENDPOINT` | The store's address; a `revisions` bucket must exist beforehand |
| `TEST_S3_ACCESS_KEY_FILE`, `TEST_S3_SECRET_KEY_FILE` | Credential files, temporary files only |
| `TEST_S3_PUBLIC_ENDPOINT`, `TEST_S3_SANDBOX_NETWORK`, plus `REQUIRE_S3_SANDBOX=1` | Cases that upload from inside the sandbox network |

These tests cover real uploads, checksums and sizes, missing objects, grant expiry and refresh, the re-check after external
verification, transaction failure and replay, doubles restarting, and historical upgrades. CI's Backend workflow runs them
against a pinned RustFS, and cluster's `task agent:acceptance` sets the environment up automatically. Full runs of `task test`
and `task test:race` should enable it as well, so a skipped S3 is never mistaken for a verified one.

## Gates

| Task | Content | Must pass when |
|---|---|---|
| `task format` / `task format:check` | gofumpt and goimports (except the generated `internal/controlpb`) | Every commit |
| `task lint` | golangci-lint with the rules in `.golangci.yml` | Every commit |
| `task proto:lint`, `task proto:breaking`, `task proto:check` | Proto rules, compatibility against `PROTO_BASE_REF` (default `main`), generated-code drift | Changing `proto/` |
| `task check` | `format:check`, `lint`, `proto:lint`, `proto:check` and the full `task test` | Behavior changes or repository-wide changes |
| `task test:race` | The full suite with the race detector; Windows needs a working C compiler | Changing transactions, leases, fencing, idempotency, recovery, shared caches or goroutines |
| `task build` | Builds every command | Changing command wiring, startup or shutdown |
| `task frontend:check` | The generated client matches the contract, plus format, lint, types, coverage, module docs and tests, dead code, duplication and build | Changing the frontend, `api/` or `internal/contract/` |
| `task frontend:check:diff` | Frontend modules changed since `BASE` (default `origin/main`) must also update their README and tests | Frontend PRs |

While iterating, run the smallest relevant task first, and the full gate before considering the work done. Never loosen a lint,
skip a test, raise a timeout or weaken an assertion or security check just to make a gate pass; when an exception is needed,
document the approved reason next to the narrowest possible suppression.

## Backend CI shards and coverage

Backend runs integration on four independent runners, assigning each top-level UTF-8 test name by CRC32 modulo 4.
New tests are assigned automatically without changing existing names' assignments. Each runner discovers tests with
`go test -list '.*' ./integration`, builds a fully anchored `-run '^(A|B|...)$'`, checks for missing or duplicate assignments,
and verifies the actual top-level starts and results after execution. Examples and fuzz seeds are included; benchmarks remain disabled as in the original command.
The `packages` job retains every other package, format, proto, lint and command-build check. All groups retain real PostgreSQL,
RustFS, race detection and the existing `-timeout 20m`.
Each shard builds Git Server before fixtures create leases, so cold compilation previously warmed by earlier tests cannot consume grant lifetimes.
The test-output `tee` pipeline uses Bash with `pipefail` to preserve a failing `go test` exit code.

The final `check` status depends on every matrix job and cannot pass after a failure or cancellation. It verifies that the
four executed sets form exactly the discovered list, then merges every job's binary coverage with `go tool covdata merge -pcombine`
and generates `coverage.out` with `textfmt` (the `coverage` artifact). Instrumentation remains `-coverpkg=./internal/...`.
`GOCOVERDIR` together with `-test.gocoverdir` retains test-process data instead of losing it in `go test`'s internal temporary directory.

## Writing tests

- Every behavior change adds or changes tests. Use table-driven tests for genuine input matrices, compare complete values where
  practical, and make failure messages name the case and the violated invariant.
- Tests are deterministic and hermetic: they use isolated schemas and temporary directories and register cleanup immediately,
  with no arbitrary sleeps, wall-clock assumptions, test-order dependencies or mutable process-global configuration.
- Use `t.Parallel` only after confirming that the test and its helpers share no schema, environment variable, port, file path,
  logger or other mutable state. Concurrent behavior must start in sync and assert the complete set of permitted outcomes, not
  merely the most common one.
- Test keys live only in process memory or temporary paths and never reach logs or persistent fixtures; real user session logs are
  never used as fixtures.

## Relation to specs core test cases

specs' `test-cases/` lists core test cases per domain and records the evidence status of each (Covered, Partial, Missing). When a
behavior maps to a core test case, keep the test path and anchor it references accurate; a lower-level test counts as evidence
for the case only when its failure directly shows the obligation is broken.

## Related decisions

- [Cloud owns the Internal Control gRPC contract and the authoritative persistence](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/0-cloud-owned-internal-grpc-contract.md)
- [Cloud core test case index](https://github.com/ora-space/specs/blob/main/test-cases/cloud/README.md)
