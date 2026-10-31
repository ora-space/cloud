# v2-server

Cloud v2 的**临时**单体后端：一个进程、一个 PostgreSQL 库，实现 `api/openapi.json` 里 v2 用到的子集，供 `frontend-v2` 演示和快速端到端测试。`frontend-v2` 切到正式 Cloud 后删除。

## 运行

需要 Linux/WSL、Git、Go（版本见 `go.mod`）和 PostgreSQL。仓库根目录的 `compose.yaml` 提供本地 PostgreSQL（127.0.0.1:55432）。v2-server 默认连接其中的 `ora_v2` 库，库不存在时自动创建，启动时建表。Node 数据目录放在 Linux 原生文件系统。

```sh
docker compose -p cloud up -d --wait   # 在仓库根目录
go run .                               # 监听 127.0.0.1:8090
```

使用本地 Node 配套服务时，自签证书从服务创建起有效 24 小时，不会自动轮换。连续运行超过一天需重启后端以重新生成证书；长时间演示应在证书到期前安排重启。

收到 SIGTERM 或中断信号时，进程会取消 HTTP 请求上下文，让保持打开的对话 SSE 关闭观察并等待其读取协程退出，再结束 HTTP、公共执行服务和数据库。关闭浏览器或单条 SSE 只停止对应观察者，不取消执行 worker；进程退出时的执行停止与完整终态保存仍由公共执行服务负责。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `V2_ADDR` | `127.0.0.1:8090` | 监听地址 |
| `V2_DATABASE_URL` | `postgres://ora:ora-local@127.0.0.1:55432/ora_v2?sslmode=disable` | PostgreSQL |
| `V2_DATA_DIR` | `$HOME/.ora-v2` | 仓库 mirror、Node 与对话目录；同一目录只允许一个进程持有，使用短路径以满足 Unix socket 长度限制 |
| `V2_USERS_FILE` | `users.json` | 启动时预置的账号文件（相对进程工作目录）：`email`、`displayName`、`role`（`owner` / `member`）。启动时幂等写入，已存在的账号只更新名字和角色，不在文件里的账号不删除；文件缺失、没有 owner 或邮箱重复时拒绝启动 |
| `V2_NODE_ASSETS_DIR` | 未设置 | `task v2:node:prepare` 生成的固定 Node、host、guardian、Deno、OpenCode 和 Claude Code 资源；未设置时只启用纯模型路径 |
| `V2_MODEL_BASE_URL` / `V2_MODEL_API_KEY` / `V2_MODEL_NAME` | 未设置 | 可选的一组 HTTPS OpenAI 兼容 provider；必须同时设置，OpenCode 与纯模型共用，清单只公开模型名称与版本 |
| `V2_ANTHROPIC_MODEL_BASE_URL` / `V2_ANTHROPIC_MODEL_API_KEY` / `V2_ANTHROPIC_MODEL_NAME` | 未设置 | 可选的一组 HTTPS Anthropic 兼容 provider（Messages API，base URL 写到 `/v1`，如 `https://api.anthropic.com/v1`）；必须同时设置，只供 Claude Code 使用 |

测试需要 `TEST_DATABASE_URL`（任意可建 schema 的库），每个测试用独立的 schema：

```sh
TEST_DATABASE_URL='postgres://ora:ora-local@127.0.0.1:55432/ora?sslmode=disable' go test -race ./...
```

### 用 DeepSeek Flash 演示

模型固定为 DeepSeek Flash（`https://api.deepseek.com` 上的 `deepseek-flash`），只需要填 key：

```sh
mkdir -p ~/ora-demo && cp v2-server/.env.example ~/ora-demo/.env   # 填入 DEEPSEEK_API_KEY
task v2:deepseek                                                       # PostgreSQL、v2-server、frontend-v2；打开 http://localhost:5173
```

key 放在仓库外的 `~/ora-demo/.env`，一台机器配一次，主目录和所有 worktree 共用，清理 worktree 也不会删掉它。`scripts/deepseek.sh` 按行读取这个文件（不当作 shell 执行），key 为空或文件不存在时在启动任何服务前退出，再导出上表的三个 `V2_MODEL_*` 后运行 `go run .`。chat 页面只提供 Agent 执行器，所以还要求先用 `task v2:node:prepare` 准备 Node 资源（默认 `~/.cache/ora-v2-node`，可在 `~/ora-demo/.env` 里用 `V2_NODE_ASSETS_DIR` 指定），缺少时同样在启动前退出。

## 已实现

