# Changing the Code

English | [中文](changing-the-code.md)

This is the practical guide for changing Cloud: where code goes, what to touch when adding an endpoint or a table,
the fixed shapes of a few common extensions, and the usual pitfalls. General coding rules (error handling,
concurrency, dependencies, file size) are in the repository root's `AGENTS.md`; for changes that touch state
transitions, ownership, persistence, external side effects, recovery, security or compatibility, read the relevant
specs ADRs first.

## Where code goes

| Location | What belongs there |
|---|---|
| `cmd/*` | Only configuration loading, dependency wiring, lifecycle and exit codes; no business logic, no SQL |
| `internal/core` | All authoritative state and policy: aggregates, state transitions, transactions, authorization decisions |
| `internal/api/router` | The HTTP route allowlist, dual-credential checks, strict decoding, mapping `Fault` to HTTP |
| `internal/controlgrpc` | Translation from gRPC to `Store.Control`, with no business logic |
| `internal/contract` | The Go model of the OpenAPI document |
| `internal/gateway`, `internal/modelgateway`, `internal/gitserver` | Implementations of the separate processes; they read and write business state only through methods core exposes |
| `internal/repository`, `internal/config`, `internal/logger` | Connection pool, configuration loading, logging |
| `internal/simulator`, `internal/collab` | Doubles and fixtures for development and tests only |

Dependencies point one way: `cmd` depends on everything else; router and controlgrpc depend on core; core depends only on
the standard library, GORM and the migrations directory, and never imports a presentation-layer package. When core needs an
external capability it declares the interface on its own side (object storage, workflow asset downloads, the collaboration
ports) and `cmd` injects the implementation. New implementation packages go under `internal/`; only code genuinely meant as
public API for external modules goes into `pkg/`.

## Adding or changing a public endpoint

1. **Route**: add `{Method, Path, "", Fields}` to `router.Routes()`, where `Fields` are the request body fields allowed. New
   path parameters are bound to fields of `core.PublicRequest` in the route handler; add a field to `PublicRequest` if needed.
2. **Field types**: for a request field that is not a string, add a case to `validField` that fixes its type or element shape.
   The type system is closed and everything defaults to string.
3. **Dispatch**: add a branch to `Store.Public` for writes or to `readPublic` for reads (see "Dispatch order" below). The
   business logic goes in a core transaction: scope to the tenant, check membership and role, and check the version where
   needed; idempotency for `POST` and `DELETE` is handled by `Public` itself.
4. **OpenAPI**: add the schema and the response, request and optional-field definitions in `internal/contract/openapi.go`, and make
   sure list endpoints are recognized as lists. Every response field must be declared in the schema.
5. **Generate**: run `task frontend:generate` (which runs `task openapi` first) and commit `api/openapi.json` and `frontend/src/api`
   together with the source. Generated files are never edited by hand.
6. **Test**: drive the new endpoint through the router in `integration/`; the tests' HTTP transport validates every real response
   against OpenAPI. When the change alters business state inside a Space, remember to collect the matching SSE invalidation event in
   the transaction.

## Adding a table or changing the schema

1. Create `internal/core/migrations/NNNN_description.sql`, numbered after the latest number on main at the time it merges; applied
   migration files never change. Alter tables with a new migration.
2. Follow the existing conventions: a `id uuid` primary key, `version` (`CHECK(version>0)`) plus `created_at` and `updated_at` on
   mutable resources, soft deletion of business rows through `deleted_at`; every table is scoped by `tenant_id`, and columns that
   reference a user point at `tenant_memberships` through a `(tenant_id, user_id)` composite foreign key. Encode cross-row rules the
   database can enforce as constraints.
3. Data every tenant must have (such as the Issue system status columns) cannot be seeded in a migration, because tenants only appear
   at run time; fill it in lazily on first read or write with `ON CONFLICT DO NOTHING`.
4. Migrations must be safe to retry and tested on both a fresh database and an upgrade from the previous schema. The upgrade test in
   `integration/cloud_test.go` keeps a fixed "before upgrade" baseline list; add your file to it only when the test is meant to upgrade
   through your migration.

