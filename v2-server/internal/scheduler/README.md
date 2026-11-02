# 定时任务编排

本包将配置、服务端时间计算、触发记录和执行适配连接起来。`main.go` 将公共 `executionruntime.Service` 注入协调器与 `Host`，用一秒 ticker 驱动周期扫描，HTTP 的立即运行/取消共用同一 `Host`；未注入执行服务时由不可用 `Catalog` 门禁阻断。

`NewHost(records, coordinator, catalog)` 不启动 goroutine；进程在公共执行完成启动恢复后运行 `Run(ctx, ticks)`，首次扫描完成时 `Ready()` 关闭。Run 只允许一次，扫描发现持久配置的有效创建者，暂停的配置也保留收尾机会。每个 Scope 先读公共模型目录确认可用，再 Recover/Tick；目录不可用时不创建或改写 occurrence，不猜测默认模型。HTTP 操作也经过这道门禁。停止时取消 Run、等待扫描及已进入的 HTTP 操作，然后才关闭公共服务和数据库；HTTP 调用同时受请求取消、宿主退出和 30 秒调用上限约束，该上限不是已接收执行的运行时限。

## 调用顺序

1. 宿主先完成公共执行服务的启动恢复，旧活动轮次标 interrupted、旧会话结束后，使用 `server.NewScheduleCoordinator(records, options, now)` 接同一 `server.Options.Execution`。它内部创建一个 ConversationAdapter 和 Coordinator，不启动 goroutine；内部测试也可直接调用 `NewCoordinator(records, runner, now)`。
2. 对每个明确的租户/用户 Scope 调用 `Recover`。持久恢复失败时该 Scope 不允许 Tick 或 Manual。旧 recorded 标为重启跳过；可能已接收的记录转为 reconciling，保留身份和占位，只调用执行适配的 Reconcile，不调用 Dispatch。公共读取不可用时返回错误，但已持久恢复的 Scope 可以处理其他任务；未解决记录继续防止本任务重叠。
3. 调用一次 `Tick` 发现配置并安排未来时刻，随后由宿主按需要继续 Tick。首次发现、版本变化或暂停后恢复都严格从当前时刻往后计算，不回补此前的执行点，间隔仍以任务创建时间为锚点。
4. Tick 先核对未释放记录，再扫描每个当前配置。到点写入固定计划时刻的记录，仅 recorded 状态交给 Dispatch。已跳过或已接收的记录不重新派发。一个任务的执行错误会返回给宿主，同时继续处理其余任务。

没有持久扫描游标或多副本选主。一个 Coordinator 负责本进程调度；每个 Scope 的 Tick/Manual/Recover 串行，不同 Scope 独立。若进程存活但扫描延迟跨过多个周期，只处理最近一个到期时刻，丢弃更旧的积压；重建 Coordinator 后只安排未来。后续时刻从当前时间计算，不因 Busy、手动运行或失败改变原计划锚点。

游标在写入与执行前推进。写入或执行返回错误时，Tick 不重新创建身份或自动重放同一触发。对可能已接收的执行，后续只按原记录核对。该选择偏向不重复执行，不能解释为故障期间保证每次触发都执行。

## 手动与取消

`Manual(scope, scheduleID, stableKey)` 要求稳定请求键。第一次保存请求时刻与配置快照，重试先查已有记录，保留原时刻、版本与身份。记录仍为 recorded（尚未完成 begin）时，可以再次交给 Dispatch 完成第一次接收；适配层重新读取状态并以原子 begin 和版本检查防重复。starting、reconciling、运行中及终态等其他状态只核对，绝不再次 Start。暂停任务可以手动请求，但同任务占位仍由数据库接口检查，不能绕过防重叠。已开始执行后的显式重跑需要新的请求键。

`Cancel(scope, occurrenceID)` 先经记录模块按当前身份读取，再交给执行适配；客户端不能直接传任意公共执行引用。取消请求/超时不构成停止或释放证明。

## 对话执行适配

`ConversationAdapter` 使用公共 `execution.Service`，不是新的 provider 或 Session/Turn 实现。从冻结配置读取执行目标，以 occurrence ID 作为公共 Caller 的 ID 和 AttemptID：

- 无项目文本对话使用 `StartModel` 与明确的模型 ID，不创建 Agent 或 Workspace；不能同时携带项目执行器 ID。
- 有项目文本对话使用 `Start`，传入冻结的 projectId 和 target.executorId（公共目录里的执行器绑定 ID）。每次触发都是独立会话，目录由公共服务创建，消费方不能选择或争抢目录。执行器名称、agent/model 展示文字不能替代绑定 ID。
- 任一路的接收错误都不会降级到另一路；缺少绑定、空提示词及工作流目标明确拒绝。工作流必须经自己的运行入口，不能在这里当作 Agent 对话运行。

配置接口和表单已经开放项目绑定：保存时按当前 `schedule-resources` 目录校验 projectId、模型与执行器绑定，触发时仍走同一个 `Start`。

## 工作流执行适配

`TargetRunner` 按冻结快照里的目标类型分派：对话交给 `ConversationAdapter`，工作流交给 `WorkflowAdapter`；没有工作流适配器时由对话适配器按不支持的目标拒绝。`WorkflowAdapter` 通过 `WorkflowRuns`（`server` 包把运行存储和执行桥 `WorkflowRunHost` 接成这个端口）在任务创建者的 scope 里执行，不复制图校验或引擎，也不直接驱动 Node：

