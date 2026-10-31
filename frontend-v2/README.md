# frontend-v2

Cloud v2 的正式 Web 前端，中文界面。技术栈：Vite、React 19、TypeScript、Tailwind CSS 4、shadcn（base-nova）、TanStack Query、zustand、React Router，客户端由 orval 从 `api/openapi.json` 生成。

## 开发

需要 Node 24、Go、Docker（跑 PostgreSQL）和 [Task](https://taskfile.dev)。在仓库根目录：

```sh
task v2:dev    # PostgreSQL + v2-server（:8090）+ 前端（http://localhost:5173）
task v2:e2e    # Playwright 端到端测试
```

打开页面后在登录页填预置账号的邮箱（默认 owner 是 `owner@demo.ora-space.dev`，见 `v2-server/users.json`）即可进入；没有密码，只用于演示。只改前端时也可以在本目录：

```sh
npm ci
npm run dev           # /api、/auth、/healthz 代理到 v2-server（V2_SERVER，默认 :8090）
npm run check         # 类型检查、lint、单元测试、构建；CI 的 v2 workflow 跑同一条
npm run api:generate  # api/openapi.json 变化后重新生成 src/api
npm run e2e           # 端到端测试，自己拉起 v2-server（:8091）和前端（:5174）
```

端到端测试默认连接 `task v2:db` 的 PostgreSQL（库 `ora_v2_e2e`，可用 `E2E_DATABASE_URL` 指定）。第一次运行前执行 `npx playwright install chromium`；本机已有 Chromium 时，也可以用 `PW_CHROMIUM_PATH` 指定可执行文件。

默认不运行真实 Agent，和 CI 一样。设了 `V2_NODE_ASSETS_DIR`（先 `task v2:node:prepare`）后，对话的端到端测试改为在真实 Node 和 Agent 上跑 fixture 模型：新建对话等到真实回复，停止另一个对话后只读，刷新后历史一致。项目第一次启动 Node 要装插件，整条用例约一两分钟。`V2_DATA_DIR` 要用短路径（如 `mktemp -d /tmp/v2e2e.XXXX`），否则 Node 的 Unix socket 路径会超长；也别和正在运行的 `task v2:dev` 共用同一个数据目录。

## 结构

```text
src/
  app/          外壳：Provider、路由、布局、左侧图标栏、登录（auth/）
  features/     每个页面一个目录：chat、collaboration、workflows、schedules、dashboard
  api/          orval 生成的客户端，不手改
  components/   跨页面共用的组件：左栏拖宽手柄（sidebar-resize-handle）；ui/ 是 shadcn 生成的组件，不手改
  lib/          跨页面共用：HTTP 实例（api-client）、登录接口（auth-api）、代码高亮（highlighter，shadcn 生成）、细滚动条样式（scrollbar）
  hooks/        shadcn 生成的 hook
  test/         单元测试的公共设置和假后端
e2e/            Playwright 端到端测试
```

左侧图标栏的每个图标对应 `src/features/<name>/` 里的一个页面。新增页面：

1. 新建 `src/features/<name>/index.ts`，导出 `Feature`（路径、标题、图标、按需加载的页面）；页面写在同目录的 `page.tsx`。
2. 在 `src/app/features.ts` 里按导航顺序加一行。

各页面互不引用，可以并行开发；页面首次访问时才加载，构建时各自拆包。

## 对话

`src/features/chat/` 是对话页，界面对齐 desktop 的对话界面。组件用 shadcn 的 chat 组件（MessageScroller、Message、Bubble、Trace、Code Block、Attachment、Marker），复制进来的 AI Elements pattern 放在 `components/`。Code Block 的高亮依赖 shadcn 一起生成的 `src/lib/highlighter.tsx` 和 `src/hooks/use-highlighted-code.ts`。

- `model/`：把 ACP 事件（`session/update`、权限请求、轮次结束）归约成界面条目，再排成显示用的行（活动块、子代理分组、改动文件）。后端对话的历史快照由 `model/history.ts` 每次整份重建成轮次，走同一个归约器。
- `api/`：由 `v2-server/chat.openapi.json` 生成的客户端（`npm run api:generate:chat`），不手改。
- `remote.ts`：对后端的调用和事件流（Server-Sent Events）。`store.ts`：项目、对话和事件流的状态。

左栏按项目分组：项目和对话都来自后端，对话直接列在所属项目下；拖动项目可调整顺序（存到后端），拖动左栏右边缘可调宽度或收起（存在浏览器本地）。

新对话的排版照 desktop：没有对话选中时显示新对话，标题和输入框在页面中间，输入框上方选项目和启动模式（网页上只有云端），旁边一个"沙盒"标签说明每个对话在独立的沙盒中运行（悬停有说明）；第一条消息发出时才由后端建对话，输入框滑到底部。输入框右下角先选 Agent 再选模型，清单来自后端的执行器。

后端对话的规则跟 `v2-server` 的 chat 一致（见 `v2-server/README.md` 的"对话"一节）：

- 每个对话一个执行 session，在独立的沙盒里运行：代码从项目仓库 clone 而来，改动不会写回，对话之间互不占用；顶栏同样标"沙盒"。
- 停止会结束整个 session，服务重启也会。之后对话只读：线程末尾标出原因，输入框换成"新对话"按钮，不会在同一对话里重开 session。
- 上一轮还没结束时发的消息进入排队；后端如果先一步拒绝（`turn_running`），消息回到队首，等这一轮的结束事件再发。
- 只能发文字，对话不能换执行器；新建项目等后端支持后再开放（`backendSupports`）。
- 没有在看、也没有在等回复的对话会关掉事件流，再打开时从上次的序号接着读。

后端还不能运行 Agent 时，新对话上方会提示。

### 运行工作流

工作流的运行照 desktop 放在对话页里（`workflow-run/`）：项目行 `+` 菜单里的"运行工作流"列出服务端已发布的工作流（每次打开菜单时读取，顶部有搜索框，列表过长时滚动），选一个后给这次运行起名，创建出一次未开始的运行。运行和会话一起列在项目下面，带状态点；选中后右侧换成运行页，选会话或新建对话就换回来。

运行页和 desktop 一样有两个视图，标题栏切换：

- **舞台**：上方是执行路径，下方一次演一个节点的卡片，跟着正在执行的节点走；并行、条件、循环和迭代各有自己的展示。卡片右上角打开节点详情（Agent 配置、执行时间、成果、失败重试），右下角打开节点会话。运行结束后显示结果卡片。运行中进入运行页、或点"启动"后都先到舞台，Esc 回到全图。
- **全图**：冻结的整张图，每个节点显示状态；点节点回到舞台并聚焦它。

标题栏还有"启动"（先填开始节点的变量和初始提示词）、"停止"、"再跑一次"（新建一次运行，旧的留在列表里）。Agent 节点的对话只在运行页里看，不进聊天记录，desktop 也是这样。

| 文件 | 来自 desktop `packages/app-shell/src/features/workflow-run` |
|---|---|
| `run-theater*`、`run-act-*`、`run-overview-*`、`run-status-*`、`run-loop-*`、`run-condition-stage.tsx`、`run-result-act.tsx`、`run-brief-popover.tsx`、`run-hitl-composer.tsx`、`use-theater-hitl.tsx`、`run-path-structure.ts`、`run-focus.ts`、`run-node-completion.ts` 和其余同名小文件 | 同名文件，只改了 import 路径 |
| `workflow-run-start-dialog.tsx`、`translations.ts` | 同名文件 |
| `run-node-session-chat.tsx`、`conversation-turns.ts` | 新写：desktop 在这里嵌入完整的聊天视图读取节点的真实会话；演示引擎只有节点的对话记录，`conversation-turns.ts` 把它们转成对话页的轮次（发给 Agent 的消息开启一轮，之后的回复、思考和工具调用归入这一轮），再交给对话页的 `Thread` 渲染，所以节点会话和对话页的样式一致；不能在里面接着发消息 |
| `hitl-text-editor.tsx` | 新写：人工介入回答框，用纯文本输入框代替 desktop 的 TipTap 编辑器 |
| `demo-host.ts` | 新写：Theater 调用的 desktop hook（保存启动输入、回答人工介入、错误提示等），同名同参数，背后是演示引擎 |
| `run-pane.tsx`、`run-sidebar.tsx`、`demo-runs.ts` | 新写：desktop 的运行页、侧栏行和运行对话框的精简版 |

后端还不能执行工作流，现在由 desktop 的内存运行时（`src/lib/workflows/runtime/memory-workflow-runtime.ts`、`mock-run-engine.ts`）在浏览器里代替：它冻结服务端发布版本的图，按连线每 1.5 秒推进一个节点，条件节点选一条分支，Agent 节点生成演示对话和成果。引擎不记录条件选了哪条分支，`demo-runs.ts` 按引擎同样的选路规则推出来，再像 desktop 读取后端运行时一样，把没走的分支上的节点标为未执行，所以舞台的执行路径不列它们，运行结束时进度是满的。所以运行页标着"演示运行"。运行只存在当前页面里，刷新后清空。人工介入的界面已经搬过来，但编辑器和 desktop 一样不提供人工确认节点，演示里不会出现；从失败处恢复、AI 诊断、会话索引和文件变更评审没有搬。接入真实执行后，把 `demo-runs.ts` 和 `demo-host.ts` 换成调接口的实现。

## 工作流

`src/features/workflows/` 是工作流页，从 desktop（`ora-space/desktop` main `cab59a3c`）的工作流编辑器原样移植：左栏是工作流列表（新建、复制、重命名、删除、导入、导出），右边是 React Flow 画布、节点检查器、撤销重做、草稿自动保存、发布和版本历史。图结构就是 desktop 的工作流文档，导出的文件和 desktop 互通。

对话页也要读工作流库、运行工作流，所以编辑器以外的部分放在 `src/lib/workflows/`，两个页面共用；只有工作流页用的留在 `src/features/workflows/`。

| 目录 | 来自 desktop |
|---|---|
| `features/workflows/editor/` | `packages/app-shell/src/features/workflow-editor`（`translations.ts`、`workflow-transfer.ts` 在 `lib/workflows/editor/`） |
| `features/workflows/composer-core/`、`prompt-composer/` | `packages/editor` 的 composer 和 app-shell 的提示词编辑器（tiptap） |
| `lib/workflows/node-chrome/` | `packages/app-shell/src/features/workflow-node-chrome` |
| `lib/workflows/mock/`、`runtime/` | `packages/workflow-mock`、`packages/workflow-runtime` |
| `lib/workflows/data/workflows.ts`、`data/workflow-analysis.ts` | `packages/app-shell/src/state/data` |
| `lib/workflows/contracts/dto/` | `packages/contracts` 生成的 DTO 类型 |

复制来的文件只改了 import 路径、几处 v2 的 lint 或 TypeScript 设置不接受的写法，以及几处网页上的差别：编辑器标题栏去掉了返回按钮（页面上没有可返回的地方）；草稿在页面刷新或关闭（`pagehide`）时立即补存，不等 1 秒的防抖；列表用共用的细滚动条。desktop 依赖的宿主能力在 `lib/workflows/host/` 里换成浏览器实现：`ui.ts` 转出 v2 生成的 shadcn 组件；`i18n.ts` 只加载 desktop 的 zh-CN 文案；`platform.tsx` 用浏览器下载代替保存对话框；`window-chrome.tsx` 去掉了 desktop 的窗口拖拽和窗口按钮。页面自带一个 QueryClient，沿用 desktop 的缓存策略（`staleTime` 30 秒），编辑器的缓存补丁和自动保存依赖这一点。

左栏和对话页的聊天记录用同一个拖宽手柄（`src/components/sidebar-resize-handle.tsx`）和同一种细滚动条（`src/lib/scrollbar.ts`）：拖动右边缘调宽度，拖过最小宽度就收起，也可以聚焦边缘后用方向键调整、按回车切换；收起后从编辑器标题栏的按钮展开。宽度和是否收起保存在浏览器本地（`lib/workflows/host/ui-store.ts`）。提示条右上角有关闭按钮。

`lib/workflows/samples/` 是样例工作流：`authored.ts` 写了两份，代码审查（开始 → Agent → 条件分支 → 两个 Agent → 变量聚合器 → 输出）和逐段翻译（迭代）；另外三份是 desktop 自带的 Loop 样例（`artifacts/workflow-loop-examples/`），原样复制，导入时把 Agent 改成演示执行器、去掉角色。五份合起来覆盖全部八种可执行节点。工作流库是整个团队共用的，所以样例不再自动加入：左栏标题旁的"添加样例工作流"按钮按导入对话框的同一条路径把它们加进库、发布为 `v1`；同名的跳过，发布被拒绝的留作草稿并提示。

工作流存在 v2-server 上（`v2-server/workflows.openapi.json`，客户端由 `npm run api:generate:workflows` 生成到 `lib/workflows/api/`，不手改）。`data/contracts-client.ts` 把 desktop 编辑器调用的 14 个定义操作接到生成的客户端上：服务端的毫秒数转成 DTO 的 `bigint`，拒绝转成编辑器按 `errors.<code>` 本地化的 `ContractError`，所以编辑器本身不用改。页面按当前用户的团队建客户端和查询缓存，换团队时整页重建。发布要服务端能检查图；服务端还不能检查时，发布返回的错误照常提示，不当作成功。

`data/local-workflow-backend.ts` 照搬 desktop Rust 实现的规则（名称去空格后不能为空、不能重名；一个可编辑的 `draft` 加不可变的发布版本；不能删除草稿和当前生效的版本等），现在是单元测试的假服务端（`lib/workflows/test/workflow-server.ts`）。

Agent、模型、角色、Skill 和 MCP 清单仍是 `data/demo-catalog.ts` 里的演示数据。工作流页本身不运行工作流，运行在对话页里（见"运行工作流"）。

## 设置

主导航的「设置」包含成员、模型连接、主题、Plugins、Skills 五个独立子页面，默认进入成员页。成员页读取真实的 `GET /api/v1/tenants/{tid}/members`，owner 还能填邮箱邀请（`POST /api/v2/tenants/{tid}/members`，契约之外，见 v2-server README）；主题真实作用于整个 V2；模型连接、Plugins、Skills 仍是清楚标注、按登录用户隔离的本机演示数据。没有真实 API Key、模型调用或插件下载，也不改变聊天和工作流的资源。详情见 [`src/features/settings/README.md`](src/features/settings/README.md)。

端到端测试可通过 `E2E_WEB_PORT`、`E2E_SERVER_PORT` 指定独立端口，默认仍为 5174、8091。与其他应用共用机器时选择空闲端口；测试不会复用已启动的服务。

## 定时任务

`src/features/schedules/` 通过列表和右侧抽屉管理服务端持久配置。直接对话从共享目录选择真实模型、项目和执行器，保存时由后端校验绑定；无项目对话使用模型，有项目对话使用所选项目和执行器。页面不提供示例项目、内存演示配置或模拟运行。

页面已接入立即运行、取消此次运行及历史分页和结果读取。手动运行使用稳定请求键，重试不创建替代执行；取消未确认时保留原状态。历史按时间倒序展示，完整答案和部分结果分别标识，配置暂停不取消已经开始的执行。

工作流和已有任务的真实目录/执行入口尚未接通时，新增入口禁用；已保存的旧引用只读保留，不从工作流编辑器的演示库生成参数或执行请求。职责边界、候选契约和验证范围见 [定时任务模块说明](src/features/schedules/README.md)。

## 登录

和正式 Cloud 一样，以 `GET /api/v1/me` 判断是否登录：`401` 时在当前位置显示登录页，登录后回到原页面；`403` 表示账号已停用。登录页按 `GET /auth/providers` 显示登录方式：v2-server 只提供 `email`，表单填邮箱，`POST /auth/login` 带上邮箱后跳转到返回的地址；不在演示团队里的邮箱得到 `email_not_invited`，页面据此提示找 owner 邀请。其他 provider 仍按按钮呈现，接入正式 Gateway 时前端不用改。

当前用户的角色不在 `GET /api/v1/me` 里（契约禁止额外字段），由 `GET /api/v1/me/tenants` 的 `role` 得出，见 `app/auth/user.ts` 的 `useRole()`；邀请入口只对 owner 显示。