| 路由 | 来源 | 说明 |
|---|---|---|
| `GET /healthz` | 契约 | 数据库可达即 `ok` |
| `GET /auth/providers` | Gateway | 只有 `email` |
| `POST /auth/login` | Gateway | 邮箱登录：`email` 必须是预置文件或 owner 邀请过的账号，否则 `403 email_not_invited`，不会自动建用户；成功后设置 `ora_session` Cookie，`authorizationUrl` 即 `returnTo`（只接受站内路径）。只凭邮箱就能登录等于谁都能冒充谁，**仅用于演示环境** |
| `POST /auth/logout` | Gateway | 吊销会话，`204` |
| `GET /api/v1/me` | 契约 | 当前用户；无会话 `401 unauthenticated`。契约禁止额外字段，当前用户的角色从 `GET /api/v1/me/tenants` 的 `role` 读取 |
| `GET /api/v1/me/tenants` | 契约 | 当前用户所在租户，一页返回全部，`nextCursor` 为空 |
| `GET /api/v1/tenants/{tid}/members` | 契约 | 当前用户所在工作区成员，一页返回全部，按显示名排序；非成员 `403 forbidden`，非法租户 id `404 not_found` |
| `GET /api/v1/tenants/{tid}/spaces` | 契约 | 当前用户所在工作区的空间，一页返回全部；v2 每个租户一个「演示空间」，`role` 取调用者的工作区角色；非成员 `403 forbidden` |
| `GET /api/v1/tenants/{tid}/spaces/{sid}/plugins` | 契约 | 该空间里 `kind=agent` 且已安装的插件，一页返回全部；空间不属于调用者的工作区时 `403 forbidden`，非法 id `404 not_found` |

## 契约之外

### 成员邀请

邀请接口不在正式契约里，由独立候选契约 [`members.openapi.json`](members.openapi.json) 描述（正式 Cloud 的成员写入收敛后替换）。

| 路由 | 行为 |
|---|---|
| `POST /api/v2/tenants/{tid}/members` | 只有 owner 可以邀请：`email` 必填，`displayName` 选填（默认取邮箱 @ 前面的部分），`squadId` 选填（同时加入该小队，小队必须属于本工作区且未归档）。提交即创建账号并加入"演示团队"（角色 `member`），不需要对方接受；被邀请的邮箱随后可以直接登录。写权限不足 `403 forbidden`，已在团队 `409 member_exists`，小队不存在 `404 not_found` |

升级已有数据库时注意：旧开发登录建出的 `dev/demo`「演示用户」仍留在库里，但邮箱登录不再认这个身份，它再也无法登录，名下的成员记录、小队与对话却还会出现在成员页里。演示请用全新数据库，不要把旧库当成干净基线；这里不做账号清理，避免删掉别人正在引用的历史。

### 定时任务配置

定时任务配置使用独立候选契约 [`schedules.openapi.json`](schedules.openapi.json)，不改变正式 Cloud 的 ScheduledTask（其 issue/agent 执行语义不能表达本次独立对话配置）。前端以独立 Orval 配置生成客户端，后续正式契约收敛时替换这一适配。

| 路由 | 行为 |
|---|---|
| `GET /api/v2/tenants/{tid}/schedule-configs` | 当前创建者在该租户的全部配置；空列表为 `[]` |
| `POST /api/v2/tenants/{tid}/schedule-configs` | 创建配置，返回 201，初始 version 为 1、enabled 为 true |
| `PUT /api/v2/tenants/{tid}/schedule-configs/{id}` | 携带 version 修改配置，保留 enabled 和创建时间 |
| `PATCH /api/v2/tenants/{tid}/schedule-configs/{id}/enabled` | 携带 version 和 enabled 暂停/恢复配置 |
| `GET /api/v2/tenants/{tid}/schedule-resources` | 当前有效成员的真实项目、模型和执行器投影；分别标明目录是否可用，不返回后端路径或凭据 |

接口要求登录 cookie、有效成员关系及创建者归属。无权限资源返回 404，过期版本返回 409，缺失更新版本返回 428；每次修改递增版本。配置持久存储于 PostgreSQL，服务重新启动后保留。请求大小限制 64 KiB，拒绝未知字段，并校验时间、时区、星期和间隔。

对话配置保存时按当前权限验证绑定：无项目的 model ID 必须来自共享 Catalog；有项目的 projectId 必须来自当前租户的共享项目存储，target.executorId 必须匹配共享执行器和所选 model ID。agent 字段只用于展示，不作为执行身份。公共目录未就绪或选择失效时拒绝写入，不猜测默认模型/项目。兼容未绑定执行的文本配置：无项目、空模型、无 executorId 可先保存，运行时仍不能通过 model_required 门禁。

资源接口复用 `Options.Catalog` 和共享 `ListProjects`/`GetProject`，没有另一份项目/模型目录。响应的 available 区分目录不可用与可用但为空，先校验当前用户/租户/成员，再返回数据；只投影名称和稳定ID，不公开镜像目录、provider binding或凭据。项目/模型/执行器在每次保存时重检，真正执行时仍由公共服务再次鉴权。

配置响应中的 `runs` 保留兼容空列表。前端另消费执行操作和历史领域的独立候选契约 `schedule-actions.openapi.json` / `schedule-history.openapi.json`；调度 Host、操作入口和只读历史后端已在主干装配，页面通过这些契约读取实际运行事实，不生成模拟结果。

专用 HTTP 集成测试按此候选 OpenAPI 验证响应，并覆盖创建编辑、暂停恢复、服务替换、归属隔离及错误输入；数据库测试覆盖连接池关闭重开和并发版本冲突。

### 定时任务职责与旧引用兼容

定时任务只负责计划、触发身份、防重叠、调用提供方的执行接口，以及关联和展示外部运行事实。工作流的定义、发布快照、参数模式/语义校验、节点调度和执行由工作流模块拥有；任务目录、描述、项目和执行绑定由任务模块拥有。定时模块不要求这些提供方实现自己预设的解析协议。

