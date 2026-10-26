# 数据库迁移目录

[中文](README.md) | [English](README.en.md)

SQL 通过 `core/store.go` 的 `embed.FS` 编入二进制。`Store.Migrate` 在事务级咨询锁内按完整文件名的字典序执行未应用文件；`schema_migrations` 保存完整文件名、SHA-256 和应用时间。数字前缀不是唯一身份：两个 `0027` 和两个 `0033` 均独立应用。

## 文件索引

当前共 73 个 SQL 文件。

| SQL 文件 | schema 变化 |
|---|---|
| [0001_core.sql](0001_core.sql) | 基础领域表 |
| [0002_aggregate_guards.sql](0002_aggregate_guards.sql) | 聚合归属约束 |
| [0003_resource_versions.sql](0003_resource_versions.sql) | 资源版本 |
| [0004_effect_intent_and_ticket_scope.sql](0004_effect_intent_and_ticket_scope.sql) | Effect 意图和 Ticket 作用域 |
| [0005_gateway_auth.sql](0005_gateway_auth.sql) | Gateway 登录与会话 |
| [0006_collab_spaces.sql](0006_collab_spaces.sql) | 协作空间与成员 |
| [0007_project_space_scope.sql](0007_project_space_scope.sql) | 默认空间与 Project 归属 |
| [0008_issues.sql](0008_issues.sql) | Issue 基线 |
| [0009_issue_extensions.sql](0009_issue_extensions.sql) | Issue 状态、评论、标签与视图 |
| [0010_issue_collaboration.sql](0010_issue_collaboration.sql) | 协作目标、运行、活动与引用 |
| [0011_issue_interactions.sql](0011_issue_interactions.sql) | 协作交互 |
| [0012_issue_interaction_input.sql](0012_issue_interaction_input.sql) | 确认的交互输入 |
| [0013_project_space_optional.sql](0013_project_space_optional.sql) | 可空空间关联（0017 取代） |
| [0014_clone_coordination.sql](0014_clone_coordination.sql) | clone 请求、执行和收据 |
| [0015_plugins.sql](0015_plugins.sql) | 插件目录、选择与实例 |
| [0016_workspace_runtime_follows_node.sql](0016_workspace_runtime_follows_node.sql) | Workspace 独立数据与 Node clone |
| [0017_tenant_membership_and_join.sql](0017_tenant_membership_and_join.sql) | 一租户一空间与加入流程 |
| [0018_runtime_creators.sql](0018_runtime_creators.sql) | 运行时创建者 |
| [0019_runtime_control.sql](0019_runtime_control.sql) | 控制会话与代次 |
| [0020_force_stop_and_restart.sql](0020_force_stop_and_restart.sql) | 强停与重启 |
| [0021_node_execution_identity.sql](0021_node_execution_identity.sql) | Node 执行身份 |
| [0022_plugin_maintenance_pending.sql](0022_plugin_maintenance_pending.sql) | 插件维护 pending 意图 |
| [0023_repository_credential_continuity.sql](0023_repository_credential_continuity.sql) | 仓库凭据连续性 |
| [0024_agent_run_control_plane.sql](0024_agent_run_control_plane.sql) | Agent 控制面 |
| [0025_agent_control_integrity.sql](0025_agent_control_integrity.sql) | 命令序号与会话约束 |
| [0026_run_runtime_control.sql](0026_run_runtime_control.sql) | 独立运行 maintenance binding |
| [0027_verified_revisions.sql](0027_verified_revisions.sql) | Revision 登记与核验 |
| [0027_workflows.sql](0027_workflows.sql) | 工作流图 |
| [0028_workflow_snapshots.sql](0028_workflow_snapshots.sql) | 发布快照 |
| [0029_workflow_runs.sql](0029_workflow_runs.sql) | 工作流运行记录 |
| [0030_agent_run_business_lifecycle.sql](0030_agent_run_business_lifecycle.sql) | Agent 业务生命周期 |
| [0031_agent_run_thread_entries.sql](0031_agent_run_thread_entries.sql) | Thread 条目 |
| [0032_user_git_identities.sql](0032_user_git_identities.sql) | 用户 Git 身份 |
| [0033_personal_model_connections.sql](0033_personal_model_connections.sql) | 个人模型连接与绑定 |
| [0033_revision_resume.sql](0033_revision_resume.sql) | Revision 续接 |
| [0034_agent_startup_windows.sql](0034_agent_startup_windows.sql) | 启动窗口与残留 |
| [0035_model_attempt_statistics.sql](0035_model_attempt_statistics.sql) | 模型请求观察 |
| [0036_workflow_asset_imports.sql](0036_workflow_asset_imports.sql) | 工作流资产导入 |
| [0037_repository_bindings_and_operations.sql](0037_repository_bindings_and_operations.sql) | 仓库 Binding、Operation 和快照 |
| [0038_agent_workspace_blocked_deadline.sql](0038_agent_workspace_blocked_deadline.sql) | Workspace blocked 截止 |
| [0039_workflow_run_identity.sql](0039_workflow_run_identity.sql) | WorkflowRun 身份 |
| [0040_executor_slot_bindings.sql](0040_executor_slot_bindings.sql) | 执行器槽位与绑定 |
| [0041_workflow_execution_node.sql](0041_workflow_execution_node.sql) | 执行的工作流节点归属 |
| [0042_workflow_run_plugin_version.sql](0042_workflow_run_plugin_version.sql) | 冻结运行插件版本 |
| [0043_external_git_line.sql](0043_external_git_line.sql) | 独立凭据与外部 Git 线 |
| [0044_model_attempt_workflow_run.sql](0044_model_attempt_workflow_run.sql) | WorkflowRun 模型请求登记 |
| [0045_agent_configuration_catalog.sql](0045_agent_configuration_catalog.sql) | 插件目录中的 Agent 声明关联、静态副本与校验问题 |
| [0046_task_agent_conversations.sql](0046_task_agent_conversations.sql) | 任务级 Agent 对话、消息队列与 Thread 可见正文 |
| [0047_thread_preview_positions.sql](0047_thread_preview_positions.sql) | 定型 Thread 条目的 Node 历史位置 |
| [0048_task_conversation_manual_pause.sql](0048_task_conversation_manual_pause.sql) | 任务对话的手动暂停 |
| [0049_thread_commands_workflow_run.sql](0049_thread_commands_workflow_run.sql) | WorkflowRun 的会话命令归属与节点范围 |
| [0050_personal_agent_configurations.sql](0050_personal_agent_configurations.sql) | 个人 Agent 配置的持续 revision 与私有运行快照 |
| [0051_workflow_run_cancel.sql](0051_workflow_run_cancel.sql) | WorkflowRun 的取消请求 |
| [0052_legacy_workflow_issue_run_settlement.sql](0052_legacy_workflow_issue_run_settlement.sql) | 存量 queued 工作流 IssueRun 的一次性结算 |
| [0053_retire_tenant_level_clones.sql](0053_retire_tenant_level_clones.sql) | 退役租户级 clone |
| [0054_git_delivery_refs.sql](0054_git_delivery_refs.sql) | 经 Git 交付的 Revision |
| [0055_workflow_run_workspace_release.sql](0055_workflow_run_workspace_release.sql) | WorkflowRun 的 Workspace 释放事实 |
| [0056_workflow_invocation_reference.sql](0056_workflow_invocation_reference.sql) | workflow IssueRun 到 WorkflowRun 的调用引用 |
| [0057_agent_run_residual_reclamation.sql](0057_agent_run_residual_reclamation.sql) | 残留回收责任、追加证据和处置状态 |
| [0058_agent_run_controlled_reclamation.sql](0058_agent_run_controlled_reclamation.sql) | 部署受控回收意图、从属数据删除与逐目标终态 |
| [0059_agent_restore_stop_evidence.sql](0059_agent_restore_stop_evidence.sql) | 停止未确认的命名证据，不改变残留责任和回收授权 |
| [0060_node_model_adapter.sql](0060_node_model_adapter.sql) | Node 实例登记的插件模型 adapter 能力 |
| [0061_scheduled_tasks.sql](0061_scheduled_tasks.sql) | 定时任务定义、幂等接纳台账与截止取消事实 |
| [0062_workflow_run_plugin_release.sql](0062_workflow_run_plugin_release.sql) | 冻结运行插件 release 包 |
| [0063_scheduled_run_results.sql](0063_scheduled_run_results.sql) | 定时运行的业务 verdict、冻结的任务归属与 occurrence 审计引用 |
| [0064_scheduled_run_artifacts.sql](0064_scheduled_run_artifacts.sql) | 定时产物的交付窗口、已核对登记、被拒清单，以及每运行每类一个未登记工作项 |
| [0065_scheduled_artifact_execution_kind.sql](0065_scheduled_artifact_execution_kind.sql) | 产物执行登记闭集与每运行每类一个未完成执行，保留插件执行类型 |
| [0066_scheduled_artifact_recurring_gc.sql](0066_scheduled_artifact_recurring_gc.sql) | 已结算运行持续重扫，清理迟到 PUT 与被淘汰的成功交付 |
| [0067_scheduled_empty_artifacts.sql](0067_scheduled_empty_artifacts.sql) | 产物大小下界改为零，允许空文件登记 |
| [0068_scheduled_manifest_completeness.sql](0068_scheduled_manifest_completeness.sql) | 保存未列明的拒绝数和遍历截断事实 |
| [0069_workflow_node_attempt.sql](0069_workflow_node_attempt.sql) | 工作流节点工作项、执行与结束请求的尝试序号 |
| [0070_d8_missing_delete_operation.sql](0070_d8_missing_delete_operation.sql) | D8 无原删除操作的显式来源标记与互斥约束 |
| [0071_scheduled_task_model.sql](0071_scheduled_task_model.sql) | 定时任务可选模型选择 |
| [0072_delivery_execution_settlement.sql](0072_delivery_execution_settlement.sql) | 独立交付业务结算、冻结原派发身份、未受理证明及矛盾事实审计 |

