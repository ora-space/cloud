# specs Repository

`specs/` is an independent Git repository. Use `git -C specs` to inspect its status and history when
changing its contents. It owns ADRs, core test cases, and domain documentation; read
`specs/AGENTS.md` before changing anything under that directory. Changes to state transitions,
ownership, persistence, external side effects, recovery, security, or compatibility must keep the
relevant approved ADRs and core-test evidence synchronized with the implementation.

# Go

Ora Cloud is an authoritative Go service. Preserve the boundaries documented in `README.md`,
`docs/core-contract.md`, `docs/execution-contract.md`, and `docs/authentication.md`. The Go version
in `go.mod`, repository tasks in `Taskfile.yml`, and checks in `.golangci.yml` are authoritative.

1. **Document intent**: Exported packages, types, functions, methods, and constants must have
   idiomatic doc comments. Document non-obvious unexported code when the contract or invariant is
   not evident from its name. Write code comments in English and explain why, including ownership,
   security, transaction, ordering, and recovery constraints; let names and structure explain what.
2. **Keep APIs idiomatic and explicit**: Use `MixedCaps`, avoid package-name stutter, accept
   interfaces at the consuming boundary, and return concrete types by default. Keep interfaces
   small and define them where they are consumed. Avoid boolean switches, ambiguous `nil` values,
   and bags of optional fields when typed constants, separate methods, or validated request types
   make the call site and valid states clear.
3. **Design for testability**: Inject databases, clocks, external clients, and other effects. Keep
   domain decisions independent of Gin, GORM, filesystem, and process wiring where practical.
   Prefer small deterministic functions for policy and state transitions. Do not introduce global
   mutable state or hidden singleton dependencies.
4. **Handle errors deliberately**: Return errors for expected failure paths, add useful operation
   context with `%w`, and inspect causes with `errors.Is` or `errors.As`. Handle each error exactly
   once; never both log and return it unless crossing a process or protocol boundary requires both.
   Panic/recover is limited to a package-internal control-flow boundary that converts known panic
   values back to errors; unexpected panics must remain visible. Public responses use the stable
   `Fault` contract and never expose SQL, stack, credential, or infrastructure detail.
5. **Own cancellation and concurrency**: Pass `context.Context` as the first parameter for work
   whose lifetime can end, and propagate it to SQL, HTTP, and process calls. Every goroutine must
   have an explicit owner, cancellation path, and completion strategy. Protect shared state through
   ownership or synchronization, keep critical sections bounded, and verify concurrent changes
   with the race detector.
6. **Keep resources scoped**: Close response bodies, rows, statements, files, processes, and
   timers on every path. Acquire a resource next to the cleanup registration when possible. Use
   `t.Cleanup` for test resources and preserve the existing graceful-shutdown behavior in commands.
7. **Preserve compatibility boundaries**: Architectural cleanup is preferred over carrying
   accidental internal abstractions. PostgreSQL data, migration history, published HTTP/OpenAPI
   behavior, durable filesystem layout, identifiers, and authentication/trust semantics are hard
   compatibility boundaries. Evolve them with an approved decision, an explicit migration or
   versioning plan, and regression evidence; never reinterpret existing durable state silently.

## Packages and dependencies

- Keep `cmd/*` limited to configuration, dependency wiring, lifecycle, and exit behavior. Put
  authoritative state and policy in `internal/core`, HTTP translation and authentication at the
  router boundary, database connection setup in `internal/repository`, and development-only
  execution doubles in `internal/simulator`.
- New implementation packages should be private under `internal/`. Add code to `pkg/` only when it
  is intentionally reusable by external modules and its API can be supported as public surface.
- Keep the public surface minimal. Constructors must return ready-to-use values or an error; do not
  expose partially initialized objects. Preserve zero-value usefulness only where it is honest.
- Prefer the standard library and existing dependencies. Before adding a module, justify the
  capability, maintenance, license, security, and binary-size cost. When dependencies change, keep
  `go.mod` and `go.sum` tidy and include both in the same change.
- Use `filepath.Join`, `filepath.Clean`, and other platform-aware APIs for filesystem paths. Never
  concatenate path separators. Treat all paths, URLs, refs, IDs, and configuration as untrusted
  input at their boundary.
- Use `time.Time` and `time.Duration` rather than numeric time units. Persist instants as PostgreSQL
  `timestamptz`; use the authoritative database time for leases and fencing, and convert to local
  display time only at a presentation boundary.
- Prefer cohesive files and packages. Target implementation files below roughly 500 lines,
  excluding tests and generated artifacts. When a file approaches 800 lines, add new behavior in a
  focused sibling file unless keeping it together protects a stronger invariant. Keep tests and
  package/type documentation close to the code that owns the behavior.
- Avoid thin helpers used once when inline code is clearer. Extract a helper when it names an
  invariant, centralizes error-prone policy, enables focused tests, or has genuine reuse.

## PostgreSQL and transactions

- PostgreSQL is the sole authoritative persistence implementation. Inject the database handle;
  do not add an in-memory, SQLite, MySQL, or package-global alternative to bypass real behavior.
- Schema changes are explicit, ordered SQL files under `internal/core/migrations`. Applied files are
  immutable because their checksums are verified. Add a forward migration for every later change,
  make it safe to retry, and test both a fresh database and an upgrade from the previous schema.
