# 数据库迁移目录

[中文](README.md) | [English](README.en.md)

SQL 通过 `core/store.go` 的 `embed.FS` 编入二进制。`Store.Migrate` 在事务级咨询锁内按完整文件名的字典序执行未应用文件；`schema_migrations` 保存完整文件名、SHA-256 和应用时间。数字前缀不是唯一身份：两个 `0027` 和两个 `0033` 均独立应用。

## 文件索引

当前共 53 个 SQL 文件。

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
| [0051_operation_last_claimed_at.sql](0051_operation_last_claimed_at.sql) | 操作领取轮转（最近一次领取时间与部分索引） |

0050 不复制个人连接或 Key，不回填历史运行；清除配置保留 revision，运行选项独立于共享运行输入保存。

## 不可变性与升级

- 已应用 SQL 不改内容、不改文件名；更正用新的前向迁移。重复应用跳过 checksum 相同的已应用文件。
- `CheckSchema` 只读检查缺失迁移、checksum 不符和未知数据库版本，发现不一致拒绝启动。生产服务不执行 DDL，运维用 [cloudctl](../../../cmd/cloudctl/README.md) 应用迁移。
- 文件描述各自应用时的变化，不能单独当成最终 schema。例如 0013 的可空空间归属被 0017 取代；0016 保留旧存储/工作树记录但停用旧步骤。
- 0017 不静默拆分多空间或改写重复 slug，不满足一租户一空间约束的数据须先处理。
- 0042 不为历史运行猜测插件版本，无冻结版本的执行由代码拒绝。0043 撤销旧凭据引用并保留审计，不把旧 secret 引用当成可兑换凭据。
- 0045 增加 `agent_configuration`（关联与逐字节静态副本）和 `agent_configuration_issue`（有界原因），不回填既有行；只保存校验通过的可消费声明，区分未声明与声明无效，后者不阻止包安装。语义见 [插件市场](../../../docs/plugins/marketplace.md)，空库、升级与重复应用覆盖见 `TestMigration0045AgentConfigurationCatalogAppliesFreshAndUpgrades`。
- 0046–0048 不回填旧消息、旧 Thread、历史位置或暂停状态；`visible_text` 与原始 `record` 分开保存，旧任务不自动暂停。空库、升级与重复应用覆盖见 `TestMigration0046TaskConversationFreshAndUpgrade` 及 integration 的迁移测试。
- 0049 让 `thread_commands` 接受 WorkflowRun 归属，并要求工作流命令带图节点；每个节点至多一条 `end_session`，调度器可安全重放。旧行全部带 `run_id`，不回填。语义见 [工作流](../../../docs/workflows/workflows.md)，覆盖见 `TestWorkflowSessionEndsAfterSuccessfulTurn` 等 DB 测试。
- 0051 增加 `operations.last_claimed_at` 与 `operations_claim_rotation` 部分索引，候选按 `COALESCE(last_claimed_at, created_at), created_at, id` 轮转；它是调度簿记，不是进度、失败次数、租约或超时起点。历史行保持 NULL，不回填、不改写操作历史，只有真正把操作交给 Controller 才记录数据库时间。语义见 [工作区](../../../docs/runtime/workspaces.md)，空库、0050 数据库升级与重复应用覆盖见 `TestMigration0051ClaimRotationAppliesFreshAndUpgrades`。
- 新增迁移须验证空库、前一 schema 升级、重复应用与关键约束。覆盖见 [migration_upgrade_path_test.go](../../../integration/migration_upgrade_path_test.go) 和 integration 的领域升级测试。

数据归属与事务规则见 [持久化](../../../docs/core/persistence.md)，运行语义见 [工作流](../../../docs/workflows/workflows.md) 和 [外部 Git 线](../../../docs/repositories/external-git.md)。
