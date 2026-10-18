# Authoritative Persistence and the Transaction Model

English | [中文](persistence.md)

PostgreSQL is Cloud's only business authority. Identities, memberships, Projects, Workspaces,
operations, execution evidence, Issues, Threads, Revision registrations and every other piece of
shared state live there; the Controller, Node, Gateway, model-gateway and git-server hold no second
copy of business state. `internal/core` is the only package that reads and writes business tables
directly. Other processes and packages reach the data through its finite entry points
(`Store.Public`, `Store.Control` and a handful of dedicated methods).

This document describes core's transaction model, its consistency tools and the migration rules.
HTTP conventions are in [HTTP API conventions](http-api.en.md).

## Store

`core.Store` is an injected set of dependencies; there is no package-global database handle.

- `core.NewStore(db)` only takes the `*sql.DB` pool out of the injected GORM handle. It creates no
  tables and runs no migrations. GORM is used solely to open the pool (`internal/repository`);
  every business query is hand-written, parameterized SQL.
- The `Store` carries the collaborators needed at run time: the object storage configuration
  `ObjectStore`, the workflow asset downloader `WorkflowAssets`, the collaboration ports
  (`Directory`, `Forms` and others, see [Issue collaboration](../collaboration/interactions.en.md)),
  the IssueRun business hooks and window settings, the SSE event hub `Events` and the Controller
  signal hub `Signals`. `cmd/*` assembles them at startup; core never reads configuration or
  environment variables itself.
- `core.NewDevelopmentStore` exists only for test fixtures and the in-process simulator. It
  additionally opens a retired, unscoped clone contract and must never be used by the production
  server.

## Transactions: `transact`

Every read and write goes through `Store.transact`:

```text
BeginTx
  SELECT pg_advisory_xact_lock(67420911)    ← one global transaction-scoped advisory lock
  fn(t)                                     ← business logic, database work only
Commit
  publish post-commit hints: Controller signals, SSE events
```

**The global lock.** Every transaction first takes the same transaction-scoped advisory lock, so
all core transactions — read-only requests included — run serially in the database. This is a
deliberate trade: write throughput is exchanged for a simple, auditable lock order, and cross-table
invariants need no per-row locking design. The lock is released automatically on commit or
rollback and is never held across requests.

**Transactions do database work only.** A transaction never sends HTTP, runs Git, touches object
storage, reads or writes files, or waits on a Node. Flows that need external I/O are split into
"transaction → external call → transaction":

- Revision delivery checks the objects in object storage outside any transaction, then opens a
  transaction to record the result (see [Revisions](../agent-runs/revisions.en.md)).
- An archived session read verifies permission in a short transaction, signs and performs the
  object storage request after it commits, and re-checks permission in a new transaction.
- A Task Mode run triggered by a comment is dispatched only after the comment's transaction commits.

The exception is Git Server's per-request authorization. It reads current state in a separate
read-only `REPEATABLE READ` transaction without the global lock, because it only decides and never
writes (see [Git Server](../repositories/git-server.en.md)).

**Errors travel as panics and are restored at the `transact` boundary.** Business code inside a
transaction raises a `*Fault` with `reject(status, code)` or `require(ok, status, code)`; database
errors are wrapped as `databaseFailure` and raised the same way. `transact` recovers in one place:
a `*Fault` is returned as the error, a `databaseFailure` is unwrapped to the underlying error, and
any other panic keeps propagating so real programming errors are never swallowed. Any error rolls
back the whole transaction, so a request's writes either all take effect or none do.

**Query helpers.**

| Method | Purpose |
|---|---|
| `t.exec(q, args...)` | Run a write statement |
| `t.execRows(q, args...)` | Run a write statement and return the affected row count, for compare-and-set guards that must tell "I claimed it" from "someone else already did" |
| `t.list(q, args...)` | Wrap the query as `SELECT row_to_json(resource) FROM (…) resource` and return `[]Object` |
| `t.one(q, args...)` | The first row of `list`, or `nil` when there is none |

`list`/`one` convert only **top-level** column names from `snake_case` to `camelCase`; keys nested
inside `jsonb` columns keep their database spelling. Aggregate columns are read under their SQL
alias (`SELECT min(position)` yields the key `min`). A `null` `issueRunId` is removed from results,
because run Workspaces never appear in public documents.

## Publish only after commit

Side effects collected while a transaction runs are emitted only after `Commit` succeeds and are
discarded on rollback:

| Collected item | Action after commit |
|---|---|
| A newly queued or retried Workspace operation | `ControlHub` sends `OperationAvailable` |
| A new clone request or execution work item | `ControlHub` sends `WorkAvailable` |
| A new Thread command | `ControlHub` sends `ThreadCommandAvailable` |
| A business change inside a Space | `SpaceHub` sends an invalidation event to that Space's SSE subscribers |

All of these are **hints** and carry no business state:

- `ControlHub` signals are delivered at most once, are not persisted, and a slow subscriber simply
  loses them. A Controller that misses a signal loses no work; it claims again on its own schedule
  (see [Controller integration](../runtime/controller-integration.en.md)).
