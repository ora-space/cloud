# Agent 启动与未知结果的有界业务收尾

[English](agent-startup-settlement.en.md) | 中文

本实现遵守已批准的 bounded-agent-startup ADR D1–D4。工作项/首条 prompt 是 Cloud 意图，登记意味着 Node 可能已执行；无回复不能证明没有外部副作用。D5 创建 blocked 放弃单独交付，不在本期启用。

## 持久窗口

| 配置（环境变量前缀 CLOUD_ISSUE_RUNS_） | 默认 | 起点 |
| --- | --- | --- |
| agent_start_timeout | 2m | 运行 Workspace 创建成功/ready |
| agent_restore_timeout | 6m | 同一 ready，替代普通窗口，包含 Revision 恢复和启动 |
| agent_end_confirm_timeout | 5m | 启动中已登记未知执行首次 ending |

缺省使用默认值，显式零、负值、非时长及溢出拒绝启动。0034 追加持久 ready/deadline 与恢复选择，普通和恢复使用一个绝对启动截止；声明、输入、重试、重启或配置调整不续期。恢复候选沿既有同 Issue/Project/仓库和 refused 规则选择，在 ready 固定以确定窗口，首轮声明复用该选择。

存量未初始化窗口的 starting Run 在串行扫描中一次固定：有成功 create_workspace 证据时使用其完成时间，否则明确以首次策略观察为 ready 起点；已有声明复用工作项的冻结恢复输入。它不声称为缺少证据的历史运行追溯恢复精确期限。历史 D8 删除放弃状态不改写。

## 未登记与已登记

未登记时，当前已初始化、连接且 30 秒内心跳的 Node 明确缺模型能力才以 model_proxy_unavailable 失败。容量、排队和暂时不可达继续保留原 work/输入，由已有领取调度在原预算内延后；本实现不新增队列或退避策略。到期原子撤回并以 agent_start_failed/releasing 结算，用户先结束则 cancelled。撤回与登记共用事务锁，终态、queued 输入丢弃和 owned delete 声明一起提交。

已登记且无启动记录时，到期请求 ending，保留既有用户结束原因和所有执行/资源证据。真实终态及时到达，沿原交付链路处理；确认截止仍未知则原子写 failed/done/ended、deliveryState=failed 和 agent_run_residuals，不伪造 Node 结果、收据或 Revision。关闭新派发/输入/模型授权，保留 Workspace 并阻止普通删除或接管。迟到真实记录和终态按原身份接管及 ACK，只补证据，不复活或再次结算。

## 运维查询与回收边界

Node 重连可以恢复连接与 ready 事实，但未清除的残留账本继续关闭 Workspace admission。连接恢复不构成重新受理或物理回收证明；同一路径对没有残留的正常运行仍恢复受理。

授权部署运维使用受控数据库会话，按 tenant_id/run_id 查询 agent_run_residuals；其中仅记录资源身份和未完成责任，不包含凭据或业务输入。session_ended_at 是迟到真实终态的证据，不是删除证明。cleared_at 保留给后续经批准的回收流程，本期不会自动设置。

账本的 tenant/project/workspace/generation 与既有 operation/effect/clone/Node execution 身份一起保留。隐藏 Workspace 的公开入口继续不可用；只有后续回收 ADR 定义授权、真实终止/late ensure fencing 和删除顺序后才能开放自动回收。业务 done 不等于物理资源已释放。

直接回归覆盖固定窗口、配置拒绝、两 Store 配置变化、延后 work、重复扫描、账本失败回滚、迁移和真实 HTTP/gRPC 迟到事实。完整真实 Controller 暂时失败调度与永久失联演练仍须和领取调度、部署验收对齐。
