# internal/core: Authoritative Domain Engine

[中文](README.md) | [English](README.en.md)

`internal/core` is the authoritative domain and state-machine layer of Ora Cloud. It owns all business aggregates, state transitions, transaction boundaries, cryptographic token verification, and PostgreSQL persistence orchestration.

## Module map

- [migrations](migrations/README.en.md) defines the forward-only, linear PostgreSQL schema migration scripts and checksum verification.
- [plugins.go](plugins.go) persists and reads the plugin catalog snapshot (the `pluginmarket.CatalogSink` implementation) and owns the space plugin selection state machine (install/remove fan-out, the aggregation rule, SSE invalidation broadcasts).
- [session_list.go](session_list.go) and [session_history.go](session_history.go) provide tenant Agent
  session discovery, current-member authorization and complete registered JSONL archive reads.

## Architecture and runtime model

### Aggregates and relationships
- **Users & Identities**: Users are identified by stable IdP claims (`source`, `subject`). Huawei login keeps the IDaaS `uuid` as its subject and associates a directory-selected person through the verified `globalUserId`; employee numbers grant no access. Conflicting identity mappings are never merged automatically.
- **Tenants, Spaces & Memberships**: Each tenant has exactly one visible collaboration space, and a user may join and switch among many tenants. `tenant_memberships` is the sole authority for the peer `admin` and `member` roles and membership status. Public deployments use invitation or application links; corporate deployments verify active employees through Tianzhou.
- **Projects**: Each project belongs to a tenant and its sole collaboration space. `(tenant_id, owner_user_id)` remains the durable resource and credential ownership boundary, while any active tenant member can access tenant projects. Each Workspace clones the Binding's current snapshot into its own data, with its `requested_ref` (the Binding branch, i.e. `defaultBranch`) as the local branch; `project_storage` rows are retained history only.
- **Repository Bindings & Operations**: A project's external repository (URL, branch to sync, credential references) lives on its sole `repository_bindings` row, read under the project's published field names through `projectView`. The transaction that creates a project also creates its Binding and one `pending` sync with trigger `project_created` (`repository_operations`). CHECK constraints fix the sync and publish states and their terminal states, and a partial unique index allows at most one unfinished sync per Binding; `repository_snapshots` records each Binding's current verified snapshot. Entering `deleting` cancels the Operations that have not reached `running` and clears the resync request in the same transaction, `delete_project` starts only once no Operation is unfinished, and a failed `create_project` always leaves the project `active`. `AuthorizeGitOperation` decides the Repo Executor's Git access from the Operation's current state; `AuthorizeGitAccess` decides per request, from current state, the Git Server access of clone, Agent session and Revision delivery executions: every granted execution reads the whole Project repository, its execution kind decides which refs it may write, and plugin executions are always denied.
- **Workspaces & Tasks**: Each project has at most one active `main` workspace (enforced by the `one_main` partial unique index). Additional workspaces are `isolated` and map 1:1 with `tasks`.
- **Operations & Effects**: Mutations (such as project creation, workspace start/stop, or deletion) execute as durable `operations` (`queued`, `running`, `retry_wait`, `blocked`, `done`, `failed`). Operations decompose into durable `effects` representing external tasks executed by Substrate and Controller.
- **Workspace Clones & Git Server**: Cloud freezes the clone step input of create_project / create_workspace from the Binding's current snapshot (`operations.request.clone`) and parks the operation instead of dispatching when there is no snapshot; a Controller registers the dispatch (`clone_executions`, always of a Workspace operation) and takes over the Node result (`clone_event_receipts`). `git_server.go` defines `GitServer` (repository base URL and ref query), `GitRefResolver` and `ErrGitRefNotFound`; without a Git Server the clone step is `blocked` (`git_server_unconfigured`) and Revision delivery is `skipped`. Repository credentials gate no Node work.
- **Revision Delivery & Resume**: Delivery work input carries `historyKey` (session JSONL), the Node-local `revisionRef`, `gitRepositoryUrl`, `deliveryRef = refs/ora/deliveries/<deliveryWorkId>` and the `workspaceRef` taken from the checkout clone's input, and no bundle; without an object store (checked first) or a Git Server the delivery is `skipped` (`object_store_unconfigured` / `git_server_unconfigured`). Verification compares the declaration with the input, then outside any transaction queries the delivery ref through `GitRefResolver` (it must name the declared final commit) and HEADs the JSONL, and registers inside the transaction: a missing ref, another commit or a history mismatch is Cloud's `failed{verification_failed}`; an unreachable Git Server or object store is UNAVAILABLE, takes nothing over and stays replayable. `delivered` requires a final commit other than the base and the prior final; `unchanged` also pushes and registers its own delivery ref. `GrantRevisionUpload` signs only the input's `historyKey`. The resume candidate is the latest Revision of another run of the same Issue that has a `delivery_ref` and new commits; the session's `priorRevision` is `{revisionId, finalCommit, baseCommit, gitRepositoryUrl, deliveryRef}`, skipped when the Project or Git Server repository differs or resume was refused (`project_changed` / `repository_changed` / `resume_refused`). The session input's `workspaceDirectory` lists up to 100 of the Project's Workspaces, newest first, for reference only and never for authorization.
- **Nodes & Sessions**: `node_instances` records Nodes on runtime workspaces. Agent conversations
  persist in `issue_runs`, `thread_entries` and execution receipts; `revisions` registers delivered
  complete JSONL objects in storage.