当前写入契约只接受已接通的 chat 配置。workflow/task 写入明确返回 `503 schedule_target_unavailable`；没有 ScheduleTargetResolver、参数字符串化或可由替身开启的引用写入通道。后续接入以提供方实际交付的接口为准。

之前已保存的引用仍按原 JSON 读取，可暂停/恢复计划；只读候选类型 `ScheduleLegacyTarget` 保留未知字段，不解释、改写或校验其中的提供方参数。没有删除数据库数据或迁移其字段，前端也不提供引用参数编辑/执行入口。

### 定时执行操作

独立候选契约为 [`schedule-actions.openapi.json`](schedule-actions.openapi.json)，不修改正式 Cloud 契约或配置生成客户端。

| 路由 | 请求与行为 |
|---|---|
| `POST /api/v2/tenants/{tid}/schedule-configs/{id}/run` | `{"requestId":"稳定请求键"}`；同键重试找回同一次手动运行，显式重跑用新键。暂停任务可手动运行，同任务防重叠仍生效 |
| `POST /api/v2/tenants/{tid}/schedule-configs/{id}/runs/{oid}/cancel` | `{}`；只取消当前用户创建、属于路径中任务的运行。取消未确认返回 `503 stop_unconfirmed`，不伪造终态或释放占位 |

先检查当前登录、成员/用户/租户有效性和创建者归属，再检查执行可用性；隐藏资源统一 404。正文不能指定用户、触发时间、状态或执行引用。成功响应是运行回执（ID、状态、原因、占位、版本、计划时间），不带 prompt/配置快照/公共 Ref；HTTP 200 只表示操作完成，不表示运行成功。并发操作返回 `409 operation_in_progress`，可以用相同请求键或同一个取消 URL 重试。请求超时等返回 `503 operation_incomplete`；已保存记录的错误响应带 `params.occurrenceId`，客户端不能因此换键自动再跑。

main 创建一个共用协调器/Host，用一秒 ticker 发现持久配置的当前有效创建者，不需要浏览器保持打开。首次扫描/恢复只安排未来时刻，暂停配置仍参与已接收运行的收尾。每个 Scope 先经公共 Catalog 可用性检查，再做一次协调器恢复；未配置执行时不恢复/写入新 occurrence，操作返回 `503 schedule_execution_unavailable`。Catalog 只是可用性门禁，真正的资源选择和授权由公共 Service 在 Start 时检查。

公共 Service 必须先完成启动恢复，再启动 Host。扫描器、立即运行和取消共用同一个 `Options.Schedules`；main 将公共 `executionruntime.Service` 注入 `Options.Execution`/`Catalog`，完成启动恢复后沿用同一接线启动 Host，不另造 provider。退出时停止 ticker、取消并等待扫描和已进入的 HTTP 操作，之后才关闭公共执行及数据库；退出不伪造成功，未完成运行下次由公共恢复和原身份核对处理。工作流不走对话适配器。

### 定时触发历史

定时触发历史提供只读 `GET /api/v2/tenants/{tid}/schedule-configs/{id}/runs`、`GET /api/v2/tenants/{tid}/schedule-configs/{id}/runs/{oid}` 及 `GET /api/v2/tenants/{tid}/schedule-configs/{id}/runs/{oid}/result`。候选契约、权限、分页和结果边界见 [运行历史 HTTP](schedule-history.md)。结果读取经统一 `Options.Execution` 注入，校验当前身份、服务端 Caller 和已存 Ref；不启动执行或推进状态。配置响应中的 `runs` 仍为空，公共事件订阅未在历史 HTTP 暴露；未注入执行服务时结果接口返回 503。

小队协作使用独立候选契约 [`squads.openapi.json`](squads.openapi.json)。正式 Cloud 尚无小队接口（P1 真实编组由 circus#750 承接），本批不修改 `api/openapi.json`；#750 合入后按正式契约收口。

| 路由 | 行为 |
|---|---|
| `GET /api/v2/tenants/{tid}/squads` | 列出未归档小队及实时人类/Agent/维护者计数 |
| `POST /api/v2/tenants/{tid}/squads` | 创建小队，并在同一事务加入创建者为人类维护者 |
| `GET /api/v2/tenants/{tid}/squads/{sid}` | 小队详情，包含已结束的编组历史 |
| `PUT /api/v2/tenants/{tid}/squads/{sid}` | 编辑名称、简介、协作说明与 leader，携带 version |
| `POST /api/v2/tenants/{tid}/squads/{sid}/archive` | 归档，结束全部有效编组并清除 leader |
| `POST /api/v2/tenants/{tid}/squads/{sid}/members` | 加入人类或 Agent 成员，携带 version |
| `PUT /api/v2/tenants/{tid}/squads/{sid}/members/{mid}` | 修改职责文本与维护者标记，携带 version |
| `DELETE /api/v2/tenants/{tid}/squads/{sid}/members/{mid}?version=N` | 结束编组并保留历史 |

成员是显式类型化引用：人类引用 user 身份，Agent 引用 Space Agent 身份，不使用裸 memberId。人类必须是当前有效的租户成员；Agent 必须是同工作区活动且观测为已安装的 Space Agent。当前工作区成员可读取小队与成员；编辑、增员与归档只限工作区管理员或本队有效人类维护者，Agent 不能成为维护者。写操作按小队 version 做乐观校验：过期返回 409 `version_conflict`，缺 version 返回 428，归档后返回 409 `squad_archived`。归档结束全部有效编组、保留历史且不提供恢复；移除现任 leader 会在同一事务清除 leader 配置。

