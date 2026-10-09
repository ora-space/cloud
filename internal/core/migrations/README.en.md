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
- **`0015_plugins.sql`**: Adds the plugin catalog, per-space selections and runtime installation records.
- **`0016_workspace_runtime_follows_node.sql`**: Retires storage/worktree provisioning and uses independent Workspace data, Node and clone initialization. Historical records remain readable.
- **`0017_tenant_membership_and_join.sql`**: Consolidates one space per tenant with tenant membership as the sole authority; restores mandatory project space association, reserves slugs globally even after archiving, and adds durable IDaaS identity association, invitation and join-request records. Incompatible test data with multiple spaces, unscoped projects, reused slugs or divergent tenant and space names must be rebuilt; the migration never silently splits or renames data.
- **`0027_workflows.sql`** (append-only, workflow editor port): the workflow graph document `workflows` — the whole tenant-owned graph (`graph jsonb`: nodes / edges / viewport / annotations / global variables). Deliberately one table and one graph column: the graph is a document, not a relational aggregate, so there are no per-node or per-edge tables and no draft/published split.
- **`0028_workflow_snapshots.sql`**: immutable versioned captures, `workflow_snapshots`. Publishing freezes the live graph into a row under the next per-workflow version; restoring writes a chosen snapshot back over the live graph. Snapshots are append-only — an edit never rewrites one.
- **`0029_workflow_runs.sql`**: `workflow_runs`, one execution against one frozen snapshot. The run viewer needs only the snapshot graph (the canvas) and per-node states (the coloring); the graph itself is read through `snapshot_id` from `workflow_snapshots.graph` rather than copied. Cloud has no workflow engine: a created run stays `pending`, and only development fixtures explicitly wired through `Store.WorkflowRunSimulator` advance it — the same "never claim real work ran" convention as `issue_runs`.

> Numbering note: these three were `0014–0016` and collided with upstream's same-numbered migrations (`0014_clone_coordination` / `0015_plugins` / `0016_workspace_runtime_follows_node`), so merging `upstream/main` moved them to `0024–0026` per the append-only rule. Upstream then added its own `0024_agent_run_control_plane` / `0025_agent_control_integrity` / `0026_run_runtime_control`, so they moved once more to `0027–0029`, after `0026`. `schema_migrations.version` is the **full file name**, so every rename changes a migration's identity: a database that already applied the old names must be rebuilt or have `schema_migrations` updated by hand, otherwise `CheckSchema` refuses to boot with "embedded migration is missing".

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

## Append-only multiplayer runtime migrations

0025 adds a monotonic Thread-command acceptance ordinal, preserving the old created_at/id order on upgrade, and enforces one session per run lifetime. 0026 retains a separate IssueRun maintenance binding after Workspace initialization so session/delivery permits do not occupy the Project operation slot. User control and run ownership remain exclusive. `integration/agent_control_upgrade_test.go` covers the real 0024 upgrade and repeated migration.

0018–0024 follow published 0017 without rewriting it: 0018 records only provable creators, otherwise NULL; 0019 persists PostgreSQL control sessions, epochs and audit; 0020 independent force-stop targets and restart phases; 0021 separates Node operation IDs from Cloud operation IDs and retains retries; 0022 plugin requester, pinned release and durable pending intent; 0023 credential ownership evidence, availability, scope, capabilities and version; 0024 adds run-workspace association, plugin/session/delivery execution registration, Thread commands and Space Agent rows, and refuses new plugin effects. Existing owner FKs, credential associations and operation history remain intact. Personal/unknown references associated with inactive members freeze on upgrade; team attribution and effective binding are never guessed.

Real PostgreSQL fresh/0017-upgrade evidence is in integration/runtime_upgrade_test.go, runtime_control_test.go, repository_credentials_test.go and plugin_pending_test.go. Transactions contain database work only; Node recovery logs are not a second business authority.

0027 only adds Revision, verification verdicts and unconfigured-storage skips, preserving raw Node evidence. Composite foreign keys enforce tenant/run/Workspace/project scope; changed and unchanged metadata are exclusive. revision_upgrade_test.go covers real 0026 and repeated upgrades; agent_plugin_upgrade_test.go covers retained 0023 in-flight plugin effects and restarted Node steps.

Merging current upstream preserves the complete filenames and original SQL of `0027_verified_revisions.sql`, `0027_workflows.sql` and `0028–0029`. Equal numeric prefixes do not imply equal identities: the runner tracks each applied migration by its complete filename and checksum. `TestRevisionUpgradePreservesPublishedWorkflowSchema` directly verifies adding Revision tables to a database that already applied upstream workflow migrations; repeated migration preserves complete workflow documents, snapshots, runs and the existing migration ledger.

0030–0031 follow published 0029 and are the **only** schema this repository adds when the B-side business lifecycle moves onto the upstream control plane: the parts functionB had already gained upstream (the execution side of 0024–0029) are not re-created. 0030 adds business columns to `issue_runs` alone (`phase`, `workspace_id`, `cancel_requested_at`, `thread_state`, `idle_since`), the agent-only column constraint, the exclusive `workspace_id` binding and the partial idle-thread index; 0031 creates `thread_entries` (primary key `(run_id,seq)`, Node-sourced entries idempotent on `(node_execution_id,node_sequence)`, user entries always carrying a turn lifecycle, `record` bounded to a 256 KiB JSON object). Both are purely forward and retry-safe, backfill nothing and rewrite no existing row — 0030's columns are created by 0030, so no pre-existing row can hold a value to migrate, and a non-agent run's row is untouched. Real fresh-database, upgrade and repeated-migration evidence is in `integration/migration_upgrade_path_test.go`: `TestMigration0030AgentRunBusinessLifecycleAppliesFreshAndUpgrades` and `TestMigration0031AgentRunThreadEntriesAppliesFreshAndUpgrades`.

0032 follows 0031 and adds `user_git_identities` (identity-access git identity D1): at most one row per user, keyed by and referencing `users(id)`, a `name` of 1–200 characters with no line break or angle bracket, an `email` of at most 254 bytes shaped `local@domain` with no whitespace or angle bracket, and a `version` that is the row's own optimistic-concurrency counter. The identity has its own table rather than columns on `users` because `/me` and every user read return the `users` row whole, so a git email there would travel to every reader; no row means the default identity applies. Purely forward and retry-safe; no existing table is altered. Fresh-database, upgrade and repeated-migration evidence is `TestMigration0032UserGitIdentitiesAppliesFreshAndUpgrades` in `integration/migration_upgrade_path_test.go`.
