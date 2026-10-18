# 有界启动：等待上限与未知结果的收尾

[English](startup-settlement.en.md) | 中文

运行 Workspace 就绪之后，Agent 会话可能迟迟起不来：Controller 没领到工作、Node 缺少能力、会话已经派发出去却一直没有记录回来。
如果没有上限，运行会永远停在 `starting`，沙盒一直被占着。本文说明 Cloud 如何给启动设定有界的等待，以及到期时如何收尾，
同时不把"没有回复"当成"外部没有执行"。

核心区分是：

- **会话工作项和首条提问是 Cloud 自己的意图**。只要还没有被 Controller 登记派发，Cloud 可以确定外部没有执行，可以原子地撤回。
- **派发登记意味着 Node 可能已经在执行**。之后再没有回复，也不能证明没有外部副作用，只能请求结束，并保留对账和残留资源的责任。

## 持久窗口

| 配置（`issue_runs.*`，环境变量前缀 `CLOUD_ISSUE_RUNS_`） | 默认 | 起点 |
|---|---|---|
| `agent_start_timeout` | 2 分钟 | 运行 Workspace 创建成功、就绪的时刻 |
| `agent_restore_timeout` | 6 分钟 | 同一个就绪时刻；运行要续接前序 Revision 时**替代**普通窗口，包含恢复和启动 |
| `agent_end_confirm_timeout` | 5 分钟 | 启动期间第一次请求结束会话的时刻 |

- 窗口在运行进入 `starting` 时一次性计算成**绝对截止时间**写进数据库（`startup_deadline_at`），同时固定是否续接前序 Revision。
  之后的声明、用户输入、重试、Cloud 重启或配置调整都**不会续期**；多个副本使用不同配置也不会改变已经固定的截止时间。
- 结束确认的截止时间在第一次请求结束时同样一次性固定（`startup_ending_deadline_at`），之后不再续期。
- 配置缺省时使用默认值；显式配置为零、负数、非时长或溢出的值时 server 拒绝启动。
- 迁移 `0034` 之前已经处于 `starting` 的运行，在串行扫描中只固定一次窗口：有成功的创建操作证据时以它的完成时间为起点，否则明确
  以第一次观察的时间为起点。它不为缺少证据的历史运行追溯精确期限，也不改写历史结果。

## 尚未登记派发

会话工作项还没有被登记派发时，下列情况由启动扫描在事务里**原子地撤回**意图并结算运行：

| 情况 | 结算 |
|---|---|
| 用户先结束了会话，或运行被取消 | `cancelled` |
| 当前 Node 已初始化、已连接、30 秒内有心跳，并且**明确**不具备模型代理能力，而运行使用个人模型 | `failed`，`failureReason = model_proxy_unavailable` |
| 启动窗口到期 | `failed`，`failureReason = agent_start_failed` |

撤回与派发登记共用同一把事务锁，所以撤回之后迟到的登记找不到工作项。运行进入 `releasing`，交付状态为 `skipped`，排队中的用户
轮次标为 `discarded`，运行 Workspace 的删除被声明，这些都在同一个事务里提交。

容量不足、排队、Node 暂时不可达都不是确定性的拒绝：工作项和输入原样保留，由已有的领取调度在原窗口内延后，Cloud 不为它另建队列
或退避策略。Node 的能力记录过期时也不被当作拒绝证据。

## 已登记、还没有启动记录

窗口到期时如果会话已经登记派发、却还没有任何启动记录，Cloud **请求结束**：放出 `EndSession{cancelled}` 命令，固定结束确认窗口，
在运行结果里记录启动状态为"等待 Node"，所有执行和资源证据都保留。用户原先给出的结束原因会被保留。

之后有两种走向：

- **真实终态及时到达**：沿正常的交付链路处理（进入 `delivering`，尝试保存 Revision）。由启动超时引起的这种取消，运行最终记为
  `failed` 而不是 `cancelled`。
- **结束确认窗口也到期，结果仍然未知**：在同一个事务里把运行写为 `failed`、`done`，Thread 为 `ended`，交付状态为 `failed`，并写入一行
  **残留账本** `agent_run_residuals`。Cloud **不伪造** Node 结果、收据或 Revision。

## 残留账本

`agent_run_residuals` 记录一个业务上已经结束、但外部执行和资源仍可能存在的运行：

- 账本行记下租户、Project、Workspace、运行代次，以及当时所有相关的执行、沙盒、Node、操作、effect 和 clone 的身份，只包含资源身份和
  未完成的责任，不包含凭据或业务输入。
