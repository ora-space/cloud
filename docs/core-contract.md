# Cloud 核心契约

Cloud 是唯一业务权威存储。Gateway 转发查询/生命周期到 cloud，执行交互到 Controller；Controller 通过 `/internal/v1` 读取受限聚合快照、领取和推进操作，不持有 PG 连接。所有核心 HTTP 命令在一个短 PG 事务内完成；外部 HTTP、Git 和 Node 调用从不跨事务。

## 归属和管理

`tenant_memberships(tenant_id,user_id)` 为 Project owner 的 FK 目标。Workspace 通过 `(project_id,tenant_id,owner_user_id)` 复合 FK 继承完整归属。Project/Workspace 归属和 Workspace kind 不可更新；operation/effect/ticket/node 也有跨表作用域约束。软删保留所有运行及清理引用。

每个未软删 Project 通过延迟约束触发器检查恰有一个未软删 main Workspace，允许在同一事务原子创建或整体删除；不能单独删 main。partial unique index 防止两个 main。isolated Workspace 有唯一 Task 展示身份。云端 main 同样有 `workspace_worktrees` 行及 linked worktree；这与只读参考的 desktop 当前 schema 不同，未改动 desktop/specs 的现有语义。

角色只分 admin/member。查询在 SQL 中过滤 tenant+owner；admin 不享有跨用户业务读权限。管理员成员列表只含身份显示信息和角色状态；资源状态只含资源 UUID、owner、kind、运行状态/generation/version。administrative-stop 的响应以及 operation GET/retry 使用专门投影，不含 repositoryUrl、secretRef、worktree、request/result/error 明细。最后一个有效管理员不能被删除/停用/降级；用户停用或租户启用也受 PG 延迟约束保护。没有公共用户删除或停用 CRUD。

## 幂等与并发

POST/DELETE 需要 `Idempotency-Key`，范围是 tenant+user，保留原始 HTTP 状态与响应。hash 由 method、path、规范化 JSON map 组成；相同 key 不同内容为 409。同 key 同请求首先重放，再检查当前资源版本，因此响应丢失后的旧 version 重试不会创建第二份资源。停用成员仍先被拒绝。

PATCH 与生命周期动作携带整数 `version`；现存 membership PUT/operation retry/Node status/idle/ticket finish 同样使用 version。缺失必需版本为 428，不匹配为 409；已经 finished 的同版本请求重放不产生第二次写入。列表按 UUID 升序，`limit` 1–100，`after` 是排他 UUID cursor；身份过滤在分页前执行。

首版所有核心事务共用 PG transaction advisory lock，每个 Project 最多一个 queued/running/retry_wait/blocked operation。创建 isolated 与删除 Project 竞争同一锁和 lifecycle 检查；先建立的 intent 获得操作权，另一方冲突。全局锁是一项明确吞吐限制，不是跨 HTTP 长事务。未来细化锁时必须保持直接约束与并发测试。

## 持久操作和外部副作用

每个外部动作前先持久化 `external_effects` plan，ID 是外部幂等键。kind、operation、Project、Workspace、状态、外部 ID 和 reconciled epoch 均为显式字段。JSONB request/result 只承载 OpenAPI 中的有限参数/证据，不藏核心状态。

| Operation | 受控推进步骤 |
|---|---|
| create_project | storage → worktree → sandbox → node → done |
| create_workspace | worktree → sandbox → node → done |
| start | sandbox → node → done |
| stop / administrative_stop | quiesce → terminate → done |
| delete_workspace | quiesce → terminate → cleanup → done |
| delete_project | quiesce → terminate → cleanup → storage_delete → done |

接口不接受“设 state=succeeded”这类任意写入。storage/worktree/sandbox 等阶段必须有同 epoch 成功 effect；Node 阶段必须有当前实例已初始化、connected、30 秒内 heartbeat 的 Node，才原子提交 worktree ready、Workspace Ready/开放准入和 operation success。Pod Running 或单个 Substrate 创建结果不能代替 Node 协议确认。

