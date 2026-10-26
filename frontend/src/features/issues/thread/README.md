# issues/thread：Issue 页的 Agent 会话面板

[中文](README.md) | [English](README.en.md)

几何变化只负责保留锚点和底部跟随；未读提示由 `data-conversation-output` 标记决定：已有 Thread 的尾部 seq/实时预览版本改变，或在既有末尾之后新增任务消息/Run，才算新输出。加载更早页、历史 Run 的延迟加载和折叠/布局变化不会制造新内容提示，也不会清除此前确有的新内容提示。

## 职责

运行中的 Thread 同时依赖 `features/thread-preview` 的可选快照。预览按历史位置覆盖，已定型记录始终优先；流式思考展开，定型后折叠。连接状态与部分缺失明确展示，预览更新保留历史阅读位置。

Issue 详情页主体的「Agent 会话」：默认读取最近一次 Agent Run，也接受任务页 URL 明确选择的历史 Run，读取其 Thread（`/runs/:rid/thread`），双向分页显示，接收空间事件实时刷新。任务级组合以只读模式展示各 Run，发送交给任务统一输入框；当前占用任务的会话可单独开放带确认步骤的结束控制，历史会话不开放。独立会话组件保留原有发送/结束控制，供旧服务升级期间使用。

不负责：run 的创建与列表（`features/issues/api` 的 `useRuns`）、SSE 连接本身（空间事件由 `features/spaces/use-space-events`、预览由 `features/thread-preview` 拥有）、Issue 其他栏目。

## 文件

| 文件 | 说明 |
| --- | --- |
| `conversation-viewport.tsx` | 任务统一滚动、阅读锚点与新内容提示 |
| `message-author.tsx` | 执行者名称、作者头像与真实时间 |
| `thread-entries.ts` | 纯函数：选最新 agent run、按 seq 合并窗口、把 ThreadEntry 映射为显示行（合并同一 turn 的连续 agent chunk）、状态中文标签 |
| `thread-api.ts` | TanStack Query 层：`threadQueryKey`、`useThread`（尾部首读 + 增量读 + 404 等待 + 兜底轮询）、`useLoadOlderThread`、`useSendThreadMessage`、`useEndThread`、故障文案 |
| `thread-messages.tsx` | 展示组件：消息列表（`role=list`，名称「会话消息」）与「加载更早」按钮 |
| `thread-row.tsx` | 安全 Markdown、折叠执行上下文、思考和可展开的工具输入输出 |
| `thread-composer.tsx` | 输入框「给 Agent 发送消息」、「发送」与可供任务页复用的确认式「结束会话」 |
| `thread-panel.tsx` | `IssueThreadPanel`：选 run、标题「Agent 会话」、状态徽章、等待提示与组合 |
| `run-delivery.tsx` | 会话下方的 Revision 交付一行（`role=status`，名称「Revision 交付」）：已登记的 Revision 显示短提交与是否交付了新提交（续接后无新提交时说明沿用了续接的成果）；Revision 经 Git 交付，不显示 bundle 大小，否则按 `result.deliveryState` 说明跳过或失败，会话已结束但运行未结算时显示保存中 |
| `run-resume.tsx` | 会话上方的续接一行（`role=note`，名称「续接」）：运行续接了同一 Issue 之前的 Revision 时，从该 Issue 的运行列表中找到它并显示短提交；全新运行不渲染 |
| `run-preparation.tsx` | `RunPreparation` 显示服务器提供的环境、克隆、Agent 准备和启动阶段，以及三次克隆预算、重试等待；`snapshot_not_ready` 显示为等待项目首次同步仓库（等待而非失败），`repository_sync_failed` 无论阶段都显示为失败，并附上形如 snake_case 的 `syncErrorCode`（其他值丢弃）；`SessionFailure` 仅显示白名单安全错误码，失败不继续显示等待或空闲 |
| `thread-failure.ts` | `sessionFailureMessage`：状态提示与持久失败条目共用的有限错误分类文案；Agent 模型配置的几类失败（缺少、不一致、协议不支持、授权不符、adapter 未应用、节点不支持）各有文案；未知字段替换为固定提示，不显示供应商诊断 |
| `*.test.ts(x)` | 纯函数单测与 MSW 集成测试 |

## 依赖

