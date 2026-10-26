# 权威持久化与事务模型

[English](persistence.en.md) | 中文

PostgreSQL 是 Cloud 唯一的业务权威。身份、成员、Project、Workspace、操作、执行证据、Issue、
Thread、Revision 登记等所有共享状态都写在这里；Controller、Node、Gateway、model-gateway 和
git-server 都不持有另一份业务状态。`internal/core` 是唯一直接读写业务表的包，其他进程和包通过
它暴露的有限入口（`Store.Public`、`Store.Control` 以及若干专用方法）访问数据。

本文说明 core 的事务模型、一致性手段和迁移规则。HTTP 层的约定见 [HTTP API 约定](http-api.md)。

## Store

`core.Store` 是注入的依赖集合，没有包级全局数据库句柄。

- `core.NewStore(db)` 只从注入的 GORM 句柄取出 `*sql.DB` 连接池，不建表、不迁移。GORM 只用于建立
  连接池（`internal/repository`），业务 SQL 全部是手写的参数化语句。
- `Store` 上挂着运行期需要的协作者：对象存储配置 `ObjectStore`、Git Server 的仓库地址与 ref 查询 `GitServer`、工作流资产下载 `WorkflowAssets`、
  协作端口（`Directory`、`Forms` 等，见 [Issue 协作](../collaboration/interactions.md)）、IssueRun 的
  业务钩子和时间窗口配置、SSE 事件 hub `Events` 与 Controller 信号 hub `Signals`。它们都由
  `cmd/*` 在启动时组装，core 不自行读取配置或环境变量。

## 事务：`transact` 与 `transactWorkflowRun`

普通读写经过 `Store.transact`，WorkflowRun 的推进、释放与对账经过 `Store.transactWorkflowRun`：

```text
BeginTx
  SELECT pg_advisory_xact_lock(67420911)    ← 全局事务级 advisory lock
  fn(t)                                     ← 业务逻辑，只做数据库操作
Commit
  发布提交后的提示：Controller 信号、SSE 事件

BeginTx
  SELECT pg_advisory_xact_lock_shared(67420911)             ← 全局锁的共享模式
  SELECT pg_try_advisory_xact_lock(67420911, hash(runId))   ← 该 WorkflowRun 的排他事务锁
  [SELECT pg_advisory_xact_lock(67420913, hash(issueId))]   ← 该 run 关联 Issue 时
  [SELECT pg_advisory_xact_lock(67420912, hash(projectId))] ← 只在决定 Project 级不变量时
  fn(t)
Commit
```

**全局锁。** 普通事务开始时先取同一把事务级 advisory lock，于是普通的 core 事务（包括只读请求）在
数据库里彼此串行，并与所有 WorkflowRun 事务互斥。这是一项有意的取舍：用写吞吐换取简单、可审计的
锁顺序，跨表不变量不必逐一设计行锁。锁在事务提交或回滚时自动释放，不会跨请求保留。

**WorkflowRun 的排他执行权。** WorkflowRun 的事务先以共享模式取全局锁，因此仍与所有普通事务互斥；
再以 `pg_try_advisory_xact_lock` 尽力取该 run 的排他锁。取不到说明另一个副本此刻正在执行同一个 run：
本次 tick 直接退出，不形成决策、不写、不发提示，下一次 tick 再试。锁就是事务本身，提交、回滚或副本
崩溃导致连接断开时由 PostgreSQL 释放，没有续期、没有过期窗口，也不依赖任何进程内状态；因此没见过
彼此的副本同样不能并发推进同一个 run。不同 run 的锁互不冲突，多个副本可以并行推进不同 run。哈希冲突
只会让两个 run 共用一把锁（多串行化一点），不会让一个 run 出现两个执行者。

**Issue Timeline 的写入顺序。** Timeline 的 `seq` 按 Issue 取 `MAX+1`（`issue_comments` 与
`issue_activities` 共用），并发写入同一 Issue 的两次读取会得到同一个值，后写入的一方会撞上
`UNIQUE(issue_id, seq)` 而丢掉整个事务。run 锁按 run 划分，表达不了这个约束，所以取到 run 锁后，
如果该 run 关联了 IssueRun，再取该 Issue 的事务锁（`pg_advisory_xact_lock`）；没有关联 Issue 的
直接运行不写 Timeline，不取这把锁。普通事务由全局排他锁串行，WorkflowRun 事务由这把 Issue 锁串行，
两条路径合起来保证同一 Issue 的 `seq` 单调且不重复。

Project 级不变量（“一个 Project 至多一个活动 operation”）跨 run，单 run 的锁表达不了，所以创建与声明
删除 run Workspace 时再取 Project 锁。两条路径的锁顺序一致：全局 → run → Issue → Project，不会成环。

