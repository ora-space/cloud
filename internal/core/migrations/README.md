# 数据库迁移模块

[中文](README.md) | [English](README.en.md)

本模块包含 Ora Cloud 线性、仅向前的 PostgreSQL Schema 迁移目录。迁移通过 `embed.FS` 直接嵌入 Go 应用程序二进制，并由 `cloudctl migrate` 以确定的方式应用。

## 迁移目录

迁移脚本严格按照数字序号递增顺序执行。序列是 **append-only**：`0001–0007` 是 upstream 基线（与 `upstream/main` 逐字节一致、不可修改），`0008–0012` 是本地 Issue 迁移，`0013` 起为后续兼容迁移。

- **`0001_core.sql`**（upstream）：基础领域 Schema：
  - 身份与访问管理：`users`、`user_identities`、`tenants`、`tenant_memberships`、`credential_refs`。
  - 项目与工作区：`projects`、`project_storage`、`workspaces`、`workspace_worktrees`、`tasks`。
  - 执行运行时：`sandbox_instances`、`workspace_nodes`、`sessions`。
  - 控制平面：`effects`、`operations`、`tickets`、`controller_leases`、`idempotency_keys`。
  - 不变量：部分唯一索引 `one_main` 保证每个项目最多只有一个活动的 `main` 工作区。外键在所有层级中严格强制租户和所有者的包含关系。
- **`0002_aggregate_guards.sql`**（upstream）：并发与互斥守卫：
  - 防止在同一个项目聚合根上发生并发的生命周期变更。
  - 确保祖先实体软删除后，其子实体无法进行活动状态转换。
- **`0003_resource_versions.sql`**（upstream）：乐观并发版本控制：
  - 在可变实体（`projects`、`workspaces`、`tasks`、`nodes`、`operations`）上强制执行 `version` 递增规则。
  - 杜绝并发 API 操作中的更新丢失（lost updates）问题。
- **`0004_effect_intent_and_ticket_scope.sql`**（upstream）：执行意图与 Ticket 作用域约束：
  - 严格将执行 Ticket 限制到活动的 Workspace Node 和有效的准入 epoch。
  - 将持久化的 Effect 声明绑定至特定的操作阶段。
