# Database migration directory

[中文](README.md) | [English](README.en.md)

SQL is embedded in the binary through `embed.FS` in `core/store.go`. `Store.Migrate` executes unapplied files in lexicographic order of complete filenames under a transaction advisory lock. `schema_migrations` stores complete filenames, SHA-256 and application times. Numeric prefixes are not unique identities: both `0027` files and both `0033` files apply independently.

## File index

There are currently 76 SQL files.

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
| [0051_workflow_run_cancel.sql](0051_workflow_run_cancel.sql) | WorkflowRun cancellation requests |
| [0052_legacy_workflow_issue_run_settlement.sql](0052_legacy_workflow_issue_run_settlement.sql) | One-time settlement of legacy queued workflow IssueRuns |
| [0053_retire_tenant_level_clones.sql](0053_retire_tenant_level_clones.sql) | Retire tenant-level clones |
| [0054_git_delivery_refs.sql](0054_git_delivery_refs.sql) | Revisions delivered over Git |
| [0055_workflow_run_workspace_release.sql](0055_workflow_run_workspace_release.sql) | Workspace release facts for WorkflowRuns |
| [0056_workflow_invocation_reference.sql](0056_workflow_invocation_reference.sql) | Invocation reference from workflow IssueRuns to WorkflowRuns |
| [0057_agent_run_residual_reclamation.sql](0057_agent_run_residual_reclamation.sql) | Residual reclamation responsibility, appended evidence and disposition |
| [0058_agent_run_controlled_reclamation.sql](0058_agent_run_controlled_reclamation.sql) | Deployment-controlled reclamation intents, dependent data deletion and per-target termination |
| [0059_agent_restore_stop_evidence.sql](0059_agent_restore_stop_evidence.sql) | Named unconfirmed-stop evidence without changing residual responsibility or reclaim authority |
| [0060_node_model_adapter.sql](0060_node_model_adapter.sql) | Plugin model-adapter capability declared by a Node instance at registration |
| [0061_scheduled_tasks.sql](0061_scheduled_tasks.sql) | Scheduled task definitions, idempotent admission ledger and deadline-cancellation facts |
| [0062_workflow_run_plugin_release.sql](0062_workflow_run_plugin_release.sql) | Frozen run plugin release package |
| [0063_scheduled_run_results.sql](0063_scheduled_run_results.sql) | Scheduled-run business verdict with its frozen task identity and occurrence audit reference |
| [0064_scheduled_run_artifacts.sql](0064_scheduled_run_artifacts.sql) | Artifact delivery windows, verified objects, rejections and one unregistered work item per run and kind |
| [0065_scheduled_artifact_execution_kind.sql](0065_scheduled_artifact_execution_kind.sql) | Artifact execution registration and one pending execution per run and kind, preserving plugin kinds |
| [0066_scheduled_artifact_recurring_gc.sql](0066_scheduled_artifact_recurring_gc.sql) | Recurring sweeps of settled runs for late PUTs and pruned successful deliveries |
| [0067_scheduled_empty_artifacts.sql](0067_scheduled_empty_artifacts.sql) | Accept zero-length artifacts in verified registration |
| [0068_scheduled_manifest_completeness.sql](0068_scheduled_manifest_completeness.sql) | Persist omitted rejection counts and walk truncation facts |
| [0069_workflow_node_attempt.sql](0069_workflow_node_attempt.sql) | Per-attempt identity for workflow node work items, executions and end requests |
| [0071_workflow_run_delivery.sql](0071_workflow_run_delivery.sql) | Whole-workflow Revision ownership, history type, save barrier and Node capability |
| [0072_workflow_dispatch_order.sql](0072_workflow_dispatch_order.sql) | Durable workflow session dispatch order independent of wall-clock timestamps |
| [0070_d8_missing_delete_operation.sql](0070_d8_missing_delete_operation.sql) | Explicit D8 missing-delete provenance and mutually exclusive identity constraints |
| [0071_scheduled_task_model.sql](0071_scheduled_task_model.sql) | Optional selected model on scheduled tasks |
| [0072_scheduled_task_conversation_reasons.sql](0072_scheduled_task_conversation_reasons.sql) | Task-conversation rejection reasons for scheduled admission |
| [0073_member_join_provenance.sql](0073_member_join_provenance.sql) | Link notes and member admission provenance without invented history |
| [0074_node_unavailable_attempts.sql](0074_node_unavailable_attempts.sql) | Non-negative Node-unavailable attempt count on operations, defaulting to 0 |

