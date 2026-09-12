# internal/core: Authoritative Domain Engine

[中文](README.md) | [English](README.en.md)

`internal/core` is the authoritative domain and state-machine layer of Ora Cloud. It owns all business aggregates, state transitions, transaction boundaries, cryptographic token verification, and PostgreSQL persistence orchestration.

## Module map

- [migrations](migrations/README.en.md) defines the forward-only, linear PostgreSQL schema migration scripts and checksum verification.

## Architecture and runtime model

### Aggregates and relationships
- **Users & Identities**: Users are identified by stable IdP claims (`source`, `subject`). User creation is tied to first authenticated access or administrative bootstrap.
- **Tenants & Memberships**: Tenants isolate organizational boundaries. Users belong to tenants with either `admin` or `member` roles.
- **Projects**: Owned by `(tenant_id, owner_user_id)`. Each project has an associated repository URL and default branch, linked to a single `project_storage` row.
- **Workspaces & Tasks**: Each project has at most one active `main` workspace (enforced by the `one_main` partial unique index). Additional workspaces are `isolated` and map 1:1 with `tasks`.
- **Operations & Effects**: Mutations (such as project creation, workspace start/stop, or deletion) execute as durable `operations` (`queued`, `running`, `retry_wait`, `blocked`, `done`, `failed`). Operations decompose into durable `effects` representing external tasks executed by Substrate and Controller.
- **Nodes & Sessions**: `workspace_nodes` represent active execution containers bound to a workspace. `sessions` track user conversational threads.

### Concurrency and locking
- **Transactional advisory lock**: Phase-one serializes domain mutations using PostgreSQL's `SELECT pg_advisory_xact_lock(67420911)` within `Store.transact`. This eliminates race conditions during aggregate state transitions while keeping locking database-local.
- **Single active operation per project**: Enforced by `idleProject`: a new project operation cannot be scheduled if another operation is currently `queued`, `running`, `retry_wait`, or `blocked`.
- **Optimistic concurrency**: Mutations on mutable entities require an explicit `version` parameter. A missing version returns `428 version_required`; a mismatched version returns `409 version_conflict`.
- **Controller leases**: Controller workers acquire exclusive leases via `/internal/v1/controller-lease/acquire`, renewed periodically. Work dispatching uses monotonic `epoch` fencing to reject stale controller instances.

### Idempotency
- Requests modifying state accept an optional `Idempotency-Key` header scoped to `(tenant_id, user_id)`.
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
- **Tenant isolation**: Every query enforces `tenant_id` and `owner_user_id` filtering. Cross-tenant leakage is prevented at the SQL constraint level.

See [Database migrations](migrations/README.en.md), [Core contract](../../docs/core-contract.md), and [Authentication](../../docs/authentication.md).