- 写入账本的同一事务关闭新的派发、用户输入和模型授权：Workspace 准入关闭，运行时控制进入 `reconciling`，运行的模型绑定和已签发的
  模型授权被撤销。Workspace 保留，普通删除和接管都被拒绝（`409 resource_in_use`）。
- **迟到的事实只补证据。** 之后到达的真实记录和终态按原执行身份接管并确认，让 Node 停止重放，但不会复活运行、不会再次结算，也不会
  当作资源已经删除。
- **重连不等于回收。** Node 重新连上可以恢复连接和就绪事实，但只要账本行没有清除，Workspace 的准入就保持关闭；同一路径对没有残留
  的正常运行照常恢复准入。
- 授权的运维人员可以用受控的数据库会话按 `tenant_id`、`run_id` 查询账本。`session_ended_at` 记录迟到的真实终态，它是证据，不是
  删除证明；`cleared_at` 保留给以后经批准的回收流程，目前不会被自动设置。

业务上的 `done` 不等于物理资源已经释放；只有以后的回收决策定义了授权、真实终止、迟到创建的隔离和删除顺序之后，才会开放自动回收。

## 创建 blocked 的有界放弃

运行 Workspace 还没就绪时也可能一直等下去：它的 `create_workspace` 操作结果不明，停在 `blocked`。运行 Workspace 对公开 API 不可见，
没有人能手动重试，而 `blocked` 的操作还占着这个 Project 唯一的操作槽，所以只能由 Cloud 的扫描（`GiveUpBlockedWorkspaceCreationsOnce`，
每 10 秒一次）收尾。

| 配置（`issue_runs.*`） | 默认 | 起点 |
|---|---|---|
| `agent_workspace_blocked_timeout` | 30 分钟 | 创建操作第一次持久写入 `blocked` 的事务 |

- 截止时间在第一次写入 `blocked` 的同一事务里冻结（`workspace_blocked_at`、`workspace_deadline_at`，来源记为 `blocked_write`）。延后、重试、
  重新领取、Cloud 重启或配置调整都不续期；临时回到 `running`、`retry_wait` 也不会抹掉它；还在排队、没拿到 Project 操作槽的创建不起算。
  配置的校验规则和启动窗口相同。
- 迁移 `0038` 之前已经 `blocked` 的创建没有不可变的首次证据，不回填；第一次扫描时以观察时间为起点给一个默认窗口，来源记为
  `first_observed`，不追认原先的等待已经计时。
- 到期时创建仍没有真实终态，就在一个事务里一起完成：写入残留账本（`reason = workspace_unavailable`，带上尚未回报的 effect 计划和
  clone、插件执行身份）；关闭 Workspace 准入并递增 epoch；撤销模型绑定和授权；丢弃排队的用户轮次；运行写为 `failed`、`done`，
  `failureReason = workspace_unavailable`，交付状态 `skipped`；原创建操作写为 `failed`（`provisioning_result_unknown`），从而释放 Project
  的操作槽，同 Project 的其他 Workspace 可以继续。任何一步写入失败都整体回滚，不会先释放操作槽。
- 不声明删除：残留 Workspace 的删除，以及涉及它的 Project 删除，都由已有的残留检查拒绝。取消不重置窗口：创建先真实结算的，沿原来的
  取消与释放路径；截止先到、结果仍然未知的，按 `failed`、`done` 收尾。
- **迟到的事实经 `RecordProvisioningFact` 补记。** 放弃之后，Controller 用当前的服务身份和租约提交迟到的真实事实（effect 的 Substrate
  结果、clone 或插件执行的结局、创建本身的结果）。Cloud 按原操作、effect、执行身份校验，经正常路径各自的校验器幂等写入原记录和账本
  后才确认：相同事实重放是空操作；`running`、`absent`、`pending` 这类非终态观察可以被后来的真实事实接管，原观察保留为先前证据；已经
  记录的终态只接受重放，矛盾的事实被拒绝。它不重开准入、不回到 `starting`、不再次结算，也不新建执行；领取、延后、推进、计划和
  effect 结果这些操作自身的动作对已放弃的创建保持关闭。

物理终止、迟到 ensure 的隔离、授权回收和删除顺序，仍然由以后的回收决策承接。

## 相关决策

- [创建结果未知与 Agent 启动都有等待上限，业务收尾保留残留责任](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/20261012-bounded-agent-startup-and-undispatched-withdrawal.md)
- [Issue 上 @Agent 的 IssueRun 由 Cloud 编排](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/0-agent-run-in-disposable-isolated-workspace.md)
