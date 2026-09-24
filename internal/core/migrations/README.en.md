# Database Migrations Module

[中文](README.md) | [English](README.en.md)

This module contains Ora Cloud's linear, forward-only PostgreSQL schema migration catalog. Migrations are embedded directly into the Go application binary using `embed.FS` and applied deterministically by `cloudctl migrate`.

## Migration catalog

Migrations are executed in ascending numerical sequence. The sequence is **append-only**: `0001–0007` is the upstream baseline (byte-identical to `upstream/main`, never modified), `0008–0012` are the local Issue migrations, and `0013` onward are follow-up compatibility migrations.

- **`0001_core.sql`** (upstream): Foundational domain schema:
  - Identity & Access: `users`, `user_identities`, `tenants`, `tenant_memberships`, `credential_refs`.
  - Projects & Workspaces: `projects`, `project_storage`, `workspaces`, `workspace_worktrees`, `tasks`.
  - Execution runtime: `sandbox_instances`, `workspace_nodes`, `sessions`.
  - Control plane: `effects`, `operations`, `tickets`, `controller_leases`, `idempotency_keys`.
  - Invariants: Partial unique index `one_main` ensures at most one active `main` workspace per project. Foreign keys strictly enforce tenant and owner containment across all hierarchy tiers.
- **`0002_aggregate_guards.sql`** (upstream): Concurrency and mutual exclusion guards:
  - Prevents concurrent lifecycle mutations on the same project aggregate.
  - Ensures soft-deleted ancestors prevent active child state transitions.
- **`0003_resource_versions.sql`** (upstream): Optimistic concurrency controls:
  - Enforces `version` incrementing rules across mutable entities (`projects`, `workspaces`, `tasks`, `nodes`, `operations`).
  - Guards against lost updates in concurrent API operations.
- **`0004_effect_intent_and_ticket_scope.sql`** (upstream): Execution intent and ticket constraints:
  - Enforces strict scoping of execution tickets to active workspace nodes and valid admission epochs.
  - Binds durable effect declarations to specific operation phases.
