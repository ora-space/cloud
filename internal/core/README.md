# internal/core: 权威领域状态机引擎

[中文](README.md) | [English](README.en.md)

`internal/core` 是 Ora Cloud 的权威领域和状态机层。它负责所有业务聚合、状态转换、事务边界、密码学 token 验证和 PostgreSQL 持久化编排。

## 模块概览

- [migrations](migrations/README.md)：定义按顺序执行、仅向前的 PostgreSQL Schema 迁移脚本和校验和验证。
- [plugins.go](plugins.go)：插件市场目录快照的落库与读取（`pluginmarket.CatalogSink` 实现）、
  工作区插件选择状态机（安装/移除 fan-out、聚合规则、SSE 失效广播）。
- [session_list.go](session_list.go) 与 [session_history.go](session_history.go)：租户 Agent 会话列表、
  当前成员鉴权和已登记 JSONL 归档的完整读取，不创建会话或修改运行状态。

## 架构与运行时模型

### 聚合根与实体关系
- **用户与身份（Users & Identities）**：用户通过稳定的 IdP 身份断言（`source`、`subject`）识别。华为登录以 IDaaS `uuid` 为登录键，以经验证的 `globalUserId` 关联预添加的人员；工号不参与授权。冲突的身份映射不能自动合并。
- **租户、协作空间与成员（Tenants, Spaces & Memberships）**：每个租户恰有一个可见的协作空间，用户可以加入多个租户并切换。`tenant_memberships` 是唯一的成员角色与状态来源，角色为平权的 `admin` 或 `member`。公网通过邀请或申请链接加入，内网通过天舟在职人员核验加入。
- **项目（Projects）**：每个项目属于一个租户及其唯一协作空间，保留 `(tenant_id, owner_user_id)` 作为持久资源归属与凭据边界；有效租户成员可以访问租户项目。每个 Workspace 从 Binding 当前快照克隆到独立数据目录，本地分支为自己的 `requested_ref`（Binding 分支，即 `defaultBranch`）；`project_storage` 记录只作为历史保留。
- **仓库 Binding 与 Operation（Repository Bindings & Operations）**：项目的外部仓库（地址、要同步的分支、凭据引用）记录在它唯一的 `repository_bindings` 行上，`projectView` 以项目原有字段名读出。建项目的同一事务建立 Binding 和一行 `pending`、`project_created` 的同步（`repository_operations`）。同步与发布 Operation 的状态和终态由 CHECK 约束固定，每个 Binding 至多一个未结束的同步（部分唯一索引）；`repository_snapshots` 记录 Binding 当前核验过的快照。项目进入 `deleting` 的事务取消尚未 `running` 的 Operation 并清除补同步标记，`delete_project` 等到没有未结束的 Operation 才开始；`create_project` 失败时项目一律转为 `active`。Repo Executor 的 Git 访问由 `AuthorizeGitOperation` 按 Operation 的当前状态判定；clone、Agent session 与 Revision 交付 execution 的 Git Server 访问由 `AuthorizeGitAccess` 按当前状态逐请求判定：每个获准的 execution 都读整个 Project 仓库，可写的 ref 由 execution 类型决定，插件 execution 一律拒绝。
- **工作区与任务（Workspaces & Tasks）**：每个项目至多拥有一个活跃的 `main` 主工作区（由 `one_main` 部分唯一索引强制约束）。其余工作区均为 `isolated` 隔离工作区，且与 `tasks` 保持 1:1 映射。
- **操作与效果（Operations & Effects）**：状态变更（如创建项目、启动/停止工作区或删除）作为持久化 `operations` 执行（状态包括 `queued`、`running`、`retry_wait`、`blocked`、`done`、`failed`）。Operation 被分解为持久化 `effects`，表示由 Substrate 和 Controller 执行的外部任务。
- **Workspace clone 与 Git Server（Workspace Clones & Git Server）**：create_project / create_workspace 的 clone 步骤由 Cloud 从 Binding 当前快照冻结输入（`operations.request.clone`），没有快照时停放 operation 而不派发；Controller 登记派发（`clone_executions`，必须属于 Workspace operation）并接管 Node 结果（`clone_event_receipts`）。`git_server.go` 定义 `GitServer`（仓库 base URL 与 ref 查询）、`GitRefResolver` 与 `ErrGitRefNotFound`；没有 Git Server 时 clone 步骤 `blocked`（`git_server_unconfigured`），Revision 交付 `skipped`。仓库凭据不控制任何 Node 工作。
- **Revision 交付与续接（Revision Delivery & Resume）**：交付工作输入带 `historyKey`（会话 JSONL）、Node 本地的 `revisionRef`、`gitRepositoryUrl`、`deliveryRef = refs/ora/deliveries/<deliveryWorkId>` 与取自 checkout clone 输入的 `workspaceRef`，不带 bundle；未配置对象存储（先检查）或 Git Server 时交付 `skipped`（`object_store_unconfigured`／`git_server_unconfigured`）。核验先比对声明与输入，再在事务外经 `GitRefResolver` 查询交付 ref（必须指向声明的最终提交）并 HEAD JSONL，最后在事务内登记：ref 缺失或指向其他提交、历史不符为 Cloud 的 `failed{verification_failed}`；Git Server 或对象存储不可达为 UNAVAILABLE，不接管、可重放。`delivered` 要求最终提交不同于基础提交和前序最终提交；`unchanged` 也推送并登记自己的交付 ref。`GrantRevisionUpload` 只签输入的 `historyKey`。续接候选是同一 Issue 其他运行最近一个带 `delivery_ref` 且有新提交的 Revision，会话 `priorRevision` 为 `{revisionId, finalCommit, baseCommit, gitRepositoryUrl, deliveryRef}`，Project 或 Git Server 仓库不同、或已拒绝续接时跳过（`project_changed`／`repository_changed`／`resume_refused`）。会话输入的 `workspaceDirectory` 列出 Project 最多 100 个 Workspace（最新在前），只供参考，从不用于授权。
- **节点与会话（Nodes & Sessions）**：`node_instances` 记录运行时工作区上的 Node。Agent 会话以
  `issue_runs`、`thread_entries` 和执行收据持久化；已交付的完整 JSONL 由 `revisions` 登记到对象存储。

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