**事务里只做数据库工作。** 事务内不发 HTTP、不调 Git、不访问对象存储、不读写文件、不等待 Node。
需要外部 I/O 的流程拆成"事务 → 外部调用 → 事务"：

- Revision 交付先在事务外核对对象存储里的对象，再开事务登记结果（见 [Revision](../agent-runs/revisions.md)）。
- 会话归档读取先在短事务里核验权限，事务提交后再签名访问对象存储，读完再开事务复核权限。
- 评论触发的 Task Mode 运行在评论事务提交之后才派发。

例外是 Git Server 的逐请求授权：它用只读、`REPEATABLE READ` 的独立事务读取当前状态，不取全局锁，
因为它只判定、不写入（见 [Git Server](../repositories/git-server.md)）。

**错误以 panic 传递，在 `transact` 边界还原。** 事务内的业务代码用 `reject(status, code)`、
`require(ok, status, code)` 抛出 `*Fault`，数据库错误包装成 `databaseFailure` 抛出。`transact`
统一 recover：`*Fault` 原样作为错误返回，`databaseFailure` 还原成底层错误，其他 panic 继续向上抛，
不吞掉真正的程序错误。任何错误都会回滚整个事务，所以一次请求的写入要么全部生效，要么都不生效。

**查询辅助。**

| 方法 | 用途 |
|---|---|
| `t.exec(q, args...)` | 执行写语句 |
| `t.execRows(q, args...)` | 执行写语句并返回影响行数，用于比较并交换（CAS）式的守卫，区分"我抢到了"和"别人已经做了" |
| `t.list(q, args...)` | 把查询包成 `SELECT row_to_json(resource) FROM (…) resource`，返回 `[]Object` |
| `t.one(q, args...)` | `list` 的第一行，没有结果时返回 `nil` |

`list`/`one` 只把**顶层**列名从 `snake_case` 转成 `camelCase`；`jsonb` 列里嵌套的键保持数据库里的原样。
聚合列按 SQL 别名读取（`SELECT min(position)` 得到键 `min`）。值为 `null` 的 `issueRunId` 会从结果里
删掉，因为运行 Workspace 不出现在公开文档里。

## 提交之后才发布

事务运行期间收集的副作用只在 `Commit` 成功后发出，回滚时全部丢弃：

| 收集项 | 提交后的动作 |
|---|---|
| 新排队或重试的 Workspace 操作 | `ControlHub` 发 `OperationAvailable` |
| 新的执行工作项（Agent 会话、Revision 交付） | `ControlHub` 发 `WorkAvailable` |
| 新的 Thread 命令 | `ControlHub` 发 `ThreadCommandAvailable` |
| Space 内的业务变化 | `SpaceHub` 向该 Space 的 SSE 订阅者发失效事件 |

这些都是**提示**，不携带业务状态：

- `ControlHub` 的信号至多投递一次，不持久化，慢订阅者直接丢信号。Controller 漏掉信号也不会丢工作，
  它会按周期重新领取（见 [Controller 内部控制契约](../runtime/controller-integration.md)）。
