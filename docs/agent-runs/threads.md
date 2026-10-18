# Thread：Agent 运行的对话

[English](threads.en.md) | 中文

每个 Agent IssueRun 恰有一个 Thread。它是 Agent 会话记录在 Cloud 里的在线副本，也是用户与 Agent 继续对话的入口：Node
产生的会话记录逐条流入 Cloud 成为 Thread 条目，用户在 Thread 里追加的消息作为新的用户轮次交给 Agent。会话结束时随
Revision 保存的会话 JSONL 是归档，Thread 是实时视图。运行的整体生命周期见 [Agent IssueRun](issue-runs.md)。

## 条目

Thread 条目存在 `thread_entries`，每条有一个 Cloud 分配、在这个运行内连续的序号 `seq`：

| `source` | `kind` | 来源 |
|---|---|---|
| `system` | `user_turn` | 首条提问：会话启动时由 Cloud 按运行创建时固定的上下文快照渲染（Issue、任务、上下文引用，以及续接信息） |
| `user` | `user_turn` | 用户在 Thread 里追加的消息 |
| `node` | `meta`、`update`、`turnEnded`、`agentSwitched`、`handoffDelivered`、`gap` | Node 的会话记录，`kind` 取记录自身的类型标签 |

- **Cloud 不解析会话记录的业务字段。** 记录格式归 desktop 的 `ora-history` 所有；Cloud 只读取顶层的类型标签并按已知集合校验，
  不认识的标签会让整批接管回滚，而不是被 Cloud 自己编一个类型。记录原样保存，单条至多 256 KiB。
- `seq` 是 Cloud 的对话顺序，与 Node 的序号、SSE 事件或执行 ID 都无关。条目里的 Node 执行 ID 和 Node 序号只在内部使用，不出现在
  公开文档里。

## 用户轮次

交给 Agent 执行的每一条输入都是一个用户轮次，身份 `turnId` 由 Cloud 生成：

```text
queued ──（Node 回显这条轮次的用户消息）──▶ delivered
   └──（会话结束时仍未执行）──▶ discarded
```

- 首条提问在会话启动的事务里写为 `seq=1`，同一事务放出会话工作项；之后的轮次由用户追加。
- Cloud 同时写一条 Thread 命令 `submit_user_turn{turnId, content}`。Node 把这条用户消息作为会话记录回显时，Cloud 把对应的轮次标为
  `delivered`，不再追加一条重复的条目；Agent 自己的回复和这一轮的 `turnEnded` 也带着同一个 `turnId`，但它们是新的条目。
- 会话终态被接管时，仍是 `queued` 的轮次一律标为 `discarded`，以后的会话也不会再执行它们。

## 追加消息

`POST .../issues/:iid/runs/:rid/thread/messages`，必须带 `Idempotency-Key`：

- 请求体 `content` 是内容块列表，目前只接受 `{"type":"text","text":"…"}`；任何其他块类型都是客户端错误，不会被静默丢弃。解码后
  的文本合计至多 64 KiB，超出返回 `400 content_too_large`。
- Thread 处于 `pending`、`active` 或 `idle`，且运行没有登记取消请求时才接受；`ending`、`ended` 或已请求取消时返回
  `409 thread_closed`。
- 使用个人模型的运行只能由**发起者**追加（`403 model_run_initiator_required`），发起者的模型连接已被撤销时返回
  `409 model_connection_unavailable`。其他运行任何当前成员都可以追加。
- 条目、Thread 状态变化、Thread 命令和幂等记录在同一个事务里提交。命令在提交之后、并且会话执行已经登记之后，才能被
  Controller 领取。

## 状态

`issue_runs.thread_state` 描述 Thread 的状态：

| 状态 | 何时进入 |
|---|---|
| `pending` | 会话已启动（首条提问已写入），还没有接管到任何 Node 记录 |
| `active` | 接管到任意 Node 记录；或写入了新的用户轮次 |
| `idle` | 接管到一个成功的 `turnEnded`（不是 `cancelled`），并且没有仍在排队的用户轮次；同时记录数据库时间 `idle_since` |
| `ending` | 用户结束、空闲期满或运行被取消，Cloud 放出 `EndSession{reason}` 命令 |
| `ended` | 会话执行的终态被接管，无论之前是否在 `ending` |

