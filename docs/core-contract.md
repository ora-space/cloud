# Cloud 核心契约

Cloud 是唯一业务权威存储。Gateway 转发查询/生命周期到 cloud，执行交互到 Controller；Controller 通过认证的 gRPC 读取受限聚合快照、领取和推进操作；生产过渡 `/internal/v1` 管理路径已关闭，不持有 PG 连接。所有核心 HTTP 命令在一个短 PG 事务内完成；外部 HTTP、Git 和 Node 调用从不跨事务。

## 归属和管理

`tenant_memberships(tenant_id,user_id)` 为 Project owner 的 FK 目标。Workspace 通过 `(project_id,tenant_id,owner_user_id)` 复合 FK 继承完整归属。Project/Workspace 归属和 Workspace kind 不可更新；operation/effect/ticket/node 也有跨表作用域约束。软删保留所有运行及清理引用。

每个未软删 Project 通过延迟约束触发器检查恰有一个未软删 main Workspace，允许在同一事务原子创建或整体删除；不能单独删 main。partial unique index 防止两个 main。isolated Workspace 有唯一 Task 展示身份。每个 Workspace 保存自己的 `requested_ref`（Binding 分支，即 Project `defaultBranch`）与 clone 得到的 `base_commit_id`；Cloud 不再为 Workspace 建立 linked worktree，历史 `workspace_worktrees` 行只保留、不再新增。

每个租户恰有一个可见协作空间，`tenant_memberships` 是唯一的成员角色与状态权威。`cloudctl bootstrap` 为部署原子创建租户、唯一空间与首位管理员，空间 slug 使用生成的全局唯一值；`POST /api/v1/tenants` 允许已验证用户用名称与全局唯一 slug 创建自己的空间和租户，并成为首位管理员。用户可拥有和加入多个租户，通过 `GET /api/v1/me/spaces` 取得完整分页列表并切换。租户级与空间级项目路径均指向该唯一空间；空间 slug 不可变、不可复用，改名同时更新租户与空间。空间级创建、归档和成员写接口已停用。

角色只分 admin/member，管理员平权。活动租户成员可读取共享 Project 与运行时安全概况；运行时内容仅真实创建者或当前管理员可用，普通操作另需页面独占与无冲突活动；Project 的 `owner_user_id` 继续作为持久归属与凭据外键，不是额外的成员授权来源。项目删除和行政停止需管理员；行政停止响应使用受限投影，不含 repositoryUrl、secretRef、worktree、request/result/error 明细。最后一个有效管理员不能被停用或降级；成员停用立即拒绝新请求和准入，已建立的 SSE 流在投递下一条通知前重新校验成员身份并关闭。已开始的任务保留原生命周期与审计引用。没有公共用户删除或停用 CRUD。

内网管理员通过 Ora 服务端搜索天舟，添加时重查所选人员与在职状态，以 `globalUserId` 建立成员关系。IDaaS 的 `uuid` 仍是登录身份键；经验证的 `globalUserId` 是同一用户的目录关联键，冲突时拒绝自动合并，工号不参与授权。公网管理员可创建 7 天单次普通成员邀请和 30 天可重复申请链接；申请经管理员批准才加入。链接可撤销，数据库只保存令牌摘要。租户成员 PUT 只修改既有成员的角色或停用状态，不能直接新增或恢复成员；重新加入仍须新一轮目录核验、邀请或审批。

## 幂等与并发

POST/DELETE 需要 `Idempotency-Key`，范围是 tenant+user，保留原始 HTTP 状态与响应。hash 由 method、path、规范化 JSON map 组成；相同 key 不同内容为 409。同 key 同请求首先重放，再检查当前资源版本，因此响应丢失后的旧 version 重试不会创建第二份资源。停用成员仍先被拒绝。`POST /api/v1/tenants` 在租户存在之前执行，因此按 user+key 在该用户所属的全部租户中匹配重放，并把记录写在新建租户名下；同 key 不同内容同样为 409。

PATCH 与生命周期动作携带整数 `version`；现存 membership PUT/operation retry/Node status/idle/ticket finish 同样使用 version。缺失必需版本为 428，不匹配为 409；已经 finished 的同版本请求重放不产生第二次写入。列表按 UUID 升序，`limit` 1–100，`after` 是排他 UUID cursor；身份过滤在分页前执行。`GET /api/v1/me/tenants` 例外：按租户创建时间升序（`created_at`，`id` 为决胜负列），`items[0]` 因此是成员最早创建的租户；`after` 仍是排他租户 UUID cursor。

