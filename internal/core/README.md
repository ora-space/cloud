# internal/core: 权威领域状态机引擎

[中文](README.md) | [English](README.en.md)

`internal/core` 是 Ora Cloud 的权威领域和状态机层。它负责所有业务聚合、状态转换、事务边界、密码学 token 验证和 PostgreSQL 持久化编排。

## 模块概览

- [migrations](migrations/README.md)：定义按顺序执行、仅向前的 PostgreSQL Schema 迁移脚本和校验和验证。

## 架构与运行时模型

### 聚合根与实体关系
- **用户与身份（Users & Identities）**：用户通过稳定的 IdP 身份断言（`source`、`subject`）进行唯一识别。用户记录在首次通过鉴权访问或由管理员初始化引导（bootstrap）时建立。
- **租户与成员（Tenants & Memberships）**：租户用于隔离组织边界。用户作为成员归属于租户，拥有 `admin` 或 `member` 角色。
- **项目（Projects）**：由 `(tenant_id, owner_user_id)` 所有。每个项目都有一个关联的仓库 URL 和默认分支，并关联一条 `project_storage` 记录。
- **工作区与任务（Workspaces & Tasks）**：每个项目至多拥有一个活跃的 `main` 主工作区（由 `one_main` 部分唯一索引强制约束）。其余工作区均为 `isolated` 隔离工作区，且与 `tasks` 保持 1:1 映射。
- **操作与效果（Operations & Effects）**：状态变更（如创建项目、启动/停止工作区或删除）作为持久化 `operations` 执行（状态包括 `queued`、`running`、`retry_wait`、`blocked`、`done`、`failed`）。Operation 被分解为持久化 `effects`，表示由 Substrate 和 Controller 执行的外部任务。
- **节点与会话（Nodes & Sessions）**：`workspace_nodes` 表示绑定到工作区的活动执行容器。`sessions` 跟踪用户的对话线程。

### 并发控制与锁机制
- **事务级咨询锁**：第一阶段在 `Store.transact` 中使用 PostgreSQL 的 `SELECT pg_advisory_xact_lock(67420911)` 将领域变更串行化。这会在将锁保持在数据库内部的同时，避免聚合状态转换期间的竞态条件。
- **单项目单活动操作约束**：由 `idleProject` 守卫强制保证：若某个项目当前存在处于 `queued`、`running`、`retry_wait` 或 `blocked` 状态的未完成操作，则严禁为其调度新的操作。
- **乐观并发控制**：对可变实体的变更必须传入显式的 `version` 资源版本号。缺失版本号返回 `428 version_required`；版本号不匹配返回 `409 version_conflict`。
- **Controller 独占租约与 Epoch 栅栏**：Controller 工作进程通过 `/internal/v1/controller-lease/acquire` 获取独占租约并定期续约。任务分发使用单调递增的 `epoch` 栅栏（fencing），杜绝过期陈旧 Controller 实例写入。

### 幂等性保障
- 变更状态的请求支持在 Header 中传入可选的 `Idempotency-Key`，其作用域受限于 `(tenant_id, user_id)`。
- 系统根据 HTTP 方法、请求路径和归一化后的请求体计算 SHA256 哈希指纹。
- 若相同请求重放已有 Key，系统直接返回之前持久化存储的 HTTP 响应。
- 若使用相同的 Key 发送不同内容的请求，则直接拒绝并返回 `409 idempotency_conflict`。

### 身份认证与信任机制
- `Authenticator` 基于启动时加载的 Ed25519/RS256 公钥验证 JWT token：
  - **服务 token**：带有 `kind="service"` 和 `role`（`gateway`、`controller` 或 `node`）声明。
  - **用户 token**：带有 `kind="user"` 声明，由网关在 `X-Ora-User-Token` header 中转发。路由层严格校验 `user.Caller == service.Subject`。

### 错误处理
- 领域层只产生 `*Fault` 值，其中包含机器可读的 `Code`、动态 `Params` 和 HTTP `Status`。
- `ErrorCode(err)` 将领域错误和内部数据库错误转换为对客户端安全的 `Fault` 对象，并将意外错误映射为 `500 internal_error`，而不泄露数据库 Schema 或凭据信息。

## 边界与不变量

- **严禁长事务**：数据库事务绝对禁止跨越外部网络调用、Git 操作、Substrate 调用或子进程执行。
- **零内存业务状态**：所有状态流转在向客户端返回成功之前，必须已提交持久化至 PostgreSQL。
- **租户强隔离**：所有 SQL 查询必须强制附加 `tenant_id` 和 `owner_user_id` 过滤条件，在 SQL 约束级别杜绝跨租户数据泄漏。

参见 [数据库迁移目录](migrations/README.md)、[核心不变量与契约](../../docs/core-contract.md) 与 [认证配置与凭据](../../docs/authentication.md)。