- **`0005_gateway_auth.sql`** (upstream): Gateway authentication tables (accessed at runtime only by `cmd/gateway`):
  - `gateway_login_attempts`: one-shot login attempts; stores only SHA-256 digests of the attempt secret and `state`, rejects absolute, `//` and `/\` `return_to` values at the database layer, bounds the lifetime to one hour, and uses `consumed_at` to guarantee at most one session per attempt.
  - `gateway_sessions`: browser sessions; stores only the token digest, requires a non-null `expires_at` no later than 90 days after creation, keeps revocation time and the bounded `revoked_reason` together, and indexes identity revocation and bounded cleanup.
- **`0006_collab_spaces.sql`** (upstream, byte-identical to `upstream/main`): Collaboration Space schema:
  - `collab_workspaces`: tenant-scoped collaboration and visibility boundary (name, immutable slug, archive time, optimistic version). Archiving is a soft delete and does not release the slug: `UNIQUE(tenant_id, slug)` covers live and archived rows alike.
  - `collab_workspace_members`: members with roles (owner/admin/member), status (active/disabled), and optimistic version.
  - Strictly separated from the runtime `workspaces` table (Runtime Workspace, execution environments).
- **`0007_project_space_scope.sql`** (upstream, byte-identical to `upstream/main`): Project-to-Space association (**upstream semantics: mandatory**):
  - Creates a default Space (slug=`default`) for every existing tenant, including deleted tenants that still own projects.
  - Adds existing active tenant members to the default Space (admin maps to owner, member maps to member).
  - Adds `projects.space_id uuid NOT NULL` and binds every existing project to its tenant's default Space.
  - A composite foreign key `(space_id, tenant_id) REFERENCES collab_workspaces(id, tenant_id)` rejects cross-tenant ownership at the SQL level, and the `project_space_list(space_id, id)` index supports listing Projects by Space.
- **`0008_issues.sql`**: the `issues` (board) base table (formerly `0006_issues.sql`; forward-renumbered in the migration reconciliation so upstream `0001–0007` stay byte-identical).
- **`0009_issue_extensions.sql`**: `issue_statuses`, `issue_comments`, `labels`, `issue_labels`, `issue_subscribers`, `issue_views` + `issues` ALTERs (`number`, `properties`, status format check). (formerly `0007_issue_extensions.sql`)
- **`0010_issue_collaboration.sql`**: `issues` ALTERs (`assignee_type`/`assignee_id`/`project_ref` + backfill), `issue_comments` ALTERs (`parent_id`/`author_type`/`author_id`/`seq` + backfill + `UNIQUE(issue_id,seq)`), new tables `issue_runs`, `issue_activities`, `issue_context_refs`. (formerly `0008_issue_collaboration.sql`)
- **`0011_issue_interactions.sql`**: new table `issue_interactions` (the `@` interaction spine) — one row per selected collaboration target: `id, tenant_id, issue_id, comment_id, target_type, target_id, mode, task, run_id, created_at`. (formerly `0009_issue_interactions.sql`)
- **`0012_issue_interaction_input.sql`**: one generic additive column: `ALTER TABLE issue_interactions ADD COLUMN input jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(input)='object')` — the confirmed form values. Deliberately excludes `version`, a `status` enum, `confirmed_at` and a separate inputs table; `0011` is not modified. (formerly `0010_issue_interaction_input.sql`)
- **`0013_project_space_optional.sql`** (append-only compatibility migration): `projects.space_id` back to **NULLABLE** — upstream `0007` imposed `NOT NULL` + full project binding; the product decision (PS3 / D2=C) keeps a Space an *optional* grouping. `0013` only relaxes the constraint; it deliberately **does NOT unbind** the projects `0007` already assigned to their default Space (no data change, no scope shrink).
- **`0014_clone_coordination.sql`** (append-only, after upstream `0008–0013`): clone coordination through the internal control contract (independent of the Effect-level `operations` model):
  - `clone_requests`: work Cloud accepted in its own business transaction, idempotent on `(tenant, user, request_id)`, state `queued→dispatched→succeeded/failed`.
  - `clone_executions`: executions a Controller registers before dispatching (exactly one per request, Controller-chosen opaque identities), their input, terminal result and the lease epoch at registration.
  - `clone_event_receipts`: exact receipts `(execution, sequence, event)` of Node events, the only basis for acknowledging a Node.
  - `control_submissions`: identity, request digest and recorded response of every state-changing submission; the same identity with the same content replays the response instead of reapplying.

## Checksum integrity and immutability

- **`schema_migrations` table**: Tracks applied versions, their SHA256 checksums, and application timestamps (`version`, `checksum`, `applied_at`). **`version` is the full filename** (e.g. `0010_issue_collaboration.sql`), so renaming an applied migration changes its identity and breaks existing databases — once applied, a migration must never be modified; add a new file instead.
- **Server startup check**: At startup, `cmd/server` runs `store.CheckSchema`, verifying that:
  1. All embedded `.sql` migration files exist in `schema_migrations`.
  2. The SHA256 checksum of each embedded file matches the recorded checksum in the database.
  3. No unknown or extraneous migration versions exist in the database.
  If any mismatch or unapplied migration is found, the server terminates immediately.
- **No AutoMigrate**: The production server daemon **never** executes DDL or modifies table structures at startup. Migrations must be applied using `cloudctl migrate` under dedicated database administrator credentials.

See [core overview](../README.en.md), [cloudctl CLI](../../../cmd/cloudctl/README.en.md), and [Core contract](../../../docs/core-contract.md).
- **`0015_plugins.sql`** (append-only): the plugin marketplace's three tables plus enum widening:
  - `plugin_sources` (deployment-global source, default `official` namespace), `plugin_catalog_entries` (catalog snapshot; reads never leave the database), `space_plugins` (authoritative per-space selection, `UNIQUE(space_id, source_namespace, identifier)`), `workspace_plugin_instances` (fan-out execution facts with tenant/owner/project/workspace composite foreign keys).
  - Widens `operations.kind` (+install_plugin/remove_plugin), `operations.step` (+plugin), `external_effects.kind` (+plugin_ensure/plugin_delete); the original CHECKs live in 0001 (PG names them table_column_check), dropped and rebuilt with the extended sets.