首版所有核心事务共用 PG transaction advisory lock，每个 Project 最多一个 queued/running/retry_wait/blocked operation。创建 isolated 与删除 Project 竞争同一锁和 lifecycle 检查；先建立的 intent 获得操作权，另一方冲突。全局锁是一项明确吞吐限制，不是跨 HTTP 长事务。未来细化锁时必须保持直接约束与并发测试。

## 持久操作和外部副作用

每个外部动作前先持久化 `external_effects` plan，ID 是外部幂等键。kind、operation、Project、Workspace、状态、外部 ID 和 reconciled epoch 均为显式字段。JSONB request/result 只承载 OpenAPI 中的有限参数/证据，不藏核心状态。

| Operation | 受控推进步骤 |
|---|---|
| create_project | sandbox → node → clone → done |
| create_workspace | sandbox → node → clone → done |
| start | sandbox → node → done |
| stop / administrative_stop | quiesce → terminate → done |
| delete_workspace | quiesce → terminate → cleanup → done |
| delete_project | quiesce → terminate → cleanup → done |

接口不接受“设 state=succeeded”这类任意写入。sandbox 阶段必须有同 epoch 成功 sandbox_ensure；Node 阶段必须有当前实例已初始化、connected、30 秒内 heartbeat 且 NodeId 等于 sandbox_ensure 返回值的 Node；create 还必须经过 clone 阶段：当前 Node 上最近一次 clone execution 为 clone_ready、带真实 40/64 位 commit 且等于冻结的 clone 输入的 `baseCommit`，才原子写入 `base_commit_id`、Workspace Ready/开放准入和 operation success。start 在 Node 阶段直接完成。Pod Running 或单个 Substrate 创建结果不能代替 Node 协议确认。

sandbox terminate 包含真实终止确认；cleanup 为每个待删 Workspace 计划 `workspace_data_delete`，只有该 Workspace 没有未终止 sandbox 时才能计划（否则 409 termination_unconfirmed），成功证据为 removed。delete_project 在 cleanup 完成全部 Workspace 数据删除后直接结束。0016 迁移把停在 storage/worktree 的进行中 operation 标记为 failed（lifecycle_flow_retired），把停在 storage_delete 的 delete_project 退回 cleanup。Cloud 信任受认证 Controller 对 Substrate 的观察，但仍检查类型、绑定和阶段。实际证明基础设施终止是 Substrate/Node 阶段二实现的责任，不能拿 PG fencing 替代。

外部 ID 一经登记不可改变，已成功 effect 的结果不可改写。失败/超时保留 plan、外部引用和当前 step；`defer` 设置 retry_wait/blocked 及有限错误码，`retry` 重新入队，不凭超时推断外部未执行。模拟器在磁盘日志成功但 HTTP 响应丢失后按原 ID 查询恢复。

## 租约与接管

全局 `controller_leases(name=global)` 使用 PG `clock_timestamp()`，有效期 30 秒，约定每 10 秒续租；模拟器每个短 Step 续租。未过期 holder 不能被夺取；过期 acquire 增加 epoch，renew/release 需要精确 holder+epoch。Controller 调度前、领取、阶段结果、推进、重试安排、sandbox 分配和 execute 准入均验证有效租约；用户 read access 只做权限查询。

claim 会领取 queued、到期 retry_wait 或任意 running operation；同一 holder/epoch 重启后重新领取 running operation 时递增 operation version，从而 fence 仍持有旧内存快照的 worker。claim 返回当前 operation、Project、全部相关 Workspace/sandbox/Node/effect、该 operation 的 clone execution，以及 clone 步骤从 Binding 当前快照冻结的 `cloneInput`（每个 operation 冻结一次，重试沿用）。没有可用快照的 clone 步骤、仍有未结束 sync/publish Operation 的 delete_project 不交给 Controller：claim 把它们停放（`retry_wait`/`failed`/`blocked`，见 [execution-contract.md](execution-contract.md)；delete_project 为 `retry_wait`/`repository_operation_running`，10 秒后重试、不计次数），同一次 claim 继续领取下一个。旧 epoch effect 必须先按稳定 ID 查询 Substrate，再登记本 epoch 的观察；否则 plan/advance 返回 reconcile_required。结果未知的 clone execution 阻止登记第二个，不能盲目再次 clone。epoch 与 Workspace runtime_generation 是独立的。