接管到第一条 Node 记录时，运行同时从 `starting` 进入 `running`。会话失败（ACP 请求错误、流意外关闭）时 Thread 直接以 `ended`
结束，Cloud 另外追加一条只含安全分类的失败条目。

## 结束

结束会话有三个触发，都只让 Thread 进入 `ending` 并放出一条 `EndSession{reason}` 命令；`ended`、交付和释放由会话终态推进：

- **用户结束**：`POST .../runs/:rid/thread/end`，带 `Idempotency-Key`，请求体为空对象。Thread 处于 `pending`、`active`、`idle` 时
  返回 `202` 和 `{"threadState":"ending"}`；已经 `ending` 或 `ended` 时返回 `409 thread_closed`。使用个人模型的运行只有发起者或
  当前管理员可以结束（`403 model_run_end_forbidden`）。
- **空闲期满**：`idle` 超过 `issue_runs.thread_idle_timeout`（默认 15 分钟）。比较完全在数据库里进行，Cloud、Controller、Node 的
  进程时钟都不参与判断。
- **运行取消**：运行登记了取消请求。取消请求优先于空闲期满，两者同时满足时以 `cancelled` 结束。

Node 收到三种原因都会停止 Agent、写完会话记录，再以终态结束会话执行。

## Controller 一侧

Thread 经 `AgentRunService` 在 Cloud 和 Controller 之间流动（见 [Controller 内部控制契约](../runtime/controller-integration.md)）：

- `TakeOverThreadEvents` 批量接管一个会话执行的 Node 记录（每批 1 到 64 条），序号必须从上次接管的位置连续接上；重复的记录必须
  与原收据逐字节一致，出现缺口或改写都不保存任何内容。业务转换只针对本次新保存的记录，并在同一事务里提交。
- `ClaimThreadCommands` 按运行和排队顺序领取尚未送达的命令，只返回会话执行已登记、未结束、沙盒仍是当前代次的那些；
  `RecordThreadCommandDelivered` 记录命令已送达这个会话执行。命令是持久的，丢掉 `ThreadCommandAvailable` 信号只会延迟领取。

## 读取与实时刷新

`GET .../issues/:iid/runs/:rid/thread` 返回 Thread 的一个窗口：

- `after={seq}` 与 `before={seq}` 互斥，都不带时返回末尾的窗口；`limit` 默认 200、最大 500；无论方向，结果都按 `seq` 升序。
- 响应带 `threadState`、`idleSince`、窗口的 `nextCursor` 与 `prevCursor`，以及发起者、可空的模型摘要和当前调用者能否追加、能否结束
  （`canAppend`、`canEnd`）。
- 授权与读取 Issue 评论相同：任何当前成员都可以读取。
- 条目只追加，所以客户端用 `after` 从已见到的最大 `seq` 继续读，既不会漏也不会重复；跨请求没有快照保证。

Thread 有变化时，Space 的 SSE 流发出 `issue_run.thread_appended`（追加了条目）或 `issue_run.thread_changed`（Thread 的 REST
表示有任何变化，包括只改状态的提交）。两者都只是提示，其中的 `lastSeq` 是高水位，不是游标；一次事务对同一个运行至多发一条
`thread_changed`（见 [SSE 失效事件流](../core/http-api.md#sse-失效事件流)）。

## 相关决策

- [Agent 运行的对话记为 Thread](https://github.com/ora-space/specs/blob/main/decisions/cloud/thread/0-durable-agent-thread-with-user-turns.md)
- [个人模型 Thread 只由发起者追加，成员可读且管理员可结束](https://github.com/ora-space/specs/blob/main/decisions/cloud/thread/20261010-personal-model-initiator-controls-input.md)
- [Internal Control gRPC 增加插件、Agent 会话与 Revision 交付三类执行，新增 AgentRunService](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260928-agent-run-executions-thread-and-upload-grants.md)
