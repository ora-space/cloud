# internal/core：权威领域与持久化

[中文](README.md) | [English](README.en.md)

本包拥有业务状态、授权、事务与恢复判定。HTTP 和 gRPC 适配层将请求翻译到 Store；跨模块契约见 [文档地图](../../docs/README.md)。

## 代码地图

| 文件组 | 包内职责 | 跨模块说明 |
|---|---|---|
| `store.go`、`public.go`、`control.go` | 注入 SQL pool、事务包装、请求分发、幂等与 Fault | [持久化与事务](../../docs/core/persistence.md) |
| `auth.go`、`tenant.go`、`join.go`、`space*.go` | 已验证身份解析、租户与成员作用域 | [身份与租户](../../docs/identity/tenancy.md) |
| `node*.go`、`runtime_*.go`、`run_workspace.go` | Node 事实、控制会话、围栏与运行 Workspace | [运行时控制](../../docs/runtime/runtime-control.md) |
| `repository_*.go`、`git_*.go` | Binding、同步/发布 Operation、GitGrant 与凭据元数据 | [外部 Git 线](../../docs/repositories/external-git.md) |
| `issue*.go`、`collaboration*.go`、`form_descriptor.go` | Issue 聚合、时间线、交互和消费方协作端口 | [Issue 协作](../../docs/collaboration/interactions.md) |
| `agent_run_*.go`、`execution*.go`、`thread*.go`、`business_hooks.go` | 运行生命周期、执行登记、Thread 与同事务结算钩子 | [IssueRun](../../docs/agent-runs/issue-runs.md)、[Controller](../../docs/runtime/controller-integration.md) |
| `agent_run_residual.go`、`agent_run_residual_reclaim.go` | 原身份/目标覆盖与证据资格、单一回收责任和唯一 owned-delete operation、effect/许可及完成检查 | [残留回收](../../docs/agent-runs/residual-reclamation.md) |
| `agent_run_residual_fact.go`、`agent_run_residual_loop.go` | 不改旧责任的追加事实、重放与持久冲突阻挡；独立于 Run phase 的账本扫描和原 operation 恢复 | [残留回收](../../docs/agent-runs/residual-reclamation.md) |
| `agent_run_controlled_reclaim*.go` | 部署运维意图与命令回执、单一根责任和从属数据删除、逐目标终止与外部确认、无账本 D8 保护 | [残留回收](../../docs/agent-runs/residual-reclamation.md) |
| `task_conversation_*.go`、`thread_preview.go` | 任务级有序消息、同任务 Agent 串行交接与暂停屏障、从 Node 可见正文冻结的后续提示、配置缺口提示和有界进程内实时预览；Agent 初始提示只读冻结的运行输入 | [Thread](../../docs/agent-runs/threads.md)、[IssueRun](../../docs/agent-runs/issue-runs.md) |
| `revision*.go`、`late_delivery.go`、`session_*.go` | Revision 登记、原交付迟到事实及创建顺序守卫、归档读取、会话列表与统计 | [Revision](../../docs/agent-runs/revisions.md)、[会话读取](../../docs/agent-runs/session-reads.md) |
| `model_*.go` | 个人配置、冻结绑定、授权摘要与请求观察；不解密 API Key | [模型访问](../../docs/models/model-access.md) |
| `plugins.go`、`plugin_*.go`、`workflow_asset_imports.go` | 含 Agent 声明副本的市场快照、空间选择、冻结声明关联的 Node 安装计划和资产导入账本 | [插件市场](../../docs/plugins/marketplace.md) |
| `workflow*.go` | 图、发布快照、准入、运行身份、调度器；Issue Confirm/manual 与直接启动共用的 WorkflowRun 创建、`workflow_invocation_ref` 关联，以及调度器把 WorkflowRun 状态投影回关联 IssueRun | [工作流](../../docs/workflows/workflows.md)、[Issue 协作](../../docs/collaboration/interactions.md) |
| `hub.go`、`signals.go` | 提交后失效通知和控制信号；不承载权威状态 | [HTTP API](../../docs/core/http-api.md) |

## 定时运行的产物交付