### Concurrency and locking
- **Transactional advisory lock**: Phase-one serializes domain mutations using PostgreSQL's `SELECT pg_advisory_xact_lock(67420911)` within `Store.transact`. This eliminates race conditions during aggregate state transitions while keeping locking database-local.
- **Single active operation per project**: Enforced by `idleProject`: a new project operation cannot be scheduled if another operation is currently `queued`, `running`, `retry_wait`, or `blocked`.
- **Optimistic concurrency**: Mutations on mutable entities require an explicit `version` parameter. A missing version returns `428 version_required`; a mismatched version returns `409 version_conflict`.
- **Controller leases**: Controller workers acquire exclusive leases via `/internal/v1/controller-lease/acquire`, renewed periodically. Work dispatching uses monotonic `epoch` fencing to reject stale controller instances.

### Idempotency
- Creation and joining writes require an `Idempotency-Key` header. Tenant-scoped records use `(tenant_id, user_id)`; pre-membership join records use `user_id`.
- Requests compute a SHA256 hash of the method, path, and normalized body.
- An identical request replaying an existing key returns the previously stored HTTP response.
- A differing request using the same key is rejected with `409 idempotency_conflict`.

### Authentication and trust
- `Authenticator` validates JWT tokens using Ed25519/RS256 public keys loaded at startup:
  - **Service tokens**: Carry `kind="service"` and `role` (`gateway`, `controller`, or `node`).
  - **User tokens**: Carry `kind="user"` and are forwarded in the `X-Ora-User-Token` header by the gateway. The router ensures `user.Caller == service.Subject`.

### Error handling
- The domain exclusively raises `*Fault` values containing a machine-readable `Code`, dynamic `Params`, and an HTTP `Status`.
- `ErrorCode(err)` converts domain and internal database errors into client-safe `Fault` objects, mapping unexpected errors to `500 internal_error` without leaking database schema or credential information.

## Boundaries and invariants

- **No long-lived transactions**: Transactions must never encompass network calls, Git operations, Substrate calls, or child process execution.
- **No in-memory state**: All state transitions must be committed to PostgreSQL before returning success.
- **Tenant isolation**: Project and runtime reads verify active tenant membership and scope queries by a trusted `tenant_id`. `owner_user_id` remains for resource, credential, and execution ownership; it does not exclude another member of the same tenant.

See [Database migrations](migrations/README.en.md), [Core contract](../../docs/core-contract.md), and [Authentication](../../docs/authentication.md).

## Personal model connections