- `SpaceHub` 的事件只告诉前端"这里变了"，前端随后通过 REST 读取权威状态（见 [HTTP API 约定](http-api.md#sse-失效事件流)）。

两个 hub 都是进程内设施：只有连到同一个 Cloud 进程的订阅者能收到，没有重放，也没有跨实例投递。

## 同一事务里的业务钩子

Agent 运行的执行证据和业务状态由两侧分别拥有：控制面写 `execution_work`、`node_executions`、
`node_event_receipts`、`thread_commands`、`revisions` 等执行证据，业务侧写 `issue_runs` 的阶段与
Thread 列、`thread_entries`。每张表只有一个写入方。

控制面在接管证据的事务里调用五个业务钩子（`OnThreadEvents`、`OnSessionEnded`、
`OnRunWorkspaceSettled`、`OnRunWorkspaceDeleted`、`OnDeliverySettled`），由 `core.BindBusinessHooks`
在启动时绑定。钩子拿到的是调用方的 `*sql.Tx`，通过 context 找回外层 `transaction`，在同一个事务里
写业务表、声明后续工作；不开新事务、不起 goroutine、不做外部 I/O。钩子返回错误时，控制面以
`409 business_hook_failed` 回滚整个接管，Node 事件不被确认，之后会重放。于是业务转换和它依据的证据
总是一起提交或一起回滚。完整生命周期见 [Agent IssueRun](../agent-runs/issue-runs.md)。

## 一致性手段

**资源版本。** 可变资源带整数 `version`。修改类请求必须带当前版本：缺失返回 `428 version_required`，
不一致返回 `409 version_conflict`；每次读改写都执行 `version=version+1`。

**幂等记录。** 会创建资源或产生副作用的公开请求（`POST`、`DELETE`）必须带 `Idempotency-Key`：

| 场景 | 记录表与作用域 |
|---|---|
| 租户内的请求 | `idempotency_records`，按 `(tenant_id, user_id, key)` |
| 自助创建租户（`POST /api/v1/tenants`） | `idempotency_records`，按 `(user_id, key)` 在该用户所属的全部租户中匹配，记录写在新租户名下 |
| 加入租户之前的请求（兑换邀请、提交申请） | `join_idempotency_records`，按 `(user_id, key)` |

请求指纹是 `method`、`path` 与规范化 JSON 请求体拼接后的 SHA-256。同一个 key、同一指纹的请求直接
返回记录下的状态码和响应体，且发生在版本检查**之前**，所以"响应丢失后用旧版本重试"不会产生第二份
资源；同一个 key、不同指纹返回 `409 idempotency_conflict`。成员被停用后，重放同样先被成员校验拒绝。

Controller 侧的内部 RPC 用调用方生成的 `submission_id` 实现同样的语义，见
[Controller 内部控制契约](../runtime/controller-integration.md#提交身份)。

**单 Project 单活动操作。** 一个 Project 同时至多有一个处于 `queued`、`running`、`retry_wait` 或
`blocked` 的生命周期操作，新操作以 `409 operation_in_progress` 拒绝（见 [Workspace 生命周期](../runtime/workspaces.md)）。

**数据库约束。** 能由数据库保证的跨行不变量写成约束，而不是只靠应用代码：

- 复合外键把每个子资源绑到它的租户和归属上，例如 Workspace 通过 `(project_id, tenant_id, owner_user_id)`
  继承 Project 的完整归属，引用用户的列通过 `(tenant_id, user_id)` 指向 `tenant_memberships`。
- 延迟约束触发器保证每个未删除的 Project 恰有一个未删除的 main Workspace、最后一个有效管理员不能被
  停用或降级。
- 部分唯一索引、`CHECK` 约束固定状态取值和"至多一个未结束"之类的规则。

**数据库时间。** 租约、有效期、截止时间和空闲窗口都用 PostgreSQL 的 `now()` 或 `clock_timestamp()`
判定，不用 Cloud、Controller 或 Node 的进程时钟。

**软删除。** 业务行删除时写 `deleted_at`，保留终止、清理、审计和恢复需要的引用；查询显式带
`deleted_at IS NULL`。纯关联表（例如 Issue 与标签的关联）直接删除。

## Schema 迁移

迁移是 `internal/core/migrations/` 下按文件名排序的 SQL 文件，编译时嵌入二进制。

- **只由 `cloudctl -command migrate` 应用。** `Store.Migrate` 在同一把全局锁下、一个事务内依次执行尚未
  应用的文件，并把文件名和 SHA-256 校验和写入 `schema_migrations`。部署用独立的迁移身份执行，业务进程
  使用只有 DML 权限的账号。
- **业务进程只校验，不执行 DDL。** server、gateway、model-gateway、git-server 启动时调用
  `Store.CheckSchema`：二进制里的每个迁移都必须已经应用且校验和一致，数据库里也不能有二进制不认识的
  迁移；任何一项不满足就拒绝启动。
- **已应用的迁移不可修改。** 校验和会暴露任何改动。后续变化一律追加新的前向迁移，并同时测试新库和
  从上一版 schema 升级。
- **按合并顺序编号。** 迁移以完整文件名追踪，执行顺序就是文件名顺序。新迁移的编号必须接在合并时
  main 上的最新编号之后，不新增与已有迁移同号或更小的编号。历史上留下的同号文件
  （`0027_verified_revisions.sql` 与 `0027_workflows.sql`、`0033_personal_model_connections.sql` 与
  `0033_revision_resume.sql`）保持原样。

每个迁移的内容见 [迁移目录说明](../../internal/core/migrations/README.md)。

## 相关决策

- [协作 Space 的 Schema、确定性回填与迁移不可变性](https://github.com/ora-space/specs/blob/main/decisions/cloud/persistence/0-collab-space-schema-and-backfill.md)
- [Cloud 拥有 Internal Control gRPC 契约并承担权威持久化](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/0-cloud-owned-internal-grpc-contract.md)
- [Agent 会话、Thread 与上传授权的执行契约](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260928-agent-run-executions-thread-and-upload-grants.md)