## Changing the Controller contract

1. Edit the `.proto` files under `proto/`: only non-breaking changes in `v1`, and breaking changes as a parallel `v2` package. Run
   `task proto:lint` and `task proto:breaking`.
2. Regenerate `internal/controlpb` with `task proto:generate` and commit it with the change.
3. In `internal/controlgrpc`, translate the new RPC into one `core.ControlRequest` action; the business logic lives in core's
   `Store.Control`, state-changing actions are wrapped in submission identity, and actions that need the lease check the epoch.
4. The contract's semantics are owned by specs ADRs; desktop pins the commit merged into main and regenerates its client (see
   [Controller integration](../runtime/controller-integration.en.md#changing-the-contract)).

## Adding a background loop

The Cloud server's periodic work all has one shape; follow it for new loops:

- The loop is owned by `cmd/server`: start it with `pluginmarket.RunSyncLoop` on the shared `WaitGroup`, so the process cancels it on
  shutdown and waits for the in-flight pass.
- Each pass first does a bounded, deterministically ordered read-only scan outside the global lock, then opens one short transaction
  per object that **re-reads** the scan's condition and transitions with a compare-and-set. A repeated pass must do nothing.
- Time windows are always compared on database time; an object that is temporarily stuck rolls back and waits for the next pass without
  flooding the logs, and only a failure of the scan itself returns an error.

## What goes in business hooks

An Agent run's business transitions are written in the control plane's five hooks (see
[business hooks in the same transaction](../core/persistence.en.md#business-hooks-in-the-same-transaction)):

- Hooks run in the caller's transaction and recover it through `businessTransaction`; they open no transaction, start no goroutine,
  perform no external I/O and defer no post-commit side effect.
- Hooks write only business tables, and execution evidence is written only by the control plane; returning an error rolls back the
  whole takeover with `409 business_hook_failed`, so a transition that authoritative data cannot justify must never be "tolerated".

## Dispatch order and other pitfalls

1. **Dispatch matches path substrings, so order matters.** In `Public` and `readPublic`, longer and more specific branches must come
   before broader ones: an Issue's sub-resources (comments, labels, subscribers, interactions, the Thread) are dispatched inside the
   Issue branch; `.../runs/:rid/thread` comes before `.../runs`; `.../workflows/:wfid/runs` comes before the Issue `/runs` branch. The
   OpenAPI generator also has branches matching the `/issues` substring, and new resource branches must come before them.
2. **Gin's static segments beat parameter segments**: `/issues/batch` is safe because the literal `batch` wins over `:iid`, but
   `c.Param("iid")` is then empty.
3. **Only top-level column names become camelCase**; keys nested in `jsonb` keep their database spelling, and aggregate columns are read
   under their SQL alias.
4. **`updated_at` is not automatic**: every `UPDATE` writes `updated_at=now()` itself, and every read-modify-write does
   `version=version+1`.
5. **Soft deletion must be spelled out in every query** as `deleted_at IS NULL`.
6. **No external I/O inside transactions**: HTTP, Git, files, object storage and Nodes stay outside; split the flow into "transaction →
   external call → transaction" when needed, with the second transaction re-checking its preconditions.
7. **A comment is not a run**: never add `run_id` or `interaction_id` to the comments table; a run's origin lives in its own
   `trigger_evidence_*`.
8. **Answer `404` for invisible resources**; never leak "exists but forbidden" with `403`.
9. **Keep internal control fields out of public documents**: fields such as the run Workspace, phase, execution identity or object keys
   are stripped explicitly in projections; after adding a column, check that it cannot leak out through a `SELECT *`.

## Keeping docs and specs in step

- When behavior, configuration or an interface changes, update the matching document under `docs/` in the same change, both languages
  together; implementation details inside a package go into that module's `README.md` and `README.en.md`.
- For changes that implement a specs ADR, state the ADR's status in the PR description; when the change completes the implementation,
  also change the ADR to `implemented` in specs and keep the core test cases' evidence status accurate.