Agent 成员引用 `v2_space_agents` 里观测为已安装的插件，候选由契约的空间插件读取路由提供（见上方契约路由表）；没有安装、卸载写接口，也不使用虚构 Agent。

小队与成员关系不授予私人模型、Git 凭据或运行时权限，也不触发 Agent 执行、leader 路由或自动委派。

### 项目仓库

项目使用独立同步候选契约 [`projects.openapi.json`](internal/projectrepos/projects.openapi.json)：
`GET/POST /api/v2/tenants/{tid}/projects` 与 `GET /api/v2/tenants/{tid}/projects/{pid}`。
创建只接受匿名 HTTPS 仓库 URL 与分支，完成一次 mirror clone、验证分支并固定 snapshot 后才写入
已有的 PostgreSQL 项目记录；clone 或分支检查失败不创建项目。之后不 fetch，也不向上游 commit/push。
列表按 ID 升序分页，读取要求当前租户成员关系；创建者来自登录身份。响应不暴露 mirror 路径、凭据或 snapshot ref。

[`projectrepos.New`](internal/projectrepos/README.md) 通过 `server.Options.Projects` 注入。
`main` 装配该依赖，普通启动为开发登录的演示团队创建或复用真实 `fixture://demo` 项目，
带本地 Git mirror 和固定 snapshot；清单读取同一 PostgreSQL 记录，重启不重复创建。
内部 `SeedDemo` 不接收浏览器路径，构造服务和 HTTP 请求不会自行调用它，HTTP 创建仍禁止 fixture/file scheme。
演示环境信任租户成员，创建项目会让服务器访问成员给出的任意 HTTPS 地址，包括私网与 loopback；
完整目标地址信任模型见 [`projectrepos/README.md`](internal/projectrepos/README.md)。
本接口不伪造正式 v1 创建契约中的 Space、Workspace 或异步 Operation，也不创建执行目录。

### 执行目录

[`catalog.openapi.json`](internal/server/catalog.openapi.json) 定义
`GET /api/v2/tenants/{tid}/execution/models` 和 `GET /api/v2/tenants/{tid}/execution/executors`。
两者要求登录和当前成员关系，响应为 `{ "items": [...] }`，不返回 provider URL、凭据或磁盘路径。
始终提供免密钥 `fixture` 模型；有 Node 资源时提供 `opencode:fixture` 和 `claude:fixture` 执行器；
完整 OpenAI 兼容 provider 配置再提供 `configured` 模型和 `opencode:configured` 执行器，
完整 Anthropic 兼容 provider 配置再提供 `claude:configured` 执行器，无 Node 资源时执行器清单为空。
每个 Agent 只用自己协议的绑定：OpenCode 走 OpenAI chat completions，Claude Code 走 Anthropic Messages；
Messages 绑定只给 Agent，不出现在模型清单里，纯模型对话不能选它。清单里 OpenCode 排在前面，默认选择不变。

### 工作流定义

工作流定义按 desktop 的编辑模型提供，使用独立候选契约 [`workflows.openapi.json`](workflows.openapi.json)，不修改正式 Cloud 的 workflow 路由（其 snapshot/publish 语义与 desktop 不同，等 #827 收口后再谈收敛）。每个定义恰好有一个可变的草稿快照（`version` 为保留字 `draft`），发布时把草稿冻结成不可变版本并把它设为活跃版本；回滚只把历史图灌回草稿，激活才移动活跃指针。时间戳按 desktop 契约用 Unix 毫秒，`graph` 按编辑器的原始字符串存取，不重新序列化。

| 路由 | 行为 |
|---|---|
| `GET …/workflows` | 租户内未删除定义，最新创建在前；只带发布版本号，不带图 |
| `POST …/workflows` | 创建定义与草稿，返回 201；名字 trim 后非空且同租户不重名（不区分 ASCII 大小写） |
| `POST …/workflows/analyze` | 分析编辑器提交的图：未参与执行的节点与无法识别的节点类型，不保存 |
| `GET …/workflows/{wid}` | 定义、草稿与当前活跃版本（未发布为 `null`） |
| `PUT …/workflows/{wid}` | 改名，保留身份与快照 |
| `DELETE …/workflows/{wid}` | 软删定义及其所有版本，之后名字可复用 |
| `GET`/`PUT …/workflows/{wid}/draft` | 读取或整份替换草稿；保存不校验，未完成的画布也能留 |
| `POST …/workflows/{wid}/publish` | `version` 为 `null` 时自动生成 `v{毫秒}`（冲突时 `v{毫秒}-{n}`）；用户版本不得是 `draft`、不超过 128 字节且可作为单个路径段 |
| `GET …/workflows/{wid}/versions` | 已发布版本（不含草稿），最新在前 |
| `GET`/`DELETE …/workflows/{wid}/versions/{version}` | 读取一个版本（`draft` 也可读）或软删；活跃版本与草稿拒绝删除，已删版本的版本名可重新发布 |
| `POST …/workflows/{wid}/activate` | 把历史版本设为活跃版本，并同步其图到草稿；返回草稿 |
| `POST …/workflows/{wid}/rollback` | 把历史版本的图灌回草稿，不动活跃指针；返回草稿 |
| `GET …/workflows/{wid}/snapshots/{sid}` | 按快照身份读取，供运行历史引用 |

