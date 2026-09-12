# 数据库迁移模块

[中文](README.md) | [English](README.en.md)

本模块包含 Ora Cloud 线性、仅向前的 PostgreSQL Schema 迁移目录。迁移通过 `embed.FS` 直接嵌入 Go 应用程序二进制，并由 `cloudctl migrate` 以确定的方式应用。

## 迁移目录

迁移脚本严格按照数字序号递增顺序执行：

- **`0001_core.sql`**：基础领域 Schema：
  - 身份与访问管理：`users`、`user_identities`、`tenants`、`tenant_memberships`、`credential_refs`。
  - 项目与工作区：`projects`、`project_storage`、`workspaces`、`workspace_worktrees`、`tasks`。
  - 执行运行时：`sandbox_instances`、`workspace_nodes`、`sessions`。
  - 控制平面：`effects`、`operations`、`tickets`、`controller_leases`、`idempotency_keys`。
  - 不变量：部分唯一索引 `one_main` 保证每个项目最多只有一个活动的 `main` 工作区。外键在所有层级中严格强制租户和所有者的包含关系。
- **`0002_aggregate_guards.sql`**：并发与互斥守卫：
  - 防止在同一个项目聚合根上发生并发的生命周期变更。
  - 确保祖先实体软删除后，其子实体无法进行活动状态转换。
- **`0003_resource_versions.sql`**：乐观并发版本控制：
  - 在可变实体（`projects`、`workspaces`、`tasks`、`nodes`、`operations`）上强制执行 `version` 递增规则。
  - 杜绝并发 API 操作中的更新丢失（lost updates）问题。
- **`0004_effect_intent_and_ticket_scope.sql`**：执行意图与 Ticket 作用域约束：
  - 严格将执行 Ticket 限制到活动的 Workspace Node 和有效的准入 epoch。
  - 将持久化的 Effect 声明绑定至特定的操作阶段。

## 校验和完整性与不可变性

- **`schema_migrations` 表**：记录已应用的迁移版本号、SHA256 校验和以及执行时间戳（`version`、`checksum`、`applied_at`）。
- **服务端启动自检**：启动时，`cmd/server` 执行 `store.CheckSchema`，严格校验：
  1. 所有内嵌的 `.sql` 迁移脚本均已存在于 `schema_migrations` 表中。
  2. 每个内嵌文件的 SHA256 校验和与数据库中记录的校验和完全吻合。
  3. 数据库中不存在任何未知或多余的迁移版本。
  如果发现任何校验和不匹配或未应用的迁移，服务器会立即终止。
- **不使用 AutoMigrate**：生产服务器守护进程启动时**从不**执行 DDL 或修改表结构。迁移必须使用专用数据库管理员凭据通过 `cloudctl migrate` 应用。

参见 [core 总览](../README.md)、[cloudctl CLI 工具](../../../cmd/cloudctl/README.md) 与 [核心不变量与契约](../../../docs/core-contract.md)。