`UNIQUE(workspace_id,generation)` 及唯一未终止 sandbox 保护替换。分配新 generation 前必须确认旧实例 terminated；登记新 Node 也不能覆盖活实例。数据库拒绝旧 epoch/旧实例迟到回写，但不会终止已经运行的文件写入。因此真实接管必须先查询/fence 外部进程；无法确认时保持 blocked，不能重放未知结果 prompt 或全局标记 Session 失败。

## 执行准入与 idle

`POST /internal/v1/access` 校验最终用户、成员、归属和 action；execute 还校验租约和可执行状态，但不产生 reservation。执行必须另用 `POST /internal/v1/admissions` 原子创建 `execution_tickets`，绑定用户、Workspace、当前 Node、admission epoch，kind=task/interaction。Node 消费受控 Controller 传递的票据；重复 ticket UUID 不应重复执行，完整 Session/Workflow 执行去重留阶段二。

这是最小真实业务活动契约，不是常量计数：任何未结束票据都阻止 stop/delete；状态不明继续视为活跃。只有该票据绑定 Node 的 `/nodes/tickets/{id}/finish` 能携带当前 ticket version 结束，已结束请求的重放幂等。正式 Node 必须对所有 Agent、PTY、后台 Job、待处理交互使用此准入边界，不能旁路发起工作。

停止先在同一锁下检查票据并关闭 `admission_open`、递增 admission_epoch。活跃票据让公开请求返回 resource_in_use 并回滚所有变更。关闭后 Controller 请求每个目标 Node 原子检查自身活动并报告 `idle`，证据绑定 operationId+Workspace+Node+admissionEpoch+Node version；同 Project 的其他 Node 无权影响本次 stop。idle=true 时 cloud 再确认零活动票据；没有 Node/旧 heartbeat/未知状态均不能推进。idle=false 在 quiesce 阶段失败该 operation 并恢复所有原准入，不取消工作。

Node 本地原子 idle 与实际开始执行之间的进程锁由阶段二 Node 实现；阶段一用真实 PG/HTTP 票据并发测试验证云端竞争，且测试了 Node 拒绝与错误 Workspace 的 idle 证据，未声称运行真实 Agent。

## 插件市场与工作区插件

插件是 cloud 工作区(collab workspace,即产品"工作区")级别的资源,与技能、智能体同级:cloud 保存
`space_plugins` 的选择状态(desired 意图 + observed 聚合,乐观 version),执行经
`workspace_plugin_instances` fan-out 到该空间下每个 live 运行时 workspace,每个实例对应一条
`install_plugin`/`remove_plugin` operation(workspace 绑定、step=plugin)。canonical plugin identity 是
显式 `(source_namespace, identifier)` 列对,不是 `operations.result` 里的 JSON 大杂烩。

市场目录由 cloud server 自己维护:`internal/pluginmarket` 每 5 分钟(可配)git fetch + 扫描
`registry/**/orax.toml`,单事务整源替换 `plugin_catalog_entries`;目录读取永远不出网。插件安装是
Workspace `plugin` 步骤上的一次 Node 执行:载荷仍从目录快照自包含拼装 url/sha256/targets,Node 无需
registry index,并在落盘前校验 sha256。`plugin_ensure` / `plugin_delete` 只保留历史行,不再新计划。

安装/移除的公开路由走 space 成员门控与严格解码;重复安装幂等、版本冲突 409、缺版本 428。
完整契约见 [docs/plugins.md](plugins.md)。

## 运行时协作边界

[完整状态机与升级限制](runtime-control.md)。Creator、operator session 与任务 actor 独立于 durable owner。生命周期串行不替代单运行时写入互斥；管理员强停有独立持久意图，不删除原在途记录。SQL 0018–0023 只追加，未知创建者和旧责任保持保守。

## Agent 会话 Dashboard 的只读边界

`GET /api/v1/tenants/:tid/agent-sessions` 只投影已持久化的 Agent IssueRun，排除已删除 Run/Issue；
不把 Chat 或 Workflow Run 当作同一种会话。列表按 `(createdAt,id)` 倒序，每页默认 50、最大 100，
`after` 接收前一页的排他 UUID 游标，并在当前租户内重新解析。筛选支持项目 UUID `projectId`、单个
运行状态 `status`、精确插件标识 `agent` 及创建时间 `[from,to)`（RFC 3339）；不支持 `before`，
未知、重复或空查询项被拒绝。响应只含 Issue/项目、Agent、状态、时间与可空冻结模型摘要。

