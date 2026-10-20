# Database migration directory

[中文](README.md) | [English](README.en.md)

SQL is embedded in the binary through `embed.FS` in `core/store.go`. `Store.Migrate` executes unapplied files in lexicographic order of complete filenames under a transaction advisory lock. `schema_migrations` stores complete filenames, SHA-256 and application times. Numeric prefixes are not unique identities: both `0027` files and both `0033` files apply independently.

## File index

There are currently 53 SQL files.

| SQL file | Schema change |
|---|---|
| [0001_core.sql](0001_core.sql) | Base domain tables |
| [0002_aggregate_guards.sql](0002_aggregate_guards.sql) | Aggregate ownership constraints |
| [0003_resource_versions.sql](0003_resource_versions.sql) | Resource versions |
| [0004_effect_intent_and_ticket_scope.sql](0004_effect_intent_and_ticket_scope.sql) | Effect intent and Ticket scope |
| [0005_gateway_auth.sql](0005_gateway_auth.sql) | Gateway login and sessions |
| [0006_collab_spaces.sql](0006_collab_spaces.sql) | Collaboration spaces and members |
| [0007_project_space_scope.sql](0007_project_space_scope.sql) | Default space and Project scope |
| [0008_issues.sql](0008_issues.sql) | Issue baseline |
| [0009_issue_extensions.sql](0009_issue_extensions.sql) | Issue statuses, comments, labels and views |
| [0010_issue_collaboration.sql](0010_issue_collaboration.sql) | Collaboration targets, runs, activities and references |
| [0011_issue_interactions.sql](0011_issue_interactions.sql) | Collaboration interactions |
| [0012_issue_interaction_input.sql](0012_issue_interaction_input.sql) | Confirmed interaction input |
| [0013_project_space_optional.sql](0013_project_space_optional.sql) | Nullable space scope (superseded by 0017) |
| [0014_clone_coordination.sql](0014_clone_coordination.sql) | Clone requests, executions and receipts |
| [0015_plugins.sql](0015_plugins.sql) | Plugin catalog, selection and instances |
| [0016_workspace_runtime_follows_node.sql](0016_workspace_runtime_follows_node.sql) | Workspace-local data and Node clone |
| [0017_tenant_membership_and_join.sql](0017_tenant_membership_and_join.sql) | One space per tenant and join flows |
| [0018_runtime_creators.sql](0018_runtime_creators.sql) | Runtime creators |
| [0019_runtime_control.sql](0019_runtime_control.sql) | Control sessions and epochs |
| [0020_force_stop_and_restart.sql](0020_force_stop_and_restart.sql) | Force-stop and restart |
| [0021_node_execution_identity.sql](0021_node_execution_identity.sql) | Node execution identity |
| [0022_plugin_maintenance_pending.sql](0022_plugin_maintenance_pending.sql) | Pending plugin maintenance intent |
| [0023_repository_credential_continuity.sql](0023_repository_credential_continuity.sql) | Repository credential continuity |
| [0024_agent_run_control_plane.sql](0024_agent_run_control_plane.sql) | Agent control plane |
| [0025_agent_control_integrity.sql](0025_agent_control_integrity.sql) | Command ordinals and session constraints |
| [0026_run_runtime_control.sql](0026_run_runtime_control.sql) | Independent run maintenance binding |
| [0027_verified_revisions.sql](0027_verified_revisions.sql) | Revision registration and verification |
| [0027_workflows.sql](0027_workflows.sql) | Workflow graphs |
| [0028_workflow_snapshots.sql](0028_workflow_snapshots.sql) | Published snapshots |
| [0029_workflow_runs.sql](0029_workflow_runs.sql) | Workflow run records |
| [0030_agent_run_business_lifecycle.sql](0030_agent_run_business_lifecycle.sql) | Agent business lifecycle |
| [0031_agent_run_thread_entries.sql](0031_agent_run_thread_entries.sql) | Thread entries |
| [0032_user_git_identities.sql](0032_user_git_identities.sql) | User Git identities |
| [0033_personal_model_connections.sql](0033_personal_model_connections.sql) | Personal model connections and bindings |
| [0033_revision_resume.sql](0033_revision_resume.sql) | Revision resume |
| [0034_agent_startup_windows.sql](0034_agent_startup_windows.sql) | Startup windows and residuals |
| [0035_model_attempt_statistics.sql](0035_model_attempt_statistics.sql) | Model request observations |
| [0036_workflow_asset_imports.sql](0036_workflow_asset_imports.sql) | Workflow asset imports |
| [0037_repository_bindings_and_operations.sql](0037_repository_bindings_and_operations.sql) | Repository Bindings, Operations and snapshots |
| [0038_agent_workspace_blocked_deadline.sql](0038_agent_workspace_blocked_deadline.sql) | Workspace blocked deadlines |
| [0039_workflow_run_identity.sql](0039_workflow_run_identity.sql) | WorkflowRun identity |
| [0040_executor_slot_bindings.sql](0040_executor_slot_bindings.sql) | Executor slots and bindings |
| [0041_workflow_execution_node.sql](0041_workflow_execution_node.sql) | Execution workflow-node ownership |
| [0042_workflow_run_plugin_version.sql](0042_workflow_run_plugin_version.sql) | Frozen run plugin version |
| [0043_external_git_line.sql](0043_external_git_line.sql) | Independent credentials and external Git line |
| [0044_model_attempt_workflow_run.sql](0044_model_attempt_workflow_run.sql) | WorkflowRun model request registration |
| [0045_agent_configuration_catalog.sql](0045_agent_configuration_catalog.sql) | Agent declaration association, static copy and validation issues in the plugin catalog |
| [0046_task_agent_conversations.sql](0046_task_agent_conversations.sql) | Task-level Agent conversations, message queue and Thread visible text |
| [0047_thread_preview_positions.sql](0047_thread_preview_positions.sql) | Node history positions for settled Thread entries |
| [0048_task_conversation_manual_pause.sql](0048_task_conversation_manual_pause.sql) | Manual pause for task conversations |
| [0049_thread_commands_workflow_run.sql](0049_thread_commands_workflow_run.sql) | WorkflowRun session-command ownership and node scope |
| [0050_personal_agent_configurations.sql](0050_personal_agent_configurations.sql) | Persistent personal Agent configuration revisions and private run snapshots |
| [0051_operation_last_claimed_at.sql](0051_operation_last_claimed_at.sql) | Claim rotation (last claim time and partial index) |

