# issues/thread：Issue 页的 Agent 会话面板

[中文](README.md) | [English](README.en.md)

## 职责

Issue 详情页右侧的「Agent 会话」栏：选出该 Issue 最近一次 agent run，读取它的 Thread（`/runs/:rid/thread`），双向分页显示，接收空间事件实时刷新，并提供发送消息与结束会话。

不负责：run 的创建与列表（`features/issues/api` 的 `useRuns`）、SSE 连接本身（`features/spaces/use-space-events`）、Issue 其他栏目。

## 文件

| 文件 | 说明 |
| --- | --- |
| `thread-entries.ts` | 纯函数：选最新 agent run、按 seq 合并窗口、把 ThreadEntry 映射为显示行（合并同一 turn 的连续 agent chunk）、状态中文标签 |
| `thread-api.ts` | TanStack Query 层：`threadQueryKey`、`useThread`（尾部首读 + 增量读 + 404 等待 + 兜底轮询）、`useLoadOlderThread`、`useSendThreadMessage`、`useEndThread`、故障文案 |
| `thread-messages.tsx` | 展示组件：消息列表（`role=list`，名称「会话消息」）与「加载更早」按钮 |
| `thread-composer.tsx` | 输入框「给 Agent 发送消息」、「发送」与带确认步骤的「结束会话」 |
| `thread-panel.tsx` | `IssueThreadPanel`：选 run、标题「Agent 会话」、状态徽章、等待提示与组合 |
| `run-delivery.tsx` | 会话下方的 Revision 交付一行（`role=status`，名称「Revision 交付」）：已登记的 Revision 显示短提交与是否有改动，否则按 `result.deliveryState` 说明跳过或失败，会话已结束但运行未结算时显示保存中 |
| `*.test.ts(x)` | 纯函数单测与 MSW 集成测试 |

## 依赖

依赖 `src/api`（生成的 Thread 客户端与类型）、`features/issues/api`（`useRuns`）、`features/issues/types`、`features/spaces/api`（`mutationHeaders`、`useIdempotencyKeys`）、`lib/api-client`（`faultCode`）、`components/ui`。由 `features/issues/issue-detail-page.tsx` 使用。`features/spaces/use-space-events.ts` 通过生成客户端的同一个 query key 失效本模块的查询，但不 import 本模块。

## 不变量

- 缓存里的 entries 永远是一段连续、按 seq 升序的窗口：首读取尾部，「加载更早」用 `before=最旧 seq` 前插，增量读用 `after=已见最大 seq`（若有 queued 的用户 turn，则退回到它之前，以刷新 queued→delivered）。按 seq 去重，绝不重复。
- 发送成功后**不**把返回的 entry 直接并入缓存：它的 seq 可能领先于尚未读到的条目，提前推进窗口会永久跳过它们；只靠重新读取按序拿到。
- 事件里的 `lastSeq` 只是提示，从不当作游标。
- GET 404 表示会话尚未声明，是正常的等待状态（每 2.5s 轮询），不是错误；已声明后在非 `ended` 状态下每 5s 兜底轮询，防止 SSE 掉线。
- Revision 只显示公开元数据（提交、是否有改动、大小）；运行结算没有空间事件，依赖 `useRuns` 在 agent 运行未结束时的轮询刷新。
- `ending` / `ended` 时禁用发送与结束（与服务端 409 `thread_closed` 规则一致）；发送与结束各自的幂等键在同一次提交的重试间保持不变。服务端返回 `canAppend` / `canEnd` 决定操作：只有发起者可追加模型请求，管理员可结束，其他成员只读。面板展示冻结的连接/模型摘要和成员显示名称，不显示密钥。

模型授权失效后，旧运行保持只读；重新配置连接不会重新开放旧会话，页面提示结束后重新发起任务。

## 测试

`thread-panel.test.tsx` 用一个具有服务端游标语义（tail / after / before）的假 Thread 后端驱动面板，覆盖尾部加载、404→出现、加载更早、发送（含 thread_closed）、结束会话以及 SSE 事件触发增量读取。404→出现依赖真实的 2.5s 轮询，单条用例约 3 秒。