- The server validates migration state and never runs DDL or `AutoMigrate` at startup. Deployment
  changes use `cloudctl migrate` with a separately authorized migration identity.
- Keep transactions short and database-only. Never hold a transaction, row lock, or advisory lock
  across HTTP, Git, filesystem, process, Node, or Substrate work. Use parameterized SQL, check every
  database error, and encode cross-row integrity in PostgreSQL constraints when the database can
  enforce it more reliably than application code.
- Preserve tenant and owner scope in every query and relationship. Resource lookup must not turn an
  authorization failure into a data leak. Soft deletion must retain the references required for
  termination, cleanup, audit, and recovery.
- Persist an external-effect plan and stable idempotency key before dispatching a mutation. On an
  ambiguous result, reconcile by stable external identity; never infer success, overwrite a live
  binding, or discard recovery evidence. Lease epochs and resource versions must fence stale
  actors on every write.

## HTTP, contracts, and security

- `router.Routes`, `internal/contract`, and `api/openapi.json` describe one contract. When a route,
  field, status, or response changes, update all three, run `task openapi`, and add contract and
  integration coverage in the same change. Generated OpenAPI output must have no hand edits.
- Decode requests strictly: retain body limits, reject malformed JSON, extra JSON values, unknown
  fields, invalid types, and server-owned identity or scope fields. Validate at the boundary and
  pass explicit trusted values inward.
- Keep service and end-user credentials independent. Authorization derives only from verified
  claims and current PostgreSQL state, never caller-selected headers or body fields. Preserve
  issuer, audience, key-purpose, role, caller, tenant, owner, workspace, sandbox, generation, and
  epoch bindings where applicable.
- Credentials remain infrastructure references. Never accept, persist, log, return, or commit raw
  deployment private keys, access tokens, passwords, or repository credentials. Generated test keys
  may exist only in isolated process memory or temporary paths and must never reach logs or durable
  fixtures. Logs use structured fields and request/resource IDs while excluding secrets and
  unnecessary personal data.
- Public endpoints return stable, bounded error shapes. Internal details belong in structured logs;
  clients receive actionable codes and safe parameters. Health checks report dependency readiness
  without disclosing configuration.

## Tests

`task check` runs format verification, lint, and the complete PostgreSQL-backed test suite. It can be
slow, so run the smallest relevant task while iterating and run the full gate before considering a
repository-wide or behavior-changing change complete. Use `task --list` for the authoritative task
list.

- Format changed Go files: `task format`
- Verify formatting without edits: `task format:check`
- Lint: `task lint`
- Unit tests without PostgreSQL: `task test:unit`
- PostgreSQL integration tests: `task test:integration`
- Complete test suite with mandatory PostgreSQL: `task test`
- Complete format, lint, and test gate: `task check`
- Race-enabled full suite: `task test:race`
- Build server and operational commands: `task build`

`task check`, `task test`, `task test:integration`, and `task test:race` require a real PostgreSQL
database through `TEST_DATABASE_URL`; they must fail rather than silently skip when
`REQUIRE_POSTGRES=1`. Follow `README.md` for the supported local PostgreSQL setup. Race tests on
Windows require a working C compiler.

- Add or update tests with every behavior change. Prefer table-driven tests for genuine input
  matrices, compare complete values when practical, and make failure messages identify the case and
  violated invariant.
- Tests must be deterministic and hermetic within their declared boundary. Use isolated PostgreSQL
  schemas and temporary directories, register cleanup immediately, and avoid arbitrary sleeps,
  wall-clock assumptions, test-order dependencies, and mutable process-global configuration.
- Use `t.Parallel` only after proving the test and its helpers do not share a schema, environment,
  port, filesystem path, logger, or other mutable state. Concurrent behavior must synchronize the
  start and assert the complete set of permitted outcomes, not merely accept the most common one.
- Integration tests exercise real HTTP, PostgreSQL constraints, disk, and Git where the contract
  crosses those boundaries. A mock or simulator can cover fault injection but cannot replace the
  real-boundary acceptance test.
- Changes to transactions, leases, fencing, idempotency, recovery, shared caches, or goroutines must
  pass `task test:race`. Changes to command wiring or startup/shutdown must also pass `task build`.
- When a behavior maps to a core test case under `specs/test-cases`, keep its stable path/anchor and
  evidence status accurate. A lower-level test counts as evidence only when its failure directly
  shows that the stated obligation is broken.

## Change workflow

1. Read the nearest `AGENTS.md`, relevant docs, existing tests, and approved ADRs before designing
   the change. Identify compatibility boundaries and failure/recovery paths before editing code.
2. Implement the smallest coherent change. Keep schema, code, OpenAPI, docs, tests, and core-test
   evidence in the same change when they describe one behavior.
3. Run `task format`, then the narrowest relevant lint/test loop until it is green. Never weaken a
   lint, skip, timeout, assertion, or security check merely to make the gate pass; fix the cause or
   document an approved exception next to the narrowest possible suppression.
4. Run `task check` for behavior or repository-wide changes, plus `task test:race` for concurrency
   or persistence lifecycle changes and `task build` for command changes. Completion requires every
   applicable gate to pass with no unexplained skips, race reports, leaked resources, or generated
   diff.
5. Review `git diff --check`, the complete diff, and both repository statuses (`git status --short`
   and `git -C specs status --short`). Confirm that logs and fixtures contain no secrets and that no
   unrelated user changes were overwritten.