0050 copies no personal connection or key and backfills no historical run. Clearing configuration retains its revision; run options are stored separately from shared run input.

## Immutability and upgrades

- Never edit or rename applied SQL; corrections use new forward migrations. Repeated application skips applied files with matching checksums.
- `CheckSchema` only reads and rejects missing migrations, checksum mismatches and unknown database versions. Production services do not run DDL; operators migrate through [cloudctl](../../../cmd/cloudctl/README.en.md).
- Files describe their application-time changes, not the final schema alone. For example, 0017 supersedes 0013's nullable space scope; 0016 retains old storage/worktree records while retiring their steps.
- 0017 does not silently split multiple spaces or rename duplicate slugs; data violating one-space-per-tenant constraints needs prior handling.
- 0042 does not invent plugin versions for historical runs; code rejects execution without a frozen version. 0043 revokes old credential references and retains audit without treating old secret references as redeemable.
- 0045 adds `agent_configuration` (association and byte-exact static copy) and `agent_configuration_issue` (bounded reason) without backfilling existing rows. Only validated declarations are consumable; absence and invalid declarations are distinct, and invalid declarations do not block package installation. See [Plugin marketplace](../../../docs/plugins/marketplace.en.md) and `TestMigration0045AgentConfigurationCatalogAppliesFreshAndUpgrades` for fresh/upgrade/repeat coverage.
- 0046–0048 backfill no old messages, Threads, history positions or pause state; `visible_text` is stored separately from the raw `record`, and old tasks are not paused automatically. Fresh, upgrade and repeated application are covered by `TestMigration0046TaskConversationFreshAndUpgrade` and the integration migration tests.
- 0049 lets `thread_commands` accept a WorkflowRun owner and requires a graph node on workflow commands; each node holds at most one `end_session`, so the scheduler can replay it safely. Every old row carries `run_id` and is not backfilled. Semantics are in [workflows](../../../docs/workflows/workflows.en.md); coverage is in the `TestWorkflowSessionEndsAfterSuccessfulTurn` DB tests.
- 0051 adds `operations.last_claimed_at` and the `operations_claim_rotation` partial index, ordering candidates by `COALESCE(last_claimed_at, created_at), created_at, id`. It is scheduling bookkeeping, not progress, a failure count, a lease or a timeout origin. Historical rows stay NULL without backfill or rewritten operation history, and only an actual hand-over to a Controller records database time. Semantics are in [workspaces](../../../docs/runtime/workspaces.en.md); fresh databases, 0050 upgrades and repeated application are covered by `TestMigration0051ClaimRotationAppliesFreshAndUpgrades`.
- New migrations must verify fresh databases, upgrades from the preceding schema, repeated application and key constraints. See [migration_upgrade_path_test.go](../../../integration/migration_upgrade_path_test.go) and domain upgrade tests in integration.

See [Persistence](../../../docs/core/persistence.en.md) for ownership/transactions, and [Workflows](../../../docs/workflows/workflows.en.md) / [External Git line](../../../docs/repositories/external-git.en.md) for runtime semantics.