0050 copies no personal connection or key and backfills no historical run. Clearing configuration retains its revision; run options are stored separately from shared run input.

## Immutability and upgrades

0070 adds the default-off `legacy_delete_operation_missing` marker to controlled reclamation intents. Existing D8 intents with original deletion identities and residual-ledger intents retain their values. Only an explicitly selected missing-delete variant records the absence; no Run history is backfilled or changed. Fresh schemas, preceding-schema upgrades, repeated migration, and rejection of contradictory provenance are covered by `TestControlledD8MissingDeleteMigrationUpgradesAndRepeats`.

0071 adds a nullable `model` to `scheduled_tasks`. The upgrade never guesses a model for existing definitions; each admission resolves the selected value through the creator's then-usable connection and freezes that resolution into the new IssueRun. A null value continues to mean the creator's default model at admission. Upgrade and replay coverage is `TestMigration0071ScheduledTaskModelUpgradeAndReplay`.

0072 only widens the `scheduled_task_occurrences.failure_reason` CHECK with `task_paused` and `task_handoff_pending`: when the bound issue's task conversation is paused, holds an unresolved failed message, or has a waiting message, admission records one of them and creates no run. Existing rows are not rewritten. Upgrade and replay coverage is `TestMigration0072ScheduledTaskConversationReasonsUpgradeAndReplay`.

0074 adds `node_unavailable_attempts` to `operations` (bigint, NOT NULL, default 0), with `CHECK (node_unavailable_attempts >= 0)` rejecting negative values. Existing operations start at 0; historical attempts are not backfilled, and existing executions retain their Node ownership and results. `ADD COLUMN IF NOT EXISTS` makes the SQL re-applicable. Upgrade, repeated application, legacy execution preservation and negative-value rejection are covered by `TestOperationWaitMigrationPreservesLegacyExecutionAndDefaults` in [operation_wait_migration_test.go](../operation_wait_migration_test.go).