- `SpaceHub` events only tell the frontend that something changed; the frontend then reads the
  authoritative state over REST (see [HTTP API conventions](http-api.en.md#sse-invalidation-stream)).

Both hubs live in process memory: only subscribers connected to the same Cloud process receive
them, with no replay and no cross-instance delivery.

## Business hooks in the same transaction

An Agent run's execution evidence and its business state are owned by two different sides. The
control plane writes execution evidence such as `execution_work`, `node_executions`,
`node_event_receipts`, `thread_commands` and `revisions`; the business side writes the phase and
Thread columns of `issue_runs` and `thread_entries`. Every table has exactly one writer.

The control plane calls five business hooks (`OnThreadEvents`, `OnSessionEnded`,
`OnRunWorkspaceSettled`, `OnRunWorkspaceDeleted`, `OnDeliverySettled`) inside the transaction that
takes over the evidence; `core.BindBusinessHooks` binds them at startup. A hook receives the
caller's `*sql.Tx`, recovers the enclosing `transaction` through the context, and writes business
tables and declares follow-up work in that same transaction. It opens no transaction, starts no
goroutine and performs no external I/O. When a hook returns an error, the control plane rolls the
whole takeover back with `409 business_hook_failed`; the Node event is not acknowledged and will be
replayed. A business transition therefore always commits or rolls back together with the evidence
it is based on. The full lifecycle is in [Agent IssueRuns](../agent-runs/issue-runs.en.md).

## Consistency tools

**Resource versions.** Mutable resources carry an integer `version`. A modifying request must send
the current version: a missing one returns `428 version_required`, a mismatch returns
`409 version_conflict`, and every read-modify-write executes `version=version+1`.

**Idempotency records.** Public requests that create resources or cause side effects (`POST`,
`DELETE`) must carry an `Idempotency-Key`:

| Situation | Record table and scope |
|---|---|
| Requests inside a tenant | `idempotency_records`, keyed by `(tenant_id, user_id, key)` |
| Self-serve tenant creation (`POST /api/v1/tenants`) | `idempotency_records`, keyed by `(user_id, key)` and matched across every tenant the user belongs to; the record is stored under the new tenant |
| Requests before joining a tenant (redeeming an invitation, submitting an application) | `join_idempotency_records`, keyed by `(user_id, key)` |

The request fingerprint is the SHA-256 of the `method`, the `path` and the normalized JSON body. A
request with the same key and the same fingerprint returns the recorded status and response body,
and this happens **before** the version check, so "retry with the old version after the response
was lost" never creates a second resource. The same key with a different fingerprint returns
`409 idempotency_conflict`. Once a member is disabled, the replay is rejected by the membership
check first, like any other request.

Internal Controller RPCs get the same semantics from a caller-generated `submission_id`; see
[Controller integration](../runtime/controller-integration.en.md#submission-identity).

**One active operation per Project.** A Project has at most one lifecycle operation in `queued`,
`running`, `retry_wait` or `blocked` at a time; a new operation is refused with
`409 operation_in_progress` (see [Workspace lifecycle](../runtime/workspaces.en.md)).

**Database constraints.** Cross-row invariants the database can enforce are written as constraints
rather than left to application code:

- Composite foreign keys bind each child resource to its tenant and ownership. A Workspace, for
  example, inherits the Project's full ownership through `(project_id, tenant_id, owner_user_id)`,
  and columns that reference a user point at `tenant_memberships` through `(tenant_id, user_id)`.
- Deferred constraint triggers guarantee that every live Project has exactly one live main
  Workspace and that the last effective administrator cannot be disabled or demoted.
- Partial unique indexes and `CHECK` constraints pin state values and rules such as "at most one
  unfinished".

**Database time.** Leases, expiries, deadlines and idle windows are judged with PostgreSQL's
`now()` or `clock_timestamp()`, never with a Cloud, Controller or Node process clock.

**Soft deletion.** Deleting a business row writes `deleted_at` and keeps the references needed for
termination, cleanup, audit and recovery; queries spell out `deleted_at IS NULL`. Pure join tables
(such as the Issue–label association) are deleted outright.

## Schema migrations

Migrations are SQL files under `internal/core/migrations/`, ordered by file name and embedded in the
binary at build time.

- **Applied only by `cloudctl -command migrate`.** `Store.Migrate` runs the not-yet-applied files in
  order, under the same global lock and in one transaction, and records each file name and SHA-256
  checksum in `schema_migrations`. Deployments run it with a separate migration identity; business
  processes use an account with DML rights only.
- **Business processes verify and never run DDL.** At startup the server, gateway, model-gateway and
  git-server call `Store.CheckSchema`: every migration in the binary must be applied with a matching
  checksum, and the database must contain no migration the binary does not know. Any violation
  stops startup.
- **Applied migrations are immutable.** The checksum exposes any edit. Every later change is a new
  forward migration, tested against both a fresh database and an upgrade from the previous schema.
- **Number in merge order.** Migrations are tracked by full file name and run in file-name order. A
  new migration's number must follow the latest number on main at the time it merges; never add a
  number equal to or lower than an existing one. The historical duplicate numbers
  (`0027_verified_revisions.sql` and `0027_workflows.sql`, `0033_personal_model_connections.sql` and
  `0033_revision_resume.sql`) stay as they are.

The contents of each migration are described in the
[migration directory README](../../internal/core/migrations/README.en.md).

## Related decisions

- [Collaboration Space schema, deterministic backfill and migration immutability](https://github.com/ora-space/specs/blob/main/decisions/cloud/persistence/0-collab-space-schema-and-backfill.md)
- [Cloud owns the Internal Control gRPC contract and the authoritative persistence](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/0-cloud-owned-internal-grpc-contract.md)
- [Execution contract for Agent sessions, Threads and upload grants](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260928-agent-run-executions-thread-and-upload-grants.md)