写操作按定义行加锁串行：发布在同一事务里读草稿、校验并冻结，避免校验过的图和冻结的图不是同一份。请求体限制 6 MiB、拒绝未知字段；响应错误沿用 Cloud 的 `{code, params, requestId}`，领域错误码与 desktop 一致（如 `workflow_name_conflict`、`workflow_version_already_exists`、`workflow_cannot_delete_active_version`、`workflow_run_graph_parse`）。

图规则不属于 HTTP 层：`server.Options.AnalyzeWorkflow` 提供草稿分析，`Options.ValidateWorkflow` 提供发布前的可执行性校验，两者由工作流领域注入；未注入时定义读写照常，分析和发布返回 `503 workflow_domain_unavailable`，不会把未校验的文档伪装成可运行版本。

与 desktop 的明确差异：v2 以租户代替 desktop 的单一 `local` 命名空间，`namespace` 恒为 `local`；重名改为由数据库唯一索引拒绝并映射为 409（desktop 并发重名可能返回 500）；发布校验放在事务内（desktop 存在校验与冻结之间的 TOCTOU）；软删记录 `deleted_at`（desktop 丢弃快照删除时间）；版本列表在同一毫秒内用 id 兜底排序；名称唯一性用 PostgreSQL `lower()`，比 desktop 的 ASCII `NOCASE` 范围更宽。

### 工作流运行历史

运行记录同样在 [`workflows.openapi.json`](workflows.openapi.json) 里：创建时冻结一个已发布版本、项目、输入与触发身份，图不复制，历史读取始终回到冻结快照。状态存 `pending`/`running`/`succeeded`/`failed`/`cancelled`；`awaitingInput` 是「running 且有 pending 节点」的派生值，不落库。

| 路由 | 行为 |
|---|---|
| `POST …/workflows/{wid}/runs` | 冻结并创建 `pending` 运行；`snapshotId` 缺省取当前活跃版本，草稿快照拒绝；`projectId` 必填且必须是本租户项目 |
| `GET …/workflows/{wid}/runs` | 该定义的全部运行（最新创建在后，与 desktop 的列表顺序一致），带冻结版本号 |
| `GET …/workflow-runs?projectId=` | 按项目列运行；不传项目则列出本租户全部运行 |
| `GET …/workflow-runs/{rid}` | 运行、节点（每个身份只返回最新一次尝试）与 Loop 轮次 scope |
| `GET …/workflow-runs/{rid}/snapshot` | 运行冻结的快照图；定义发布新版本后历史仍按原图渲染 |
| `DELETE …/workflow-runs/{rid}` | 软删已结束的运行及其节点；running 或还有 pending/running 节点时拒绝 |
| `POST …/workflow-runs/{rid}/start` | 交给执行桥启动运行 |
| `POST …/workflow-runs/{rid}/cancel` | 取消运行；停止未确认时返回 `503 workflow_run_stop_unconfirmed` |
| `PATCH …/workflow-runs/{rid}/input` | 更新启动输入与变量；执行桥判定不可编辑时返回 `409 workflow_run_not_editable`（desktop 允许未开始和已结束的运行修改，用于准备下一次执行） |
| `POST …/workflow-runs/{rid}/nodes/{nodeId}/complete` | 交互节点完成（`force`、`requester: human`） |
| `GET …/workflow-runs/{rid}/nodes` | 只要节点行 |

- **冻结与身份**：`triggerKind`/`triggerRef` 记录手动或定时触发的稳定身份，定时侧只创建与跟踪，不复制图校验或引擎；运行与节点身份稳定，Loop 轮次是 `v2_workflow_scopes` 的一行（同一 Loop 同时最多一个活动轮），Iteration 轮次是节点行的 `iteration`（0 基）。
- **attempt 就是软删行**：失败尝试把节点行软删，再插入下一次尝试；同一 `(scope, node, 迭代轮)` 只允许一行未删记录，数据库唯一索引兜住并发派发。
- **引用保护**：运行冻结的版本不能被删除（`400 workflow_snapshot_in_use`），还有未删运行的定义也不能删（`400 workflow_active_runs`）；把已结束的唯一运行删掉（`DELETE …/workflow-runs/{rid}`）后，版本和定义才重新可删。运行中或还有未完成节点的运行拒删（`400 workflow_run_active`），避免删掉执行桥仍在写的会话行。
- **执行桥接缝**：`server.Options.WorkflowRuns`（`server.WorkflowRunHost`）由执行桥实现、在 `main` 注入；未注入时 start/cancel/input/complete 返回 `503 workflow_run_host_unavailable`，定义与历史读取照常。HTTP 只做登录/成员校验、严格解码和错误码映射，派发、停止确认、会话与对话历史都留在执行桥；拒绝用 server 包的哨兵错误表达（`ErrWorkflowRunNotStartable`、`ErrWorkflowRunNotEditable`、`ErrWorkflowNodeNotAwaitingInput`、`ErrWorkflowNodeNotFound`、`ErrWorkflowRunStopUnconfirmed`、`WorkflowRunInputInvalid`）。
- **进程内存储端口**：执行桥直接调用 store 上的状态转移，不经 HTTP——`StartWorkflowRun`、`CreateWorkflowNodeRun`、`BindWorkflowNodeSession`、`CompleteWorkflowNodeRun`、`FailWorkflowNodeRun`（失败 attempt 号在同一事务里分配，payload 由领域构造）、`RetryWorkflowNodeRun`（失败行软删 + 插入等待运行的下一 attempt，编号连续）、`WakeWorkflowNodeRun`（唤醒等待的重试，重复唤醒 no-op）、`FinishWorkflowRun`、`CancelWorkflowRun`、`StartWorkflowLoopRound`（同一 Loop 只允许一个活动轮）、`SettleWorkflowLoopRound`（重复结算同状态为 no-op）、`DeleteWorkflowRun`。节点完成对已终态尝试是 no-op，取消对已取消运行幂等。
- **尚未包含**：`failedAttempts`、变量与 condition 决策投影、整次运行 restart/resume 仍由执行桥的后续 PR 提供；它们依赖节点 payload 的键，这一层不解释、不编造。

