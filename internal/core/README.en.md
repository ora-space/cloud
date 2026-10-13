# internal/core: Authoritative Domain Engine

[中文](README.md) | [English](README.en.md)

`internal/core` is the authoritative domain and state-machine layer of Ora Cloud. It owns all business aggregates, state transitions, transaction boundaries, cryptographic token verification, and PostgreSQL persistence orchestration.

## Module map

- [migrations](migrations/README.en.md) defines the forward-only, linear PostgreSQL schema migration scripts and checksum verification.
- [plugins.go](plugins.go) persists and reads the plugin catalog snapshot (the `pluginmarket.CatalogSink` implementation) and owns the space plugin selection state machine (install/remove fan-out, the aggregation rule, SSE invalidation broadcasts).

## Architecture and runtime model

### Aggregates and relationships
- **Users & Identities**: Users are identified by stable IdP claims (`source`, `subject`). Huawei login keeps the IDaaS `uuid` as its subject and associates a directory-selected person through the verified `globalUserId`; employee numbers grant no access. Conflicting identity mappings are never merged automatically.
- **Tenants, Spaces & Memberships**: Each tenant has exactly one visible collaboration space, and a user may join and switch among many tenants. `tenant_memberships` is the sole authority for the peer `admin` and `member` roles and membership status. Public deployments use invitation or application links; corporate deployments verify active employees through Tianzhou.
- **Projects**: Each project belongs to a tenant and its sole collaboration space. `(tenant_id, owner_user_id)` remains the durable resource and credential ownership boundary, while any active tenant member can access tenant projects. Each Workspace clones the project repository into its own data at its `requested_ref`; `project_storage` rows are retained history only.
- **Workspaces & Tasks**: Each project has at most one active `main` workspace (enforced by the `one_main` partial unique index). Additional workspaces are `isolated` and map 1:1 with `tasks`.
- **Operations & Effects**: Mutations (such as project creation, workspace start/stop, or deletion) execute as durable `operations` (`queued`, `running`, `retry_wait`, `blocked`, `done`, `failed`). Operations decompose into durable `effects` representing external tasks executed by Substrate and Controller.
- **Clone Requests**: independent work items accepted idempotently by `(tenant_id, actor_user_id, request_id)`, outside the operation/effect model; a Controller claims them over the internal control contract, registers the dispatch (`clone_executions`) and takes over the Node result (`clone_event_receipts`). The public `/clones` routes are visible to the submitting user only; `state` is projected from the request state and the execution result.
- **Nodes & Sessions**: `workspace_nodes` represent active execution containers bound to a workspace. `sessions` track user conversational threads.

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

### Credential-executor dispatch and mirror preparation

Cloud dispatches two things to the credential executor (C). They share one deployment section and have two independent switches, and neither is wired by default:

- **`CloneDispatcher`** (`credentialed_clone.go`): signs one single-use authorization per admitted credentialed clone and dispatches it. Only with it installed can `requireRepositoryAccess` ever admit a credential-bearing Project.
- **`MirrorDispatcher`** (`mirror_dispatch.go`): signs a `mirror_prepare` authorization per claimed preparation attempt and dispatches it. The authorization binds the Operation, the attempt generation, the executor's SPIFFE identity, the repository, the ref and the capability set, and carries **no** execution id and no Node runtime triple; settling the attempt writes the returned commit and mirror generation as the `ready` evidence.

Mirror preparation is driven by `PrepareMirrorsOnce`, and `cmd/server` starts that loop only when a `MirrorPreparer` is installed (off by default). Each attempt is exactly three steps and **no transaction spans the dispatch**: a short claim transaction (`FOR UPDATE` plus the lease), issuance and dispatch outside any transaction, then a short settlement transaction. The lease is 5 minutes, one Operation gets at most 3 attempts, and a failure backs off 30 seconds; `mirror_preparations.generation` increases monotonically and is the late-result fence — an answer whose generation has been superseded is refused rather than written. When attempts run out or the credential is terminally refused, the Operation is settled under the `controller_epoch` fence. The authorization lifetime is bounded by the attempt lease (`internal/config` enforces it at load), so no dispatch can hand out authority that outlives the attempt it serves and whose result the fence would discard anyway. Dispatch failures are classified in exactly two halves: the executor's own stable reason code, and "the executor was unreachable".

## Boundaries and invariants

- **No long-lived transactions**: Transactions must never encompass network calls, Git operations, Substrate calls, or child process execution.
- **No in-memory state**: All state transitions must be committed to PostgreSQL before returning success.
- **Tenant isolation**: Project and runtime reads verify active tenant membership and scope queries by a trusted `tenant_id`. `owner_user_id` remains for resource, credential, and execution ownership; it does not exclude another member of the same tenant.

See [Database migrations](migrations/README.en.md), [Core contract](../../docs/core-contract.md), and [Authentication](../../docs/authentication.md).