- Never edit or rename applied SQL; corrections use new forward migrations. Repeated application skips applied files with matching checksums.
- `CheckSchema` only reads and rejects missing migrations, checksum mismatches and unknown database versions. Production services do not run DDL; operators migrate through [cloudctl](../../../cmd/cloudctl/README.en.md).
- Files describe their application-time changes, not the final schema alone. For example, 0017 supersedes 0013's nullable space scope; 0016 retains old storage/worktree records while retiring their steps.
- 0017 does not silently split multiple spaces or rename duplicate slugs; data violating one-space-per-tenant constraints needs prior handling.
- 0042 does not invent plugin versions for historical runs; code rejects execution without a frozen version. 0043 revokes old credential references and retains audit without treating old secret references as redeemable.
- 0045 adds `agent_configuration` (association and byte-exact static copy) and `agent_configuration_issue` (bounded reason) without backfilling existing rows. Only validated declarations are consumable; absence and invalid declarations are distinct, and invalid declarations do not block package installation. See [Plugin marketplace](../../../docs/plugins/marketplace.en.md) and `TestMigration0045AgentConfigurationCatalogAppliesFreshAndUpgrades` for fresh/upgrade/repeat coverage.
- 0046–0048 backfill no old messages, Threads, history positions or pause state; `visible_text` is stored separately from the raw `record`, and old tasks are not paused automatically. Fresh, upgrade and repeated application are covered by `TestMigration0046TaskConversationFreshAndUpgrade` and the integration migration tests.
- 0049 lets `thread_commands` accept a WorkflowRun owner and requires a graph node on workflow commands; each node holds at most one `end_session`, so the scheduler can replay it safely. Every old row carries `run_id` and is not backfilled. Semantics are in [workflows](../../../docs/workflows/workflows.en.md); coverage is in the `TestWorkflowSessionEndsAfterSuccessfulTurn` DB tests.
- 0051 adds the cancellation-request instant to `workflow_runs` and backfills no old row; the request records intent only and is settled by the scheduler on later passes. Semantics are in [workflows](../../../docs/workflows/workflows.en.md#scheduling); coverage is in the `TestWorkflowRunCancelBeforeStartNeverStarts` DB tests.
- 0052 settles the live `executor_type='workflow'` IssueRuns still `queued`/`dispatched` at upgrade time: they have no executor and no frozen workflow identity, so the ADR's D3 marks them failed with `failure_reason='workflow_execution_unavailable'` instead of migrating them. Other runs and soft-deleted rows are untouched. Rows of that shape stop being produced only once the workflow launch gate binds a manual run to a real WorkflowRun; until that gate (circus#71) lands, `createRun` keeps producing new ones, and this migration runs once at upgrade time and does not settle them. Coverage is `TestMigration0052LegacyWorkflowIssueRunSettlementAppliesFreshAndUpgrades`.
- 0053 is the switch to Workspaces cloned from Git Server snapshots and migrates no data: clone executions that belong to no Workspace and their receipts are deleted, `clone_executions.clone_request_id` and the `one_request_execution` index are dropped, `clone_executions.workspace_id` becomes NOT NULL, and `clone_requests` is dropped; Workspace clone executions and their receipts stay, and it is retry-safe. See [Workspaces](../../../docs/runtime/workspaces.en.md); upgrading a database that still holds tenant-level clones is covered by `TestMigration0053RetiresTenantLevelClonesKeepingWorkspaceClones`.
- 0054 is the switch to Revisions delivered over Git and migrates no data: `revisions` gains `delivery_ref`, and `revisions_bundle_shape` is replaced by `revisions_shape` (with a NULL `delivery_ref` a row keeps a legacy bundle shape; with one it must be a well-formed `refs/ora/deliveries/<UUID>` and carry no bundle column); the partial unique index `revisions_delivery_ref` lets one delivery ref belong to one Revision only. Existing bundle rows are never rewritten, and it is retry-safe. See [Revisions](../../../docs/agent-runs/revisions.en.md); the constraints are covered by `TestRevisionShapeKeepsGitAndBundleRowsApart`.
- 0055 adds the forward-only `workspace_release_state` (`pending`/`releasing`/`released`/`abandoned`/`not_required`), `workspace_release_error` and `workspace_released_at` to `workflow_runs`, and backfills by upgrade-time state: non-terminal rows stay `pending`; terminal rows with no Workspace become `not_required`, those whose Workspace is already soft-deleted become `released` (using that row's `deleted_at`), and the rest become `releasing`. Two CHECK constraints require a release instant for `released` and a reason for `abandoned`. Only `released` means Cloud observed the Workspace deleted; `abandoned` keeps the Workspace row and writes no release instant. Semantics are in [workflows](../../../docs/workflows/workflows.en.md#scheduling); coverage is `TestMigration0055WorkflowRunWorkspaceReleaseAppliesFreshAndUpgrades` and `internal/core/workflow_run_release_db_test.go`.
- 0056 first settles every unlinked non-terminal historical workflow IssueRun (including running and deferred) as failed with `workflow_execution_unavailable_before_invocation_linkage`, preserving its input and outcome without inventing an execution. It then adds the `(workflow_invocation_ref, tenant_id)` → `workflow_runs(id, tenant_id)` composite foreign key (targeting the `workflow_run_tenant_identity` unique constraint 0044 already added, without duplicating it), a unique reference, and a CHECK that non-terminal workflow rows carry a reference while other executors carry none, all unconditionally validated. Historical terminal workflow rows may keep a NULL reference. Fresh, seeded upgrade, repeated application and constraint validation coverage is in `integration/workflow_invocation_migration_test.go`.
- 0057 only adds residual-ledger columns: defaults initialize existing D4/D5 rows as `reconciling`, without backfilling targets or evidence, rewriting old `reason`, `responsibility` or business settlement, or backfilling D8. Legacy rows with non-null `cleared_at` cause an explicit migration error and rollback pending compatibility handling (A2). Reentrant DDL preserves progressed rows. The schema accepts `released`, `terminated_data_retained` and `externally_confirmed`; this step's automatic path implements only `released`, leaving the other dispositions to later controlled entry points. The authorization transaction checks the operation's tenant and Workspace relationships; a UUID foreign key alone does not establish resource scope. See [agent_run_residual_reclaim_migration_test.go](../../../integration/agent_run_residual_reclaim_migration_test.go) for fresh, upgrade, DDL replay and constraint coverage.
- 0058 preserves 0057 history, responsibility, evidence, disposition and first `cleared_at`, and never backfills D8. Only an explicit root intent owns protection and evidence for ledgerless D8. Root termination and dependent data deletion each have three states; each resource has one root, at most one deletion intent and one fixed deletion operation. Command keys are unique throughout a tenant. Successful targets require both boolean confirmations, so a missing JSON field cannot pass through SQL NULL. Fresh, 0057-upgrade and constraint coverage is in [agent_run_controlled_reclaim_migration_test.go](../../../integration/agent_run_controlled_reclaim_migration_test.go).
- 0059 only adds `stop_evidence`, initially empty for existing rows; no historical stop observation is invented. Covered by `TestMigration0059RestoreStopEvidenceFreshUpgradeAndRepeat`.
- 0060 adds `model_adapter` to `node_instances` (a jsonb object, default `{}`) recording the plugin model-adapter capability a Node instance declares at registration. Nothing is backfilled: existing instances count as having no capability and receive no session that needs an adapter until they register again with a newer version. Fresh and upgrade coverage is in `TestMigration0060ModelAdapterFreshAndUpgrade`.
- 0061 adds `scheduled_tasks` (frozen prompt, agent ref, `schedule` jsonb, IANA timezone, unattended consent and soft-delete tombstone) and `scheduled_task_occurrences` (an idempotent admission ledger keyed by `(task_id, scheduled_at)`, holding the deadline instant and deadline-cancellation fact); it backfills no historical row. Admission creates an agent IssueRun under the bound issue in the same transaction, and the plugin version is snapshotted from the Space at admission rather than frozen on the definition. Semantics are in the specs root decision `decisions/cloud/scheduled-task`; fresh/upgrade/repeat integration coverage lands with the Web and end-to-end acceptance commit.
- 0062 adds a nullable `plugin_release` (jsonb) to `workflow_runs` recording the Agent plugin release package the creation transaction froze; it is written only when the Space's `selected_release` version equals the frozen `plugin_version`, and stays NULL otherwise. The run Workspace's `plugin` step uses it in place of the Space's current selection for that plugin; a plugin the Space already removed is not appended, and the start gate fails the run as `agent_plugin_unavailable`. Semantics are in [workflows](../../../docs/workflows/workflows.en.md#scheduling); coverage is `internal/core/workflow_run_plugin_db_test.go`.
- 0063 gives `scheduled_task_occurrences` a surrogate `id` (with a default, so the existing admission transaction needs no change) and adds `scheduled_run_results`: `issue_run_id` is the identity, `verdict` comes from the closed set, `verdict_detail` is at most 2000 characters, `task_id` and `scheduled_at` are the task ownership and planned instant frozen at write time, and `occurrence_id` is an audit reference only, nulled when the occurrence is trimmed. No historical run is backfilled. Semantics are in the specs decision `decisions/cloud/scheduled-task/20261022-run-verdict-and-artifacts.md`; coverage is in `TestSessionEndedWritesTheScheduledVerdict`, `TestScheduledRunResultIsWrittenOncePerRun`, `TestScheduledRunResultRefusesUnattributableReports` and `TestScheduledRunResultSurvivesOccurrenceTrim`.
- 0064 adds the three scheduled-artifact tables: the delivery state with its non-renewable ten-minute window (and its prefix-cleanup marker), the verified object index and the rejection list; all three freeze `task_id`/`scheduled_at` and keep `occurrence_id` as an audit reference only. It also narrows `execution_work`'s unregistered uniqueness from "one per run" to "one per run and kind" and adds `deliver_scheduled_artifacts` to the kind set: a scheduled run holds its Revision delivery and its artifact upload at the same time. Coverage is in `TestScheduledArtifactsSkipWithoutObjectStorage`, `TestScheduledArtifactManifestIsValidatedBeforeStorage`, `TestCollectScheduledArtifactObjectsRemovesOnlyUnregisteredKeys` and `TestPruneScheduledRunArtifactsKeepsTheTenNewestRuns`.
- 0069 adds `workflow_node_attempt` to `execution_work`, `node_executions` and `thread_commands` (integer, NOT NULL, default 1) and re-keys the `one_session_per_workflow_node` and `one_workflow_session_end` partial unique indexes on the attempt, so retrying an agent node can register a second session and its own end request without reusing the first attempt's evidence. Every existing row belongs to its node's first attempt, so nothing is backfilled. Fresh, 0068-upgrade, repeated application and constraint coverage is in `TestMigration0069WorkflowNodeAttemptAppliesFreshAndUpgrades`.
- New migrations must verify fresh databases, upgrades from the preceding schema, repeated application and key constraints. See [migration_upgrade_path_test.go](../../../integration/migration_upgrade_path_test.go) and domain upgrade tests in integration.

See [Persistence](../../../docs/core/persistence.en.md) for ownership/transactions, and [Workflows](../../../docs/workflows/workflows.en.md) / [External Git line](../../../docs/repositories/external-git.en.md) for runtime semantics.
