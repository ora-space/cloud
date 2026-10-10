# internal/core: 权威领域状态机引擎

[中文](README.md) | [English](README.en.md)

`internal/core` 是 Ora Cloud 的权威领域和状态机层。它负责所有业务聚合、状态转换、事务边界、密码学 token 验证和 PostgreSQL 持久化编排。

## 模块概览

- [migrations](migrations/README.md)：定义按顺序执行、仅向前的 PostgreSQL Schema 迁移脚本和校验和验证。
- [plugins.go](plugins.go)：插件市场目录快照的落库与读取（`pluginmarket.CatalogSink` 实现）、
  工作区插件选择状态机（安装/移除 fan-out、聚合规则、SSE 失效广播）。

## 架构与运行时模型

### 聚合根与实体关系
- **用户与身份（Users & Identities）**：用户通过稳定的 IdP 身份断言（`source`、`subject`）识别。华为登录以 IDaaS `uuid` 为登录键，以经验证的 `globalUserId` 关联预添加的人员；工号不参与授权。冲突的身份映射不能自动合并。
- **租户、协作空间与成员（Tenants, Spaces & Memberships）**：每个租户恰有一个可见的协作空间，用户可以加入多个租户并切换。`tenant_memberships` 是唯一的成员角色与状态来源，角色为平权的 `admin` 或 `member`。公网通过邀请或申请链接加入，内网通过天舟在职人员核验加入。
- **项目（Projects）**：每个项目属于一个租户及其唯一协作空间，保留 `(tenant_id, owner_user_id)` 作为持久资源归属与凭据边界；有效租户成员可以访问租户项目。每个 Workspace 按自己的 `requested_ref` 将项目仓库克隆到独立数据目录；`project_storage` 记录只作为历史保留。
- **工作区与任务（Workspaces & Tasks）**：每个项目至多拥有一个活跃的 `main` 主工作区（由 `one_main` 部分唯一索引强制约束）。其余工作区均为 `isolated` 隔离工作区，且与 `tasks` 保持 1:1 映射。
- **操作与效果（Operations & Effects）**：状态变更（如创建项目、启动/停止工作区或删除）作为持久化 `operations` 执行（状态包括 `queued`、`running`、`retry_wait`、`blocked`、`done`、`failed`）。Operation 被分解为持久化 `effects`，表示由 Substrate 和 Controller 执行的外部任务。
- **clone 请求（Clone Requests）**：由 `(tenant_id, actor_user_id, request_id)` 幂等接受的独立工作项，不挂在 operation/effect 模型上；Controller 经内部控制契约领取、登记派发（`clone_executions`）并接管 Node 结果（`clone_event_receipts`）。公开 `/clones` 路由只对提交者可见，`state` 由请求状态与执行结果投影而来。
- **节点与会话（Nodes & Sessions）**：`workspace_nodes` 表示绑定到工作区的活动执行容器。`sessions` 跟踪用户的对话线程。

### 并发控制与锁机制
- **事务级咨询锁**：第一阶段在 `Store.transact` 中使用 PostgreSQL 的 `SELECT pg_advisory_xact_lock(67420911)` 将领域变更串行化。这会在将锁保持在数据库内部的同时，避免聚合状态转换期间的竞态条件。
- **单项目单活动操作约束**：由 `idleProject` 守卫强制保证：若某个项目当前存在处于 `queued`、`running`、`retry_wait` 或 `blocked` 状态的未完成操作，则严禁为其调度新的操作。
- **乐观并发控制**：对可变实体的变更必须传入显式的 `version` 资源版本号。缺失版本号返回 `428 version_required`；版本号不匹配返回 `409 version_conflict`。
- **Controller 独占租约与 Epoch 栅栏**：Controller 工作进程通过 `/internal/v1/controller-lease/acquire` 获取独占租约并定期续约。任务分发使用单调递增的 `epoch` 栅栏（fencing），杜绝过期陈旧 Controller 实例写入。

### 幂等性保障
- 创建与加入等写请求需要 `Idempotency-Key`；租户内记录按 `(tenant_id, user_id)` 隔离，加入前的记录按 `user_id` 隔离。
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
- **租户强隔离**：项目和运行时读取先核验活动租户成员身份，再以可信 `tenant_id` 限定查询；`owner_user_id` 保留资源、凭据与外部执行归属，不用于排除同租户的其他成员。

参见 [数据库迁移目录](migrations/README.md)、[核心不变量与契约](../../docs/core-contract.md) 与 [认证配置与凭据](../../docs/authentication.md)。

## 个人模型连接

`model_connections.go`、`model_credentials.go`、`model_run_binding.go` 和 `model_grants.go`
管理用户私有连接、不可变密文引用、运行快照与临时令牌摘要。Core 不接收或解密上游 API Key；
冻结、权限、续期与撤销规则见 [个人模型连接](../../docs/model-connections.md)。