### 对话

对话页使用独立候选契约 [`chat.openapi.json`](chat.openapi.json)。正式契约里的对话挂在 issue 下，没有独立的对话列表。前端以独立 Orval 配置生成客户端。领域逻辑在 [`internal/chat`](internal/chat)，只经 [`internal/execution`](internal/execution/README.md) 的公共接口运行 Agent。

| 路由 | 行为 |
|---|---|
| `GET …/chat/executors` | 可选的 Agent 与模型绑定，来自公共执行目录 |
| `GET …/chat/conversations` | 本人在该租户的对话，按创建时间排序，各带自己的执行 session |
| `POST …/chat/conversations` | 以第一条消息创建对话并 `Start`，返回 201 和该轮的 session/turn |
| `PATCH …/chat/conversations/{cid}` | 改名 |
| `DELETE …/chat/conversations/{cid}` | 先 `End` 活 session，确认后再删除对话 |
| `POST …/chat/conversations/{cid}/messages` | 在对话的 session 上 `Append` 下一条消息 |
| `POST …/chat/conversations/{cid}/stop` | `Cancel` 最近一轮 |
| `GET …/chat/conversations/{cid}/events` | 对话 session 的事件，Server-Sent Events |
| `GET`/`PUT …/chat/project-order` | 本人在左栏的项目顺序 |

`…` 是 `/api/v2/tenants/{tid}`。所有路由要求登录 cookie 和成员关系；他人的对话按 404 处理。执行服务和目录取自统一注入点 `server.Options.Execution` / `Catalog`（见下一节）；它们还是未配置的 Stub 时，对话列表照常读取，执行器清单、创建对话和发消息等需要执行的调用返回 503 `execution_unavailable`。

- **一个对话、一个 session**：创建对话时只选项目，执行服务为这个对话新建目录，对话之间、对话与定时任务和工作流之间都不争抢目录。session 在两轮之间保持，直到下列情况之一，并记下 `endReason`：
  - `stopped`：停止。固定 Node 取消进行中的一轮会结束整个 session。
  - `deleted`：删除对话。
  - `interrupted`：在对话之外结束，例如服务重启后 `Append` 返回 `ErrSessionEnded`。

  session 结束后对话只读：再发消息返回 409 `conversation_ended`，页面提示新开对话。不会在同一对话里新开 session，也不重放上下文。
- **幂等**：创建对话和发消息必须带 `Idempotency-Key`，它就是该轮的 attempt ID。用同一个 key 重试，返回原对话和原 Ref，不会重复执行；同一个 key 换了参数，返回 409 `idempotency_conflict`。结果未知时返回 503 `outcome_unknown`，要用同一个 key 重试。第一条消息被明确拒绝时不留下空对话。
- **上一轮未结束**：返回 409 `turn_running`，由页面排队，上一轮结束后再发。
- **事件流**：每条消息的 `id` 是 session 内序号，`Last-Event-ID`（或 `after`）从其后续上。`session_ended` 之后发 `end` 事件；读取失败发 `failure`，不会冒充正常结束。快照 `history` 每次整份替换，`records[].data` 是固定 Node 的 HistoryLine。关闭事件流只停止观察，不停止 Agent。
- **简化**：同一对话的操作用进程内互斥串行，依赖 v2-server 是单进程；首版只接文本消息，必须选项目。

装配：`server.New` 用传入的 store 和注入的 `Execution` / `Catalog` 建一个 `chat.Service`，chat 不改 `main.go` 或启动入口。chat 只调用公共 `execution.Service`，不直接写执行存储的事件、终态或 End；chat 自己的表只记录对话、session 归属和结束原因。

HTTP 集成测试按 `chat.openapi.json` 验证 JSON 响应，向注入点传入 `executiontest.Stub`，覆盖对话生命周期、停止后只读、事件流续传、项目顺序和执行不可用；`internal/chat` 的测试用真实 PostgreSQL 和执行端口替身，覆盖幂等重试、结果未知、上一轮未结束、停止与重启后只读和删除顺序。

## 进程内 Agent 执行接口