依赖 `src/api`（生成的 Thread 客户端与类型）、`features/issues/api`（`useRuns`）、`features/issues/types`、`features/spaces/api`（`mutationHeaders`、`useIdempotencyKeys`）、`lib/api-client`（`faultCode`）、`components/ui`。由 `features/issues/issue-detail-page.tsx` 使用。`features/spaces/use-space-events.ts` 通过生成客户端的同一个 query key 失效本模块的查询，但不 import 本模块。

## 不变量

Node 条目公开其真实 `turnId`，工具调用与更新优先按该轮次关联，不能把已排队的下一条用户输入当作正在回复的轮次。旧响应缺少 Node 轮次时使用 `session-data` 的用户边界标记；完全没有轮次依据的调用保持独立，不能跨 Thread 合并。

显示层依赖 `features/session-data` 的公开解析器和 `features/conversation-content` 的安全 Markdown。工具更新按 turn/call 身份合并，缺少身份则保持独立；未知耗时不猜值。明确选择的历史 Run 缺失时显示错误，不能回退到最新执行；没有 Thread 的终态 Run 不继续轮询等待声明。阅读历史时新消息只提示，不抢滚动；前插历史页按高度差保持锚点。

- 缓存里的 entries 永远是一段连续、按 seq 升序的窗口：首读取尾部，「加载更早」用 `before=最旧 seq` 前插，增量读用 `after=已见最大 seq`（若有 queued 的用户 turn，则退回到它之前，以刷新 queued→delivered）。按 seq 去重，绝不重复。
- 发送成功后**不**把返回的 entry 直接并入缓存：它的 seq 可能领先于尚未读到的条目，提前推进窗口会永久跳过它们；只靠重新读取按序拿到。
- 事件里的 `lastSeq` 只是提示，从不当作游标。
- GET 404 表示会话尚未声明，是正常的等待状态（每 2.5s 轮询），不是错误；已声明后在非 `ended` 状态下每 5s 兜底轮询，防止 SSE 掉线。
- Revision 只显示公开元数据（提交、是否有改动）；Git 交付的 Revision `bundleSize` 为 null，交付行从不显示大小；运行结算没有空间事件，依赖 `useRuns` 在 agent 运行未结束时的轮询刷新。
- `ending` / `ended` 时禁用发送与结束（与服务端 409 `thread_closed` 规则一致）；发送与结束各自的幂等键在同一次提交的重试间保持不变。服务端返回 `canAppend` / `canEnd` 决定操作：只有发起者可追加模型请求，管理员可结束，其他成员只读。面板展示冻结的连接/模型摘要和成员显示名称，不显示密钥。
- 任务页只对 `activeRunId` 指向、`canEnd` 为真且当前用户是发起者的存活 Thread 单独展示结束控制；它调用正常 Thread end，不调用任务停止，后续队列不因此暂停。独立 Thread 面板原有管理员结束权限不变。

模型授权失效后，旧运行保持只读；重新配置连接不会重新开放旧会话，页面提示结束后重新发起任务。

任务存在交接屏障或暂停时，服务端同样关闭旧 Thread 的追加权限。旧客户端直接发送会收到 `409 task_handoff_pending` 或 `409 task_paused`；后续消息使用任务对话队列，不越过等待交接的消息。

## 测试

插件准备失败时保留服务端的 `agent_plugin_unavailable` 分类，提示检查插件安装状态后重新发送；未知错误仍退回固定文案，不展示下载器或供应商原始诊断。

`run-preparation.test.tsx` 直接渲染各准备状态，覆盖快照等待、仓库同步失败及其错误码过滤。`thread-panel.test.tsx` 用一个具有服务端游标语义（tail / after / before）的假 Thread 后端驱动面板，覆盖尾部加载、404→出现、加载更早、发送（含 thread_closed）、结束会话以及 SSE 事件触发增量读取。404→出现依赖真实的 2.5s 轮询，单条用例约 3 秒。

`conversation-viewport.tsx` 为任务内嵌 Thread 提供唯一滚动容器，观察内容大小并保持可见消息锚点；独立 Thread 仍使用原分页/跟随行为。`message-author.tsx` 展示真实作者与可用时间。内嵌消息不拥有滚动条，身份与模型信息可折叠；所有原始内容、失败及交付信息保留。浏览器几何验收补足 jsdom 无布局引擎的边界。