`GET /api/v1/tenants/:tid/issues/:iid/runs/:rid/history` 读取已登记 Revision 的完整 JSONL。
它不接受客户端 URL、文件路径、对象键或查询参数，不扫描用户个人文件系统。两个新入口只解析已有身份
及 Huawei 别名，检查冲突与启用状态，再核验当前活动租户成员资格；不 JIT 建号、绑定别名或刷新运行时。

归档读取先用短数据库事务核验租户、Issue、Agent Run 和未过期 Revision。对象键只能来自登记，并匹配
`revisions/{tenant}/{run}/{work UUID}/session.jsonl`。事务提交后才向配置的私有 S3 端点签名 GET；
禁止重定向，传播请求取消，存储超时 30 秒。校验真实字节大小和 SHA-256，最多读取 32 MiB，最多返回
100000 个合法 UTF-8 JSONL 对象。外部读取后在新事务中重新检查当前权限和登记一致性，撤销成员权限或
替换登记均不能让原请求继续返回会话内容。此路径不修改业务状态，也不需要数据库迁移。

成功响应为 `{source:"archive",records:[...],sizeBytes,sha256}`，记录保持原文件顺序、字段、JSON
数字精度与重复 `seq` 更新；消费者按 history 语义重放，不将 Cloud Thread 分页 `seq` 作为原记录序号。
没有或已过期登记返回 `404 history_unavailable`；实际大小或记录数超限返回 `413 history_too_large`；
完整性/JSONL 损坏返回 `502 history_invalid`，存储故障返回 `502 history_unavailable`。不在公开响应、
错误或日志中输出私有 S3 地址、签名 URL、对象键或凭据。`historyAvailable` 仅表达登记存在且未过期；
读取时仍可能因对象不可达、损坏或超限失败。

实时显示继续使用已有 Thread 投影与空间 SSE 失效通知，归档不是实时 token 流。历史协议并不保证
token/cost、父子会话关系或每个工具的原始输入/输出和耗时；只能展示已有证据，缺失项保持不可用。
用真实 HTTP、隔离 PostgreSQL schema 和合成记录验证 scope、分页、下载前后鉴权、只读身份解析、
摘要与上限、取消和数值保真；真实用户会话只能用于本地受控验收，不能进入产品 fixture。

## 模型 HTTP 请求统计边界

迁移 0035 为新模型请求建立独立台账。Model Gateway 在真正发送 HTTP 之前登记，之后保存完成观察；
每次 HTTP 尝试计数一次，客户端重试是新的尝试，SSE chunk 和 Thread 记录不是请求次数。
台账不含正文、Key、header、provider 错误原文或用户文件路径。服务崩溃留下的 `started` 保持未知，
不虚构完成或消费；完成写入使用有界独立上下文重试，重复内容幂等。

`GET .../runs/:rid/statistics` 聚合全 Run 已登记尝试，`GET .../runs/:rid/model-calls` 提供升序分页。
两者只读解析已有身份，校验当前成员、Issue 和 Agent Run，不依赖 Revision，返回 `private, no-store`。
前者不接受查询，后者仅 `limit`（默认 50，最大 100）和同 Run 的 UUID `after`。
来源固定为 `model_gateway`、范围为 `recorded_http_attempts`，不是旧会话完整请求数或模型账单。

六类 token 独立保留 provider 字段并返回覆盖率，未知为 null，测得 0 为 0。SSE 累计快照替换而非相加；
无终止事件的快照只能是部分数据。OpenAI input 含缓存读，Anthropic input 不含缓存读写；reasoning 是
output 子集，不与 output 重加。Provider 未报 total 时不猜总量；费用暂不推算。耗时来自网关单调时钟，
首个有效输出耗时排除 ping、空 role 和 usage-only 事件，不称 TTFT。汇总范围不随请求表分页改变。

Runtime 的已有最终 `turnEnded` 另外可携带 ACP `token_usage`，保留 accountingScope，与 Gateway 是
不同来源，不相加；unspecified 不作跨轮累计，内部重试不新加结束事件、不改变 Cloud Thread 状态机。
旧记录缺字段仍兼容，旧归档无用量不会伪造或回写。此阶段不覆盖其它未经过网关的 Agent、Chat/Workflow
全量会话、费用定价、子 Agent 拓扑或用户本地扫描。