[`internal/execution`](internal/execution/README.md) 的公共 Go 接口由
`server.Options.Execution`（`execution.Service`）与 `server.Options.Catalog`（`execution.Catalog`）
统一注入，页面 handler 通过保存的 Options 使用同一个执行后端。页面服务测试可注入显式 callback Stub，
不必各自创建执行实现或修改启动入口。

`server.New` 将未配置的 Execution/Catalog 字段默认设为同一个未配置 Stub；显式传入的依赖保留不变。
这两个字段分别配置：即使 Execution 同时实现 Catalog，省略 Catalog 仍表示该清单不可用。

`main` 将 [`internal/executionruntime`](internal/executionruntime/README.md) 的同一个 Service 注入这两个字段。
Session、Turn、连续事件、完整结果和 Caller attempt 保存在 PostgreSQL；worker 不依赖 HTTP 观察者。
有项目的调用经过每项目 Node 与独立 checkout，无项目 `StartModel` 直接使用
[`internal/modelturn`](internal/modelturn/README.md)，不创建目录。同一 Caller attempt 返回原 Ref；
同一对话或 workflow run 还在执行时拒绝新轮次。

有项目 Cancel 会结束整个物理会话，确认停止后后续 Append 返回 `ErrSessionEnded`；
纯模型 Cancel 只停止当前轮，之后可继续。取消已经结束的旧轮只返回原结果，不停止后来的轮次。
End 确认停止后持久化会话结束，历史仍可读。启动先恢复并停止旧 Node，再把未完成轮记为 interrupted、
将旧会话全部结束；不重放 prompt 或恢复 Agent 上下文。页面领域继续通过同一执行接口接入。

fixture 只替代模型，Git、Node、OpenCode / Claude Code 与 shell 工具都实际执行：

```sh
# 在仓库根目录准备资源，随后到 v2-server；资源和数据目录均使用 Linux 原生磁盘。
task v2:node:prepare
V2_NODE_ASSETS_DIR=/absolute/path/to/assets go run . -fixture-check
# 公共 Service 的真实工具写入、End 和 PostgreSQL 重启读取
V2_NODE_ASSETS_DIR=/absolute/path/to/assets TEST_DATABASE_URL='postgres://ora:ora-local@127.0.0.1:55432/ora?sslmode=disable' go test -race -count=1 -run TestReal -v ./internal/executionruntime
```

每个项目的 Node 在某个插件第一次被用到时才安装它。Claude Code 包约 190 MB，解压后约 400 MB，
用 debug 构建的 Node 首次安装约需一分钟；安装结果留在 `V2_DATA_DIR` 的项目 Node 目录里，
之后重启服务再用只需约一秒。演示前先在每个演示项目里跑一轮 Claude Code，可避开这次等待。

End 和重启不自动删除目录。停止服务后 `go run . -cleanup` 只删除确认停止后的登记 Node 与对话目录，
保留项目 mirror 和数据库历史；命令拒绝与持有同一数据目录的服务并行运行。
外部 provider 的显式验收入口与配置见执行服务包 README；默认 skip 不代表外部 provider 验收通过。

## 与正式 Cloud 的差异

### 调度编排与对话消费适配

[`internal/scheduler`](internal/scheduler/README.md) 提供显式注入记录、执行接口和时钟的协调器。Recover 完成持久恢复后才允许 Tick/Manual，首次发现、版本变化和暂停恢复从未来时刻安排；单进程迟到扫描只处理最近到期点，不回补积压。手动请求用固定键找回原时刻和记录，尚未 begin 的记录可继续首次接收，已接收或未知结果仅按原身份核对。

ConversationAdapter 按冻结配置分流：无项目文本对话用公共 StartModel 和明确模型 ID，有项目文本对话用公共 Start 和明确的 projectId、target.executorId。每次触发新建会话，有项目时由公共服务创建独立目录，两路不相互降级。适配层共用 ReadAttempt/Cancel/End，不直接调用 provider 或 Node，也不另存 Session/Turn。成功要求 AnswerComplete；结果终态和 End 确认分开，End 失败不释放任务占位。缺失绑定、工作流目标明确拒绝，不从演示名称猜测可执行绑定。

协调器本身不创建 goroutine；main 的 Host 接入周期扫描和上述 HTTP 操作，前端页面已接入运行与取消等操作。main 注入公共 `executionruntime.Service` 驱动实际派发；普通测试通过公共 Stub 和实际 PostgreSQL 验证编排、HTTP 和生命周期，opt-in 测试另组合真实公共 Service。

`server.NewScheduleCoordinator(records, options, now)` 使用页面统一的 `Options.Execution`，不另建执行服务；宿主在公共启动恢复完成后调用 Recover，再驱动 Tick/Manual。Start 与同记录的核对/取消不会并行。启动错误或本地引用未保存时只按原 Caller 读取；无引用且确定没有进行中的 Start，查询未命中可记 skipped/execution_not_started。读取错误或已有引用但缺失记录时保留占位，不重放 prompt。Agent Cancel 结束会话，model-only Cancel 只停当前轮；定时对话两路都随后确认 End，保留历史与目录。详细状态、原因及 released 含义见 [状态说明](internal/store/occurrence-states.md)。

### 定时触发记录基础

`internal/schedule.Next` 提供服务端时间计算；`Store.RecordOccurrence` 在事务内锁定配置、检查创建者与当前成员关系，并写入 `v2_schedule_occurrences`。记录层自身不调用执行模块；扫描和操作入口由 Host/协调器承担。