`scheduled_artifact*.go` 保存声明、签发授权、核验对象并收敛交付，依据
[verdict 与产物 ADR](https://github.com/ora-space/specs/blob/main/decisions/cloud/scheduled-task/20261022-run-verdict-and-artifacts.md)。
Revision 与产物可以分别派发，每类最多一个未完成执行，两者均收敛后才释放运行 Workspace。
上传授权不超过运行的固定截止；到期回执仍按原执行身份确认，但不请求 HEAD、不登记对象。
最终登记在持锁后以数据库实际当前时间检查截止，不能沿用等待锁之前的事务开始时间。
整批失败或到期时，同事务为冻结清单中未登记的文件补齐拒绝事实；既有采集/核验拒绝与成功登记保持不变，重放不增记。
对象核验与删除都在事务外执行。清理按确定性键删除索引未覆盖的对象，已结算运行每分钟可再次扫描，
包含已被保留策略淘汰索引的成功交付；`artifacts_gc_at` 表示上次扫描时间，不表示永久完成。

零字节文件可声明、上传并登记；每个条目的 index 和名称分别去重。会话原始清单及交付行保留
`omitted_rejections` 和 `walk_truncated`，不会把有界拒绝明细当成完整遍历结果。只有拒绝或截断信息、
没有可上传文件时，保留这些事实并直接收敛，不派发空上传或等待截止。

## Store 与事务边界

- `NewStore` 从注入的 GORM 获取 `*sql.DB`，装配真实 Workflow Directory/Forms；不连接、迁移或建立全局数据库。
- `transact` 在 PostgreSQL 事务内取得 `pg_advisory_xact_lock(67420911)`，处理领域 Fault 和提交后的通知。调用方必须使用传入的事务执行钩子，不能嵌套开启 Store 事务。
- `transactWorkflowRun` 以共享模式取全局锁，再尽力取该 run 的排他锁；取到后按关联 Issue 取 Issue 锁，保证同一 Issue 的 Timeline `seq` 写入不并发。锁顺序固定为全局 → run → Issue → Project。
- 外部网络、Git、文件和对象存储调用必须在事务外；返回后按原身份、版本和授权重新核对。归档读取、发布源核对与凭据确认有专用入口。
- `Object` 是边界 JSON 值；`Fault` 与 `ErrorCode` 将可预期错误和数据库失败转换为安全公开错误。领域内部的 panic 只在事务控制边界转换，不能吞掉未知 panic。
- `Store` 的端口与策略时长由命令装配。开发协作端口见 [collab](../collab/README.md)；物理执行证据不由模拟结果代替。

迁移目录与完整文件名见 [migrations](migrations/README.md)。包内 `dispatcherDB` fixture 每个进程迁移一次真实 PostgreSQL 模板，再为各 fixture 创建独立数据库副本；跨 HTTP、Git 与存储边界的覆盖在 [integration](../../integration/README.md)。

残留事实继续由原入口接管：`agent_run_provisioning_fact.go` 处理创建侧，`node_execution.go` 与 `revision.go` 处理原执行终态和交付，
`runtime_control_delivery.go` 接收已有 `runtime_ack` 的完整运行时证据。`run_workspace.go`、`control.go` 及 effect/许可路径检查原责任绑定；
不新增 RPC/proto 或公开的系统删除开关。第 2 步受控入口为 `cloudctl residual`：以部署数据库凭据及 `session_user` 识别认证主体，
人工 operator 标签仅为自述，不新增 HTTP 或租户权限。根 terminate 与最多一个从属 delete_data 各自三态推进；根完成不回退，
删除只绑定一个 operation，保留首次 `cleared_at`。外部确认仅面向已有未完成意图，并要求逐目标事实引用，不替代物理成功证据。
受控目标借既有 `ListForceStops` / `ConfirmForceStop` / `GetEffectPermit` 字段交付，普通 force-stop 历史与业务收尾保持独立。
D8 按原身份显式接管，不回填账本。自动层 A1–A6、两阶段处置和恢复保护见上述专页；本文说明 Cloud 实现合同，
不声明测试通过，Controller/Sandbox 物理证明及跨仓故障验收仍分别属于第 3/4 步。

## 数据库测试 fixture

`fixture_template_test.go` 只用于本包测试：以 `core_template_` 加 32 位 UUID 建立并迁移模板，关闭迁移连接后封闭模板；每个 fixture 取得 `core_run_` 加 32 位 UUID 的独立副本。测试账号需要 `CREATEDB`。普通副本由 `t.Cleanup` 删除，受控回收矩阵沿用逐用例重置，并在 `TestMain` 先清理其副本、再清理模板。模板不接收业务写入，新的 Store 继续绑定原业务 hooks。包结束输出模板迁移和复制累计耗时。

`fixture_template_isolation_test.go` 验证数据、DDL、迁移记录、后续副本与并行副本隔离，并调用产品的 `CheckSchema`；迁移/升级专用测试仍走 integration 的空白/旧 schema 入口，不使用最新模板替代迁移。清理强制终止进程留下的库见[测试文档](../../docs/development/testing.md#清理遗留测试数据库)。

本包镜像 integration 的小型测试 helper：integration 的 `_test.go` 代码不能作为包导入，而且 integration 已依赖 core；抽取共享包会额外引入回调 API 并改动另一套 fixture。生产代码不依赖这些全局测试状态。

`scheduled_run_artifact_api.go` 在短事务中按当前成员、Issue 和 Run 检查读取范围。列表只返回登记元数据，下载在事务外通过私有端点读取并校验大小/SHA-256（上限 10 MiB），返回前再次核验权限与登记；调用者不能指定对象键。登记/淘汰同事务发出失效提示，历史中的 verdict 与数量从独立运行事实读取。

产物读取将成员关系不足映射为与不存在一致的 404。历史只有在已成功/部分交付且登记索引为空时报告产物过期，不从任务历史年龄或失败状态推断。