- **`0005_gateway_auth.sql`**（upstream）：Gateway 认证表（由 `cmd/gateway` 独占运行时访问）：
  - `gateway_login_attempts`：一次性登录尝试；只保存 attempt secret 与 `state` 的 SHA-256 digest，`return_to` 在数据库层拒绝绝对、`//`、`/\` 形式，有效期不超过 1 小时，`consumed_at` 保证最多创建一个 session。
  - `gateway_sessions`：浏览器会话；只保存 token digest，`expires_at` 非空且不超过创建后 90 天，吊销时间与有限的 `revoked_reason` 同时存在，并为 identity 吊销与有界清理建立索引。
- **`0006_collab_spaces.sql`**（upstream，与 `upstream/main` 逐字节一致）：协作空间（Collaboration Space，简称 Space）Schema：
  - `collab_workspaces`：租户内的协作与可见性边界（名称、不可变 slug、归档时间、乐观版本）。归档是软删除，slug 不随之释放：`UNIQUE(tenant_id, slug)` 覆盖活动与已归档行。
  - `collab_workspace_members`：成员与角色（owner/admin/member）、状态（active/disabled）、乐观版本。
  - 与运行时 `workspaces` 表（Runtime Workspace，执行环境）严格分离。
- **`0007_project_space_scope.sql`**（upstream，与 `upstream/main` 逐字节一致）：Project 的 Space 关联（**upstream 原语义：强制**）：
  - 为每个既有租户（含仍有 Project 的已删除租户）创建默认 Space（slug=`default`）。
  - 既有 active tenant members 加入默认 Space（admin→owner，member→member）。
  - 新增 `projects.space_id uuid NOT NULL`，并把每个既有 Project 绑定到其租户的默认 Space。
  - 复合外键 `(space_id, tenant_id) REFERENCES collab_workspaces(id, tenant_id)` 在 SQL 级杜绝跨租户归属；`project_space_list(space_id, id)` 索引支持按 Space 列举 Project。
- **`0008_issues.sql`**：Issues 看板基线表 `issues`（原 `0006_issues.sql`；迁移对账时前移重编号，令 upstream `0001–0007` 保持逐字节一致）。
- **`0009_issue_extensions.sql`**：`issue_statuses`、`issue_comments`、`labels`、`issue_labels`、`issue_subscribers`、`issue_views` + `issues` ALTER（`number`、`properties`、状态格式检查）。（原 `0007_issue_extensions.sql`）
- **`0010_issue_collaboration.sql`**：`issues` ALTER（`assignee_type`/`assignee_id`/`project_ref` + 回填）、`issue_comments` ALTER（`parent_id`/`author_type`/`author_id`/`seq` + 回填 + `UNIQUE(issue_id,seq)`）、新表 `issue_runs`、`issue_activities`、`issue_context_refs`。（原 `0008_issue_collaboration.sql`）
- **`0011_issue_interactions.sql`**：新表 `issue_interactions`（`@` 交互脊）——每个选中的协作目标一行：`id, tenant_id, issue_id, comment_id, target_type, target_id, mode, task, run_id, created_at`。（原 `0009_issue_interactions.sql`）
- **`0012_issue_interaction_input.sql`**：一个通用增量列：`ALTER TABLE issue_interactions ADD COLUMN input jsonb NOT NULL DEFAULT '{}' CHECK (jsonb_typeof(input)='object')` —— 已确认的表单值。刻意排除 `version`、`status` 枚举、`confirmed_at` 与独立 inputs 表；`0011` 不被修改。（原 `0010_issue_interaction_input.sql`）
- **`0013_project_space_optional.sql`**（append-only 兼容迁移）：`projects.space_id` 恢复为**可空**——upstream `0007` 施加了 `NOT NULL` 并对既有项目做了全量绑定；产品决策（PS3 / D2=C）要求 Space 保持**可选**分组。`0013` 仅放开约束；刻意**不解绑** `0007` 已分配给默认 Space 的项目（无数据改动、无作用域收缩）。
- **`0014_clone_coordination.sql`**（append-only，排在 upstream `0008–0013` 之后）：经内部控制契约的 clone 协调（与 Effect 级 `operations` 模型独立）：
  - `clone_requests`：Cloud 在业务事务中接受的工作项，`(tenant, user, request_id)` 幂等，状态 `queued→dispatched→succeeded/failed`。
  - `clone_executions`：Controller 派发前登记的执行（每个请求恰一个执行，身份为 Controller 选择的 opaque 字符串）、输入与终态结果、登记时的租约 epoch。
  - `clone_event_receipts`：Node 原事件的精确收据 `(execution, sequence, event)`，是确认 Node 的唯一依据。
  - `control_submissions`：每个状态变更提交的身份、请求摘要与记录的响应；同身份同内容回放响应，不重新应用。
- **`0015_plugins.sql`**：增加插件目录、空间级选择和运行时安装记录。
- **`0016_workspace_runtime_follows_node.sql`**：停用旧存储卷与工作树创建流程，采用独立 Workspace 数据、Node 和仓库克隆初始化，保留历史记录供读取。
- **`0017_tenant_membership_and_join.sql`**：收敛为一租户一空间，租户成员身份成为唯一权限来源；恢复项目必须归属空间的约束，空间 slug 在全平台唯一且归档后不复用；为 IDaaS 关联身份、邀请和加入申请增加持久化表。历史多空间、无空间项目、重复 slug 或租户与空间名称不一致的测试数据必须重建；迁移不会静默拆分或改名。
- **`0027_workflows.sql`**（append-only，工作流编辑器移植）：工作流图文档 `workflows`——租户拥有的整份图（`graph jsonb`：nodes / edges / viewport / annotations / global variables），刻意只有一张表和一个图列：图是文档而不是关系聚合，没有逐节点/逐边表，也没有 draft/published 拆分。
- **`0028_workflow_snapshots.sql`**：不可变的版本化快照 `workflow_snapshots`。发布把实时图冻结成一行并递增每个工作流的版本号，恢复把选中的快照写回实时图；快照 append-only，一次编辑不会重写任何历史版本。
- **`0029_workflow_runs.sql`**：一次执行对应一个冻结快照的 `workflow_runs`。运行视图只需要快照图（画布）与逐节点状态（着色），图本身经 `snapshot_id` 从 `workflow_snapshots.graph` 读取而不复制。Cloud 没有工作流引擎：新建的运行停在 `pending`，只有经 `Store.WorkflowRunSimulator` 显式接入的开发夹具才会推进状态——与 `issue_runs` 一样，绝不谎称真实执行发生过。

> 编号说明：这三个迁移原为 `0014–0016`，与 upstream 的同号迁移（`0014_clone_coordination` / `0015_plugins` / `0016_workspace_runtime_follows_node`）撞号，合入 `upstream/main` 时按 append-only 规则顺延到 `0024–0026`；随后 upstream 又追加了自己的 `0024_agent_run_control_plane` / `0025_agent_control_integrity` / `0026_run_runtime_control`，于是再顺延到 `0027–0029`，排在 `0026` 之后。`schema_migrations.version` 是**完整文件名**，所以每次重命名都会改变迁移身份：已经跑过旧名字的数据库必须重建或手工改 `schema_migrations`，否则 `CheckSchema` 会以「内嵌迁移缺失」拒绝启动。

## 校验和完整性与不可变性

- **`schema_migrations` 表**：记录已应用的迁移版本号、SHA256 校验和以及执行时间戳（`version`、`checksum`、`applied_at`）。**`version` 是完整文件名**（如 `0010_issue_collaboration.sql`），因此重命名已应用的迁移会改变其身份并破坏既有数据库——迁移一经应用不可修改，只能追加新文件。
- **服务端启动自检**：启动时，`cmd/server` 执行 `store.CheckSchema`，严格校验：
  1. 所有内嵌的 `.sql` 迁移脚本均已存在于 `schema_migrations` 表中。
  2. 每个内嵌文件的 SHA256 校验和与数据库中记录的校验和完全吻合。
  3. 数据库中不存在任何未知或多余的迁移版本。
  如果发现任何校验和不匹配或未应用的迁移，服务器会立即终止。
- **不使用 AutoMigrate**：生产服务器守护进程启动时**从不**执行 DDL 或修改表结构。迁移必须使用专用数据库管理员凭据通过 `cloudctl migrate` 应用。

参见 [core 总览](../README.md)、[cloudctl CLI 工具](../../../cmd/cloudctl/README.md) 与 [核心不变量与契约](../../../docs/core-contract.md)。
- **`0015_plugins.sql`**（append-only）：插件市场三张表与枚举放宽：
  - `plugin_sources`（部署全局源，默认 `official` 命名空间）、`plugin_catalog_entries`（目录快照，读取永远不出网）、`space_plugins`（工作区选择状态权威，`UNIQUE(space_id, source_namespace, identifier)`）、`workspace_plugin_instances`（fan-out 执行事实，复合外键继承 tenant/owner/project/workspace）。
  - 放宽 `operations.kind`（+install_plugin/remove_plugin）、`operations.step`（+plugin）、`external_effects.kind`（+plugin_ensure/plugin_delete）；原 CHECK 在 0001 定义，PG 命名为 `表_列_check`，0015 DROP 后以扩展集合重建。

## 多人运行时控制的追加迁移

0025 为 Thread 命令添加单调接受序号（同事务时间戳相同也不乱序），保留旧 created_at/id 排序，并约束每个运行一生只有一个会话。0026 为运行 Workspace 保留独立 IssueRun maintenance binding；初始化交接后会话和交付可以取得围栏许可，不占 Project operation。用户会话与运行占用互斥。真实 0024 升级和重复迁移证据见 `integration/agent_control_upgrade_test.go`。

0018–0024 在已发布 0017 后追加，不改写旧迁移：0018 单独保存经可靠记录证明的创建者，未知保持 NULL；0019 保存 PostgreSQL 操作会话、控制代次与审计；0020 保存独立强停意图、目标及重启阶段；0021 区分 Node operation ID 和 Cloud operation ID，保留重试历史；0022 保存插件请求者、固定版本与持久 pending（待执行）；0023 保存凭据引用归属依据、可用性、范围、能力和版本；0024 增加运行 Workspace 关联、插件/会话/交付执行登记、Thread 命令和 Space Agent 行，并停止新建插件 effect。旧 owner 外键、凭据关联与操作记录继续保留。历史个人/未知引用在关联成员已停用时冻结；不猜测团队身份或有效绑定。

真实 PostgreSQL 新库与 0017 升级证据见 integration/runtime_upgrade_test.go、runtime_control_test.go、repository_credentials_test.go 和 plugin_pending_test.go。业务事务只有数据库操作，不跨执行程序、网络或文件调用持锁。Node 的本地恢复日志不替代此业务权威。

0027 仅前向新增 Revision、验证结论与未配置存储跳过记录，保留原 Node 证据。复合外键约束租户/run/Workspace/project 范围；changed 与 unchanged 元数据互斥。revision_upgrade_test.go 验证真实 0026 升级与重复迁移；agent_plugin_upgrade_test.go 验证 0023 在途旧插件 effect 保留及新步骤重启。

与当前上游合并时保留 `0027_verified_revisions.sql`、`0027_workflows.sql` 及 `0028–0029` 的完整文件名和原始 SQL；前缀相同不等于迁移身份相同，执行器按完整文件名和校验和追踪已应用迁移。`TestRevisionUpgradePreservesPublishedWorkflowSchema` 直接验证已经应用上游工作流迁移的数据库可追加 Revision 表，重复迁移后原工作流、快照、运行和迁移账本均保持完整。

0030–0031 在已发布 0029 之后追加，是把 B 侧业务生命周期移植到上游控制面时**唯一**新增的 schema，functionB 原有但上游已具备的部分（0024–0029 的执行侧）一律不再新建：0030 只给 `issue_runs` 增加业务列（`phase`、`workspace_id`、`cancel_requested_at`、`thread_state`、`idle_since`）、agent 专用列约束、`workspace_id` 唯一绑定和 idle 线程的部分索引；0031 新建 `thread_entries`（`(run_id,seq)` 主键、Node 来源按 `(node_execution_id,node_sequence)` 幂等、user 来源必带 turn 生命周期、`record` 限 256 KiB JSON 对象）。两者纯前向、可重复执行，不回填也不改写任何既有行（0030 的列由它自己创建，旧行不可能带有需要迁移的值；非 agent 运行的整行保持原样），并且不覆盖上游 `0018–0029` 的同号文件。真实新库、升级与重复迁移证据见 `integration/migration_upgrade_path_test.go` 的 `TestMigration0030AgentRunBusinessLifecycleAppliesFreshAndUpgrades` 与 `TestMigration0031AgentRunThreadEntriesAppliesFreshAndUpgrades`。

0032 在 0031 之后新增 `user_git_identities`（identity-access git 身份 D1）：每个用户至多一行，主键并外键引用 `users(id)`，`name` 为 1–200 个字符且不含换行与尖括号，`email` 不超过 254 字节、形如 `local@domain` 且不含空白与尖括号，`version` 为该行自己的乐观并发计数。身份单独成表而不是给 `users` 加列，因为 `/me` 与所有用户读取都返回整行 `users`，git 邮箱放在那里会随之流向每个读者；没有行即使用默认身份。纯前向、可重复执行，不改动任何既有表。新库、升级与重复迁移证据见 `integration/migration_upgrade_path_test.go` 的 `TestMigration0032UserGitIdentitiesAppliesFreshAndUpgrades`。