- 自动触发身份为配置 ID + scheduled + UTC 计划时刻，服务端从时刻生成键；同一时刻不因配置版本变化重复生成。
- 手动触发使用配置 ID + manual + 稳定请求键；同键同版本同请求时刻返回原记录，变更参数返回冲突；显式重跑使用新键。暂停配置允许明确的手动触发；记录层不直接执行，实际派发由协调器承担。
- 每次记录冻结配置全文和版本、计划时刻与记录时间。初始 `recorded` 只表示触发已记录，不表示已执行；已有同任务占位时直接记为 skipped。执行状态与稳定执行引用由公共执行适配层通过内部证据接口推进。
- 同一身份的重试先找已存在记录，因此后续编辑/暂停不会阻断合法重试，也不会改写历史。新自动触发要求版本仍匹配、配置启用、时间已到且符合规则；未来时刻、创建前时刻和非规则时刻被拒绝。
- 配置行锁串行化创建触发与编辑，数据库 UNIQUE 约束兜底防重复。跨租户/用户读写不可用。`Occurrences` 是内部有界分页接口（1–100条），按 `recorded_at DESC, id DESC` 时间倒序排列，游标携带上一页末项的 `(recorded_at, id)`。
- 时间统一为 PostgreSQL 支持的微秒精度；固定间隔沿创建时间锚点，日历规则跳过 DST 空缺，同一本地时刻回拨时仅记录第一次。

这一层接受显式计划时刻，不替调用方选择停机期间哪些时刻应记录。Host 保留同任务占位与重启不补跑，真实执行取决于注入的公共服务。公共 Busy 仅表示同会话/运行的上一轮仍活动，定时消费保留明确拒绝的 skipped/execution_busy，不做跨任务目录竞争或等待。结果终态与任务占位释放分开，未确认 End 前继续阻止重叠。记录防重复不等于外部执行恰好一次，公共 Session/Turn/事件仍由公共执行模块保存。

真实 PostgreSQL 测试覆盖八个并发请求返回同一个触发、不同触发各自存在、编辑后的快照不变、暂停/版本/时刻拒绝、手动重试身份、按时间倒序与同时间戳并列时的复合游标分页隔离，以及关闭数据库连接后重新读取。

- 登录：没有外部 provider，也没有 attempt 和 PKCE；dev 登录固定为一个演示用户。
- 会话存在同一个库里，只存 token 的 SHA-256；没有 Gateway 现签的内部 JWT。
- 写请求不检查 `Idempotency-Key`，没有执行代次隔离；定时任务配置与小队更新使用乐观版本检查，对话的创建和发消息把 `Idempotency-Key` 用作执行的 attempt ID。
- 小队权限把开发登录的 `owner` 角色也当作工作区管理员；成员撤销不会自动关闭其小队编组（由后续接入撤销事务的运行承接）。
- 空间与插件：每个租户固定一个「演示空间」，开发登录时自动创建；里面登记两个真实安装的 Agent 插件——`official/ora-space.opencode@0.6.11` 和 `official/ora-space.claude@0.4.5`，即 [`scripts/prepare-node.sh`](scripts/prepare-node.sh) 校验 SHA 后准备、chat、工作流与定时任务真正加载的那两个；已有演示库在下次登录时补上 Claude Code 一行。没有安装、卸载写入接口，`kind` 恒为 `agent`，安装计数与请求者不记录；`SpaceAgentPlugins` 的读法在 `#774` 的 `Catalog.ListExecutors` 装配好后可以换成真实来源，契约形状不变。
- 成员列表：`memberships` 没有成员 id、版本和加入来源，所以 `id` 直接用 user id、`version` 恒为 1、`status` 取用户状态，`joinMethod` / `joinedAt` / `actorDisplayName` 恒为空；分页参数被忽略，一页返回全部。切到正式 Cloud 的成员接口时替换。
- 启动时直接建表，没有迁移。
- 本地 Node 的传输、监督和配套服务位于 [`internal/node`](internal/node/README.md) 与
  [`internal/nodeservices`](internal/nodeservices/README.md)，通过项目 Agent driver 接入服务入口。
  配套服务只支持演示仓库的只读 Git、固定 OpenCode / Claude Code 包、各协议的 fixture，以及配置的 OpenAI / Anthropic 兼容 provider；
  Claude Code 的冻结配置没有个人 Settings（`settings_json` 为 `{}`、revision 为 0）；
  本地证书有效 24 小时，关闭后撤销凭据，无正式 Cloud 的身份、Git 交付或证书轮换机制。
- [`internal/nodehistory`](internal/nodehistory/README.md) 将固定 Node 的历史转换为替换快照，
  只读其本地 JSONL 恢复完整结果；历史缺失、损坏或超限明确失败。提供真实 Node + PostgreSQL
  的超长回答验证；公共执行服务在 PostgreSQL 保存历史快照与终态后才 ACK Node。

定时运行的内部状态与只读核对查询见 [状态说明](internal/store/occurrence-states.md)。采用同步接收后的原身份核对：reconciling 不重派，execution_busy 不表示跨任务争抢目录，只有 End 确认后释放同任务占位。schema 只接受当前状态枚举；本层不启动执行或定时器。
