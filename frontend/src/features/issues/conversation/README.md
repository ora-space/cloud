# 任务对话

[English](README.en.md) | 中文

任务级对话读取、发送、待交接消息及 Agent 选择界面。当前会话发起者可从任务页确认正常结束；「停止当前执行」仍是取消并暂停队列。一次 Run 的 Thread 内容仍由 `issues/thread` 提供；本模块不拥有执行状态或 Node 连接。

| 文件                                             | 职责                                                                              |
| ------------------------------------------------ | --------------------------------------------------------------------------------- |
| `conversation-footer.tsx`                        | 绑定当前执行的结束、停止、恢复与常驻输入区                                        |
| `api.ts`                                         | 任务消息双向分页、发送、停止、取消/重试、恢复/放弃未交付成果 API 与安全错误文案。 |
| `conversation-panel.tsx`                         | 聚合任务评论、Agent Run 与待交接消息；只对当前占用任务的会话展示授权的结束入口。  |
| `conversation-composer.tsx`                      | 单一 Agent 目标选择、显式提及菜单、Agent 消息/仅评论模式及交接提示。              |
| `use-conversation-draft.ts`                      | 一份任务草稿及历史引用，切换 Agent 只改变输入目标。                               |
| `task-stop.tsx` / `task-recovery.tsx`            | 当前执行停止、持久暂停和保存/放弃后的明确恢复，独立于历史浏览。                   |
| `agent-readiness.tsx` / `use-composer-submit.ts` | 发送前展示当前用户模型/项目配置缺口，统一发送资格并保留失败草稿。                 |
| `*.test.ts(x)`                                   | MSW 与组件场景，覆盖终态续聊、切换、失败与队列。                                  |

依赖 `src/api` 生成客户端、`features/issues/api`、`features/issues/thread`、`features/spaces/api`、`features/conversation-content` 和 `components/ui`。只使用服务端授权后的目标名册；选择器和 `@` 菜单只在明确选择后改变接收者，普通文本中的 `@名称` 不参与路由，本地选择也不触发执行。历史选中 Run 只读；任务级发送框始终把新消息发往当前目标。结束入口只指向服务端 `activeRunId`，还须 Thread 的 `canEnd` 为真且当前用户是该会话发起者；排在后面的 Run 不能遮住当前会话的结束入口。分页按服务端 seq 合并，响应丢失后的重试保留同一个幂等键。

`conversation-footer.tsx` 将当前会话结束、任务停止/恢复和输入区固定在正文之外，操作始终绑定当前执行。正文以轻量 Run 分隔连续阅读，作者来自消息或 Thread 发起者，只有当前用户显示“你”；Agent 名称显式传给消息展示。正文借用 thread 的统一 viewport，授权拒绝后隐藏旧消息。

任务对话在 activeRunId 存在或有等待消息时继续刷新；运行完成后由服务端清空当前占用，不能因队列已空而留下陈旧的执行控制。

准备阶段失败或取消的运行可能没有交付结果。它仍保持失败暂停，页面提供同一个明确的放弃未交付成果并继续入口；服务端只有在资源释放完成、发起者或管理员获准且明确确认后才恢复队列，不自动跳过，也不把旧运行改成成功。

当前选择的个人 Agent 有配置缺口时，即使任务尚无 Run 或等待消息，也每五秒重新读取就绪状态。用户在另一窗口修复后，原页恢复发送并保留草稿；缺口消失、改为仅评论或选择其他 Agent 后不再为旧缺口轮询。权限拒绝后停止轮询，所有发送仍由服务端重新准入。

发送框在正常文档流中占位，不悬浮覆盖任务历史；长会话滚动与最新消息阅读不依赖固定高度补白。

执行前的缺口按服务端错误码给出处理入口：旧插件缺少个人配置声明、插件未绑定模型或声明变更时前往插件设置；连接、凭据和默认模型缺失时前往个人模型连接。部署未启用 adapter 时提示联系部署管理员，不把所有拒绝误报成临时故障或重复配置模型。

连接不可用、缺 Key 和所选模型已撤下分别消费服务端的 `model_connection_unavailable`、`model_credential_required`、`model_not_found`；不将工作流解析器的内部原因码当作任务对话协议。未配置插件模型选择使用 `agent_configuration_required` 并保留插件设置入口。

任务消息、评论和 Run 用稳定标识标记对话输出，Thread 自行标记尾部序号/实时版本；统一 viewport 据此区分历史前插与真正新增的输出，翻页按钮和历史展开不会误报新内容。

默认对话的发送给和 @ 菜单区分 Agent 与工作流。工作流选择只打开 workflows 的既有版本表单，审阅后 Confirm 才创建运行；取消保留 Agent 草稿和重试身份。conversation-workflow.tsx 组合既有表单，workflow-run-card.tsx 读取同一 IssueRun 关联的 WorkflowRun 状态、冻结版本和详情链接；历史选择不读 Agent Thread。所有类型共用展示顺序，Agent 发送/停止/交接控制仍只取 Agent 运行，不建立混合队列或成果交接。依赖增加 features/issues/components/workflow-interaction-composer 与 features/workflows/api。conversation-workflow.test.tsx 覆盖选择无写入、取消、Confirm 以及工作流历史身份。