- **Dispatch**：`begin` 之后按 occurrence ID 这个稳定身份创建运行（重复创建得到 `store.ErrWorkflowRunTriggerTaken` 时按身份找回），再启动，成功后 `accepted` 并记 `{"workflowRunId"}`。快照、工作流或项目已不存在记 `rejected`（`workflow_unavailable` / `project_unavailable`）；执行桥拒绝启动或执行不可用时取消那个未开始的运行，记 `rejected`；结果未知（超时、取消、其他错误）记 `reconcile`，交给 Reconcile 按身份查。
- **Reconcile**：按身份找运行。没有运行且没绑定过记 `not_accepted`；运行还是 pending 说明启动没有提交，取消它再记 `not_accepted`；运行中则绑定后等待；结束则记结果并释放占位（没有会话需要 End）。重启恢复（`server_restarted`）中失败的运行记 `interrupted`。
- **Cancel**：`cancel_requested` 后经执行桥取消；停止未确认时保持 `cancelling`，运行真正结束后由 Reconcile 记录。

两路共用结果、取消与释放流程。已接收的执行结果通过公共记录读取。成功必须满足 `AnswerComplete`；完整性不足不能标 succeeded。Agent Cancel 结束会话，model-only Cancel 只停止当前轮；定时任务不追问结果，两路随后都调用幂等 End。只有 End 确认后才释放同任务占位，历史和目录继续保留。

Start 是进程内同步调用。ConversationAdapter 对每条 occurrence 的操作串行；同一条正在处理时返回 `ErrOperationInProgress`，后续 Tick 可继续观察，不排队或重派。一个适配器负责全部定时对话，不能为相同记录创建并行适配器；启动恢复必须先于新派发。

调用错误或本地引用保存失败后按原 Caller `ReadAttempt`：存在时校验 Caller/Ref 并保存绑定；Start 已返回（或公共启动恢复已完成）、无已知 Ref 且查询未命中时，记 skipped/execution_not_started。读取错误或已绑定记录缺失保留占位。恢复只读，不提交 prompt；没有接收闭锁接口。相同手动键始终找回原记录，显式新键仍受同任务防重叠保护。

`ErrWorkspaceBusy` 只指同一会话/工作流运行的前一轮仍活动。每次定时对话都新建会话，正常不会遇到；若公共服务仍明确拒绝，记 skipped/execution_busy，不自动重试，也不把它解释成项目目录被其他任务占用。

## 验证边界

协调器测试使用显式时钟和记录/执行替身，验证调度选择、跨 Scope 隔离、并发扫描、防止重复手动请求、失败不自动派发、暂停恢复和重启核对门禁。记录模块的 PostgreSQL/race 验证承担实际原子接收、权限和占位约束；公共执行替身只验证消费方式，不算真实模型或完整运行时验收。

恢复测试覆盖两种对话模式、本地引用写入失败、关闭重开连接池后原身份核对、未启动记录收尾、读取错误保留占位、启动与观察并发、同项目不同任务独立会话、Cancel 后 End 失败重试和旧草稿状态升级。统一 Options 测试验证未配置 Stub 返回不可用及显式注入的消费路径。完整回答持久化、End/重启读取与真实 Agent 由下面的 opt-in 组合测试承担；普通 CI 的替身仍不冒充这些证据。

### 真实公共执行组合测试

`runtime_integration_test.go` 是 Linux 的 opt-in 验收。需要公共 `executionruntime`、`modelturn`、`agentruntime`、`projectrepos`、`nodehistory` 与实际 store/schema 实现已经组合在同一源码目录；没有这些实现时，普通 CI 不打开此 tag，不用 Stub 替代。Agent 还需通过 `scripts/prepare-node.sh` 准备的固定 Node 资产。

```sh
TEST_DATABASE_URL=... go test -race -count=1 -tags execution_integration ./internal/scheduler -run '^TestScheduledModelRuntimeRestart$' -v
V2_NODE_ASSETS_DIR=... TEST_DATABASE_URL=... go test -race -count=1 -tags execution_integration ./internal/scheduler -run '^TestScheduledAgentRuntimeRestart$' -v
```

每项测试先在子进程中经统一 Options 创建协调器和 Host，由真正运行的 ticker 扫描冻结时钟的到期配置，触发真实 fixture 执行、保存完整结果并确认 End；再经带登录 cookie 的 HTTP 立即运行（含同键重试）和取消入口，在实际 provider 边界暂停后验证 Cancel 的模式差异及 End。该进程退出后，第二个独立进程重新打开 PostgreSQL、Service 和 Host，核对两条原记录、完整结果与已结束会话，验证旧会话不可 Append、停机不补跑。Agent 路径还验证真实 shell 工具写出的文件在 End/重启后仍存在。测试配置使用已验证的持久绑定；页面上的项目选择、立即运行与历史读取由 `frontend-v2/e2e/schedule-runs.spec.ts` 在真实栈上核对。默认 fixture 不需要凭据，这不是远端真实模型 provider 验收。