0050 不复制个人连接或 Key，不回填历史运行；清除配置保留 revision，运行选项独立于共享运行输入保存。

## 不可变性与升级

0070 给受控回收意图增加默认关闭的 `legacy_delete_operation_missing`。有原删除操作的 D8 和残留账本意图保持原值；仅明确选择无删除操作变体时才可记录缺失，不回填或修改 Run 历史。空库、前一版升级、重复迁移和矛盾来源拒绝由 `TestControlledD8MissingDeleteMigrationUpgradesAndRepeats` 覆盖。

0071 为 `scheduled_tasks` 添加可空的 `model`。升级不为既有定义猜测模型；每次接纳才按创建者当前可用连接解析该选择，并将解析结果冻结到新 IssueRun。空值仍表示该次接纳使用创建者默认模型。升级与重放由 `TestMigration0071ScheduledTaskModelUpgradeAndReplay` 覆盖。

- 已应用 SQL 不改内容、不改文件名；更正用新的前向迁移。重复应用跳过 checksum 相同的已应用文件。
- `CheckSchema` 只读检查缺失迁移、checksum 不符和未知数据库版本，发现不一致拒绝启动。生产服务不执行 DDL，运维用 [cloudctl](../../../cmd/cloudctl/README.md) 应用迁移。
- 文件描述各自应用时的变化，不能单独当成最终 schema。例如 0013 的可空空间归属被 0017 取代；0016 保留旧存储/工作树记录但停用旧步骤。
- 0017 不静默拆分多空间或改写重复 slug，不满足一租户一空间约束的数据须先处理。
- 0042 不为历史运行猜测插件版本，无冻结版本的执行由代码拒绝。0043 撤销旧凭据引用并保留审计，不把旧 secret 引用当成可兑换凭据。
- 0045 增加 `agent_configuration`（关联与逐字节静态副本）和 `agent_configuration_issue`（有界原因），不回填既有行；只保存校验通过的可消费声明，区分未声明与声明无效，后者不阻止包安装。语义见 [插件市场](../../../docs/plugins/marketplace.md)，空库、升级与重复应用覆盖见 `TestMigration0045AgentConfigurationCatalogAppliesFreshAndUpgrades`。
- 0046–0048 不回填旧消息、旧 Thread、历史位置或暂停状态；`visible_text` 与原始 `record` 分开保存，旧任务不自动暂停。空库、升级与重复应用覆盖见 `TestMigration0046TaskConversationFreshAndUpgrade` 及 integration 的迁移测试。
- 0049 让 `thread_commands` 接受 WorkflowRun 归属，并要求工作流命令带图节点；每个节点至多一条 `end_session`，调度器可安全重放。旧行全部带 `run_id`，不回填。语义见 [工作流](../../../docs/workflows/workflows.md)，覆盖见 `TestWorkflowSessionEndsAfterSuccessfulTurn` 等 DB 测试。
- 0051 给 `workflow_runs` 增加取消请求时刻，不回填旧行；请求只记录意图，由调度器在后续轮次结算。语义见[工作流](../../../docs/workflows/workflows.md#调度)，覆盖见 `TestWorkflowRunCancelBeforeStartNeverStarts` 等 DB 测试。
- 0052 结算升级时仍为 `queued`/`dispatched` 的、存活的 `executor_type='workflow'` IssueRun：它们没有执行器、也没有冻结的工作流身份，按 ADR D3 标记失败并写 `failure_reason='workflow_execution_unavailable'`，不做迁移；其他状态的行与软删除行保持原样。工作流启动门禁把 manual 运行绑定到真实 WorkflowRun 之后才会停止产生这种行；门禁（circus#71）落地前 `createRun` 仍会造出新行，而本迁移只在升级时执行一次，不会回头结算它们。覆盖见 `TestMigration0052LegacyWorkflowIssueRunSettlementAppliesFreshAndUpgrades`。
- 0053 是 Workspace 改为从 Git Server 快照 clone 的切换，不迁移数据：删除不属于 Workspace 的 clone execution 及其收据，去掉 `clone_executions.clone_request_id` 与索引 `one_request_execution`，`clone_executions.workspace_id` 改为 NOT NULL，删除 `clone_requests`；Workspace 的 clone execution 与收据保留，可重复执行。语义见 [Workspace](../../../docs/runtime/workspaces.md)，仍有租户级 clone 的库的升级覆盖见 `TestMigration0053RetiresTenantLevelClonesKeepingWorkspaceClones`。
- 0054 是 Revision 改为经 Git 交付的切换，不迁移数据：`revisions` 增加 `delivery_ref`，`revisions_bundle_shape` 换成 `revisions_shape`（`delivery_ref` 为 NULL 时仍是原有的 bundle 形状，非 NULL 时必须是格式正确的 `refs/ora/deliveries/<UUID>` 且不带任何 bundle 列），部分唯一索引 `revisions_delivery_ref` 让一个交付 ref 只属于一个 Revision；既有 bundle 行不改写，可重复执行。语义见 [Revision](../../../docs/agent-runs/revisions.md)，约束覆盖见 `TestRevisionShapeKeepsGitAndBundleRowsApart`。
- 0055 给 `workflow_runs` 增加单向的 `workspace_release_state`（`pending`/`releasing`/`released`/`abandoned`/`not_required`）、`workspace_release_error` 和 `workspace_released_at`，并按升级时的状态回填：非终态行保持 `pending`；终态行没有 Workspace 的记 `not_required`，Workspace 已软删除的记 `released`（释放时刻取该行的 `deleted_at`），其余记 `releasing`。两条 CHECK 保证 `released` 必有释放时刻、`abandoned` 必有原因。只有 `released` 表示 Cloud 观察到 Workspace 已被删除；`abandoned` 保留 Workspace 行、不写释放时刻。语义见[工作流](../../../docs/workflows/workflows.md#调度)，覆盖见 `TestMigration0055WorkflowRunWorkspaceReleaseAppliesFreshAndUpgrades` 与 `internal/core/workflow_run_release_db_test.go`。
- 0056 先把没有引用的历史非终态 workflow IssueRun（含 running、deferred）结算为 failed（`workflow_execution_unavailable_before_invocation_linkage`），保留其输入和结果、不虚构执行；再加 `(workflow_invocation_ref, tenant_id)` → `workflow_runs(id, tenant_id)` 复合外键（引用 0044 已建的 `workflow_run_tenant_identity` 唯一约束，不重复建）、唯一引用，以及「非终态 workflow 行必须有引用、其他执行器不得有引用」的 CHECK，无条件 VALIDATE。历史终态 workflow 行可保留 NULL 引用。空库、带历史数据升级、重复应用与约束验证状态见 `integration/workflow_invocation_migration_test.go`。
- 0057 只给残留账本增列：存量 D4/D5 行通过默认值初始化为 `reconciling`，不回填目标或证据，不改旧 `reason`、`responsibility` 或业务结算，也不回填 D8。历史行若已有非空 `cleared_at`，迁移明确报错并回滚，等待显式兼容处理（A2）。DDL 可重入，不会重置已推进条目。数据库接受 `released`、`terminated_data_retained`、`externally_confirmed` 三种处置；本步自动路径只实现 `released`，其余处置留给后续受控入口。operation 的租户和 Workspace 关联由授权事务比对，UUID 外键本身不证明资源范围。空库、升级、DDL 重放和约束覆盖见 [agent_run_residual_reclaim_migration_test.go](../../../integration/agent_run_residual_reclaim_migration_test.go)。
- 0058 不改 0057 的历史列、责任、证据、处置或首次 `cleared_at`，也不回填 D8；无账本的 D8 只由显式根意图承载保护与证据。根终止与从属删卷各自三态；每资源一个根、最多一个从属删除意图及固定删除 operation，命令键按 tenant 范围统一唯一。目标的成功约束要求两个布尔确认均为 true，缺字段不能以 SQL NULL 通过。新库、0057 升级与约束覆盖见 [agent_run_controlled_reclaim_migration_test.go](../../../integration/agent_run_controlled_reclaim_migration_test.go)。
- 0059 只增 `stop_evidence`，存量为空对象，不追认历史未知停止；覆盖见 `TestMigration0059RestoreStopEvidenceFreshUpgradeAndRepeat`。
- 0060 给 `node_instances` 增加 `model_adapter`（jsonb 对象，默认 `{}`），记录 Node 实例登记时声明的插件模型 adapter 能力；不回填，既有实例按空能力处理，不会被派发需要 adapter 的会话，直到以新版本重新登记。空库与升级覆盖见 `TestMigration0060ModelAdapterFreshAndUpgrade`。
- 0061 增加 `scheduled_tasks`（冻结 Prompt、agent ref、`schedule` jsonb、IANA 时区、无人值守同意与软删除墓碑）和 `scheduled_task_occurrences`（`(task_id, scheduled_at)` 唯一的幂等接纳台账，含截止时刻与截止取消事实）；不回填历史行。接纳由工作线程在同一事务里创建绑定 issue 下的 agent IssueRun，插件版本在接纳时按 Space 当时值快照而非定义上冻结。语义见 specs 根决策 `decisions/cloud/scheduled-task`；空库、升级与重复应用的集成覆盖随 Web 与端到端验收 commit 一并补齐。
- 0062 给 `workflow_runs` 增加可空 `plugin_release`（jsonb），记录创建事务冻结的 Agent 插件 release 包；仅当 Space 的 `selected_release` 版本等于冻结的 `plugin_version` 时写入，其余为 NULL，不回填。运行 Workspace 的 `plugin` 步骤用它替换 Space 当前选择里的 Agent 插件条目；Space 已移除该插件时不追加，交由启动判定以 `agent_plugin_unavailable` 失败。语义见[工作流](../../../docs/workflows/workflows.md#调度)，覆盖见 `internal/core/workflow_run_plugin_db_test.go`。
- 0063 给 `scheduled_task_occurrences` 补一个代理 `id`（默认值，既有接纳事务不用改），并增加 `scheduled_run_results`：以 `issue_run_id` 为唯一身份，`verdict` 取自 Close 集、`verdict_detail` 至多 2000 字符，`task_id` 与 `scheduled_at` 是写入时冻结的任务归属与计划时间事实，`occurrence_id` 只作审计并在裁剪 occurrence 时置空。不回填任何历史运行。语义见 specs `decisions/cloud/scheduled-task/20261022-run-verdict-and-artifacts.md`；覆盖见 `TestSessionEndedWritesTheScheduledVerdict`、`TestScheduledRunResultIsWrittenOncePerRun`、`TestScheduledRunResultRefusesUnattributableReports` 与 `TestScheduledRunResultSurvivesOccurrenceTrim`。
- 0064 增加定时产物的三张表：交付状态与不可续期 10 分钟窗口（含前缀清理标记）、已核对对象索引、被拒清单；三者都冻结 `task_id`/`scheduled_at` 并把 `occurrence_id` 只作审计。同时把 `execution_work` 的未登记唯一性从"每运行一个"改为"每运行每类一个"，并在 kind 闭集里加入 `deliver_scheduled_artifacts`——定时运行要同时持 Revision 交付项和产物上传项。覆盖见 `TestScheduledArtifactsSkipWithoutObjectStorage`、`TestScheduledArtifactManifestIsValidatedBeforeStorage`、`TestCollectScheduledArtifactObjectsRemovesOnlyUnregisteredKeys` 与 `TestPruneScheduledRunArtifactsKeepsTheTenNewestRuns`。
- 0069 给 `execution_work`、`node_executions` 和 `thread_commands` 增加 `workflow_node_attempt`（整数，NOT NULL，默认 1），并把 `one_session_per_workflow_node`、`one_workflow_session_end` 两个部分唯一索引按尝试重键，使 agent 节点重试时可以登记第二次会话及其自己的结束请求，而不复用第一次尝试的证据。既有行都属于各自节点的第一次尝试，不回填。空库、0068 升级、重复应用与约束覆盖见 `TestMigration0069WorkflowNodeAttemptAppliesFreshAndUpgrades`。
- 新增迁移须验证空库、前一 schema 升级、重复应用与关键约束。覆盖见 [migration_upgrade_path_test.go](../../../integration/migration_upgrade_path_test.go) 和 integration 的领域升级测试。

数据归属与事务规则见 [持久化](../../../docs/core/persistence.md)，运行语义见 [工作流](../../../docs/workflows/workflows.md) 和 [外部 Git 线](../../../docs/repositories/external-git.md)。