## 会话 Dashboard 读取

`GET /api/v1/tenants/:tid/agent-sessions` 仅列出已持久化的 Agent IssueRun，排除已删除的 Run 和
Issue。筛选项为 `projectId`、单个 `status`、精确插件标识 `agent` 和创建时间区间 `from`（含）/
`to`（不含，RFC 3339）。默认 `limit=50`，最大 100；按 `(createdAt,id)` 倒序，`after` 为上一页的
`nextCursor` UUID。游标必须属于当前租户内可见的 Agent Run；不支持 `before`。

两个新读取入口在通用 `Public` 事务之前分派：`sessionReadIdentity` 只解析已有身份及 Huawei 别名，
检查身份冲突、用户启用状态和当前租户成员资格，不进行 JIT 建号、别名绑定或运行时状态刷新。
列表只投影 Issue、项目、Agent 插件、状态、时间及可空的冻结模型摘要；`historyAvailable` 仅表示存在
未过期的 Revision 登记，不保证对象存储当前可达，也不保证文件在读取上限内。

`GET /api/v1/tenants/:tid/issues/:iid/runs/:rid/history` 由 `Store.SessionHistory` 实现。
它先在短事务中校验成员、Issue、Agent Run 和未过期 Revision，再在事务外通过私有 S3 端点签名 GET。
对象键只能来自登记，并精确匹配 `revisions/{tenant}/{run}/{work UUID}/session.jsonl`；客户端不能传 URL
或路径。实际字节必须与登记大小、SHA-256 相符，最大 32 MiB、100000 条合法 UTF-8 JSONL 对象；
拒绝重定向、传播取消，存储读取超时 30 秒。返回前再次核验当前权限和登记一致性。

成功响应为 `{source:"archive",records:[...],sizeBytes,sha256}`。`records` 保留文件顺序、重复 `seq`
更新、原始字段和 JSON 数字精度，由消费者按原 history 语义重放。缺少或过期登记返回
`404 history_unavailable`，超限返回 `413 history_too_large`，损坏返回 `502 history_invalid`，
存储故障返回 `502 history_unavailable`；响应不含存储 URL、对象键或凭据。

第一版不将 Chat 或 Workflow Run 声称为 Agent 会话，也不扫描用户个人会话目录。实时页面继续通过已有
Thread API 读取投影，空间 SSE 只作失效提示。完整归档和 Thread 都不能保证存在 token、cost、父子会话
关系或每个工具的输入/输出与耗时；缺失信息应显示不可用，不能补造。原始记录的 `seq` 与 Cloud Thread
分页 `seq` 是两个序列，归档回放不得用后者覆盖前者。

`session_history*_test.go` 使用合成数据、真实 HTTP 和独立 PostgreSQL schema，覆盖授权、路径隔离、
纯只读身份解析、下载期间撤销权限/替换登记、大小/摘要/JSONL 上限、取消及数值保真；
`session_list_test.go` 覆盖列表作用域与分页。测试不使用真实用户日志作为 fixture。

## 模型请求统计

`model_attempts.go` 在发送上游 HTTP 前，以当前 grant 的真实租户、Run、执行与冻结模型登记请求。
迁移 `0035_model_attempt_statistics.sql` 新增 `model_request_attempts`，通过复合外键约束作用域；不回填旧会话。
完成记录只含状态、协议、模型标识、HTTP 状态、安全错误码、网关观察耗时与可空 token 指标，不保存正文或凭据。
开始失败则不发送；完成允许在 grant 撤销后写入，重复同内容完成幂等，不同内容为冲突。

`session_model_statistics.go` 提供同 Run 的 `/statistics` 与 `/model-calls` 只读接口，独立于归档可用性。
前者聚合该 Run 全部已登记 HTTP 尝试，后者按 `(startedAt,id)` 升序分页，默认 50、最大 100。
每个指标带 `value/knownCount/totalCount/availability`，缺失为 null，已知零仍为零；未完成快照不标为完整。
`started` 可能是正在请求，也可能是进程退出留下的未完成观察，不推断模型成功、失败或消费为零。
数据库聚合不读取日志正文，不执行身份创建或生命周期刷新；测试覆盖真实 PG、并发完成、只读查询和覆盖率。