`model_connections.go`, `model_credentials.go`, `model_run_binding.go` and `model_grants.go`
own private metadata, immutable ciphertext references, run snapshots and temporary-token digests.
Core never receives or decrypts upstream API keys. See [personal model connections](../../docs/model-connections.en.md)
for freezing, authorization, renewal and revocation rules.

## Session Dashboard reads

`GET /api/v1/tenants/:tid/agent-sessions` lists persisted Agent IssueRuns, excluding deleted runs
and Issues. Filters are `projectId`, one `status`, the exact plugin identifier `agent`, and creation
times `from` (inclusive) / `to` (exclusive, RFC 3339). `limit` defaults to 50 and caps at 100.
Ordering is descending `(createdAt,id)`; `after` is the preceding page's `nextCursor` UUID and must
resolve to a visible Agent Run in the current tenant. `before` is unsupported.

Both new readers dispatch before the general `Public` transaction. `sessionReadIdentity` resolves
existing identities and Huawei aliases and checks conflicts, active user status and current tenant
membership without provisioning users, binding aliases or refreshing runtime state. The list projects
Issue/project identity, Agent plugin, status, timestamps and a nullable frozen model summary.
`historyAvailable` means an unexpired Revision registration exists; storage reachability and size
bounds are checked only when reading the archive.

`GET /api/v1/tenants/:tid/issues/:iid/runs/:rid/history` calls `Store.SessionHistory`. A short
transaction checks membership, Issue, Agent Run and unexpired registration. Private signed S3 GET
runs outside the transaction. Only the registered key is accepted, with the exact layout
`revisions/{tenant}/{run}/{work UUID}/session.jsonl`; callers cannot supply a URL or path. Actual
bytes must match the registered size and SHA-256, with a 32 MiB / 100000-object bound and valid UTF-8
JSONL. The read refuses redirects, propagates cancellation and has a 30-second storage timeout.
A second short transaction rechecks current authorization and unchanged registration before return.

The response is `{source:"archive",records:[...],sizeBytes,sha256}`. Records retain file order,
duplicate `seq` updates, original fields and JSON numeric precision for consumer-side history replay.
Absent/expired registration returns `404 history_unavailable`; oversized input returns
`413 history_too_large`; invalid bytes or JSONL return `502 history_invalid`; storage failures return
`502 history_unavailable`. No storage URL, object key or credential is exposed.

This version does not represent Chat or Workflow Runs as Agent sessions and never scans personal
session directories. Live views keep using the existing Thread projection; space SSE is an invalidation
hint. Neither Thread nor an archive guarantees token/cost usage, parent-child relationships, or every
tool's input/output and timing. Missing data stays unavailable. Record `seq` and Cloud Thread paging
`seq` are separate sequences and must remain distinct during replay.

`session_history*_test.go` uses synthetic data, real HTTP and isolated PostgreSQL schemas for access,
scoped keys, read-only identities, revocation/replacement during I/O, byte/checksum/JSONL bounds,
cancellation and numeric preservation. `session_list_test.go` covers list scope and pagination.
No real user conversation is a product test fixture.

## Model request statistics

`model_attempts.go` records intent before outbound HTTP using the live grant's tenant, run, execution and frozen model.
Append-only migration `0035_model_attempt_statistics.sql` adds `model_request_attempts` with composite scope foreign keys;
existing sessions are not backfilled. Rows contain statuses, protocol/model, safe error codes, gateway timing and nullable
usage counters, never request/response content or credentials. Failed intent persistence blocks forwarding. Finalization
may close a revoked grant's existing attempt; identical completion is idempotent and conflicting completion is rejected.

`session_model_statistics.go` serves authorized run `/statistics` and `/model-calls` reads independently of archives.
Statistics aggregate every recorded HTTP attempt in the run; calls page ascending `(startedAt,id)`, default 50, maximum 100.
Every metric carries `value/knownCount/totalCount/availability`: missing stays null, measured zero stays zero, and unfinished
usage snapshots remain partial. `started` may mean an active request or a lost final observation, never inferred success,
failure or zero consumption. Reads do not provision identities or refresh lifecycle state. Real PG tests cover immutable
concurrent completion, coverage, pagination and database-enforced read-only access.