worktree 成功证据包含解析后的真实 40/64 位 commit 和维护 Job 终止确认；cleanup 包含 removed+jobTerminated；sandbox terminate 包含真实终止确认；存储删除必须等所有 sandbox 和已计划维护工作完成。Cloud 信任受认证 Controller 对 Substrate 的观察，但仍检查类型、绑定和阶段。实际证明基础设施终止是 Substrate/Node 阶段二实现的责任，不能拿 PG fencing 替代。

外部 ID 一经登记不可改变，已成功 effect 的结果不可改写。失败/超时保留 plan、外部引用和当前 step；`defer` 设置 retry_wait/blocked 及有限错误码，`retry` 重新入队，不凭超时推断外部未执行。模拟器在磁盘日志成功但 HTTP 响应丢失后按原 ID 查询恢复。

## 租约与接管

全局 `controller_leases(name=global)` 使用 PG `clock_timestamp()`，有效期 30 秒，约定每 10 秒续租；模拟器每个短 Step 续租。未过期 holder 不能被夺取；过期 acquire 增加 epoch，renew/release 需要精确 holder+epoch。Controller 调度前、领取、阶段结果、推进、重试安排、sandbox 分配和 execute 准入均验证有效租约；用户 read access 只做权限查询。

claim 会领取 queued、到期 retry_wait 或任意 running operation；同一 holder/epoch 重启后重新领取 running operation 时递增 operation version，从而 fence 仍持有旧内存快照的 worker。claim 返回当前 operation、Project/storage、全部相关 Workspace/sandbox/Node/effect。旧 epoch effect 必须先按稳定 ID 查询 Substrate，再登记本 epoch 的观察；否则 plan/advance 返回 reconcile_required。未知进行中维护 Job 先等终止或恢复同一任务，不能盲目创建另一个 Job。epoch 与 Workspace runtime_generation 是独立的。

`UNIQUE(workspace_id,generation)` 及唯一未终止 sandbox 保护替换。分配新 generation 前必须确认旧实例 terminated；登记新 Node 也不能覆盖活实例。数据库拒绝旧 epoch/旧实例迟到回写，但不会终止已经运行的文件写入。因此真实接管必须先查询/fence 外部进程；无法确认时保持 blocked，不能重放未知结果 prompt 或全局标记 Session 失败。

## 执行准入与 idle

`POST /internal/v1/access` 校验最终用户、成员、归属和 action；execute 还校验租约和可执行状态，但不产生 reservation。执行必须另用 `POST /internal/v1/admissions` 原子创建 `execution_tickets`，绑定用户、Workspace、当前 Node、admission epoch，kind=task/interaction。Node 消费受控 Controller 传递的票据；重复 ticket UUID 不应重复执行，完整 Session/Workflow 执行去重留阶段二。

这是最小真实业务活动契约，不是常量计数：任何未结束票据都阻止 stop/delete；状态不明继续视为活跃。只有该票据绑定 Node 的 `/nodes/tickets/{id}/finish` 能携带当前 ticket version 结束，已结束请求的重放幂等。正式 Node 必须对所有 Agent、PTY、后台 Job、待处理交互使用此准入边界，不能旁路发起工作。

停止先在同一锁下检查票据并关闭 `admission_open`、递增 admission_epoch。活跃票据让公开请求返回 resource_in_use 并回滚所有变更。关闭后 Controller 请求每个目标 Node 原子检查自身活动并报告 `idle`，证据绑定 operationId+Workspace+Node+admissionEpoch+Node version；同 Project 的其他 Node 无权影响本次 stop。idle=true 时 cloud 再确认零活动票据；没有 Node/旧 heartbeat/未知状态均不能推进。idle=false 在 quiesce 阶段失败该 operation 并恢复所有原准入，不取消工作。

Node 本地原子 idle 与实际开始执行之间的进程锁由阶段二 Node 实现；阶段一用真实 PG/HTTP 票据并发测试验证云端竞争，且测试了 Node 拒绝与错误 Workspace 的 idle 证据，未声称运行真实 Agent。
