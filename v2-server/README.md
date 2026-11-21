# v2-server

## 会话分析与模型请求观测

`dashboard.openapi.json` 定义临时 v2 分析契约：`/api/v2/tenants/{tid}/dashboard/sessions` 及其详情、history、statistics、model-calls 子接口。这些接口读取真实 PostgreSQL 数据，按来源展示普通 Chat 与手动任务 Agent 会话。任务保留成员共享权限；Chat 只对 conversation 所属用户可见，同时校验当前成员关系、原始 caller、项目与执行器绑定，删除 conversation 后所有分析接口均不可读。读接口返回 private/no-store，不触发执行。列表按创建时间倒序，筛选和游标绑定；请求明细每页 25 条，汇总独立覆盖全部已记录 attempt。

历史返回有界的定型快照，标为 `persisted_snapshot`，不冒充正式 Cloud 的 Revision 归档，不静默回填缺失数据。普通 Chat 与任务 Agent 模型代理通过 `internal/requestmetrics` 在派发前记录可信 session/turn；登记失败返回 503 且不发上游；完成记录只接受同 ID 同结果的有界幂等重试，失败保留 started。已有 Chat 历史可直接读取，过去未观测的模型请求保持未采集，不从回复反推调用次数或用量。记录仅包含允许的数字和状态，不保存请求/响应正文、模型 key 或 provider URL。OpenAI/Anthropic、JSON/SSE 的 Token 独立、未知和真实零保持原义，fixture/上游来源明确。

B 段及合成 demo 的陈默账号会预置一条有标记的分析历史，未调用模型，也没有虚构 HTTP 请求统计。现场任务的模型请求由真实执行采集。分析使用现有 demo 数据库、用户、重置和 seed 机制，前端团队 mock 开启不影响真实分析读取。

Cloud v2 的**临时**单体后端：一个进程、一个 PostgreSQL 库，实现 `api/openapi.json` 里 v2 用到的子集，供 `frontend-v2` 演示和快速端到端测试。`frontend-v2` 切到正式 Cloud 后删除。

## 运行

需要 Linux/WSL、Git、Go（版本见 `go.mod`）和 PostgreSQL。仓库根目录的 `compose.yaml` 提供本地 PostgreSQL（127.0.0.1:55432）。v2-server 默认连接其中的 `ora_v2` 库，库不存在时自动创建，启动时自动执行尚未应用的迁移（见下文「数据库迁移」）。Node 数据目录放在 Linux 原生文件系统。

```sh
docker compose -p cloud up -d --wait   # 在仓库根目录
V2_LOGIN=email go run .                # 开发模式，监听 127.0.0.1:8090
```

收到 SIGTERM 或中断信号时，进程会取消 HTTP 请求上下文，让保持打开的对话 SSE 关闭观察并等待其读取协程退出，再结束 HTTP、公共执行服务和数据库。关闭浏览器或单条 SSE 只停止对应观察者，不取消执行 worker；进程退出时的执行停止与完整终态保存仍由公共执行服务负责。

| 环境变量 | 默认值 | 说明 |
|---|---|---|
| `V2_LOGIN` | 必填 | 开放的登录方式，逗号分隔，登录页按这个顺序显示；不设或为空拒绝启动（没有默认值，忘了配置的部署不会开出无密码的邮箱登录），开发写 `email`，`task v2:demo` 等入口已设好。可选 `email`（只凭邮箱登录，仅用于开发和演示）与 `huawei-idaas`（华为 IDaaS，配置见[部署说明](docs/deployment.md)）；写了未知、重复或空的项在任何数据操作前拒绝启动。`-fixture-check`、`-test-fixtures` 要求开了 `email`。开了 `email` 的启动只能用于开发库，没开的只能用于部署库：库在第一次启动时记下类型，之后换方式启动会被拒绝 |
| `V2_PUBLIC_URL` | 未设置 | 浏览器访问本服务的地址（`http`/`https`，只能是根地址，不带路径、查询串）。设置后，带会话 Cookie 的写请求只接受这个 origin；未设置时 `Origin` 须与请求的 Host 相同。是 `https` 时会话 Cookie 改名为 `__Host-ora_session` 并加 `Secure`。在会改写 Host 的反向代理后面必须设置它，否则 `Origin` 与 Host 对不上，所有写请求都会 `403`。开了 `email` 又是 `https` 时启动日志会警告 |
| `V2_FRONTEND_DIR` | 未设置 | 前端构建目录；未设置保留 API-only |
| `V2_ADDR` | `127.0.0.1:8090` | 监听地址 |
| `V2_DATABASE_URL` | `postgres://ora:ora-local@127.0.0.1:55432/ora_v2?sslmode=disable` | PostgreSQL |
| `V2_DATA_DIR` | `$HOME/.ora-v2` | 仓库 mirror、marketplace checkout、Node 与对话目录；同一目录只允许一个进程持有，使用短路径以满足 Unix socket 长度限制；用 release 构建的 Node 时，它和各级父目录都不能组可写或其他人可写（不能放在 `/tmp` 下），否则启动时报错退出 |
| `V2_DELIVERY_MAX_OBJECT_BYTES` | `67108864`（64 MiB） | 每轮交付上限，分别检查变更和未跟踪文件的大小总数，以及相对起点新增的 Git 对象的未压缩字节总数，须为正整数；超限记录「改动过大」 |
| `V2_DELIVERY_MAX_PATCH_BYTES` | `16777216`（16 MiB） | 在线下载一次交付的完整 patch 的字节上限，须为不超过 `268435456`（256 MiB）的正整数，否则启动时报错退出；超限返回 413 `patch_too_large`，交付本身保留 |
| `V2_USERS_FILE` | `users.json` | 启动时预置的账号文件（相对进程工作目录）：`users` 里的 `email`、可选的 `displayName`、`role`（`owner` / `member`）；`retired` 里列出要从成员页移除的旧预置 `email`。`users` 幂等写入，已存在的账号只更新名字和角色；不在 `users` 里的账号默认保留，只有 `retired` 中的地址会在首次启动时软删除。文件缺失、没有 owner、邮箱重复或 `users` 与 `retired` 重叠时拒绝启动 |
| `V2_PLUGIN_MARKETPLACE_URL` | `https://github.com/ora-space/marketplace` | 未设置使用开发默认，显式空字符串关闭官方源；插件市场 Git HTTPS 地址；禁止携带凭据、query 或 fragment。离线测试直接构造 Syncer 使用本地仓库 |
| `V2_PLUGIN_MARKETPLACE_BRANCH` | `main` | 市场同步分支；启动时浅克隆并拉取，之后按间隔快进工作副本 |
| `V2_PLUGIN_MARKETPLACE_INTERVAL` | `5m` | 市场刷新间隔；必须为正 duration。失败时保留上一版 PostgreSQL 快照 |
| `V2_MARKET_SOCKS_PROXY` | 未设置 | 自定义来源启用代理时使用的部署级 SOCKS5 地址；Git 与 Skill 包下载都使用，客户端只把已校验的公网 IP 交给代理。设置了合法的 `socks5://` 地址时，安装官方市场的 Skill、工作流和 MCP 包、以及向项目 Node 下发官方 Skill 时的下载也走它；未设置或不合法时官方包直连 |
| `V2_MARKET_CREDENTIAL_KEY` | 未设置 | 来源凭据、租户 MCP 设置与私有项目仓库令牌的 AES-256-GCM 密钥（Base64 编码的 32 字节）；保存 MCP 设置、启用 S3 来源、自定义来源 Git 认证或私有项目仓库令牌时必须持久提供 |
| `V2_NODE_ASSETS_DIR` | 未设置 | `task v2:node:prepare` 生成的固定 Node、reaper、Deno、OpenCode 和 Claude Code 资源；未设置时只启用纯模型路径 |
| `V2_MODEL_BASE_URL` / `V2_MODEL_API_KEY` / `V2_MODEL_NAME` | 未设置 | 可选的一组 HTTPS OpenAI 兼容 provider；必须同时设置，OpenCode 与纯模型共用，清单只公开模型名称与版本 |
| `V2_ANTHROPIC_MODEL_BASE_URL` / `V2_ANTHROPIC_MODEL_API_KEY` / `V2_ANTHROPIC_MODEL_NAME` | 未设置 | 可选的一组 HTTPS Anthropic 兼容 provider（Messages API，base URL 写到 `/v1`，如 `https://api.anthropic.com/v1`）；必须同时提供地址、凭据和模型名，只供 Claude Code 使用。使用 Bearer 网关时以 `V2_ANTHROPIC_MODEL_AUTH_TOKEN` 代替 `V2_ANTHROPIC_MODEL_API_KEY`，二者不能同时配置；Claude Code 的 base URL 若以 `/compatible` 结尾，这里填 `/compatible/v1` |
| `V2_MODEL_IDLE_TIMEOUT` / `V2_MODEL_MAX_DURATION` | `2m` / `30m` | 单次上游模型请求的空闲超时和总上限，两种协议共用；必须为正 duration，空闲超时不能大于总上限。连续这么久收不到上游数据、或总时长到上限时中止响应，Agent 收到的是失败而不是正常结束的流；总上限切断后这一轮不再放行重发，空闲超时切断后只放行 1 次，这一轮按失败结束并显示原因 |

测试需要 `TEST_DATABASE_URL`（任意可建 schema 的库），每个测试用独立的 schema：

```sh
TEST_DATABASE_URL='postgres://ora:ora-local@127.0.0.1:55432/ora?sslmode=disable' go test -race ./...
```

### 数据库迁移

表结构由 [`internal/store/migrations`](internal/store/migrations) 下按序编号的 SQL 文件定义（`NNNN_snake_name.sql`，从 `0001` 起连续），编译进 binary，`store.Open` 在启动时自动执行尚未应用的文件。每个文件在单独的事务里执行，并在同一事务里把版本、文件名和文件内容的 SHA-256 写进当前 schema 的 `v2_schema_migrations`；多个进程同时启动时由按 schema 加的事务级 advisory lock 串行，每个迁移只执行一次。`0001_initial.sql` 是迁移引入前 `schema.sql` 在空库上建出的最终结构。

启动在执行任何迁移前先校验，不满足就报错退出并指出文件或版本：文件名不合规或编号不连续、已应用的迁移内容或文件名被改动、库里有本 binary 不认识的更新版本、已应用的最高版本以下还有未应用的文件，以及库里已有 v2-server 的表却没有 `v2_schema_migrations`（迁移引入前的库，须删除重建，不做衔接）。

改表结构或修数据：

- 新增下一个编号的文件，不要修改、改名或重新编号已合入 `main` 的文件；已应用文件的校验和不一致时服务拒绝启动。
- 编号取 `main` 上最新编号的下一个；合入前 rebase 后再核对，不能使用等于或小于 `main` 最新编号的数字。按名字顺序执行"所有未应用文件"时，插在已有编号之下的文件在升级库上最后执行、在新库上却在中间执行，两边的结构会分叉，所以启动直接拒绝这种情况。
- 数据修复也写成迁移，只执行一次，不再每次启动重跑。
- 每个文件在一个事务里执行，所以文件里不能写 `BEGIN`/`COMMIT` 等事务控制语句，也不能用 PostgreSQL 禁止在事务块中执行的语句（`CREATE INDEX CONCURRENTLY`、`VACUUM`、`ALTER SYSTEM` 等）；以行首事务控制语句开头的文件启动时直接拒绝。
- 迁移需要能在空库上从 `0001` 起建出同样的结构：`internal/store/migrate_test.go` 对每个 k 先应用 1..k 再升级到最新，与新建库比较表、列、约束、索引、函数、触发器、枚举值、视图定义和序列，必须完全一致。

### 录屏场景 seed

新人需求开发、需求拆分和每日巡检的工作流 seed 见 [`demo/README.md`](demo/README.md)。三个 seed 接收各段明确的用户、项目和真实模型绑定，可按任意顺序叠加；每日巡检带三晚可查询的预置历史。演示环境的重置与进程启动由公共框架负责。

### 用 DeepSeek Flash 演示

模型固定为 DeepSeek Flash（`https://api.deepseek.com` 上的 `deepseek-flash`），只需要填 key：

```sh
mkdir -p ~/ora-demo && cp v2-server/.env.example ~/ora-demo/.env   # 填入 DEEPSEEK_API_KEY
task v2:deepseek                                                       # PostgreSQL、v2-server、frontend-v2；打开 http://localhost:5173
```

key 放在仓库外的 `~/ora-demo/.env`，一台机器配一次，主目录和所有 worktree 共用，清理 worktree 也不会删掉它。`scripts/deepseek.sh` 按行读取这个文件（不当作 shell 执行），key 为空或文件不存在时在启动任何服务前退出，再导出上表的三个 `V2_MODEL_*`；如果文件还配置了 `V2_ANTHROPIC_MODEL_*`，也会读取并校验 Claude 的配置。随后运行 `go run .`。chat 页面只提供 Agent 执行器，所以还要求先用 `task v2:node:prepare` 准备 Node 资源（默认 `~/.cache/ora-v2-node`，可在 `~/ora-demo/.env` 里用 `V2_NODE_ASSETS_DIR` 指定），缺少时同样在启动前退出。

### 录屏演示环境

11-06 录屏的三段各有一个命令，另有一个把三段合在一起的命令。每个命令都用独立的库、数据目录和端口，和日常开发的 `ora_v2` 库及 8090/5173 分开，可以同时运行：

| 命令 | 段 | 数据库 | 数据目录 | 后端 / 前端 | 账号 | `VITE_DEMO_MOCK` |
|---|---|---|---|---|---|---|
| `task v2:demo:newcomer` | A 新人 | `ora_v2_demo_newcomer` | `~/.od/a` | 8191 / 5191 | 林新 | 关 |
| `task v2:demo:lead` | B Tech Lead | `ora_v2_demo_lead` | `~/.od/b` | 8192 / 5192 | 陈默、苏芮、周衡 | 开 |
| `task v2:demo:ops` | C 运维 | `ora_v2_demo_ops` | `~/.od/c` | 8193 / 5193 | 许运 | 关 |
| `task v2:demo` | 三段合成 | `ora_v2_demo` | `~/.od/all` | 8190 / 5190 | 以上全部 | 开 |

每个命令依次：按上一节检查 `~/ora-demo/.env` 的 key 和 Node 资源；启动 compose PostgreSQL；删掉并重建本段的库、清空数据目录（同一段还在运行时拒绝，并按可执行文件路径停掉上次留下的 Node 进程）；用 `V2_DATABASE_URL`、`V2_DATA_DIR`、`V2_ADDR`、`V2_USERS_FILE` 启动预先编译的 v2-server；`/healthz` 就绪后运行本段的 seed；最后以 `0.0.0.0` 启动 frontend-v2。所以重跑同一个命令总是回到同一个初始状态。账号用邮箱登录，seed 完成后终端会列出来。前端端口被占用时，用 `ORA_DEMO_WEB_PORT` 换一个。

数据目录没有用 #882 原定的 `~/ora-demo/data/<段>`：Agent 运行时要在数据目录下建 Node 的 Unix socket，路径必须短于 108 字节，数据目录本身最多 67 个字符，所以放在短的 `~/.od/<a|b|c|all>`（A、B、C 三段和合成）。主目录较长时用 `ORA_DEMO_DATA` 换一个更短的目录，太长时命令会在启动前报错。

seed 在 [`demo/`](demo/README.md)：`env.go` 是上表和重置、准备本地 Git 项目、合并用户文件的公共部分，`scenes.go` 接上三段，`lead.go` 是 B 段，`users/<段>.json` 是各段的账号（合成时合并）。同一项目第一次执行 Agent 要多等约一分钟在 Node 上安装 Agent，录屏前先在本段的项目里跑一次热身。

## 租户授权

所有 `/tenants/{tid}` 路由经同一个入口判定成员身份（`internal/server/routes.go`）：

- **一个判定函数**：`store.TenantAccess(tenant, user)` 返回调用者在该租户的角色（`owner` / `member`）。条件只写在这一处：有成员关系、用户 active 且未删除、租户 active。
- **一个路由中间件**：路由只能经路由表的 `public` / `session` / `tenant` / `owner` 四种方式注册。`tenant` 级先判会话（`401 unauthenticated`），再判 `TenantAccess`：非成员、被移出的成员、停用的用户或租户、格式不对的租户 id，一律 `404 not_found`，看不出租户是否存在。通过后把 `{tenant, user, role}` 交给 handler，handler 不再读 `tid`、不再判成员。
- **owner 操作在路由表上声明**：用 `owner` 注册的路由，成员访问得到 `403 forbidden`。现在是邀请成员、插件来源的增改删与同步、空间插件的安装与移除。「owner 或任务创建人 / 表单审核人 / 小队维护者」这类业务规则仍在 store 里判断，角色由路由传入。
- **业务 SQL 只按 `tenant_id` 过滤**，不再各自核对成员关系；读写成员关系本身的查询（成员名单、我所在的团队、登录、邀请）除外。
- **后台工作开始执行时用同一个判定**：执行会话创建与追加一轮、定时任务每次触发、扫描定时任务所属的人、工作流表单提交后续跑。被移出租户的人，已排好的定时任务不再以他的身份执行；已经开始的工作照常收尾。
- **路由表测试**（`routes_test.go`）遍历全部路由，断言非成员一律 404、未登录 401、owner 路由对成员 403，并禁止绕过路由表直接注册路由。

## 已实现

| 路由 | 来源 | 说明 |
|---|---|---|
| `GET /healthz` | 契约 | 数据库可达即 `ok` |
| `GET /auth/providers` | Gateway | `{"providers": [...]}`：`V2_LOGIN` 开放的登录方式，按配置顺序 |
| `POST /auth/login` | Gateway | 邮箱登录：`email` 必须是预置文件或 owner 邀请过的账号，否则 `403 email_not_invited`，不会自动建用户；成功后设置会话 Cookie（`ora_session`，`V2_PUBLIC_URL` 为 https 时 `__Host-ora_session`），`authorizationUrl` 即 `returnTo`（只接受站内路径）。`V2_LOGIN` 没开的方式返回 `400 unknown_provider`；`Origin` 是别的站点时 `403 origin_mismatch`。只凭邮箱就能登录等于谁都能冒充谁，**仅用于演示环境** |
| `GET /auth/callback/{provider}` | Gateway | 跳转式登录（`huawei-idaas`）回来的地址：核对登录尝试 Cookie（https 下为 `__Host-ora_login`、`Path=/`，防止兄弟子域种入别人的尝试；http 下为只发往这个路径的 `ora_login`）里的登录尝试（内存里，10 分钟、只能用一次；最多 10000 条，满了挤掉最早的，所以灌满它只会让别人未完成的登录重来，不会拒绝新登录）和 `state`，向提供方换取身份，按 `source`+`subject`（IDaaS 为 `huawei-corp`+`uuid`）找或建账号、刷新显示名，签会话后 303 回到发起登录时的 `returnTo`。失败一律 303 到 `/?login_error=failed\|unavailable\|disabled`，日志只记步骤和对方错误码。`POST /auth/login` 对跳转式方式返回提供方的登录地址，`returnTo` 不出现在其中；回调地址只由 `V2_PUBLIC_URL` 得出 |
| `POST /auth/logout` | Gateway | 吊销会话，`204`；`Origin` 是别的站点时 `403 origin_mismatch` |
| `POST /auth/desktop/approve`、`POST /auth/desktop/token`、`POST /auth/desktop/logout` | 契约之外 | desktop 登录，见下文「desktop 登录」 |
| `GET /api/v1/me` | 契约 | 当前用户；无会话 `401 unauthenticated`。契约禁止额外字段，当前用户的角色从 `GET /api/v1/me/tenants` 的 `role` 读取 |
| `GET /api/v1/me/tenants` | 契约 | 当前用户所在租户，一页返回全部，`nextCursor` 为空 |
| `GET /api/v1/tenants/{tid}/members` | 契约 | 当前用户所在工作区成员，一页返回全部，按显示名排序；非成员与非法租户 id 都是 `404 not_found` |
| `GET /api/v1/tenants/{tid}/spaces` | 契约 | 当前用户所在工作区的空间，一页返回全部；v2 每个租户一个「演示空间」，`role` 取调用者的工作区角色；非成员 `404 not_found` |
| `GET /api/v1/tenants/{tid}/spaces/{sid}/plugins/catalog` | 契约 | 从 `ora-space/marketplace` 的 `registry/**/orax.toml` 同步的真实市场快照，包含 Agent；启动时先同步，之后每 5 分钟刷新并原子替换 PostgreSQL 目录。成员可读 |
| `GET /api/v1/tenants/{tid}/spaces/{sid}/plugins` | 契约 | 当前空间的全部活动插件选择，包括显式移除的行；空间不属于调用者的工作区时 `403 forbidden` |
| `POST /api/v1/tenants/{tid}/spaces/{sid}/plugins` | 契约 | owner 安装目录中的 Skill 或工作流插件；其他类型（市场 Agent、Hook、合集等）在此接口只读；MCP 使用独立租户接口，返回 `400 plugin_kind_not_installable`（已有市场 Agent 选择的更新同样拒绝；预装 Agent 返回 `409 plugin_runtime_managed`）。工作流包由服务端按来源自己的 `Retrieval`/`Credentials` 下载并校验 `.orax`，导入其中的工作流并发布 `v1`；已有同名工作流时自动改名为「名称（插件）」并递增序号，不改动已有定义。重复安装幂等，卸载不删除已导入工作流。升级工作流包时，仍保持导入原样（草稿和生效版本都未改动）的工作流会更新草稿并发布新的顺序版本；用户已保存或发布过编辑的定义保持原样。Skill 安装同样按来源下载并校验 `.orax`，把包内 `assets/<name>/SKILL.md` 的技能名记录到目录条目（`packMembers`，设置页同名提示用），再立即记录为空间选择，不等项目 Node 确认。支持版本保护与 `Idempotency-Key` 重放。官方市场包的下载默认走直连 HTTPS；设了 `V2_MARKET_SOCKS_PROXY` 时走该 SOCKS 代理（demo 主机直连不到市场主机时靠它下载）。
| `DELETE /api/v1/tenants/{tid}/spaces/{sid}/plugins` | 契约 | owner 移除 Skill 或工作流选择。Skill 删除整条选择行，立即不再出现在设置页和工作流节点的技能候选里；工作流选择保留 `desiredState=removed` 行；工作流及其历史保留为用户数据。预装 Agent 返回 `409 plugin_runtime_managed`；要求版本与幂等键 |

所有带会话 Cookie 的非 GET/HEAD/OPTIONS 请求先核对 `Origin`（规则见 `V2_PUBLIC_URL`），不符返回 `403 origin_mismatch`；没有 `Origin` 的请求不是页面发出的，照常处理。账号被停用或删除后，它的会话在下一个请求就失效（`401 unauthenticated`）。

带 `Authorization` 头的请求只按这个头认人（A6 D6）：必须是 `Bearer <desktop 令牌>`，否则 `401 unauthenticated`，不会退回去看 Cookie，所以浏览器里登录着的人不能替一个缺失或已吊销的令牌顶上。这类请求不核对 `Origin`：浏览器不会自己带上令牌，不存在跨站代发的风险。凡是接受会话的路由（`session` / `tenant` / `owner` 级）都接受 desktop 令牌，判定的账号、团队和权限与网页会话完全相同。

## 契约之外

任务 Agent 会话分析的五个只读接口见上文「会话分析与模型请求观测」和 `dashboard.openapi.json`。

### 成员与邀请

登录不等于入队：没有加入任何工作区的账号登录后，页面显示「还没有加入团队」（A6 D4）。入队靠一次性邀请链接；开了邮箱登录时另有按邮箱邀请。这些接口不在正式契约里，由独立候选契约 [`members.openapi.json`](members.openapi.json) 描述（正式 Cloud 的成员写入收敛后替换）。

| 路由 | 行为 |
|---|---|
| `POST /api/v2/tenants/{tid}/members` | 按邮箱邀请，**只在 `V2_LOGIN` 开了 `email` 时提供**，否则 `404 not_found`。只有 owner 可以邀请：`email` 必填，`displayName` 选填（默认取邮箱 @ 前面的部分），`squadId` 选填（同时加入该小队，小队必须属于本工作区且未归档）。提交即创建账号并加入"演示团队"（角色 `member`），不需要对方接受；被邀请的邮箱随后可以直接登录。写权限不足 `403 forbidden`，已在团队 `409 member_exists`，小队不存在 `404 not_found` |
| `POST /api/v2/tenants/{tid}/invite-links` | owner 生成一条成员邀请链接，`squadId` 选填（同时加入该未归档小队，否则 `404 squad_not_found`）。令牌 256 位随机数，库里只存 SHA-256；7 天内有效、只能用一次。响应里的 `url` 是唯一一次出现令牌的地方：设置了 `V2_PUBLIC_URL` 时是绝对地址，否则是站内路径 `/invite/<令牌>` |
| `GET /api/v2/tenants/{tid}/invite-links` | owner 查看还能用的链接（生成人、小队、过期时间）和已用的链接（使用人、时间）；过期或吊销且没人用过的不列；从不返回令牌 |
| `DELETE /api/v2/tenants/{tid}/invite-links/{id}` | owner 吊销一条没用过的链接，`204`；已用 `409 invite_already_used`，不存在 `404 invite_not_found` |
| `POST /api/v1/invites/preview` | 登录用户查看链接去向：`{state, tenant, squad, role, alreadyMember}`，`state` 为 `active` / `used` / `expired` / `revoked`；不改变任何状态。令牌放请求体，不进访问日志。未知令牌 `404 invite_not_found` |
| `POST /api/v1/invites/accept` | 加入：一个事务里锁定链接、核对未用未过期未吊销、标为已用并记下使用人，按链接的角色加入工作区（选了小队且小队仍有效就同时加入），返回 `{tenantId}`。已是成员直接返回，链接不消耗。不可用 `410 invite_unavailable`（`params.reason`：`used` / `expired` / `revoked`） |
| `DELETE /api/v2/tenants/{tid}/members/{uid}` | owner 把成员移出工作区：删除成员关系、结束他在本工作区小队里的编组（保留历史，小队 `version` 加一），账号保留、还能登录；下一个请求就看不到这个工作区。最后一个 owner `409 last_owner`，不是成员 `404 member_not_found` |

邮箱登录只要求账号存在、未停用，不要求是成员，所以被移出的人仍能登录，看到的是「还没有加入团队」。

**第一个管理员**：`v2-server admin-invite [-team <名称>]` 直连数据库（`V2_DATABASE_URL` 或 `-db`），确保这个工作区存在，写一条角色为 `owner`、24 小时有效、只能用一次的链接并打印，然后退出，不启动服务。库里只有一个工作区时可以省略 `-team`。HTTP 接口不能生成 `owner` 链接。这条命令也用来找回管理员（例如唯一的管理员离开）。第一个人打开链接加入时，新工作区会补上项目和 Agent 所需的默认空间。

令牌由邀请链接本身携带：接口里放请求体，但打开链接时它出现在浏览器地址里，没有会话时登录后回到这个地址，也会经过代理和服务的访问日志。它只能用一次、7 天内有效，这是这里的边界。

前端：`/invite/<令牌>` 页预览并「加入」（没有会话先登录，登录后回到这一页）；设置 / 小队的「成员与邀请」（只有 owner 看得到）里生成链接、吊销未用的链接、查看已用记录、移出成员（最后一个管理员不能移出）。

升级已有数据库时，启动会软删除旧开发登录建出的 `dev/demo`「演示用户」，并按 `users.json` 的 `retired` 列表一次性软删除旧预置成员。这些账号的成员记录、小队与对话引用保留，但不再出现在成员页，也不能登录。它们在小队里的成员关系会在每次启动时结束；记录留在库里，但小队详情（包括「已退出」历史）不再列出这些账号，也不计入成员、维护者人数。软删除后账号不会恢复：再邀请同一邮箱返回 `409 user_unavailable`，把它重新写回 `users` 也只会更新名字，既不恢复登录，也不补发 starter skill；`retired` 的清理同样只执行一次，手动恢复的账号不会在下次启动时再被删除。

### desktop 登录

Ora desktop 经系统浏览器和本机回环地址登录（A6 D6，RFC 8252），接口由独立候选契约 [`desktop-login.openapi.json`](desktop-login.openapi.json) 描述，A6 写明路径以正式契约为准：

1. desktop 在 `127.0.0.1`（或 `[::1]`）的系统分配端口上监听，生成 `state` 和 PKCE verifier，用系统浏览器打开 `/desktop/authorize?redirect_uri=http://127.0.0.1:<端口>/<路径>&state=…&code_challenge=…&code_challenge_method=S256`。
2. 这一页要求网页会话（没有就先按 `V2_LOGIN` 登录，登录后回到这一页），显示「允许 Ora desktop 以 <显示名> 的身份登录？」。用户点「允许」才调 `POST /auth/desktop/approve`。
3. 浏览器被送回回环地址，带上一次性授权码和 `state`；desktop 用授权码、verifier 和同一个回调地址调 `POST /auth/desktop/token` 换到令牌。

| 路由 | 行为 |
|---|---|
| `POST /auth/desktop/approve` | 只接受网页会话：带 `Authorization` 头 `403 web_session_required`（desktop 不能用令牌给自己再签令牌），`Origin` 不符 `403 origin_mismatch`。`redirectUri` 只能是 `http://127.0.0.1:<端口>/<路径>` 或 `http://[::1]:<端口>/<路径>`，不带查询串和 fragment，否则 `400 invalid_redirect_uri`（不收 `localhost`：它要经过 DNS 和 hosts 解析）；`codeChallengeMethod` 必须是 `S256`、`codeChallenge` 是 43 个 base64url 字符，否则 `400 invalid_code_challenge`；`state` 必填、不超过 512 字节。返回 `{redirectUrl}`：回调地址加上 `code` 和 `state` |
| `POST /auth/desktop/token` | 公开，凭授权码：`{code, codeVerifier, redirectUri}`。授权码只在进程内存，2 分钟有效，第一次兑换不论成败都会用掉；verifier 的 S256 与批准时的 challenge 一致、`redirectUri` 与批准时完全相同，才签令牌。授权码不存在、过期、用过，verifier 或回调不符，或账号在批准后被停用，一律 `400 invalid_grant`。成功 `{token, user: {id, displayName}}`，`Cache-Control: no-store` |
| `POST /auth/desktop/logout` | 吊销本请求 `Bearer` 头里的这一个令牌，`204`；令牌已不存在也是 `204`。没有合法的 `Bearer` 头 `401`。网页会话和其他设备的令牌不受影响 |

令牌是 `ora_dt_` 加 43 个 base64url 字符，库里和网页会话在同一张 `sessions` 表、只存 SHA-256，`kind` 区分两种：网页会话只认 Cookie、登录后固定 7 天到期；desktop 令牌只认 `Authorization` 头，连续 30 天没用才过期（最近使用时间最多一小时刷新一次，刷新后一小时内的使用不记，所以实际在最后一次使用后 30 天减 1 小时到 30 天之间过期），没有绝对上限、不轮换。网页登出不影响 desktop 令牌，desktop 登出也不影响网页会话。授权码只在内存，最多 10000 条，满了挤掉最早的；重启后进行中的授权要重新点「允许」。

### 用户技能

用户技能使用独立候选契约 [`skills.openapi.json`](skills.openapi.json)。正式 Cloud 尚无这套 CRUD
契约，因此不改 `api/openapi.json`；前端由独立 Orval 配置生成共享客户端。

| 路由 | 行为 |
|---|---|
| `GET /api/v2/tenants/{tid}/skills` | 当前登录用户的活动技能，最近更新在前；不返回其他用户的行 |
| `POST /api/v2/tenants/{tid}/skills` | 新建，返回 201；名称在同一用户内不区分大小写去重 |
| `GET`/`PUT`/`DELETE /api/v2/tenants/{tid}/skills/{sid}` | 读取、替换或软删除自己的技能；其他用户和已删除行都按不存在处理 |
| `POST /api/v2/tenants/{tid}/skills/import` | 导入一个 1 MiB 以内的 `SKILL.md`；解析 YAML front matter 的 `name`、`description`，正文去掉 front matter 后保存。同名返回 409，显式 `overwrite` 才覆盖并保留 id |

名称遵循 desktop 的安全规则：单个 ASCII slug 路径段，不能以 `.` 开头，不能包含路径分隔符；
简介最多 4096 字节，正文最多 900 KiB。写请求拒绝未知字段和多余 JSON；校验失败返回稳定的
`skill_*` 错误码，不为越权用户暴露其他账号的资源。

账号首次创建或首次被邀请时写入三个真实 starter skill（`ora-code-review`、`test-plan`、
`release-notes`），登记在独立的 `v2_skill_seeds` 一次性标记里；之后它们和用户创建的技能
完全一样，可编辑、删除，删除后不会在下次登录时重新出现。
存量账号中未编辑且目标名仍空闲的旧 `code-review` starter 会在启动时一次性改名为
`ora-code-review`；已编辑、已删除或目标名已被占用的条目不改，留给用户自行处理。

创建或导入的技能会在新 Agent 会话完成工作副本克隆后，物化为
`.opencode/skills/<name>/SKILL.md` 和 `.claude/skills/<name>/SKILL.md`。项目已有同名目录时保留
项目内容；市场 Skill 与用户技能同名时**市场优先**：该用户技能不写入、会话照常启动，只在日志
里带名字记录跳过，设置页会把这只用户技能标记出来提示改名；已经启动的会话保留启动时的副本。
市场优先只适用于对话和任务这类 Agent 会话。工作流节点不物化空间里的市场 Skill 和用户技能目录，
只投影节点显式选择的 Skill：单独选了同名的用户技能时，生效的就是这只用户技能；同一节点同时选了它和同名的市场
Skill，或者同运行里在跑的兄弟用着同名的另一份，启动方以 `workflow_skill_name_conflict` 失败。
设置页拿到的同名名单来自服务端：安装 Skill 包时会下载并校验 `.orax`，把其中
`assets/<name>/SKILL.md` 的技能名记录到目录条目（`packMembers`），与运行时的物化名完全一致，
不依赖 README 解析；单技能包即使标题与技能名不同也按技能名匹配。名单按包的 SHA-256 记录，通过空间选择冻结的安装包解析，所以别的空间安装同一插件的其他版本、或者一次安装失败，都不会改变已安装选择的名单；在记录名单之前安装的 Skill 没有名单，设置页对它退回按 README 标题匹配，重新安装后改用技能名。
市场 Skill 安装会替换同名且只由 Ora 物化出来的旧目录（旧的用户技能目录登记在 `.git/info/exclude`
自己的标题段下，识别为服务写出的才替换），项目自带的同名技能则保留，安装返回失败并带技能名。
Ora 实际创建的技能目录和市场技能链接会追加到该工作副本自己的 `.git/info/exclude`（不改项目的 `.gitignore`，保留已有规则），所以 Agent 的 `git add -A` 不会提交它们，工作区「变更」也不显示；项目自带的技能和 Agent 自己的改动照常可见。v2 的导入只支持
单个 Markdown 文件，不支持文件夹、归档和附带脚本/资源。

### 定时任务配置

定时任务配置使用独立候选契约 [`schedules.openapi.json`](schedules.openapi.json)，不改变正式 Cloud 的 ScheduledTask（其 issue/agent 执行语义不能表达本次独立对话配置）。前端以独立 Orval 配置生成客户端，后续正式契约收敛时替换这一适配。

| 路由 | 行为 |
|---|---|
| `GET /api/v2/tenants/{tid}/schedule-configs` | 当前创建者在该租户的全部配置；空列表为 `[]` |
| `POST /api/v2/tenants/{tid}/schedule-configs` | 创建配置，返回 201，初始 version 为 1、enabled 为 true |
| `PUT /api/v2/tenants/{tid}/schedule-configs/{id}` | 携带 version 修改配置，保留 enabled 和创建时间 |
| `PATCH /api/v2/tenants/{tid}/schedule-configs/{id}/enabled` | 携带 version 和 enabled 暂停/恢复配置 |
| `GET /api/v2/tenants/{tid}/schedule-resources` | 当前有效成员的真实项目、Agent 和模型投影；Agent 带自己的可运行模型，分别标明目录是否可用，不返回后端路径或凭据 |

接口要求登录 cookie、有效成员关系及创建者归属。无权限资源返回 404，过期版本返回 409，缺失更新版本返回 428；每次修改递增版本。配置持久存储于 PostgreSQL，服务重新启动后保留。请求大小限制 64 KiB，拒绝未知字段，并校验时间、时区、星期和间隔。

对话配置保存时按当前权限验证绑定：无项目的 model ID 必须来自直接模型 Catalog；有项目的 projectId 必须来自当前租户的共享项目存储，target.agentId 必须来自共享执行目录且该 Agent 能运行所选 model ID。新配置不再写 executorId；旧配置仍可按冻结的 executorId 读取和执行。`agent` 只是展示名，不参与执行身份。公共目录未就绪或选择失效时拒绝写入，不猜测默认 Agent/模型/项目。兼容未绑定执行的文本配置：无项目、空模型、空 Agent 可先保存，运行时仍不能通过 model_required 门禁。

资源接口复用 `Options.Catalog` 和共享 `ListProjects`/`GetProject`，没有另一份项目/Agent/模型目录。响应的 available 区分目录不可用与可用但为空，先校验当前用户/租户/成员，再返回数据；只投影名称和稳定ID，不公开镜像目录、provider binding或凭据。项目/Agent/模型在每次保存时重检，真正执行时仍由公共服务再次鉴权。

配置响应中的 `runs` 保留兼容空列表。前端另消费执行操作和历史领域的独立候选契约 `schedule-actions.openapi.json` / `schedule-history.openapi.json`；调度 Host、操作入口和只读历史后端已在主干装配，页面通过这些契约读取实际运行事实，不生成模拟结果。

专用 HTTP 集成测试按此候选 OpenAPI 验证响应，并覆盖创建编辑、暂停恢复、服务替换、归属隔离及错误输入；数据库测试覆盖连接池关闭重开和并发版本冲突。

### 定时任务职责与旧引用兼容

定时任务只负责计划、触发身份、防重叠、调用提供方的执行接口，以及关联和展示外部运行事实。工作流的定义、发布快照、参数模式/语义校验、节点调度和执行由工作流模块拥有；任务目录、描述、项目和执行绑定由任务模块拥有。定时模块不要求这些提供方实现自己预设的解析协议。

写入契约接受 chat、工作流和本人协作任务目标；缺少可执行绑定的旧 task 引用仅供读取，没有 ScheduleTargetResolver、参数字符串化或可由替身开启的引用写入通道。

**工作流目标** 在配置的 `projectId`（必填）里运行一个工作流。默认或 `versionPolicy: "fixed"` 保存 `{kind: "workflow", workflowId, snapshotId, input}`，校验项目属于当前用户、`snapshotId` 是这个工作流在本租户的非草稿版本；`schedule-resources` 列出已发布工作流及当前生效快照，固定模式保存此快照。`versionPolicy: "latest"` 不带 `snapshotId`，保存时只校验存在生效发布版本，每次触发在创建运行的事务内解析当前发布快照。两种模式的运行都保留实际使用的快照；发布不改写配置或历史。启动变量用快照里的默认值，`input` 作为开始节点的初始输入。进程没有执行桥时（`Options.WorkflowRuns` 为空）不列出工作流，写入返回 `503 schedule_target_unavailable`。旧的、没有这些字段的 workflow 引用照旧只读，按它写入返回 `400 invalid_target`。

执行走页面同一个运行入口：`scheduler.WorkflowAdapter` 用任务创建者的 scope 创建运行（冻结快照、项目、输入，名称取任务名）并经执行桥启动，occurrence 的 `executionRef` 记 `{"workflowRunId"}`。运行的 `trigger_kind = 'schedule'`、`trigger_ref = <occurrence ID>`，唯一索引 `v2_workflow_runs_schedule_trigger` 保证一个触发只对应一个运行，重复派发和恢复都按这个身份找回原运行，不创建替代运行。执行桥拒绝启动（图里有还不能执行的节点、输入不满足开始节点）时记 `skipped / workflow_run_not_startable` 并取消那个未开始的运行。协调器每次 Tick 核对未释放的记录：运行结束后成功记 `succeeded`、失败记 `failed / workflow_run_failed`（重启恢复中失败的记 `interrupted`；boot sweep 吸收并恢复的 Iteration 运行仍在执行，记录回到 `running`，可从定时任务取消，之后按它自己的结果记）、取消记 `cancelled`，随即释放占位；运行结束后、结算之前被用户删除的，记 `failed / workflow_run_deleted` 并释放；节点会话由执行桥自己结束，定时侧没有会话要 End。还是 pending 的运行说明启动没有提交（进程重启过），取消它并记 `execution_not_started`。工作流 occurrence 的结果就是它的运行，`…/runs/{oid}/result` 返回 `404 execution_result_not_found`，页面按 `workflowRunId` 读运行详情。

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

成员是显式类型化引用：人类引用 user 身份，Agent 引用 Space Agent 身份，不使用裸 memberId。人类必须是当前有效的租户成员；Agent 必须是同工作区活动、观测为已安装且由系统预装的可执行 Space Agent。市场 Agent 的安装标记不进入小队和任务候选。当前工作区成员可读取小队与成员；编辑、增员、移除成员与归档只限工作区 owner（v2 只有 owner/member，没有 admin）或本队有效人类维护者。有效人类成员可以自己退出所在小队：被移除的是调用者本人时跳过维护者检查，即使他是最后一名维护者也成功退出，退出保留编组历史；不设"最后维护者"保护，退出后允许零维护者，之后只有工作区 owner 能恢复维护者。本队有效人类维护者可增删其他维护者的标记；Agent 不能成为维护者。leader 必须引用本队所属空间的活动 Space Agent，本队维护者或工作区 owner 可设置或清除。写操作按小队 version 做乐观校验：过期返回 409 `version_conflict`，缺 version 返回 428，归档后返回 409 `squad_archived`。归档结束全部有效编组、保留历史且不提供恢复；移除现任 leader 会在同一事务清除 leader 配置。

Agent 成员只引用 `v2_space_agents` 里由系统预装、可执行且观测为已安装的插件。Plugins 页的市场 Agent 选择不改变这个小队/任务候选集合，也不使用虚构 Agent。

小队与成员关系不授予私人模型、Git 凭据或运行时权限，也不触发 Agent 执行、leader 路由或自动委派。

### 任务

任务使用独立候选契约 [`tasks.openapi.json`](tasks.openapi.json)。正式 Cloud 的任务挂在 issue 与
运行之下，没有这样一条独立任务记录，本批不修改 `api/openapi.json`；正式契约收敛时替换这一适配。

| 路由 | 行为 |
|---|---|
| `GET /api/v2/tenants/{tid}/tasks` | 列出工作区全部任务，最近变更在前 |
| `POST /api/v2/tenants/{tid}/tasks` | 创建任务，返回 201，初始状态 `pending`、version 为 1；不接受调用方给 version |
| `GET /api/v2/tenants/{tid}/tasks/{taskid}` | 任务详情 |
| `PUT /api/v2/tenants/{tid}/tasks/{taskid}` | 携带 version 修改标题、描述与所属项目 |
| `POST /api/v2/tenants/{tid}/tasks/{taskid}/status` | 携带 version 按生命周期推进状态；确认完成和取消只限任务创建人和工作区 owner，其他人 `403 forbidden` |
| `PUT /api/v2/tenants/{tid}/tasks/{taskid}/assignment` | 携带 version 指派或清除指派人，显式 `null` 表示清除 |
| `GET /api/v2/tenants/{tid}/tasks/{taskid}/timeline` | 任务的运行与评论，各按时间先后 |
| `GET /api/v2/tenants/{tid}/tasks/{taskid}/executors` | 能运行这个任务的执行器：指派给 Agent 时是该 Agent 的执行器（执行目录里 `agentId` 等于该 Agent 插件标识的条目）；指派给人时，只有负责人本人能读，返回他能用的全部执行器，别人是 409 `not_assignee`；指派给小队或未指派是 409 `not_agent_task` |
| `POST /api/v2/tenants/{tid}/tasks/{taskid}/runs` | 必须带 `Idempotency-Key`；`{executorId?, note?}` 用调用者身份开一次新运行，返回 201。见下文「任务运行」 |
| `POST /api/v2/tenants/{tid}/tasks/{taskid}/runs/{rid}/stop` | 结束一次活着的运行：正在执行的一轮先取消，空闲的会话直接结束；任何成员都可以停 |
| `GET /api/v2/tenants/{tid}/tasks/{taskid}/runs/{rid}/events` | 运行会话的 SSE，形状与对话事件流相同；工作区任何成员都能读 |
| `POST /api/v2/tenants/{tid}/tasks/{taskid}/comments` | 必须带 `Idempotency-Key`；`{body, mentions?, toAgent?, executorId?, workflow?}` 记一条评论，`toAgent` 同时发给 Agent；`workflow` 按发布工作流的 Start 表单校验输入后以评论人身份启动运行并在评论上返回 `workflowRun` 摘要 |
| `GET /api/v2/tenants/{tid}/decomposition-workflows` | 拆分选择器专用目录，返回工作流摘要及服务端判定的 `canDecompose`、`decompositionBlockedReason`；独立候选契约 [`decomposition-workflows.openapi.json`](decomposition-workflows.openapi.json) |
| `GET /api/v2/tenants/{tid}/tasks/{taskid}/decompositions` | 任务的拆分记录，最新在前；运行已结束但还没结算的先结算再返回 |
| `POST /api/v2/tenants/{tid}/tasks/{taskid}/decompositions` | 必须带 `Idempotency-Key`；`{workflowId}` 用一个已发布的工作流拆分任务，返回 201。见下文「任务拆分」 |
| `GET /api/v2/tenants/{tid}/task-mentions` | 调用者还没回复的 @：每个未结束任务一条，取别人最近一次 @ 调用者、且调用者此后没在该任务评论过的那条 |
| `GET /api/v2/tenants/{tid}/notifications` | 调用者在本团队的应用内提醒，最新在前，分页（`limit` 1–100，默认 100；`cursor` 取上一页的 `nextCursor`，最后一页为空）；`unread` 是全部未读数，顺着 `nextCursor` 能翻到每一条。每条带任务标题与状态、发起人、@ 评论摘要。见下文「应用内提醒」 |
| `POST /api/v2/tenants/{tid}/notifications/read` | `{ids}` 或 `{all: true}` 把调用者自己的提醒标为已读，返回剩余未读数；别人的 id 忽略 |

任务归属于一个工作区项目，`projectId` 必须在同一工作区内，否则 400 `invalid_project`。指派人显式类型化，
与小队成员一致：`human` 引用当前有效的租户成员，`agent` 引用同工作区活动且可执行的预装 Space Agent，
`squad` 引用未归档小队；名字与 Agent 的 `namespace/identifier` 由实时 join 读取，不在任务行上复制。
`squad` 只表示该小队负责这件事，不派发给某个成员。指派接口里缺 `assignee` 字段是 400
`invalid_assignee`，只有显式 `null` 才清除，避免客户端缺陷悄悄解除指派。

状态为 `pending → running → review → done`，`cancelled` 可从任一未终结状态到达，`done` 与 `cancelled`
终结且不能重开；跳过验收是 400 `invalid_transition`，不是版本冲突。同一函数
[`store.CanTransitionTask`](internal/store/tasks.go) 供人工推进和执行服务共用，执行服务经
`TransitionTaskStatus(ctx, tenant, id, from, to)` 在运行开始与结束时移动任务，`from` 防止过期运行覆盖人工修改。
运行按 [`store.CanRunTransitionTask`](internal/store/tasks.go) 移动任务：开始或追问进入 `running`（从 `pending`
或 `review`），一轮结束（成功或失败）进入 `review`，被停止或中断回到 `pending`；后两种人不能手动做。
Agent 正在执行一轮时，人工推进返回 409 `run_active`。

当前工作区任意成员都可读写、指派与推进任务：v2 没有任务级归属，谁来改由版本号防止互相覆盖。
过期 version 返回 409 `version_conflict`，缺 version 返回 428 `version_required`；跨租户的任务与项目
一律按不存在处理（404 或 400 `invalid_project`）。请求大小限制 64 KiB，拒绝未知字段与多余的 JSON 值。
附件不提供。

#### 任务运行

任务运行（[`internal/taskrun`](internal/taskrun/taskrun.go)，表 `v2_task_runs`）是任务所指派 Agent（派给人的任务是负责人选的 Agent）的一次执行会话，
经公共执行服务以调用方类型 `task` 运行，Caller ID 是运行 ID。和对话不同，它属于团队：

- **身份归发起人**：会话在点「开始处理」的人的 scope 下运行。工作区任何成员都能读运行和它的事件流
  （服务端校验读者是成员后，用发起人的 scope 读），但只有发起人能发下一轮：别人代发，等于用发起人的身份（以后是他的模型凭据）跑。
  其他成员发给 Agent 是 409 `run_owned`，他们可以重新执行，用自己的身份开一次新运行。
- **一个任务最多一个活着的运行**：与活工作流处理也互斥（409 `run_active`），空闲 Agent 会话必须先结束才能交给工作流。重新执行会先结束空闲的旧会话（`endReason = replaced`）；正在执行一轮时是 409 `run_active`，要先停。
  页面不排队，追问在一轮执行中同样是 409 `run_active`。
- **停止**：正在执行的一轮被取消（见下文「停止一轮」）。会话在停止后保留时，运行仍然活着、空闲，等发起人追问，任务回到待处理；停止让会话结束了（Node 没能保留它），或者运行本来就空闲，就结束这次运行（`endReason = stopped`），之后只能重新执行。任务完成或取消时结束空闲会话（`closed`）。
- **回合交付**：完成或执行失败的回合，在交付为 `delivered` 或 `empty` 后进入待验收。交付失败或停止回到待处理，交付失败的原因保存在交付记录中。交付期间任务仍为进行中，人工推进返回 409 `run_active`。
- **重启**：进程启动前结束上一个进程留下的活运行，正在执行或交付的任务回到 `pending`。启动补交付只保存原工作目录的内容，不重放回合。
- **派给人的任务**：负责人本人可以用自己选的 Agent 处理，执行器就决定了 Agent。只有负责人能开运行（包括重新执行和 `toAgent` 评论开的新运行），
  别人是 409 `not_assignee`；别人仍能看过程、停止。跑完、停止对任务状态的推进与 Agent 任务相同。
- **执行器**：`executorId` 必须是上面 `executors` 列出的执行器，否则 400 `invalid_executor`；不填时选目录中的第一项，目录为空时不启动执行。
- **列表**：任务带 `activeRun`（活着的运行的发起人、执行器与 `running`/`idle`），列表不用逐个读时间线就能显示谁在干活。

评论存在 `v2_task_comments`，全团队可见。`mentions` 只接受从 @ 菜单选出的类型化对象（`user` 为有效成员、`agent` 为预装且已安装的
Space Agent），写入时解析名字；正文里手打的「@名字」不路由。`toAgent` 把评论作为一轮发给 Agent：调用者自己的活运行就追加到同一会话，
没有活运行就用调用者身份开新运行。`task-mentions` 列出还在等回复的 @：被 @ 的人在任务里评论一次就算回复，
自己 @ 自己和已完成、已取消任务上的 @ 不算。每条 @ 同时产生一条应用内提醒（见下文「应用内提醒」）。

#### 应用内提醒 / In-app notifications

第一版只有应用内提醒，不发邮件、不接企业 IM，前端每 15 秒读一次（没有推送通道）。提醒存在 `v2_notifications`，
在引起它的那次写入的同一事务里记下，三类：

| 类型 | 什么时候 | 发给谁 |
|---|---|---|
| `assigned` | 建任务时带指派人、改派（含工作流「创建任务」表单节点建的任务）；重复提交同一指派人不算 | 指派给人：这个人；指派给小队：小队当前的人类成员；指派给 Agent、清除指派：不发 |
| `mentioned` | 新评论 @ 了人（普通评论和 @工作流评论都算；同一 `Idempotency-Key` 重试不重复） | 被 @ 的人；@ Agent、@工作流不发 |
| `review` | 任务进入 `review`：手动推进、Agent 运行一轮结束、处理工作流结算 | 任务创建人 |

- 引起它的人自己不收（自己指派给自己、自己 @ 自己、创建人自己手动提交验收）；Agent 运行和工作流引起的没有发起人（`actorId` 为 null）。
- 只发给有权看到任务的当前成员：写入前对收件人做与租户路由相同的 `TenantAccess` 判断，不通过就不写。判断前先对收件人的成员行加 `FOR KEY SHARE` 锁并持有到提交，
  并发移出成员要么先提交（这时跳过这个人），要么等这次写入提交后再删，提醒的外键不会失败、不会连带回滚评论或状态变更。
  一个事务里的全部收件人用一条语句按 `user_id` 升序加锁（与 `RemoveMember` 锁 owner 和成员行的顺序相同），两者不会互相等待成死锁；
  工作流表单一次建多个任务时，提醒在事务末尾一起记下。
- 身份比较统一用小写 UUID：请求里大写的同一个人不算改派，大写的自己也不会收到自己的提醒。
- 提醒行外键挂在 `memberships` 上并级联删除：成员被移出团队时，他在该团队的提醒一并删除；读接口本身要求是当前成员，
  被移出的人读不到（404），重新加入也不会看到旧提醒。
- 每个事件对每个收件人只记一条（`source` 唯一）：指派与待验收按任务版本，@ 按评论。

#### 任务工作流处理 / Task workflow handling

正式 `api/openapi.json` 没有以下两条任务工作流路由。候选契约为
[`task-workflows.openapi.json`](task-workflows.openapi.json)，独立 Orval 配置为
`frontend-v2/orval.task-workflows.config.ts`；仅用于 v2，不修改 Cloud 契约。

| 路由 / Route | 行为 / Behavior |
|---|---|
| `POST /api/v2/tenants/{tid}/tasks/{taskid}/workflow-runs` | 必须带 `Idempotency-Key`；`workflowId`、可选 `snapshotId`、`input`、Start `values`；以负责人身份启动冻结的发布版本 / Requires an idempotency key; starts a frozen published snapshot as the assignee |
| `GET /api/v2/tenants/{tid}/tasks/{taskid}/workflow-runs` | 返回处理、拆分、巡检来源、评论 @ 四类记录及工作流名称、版本、真实发起人、状态和失败原因 / Lists handling, decomposition, inspection sources and mention comments with workflow name, version, actual starter, status and failure |

权限与 Agent 任务运行共用同一函数：派给人时仅本人可启动，派给小队时仅当前人类队员可启动，
派给 Agent 时工作区成员均可启动；不符合指派范围返回 409 `not_assignee`，未指派返回
409 `not_agent_task`。项目由任务固定，草稿不可运行。两种运行都锁任务行准入；活 Agent 会话
（含空闲会话）与处理工作流互斥，冲突为 409 `run_active`。处理期间人工推进（包括关闭任务）
同样返回该错误。能查看任务的成员在任务的「工作流」页签打开处理运行后点击「停止」（列表也提供入口），
任务回到 `pending` 后再操作；权限与对话运行视图的取消接口一致，不要求本人是发起人。

启动前先读取并固定发布快照，调用执行引擎相同的图可执行性、Start 类型、必填和选项校验。
这些拒绝返回 400，不创建运行或关联，也不改变任务状态。随后在一个数据库事务里进行准入、
按 `CanRunTransitionTask` 进入 `running` 并写关联及关联自己的状态修订号。事务提交后的准备、校验或启动
发生错误时，先重读运行：仍未执行的（`pending`，或 `started_at` 为空的 `cancelled`）在同一个事务里取消
运行、把关联记为 `failed` 并保存原因（停止先于启动时原因固定为「启动前被停止」），只有本次处理关联
仍拥有它写入的 `running` 状态时才恢复启动前状态；已经开始执行的运行改经工作流宿主取消（连同已派发的
Agent 一轮；停止未确认时由运行自己的 worker 收尾），再由正常结算记录真实结果，恢复原状态是错的。
清理遇到暂时错误会由受进程管理的 worker 重试。已执行的运行不允许按拒绝结算。
每个任务使用自己的进程锁，持有到首次调度返回，防止同键重试越过尚未确定的启动结果；其他任务不等待；持有者与排队者全部退出后即删除该锁条目。

成功或执行失败从 `running` 进入 `review`，执行后取消或重启中断进入 `pending`；执行前就被停止
（`started_at` 为空的 `cancelled`）的启动不算发生过，关联记为 `failed`（「启动前被停止」）并恢复原任务
状态，与从未执行的删除准入同规则。结算同时校验处理关联的身份、状态修订号和任务状态；仅状态写入会撤销该关联的状态所有权，
所以描述、标题、项目和指派修改不影响结算，`running → pending → running` 则不会被旧运行覆盖。
旧的 `task_version` 列保留兼容，不再用于结算。重启先结算已完成运行，再取消未完成的处理运行并标为 `interrupted`，
不恢复执行。拆分和巡检不推进任务状态，巡检可关联多个任务。

同一任务、发起人、幂等键返回原运行，终态也一样；同键不同请求体仍沿用原请求。对话框复用普通
运行的类型化 Start 字段，固定显示的发布快照；HTTP 错误或请求体改变后使用新键，只有相同请求的
网络失败重试保留原键。页签只在有活运行时轮询；读取只结算尚未释放的处理关联，历史读取不加写锁。
巡检来源查询使用 `results @>` 与 GIN 索引。删除运行历史后，处理和拆分记录仍可见，标为已删除且
不可打开。处理关联尚未结算时按运行表里保留的状态结算：执行后取消仍回到 `pending`；从未执行的
`pending` 准入（含删除）以及执行前就被停止的运行按启动拒绝处理，恢复原任务状态。
运行详情的声明式 store 方法提供 `taskId`，拆分关联按租户与运行 ID 建索引。
390px 导航完整换行，开关文字和时间不截断，工作区使用可用全宽。巡检任务描述不再写对话页 URL。

Agent and workflow task starts share one permission function: the named human, current human squad
members, or any workspace member for an Agent-assigned task. Out-of-scope callers receive 409
`not_assignee`; unassigned tasks receive `not_agent_task`. Both kinds lock the task row for admission.
Live Agent sessions, including idle sessions, and handling workflows exclude each other with 409
`run_active`. Manual status changes, including closing a task, are refused while handling is live;
any member who can see the task opens the handling run in its Workflow tab and clicks 停止 (also
available in the list) to return it to pending, using the chat run view’s existing cancellation
endpoint without a starter-only restriction.

Before admission, the service freezes a published snapshot and applies the engine's executable-graph
and Start-input checks. Refusals return 400 without creating a run/link or moving the task. Admission,
the running transition and the versioned link commit together. Any later preparation or start error
re-reads the run: one that never executed (pending, or cancelled with a NULL started_at) is cancelled
and settled failed with its reason in one transaction — a run stopped before start records exactly
that reason — restoring the original task status only while the handling link still owns its admitted
running status; a run that already began executing is cancelled through the workflow host, dispatched
Agent turn included (the run's own worker finishes an unconfirmed stop), and settles with its real
outcome, since restoring the original status would misreport work that really ran. Process-owned
workers retry transient cleanup failures, and an executed run is no longer refuseable. Per-task
locks remain held through the first scheduling pass so same-key retries cannot overtake its outcome;
unrelated tasks proceed independently. Lock entries are removed when their last holder or waiter exits.

Execution success/failure moves running to review; cancellation after execution and restart
interruption move it to pending. A start stopped before it ever executed (cancelled with a NULL
started_at) never happened: the link records the stop as failed and the task returns to its original
status, the same rule as a never-executed deleted admission. Settlement fences on the handling link’s identity/revision and admitted task status. Only status
writes revoke that ownership; metadata edits and reassignment do not prevent settlement, while a
running/pending/running cycle invalidates it. The legacy task_version column is retained but unused.
Recovery settles completed runs normally, cancels unfinished handling runs, and marks them interrupted.
Decomposition and inspection do not move tasks. Reusing task/caller/key replays the original run;
the dialog rotates its key after HTTP errors or body edits, retaining it only for identical network
retries. It shares the normal typed Start fields and sends the displayed snapshot ID.

List reads settle only unsettled links and query history once; the frontend polls only live runs
and immediately refreshes task status/version when handling settles.
Inspection sources use indexed JSON containment (`results @>` with GIN). Deleted handling and
decomposition runs remain listed but cannot open. An unsettled handling link uses the stored run
outcome: a cancellation after execution still returns the task to pending, while a never-executed
pending admission (deleted or not) and a run stopped before it ever executed restore the original
task status as refused starts. A declared store method supplies task backlinks;
decomposition lookups use a tenant/run index. Phone navigation wraps fully, toggle labels and times
remain readable, and workspaces use the available width. Inspection descriptions omit chat-page URLs.

Desktop references (read-only at `ebd5b725`): explicit published choices follow
`packages/app-shell/src/features/workspace/sidebar-create-menu.tsx:52-65`; execution project and
workflow identity follow `packages/app-shell/src/features/workflow-run/run-workflow-dialog.tsx:73-81`.
Task ownership and status policy are Cloud v2 owner decisions, since Desktop tasks are workspaces.

#### 任务拆分

任务拆分（[`internal/taskdecomp`](internal/taskdecomp/taskdecomp.go)，表 `v2_task_decompositions`）是 Tech Lead 在协作页点「拆分」：
用一个已发布的工作流（演示里是「需求拆分」）把大需求拆成子任务。

- **一次拆分 = 一次工作流运行**：用点「拆分」的人的身份运行工作流的已发布版本，输入是任务标题和描述，项目是任务的项目。
  运行和普通运行一样出现在工作流的运行历史里。同一任务同时只有一次进行中的拆分（409 `decomposition_active`）；
  已结束的任务（409 `task_closed`）和子任务（409 `subtask_not_decomposable`）不能拆；工作流不存在或没有发布版本是 400 `invalid_workflow`，已发布但缺少拆分输出的工作流是 400 `decomposition_output_required`，在创建运行前拒绝。运行固定使用通过检查的快照，检查后重新发布不会替换它。
- **输出要求 / Output requirement**：发布快照的 `output` 节点必须声明名为 `tasks` 的输出；其类型只能是 `array`、`array[object]`、`string`（JSON 编码的数组）或未声明类型 / `any`，其他类型（包括 `array[string]` 等元素不是对象的数组）不能用于拆分，在创建运行前拒绝。运行固定使用通过检查的快照，即使期间发布了别的版本。服务在 `GET …/decomposition-workflows` 的每个摘要里返回 `canDecompose` 和 `decompositionBlockedReason`，选择器据此把不符合的工作流放进禁用分组，不重复实现规则；判定按已发布快照 ID 缓存在内存里（已发布快照不可变，最多 1024 条，满了淘汰最早的），只有未缓存的快照才读取和解析图。运行后的解析失败也会附上这项要求。
  The `output` node of the published snapshot must declare an output named `tasks`; its type can only be `array`, `array[object]`, `string` (a JSON-encoded array), or undeclared / `any`. Any other type (including arrays whose elements are not objects, such as `array[string]`) cannot be used for decomposition and is rejected before a run is created. The run uses the snapshot that passed the check, even if another version is published meanwhile. The server returns `canDecompose` and `decompositionBlockedReason` in every summary of `GET …/decomposition-workflows`, and the picker uses them to put non-conforming workflows in a disabled group without re-implementing the rule; verdicts are cached in memory by published snapshot ID (published snapshots are immutable; at most 1024 entries, the oldest is evicted when full), and only snapshots not yet cached have their graph read and parsed. Parsing failures after a run also carry this requirement.
- **目录开销 / Catalog cost**：判定一个未缓存的快照要读出整张图并完整解析一次（[`decomposition_catalog.go`](internal/server/decomposition_catalog.go)）。实测 5 MiB 的图：读取一行约分配 13 MiB（pgx 行缓冲加字符串拷贝），`workflow.ParseGraph` 约 26 MiB（UTF-8 / 转义校验前的 `[]byte` 拷贝 5 MiB，`json.Decoder` 缓冲增长和解码后的值约 21 MiB），`NewPool` 可忽略，单个约 25 ms。这些是分配总量，解析完即可回收，常驻的只有判定结果。重启后缓存为空，20 个这样的工作流让首次请求分配约 740 MiB、耗时约 470 ms，但峰值堆约 60 MiB；之后的请求只查一次 ID（约 1 ms）。并发请求同一快照时共用一次读取和解析，整个进程同时最多解析 `maxConcurrentVerdictParses = 2` 个快照，其余排队，所以 K 个成员同时打开选择器的开销和一个人打开相同（K=5 时约 730 MiB 分配、峰值约 60 MiB），最坏峰值约为 2 × 单图解析开销，不随并发数增长。共用的判定脱离发起请求的生命周期运行，但不超过服务进程的生命周期：最长 30 秒，服务关闭时立即停止，结果写入缓存供等待者使用；先关掉选择器的请求只是不再等待。判定中途 panic 只让这次请求返回 500 `internal`（记录一次日志和调用栈，不写缓存），不会让进程崩溃；数据库出错同样返回 500，也不会被记成“不可拆分”。缓存最多 `maxDecompositionVerdicts = 65536` 条（每条约 100 字节，满了约 6.5 MiB），满了淘汰最早写入的。每次请求都按固定顺序遍历一个工作区的工作流，所以一旦所有被打开过选择器的工作区的已发布快照合计超过这个上限，淘汰总是先删掉下一次要用的条目，命中率会直接降到 0：之后每次打开选择器都要重新读取、解析全部图，代价回到上面的首次请求开销。上限因此设得远高于演示部署的规模，而不是贴近它。
  Judging an uncached snapshot reads the whole graph and parses it once ([`decomposition_catalog.go`](internal/server/decomposition_catalog.go)). Measured on a 5 MiB graph: reading the row allocates about 13 MiB (pgx row buffer plus the string copy), `workflow.ParseGraph` about 26 MiB (a 5 MiB `[]byte` copy for the UTF-8 / escape check, about 21 MiB of `json.Decoder` buffer growth and decoded values), `NewPool` is negligible, about 25 ms each. These are total allocations, collectable as soon as the parse ends; only the verdict stays resident. After a restart the cache is empty: 20 such workflows make the first request allocate about 740 MiB and take about 470 ms, with a peak heap of about 60 MiB; later requests run one id query (about 1 ms). Concurrent requests for the same snapshot share one read and parse, and the whole process parses at most `maxConcurrentVerdictParses = 2` snapshots at a time while the rest queue, so K members opening the picker at once cost the same as one (K=5: about 730 MiB allocated, peak about 60 MiB); the worst-case peak is about 2 × one graph's parse cost and does not grow with concurrency. A shared judgement runs detached from the request that started it but never beyond the server process: for at most 30 seconds, and it stops at once on shutdown; it caches its answer for the waiters, and a request whose picker closes first just stops waiting. A panic during a judgement only fails that request with 500 `internal` (logged once with its stack, not cached) instead of crashing the process; a database error likewise returns 500 and is never recorded as "not decomposable". The cache holds at most `maxDecompositionVerdicts = 65536` entries (about 100 bytes each, about 6.5 MiB when full) and evicts the oldest insertion when full. Every request walks one workspace's workflows in a fixed order, so once the published snapshots of all workspaces whose picker is opened exceed the bound together, eviction always removes the entry needed next and the hit rate drops straight to 0: every picker open then re-reads and re-parses every graph, the first-request cost above. The bound is therefore set far above a demo deployment's size rather than close to it.
- **子任务**：运行成功后，输出的 `tasks` 数组（每项 `title`、可选的 `description`，也接受整段数组的 JSON 字符串）变成任务的子任务：
  `v2_tasks.parent_id` 指向大需求，项目同大需求，创建人是发起人，不指派，状态 `pending`，按输出顺序排在列表里。只有一层，子任务不再拆。
  标题和描述超长的部分截断；一次最多 20 个。
- **幂等**：结束拆分和写子任务在同一个事务里，以拆分仍是 `running` 为条件，所以同一次运行无论被看到结束几次（后台观察、页面读取、重启恢复），子任务只写一次。
- **失败可见**：运行失败或被取消、输出里没有 `tasks` 数组、数组为空或某项缺标题、执行桥拒绝启动，拆分都记为 `failed` 并写明原因（`failure`），可以重新拆分。
- **重启**：拆分不跨进程。启动时在工作流执行桥的 boot sweep **之前**结算上一个进程留下的拆分（顺序由 `internal/boot` 统一编排，`main.go` 与测试共用）：运行已结束的照常结算（成功的建子任务），仍未结束的先取消其运行，再把拆分记为失败（「服务重启，拆分已中断，请重新拆分。」）。所以 sweep 不会吸收并恢复一个拆分运行，拆分记录也不会和继续执行的运行不一致；已结算的拆分运行不能续跑，也不能重试任务创建（D32），所以它也不会在拆分结束后重新跑起来。唯一例外是审核表单已提交的拆分运行：它不被取消，由执行桥的表单恢复重放提交后的确定性任务步骤（sweep 不处理表单运行，也就不会吸收它），sweep 之后拆分再按运行的真实结果结算（运行仍未结束的继续观察到结束）。

HTTP 集成测试按此候选 OpenAPI 校验每一次响应，并覆盖创建、编辑、合法与非法状态迁移、三种指派、
版本冲突和越权读取，以及开始、停止、评论、@、时间线和事件流；数据库测试另覆盖跨租户隔离与执行服务的推进；
`internal/taskrun` 用内存执行端口和真实 PostgreSQL 覆盖发起人身份、追问权限、停止、重新执行、关闭任务与重启中断。

### 协作 @ 目录

`GET /api/v1/tenants/{tid}/collaboration/targets` 按正式契约实现子集：`user`（工作区有效成员，交互 `mention`）、
`agent`（预装且已安装的 Space Agent，交互 `task`，`description` 是插件标识）与 `workflow`（租户内未删除且已发布的工作流，
交互 `form`，`formRef` 指向工作流 ID）。未发布或已删除的工作流不返回；正式契约里的 `team` 仍不返回（v2 不向小队自动派发）。

`GET /api/v1/tenants/{tid}/collaboration/forms/{formRef}` 按正式契约返回 `FormDescriptor`，从该工作流当前激活的**已发布快照**
根节点 `Start` 的 `inputVariables` 生成启动表单字段（忽略未发布草稿改动）；未发布、已删除或跨租户工作流返回 `404 form_descriptor_not_found`。

与正式 Cloud 的已知差异：
- `collaboration/targets` 只列出**已发布**的未删除工作流；Cloud 会列出所有未删除工作流。
- `collaboration/forms/{formRef}` 只投影根节点 `Start` 的 `inputVariables`，不含 `globalVariables`、版本选择器或模型槽位字段；`file-list`（`array[file]`）与 `json`（`object`、`array`、`array[...]`）变量映射为 `textarea`。
- 可选查询参数 `issueId` 在 v2 被忽略，传其他租户或不存在的 `issueId` 仍返回 200（Cloud 对未知或跨租户 `issueId` 返回 404）。
- 通过 `POST /api/v2/tenants/{tid}/tasks/{taskid}/comments` `@` 工作流提交时，对声明为 `object`、`array`、`array[...]`、`array[file]` 的文本输入在 `workflow.CheckStart` 前先按 JSON 解码（非法 JSON 返回 `400 workflow_run_input_invalid`、`reason: "type_mismatch"`）；直接传结构化 JSON 值也照常接受。
- 普通评论与工作流评论共用 `(task_id, author_id, create_key)`：同一个 key 换内容仍返回原评论，不比较请求体；正式 Cloud 返回 `409 idempotency_conflict`。
- 工作流评论在准备或启动被拒绝、且运行从未开始执行时（包括在提交评论后、运行开始前发生进程重启的场景，由 `taskworkflow.Host.Recover` 清理），事务撤销评论、运行及初始 scope，释放 key；同 key 重试可重新启动。若 `Start` 已进入执行后才报错，则保留评论与运行，确认停止后标为已取消并记录安全的失败原因；同 key 重试返回这条记录，不重复执行。后一种情况与正式 Cloud 的失败请求不留记录约定不同，重新发起须使用新 key。
- 已软删除运行的评论卡片与 `GET /api/v2/tenants/{tid}/tasks/{taskid}/workflow-runs` 只保留名称、版本和状态，不返回其输出或失败原因，前端也不渲染这些内容。

### 项目仓库

项目使用独立同步候选契约 [`projects.openapi.json`](internal/projectrepos/projects.openapi.json)：
`GET/POST /api/v2/tenants/{tid}/projects`、`GET /api/v2/tenants/{tid}/projects/{pid}`、
`POST /api/v2/tenants/{tid}/projects/{pid}/sync` 和
`PUT`/`DELETE /api/v2/tenants/{tid}/projects/{pid}/repository-token`。
创建只接受匿名 HTTPS 仓库 URL 与分支（私有仓库可同时给出令牌），完成一次 mirror clone、验证分支并固定
snapshot 后才写入已有的 PostgreSQL 项目记录；clone 或分支检查失败不创建项目。

**手动同步**（A4 D3）：项目所在团队的成员点「同步」后，Cloud 在进程内把绑定的分支取进镜像（完整历史、
不取 tag），写入新的快照 ref `refs/ora/upstreams/<pid>/snapshots/<commit>`（只新建、不改写），再把项目的当前
快照指向它；之后新建的会话以它为基准，已有会话目录不变。每个项目同时最多一次同步，进行中再点共享同一次
的结果；没有 webhook、轮询或建会话时的隐式同步。外部分支被强推时照常跟随并标注，旧快照保留；分支删除、
不可达、认证失败或超时会话不变，原因写入项目记录并在项目页显示，不自动重试。Cloud 重启会把进行中的同步
标为中断。

**私有仓库令牌**（A4 D4）：每个项目至多一个，建项目时填写，之后只由 owner 或项目创建者更换/删除。令牌用
部署配置的密钥（复用 `V2_MARKET_CREDENTIAL_KEY`）以 AES-256-GCM 加密后存 PostgreSQL，附加数据绑定租户、
项目和仓库地址；密钥缺失时 Cloud 照常启动，需要令牌的拉取失败并写明原因。明文只在执行一条外部 Git 命令
时交给子进程（`http.extraHeader`），不进命令行、URL、Git 配置、日志和 API 应答；API 只返回是否已配置和最后
更换时间，也不下发给 Node。

列表按 ID 升序分页，读取要求当前租户成员关系；创建者来自登录身份。响应不暴露 mirror 路径、令牌密文或
snapshot ref。

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
目录只包含部署配置的真实 provider，不再默认提供 `fixture`。完整 OpenAI 兼容 provider 配置提供
`configured` 模型和 `opencode:configured` 执行器；完整 Anthropic 兼容 provider 配置提供
`claude:configured` 执行器。没有配置模型时两个目录都为空，服务仍可启动并返回可理解的空状态；
无 Node 资源时执行器清单也为空。fixture 只供内部测试使用：Playwright 启动 v2-server 时显式传
`-test-fixtures`，`-fixture-check` 也只在自身进程内合入 fixture 目录。
每个 Agent 只用自己协议的绑定：OpenCode 走 OpenAI chat completions，Claude Code 走 Anthropic Messages；
Messages 绑定只给 Agent，不出现在模型清单里，纯模型对话不能选它。清单里 OpenCode 排在前面，默认选择不变。


### 工作区角色

角色是工作区的 Agent 目录，使用独立候选契约 [`roles.openapi.json`](roles.openapi.json)：设置页的
「角色」和所有工作流节点选择器读同一份目录，执行时从这里读取内容注入提示词。

| 路由 | 行为 |
|---|---|
| `GET /api/v2/tenants/{tid}/roles` | 工作区的活动角色，最近更新在前；租户内所有成员看到同一份目录 |
| `POST /api/v2/tenants/{tid}/roles` | 新建，返回 201；`(tenant_id, lower(name))` 在未删除的行里唯一，同名返回 409 |
| `GET`/`PUT`/`DELETE /api/v2/tenants/{tid}/roles/{rid}` | 读取、替换或软删除；跨租户和已删除行都按不存在处理 |
| `POST /api/v2/tenants/{tid}/roles/import` | 导入一个 1 MiB 以内的角色 Markdown：YAML front matter 的 `name`、`description` 加正文。同名返回 409，显式 `overwrite` 才覆盖并保留 id |

形状校验在 `internal/roledoc`：标题 1–80 个字符、不含换行或路径分隔符（中文等 Unicode 字符可用），
描述最多 500 个字符，正文最多 100,000 个字符；导入文件必须声明非空的 `description`。错误码用
稳定的 `role_*`，写请求拒绝未知字段。不内置默认角色、不做一次性 seed：目录从空开始，部署版与
演示版都没有预置的「代码审查」「技术写作」。角色按 `tenant_id` 共享，所有成员都可以增删改和导入，
规则和 workflows、squads 一样。

运行时按 desktop `ebd5b725` 的语义执行：

- **启动时预检整张图**：`StartRun` 在创建第一次调度前收集冻结图里所有可执行 Agent 节点引用的角色
  （根图与每个 Loop 循环体；备用节点不参与），逐个在当前租户的角色目录里解析。**条件分支里本次
  不会触发的节点同样参与检查**：桌面端把角色当作启动硬依赖，缺角色就拒绝启动，v2 与之一致。
  失败返回 `400 workflow_role_not_found`，运行保持 `pending`，补建同名角色后可以直接启动。
- **节点启动前再读一次**：`Host.runAgent` 在启动会话前按运行作用域实时解析角色（先按名称不区分
  大小写，找不到再按 id 兜底，兼容旧图存 id 的写法），所以运行期间改名或改内容会作用于之后启动的
  节点。启动后角色被删除时，该节点以 `workflow_role_not_found` 失败，`resumable: true`、
  `auto_retryable: false`：补建角色后「从失败处继续」即可。
- **注入格式**：角色正文非空白时，按 desktop 的 `<system_instructions>` 模板注入到
  `<workspace_root>` 之后、`<current_workflow_step>` 之前，标签内先写角色说明和角色的引用名
  （空引用回退到「工作流角色」/“workflow role”），正文为空时不发这一段。执行器与模型选择的解析
  不变。

### Workflow analysis additions (English)

The v2-only `POST …/workflows/analyze` response adds `unappliedSettings` and `unsupportedNodes`
in `workflows.openapi.json`. Analysis does not save or rewrite the graph. For each Agent,
including spare nodes, `unappliedSettings` calls the same decision as `Node.Agent().Unapplied`, the
exact source of attempt payloads (`unappliedAgentSettings` in `internal/workflow`); both pass the
node's Iteration or Loop, and a spare node is placed by the container it is authored in.
Interactive mode (a Loop member only, which v2 runs automatically; a root-graph interactive
Agent parks for its starter) is currently saved but unapplied, and so are a root interactive
Agent's `completionSignal: "agent"` and `autoAcceptAgentCompletion: true`: the agent policy needs
the host completion tool, which v2 cannot hand to the Agent yet (circus#1242), so such a node
completes as `end_turn`. Roles and Skills are applied.
Nonempty MCP selections fail explicitly until Node and Controller lifecycle support is ready. Retry
follows desktop's effective policy (`crates/application/src/workflow_run/engine/retry.rs:66-74` at
`ebd5b725`): an absent `retry` is the default policy (enabled, 2 retries) and is listed; only
`enabled: false` or `maxRetries: 0` means no retry and is not listed. Desktop never retries an
interactive node, which a person drives (`node_runtime.rs:373-379`), so an interactive Agent never
lists `retry`; settings no longer listed lose their editor hint automatically.

`unsupportedNodes` uses the engine’s `runtimes` table and execution projection to name reachable
`prompt` and `tool` nodes, including reachable Loop bodies. Spare nodes are omitted; without a
root Start the list is empty because start fails for the missing entry first. The frontend warns
before start; the server still rejects with `400 workflow_run_start_failed`. Desktop also does
not execute these kinds: `crates/application/src/workflow_run/engine/node_type.rs:56` at
`ebd5b725a8a21d72ecf5e6fb7ced10d007b72dfe`. No setting application or execution behavior changes.

### 工作流定义

工作流定义按 desktop 的编辑模型提供，使用独立候选契约 [`workflows.openapi.json`](workflows.openapi.json)，不修改正式 Cloud 的 workflow 路由（其 snapshot/publish 语义与 desktop 不同，等 #827 收口后再谈收敛）。每个定义恰好有一个可变的草稿快照（`version` 为保留字 `draft`），发布时把草稿冻结成不可变版本并把它设为活跃版本；回滚只把历史图灌回草稿，激活才移动活跃指针。时间戳按 desktop 契约用 Unix 毫秒，`graph` 按编辑器的原始字符串存取，不重新序列化。

| 路由 | 行为 |
|---|---|
| `GET …/workflows` | 租户内未删除定义，最新创建在前；只带发布版本号，不带图 |
| `POST …/workflows` | 创建定义与草稿，返回 201；名字 trim 后非空且同租户不重名（不区分 ASCII 大小写） |
| `POST …/workflows/analyze` | 分析编辑器提交的图，不保存：未参与执行的节点、无法识别的节点类型，以及 v2 新增的两项——`unappliedSettings`（每个 Agent 节点保存了但运行不应用的设置，可达和备用节点都列）与 `unsupportedNodes`（执行路径上启动会拒绝的已知类型节点，即 `prompt` / `tool`；没有根 Start 时为空） |
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

写操作按定义行加锁串行：发布在同一事务里读草稿、校验并冻结，避免校验过的图和冻结的图不是同一份。请求体限制 6 MiB、拒绝未知字段；响应错误沿用 Cloud 的 `{code, params, requestId}`，领域错误码与 desktop 一致（如 `workflow_name_conflict`、`workflow_version_already_exists`、`workflow_cannot_delete_active_version`、`workflow_run_graph_parse`）。`workflow_name_conflict` 只用于手动创建或重命名工作流；工作流插件导入遇到同名定义会自动改名，不返回该错误。

图规则不属于 HTTP 层：`server.Options.AnalyzeWorkflow` 提供草稿分析，`Options.ValidateWorkflow` 提供发布前的可执行性校验，两者由工作流领域注入；未注入时定义读写照常，分析和发布返回 `503 workflow_domain_unavailable`，不会把未校验的文档伪装成可运行版本。

与 desktop 的明确差异：v2 以租户代替 desktop 的单一 `local` 命名空间，`namespace` 恒为 `local`；重名改为由数据库唯一索引拒绝并映射为 409（desktop 并发重名可能返回 500）；发布校验放在事务内（desktop 存在校验与冻结之间的 TOCTOU）；软删记录 `deleted_at`（desktop 丢弃快照删除时间）；版本列表在同一毫秒内用 id 兜底排序；名称唯一性用 PostgreSQL `lower()`，比 desktop 的 ASCII `NOCASE` 范围更宽。

### 工作流运行历史

运行记录同样在 [`workflows.openapi.json`](workflows.openapi.json) 里：创建时冻结一个已发布版本、项目、输入与触发身份，图不复制，历史读取始终回到冻结快照。状态存 `pending`/`running`/`succeeded`/`failed`/`cancelled`；`awaitingInput` 是「running 且有 pending 节点」的派生值，不落库。

| 路由 | 行为 |
|---|---|
| `POST …/workflows/{wid}/runs` | 冻结并创建 `pending` 运行；`snapshotId` 缺省取当前活跃版本，草稿快照拒绝；`projectId` 必填且必须是本租户项目 |
| `GET …/workflows/{wid}/runs` | 该定义的全部运行（最新创建在后，与 desktop 的列表顺序一致），带冻结版本号 |
| `GET …/workflow-runs?projectId=` | 按项目列运行；不传项目则列出本租户全部运行 |
| `GET …/workflow-runs/{rid}` | 运行、节点（每个身份只返回最新一次尝试）、Loop 轮次 scope，以及照 desktop 投影的 `variables`（根变量池）和 `failedAttempts`（被替换的失败尝试） |
| `GET …/workflow-runs/{rid}/snapshot` | 运行冻结的快照图；定义发布新版本后历史仍按原图渲染 |
| `DELETE …/workflow-runs/{rid}` | 软删已结束的运行及其节点；running 或还有 pending/running 节点时拒绝 |
| `POST …/workflow-runs/{rid}/start` | 交给执行桥启动运行 |
| `POST …/workflow-runs/{rid}/cancel` | 取消运行；停止未确认时返回 `503 workflow_run_stop_unconfirmed` |
| `GET …/workflow-runs/{rid}/resume-preview` | v2 专属（契约之外）：现在「从失败处继续」会做什么——从哪些失败或取消的节点继续、哪些 Loop / Iteration 从第一轮重来、清掉哪些节点；被拒时 `resumable: false` 并给出 `reason`。见下文「从失败处继续」 |
| `POST …/workflow-runs/{rid}/resume` | v2 专属（契约之外）：在同一次运行里从失败处继续，保留工作区文件；被拒返回 `409 workflow_run_not_resumable`（`params.reason`、`params.runningNodeIds`）。权限同取消 |
| `PATCH …/workflow-runs/{rid}/input` | 更新启动输入与变量；执行桥判定不可编辑时返回 `409 workflow_run_not_editable`（desktop 允许未开始和已结束的运行修改，用于准备下一次执行） |
| `POST …/workflow-runs/{rid}/nodes/{nodeId}/complete` | 完成等待输入的交互式 Agent 节点（`force`、`confirmInitialTurn`、`requester: human`），只有发起人可以操作，规则见下文“交互节点的完成门禁” |
| `POST …/workflow-runs/{rid}/nodes/{nodeId}/turns` | v2 独有：发起人向等待输入的交互式节点发一轮消息（`{"content"}`，1–16000 字），在节点自己的会话里执行；响应除 `run` 外带 `sent`，是这条消息在发起人发给该节点的消息里的序号 |
| `POST …/workflow-runs/{rid}/nodes/{nodeId}/stop` | v2 独有：停止交互式节点正在执行的一轮（见下文「停止一轮」）。之后节点等发起人：会话保留时可以接着发消息，停止让会话结束了就只能完成；运行继续 |
| `GET …/workflow-runs/{rid}/nodes` | 只要节点行 |
| `GET …/workflow-runs/{rid}/events` | SSE，只发 `workflow_run_invalidated`（data `{"runId"}`，不带状态，同 desktop 的同名事件）：连上先发一条，之后每次提交的变化一条，未读的合并；页面收到后重新读取运行。未注入运行事件端口时返回 `503 workflow_run_events_unavailable` |
| `GET …/workflow-runs/{rid}/nodes/{nodeRunId}/events?after=` | 一次节点尝试的会话事件，帧格式、游标和 `end`/`failure` 与对话的事件流相同；校验当前成员租户归属后按运行发起人身份读共享会话（没有记录发起人的运行回退为调用者），尝试不属于该运行返回 `404 workflow_node_run_not_found`，还没派发（`sessionId` 为空）返回 `404 workflow_node_session_not_found` |

- **冻结与身份**：`triggerKind`/`triggerRef` 记录手动或定时触发的稳定身份，定时侧只创建与跟踪，不复制图校验或引擎；运行与节点身份稳定，Loop 轮次是 `v2_workflow_scopes` 的一行（同一 Loop 同时最多一个活动轮），Iteration 轮次是节点行的 `iteration`（0 基）。
- **attempt 就是软删行**：失败尝试把节点行软删，再插入下一次尝试；同一 `(scope, node, 迭代轮)` 只允许一行未删记录，数据库唯一索引兜住并发派发。
- **引用保护**：运行冻结的版本不能被删除（`400 workflow_snapshot_in_use`），还有未删运行的定义也不能删（`400 workflow_active_runs`）；把已结束的唯一运行删掉（`DELETE …/workflow-runs/{rid}`）后，版本和定义才重新可删。运行中或还有未完成节点的运行拒删（`400 workflow_run_active`），避免删掉执行桥仍在写的会话行。
- **交互节点接口不在正式契约里**：`api/openapi.json` 在节点级只有 `GET …/runs/{rid}/nodes/{nodeId}/session`，没有发消息和停止本轮，所以 `turns`、`stop` 只在 v2，是 Cloud 的候选接口。The interactive routes are v2-only: Cloud's `api/openapi.json` has only `GET …/runs/{rid}/nodes/{nodeId}/session` at node level, so `turns` and `stop` are candidates for Cloud.
- **执行桥接缝**：`server.Options.WorkflowRuns`（`server.WorkflowRunHost`）由执行桥实现、在 `main` 注入；未注入时 start/cancel/input/complete 返回 `503 workflow_run_host_unavailable`，定义与历史读取照常。续跑是同一个执行桥上可选的 `server.WorkflowRunResumer`，没有实现它时 resume 与 resume-preview 同样返回 503；拒绝用 `*server.WorkflowRunNotResumable` 带出拒绝原因。HTTP 只做登录/成员校验、严格解码和错误码映射，派发、停止确认、会话与对话历史都留在执行桥；拒绝用 server 包的哨兵错误表达（`ErrWorkflowRunNotStartable`、`ErrWorkflowRunNotEditable`、`ErrWorkflowNodeNotAwaitingInput`、`ErrWorkflowNodeNotFound`、`ErrWorkflowRunStopUnconfirmed`、`WorkflowRunInputInvalid`）。
- **进程内存储端口**：执行桥直接调用 store 上的状态转移，不经 HTTP——`StartWorkflowRun`、`CreateWorkflowNodeRun`、`BindWorkflowNodeSession`、`CompleteWorkflowNodeRun`、`FailWorkflowNodeRun`（失败 attempt 号在同一事务里分配，payload 由领域构造）、`RetryWorkflowNodeRun`（失败行软删 + 插入等待运行的下一 attempt，编号连续）、`WakeWorkflowNodeRun`（唤醒等待的重试，重复唤醒 no-op）、`ResumeWorkflowEngineRun`（从失败处继续：运行锁内按领域给出的计划软删节点行、关闭被清 Loop 的轮次、清 payload，运行回到 running；缺省发起人时在同一事务提交前补记）、`FinishWorkflowRun`（以失败结束时同一事务取消等待中的重试）、`AbandonWorkflowRetryWait`（运行已结束时收尾单个等待行）、`CancelWorkflowRun`（同一事务里收尾活动 Loop 轮次并去掉等待中重试的 `retry_wait`）、`StartWorkflowLoopRound`（同一 Loop 只允许一个活动轮）、`SettleWorkflowLoopRound`（重复结算同状态为 no-op）、`DeleteWorkflowRun`。节点完成对已终态尝试是 no-op，取消对已取消运行幂等。取消也接受 `pending` 运行（desktop 只接受 `running`，其余返回 `NotActive`）：v2 的取消是用户可重试的操作，执行桥确认停止前重复调用必须返回同一个已取消运行，排队中的运行也要能直接从页面撤掉。
- **事件流与工作流节点会话**：运行事件流订阅 `server.Options.WorkflowRunEvents`，`main` 注入执行桥的 `workflowhost.Hub`。节点会话事件流以节点尝试为键（重试是新的尝试和新的会话）：公共执行服务把会话记在启动运行的人（`started_by`，定时触发时是任务的创建者）的 scope 下；服务端先校验调用者的租户成员身份和运行归属，再按运行记录的 `started_by` 身份向公共执行服务订阅会话和读取轮次提示，因此节点会话对整个租户可见（跨租户或未派发仍返回 404），包括 Agent 读取的文件内容和工具调用；在共享运行里选用个人 Skill，就等于把它的内容共享给能看到这次运行的所有成员（从任务评论里发起的运行同样适用，发起人就是评论人）；同样，若运行启用 `injectLastFailure` 把上次失败的文字注入 `agentPrompt`（`workflowhost/prompt.go`），同租户成员也都能在重跑或续跑尝试的 `turn_ended.prompt` 里看到这段内容。没有记录发起人的运行读取节点会话时回退为调用者；成功续跑这样的运行时，在 `ResumeWorkflowEngineRun` 事务内补记发起人（已有节点会话时，`WorkflowResumeStarter` 按最早优先取所有者仍是本租户有效成员的会话，否则取续跑的人；续跑被拒绝时不改动）。补记会扩大可见范围：这些早先的节点会话原先只有所有者自己能读，补记后同租户成员都能读，和续跑开的会话一致。运行详情的 `startedBy` 给出启动门禁记下的发起人，页面据此只给发起人显示交互节点的操作，运行详情和运行列表另带 `startedByName` 供显示。
- **分支决策投影**：根 Condition 决策在 `conditionDecisions`，Iteration 决策在 `iterationConditionDecisions`（`nodeId#round`），Loop 决策留在每轮 `scope.state`。
- **变量与失败尝试投影**：运行 payload 的其余部分不公开；除了 Condition 的 `conditionDecisions`，详情再投影 desktop `GetWorkflowRunResponse` 里的两项。`variables` 读 `variablePool` 的 catalog（`<节点>.<名字>`，按键排序），`sourceNodeId` 取 `writer`，没赋值的不带 `value`，赋了 `null` 的带 `null`；跳过旧的 `selected_branch_id` 路由项，键不合法的项跳过而不让整个详情失败（desktop 在这里报 payload 损坏）。运行开始前没有变量池，读出空列表。`failedAttempts` 是被软删的 `failed` 节点行（desktop 的 `list_failed_attempts`），取各行 `payload.error_detail`，没有的跳过；只有重试、续跑和整次重跑会软删失败行；v2 由自动重试（根作用域与 Loop / Iteration 成员）和「从失败处继续」软删（v2 没有整次重跑），其余情况下为空，当前失败的尝试就是节点自己的行。
- **能力边界**：支持在同一次运行里「从失败处继续」（只保留现状，不回滚文件，见下文）；整次运行 restart、checkpoint 回滚和换用新发布版本续跑不在演示范围，顶栏“再跑一次”创建新运行。重启中断由执行桥提供，见下文“工作流执行”。

运行页按选中的 Loop scope 或 Iteration 下标投影节点状态，并以该次尝试的 `nodeRunId` 订阅会话事件。该轮未执行的成员没有尝试身份，不继承前一轮的会话或执行元数据。启动输入和根变量只覆盖根 Start，不覆盖 Loop 的本轮入口；当根 Start 声明了名为 `input` 的显式启动变量且已赋值时，`StartAs` 保留该显式值、不再用 kickoff `input` 覆盖（与 desktop 先写变量再镜像 kickoff 相反，按 `#1235` 有意保留）。`failedAttempts` 按发生它的作用域归属：根作用域的尝试归根节点，Iteration 尝试归它的轮次，Loop 尝试归它的轮次 scope；前端按选中的轮次展示，跨轮不串。

### 对话

对话页使用独立候选契约 [`chat.openapi.json`](chat.openapi.json)。正式契约里的对话挂在 issue 下，没有独立的对话列表。前端以独立 Orval 配置生成客户端。领域逻辑在 [`internal/chat`](internal/chat)，只经 [`internal/execution`](internal/execution/README.md) 的公共接口运行 Agent。

| 路由 | 行为 |
|---|---|
| `GET …/chat/executors` | 可选的 Agent 与模型绑定，来自公共执行目录；包含 `agentId` / `modelId`，供工作流选择器保存真实执行身份 |
| `GET …/chat/conversations` | 本人在该租户的对话，按创建时间排序，各带自己的执行 session |
| `POST …/chat/conversations` | 以第一条消息创建对话并 `Start`，返回 201 和该轮的 session/turn |
| `PATCH …/chat/conversations/{cid}` | 改名 |
| `DELETE …/chat/conversations/{cid}` | 先 `End` 活 session，确认后再删除对话，同一事务里登记回收它的工作目录并立即执行；可选 `workspace=reclaim`（默认）/ `discard`（须带预览的 `revision`），返回工作目录的结果（见下文「删除对话时回收工作目录」） |
| `GET …/chat/conversations/{cid}/deletion` | 删除前预览工作目录会怎样：能否恢复及原因、未交付的改动、有无被忽略的文件、可下载的最近交付、`revision` |
| `POST …/chat/conversations/{cid}/messages` | 在对话的 session 上 `Append` 下一条消息 |
| `POST …/chat/conversations/{cid}/stop` | `Cancel` 最近一轮 |
| `GET …/chat/conversations/{cid}/events` | 对话 session 的事件，Server-Sent Events |
| `GET`/`PUT …/chat/project-order` | 本人在左栏的项目顺序 |

`…` 是 `/api/v2/tenants/{tid}`。所有路由要求登录 cookie 和成员关系；他人的对话按 404 处理。执行服务和目录取自统一注入点 `server.Options.Execution` / `Catalog`（见下一节）；它们还是未配置的 Stub 时，对话列表照常读取，执行器清单、创建对话和发消息等需要执行的调用返回 503 `execution_unavailable`。

- **一个对话、一个 session**：创建对话时只选项目，执行服务为这个对话新建目录，对话之间、对话与定时任务和工作流之间都不争抢目录。session 在两轮之间保持，直到下列情况之一，并记下 `endReason`：
  - `stopped`：停止让 session 结束了（Node 没能在取消这一轮后保留它，见下文「停止一轮」）。只停了这一轮、session 保留时不记结束原因，对话照常接着发消息。
  - `deleted`：删除对话。
  - `interrupted`：在对话之外结束，例如服务重启后 `Append` 返回 `ErrSessionEnded`。

  session 结束后对话只读：再发消息返回 409 `conversation_ended`，页面提示新开对话。不会在同一对话里新开 session，也不重放上下文。
- **幂等**：创建对话和发消息必须带 `Idempotency-Key`，它就是该轮的 attempt ID。用同一个 key 重试，返回原对话和原 Ref，不会重复执行；同一个 key 换了参数，返回 409 `idempotency_conflict`。结果未知时返回 503 `outcome_unknown`，要用同一个 key 重试。第一条消息被明确拒绝时不留下空对话。
- **上一轮未结束或正在交付**：返回 409 `turn_running`，由页面排队，交付有结果后再发。交付失败单独记录，不结束仍健康的 Agent 会话。
- **事件流**：每条消息的 `id` 是 session 内序号，`Last-Event-ID`（或 `after`）从其后续上。`session_ended` 之后发 `end` 事件；读取失败发 `failure`，不会冒充正常结束。快照 `history` 每次整份替换，`records[].data` 是固定 Node 的 HistoryLine。关闭事件流只停止观察，不停止 Agent。
- **简化**：同一对话的操作用进程内互斥串行，依赖 v2-server 是单进程；首版只接文本消息，必须选项目。

装配：`server.New` 用传入的 store 和注入的 `Execution` / `Catalog` 建一个 `chat.Service`，chat 不改 `main.go` 或启动入口。chat 只调用公共 `execution.Service`，不直接写执行存储的事件、终态或 End；chat 自己的表只记录对话、session 归属和结束原因。

#### 删除对话时回收工作目录

用户删除普通对话时，它的工作目录在成果能从回合交付恢复、或用户选了 `discard` 时才删除。判定、状态与运维见 [`internal/workspacereclaim`](internal/workspacereclaim)：

- **能恢复**（按登记的 session 直接查 `v2_execution_turns` / `v2_turn_deliveries` / `v2_turn_delivery_work`，对话行删了也能判）：有交付记录（否则 `legacy`）；没有未结束的回合和交付义务（否则 `delivering`）；最后一轮的交付是 `delivered`/`empty`（否则 `last_turn_undelivered`）；Agent 仓库没有 `refs/stash`、`refs/remotes/` 之外的 ref 都剥成提交且不超出起点与 `HEAD` 的祖先（伪 ref 如 `ORIG_HEAD` 不算）、快照里没有与起点不同的 gitlink、顶层以下没有任何名为 `.git` 的条目（目录、gitfile、链接都算，被忽略的目录也扫）、`.git/modules/` 下没有已初始化的子模块、HEAD 是起点或其祖先或它跟踪的每个路径都与交付树或起点树一致、index 没有冲突且每个条目都与交付树、起点树或（未暂存时）HEAD 一致（否则 `git_state`，预览的 `git` 列出 stash 条数、各 ref 超出的提交数、嵌套仓库路径、`headDiffers`/`headCommits`、`indexDiffers`）；`.git` 解析不出来、HEAD 无法解析成可读的提交（只有直接目标分支不存在的 unborn HEAD 可跳过，符号引用不递归）、磁盘上记录的 ref（loose 与 packed-refs，只看 Git 会当作 ref 的名字，过期的 `.lock` 等跳过）格式不对或 Git 读不出来、Git 明确报对象缺失或损坏，都判 `unverifiable`；I/O、权限、超时等其他 Git 失败按 `check_failed` 重试；持交付的目录锁按交付快照规则重新生成树（只写临时目录），与最后一轮交付的树（`empty` 时起点的树）相同（否则 `changed_after_delivery`，生成不了为 `unverifiable`）。
- **预览**：`workspace`（`none` 附 `no_workspace` / `workspace_missing`；`recoverable`；`undelivered` 附上面的原因）、`ignored`、`undeliveredChanges`（相对最近一次 `delivered` 交付，没有时相对起点，至多 200 条）与 `undeliveredTotal`、`latestDelivery`（页面经 `…/workspace/deliveries/{id}/patch` 下载）、`revision`（最后一轮、交付、判定、快照树、Git 统计，以及 discard 会删掉、交付又带不走的全部 Git 相关内容的字节摘要：顶层仓库的 ref/HEAD/stash OID 与 index 内容，每个嵌套仓库整棵子树（工作树与 `.git`，含对象）的内容，gitfile、链接或 `.git` 目录里的 `commondir` 指向 checkout 内的 Git 目录时连同目标整棵（同一目录只算一次）；任何字节变化都会改变它）、`discardable`（只有未交付且内容摘要完整时为 true；字节或条目超限、特殊文件、读取失败、指针指向 checkout 外或解析不了都为 false，没有检查就为 false；此时 discard 返回 409 `discard_unverifiable`，界面不给丢弃选项）。
- **删除**：删除请求在删除事务提交后用服务生命周期的上下文（不随请求取消）启动回收，5 秒内删完返回 `reclaimed`，否则返回 `pending`/`in_progress`、后台继续删完（关机时取消，记录留 `deleting` 给启动）；事务提交后的任何错误都只体现在结果里，不报成删除失败。`reclaim` 不需要 `revision`，删除时重新判定，结果只会更保守。`discard` 必须带回预览的 `revision`，在结束会话之前重新计算，不一致或还在交付返回 409 `deletion_preview_stale`，对话和目录都不动。返回 `workspace`：`reclaimed`（附 `releasedBytes`、`releasedFiles`）、`kept`（附原因）、`pending`（`in_progress` 后台继续；`delivering`、`removal_failed`、`check_failed` 下次启动再做；`reclaim_unavailable` 等带运行时的启动）、`none`。
- **先登记再删**：`store.DeleteChatConversationReclaiming`（只有用户删除走它；首轮被拒回滚对话用的 `DeleteChatConversation` 不登记）在删除对话的同一事务里写一行 `v2_workspace_reclaims`：目录 ID、项目、session、路径、`clone_execution`、模式（不可变）。状态 `pending → deleting → reclaimed | kept`；`deleting` 之后只重查身份与路径，不再判定。启动时在 `boot.Recover` 里 `RecoverTurnDeliveries` 之后、恢复工作流之前，把 `pending` / `deleting` 的再执行一遍；运行中不另外触发，不加定时任务。同一条回收在进程内按目录 ID 互斥。
- **保留与重试**：`reclaim` 下不能恢复、身份不符（`identity_mismatch`：路径或 `clone_execution` 与 `v2_workspaces` 不一致或路径被别的行记录）、别的 session 用过（`shared`）、仍有活 session（`session_live`）、路径不安全（`unsafe_path`）记 `kept`；检查时（读工作目录、镜像、Git 失败，`check_failed`）或删除时（`removal_failed`）的基础设施错误保持原状态，`attempts` 加一、记 `last_error` 与日志，下次启动重试；只有目录内容本身无法验证（Git 元数据不能安全读取、改动过大、分不清物化技能）才判 `unverifiable`。
- **精确删除**：加锁顺序 `LockWorkspace` → `skillsMu` → `p.mu`。`agentruntime.Driver.RemoveCheckout` 只在 `skillsMu`（项目 runtime 不在时用按项目 ID 的锁，`d.project()` 插入新 runtime 前也先拿它）下确认 `p.owners` 为空、摘掉 `p.checkouts` 与 `skill-checkouts.json` 里的项，随即释放；统计和删除只持交付的目录锁，同项目的会话照常启动。技能投影遇到已不存在的 checkout 跳过。`workspacefs.RemoveCheckout` 要求路径在 `<V2_DATA_DIR>/nodes/<项目摘要>/w/r` 之下、逐级 `Lstat` 都不是链接，`os.OpenRoot` 打开后比对 dev/inode，从 checkout 根起经目录句柄逐级打开（每级 `Lstat` 不是链接、打开后比对 dev/inode），统计（普通文件逻辑大小之和、所有条目数；列出后消失的文件不计，其他读不到的条目让本次删除失败待重试）、补属主写权限、删内容都经 checkout 句柄，最后经父目录句柄删目录本身，不再按路径重新解析。
- **不碰**：`v2_workspaces`、交付记录与 ref、镜像、Node 会话历史和 Node 账本。工作流和任务运行的目录、本功能上线前已删除对话的目录都不回收，后者只能停服后 `-cleanup`。
- **运维**：`v2-server reclaims [pending|deleting|kept|reclaimed ...]` 只读列出回收记录（服务运行时可用）；`v2-server discard-kept <目录 ID>` 停服后丢弃一条 `kept`（持数据目录锁，按同样的身份与路径检查删除，记 `reclaimed`、原因 `operator_discard`）。已回收目录的成果按记录里的交付提交与起点提交在项目镜像上恢复：`git diff --binary <起点> <交付提交>` 或 `git archive <交付提交>`。
- **Git 版本**：回收用到的 Git 命令和参数在 Git 2.34（Ubuntu 22.04 自带）上可用，相关测试在 2.34.1 与 2.53 上都通过。HEAD 的形态直接读 HEAD 文件判断，不依赖较新的 `symbolic-ref --no-recurse`；旧版 Git 对读不到的对象和缺失的对象报同样的话，所以"对象缺失"还要在磁盘上确认。
- **没有 Agent 运行时**（未配置 `V2_NODE_ASSETS_DIR` 或没有模型）时不起回收器，但之前带运行时的进程留下的 checkout 还在：有 checkout 的对话，预览和 `discard` 在结束会话、删除对话之前返回 503 `workspace_reclaim_unavailable`；默认删除照常删对话并登记 `pending`（`reclaim_unavailable`），等以后带运行时的启动按同样的条件判定。

HTTP 集成测试按 `chat.openapi.json` 验证 JSON 响应，向注入点传入 `executiontest.Stub`，覆盖对话生命周期、停止后只读、事件流续传、项目顺序和执行不可用；`internal/chat` 的测试用真实 PostgreSQL 和执行端口替身，覆盖幂等重试、结果未知、上一轮未结束、停止后保留会话接着发消息、停止让会话结束与重启后只读和删除顺序。

#### 停止一轮

对话、任务运行和工作流交互节点的「停止」都经 `execution.Service.Cancel` 停这一轮：它让 driver 只停这一轮（`StopTurn`），driver 不能保留会话时才结束会话。v2-server 在 Hello 里声明 `turn_cancel`；Node 也支持时（握手回 `turn_cancel: true` 和同名能力），新会话在 `StartAgentSession` 里选择 `keep_session_on_cancel`，停止发 `CancelTurn{command_id, turn_id}`，按 Node 的 `TurnCancelSettled` 决定：`stopped`、`not_started` 只停这一轮，会话保留；`already_ended` 是这一轮已经自己结束，保留它自己的结果；`session_ending` 表示 Node 不保留会话，它自己停 Agent 并在取得停止证据后发终态。Node 不支持 `turn_cancel` 时照旧发 `EndSession`，停止就结束会话。依据是 specs 的 `decisions/node/protocol/20261116-cancel-turn-and-keep-session-after-turn-ends.md`。

提交被 Node 以 `mcp_update_required` 拒绝、还在等 MCP 刷新后重发的一轮，Node 并没有这一轮（发 `CancelTurn` 只会得到 `unknown`），所以不发 `CancelTurn`：这一轮在本地记为 `cancelled`、不再重发，会话保留。

等结论的规则（v2 对 ADR D4「按投递进展等待」的实现）：发出 `CancelTurn` 后等 30 秒；等待期间这个会话每收到一条消息就重新计 30 秒，所以投递积压不会被当成 Node 不结算；整个等待不超过 `execution.TurnStopSettleLimit`（60 秒）。等满、Node 拒绝这条命令（`session_command_rejected`）或回 `unknown` 时，退回 `EndSession` 结束会话；之后才到的 `stopped`、`not_started` 不再保留会话，这一轮按会话丢失（`AgentLost`）报告。等待只属于停止本身：`End`、`Close` 和进程关停照旧取消这一轮的执行，等待中也立刻结束会话，不等结论。调用方给停止定的时限要加上 `TurnStopSettleLimit`，工作流的停止本轮、取消运行就是这样（`stopTimeout` + 60 秒），Node 结算得慢不会被报成 `workflow_run_stop_unconfirmed`。等结论期间 Node 断开时，这一轮仍记为用户的停止（`cancelled`），会话结束，不按 Node 丢失恢复。

任务运行的「停止」是在一轮执行时发出的，就只停那一轮：等到拿到任务锁时那一轮已经结束了（例如另一个成员同时点了停止、会话保留），它什么也不做，不会把空闲的运行结束掉；结束空闲运行是在运行空闲时发出的「停止」（页面上的「结束会话」）。

用户停的这一轮以 `cancelled` 结束，即使它最近的模型请求被限值切断过（只有不是用户停的一轮才按切断原因记为 `failed`，模型代理 ADR `20261113-…` D4）。

简化（不是 Cloud 的行为）：

- 只选 `keep_session_on_cancel`，不选 `keep_session_on_failure`：失败的一轮仍然结束会话，等失败原因能上报（circus#1138）再打开。
- 等 `TurnCancelSettled` 用固定窗口加「收到消息就重新计时」近似 D4，没有按确认进度判断，并且设了总上限（见上）。
- 随后反正会结束会话的调用方（取消工作流运行、自动执行的工作流节点、定时对话的取消）也先走 `CancelTurn`，再 `End`：结果和直接结束一样，只是多一次往返，停止多花 Node 结算的时间（最多 G + Q）。
- 升级锁定的 Node 会把 `V2_DATA_DIR` 下保留的 Node 账本迁到 v16（desktop#802 的停止证明 `lock_processes_and_marks` 和尝试范围标志；从 v14 升级的账本经过 desktop#793 的 v15 一次升到 v16）。迁移是单向的：锁定更早 Node 的 v2（包括锁定 desktop#793 合并 `2fc5e2ca`、只认到 v15 的版本）打不开它。把 v2 回滚到锁定旧 Node 的版本之前，要先停掉 v2 并清掉 Node 账本（各项目目录下的 Node 数据库），和其他账本升级一样。
- 还在准备工作区（Agent 尚未启动）时停止，照旧结束会话。
- 提交刚写给 Node、Node 的 `mcp_update_required` 拒绝还没到时停止，这一轮还没被标为被拒，于是照常发 `CancelTurn`，Node 回 `unknown`，退回 `EndSession` 结束会话：结果安全，和不保留会话时一样，只是这次没能保留会话。
- 任务运行的「停止」按请求到达服务端时的状态判断要停的是不是一轮：页面还显示「正在执行」、而那一轮在请求到达前已经结束时，这个「停止」会结束空闲运行。要彻底区分，得让 Stop 带上目标轮次 ID（契约改动），v2 不做。
- 会话能否保留由 Node 判定：Agent 版本不在 Node 的轮次边界清单里，或这一轮不满足静止条件时，结论是 `session_ending`，会话照旧结束。

#### 会话结束时杀掉的进程

会话以任何方式结束时（「结束会话」、删除对话、没能保留会话的「停止一轮」、工作流节点结束会话或取消运行、服务关闭），Node 在报告会话已停止之前，杀掉这个会话的 Agent 起的、带尝试环境标记（`ORA_SESSION_ATTEMPT`）的所有进程，所以 v2 收到停止确认之后再交付、回收工作区或开始后继节点时，不会再有这些进程写工作区。停止时 Agent 工具起的后台作业也会被杀，包括开发服务器、Gradle、sccache 这类守护进程；需要在会话结束后继续运行的进程，要在 Ora 之外启动。轮次之间、保留了会话的「停止一轮」不杀任何进程，后台作业一直运行到会话结束。这是 desktop#802 的行为，依据是 specs 的 `decisions/node/agent-runtime/20261120-attempt-mark-in-session-stop-evidence.md`；Node 把这种停止证据记为 `lock_processes_and_marks`，v2 不读这个值。

保证等级仍是 BestEffort，只在 Linux 上提供。以下进程 Node 找不到，会在会话结束后继续运行、继续写工作区：清空或改写了自己环境的孤儿进程（例如 Agent 起的 PostgreSQL、nginx 会用进程标题覆盖环境区域）、经 `sudo` 等换了用户的进程、经 `docker run` 或 `systemd-run` 由别的服务代为启动的进程。v2 的 `-cleanup` 和工作区回收都不按 cwd 杀进程，不补这个缺口。

Node 进程本身被杀（崩溃、OOM、SIGKILL）时，它来不及杀后台作业，作业带着标记留在工作区里。Node 的重启恢复在开放控制 socket 之前，会对账本里没确认停止的尝试做一遍收口，杀掉找到的带标记进程。v2 借它收口：清理项目的运行目录时（见 [`internal/node`](internal/node/README.md)），等 3 秒后仍有进程留在运行目录里、其中有带这个项目 Node 账本里尝试标记的进程，v2 就用上一次的 Node 可执行文件和配置把这个项目的 Node 启动一次，等它开放控制 socket 就停掉它，然后照旧严格确认运行目录里已没有进程。socket 开放只说明这一遍做过，不说明作业都死了，算不算清理完成只看之后这次确认。杀作业的是 Node，v2 只给自己复制进运行目录的 Node、reaper 发信号；v2 认标记的规则比 Node 更严（例如还要求用户一致），认漏了只会让清理照旧失败，不会误杀或误判完成。带别的项目或别的 Node 尝试标记的进程不算这个项目的，不会因为它们启动 Node，也不会被这次启动杀掉。这次启动用的配置引用了上一个服务实例的 TLS 证书等文件，停止确认之前它们一直保留；万一缺了，Node 起不来，清理照旧失败。

清理在这些时候进行：崩溃时正在执行的一轮以 Node 丢失结束后，执行服务立即结束这个会话；同项目的下一个会话、删除对话、v2-server 启动（v2 和 Node 一起被杀之后）和 `-cleanup`。所以崩溃那一轮的会话在作业被杀之后才确认停止，这一轮的交付照常进行，不和作业抢写；同项目的新会话在作业被杀之后才开始，不再以 `agent_prepare_failed` 失败，只多花一次 Node 启动的时间（实测约 1 秒，最多等 20 秒）。会话空闲时（两轮之间）Node 崩溃，没有轮次结束来触发清理，作业运行到这个项目下次被用到或 v2-server 重启。与 desktop 的差异：desktop 在 Node 退出后重新启动它，重启恢复随即杀掉作业；v2 的项目 Node 按需启动，所以由清理来启动它。

以下情况和以前一样：项目保持不可用（新会话以 `agent_prepare_failed` 结束，v2-server 启动时则启动失败），直到进程自己退出。运行目录里留下的进程都没有这个项目的标记（例如清空了环境、父进程又已退出的孤儿，Node 也认不出它；或者用户自己的 shell），v2 不启动 Node；Node 启动失败、20 秒内没开放控制 socket，或者启动了却没杀掉作业（例如读 `environ` 卡住），也不算清理完成。cwd 不在运行目录里的作业本来就不挡 v2 的清理，会在这个项目的 Node 下次启动时被杀。

部署前提是 Node 与 Agent 以同一用户运行，或 Node 有 `CAP_SYS_PTRACE`，否则 Node 读不到 Agent 进程的环境，停止多半记为 `stop_unconfirmed`（Node 启动时只告警）。v2 生成的 Node 配置不设 `process.workload_uid`，Node 和 Agent 都以 v2-server 自己的用户运行，这个前提总是成立；把 Agent 改成以别的用户运行时要同时满足它。

### 回合交付

普通对话、任务 Agent 运行和工作流 Agent 节点的每一轮已接受消息在结束后交付，包含失败和停止。工作流节点完成操作和没有接受消息的尝试不产生交付。工作流共用目录的交付按顺序进行，其他仍在运行的节点可以继续写入。交付失败单独记录，工作流仍按执行结果推进。

交付在项目镜像里新建 `refs/ora/deliveries/<projectId>/<deliveryId>`（两个 ID 都是 UUID）。提交的唯一父提交是工作区登记的起点，包含 Git 纳入的工作区内容，忽略规则和 Ora 物化的技能目录照常排除。交付提交使用固定的 Ora 身份。同步只更新该项目绑定分支的快照，已创建的交付 ref 不会重写或删除。

先写 Git ref，再写 PostgreSQL 记录。起点和结果的树相同记为 `empty`，无 ref；大小超限或其他失败记为 `failed` 并保留原因。崩溃时写到镜像但未落库的 ref 不会从交付读接口发现。启动先结束未完成的执行，再补交付，之后才恢复工作流调度和接收请求。Agent 删掉 `.git` 的目录照常交付，但技能目录（`.opencode/skills`、`.claude/skills`）下有改动时记为 `delivery_unavailable`，因为已无法区分哪些是 Ora 物化的技能；`.git` 是指向仓库目录的符号链接时按 gitfile 指针处理。已清理的目录记录 `workspace_removed`；`-cleanup` 保留镜像与交付历史。

变更视图、单文件 diff 和 patch 下载从镜像读取交付，路由见下节。

如果交付失败记录也无法落库，轮次仍以 `delivery_unavailable` 结束，页面不再显示进行中。未落库的交付留待启动重试，当时工作目录可能已包含后续改动。

### Agent 工作区查看

对话页、任务运行页和工作流运行页的右侧面板只读查看 Agent 的工作副本和它的回合交付，使用独立候选契约 [`workspace.openapi.json`](workspace.openapi.json)。浏览文件读工作副本：正式 Cloud 中 Node 运行在沙盒里，服务端读不到工作副本，届时要改为经 Node 读取；这里因为 v2-server 与 Node 同机，直接读本机目录。变更和 patch 读项目镜像里的交付（见上节）。读取逻辑在 [`internal/workspacefs`](internal/workspacefs)。

| 路由 | 行为 |
|---|---|
| `GET …/chat/conversations/{cid}/workspace/entries?path=` | 对话工作副本的一层目录，目录在前；`path` 省略为根 |
| `GET …/chat/conversations/{cid}/workspace/file?path=` | 一个文本文件的内容 |
| `GET …/chat/conversations/{cid}/workspace/changes` | 最近一次 `delivered` 或 `empty` 交付相对起点的变更文件，带增删行数，并带 `deliveryState` |
| `GET …/chat/conversations/{cid}/workspace/diff?path=&deliveryId=` | 一个文件在该交付里相对起点的 unified diff；未变更时为空。`deliveryId` 可选，固定读同一来源的某次成功交付 |
| `GET …/chat/conversations/{cid}/workspace/deliveries/{deliveryId}/patch` | 下载一次交付相对起点的完整 patch（含新文件和二进制，`git apply` 可直接应用） |
| `GET …/workflow-runs/{rid}/workspace/{entries,file,changes,diff,deliveries/{deliveryId}/patch}` | 同上，读取该运行各 Agent 节点共用的工作副本和该运行的交付 |
| `GET …/tasks/{taskid}/runs/{rid}/workspace/{entries,file,changes,diff,deliveries/{deliveryId}/patch}` | 同上，读取这次任务运行的工作副本和交付 |

`…` 是 `/api/v2/tenants/{tid}`。对话只能读本人的（他人的按 404 `not_found`，和不存在的对话相同），运行与运行详情一样对租户成员可见。交付的读取权限与此相同；来源被删除后（删除对话、删除工作流运行），它的交付不再可读，交付 ref 留在镜像里。交付 ID 只在当前来源内查找，别的来源的 ID 返回 404 `delivery_not_found`。

- **变更视图**：`deliveryState.mode` 为 `delivery` 时，`viewDelivery` 是正在显示的成功交付（`empty` 也算，它取代之前的交付，文件列表为空），`latestDelivery` 是最近一次结果（含失败），`latestFailedDelivery` 是最近一次失败，`reasonCode`、`reason` 说明原因；`inProgress` 表示这个来源有一轮正在执行或正在交付，此时仍显示上一次交付。还没有成功交付时文件列表为空，`baseCommit` 取记录的起点。只要来源有一轮登记过交付，变更和 diff 就不再读工作副本，第一轮还在执行或第一次交付失败时也一样。
- **旧会话**：本功能上线前的会话没有登记过交付，`mode` 为 `working_directory`，照旧读工作副本：相对克隆时 commit 的变更文件，含未跟踪且未忽略的文件；工作副本被删后返回 410 `workspace_removed`。不为它们补交付记录。
- **固定某次交付的 diff**：`deliveryId` 格式不对返回 400 `invalid_delivery_id`，找不到返回 404 `delivery_not_found`，是失败的交付返回 409 `delivery_failed`（`params.reasonCode`、`params.reason`）；`viewDelivery` 是这次交付，其余字段反映来源当前状态。
- **patch 下载**：200 为 `application/octet-stream`，`Content-Disposition: attachment; filename="delivery-<id>.patch"`；`empty` 交付返回 204；失败的交付 409 `delivery_failed`；超过 `V2_DELIVERY_MAX_PATCH_BYTES` 返回 413 `patch_too_large`（`params.message` 为「改动过大，无法在线下载」，`params.maxBytes` 为上限），交付保留；镜像或交付 ref 读不到返回 503 `workspace_unavailable`。patch 先在上限内完整生成再发送，出错时不会有部分内容。整个进程同时最多生成 4 份 patch，更多的下载排队等待，调用方断开时放弃等待；因此 patch 占用的内存约为 4 份上限（缓冲扩容时最多约两倍）。交付只能下载 patch，没有 `git fetch` 入口，也没有任何写入交付或 ref 的路由。
- **缓存**：变更、diff 和 patch 的响应带 `Cache-Control: private, no-store`。
- **位置落库**：Agent 运行时在工作副本克隆并物化技能后、Agent 启动前，把 `WorkspaceID → 目录、克隆时 commit` 写进 `v2_workspaces`，所以会话结束、服务重启后仍能浏览。还没克隆好时浏览（以及旧会话的变更）返回 404 `workspace_not_ready`；目录已被 `-cleanup` 删除时浏览返回 410 `workspace_removed`，变更、diff 和 patch 照常读交付。
- **路径**：只接受相对路径，解析符号链接后必须仍在工作副本内，`.git` 不可见；越界返回 400 `invalid_path`。目录列表跳过指向工作副本外的符号链接。
- **上限**：文件 2 MiB、diff 输出 4 MiB（413 `too_large`），含 NUL 或非 UTF-8 的文件返回 422 `binary_file`；目录最多 5000 项、变更最多 2000 个文件，超出时 `truncated`。
- **不改 Agent 的 Git 状态**：旧会话的变更和 diff 在 index 的临时副本里把未跟踪文件标为 intent-to-add 后计算，Agent 的 index 不变。git 调用关闭 hooks、fsmonitor、外部 diff 和 textconv。v2 的 Agent 本来就以同一用户在本机执行命令，这些读取不增加它的权限。
- **简化**：不检测重命名（显示为删除加新增）；不做文件监听，页面在工具调用完成和一轮结束时刷新；工作流只到运行级，不区分节点各自改了哪些文件。没有交付历史列表，只能读最近的结果，或按已知 ID 固定 diff、下载 patch。Ora 物化进工作副本的技能不算变更，见「用户技能」。

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
同一对话还在执行时拒绝新轮次；workflow run 的每个会话
各自一次只执行一轮，同一 run 的多个会话可以同时执行。

有项目 Cancel 只停这一轮、会话照常 Append（Node 协商了 `turn_cancel` 并保留了会话，见「停止一轮」），否则结束整个物理会话，确认停止后后续 Append 返回 `ErrSessionEnded`；
纯模型 Cancel 只停止当前轮，之后可继续。取消已经结束的旧轮只返回原结果，不停止后来的轮次。
End 确认停止后持久化会话结束，历史仍可读；停止时 Agent 工具起的后台作业也会被杀，包括开发服务器、Gradle、sccache 这类守护进程，范围和缺口见「会话结束时杀掉的进程」。启动先恢复并停止旧 Node，再把未完成轮记为 interrupted、
将旧会话全部结束；不重放 prompt 或恢复 Agent 上下文。页面领域继续通过同一执行接口接入。

fixture 只替代模型，Git、Node、OpenCode / Claude Code 与 shell 工具都实际执行：

```sh
# 在仓库根目录准备资源，随后到 v2-server；资源和数据目录均使用 Linux 原生磁盘。
task v2:node:prepare
V2_LOGIN=email V2_NODE_ASSETS_DIR=/absolute/path/to/assets go run . -fixture-check
# 公共 Service 的真实工具写入、End 和 PostgreSQL 重启读取
V2_NODE_ASSETS_DIR=/absolute/path/to/assets TEST_DATABASE_URL='postgres://ora:ora-local@127.0.0.1:55432/ora?sslmode=disable' go test -race -count=1 -run TestReal -v ./internal/executionruntime
```

每个项目的 Node 在某个插件第一次被用到时才安装它。Claude Code 包约 190 MB，解压后约 400 MB，
首次安装要解压和校验，明显慢于之后的启动；安装结果留在 `V2_DATA_DIR` 的项目 Node 目录里，
之后重启服务再用只需约一秒。演示前先在每个演示项目里跑一轮 Claude Code，可避开这次等待。

End 和重启不自动删除目录；只有删除对话时按「删除对话时回收工作目录」回收那一个对话的目录。停止服务后 `V2_LOGIN=email go run . -cleanup` 只删除确认停止后的登记 Node 与对话目录，
保留项目 mirror 和数据库历史；命令拒绝与持有同一数据目录的服务并行运行。
外部 provider 的显式验收入口与配置见执行服务包 README；默认 skip 不代表外部 provider 验收通过。

## 与正式 Cloud 的差异

### 调度编排与对话消费适配

[`internal/scheduler`](internal/scheduler/README.md) 提供显式注入记录、执行接口和时钟的协调器。Recover 完成持久恢复后才允许 Tick/Manual，首次发现、版本变化和暂停恢复从未来时刻安排；单进程迟到扫描只处理最近到期点，不回补积压。手动请求用固定键找回原时刻和记录，尚未 begin 的记录可继续首次接收，已接收或未知结果仅按原身份核对。

ConversationAdapter 按冻结配置分流：无项目文本对话用公共 StartModel 和明确模型 ID，有项目文本对话用公共 Start 和明确的 projectId、target.agentId、target.model；旧任务仍可传冻结的 target.executorId。每次触发新建会话，有项目时由公共服务创建独立目录，两路不相互降级。适配层共用 ReadAttempt/Cancel/End，不直接调用 provider 或 Node，也不另存 Session/Turn。成功要求 AnswerComplete；结果终态和 End 确认分开，End 失败不释放任务占位。缺失 Agent/model、工作流目标明确拒绝，不从展示名称猜测可执行绑定。

协调器本身不创建 goroutine；main 的 Host 接入周期扫描和上述 HTTP 操作，前端页面已接入运行与取消等操作。main 注入公共 `executionruntime.Service` 驱动实际派发；普通测试通过公共 Stub 和实际 PostgreSQL 验证编排、HTTP 和生命周期，opt-in 测试另组合真实公共 Service。

协作任务目标保存原任务 ID、执行器和可选补充说明；项目继承自任务，创建和触发时均校验归属和状态。调度器以计划创建者身份通过协作运行入口启动，使用到期记录的稳定身份核对原始执行，避免重复派发。历史只保存任务和运行引用，不复制原任务的过程或回答。仅一次规则按配置时区的指定日期触发，重启不补跑。候选契约见 `schedules.openapi.json`、`tasks.openapi.json`。

`server.NewScheduleCoordinator(records, options, now)` 使用页面统一的 `Options.Execution`，不另建执行服务；宿主在公共启动恢复完成后调用 Recover，再驱动 Tick/Manual。Start 与同记录的核对/取消不会并行。启动错误或本地引用未保存时只按原 Caller 读取；无引用且确定没有进行中的 Start，查询未命中可记 skipped/execution_not_started。读取错误或已有引用但缺失记录时保留占位，不重放 prompt。Agent Cancel 结束会话，model-only Cancel 只停当前轮；定时对话两路都随后确认 End，保留历史与目录。详细状态、原因及 released 含义见 [状态说明](internal/store/occurrence-states.md)。

### 定时工作流版本策略

`ScheduleWorkflowTarget.versionPolicy` 为 `fixed` 或 `latest`，省略仍按固定版本处理。固定版本必须有已发布的 `snapshotId`；跟随最新版本禁止传 `snapshotId`，保存时要求存在生效发布版本。每次手动或定时触发在 `CreateWorkflowRun` 的工作流行锁事务内解析当前发布指针并冻结实际快照；已有运行及同请求键的重试、恢复保持原快照。没有发布版本、快照删除或工作流不可用时按现有 `workflow_unavailable` 路径拒绝，不回退草稿。仅使用 v2 专属 schedules 契约，无 Cloud 契约变化。发布不会修改任何任务；前端发布弹窗只检查当前用户在当前租户的可见配置，因此不公开其他用户的计划。

### 工作流执行

[`internal/workflowhost`](internal/workflowhost) 实现 `server.WorkflowRunHost`，在本进程里执行运行；调度由 [`internal/workflow`](internal/workflow) 的无状态引擎完成，每一波都从数据库重新计算。行为、图模型和执行语义以 desktop（`crates/application/src/workflow_run/engine/`、`crates/backend/src/workflow/run/`）为准，存储沿用运行/scope/节点行模型，引擎的状态转移是 store 的 `*WorkflowEngine*` 事务（每个一次事务，完成节点时同时折叠运行 payload、维护 `state.current_nodes`）。

演示范围（#827 的结论）：

- **只做核心执行**：启动、取消、历史和刷新；普通节点失败使运行失败，Iteration 内失败由其 `errorStrategy` 决定停止或继续。非交互 Agent 按 desktop 自动重试（见下文「Agent 自动重试」，根作用域与 Loop / Iteration 成员一致）；失败或取消的运行可以「从失败处继续」（见下文），Loop / Iteration 内的失败按 desktop 整个容器从第一轮重来；不做轮内续跑、快照接管、checkpoint/回滚和 AI 诊断。交互模式只对根作用域的 Agent 生效，且是简化版（见下文“交互式 Agent 节点”）；Loop 成员上的 interactive 配置照常保存，执行时不生效。
- **节点**：执行 Start、Agent、Output、Condition、Aggregator、Loop、LoopExit、Iteration。历史 `prompt` / `tool` 节点仍可解析，但启动返回 `400 workflow_run_start_failed`（desktop `NodeType::supported` 同样不执行它们）。analyze 的 `unsupportedNodes` 与启动用同一张已执行类型表（`internal/workflow` 的 `runtimes`），只列执行路径上的节点（含可达 Loop 的循环体），前端在启动对话框里据此提前说明；备用节点不列，启动也不拒绝它们。
- **Loop**：轮次从 1 开始、最多 100 轮，每轮重新建独立变量池，只导入上游、全局与显式携带值。反馈值从同一完成快照读取，随后检查 `until`；最后一轮满足条件仍成功，未满足时按 `maxIterationsBehavior` 失败或导出。父节点行保存 JSON 结果，变量池只发布配置过的命名输出，不隐式赋值 `{loop}.output`；`stop_reason` 区分 `loop_succeeded`、`loop_max_iterations`。轮次关闭与下一轮入口同事务提交。
- **LoopExit**：同一 ready wave 优先退出，先冻结导出结果、将同轮未完成子节点取消，再由 host 同时停止本轮所有活动 turn，并等每个 session 的 End 确认。确认前父 Loop 保持运行；停止未确认时失败，不启动下游。成功原因为 `loop_exit`，不读取下一轮反馈。取消后的回调不能改写变量池或冻结输出；请求已持久化、清理尚未确认时重启，会恢复清理并按冻结结果结算，不重放 Agent。
- **Iteration**：串行遍历数组，节点 `iteration` 为 0 基，默认 `maxIterations=50`，可显式设置更大值。空数组直接成功，超限在首轮前失败。每轮必须有 collect 节点成功记录，未执行 collect 不能借用上一轮的值。`fail` 停在失败轮；`continue` 保留失败并继续。父节点输出 `output`（成功值）、`entries`（每项成功或失败）和 `failed_count`；ledger 与下一轮或父节点终态原子提交。成员始终留在原 scope，不复用 Loop scope 作为数组下标。
- **Condition**：按 desktop 的 `condition.rs` 依次判断各个 case，第一个成立的 case 胜出，都不成立走 `else`（没有 `sourceHandle` 的出边就是 `else`）；还没写规则的 case 直接跳过。16 个比较运算符同 desktop；只有 `exists`/`not_exists`/`empty`/`not_empty` 容忍未赋值，其余读到未赋值、未声明或类型不符的变量时节点以 `condition_evaluation` 失败，不猜分支。比较值只写一个 `{{#…#}}` 时保留变量的原类型，混排时按字符串渲染。选中的分支写进运行 payload 的 `conditionDecisions`，不写节点输出，也不进变量池；启动时拒绝重复的 case id（`workflow_run_duplicate_condition_case`）和指向不存在分支的出边（`workflow_run_unknown_condition_branch`）。没走的分支上的节点不建节点行（desktop 同样不落库分支激活），运行详情返回 `conditionDecisions`，由客户端投影为“未激活分支”。
- **Aggregator**：按 desktop 的 V1 契约，按声明顺序取第一个**已赋值**的候选原样传出（键存在即算，`null`、`false`、`0`、`""`、`[]`、`{}` 都命中），写入 `{node}.output`；全部未赋值时以 `aggregator_no_match` 失败，读到未声明的候选直接失败。它不读分支决策，只看变量池。
- **Agent 结构化输出**：`outputContract.type` 为 `structured` 的 Agent，prompt 末尾追加 desktop 的输出约定（示例和 Schema）；回答按 desktop 的顺序提取 JSON 对象（整段 → 代码块 → 文中第一个完整对象），再按 Schema 子集（`type`、`properties`、`required`、`additionalProperties`、`items`）校验。通过时原回答仍是 `{node}.output`，对象写入 `{node}.structured_output`；不通过时节点以 `structured_output` 失败并保留原回答。
- **节点 Skill**：启用的个人 Skill 按启动运行的用户读取，在节点开始时冻结文档；插件 Skill 使用 `namespace/identifier`，必须有目标项目当前安装版本的成功回执。启动 Agent 前，仅将所选导入放到运行目录的 `.opencode/skills`、`.claude/skills`，并在 prompt 中要求读取。插件选择包含该包内全部 Skill；下一节点替换上一个节点的投影，未选择的导入不会自动继承。项目自带的技能保留，同名冲突、已修改的投影文档、缺失包或失效回执会让节点失败，不覆盖或静默略过。节点 payload 的 `skills` 记录所选标识，执行会话保留冻结的文档和插件版本。
- **节点 MCP**：启用绑定从租户目录读取精确版本、配置 revision 和设置，最多 16 个服务、设置紧凑 JSON 合计最多 64 KiB。首轮向支持 `session_mcp_lifecycle` 的 Node 发送选中插件和设置。会话存储支持固定显式选择，并为每个成员保留无值目标。保存配置、移除或重装插件的事务同步推进受影响的活跃会话 generation。每轮准入与配置保存共用会话行锁，派发使用该轮提交时记录的 generation。禁用或空选择沿用普通执行路径。
- **角色、Skill、MCP 与重试已应用；Loop 内的交互模式不应用**：节点 payload 的 `unapplied` 只剩 `interactive`（只有 Loop 成员：v2 自动执行它；根图中的交互式 Agent 停下等发起人，见下一条）。角色从工作区的角色目录实时读取并注入提示词（见「工作区角色」），Skill 按节点选择物化，MCP 按上一条解析并派发，三者都不再列出；`retry` 不再列出：自动重试已按 desktop 覆盖根作用域、Loop 成员与 Iteration 成员（交互节点从不重试，`node_runtime.rs:373-379`@`ebd5b725`），`enabled: false` 或 `maxRetries: 0` 表示不会发生重试，也不算 v2 的缺口。是否自动重试由 `internal/workflow` 的 `retryApplies` 一处决定，`unappliedAgentSettings` 与运行时的重试判断都调用它。历史运行保存的 `skill`、`retry` 标记仍按原样展示。analyze 的 `unappliedSettings` 与运行调用同一个判断（`internal/workflow` 的 `unappliedAgentSettings`），两边都传入节点所在的 Iteration 或 Loop，备用节点按它所属的容器算；某项设置开始应用后从这份列表里去掉，编辑器的提示随之消失，前端不需要改。
- **交互式 Agent 节点（简化版）**：按 desktop 的旧版 `end_turn` 完成规则（desktop@ebd5b725 `docs/workflows/workflow-runs.md` “Interactive nodes”）。根作用域开了 `interactive` 的 Agent 首轮结束后不结束会话，节点行回到 `pending`（运行读作 `awaitingInput`），payload 的 `interactive` 记下已结束的轮数、会话是否已结束、能否完成、阻止完成的原因和完成时会采用的回答，以及发起人发过几条消息（`sent`）和其中最后一条是否没有送达（`last_turn_error`，见下文）。只有发起人可以发消息、停止本轮或完成，其他成员调用得到 `409 run_owned`，只能看。每轮消息立即在同一会话里执行，与运行里其他节点的轮次同时进行，执行期间节点为 `running`，结束后再回到等待输入。一轮怎么结束都不会让节点失败（desktop `workflow-runs.md:125-126, 152-155`）：Agent 自己结束、但没有正常结束的一轮（拒绝、`max_tokens`、`max_turn_requests`、未知停止原因、v2 没有要求的取消、模型调用被时限截断），包括首轮，都让节点回到等待输入，会话保持打开（执行服务的 `LifetimeHeld`），可以继续对话。能不能完成由完成门禁决定（见下一条“交互节点的完成门禁”），被拒时节点保持等待输入。通过后先结束会话（结束没有得到确认时返回 `503`，节点留在等待完成，但会话不再接受消息，可以重试完成），再一次性写入输出和 `{node}.structured_output`、调度后继。“停止本轮”只停这一轮（见「停止一轮」）：会话保留时节点回到等待输入、可以接着发消息；Node 没能保留会话时会话结束，节点继续等待完成、不再接受消息。被停的这一轮记 `turn_stopped`，要「仍然完成」（`force`）或接着对话。在提示送到 Agent 之前就被停的消息（Node 在准入前丢弃了它，或者它的提交被拒、还没重发）也算作一轮被停的轮次，不提示「未送达」，但它没有回答任何东西：节点保留上一次回答过的轮次，原来能完成的回答照样能直接完成，重启后重建的节点也一样。Agent 丢失（崩溃、事件流断开、历史读不全，执行服务报告 `AgentLost`，或结果读不到；Node 本身丢失除外，见「交互等待跨重启」）时会话结束，节点同样停在等待完成，`session_ended` 为 true，丢失的这一轮记 `turn_unknown`，同样要 `force`。同一运行的节点共用一个目录，目录里可以同时有多个活着的会话（和 desktop 一样）：节点等待输入时，运行里其他分支的 Agent 照常开始、执行和结束自己的会话，等待中的会话不受影响，之后的消息仍进同一个会话。这依赖 Node 的 `shared_checkout_sessions` 能力（desktop `c5265072` 起，同一 checkout 最多 8 个未结束的会话），执行服务在握手时要求它；每个会话一次只执行一轮，运行里不同节点的会话同时执行，发给等待中节点的消息不用等其他节点的轮次。执行桥让同一运行的未结束会话不超过这个上限（等待输入的交互会话也占名额），多出的节点等到有会话结束再开始，所以一波节点多于空余名额时不会失败，只是排队；排队中的节点行仍是 `running`、没有会话，payload 带 `session_slot_wait`（`since` 开始等待的时刻、`limit` 上限），拿到名额（在 Start 之前）或因取消、Loop 关闭、续跑换代、关停而放弃时去掉，绑定会话的同一条语句也会去掉（那次清除失败时，执行中的节点也不会读作排队），页面据此对还没有会话的节点显示「等待会话名额」而不是运行中；进程在排队时退出留下的标记随 boot sweep 失败的行留着，页面不把终态运行下的行显示为等待；名额全被等待输入的会话占着时，要等其中一个完成或运行结束；重启不会让出这些名额，合法的交互等待跨重启保留（见「交互等待跨重启」）。Node 仍拒绝超出的会话（`checkout_session_limit`，例如上一个会话的结束还没被 Node 确认）时，只有它自己失败：普通节点按 `session` 失败并照常自动重试，交互节点首轮就被拒时没有会话可等，按 `interactive_session` 失败（可续跑）。会话已经结束的节点（停止时没能保留会话，或 Agent 丢失）再发消息得到 `409 workflow_node_session_ended`，不会先返回 200；兄弟节点让运行失败时，正在执行的消息照常执行完，之后节点不再回到等待输入，会话结束、随运行一起关闭；还没开始执行的消息不再执行。已接受的一轮在提交给会话时被执行服务拒绝的（例如服务不可用，或会话已在别处结束），这条消息没有执行，而发消息的接口此前已经返回 200：节点回到等待输入，`payload.interactive.last_turn_error` 记为 `not_delivered`，说的是第 `sent` 条消息，运行页面据此提示上一条消息未送达；发消息的接口在响应里返回这条消息的序号（`sent`），页面拿它和摘要里的 `sent` 比对，只对发出这条消息的页面提供重新填入；发起人的下一条消息被接受时清掉这个标记。会话还开着的可以重发。执行服务接受了、随后在 Agent 驱动里被拒绝、Agent 并没有收到的消息也一样处理（#1234），不算一轮：轮数、回答和完成条件都停在上一轮送达的样子，`sent` 照常占用这个序号，下一条消息用下一个序号。驱动确知没送到的情况有：发给 Node 之前就被拒绝（会话在 Node 上已经结束、已损坏或 Node 已经断开而没有新 Agent 接手，项目解析失败，或这一轮没能启动），以及 Node 不执行就拒收了这条命令（会话已结束，或等的 MCP 配置一直没到）；执行服务把这样的结果标为 `NotDelivered`。拒绝同时结束了会话的（执行服务报 `AgentLost`），摘要里还有 `session_ended: true`，页面提示消息没送到、会话也已结束，并给出两条出路：用上一轮送达的回答完成这一步，或者停止运行后「从失败处继续」重新开始这一步（整次运行会被取消，这个节点从首轮重来，这段对话不再接续）；会话没结束的可以重发。发送中途失败（写到一半 Node 断了）或已经交给 Node 之后会话才结束的，消息可能已经到了 Agent，仍按丢失的一轮计数、会话结束。重启后从会话轮次重建节点时，这样的记录同样不计入轮数；只认 `NotDelivered`，升级前写下的、只带 `NotStarted` 的追问记录当时算了一轮，重建后照旧算一轮（不会悄悄去掉这一轮和它的阻止原因）。共用目录里的 Skill 是各个活着会话所选 Skill 的并集：等待中节点的 Skill 留在目录里，同名时以后开始的节点为准，会话结束后在下一个节点开始时清掉。交互节点仍可能失败的只有会话没能建立（执行器清单不可用、执行服务拒绝启动、会话绑定被拒，或首轮在 Agent 启动之前就失败：项目、checkout、插件、Skill 没准备好，Agent 配置或模型授权在发给 Node 之前没通过，或被 Node 按 checkout 拒绝；Agent 启动之后才丢失的首轮仍停在等待完成），失败类型是 v2 自己的 `interactive_session`（可续跑，不自动重试；desktop 从不重试交互节点，`retry_tests.rs:636-644`）；模型、Agent、提示词模板等配置错误照普通 Agent 的类型失败，这些类型本来就不自动重试。完成、发消息、停止在执行桥里按节点认领，同时到达时只有一个生效，其余得到 `400 workflow_node_not_awaiting_input` 或 `409 workflow_node_turn_not_running`。取消运行会先结束所有等待中的会话；行刚移到 `running` 而运行恰好被取消时，发消息返回 `400 workflow_node_not_awaiting_input`，不返回 200。兄弟节点让运行失败时，等待中的节点结束会话并记为 `cancelled`；完成请求此时已经在结束会话的，照样记下采用的回答（和运行失败后才结束的其他根节点一样，「从失败处继续」会沿用它），不启动后继，运行可以删除。不做：Agent 完成信号（`completionSignal: "agent"` 按 `end_turn` 执行，记进 `unapplied`，等 Node 协议能交给 Agent 宿主 MCP，circus#1242），以及人工轮次前缀的 `<workflow_step_boundary>`；Loop 内的交互式 Agent 照普通节点执行并在 `unapplied` 记 `interactive`（desktop 会在 Loop 里停住，v2 不支持），Iteration 内的交互式成员和 desktop 一样在发布校验时被拒。
- **Interactive Agent nodes, simplified (English)**: Desktop's legacy `end_turn` completion only (desktop@ebd5b725 `docs/workflows/workflow-runs.md`, "Interactive nodes"). A root-scope Agent with `interactive` keeps its session open after the first turn; its row returns to `pending` (the run reads `awaitingInput`) and `payload.interactive` records the settled turns, whether the session ended, whether it can complete, what blocks it, the answer completion would accept, how many messages the starter has sent (`sent`) and whether the last of them was not delivered (`last_turn_error`, see below). Only the run's starter can send a turn, stop a turn or complete; anyone else gets `409 run_owned` and reads. Each turn runs at once in the same session, beside the turns of the run's other nodes, the node `running` meanwhile, then parks again. How a turn ended never fails the node (desktop `workflow-runs.md:125-126, 152-155`): a turn the Agent itself ended without ending it normally (refusal, `max_tokens`, `max_turn_requests`, an unknown stop reason, a cancellation v2 did not ask for, a model call cut at its time limit), the first turn included, parks the node with its session open (the execution service's `LifetimeHeld`), and the starter can keep talking. Whether it can complete is the completion gate's decision (see "Completion gate of interactive nodes" below); a refusal leaves the node parked. On success the session ends first (an unconfirmed end returns `503` and leaves the node awaiting completion, its session taking no further message; the completion can be retried), then the output and `{node}.structured_output` are written once and successors scheduled. "Stop this turn" stops only the turn (see "停止一轮"): when the session outlives the stop the node parks awaiting input and the starter can keep talking; when the Node cannot keep the session, it ends and the node waits for completion, taking no more turns. The stopped turn records `turn_stopped` and needs "complete anyway" (`force`) or another turn. A message stopped before its prompt reached the Agent (Node dropped it before admission, or its refused submission was not sent again) counts as a stopped turn too, with no undelivered notice, but it answered nothing: the node keeps the turn it last answered, so an answer it could complete with before still completes as it was, also after a restart rebuilds the node. A lost Agent (a crash, a dropped stream, an unreadable history, reported by the execution service as `AgentLost`, or a result that cannot be read; a lost Node is the exception, see "Interactive waits survive a restart") ends the session and parks the node as well, with `session_ended: true`; the lost turn records `turn_unknown` and likewise needs `force`. A run's nodes share one directory, which holds several live sessions at once, as on desktop: while a node waits for input, the Agents of the run's other branches start, run and end their own sessions beside it, and the next message still reaches the same session. This relies on Node's `shared_checkout_sessions` capability (from desktop `c5265072`, up to 8 unfinished sessions per checkout), which the execution service requires at the handshake. Each session executes one turn at a time and the run's sessions execute theirs concurrently, so a message to a parked node never waits for another node's turn. The bridge keeps a run's unfinished sessions within that limit, parked interactive sessions included: a node beyond it waits until a session ends, so a wave wider than the free slots queues instead of failing, and with every slot held by parked sessions it waits until one completes or the run ends; a restart frees none of them, as a valid interactive wait survives it (see "Interactive waits survive a restart"). If Node still refuses a session (`checkout_session_limit`, for example while the end of an earlier one is not yet confirmed by Node), only that session fails: an automatic node fails as `session` and retries as usual, and an interactive node refused on its first turn, with no session to wait on, fails as `interactive_session` (resumable). A node whose session has ended (it could not be kept through a stop, or its Agent was lost) answers a follow-up with `409 workflow_node_session_ended`, never a `200`; a message executing when a sibling fails the run finishes, but the node does not park again: its session ends and it closes with the run, and a message not yet executing does not run. An accepted turn that the execution service then refuses (the service is unavailable, say, or the session ended elsewhere) never ran, although sending it had already returned 200: the node goes back to awaiting input with `payload.interactive.last_turn_error` set to `not_delivered`, which is about message number `sent`; the run page shows it as an undelivered message until the starter's next message is accepted. Sending a message answers with that message's number (`sent` in the response), which the page compares with the summary's `sent`, so only the view that sent the message offers to put its text back. A session that is still open takes the message again. A message the execution service accepted and the Agent driver then refused before the Agent got it is handled the same way (#1234) and is no turn: the turn count, the answer and the completion gate stay as the last delivered turn left them, while `sent` keeps the message's number and the next message takes the next one. The driver knows the Agent did not get it when it refused the turn before sending it to Node (the session had ended on Node, broken, or lost its Node with no new Agent to take it, the project could not be resolved, or the turn did not start), or when Node refused the command without running it (its session had ended, or the MCP generation it waited for never came); the execution service marks such a result `NotDelivered`. When the refusal also ended the session (the execution service reports `AgentLost`), the summary also says `session_ended: true`, and the page says that the message was not delivered and the session has ended, and offers both ways on: complete the step with the last delivered answer, or stop the run and resume from the failure to start this step again (the whole run is cancelled and this node starts over from its first turn, without this conversation); otherwise the message can be sent again. A send that failed midway (the Node died during the write) or a session that ended after Node took the command may have reached the Agent, so it still counts as a lost turn and ends the session. The rebuild after a restart does not count such turns either; it goes by `NotDelivered` alone, so a follow-up recorded before the upgrade with only `NotStarted`, which was counted as a turn then, is still counted (the rebuild does not quietly drop that turn and its blocker). The Skills in the shared directory are the union of the live sessions' selections: a parked node's Skills stay, the node that starts later wins a shared name, and a Skill goes with the first node that starts after its session ended. The only remaining failures of an interactive node are a session that could not be set up (executor list unavailable, start refused, binding rejected, or a first turn that failed before its Agent started: the project, checkout, plugin or Skills could not be prepared, the Agent configuration or model permit was refused before anything was sent to Node, or Node refused it at the checkout; a first turn lost after its Agent started still parks for completion), recorded with v2's own kind `interactive_session` (resumable, never auto-retried; desktop never retries interactive nodes, `retry_tests.rs:636-644`); configuration errors (model, Agent, prompt template) keep the kinds an automatic Agent uses, none of which retries automatically. Complete, turn and stop claim the node in the bridge, so of concurrent requests exactly one wins and the others get `400 workflow_node_not_awaiting_input` or `409 workflow_node_turn_not_running`. Cancelling the run ends every parked session first; a follow-up whose row moved just as the run was cancelled gets `400 workflow_node_not_awaiting_input`, not `200`. A sibling failing the run ends the parked session and closes its row as `cancelled`; a completion that is already ending that session still records its accepted answer (like any other root node that settles after the run failed, so a resume from failure reuses it), starts no successor, and the run can be deleted. Not implemented: the Agent completion signal (`completionSignal: "agent"` runs as `end_turn` and is listed in `unapplied` until Node's protocol can hand the Agent a host MCP, circus#1242) and the `<workflow_step_boundary>` prefix on human turns. A Loop member with `interactive` runs as an automatic node and records `interactive` in `unapplied` (desktop parks it, v2 does not support it); interactive Iteration members are rejected at publish validation as in desktop.
- **交互节点的完成门禁**（#1166，照 desktop 默认模式去掉 Agent 完成信号：desktop `0fcc3b30` `crates/backend/src/workflow/run/interactive/`、`completion_snapshot.rs`；specs `decisions/desktop/core/workflow/` 的完成契约、首轮确认、产出物声明 ADR）：每次停靠，`payload.interactive.completion` 记下这一轮的快照：`turnKey`（打开这一轮的消息序号 `userSeq`，首轮为 0；`endedSeq` 是已结束的轮数）、`outputDigest`（回答的 sha256）、`turnOrigin`（`workflow` 为首轮，`human` 为发起人的消息）、`stopCause`、`parkKind`（`awaiting_user` / `abnormal`）和 `blockers`。blocker 用 desktop 的词：没有正常结束的一轮是 `turn_stopped`（发起人停止的 `stopCause` 为 `user_stopped`，模型调用超时为 `prompt_timed_out`）、`turn_truncated`、`turn_refused`、`turn_unknown`（Agent 丢失或结果读不到，`stopCause: session_crashed`），回答不符合结构化输出是 `structured_invalid`；首轮正常结束也不能直接完成，记 `turn_unconfirmed`。完成请求的规则照首轮确认 ADR D2：`confirmInitialTurn: true` 只解除 `turn_unconfirmed`；`force: true` 越过其余 blocker、不能代替确认；两者同时出现返回 `400 invalid_request`（desktop 只在 `agent` 模式下放开这个组合）；被拒时 `409 workflow_node_incomplete`，`params.blockers` 列出请求没解决的 blocker（`{kind, path?, message?}`）。强制完成在 payload 写 `completion_forced{by, blockers, stop_cause, without_turn, output_source}`，普通完成和首轮确认不写；payload 都写 `requester: "human"`。force 的产出：会话还开着时取这一轮的回答（`turn_window`）；会话已经结束（例如停止时没能保留会话）时取之前最后一次正常结束的回答（`last_completed_turn`），没有时才取这一轮的文字，这一点和 desktop 不同。完成时先比对停靠记下的 `turnKey` 和 `outputDigest`，结束会话后再比一次，不一致返回 `409 workflow_node_turn_changed` / `workflow_node_output_changed`，`force` 也越不过。`agentConfig.completion.files`（1–32 条，路径或最小 glob `*` `?` `**`，`mustChange` 可选，要求 `schemaVersion: 4`）在每次完成时对照运行的 checkout 检查，不写进快照：没有文件匹配记 `file_missing`，`mustChange` 的匹配文件都和基线相同记 `file_unchanged`，挡住普通完成和首轮确认，`force` 可以越过并记进审计；符号链接和 `.git` 下的文件不算。字面路径直接查找（被 gitignore 的也算）；通配声明和 desktop 一样经 `git ls-files -co --exclude-standard` 列举，只列它的字面前缀目录下、未被忽略的文件，所以 `node_modules` 这类忽略的目录不会被遍历。checkout 的 `.git`（包括它的配置）由 Agent 控制，这里的 git 调用和回合交付（`workspacefs` 的 `deliveryGit`）一样从不在它上面运行：已提交的内容从项目镜像读（`git cat-file blob`，在 v2-server 里算摘要），列文件用临时的 scratch 仓库，索引装 base commit 的树，排除规则经 `internal/gitexclude` 复制过来；git 在干净的环境里运行（无全局配置、`protocol.allow=never`、`GIT_NO_LAZY_FETCH`、`GIT_NO_REPLACE_OBJECTS`），超时连同进程组一起结束。所以 checkout 里配置的过滤程序、fsmonitor、hook、promisor 按需拉取的传输程序都不会执行，检查也不会因此卡住。文件内容在 checkout 根目录下打开（`os.OpenRoot`），只读普通文件，每个最多 64 MiB，超过的不判定。基线和 desktop 不同：desktop 在节点开始时给整个工作区拍快照，v2 只在首轮派发前给每条 `mustChange` 声明的全部匹配算一个总摘要（路径和内容），派发时写一次 `payload.completion_baseline`，跨重启保留；运行里还没有 checkout 时（第一个 Agent）以 checkout 的 base commit 为基线，逐个比较匹配文件。列举超过 50000 条、或一条声明匹配超过 200 个文件时，这条声明不判定 blocker，记 `file_check_unavailable` 警告；基线记不下时同样只查存在并带这个警告。确认之前后继节点不启动。重启后的节点从轮次记录重建首轮来源，从快照恢复停止原因，门禁不变。
- **Completion gate of interactive nodes (English)**: (#1166, desktop's default mode without the Agent completion signal: desktop `0fcc3b30` `crates/backend/src/workflow/run/interactive/` and `completion_snapshot.rs`; the specs ADRs on interactive completion, first-turn confirmation and deliverables under `decisions/desktop/core/workflow/`.) Every park records the turn's snapshot as `payload.interactive.completion`: `turnKey` (`userSeq`, the number of the message that opened the turn, 0 for the first; `endedSeq`, the settled turns), `outputDigest` (the answer's sha256), `turnOrigin` (`workflow` for the first turn, `human` for the starter's), `stopCause`, `parkKind` (`awaiting_user` / `abnormal`) and `blockers`, in desktop's words: a turn that did not end normally is `turn_stopped` (`stopCause` `user_stopped` for the starter's stop, `prompt_timed_out` for a model call cut at its limit), `turn_truncated`, `turn_refused` or `turn_unknown` (a lost Agent or unreadable result, `stopCause: session_crashed`); an answer that does not fit the structured output is `structured_invalid`; and the workflow's own first turn, even ended normally, is `turn_unconfirmed`. A completion follows the first-turn ADR's D2: `confirmInitialTurn: true` lifts only `turn_unconfirmed`; `force: true` covers every other blocker and never stands in for the confirmation; both at once return `400 invalid_request` (desktop admits the pair only for the agent policy); a refusal is `409 workflow_node_incomplete` listing the unresolved blockers in `params.blockers` (`{kind, path?, message?}`). A forced completion writes `completion_forced{by, blockers, stop_cause, without_turn, output_source}`; a normal one or a confirmation does not; every completion writes `requester: "human"`. What force takes: the turn's own answer while the session is open (`turn_window`); once the session ended (v2 stops a turn only by ending the session until #1111) the last answer that ended normally (`last_completed_turn`), and the turn's text only without one, unlike desktop. A completion compares the parked `turnKey` and `outputDigest`, before and again after ending the session, and refuses with `409 workflow_node_turn_changed` / `workflow_node_output_changed`, force or not. `agentConfig.completion.files` (1–32 paths or minimal globs `*` `?` `**`, optional `mustChange`, requiring `schemaVersion: 4`) is checked against the run's checkout on every completion and never stored in the snapshot: no matching file is `file_missing`, a `mustChange` declaration whose matches all equal the baseline is `file_unchanged`; both block a plain completion and a confirmation, and force covers them in its audit; symbolic links and files under `.git` never count. A literal declaration is looked up directly (an ignored file counts); a wildcard is enumerated as desktop does, through `git ls-files -co --exclude-standard` under its literal prefix, so ignored trees such as `node_modules` are never walked. The checkout's `.git`, its config included, is the Agent's, so like turn delivery (`workspacefs`'s `deliveryGit`) no git call here runs against it: committed content is read from the project's mirror (`git cat-file blob`, hashed in v2-server), and files are listed through a scratch repository whose index holds the base commit's tree, with the excludes copied through `internal/gitexclude`; git runs in a clean environment (no global config, `protocol.allow=never`, `GIT_NO_LAZY_FETCH`, `GIT_NO_REPLACE_OBJECTS`) and is ended with its process group when its time is up. No filter, fsmonitor, hook or promisor transport configured in the checkout runs, and none can hang the check. File contents are opened beneath the checkout root (`os.OpenRoot`), regular files only, at most 64 MiB each; a larger one is not judged. The baseline differs from desktop's whole-worktree snapshot at node start: v2 records one digest per `mustChange` declaration over all its matches (paths and contents) before the first turn is dispatched, written once into `payload.completion_baseline` at dispatch and kept across restarts, or uses the checkout's base commit, compared file by file, when the run has no checkout yet (its first Agent). A listing past 50,000 entries, or a declaration matching more than 200 files, is not judged: it adds the `file_check_unavailable` warning instead of a blocker, as does a baseline that could not be recorded. Successors start only once the node completes. A recovered node rebuilds the first turn's origin from its turns and the stop cause from its snapshot, so the gate is the same after a restart.
- **执行器映射**：节点的 `executor.agentCli` 匹配执行目录里执行器的 `AgentID`（`official/ora-space.opencode`、`official/ora-space.claude`），找不到时节点以 `missing_agent_ref` 失败；`modelId` 匹配该 Agent 执行器的模型 ID 或名称，为空或找不到时节点以 `workflow_model_not_found` 失败，消息为 `model {model_id} is not advertised by agent {agent_ref}`（空模型使用 `model is not advertised by agent {agent_ref}`，去掉 desktop 字面格式中的双空格），不启动会话，也不退回默认执行器。成功节点的 payload 写入 `stop_reason`、实际使用的 `executor`（`id`、`agentId`、`name`、`pluginVersion`）、`model`（`id`、`name`、`requested`、`fallback`，新运行始终为 `false`），以及按变量池渲染后的任务要求 `task`（节点尝试的 `input` 保留模板原文）。
- **Executor mapping (English)**: The node’s `executor.agentCli` matches executor `AgentID` in the execution catalog (`official/ora-space.opencode`, `official/ora-space.claude`); if none is found, the node fails with `missing_agent_ref`. `modelId` matches that Agent executor’s model ID or name; if blank or unmatched, the node fails with `workflow_model_not_found` and the message `model {model_id} is not advertised by agent {agent_ref}` (blank models use `model is not advertised by agent {agent_ref}`, removing the double space in desktop’s literal format), without starting a session or falling back to a default executor. Successful node payloads record `stop_reason`, the actual `executor` (`id`, `agentId`, `name`, `pluginVersion`), `model` (`id`, `name`, `requested`, `fallback`, always `false` for new runs), and the task instruction `task` rendered from the variable pool (the node attempt’s `input` retains the original template).
- **编辑器模型修复**：编辑器加载草稿时，若目录非空且未覆盖能力配置，会将失效或空白模型替换为同一 Agent 的第一个可用模型；没有该 Agent 的模型时保留原值。此投影不标记草稿为未保存，后续保存才写入；已有发布版本不会改变。目录为空时，新 Agent 节点保留 desktop 的基础执行器默认值，不创建空白模型。
- **Editor model repair (English)**: When loading a draft with a nonempty catalog and no capability override, the editor replaces a stale or blank model with the first available model for the same Agent; if that Agent has no model, the original value is retained. This projection does not mark the draft unsaved and is written only on a later save; existing published versions are unchanged. With an empty catalog, new Agent nodes retain desktop’s base executor defaults instead of creating a blank model.
- **重启**：启动时的 boot sweep 以一次集合更新失败上个进程留下的所有 `running`（或已开始工作的 `pending`）运行——包括最后一次完成与下一波之间的间隙里没有活动节点行的运行——并连带失败其 Loop 轮次；冻结表单的运行除外。仍在执行的 Iteration（其父行也在运行）吸收中断：只有该轮成员以 `interrupted_by_restart` 失败，运行存活，随后 owner 的 `errorStrategy` 决定 `continue` 记失败账目继续下一轮，或 `fail` 结束运行（desktop `recovery.rs` 的 `AbsorbIntoComposite`）；只对仍为 `running` 的运行考虑吸收：已结束运行留下的活动行（例如根 Agent 失败结束运行时旁边仍在运行的 Iteration 父行）照旧失败。吸收要求运行有冻结的 `started_by`，没有就按普通中断失败，不会阻塞启动。任务的处理运行和拆分运行在 sweep 之前已由其所属方取消，从不被吸收（`internal/boot`）。被中断的等待行去掉 `retry_wait` 且不重排；恢复使用冻结的 `started_by` 身份重新调度。这样存活的运行重启后仍用原来的目录：工作区映射原本只在进程内存里，执行服务启动时按每个仍是 `running` 的运行最近一个 Agent 节点的会话找回它的工作区（任务的处理运行和拆分运行除外：它们的所属方在 sweep 之前就结算了，结算后不会原地继续），按记下的 Node clone 执行 ID（`v2_workspaces.clone_execution`）接回 checkout，之后的节点能看到之前的改动；目录已被删掉、或升级前记下的目录没有 clone 执行 ID 时，照旧新 clone。被 sweep 判失败的 `running` 运行也保留这个映射，重启后「从失败处继续」同样接着用原目录，和不重启时一致；sweep 同样判失败的、已开始工作的 `pending` 运行不在此列，从失败处继续照旧换新工作区。已持久化 LoopExit 请求的活动轮次也能存活：退出事务已冻结结果并取消同轮未完成子节点，启动时公共执行服务先清理上个进程的会话，再由工作流引擎重放清理确认，以冻结的结果（成功输出或导出错误）结算父 Loop；重复恢复和迟到回调都不能改写这些结果。此例外不豁免并行的外层 Agent、重试等待或普通 Loop 工作；它们仍使整次运行失败。不重放 Agent。合法的交互等待保留下来（见下一条「交互等待跨重启」）；有审阅表单的运行本来会保留，但其中仍有运行中的 Agent 节点时也照此失败（发布校验目前让 Agent 都在表单之前，这条规则是防线）。Restart: a valid interactive wait survives (see the next item); a run with a review form, otherwise kept, fails the same way when it still has a running Agent attempt; publication currently keeps every Agent ahead of the form, so this rule is a safeguard. A run that survives keeps its directory: the workspace mapping lived only in process memory, so on startup the execution service finds the workspace of each run still `running` from the session of its latest Agent node (except a task's handling and decomposition runs, which their owners settle before the sweep and which never continue in place) and reattaches the checkout by its recorded Node clone execution ID (`v2_workspaces.clone_execution`), and later nodes see the earlier edits; a removed directory, or one recorded before clone execution IDs were kept, still means a fresh clone. A `running` run the sweep fails keeps the mapping too, so resuming it from the failure after the restart continues on its directory, as without a restart; a `pending` run that had started work, which the sweep fails as well, is not covered and resumes in a new workspace.
- **交互等待跨重启**（#1167，照 desktop `docs/workflows/workflow-runs.md`「Recovery after restart」和 `engine.rs` 的 `is_awaiting_input` 选择性恢复）：停在等待输入的交互节点重启后仍然等待输入，发起人可以接着发消息、停止本轮或完成。合法的等待是：节点行 `pending`、绑定了会话、冻结图里是根作用域的交互式 Agent、payload 里有 `interactive` 摘要，而且运行里没有正在执行的根作用域 Agent；运行里有正在执行的 Agent（包括重启时正在执行一轮追问的这个节点自己）时，和 desktop 一样整次运行以 `interrupted_by_restart` 失败，停住的节点一起失败、会话随之结束，可以「从失败处继续」。执行服务启动时保留这类节点的会话（其余会话照旧结束），执行桥从节点行和会话的轮次记录重建节点状态（轮数、最后一轮、可采用的回答、追问编号），执行器已不在目录里（Agent 或模型不再提供）或会话不在了的节点按失败关闭；启动时读不了目录或会话的，不判节点失败：目录读取的瞬时错误在约一分钟内重试，仍读不了就中止启动，节点行原样保留，下次启动再恢复；目录明确回答这个范围下没有可用的执行器（`ErrNotFound`；现在的目录不区分范围，不会返回它，这条是为按范围的目录准备的）算作对节点的结论，节点照旧按失败关闭；其他非瞬时错误不重试，立即中止启动，日志写明是哪个运行、哪个节点、什么原因（这类错误不消失，每次启动都会在同一处中止）。启动先重建所有可恢复的运行，全部成功后才逐个继续，所以中止时同一次启动里还没有任何运行继续执行，不会有刚派发的工作被退出打断。重启时正在完成的节点（会话已经结束、输出还没写入）也算合法等待：重启后停在等待完成（`session_ended: true`，恢复时按会话重写节点摘要，页面不再提供输入框；重写写不进数据库时中止启动，不带着过时的摘要继续），可以再点一次完成。重写摘要时保留「消息未送达」的提示（`last_turn_error: not_delivered`）和 `sent`：被拒收的消息没有留下轮次记录，摘要是它唯一的记录。固定 Node 不能接回原来的 Agent 会话（协议没有 `session/load` 入口，会话结束时 Agent 自己的会话目录也被删掉；另见 #1230），所以重启后的第一轮在原 checkout 上新起一个 Agent：checkout 按记下的 Node clone 执行 ID（`v2_workspaces.clone_execution`）接回，未提交的改动都在；新 Agent 先收到一段交接记录（之前每轮的用户消息、回答，以及工具调用的标题和结果），再收到这次的消息，相当于 desktop `session/load` 失败后的 `rebuild_provider_session`。**交接记录只按标题和结果列出工具调用：工具的输出不在（读过的文件内容、命令输出都不在），输入也只有工具标题带出的那部分，Agent 的中间推理和计划也不在；重启后第一轮也没有提示缓存。** 工具标题可能就是完整的输入：OpenCode shell 工具的标题就是完整命令，会原样出现在交接记录里（desktop 的 `render_handoff` 也一样）；这不算泄露，页面的会话历史里本来就显示这些标题。交接记录作为这一轮的第一个文本块单独发送，长度和这次的消息加起来不超过 Node 的 64 KB 上限，放不下时先去掉最早的轮次并注明；它不写进这一轮的提示记录，页面也不显示：会话历史里重启前的记录原样保留，新 Agent 的记录接在后面，交接块被去掉，看起来是一段连续的对话。同一运行之后开始的节点继续用这个目录。接不上时分两档：启动时判定不合法的按失败关闭（见上）；合法、但追问时新 Agent 没能启动的（例如 Node 暂时不可用），这条消息没有送到 Agent，不算一轮，节点仍停在等待并提示消息未送达，可以重发；只有确认 checkout 已经不在了（目录被删掉，升级前记下的目录没有 clone 执行 ID，或者 Node 的账本已经不认识记下的 clone 执行 ID，例如 Node 的数据目录被清掉、只剩 checkout 目录），会话结束、节点以 `interactive_session` 失败；Node 不认识的 clone 执行 ID 同时从记录里清掉（Node 读不了自己的账本时也报同一个 `checkout_unavailable`：desktop 的 `ledger.rs`、`branch_heads.rs` 把存储、SQLite、JSON 错误都当成没有这条记录，所以重启后账本偶发读失败，也会让合法的 checkout 按已不在处理、记录被清掉、目录里没提交的改动留在旧目录；要等 Node 给读错误一个单独的码，见 #1230），之后这个 run 的节点（包括从失败处继续、以及没追问就完成后的后继节点）重新 clone。没追问就完成时，后继节点的第一次尝试仍用这个失效的 clone 执行 ID，失败后记录才被清掉：自动节点的这次失败照常显示、用掉一次自动重试额度，重试在新 clone 上进行，关掉重试时运行失败；交互节点以 `interactive_session` 失败、运行随之失败，提示说明「从失败处继续」会在新 clone 上开始；同一 run 另一个停住的节点，会话第一次跨重启时记下它所在 checkout 的 clone 执行 ID（`v2_execution_sessions.clone_execution`），追问时记录已换成后继重新 clone 的新 clone，也按 checkout 已不在处理（`interactive_session`），不会接到看不到之前改动的新 clone 上，旧 clone 的目录原样留在磁盘上（已知缺口：绑定发生在会话第一次跨重启时，所以如果 Node 在 v2-server 一直运行时丢了账本，并在下一次重启之前就有节点重新 clone，那次重启会把这个会话绑定到新 clone，它之后的追问就在新 clone 上继续，看不到之前的改动，这个绑定以后也不会被纠正）；Node 按 checkout 拒绝新 Agent（例如会话名额已满）时，会话也和普通会话一样结束，节点只能用之前最后一次正常结束的回答完成（`session_ended: true`）。普通对话和任务运行不在此列，重启后照旧结束（#1148 场景 4 仍是已知限制）。**v2 没重启、只是项目的 Node 崩了（进程退出或连接断开，失败码 `agent_transport_closed`）时，交互节点也走这条重建路径，这改变了原来的失败语义**：原来 Node 一断，等待中的交互会话就结束，节点只能用之前最后一次正常结束的回答完成（`session_ended: true`）；现在会话保持打开，下一条消息在原 checkout 上新起 Agent 并交接记录。消息还没送到 Agent 时 Node 已经不在的，这条消息直接由新 Agent 执行；Node 死时正在执行（或正在发送）的那一轮失败、不重放，节点回到等待输入，下一条消息再重建；Node 重建后，这个 run 之后的节点也继续用原目录（自动节点也一样：原来 Node 断掉后同一 run 的下一个节点会重新 clone，现在继续用原目录，能看到之前尝试留下的改动，和 Node 一直活着时一致）。首轮开始前 Node 就已经不在的（这一轮还没送到 Agent），没有可交接的记录，照旧按 `interactive_session` 失败（可续跑）；首轮进行中 Node 丢失的，和原来一样会话结束，节点停在等待完成（`session_ended: true`）。只有 Agent 进程自己崩了而 Node 还活着时，会话仍照旧结束。对话和任务运行不受影响，只是 Node 不在时这一轮的失败码统一成 `agent_transport_closed`。
- **Interactive waits survive a restart (English)**: (#1167, desktop's selective recovery: `docs/workflows/workflow-runs.md` "Recovery after restart" and `engine.rs` `is_awaiting_input`.) A node parked awaiting input still awaits input after a restart, and the starter can send a turn, stop it or complete the node. A valid wait is a `pending` row with a bound session, a root-scope interactive Agent in the frozen graph and an `interactive` summary in its payload, in a run with no root Agent executing; a run with an executing Agent (this node included, when it was running a follow-up at the restart) fails as a whole with `interrupted_by_restart`, as on desktop, its parked nodes with it and their sessions ended, and can resume from the failure. The execution service keeps such a node's session on startup (it ends every other one as before), and the bridge rebuilds the node from its row and the session's recorded turns (turn count, last turn, the answer completion would accept, follow-up numbering); a node whose executor the catalog no longer offers (its Agent or model is gone) or whose session is gone fails; a catalog or session that cannot be read on startup fails no node: a transient catalog error is retried for about a minute, and one that persists aborts startup with the row left as it was, for the next start to recover; a catalog that answers it has nothing for the run's scope (`ErrNotFound`; today's catalog ignores the scope and never returns it, this is for a scoped catalog) gives a verdict, and the node fails as before; any other error that is not transient is not retried and aborts startup at once, with a log naming the run, the node and the cause (such an error, until it clears, aborts every start at the same place). Startup rebuilds every recoverable run before it resumes any, so an abort finds no run of that start already executing and interrupts no work it had just dispatched. A node being completed at the restart (its session already ended, its output not yet written) is also a valid wait: after the restart it awaits completion (`session_ended: true`: the recovery rewrites the node's summary from its session, so the page offers no message box; a rewrite the database refuses aborts startup rather than serving the stale summary) and can be completed again. The rewrite keeps an undelivered notice (`last_turn_error: not_delivered`) and `sent`: a refused message left no turn, and the summary is its only record. The pinned Node cannot reattach the original Agent session (its protocol has no `session/load` entry, and it removes the Agent's own session store when the session ends; see #1230), so the first turn after the restart starts a new Agent on the original checkout: the checkout is reattached by its recorded Node clone execution ID (`v2_workspaces.clone_execution`), uncommitted edits included, and the new Agent is first sent a handoff (each earlier turn's user message and answer, and tool calls by title and outcome), then this turn's message, like desktop's `rebuild_provider_session` after a failed `session/load`. **The handoff lists tool calls by title and outcome only: it carries no tool outputs (no file contents, no command output) and tool inputs only as far as a title shows them, nor the Agent's thoughts or plans, and the first turn after a restart has no prompt cache.** A title can be the whole input: an OpenCode shell tool is titled with its full command, which reaches the handoff as is (desktop's `render_handoff` does the same); this leaks nothing, as the page's session history shows the same titles. The handoff is sent as the turn's own first text block, within Node's 64 KB limit together with the message (the earliest turns are left out first, and the block says so); the turn records only the typed message, and the page never shows the block: the session history keeps its records from before the restart, the new Agent's records follow them, and the handoff block is left out, so the page reads as one conversation. Later nodes of the run keep using the directory. When the node cannot continue: an invalid wait fails on startup (above); a valid one whose new Agent could not be started on a follow-up (Node briefly unavailable, say) never delivered the message, so it is no turn, and the node stays awaiting input with the undelivered notice, so the message can be sent again; only a checkout confirmed gone (its directory removed, recorded before upgrade without its clone execution ID, or a clone execution ID Node's ledger no longer knows, as when Node's data directory was cleared and only the checkout's directory is left) ends the session and fails the node as `interactive_session`; a clone execution ID Node no longer knows is also cleared from the record (Node reports the same `checkout_unavailable` when it cannot read its own ledger: desktop's `ledger.rs` and `branch_heads.rs` treat storage, SQLite and JSON errors as a missing record, so a transient ledger read error after a restart also treats a valid checkout as gone, clears its record and leaves the directory's uncommitted edits behind; telling them apart needs a distinct Node code, see #1230), so the run's later nodes (a resume from the failure, or the successor of a node completed without a follow-up) clone again, though a successor of a node completed without a follow-up still meets the stale ID on its first attempt, which fails and drops it: an automatic successor shows that failed attempt and spends one of its automatic retries, which runs on a fresh clone (with retries off, the run fails), and an interactive successor fails as `interactive_session` with its run, its message saying that a resume from the failure starts on a fresh clone; another parked node of the run, whose session recorded its checkout's clone execution ID when it first survived a restart (`v2_execution_sessions.clone_execution`), finds its checkout gone too (`interactive_session`) once the record names a successor's fresh clone, rather than continuing there without its earlier edits, and the old clone's directory is left on disk (a known gap: the binding happens when the session first survives a restart, so if Node loses its ledger while v2-server keeps running and a node clones again before the next restart, that restart binds the session to the new clone, its later follow-ups continue there without the earlier edits, and nothing corrects the binding afterwards); Node refusing the new Agent at the checkout (its session limit, say) also ends the session, like any session it refuses, and the node completes only with its last normal answer (`session_ended: true`). Conversations and task runs are not covered and still end on a restart (#1148 scenario 4 remains a known limitation). **When the server keeps running but the project's Node is lost (its process exits or its connection breaks, failure code `agent_transport_closed`), an interactive node takes the same rebuild path, which changes the earlier failure semantics**: the parked session used to end with its Node, leaving the node completable only with its last normal answer (`session_ended: true`); it now stays open, and the next message starts a new Agent on the original checkout with the handoff. A message that finds the Node already gone before reaching the Agent runs on the new Agent at once; a turn executing (or being sent) when the Node died fails and is not replayed, the node parks again, and the next message rebuilds; after the Node is rebuilt, the run's later nodes keep using the directory (automatic nodes too: they used to get a fresh clone after a Node loss and now see the edits earlier attempts left, as while the Node stays alive). A first turn whose Node was already gone before it reached the Agent has nothing recorded to hand over and still fails as `interactive_session` (resumable); a first turn whose Node is lost while it runs ends the session as before, and the node awaits completion (`session_ended: true`). A crash of the Agent process alone, with Node alive, still ends the session. Conversations and task runs are unaffected, except that a turn finding its Node gone now reports `agent_transport_closed`.

#### Agent 自动重试 / Agent automatic retry

非交互 Agent（根作用域、Loop 成员或 Iteration 成员）仅在失败种类的 `auto_retryable` 为 true 且预算未用尽时重试；交互节点即使显式开启策略也不重试。缺省策略启用、最多重试 2 次、初始等待 10 秒；显式 `enabled: false` 或 `maxRetries: 0` 禁止重试。第 n 次重试等待 `initialDelaySeconds × 2^(n−1)`，最多 600 秒，依据 desktop `ebd5b725` 的 `crates/application/src/workflow_run/engine/retry.rs:22–31,66–92`。

失败尝试连同真实会话、原回答和 `error_detail` 软删留史；同事务在**同一 run、同一 scope、同一轮**创建没有会话、没有 `started_at` 的下一尝试，保存 `retry_wait`（attempt、截止时间、延迟、前次尝试及失败原因）、`auto_retry` 和 `retry_chain`。`error_detail.attempt` 按 `(run, node, iteration)` 计数，Loop 成员按轮次 scope 单独计数（`workflowNodeAttempt`），因此每个 Loop 新轮都从 1 开始，`auto_retry` 预算也随新轮重置。运行继续执行独立分支，下游等待重试成功。引擎拥有进程内定时器；到期后在运行门禁内核对运行、等待尝试及截止时间，清除等待标记再派发全新会话，派发使用失败所在 scope 的图与变量池（Loop 成员用该轮的 loopState 池）。Loop 成员重试期间该轮保持 in-flight，轮次结算只在预算耗尽后才发生。重复、取消后的唤醒不派发；提前唤醒（如墙钟回拨）按持久化的截止时间重新计时。运行因其他分支或容器失败时，同一事务把等待行取消为 `{"reason":"retry_abandoned"}` 并去掉 `retry_wait`（根作用域、Iteration 与 Loop 的等待行同一条集合路径）；LoopExit 取消同轮兄弟时同样去掉 `retry_wait`；唤醒时运行已不在执行的，也同样收尾该行。重试决策出错、或唤醒已提交开始后派发失败，都让该尝试失败，不留下无人驱动的运行行，并紧跟一次调度让容器按其错误策略结算。取消与关闭停止定时器，关闭等待已在执行的回调结束；取消的门禁拦截可重试的 Agent，运行关闭后释放。重启沿用 boot sweep：Loop 或根作用域的等待行随运行失败，运行中 Iteration 的等待行按轮吸收后由错误策略结算，均去掉 `retry_wait`、不重排。页面不把终态运行下的等待行显示为等待；“已安排自动重试”只用于运行仍在执行且替代尝试仍在等待的情况。页面复用倒计时与失败历史组件；历史尝试会话仍受原有运行成员关系和会话所有者校验。

Only non-interactive Agent nodes retry — at the root, inside a Loop body or inside an Iteration region — and only for auto-retryable failures with remaining budget. Missing policy enables two retries with an initial 10-second delay; disabled policies and zero budgets do not retry. Retry n waits `initialDelaySeconds × 2^(n−1)`, capped at 600 seconds, following desktop `ebd5b725`, `crates/application/src/workflow_run/engine/retry.rs:22–31,66–92`.

The failed attempt is soft-deleted with its real session, answer and failure detail preserved. The same transaction inserts a waiting attempt **in the same run, scope and round** without a session or start instant, with `retry_wait` (attempt, deadline, delay, prior attempt and reason), `auto_retry` and `retry_chain`. `error_detail.attempt` counts per `(run, node, iteration)`, while Loop members count within their round's scope (`workflowNodeAttempt`), so every new Loop round restarts at attempt 1 with a fresh `auto_retry` budget. Independent branches continue; successors wait for success. Engine-owned timers revalidate the run, waiting attempt and deadline under the run gate before waking and dispatching a new session, using the graph and variable pool of the scope the attempt failed in (Loop members dispatch against their round's loopState pool). A Loop round stays in flight while a member's retry waits; the round settles only once the budget is exhausted. Duplicate and cancelled wakes do nothing; an early wake (e.g. after a wall-clock step back) re-arms for the persisted deadline. When another branch or a container fails the run, the same transaction cancels waiting rows with `{"reason":"retry_abandoned"}` and drops `retry_wait` (root, Iteration and Loop waits share that one set-based path); LoopExit's sibling cancellation drops the marker too; a wake that finds its run no longer running settles the row the same way. An error while deciding a retry, or while dispatching after a wake committed the start, fails the attempt instead of leaving a running row nothing drives, and a scheduling pass follows so the container settles by its error policy. Cancellation and shutdown stop timers, and shutdown waits for callbacks already running; the cancellation fence covers retry-eligible Agents and is released once the run is closed. Restart settles waiting rows through the boot sweep: Loop and root waits fail with the run, a running Iteration's wait is absorbed with its round and settled by the error strategy, all dropping `retry_wait` and never re-arming it. The run view never shows a waiting row under a terminal run as waiting, and labels a failed attempt "retry scheduled" only while its replacement still waits under a live run. The run view reuses countdown and failure-history components; earlier sessions retain run membership and execution-owner checks. Interactive Agents never retry. `retryApplies` in `internal/workflow` is the single rule, used by `unappliedAgentSettings` and by the runtime; it now reports retry as applied at every placement, so the `retry` unapplied entry is gone from new analysis and payloads.

#### 从失败处继续 / Resume from failure

照 desktop 的 `resume_from_failure`（`crates/application/src/workflow_run/engine/engine.rs:169-211`、`crates/db/src/repository/workflow_run_engine/resume.rs:12-146`@`ebd5b725`），只提供 desktop 的“保留现状”（keep）模式：工作区文件不动。`GET …/resume-preview` 与 `POST …/resume` 都由 `internal/workflow` 的 `planResume` 一处给出计划，续跑在运行锁内按锁住的行重新计划，过时的预览绕不过任何规则。规则：

- 只接受 `failed` / `cancelled` 运行（`run_not_ended`）；运行里还有正在执行的节点时拒绝（`node_running`，空闲门；被结束的运行停住的 Loop / Iteration 行不算，`region.rs:12-24`）；没有失败或取消的节点时拒绝（`no_failed_node`，页面此时不显示「从失败处继续」）。v2 另加三条：挡住空闲门的只剩同波次失败后留在 `running` 的 swift 行（Start、Output、Condition、Aggregator；desktop 同样留下它们，`engine.rs:406-439`）时按 `node_stuck` 拒绝，这些行不会再结束，页面让用户「再跑一次」；要清掉的节点里有已冻结的确认表单时拒绝：表单没有提交（运行停在确认页时被取消）按 `review_form`，表单的 `…/form/retry` 也不适用，页面让用户「再跑一次」；表单已经提交、却因为前面的容器从第一轮重来而被清掉时，失败的运行里任务创建也失败了（正是 `…/form/retry` 的前提）按 `review_form_retry`，页面让用户去确认页点「重试任务创建」，否则按 `review_form_submitted`，表单不能重新填写，页面让用户「再跑一次」。没有容器重来时，提交后任务创建失败的运行保留表单节点、正常续跑，只重跑任务创建。按 owner 决定 D25（2026-11-15），任务的处理运行（`v2_task_workflow_runs` 里的链接）按 `task_handling` 拒绝：它的链接只结算一次，续跑会在链接已结算的情况下替任务再执行一次，任务状态不再跟随，还会让同一任务同时有两个处理运行；页面让用户回到任务页重新开始处理，并隐藏「从失败处继续」和节点详情里的续跑提示。按 owner 决定 D30（2026-11-16），确认表单的「重试任务创建」（`POST …/form/retry`）同样会让失败的运行回到 running，所以处理运行也拒绝它：重试事务在运行锁下用与续跑同一个检查（`runTaskLink`）读任务链接，返回 409 `task_handling`，运行、任务和链接都不变，页面隐藏这个按钮并让用户回到任务页，任务创建失败时（首次派发和重放都一样）记录的节点错误也改为指回任务、不再提示重试，这段文字同时是任务页工作流历史里显示的失败原因；按 owner 决定 D32（2026-11-16），拆分运行（`v2_task_decompositions.workflow_run_id`）同样不能续跑，也不能「重试任务创建」：拆分在运行结束时只结算一次，续跑后的运行和拆分脱钩（子任务永远不会写入），又因不再挂在进行中的拆分上，重启时会被 boot sweep 当作普通运行吸收并继续执行。续跑预览和 `POST …/resume` 在续跑事务里、运行锁下按 `task_decomposition` 拒绝，表单重试返回 409 `task_decomposition`，任务创建失败的节点错误改为让用户回到任务页重新拆分；页面同样隐藏「从失败处继续」、节点详情的续跑提示和「重试任务创建」。没有任务的运行照旧可以续跑和重试。这条在续跑事务里、运行锁下检查，处理链接与运行在同一事务里创建，所以不会漏过；巡检、确认表单来源的运行不归任务所有（没有 `run.taskLink`），照常可以续跑。拆分链接在拆分运行创建为 pending、开始之前插入，所以能续跑的运行（有失败或取消的尝试）读到时链接一定已在。运行详情的 `run.taskLink`（`handle` / `decomposition`）告诉页面这次运行属于哪种链接。执行桥还在启动或结束这次运行的 Agent（例如刚停止的会话还没结束）时，预览和续跑都按 `run_settling` 说明稍后再试。
- 清除集合同 `resume_clear_node_ids`（`region.rs:56-120`）：每个失败或取消的节点连同它在根图上的全部下游；Loop 体内节点、Iteration 区域成员以及 Loop / Iteration 自身，续跑单位都是所在容器，清掉容器、全部成员和容器的全部下游，容器从第一轮重新开始（不做轮内续跑）。desktop 的规则只看行状态，所以 `continue` 策略下吸收了成员失败而成功结束的 Iteration 也会整体重来。
- 同一事务里：软删这些节点在所有 scope 的未删行；被清 Loop 行的轮次 scope 置为 `cancelled` 并软删其中的行；从运行 payload 去掉写入者被清的变量值（catalog 保留，`revision` 加一）、它们的 Condition 决策、Iteration 账本和逐轮决策；运行回到 `running`，清掉 `error`、`finished_at` 与 `current_nodes`。然后在同一个运行门禁下走普通调度从存活状态重算就绪集。成功节点保留行和输出，无关分支不动。
- 根节点和 Iteration 成员的尝试号按 `(运行, 节点, 迭代轮)` 接着数：被软删的行都计数，三次失败后续跑的是第 4 次尝试；新行不带 `auto_retry`，自动重试预算重新开始。Loop 成员例外：续跑替换了 Loop 行，新轮是新的 scope，按轮次 scope 计数（`workflowNodeAttempt`，见「Agent 自动重试」）从 1 重新开始。被替换的失败尝试留在 `failedAttempts`；根节点和 Iteration 成员的被运行页标为「已手动续跑」（按同一 `(节点, 迭代轮)` 的当前行判断），Loop 旧轮的不标。
- 进程内的派发：每次续跑让运行的进程状态换一代，派发的 Agent 记下自己所属的代；还没开始或已经过了放弃检查、正在解析执行器的旧派发，在登记为执行中的尝试后都会再检查一次（代是否还是当前的、节点行是否还在运行），不满足就撤销登记、不启动会话。所以取消再续跑之后，取消前派发的尝试不会为已软删的行启动 Agent，续跑的新尝试照常执行；同一检查也让兄弟节点在解析执行器期间让运行失败时，这个尝试不再启动。
- 取消与续跑按运行串行：续跑进行中开始的取消会等它结束，再在运行门禁下连同重试门禁重新读运行状态（`FenceRetriesForCancel`），于是看到 running 并停止续跑派发的 Agent；取消进行中时续跑按 `run_settling` 拒绝。这把锁只在先按租户查到运行之后才建，有人持有或等待时才存在，用完即删，所以未知或别的租户的运行 id 不会留下状态；取消先读运行状态，读到之后才设重试门禁、取消重试计时器；状态读失败时取消到此为止，运行原样保留，门禁不设，计时器照常（被取消的计时器不会再有人重新武装）。
- 进程内的重试状态与 #1168 的结算一致：运行离开 running 时等待中的重试已在 PostgreSQL 里收尾，续跑成功后再取消该运行仍挂着的定时器并去掉取消门禁（`CancelRetries` 留下的），否则门禁会吞掉续跑后的下一次可重试失败。
- 续跑的 Agent 优先在运行持久化的发起者名下执行，运行的会话始终属于同一个人。旧运行没有发起人时，`WorkflowResumeStarter` 从已绑定的节点会话中按最早优先取所有者仍是本租户有效成员的那一条，沿用该成员的身份；没有符合条件的会话时才使用调用者。成功续跑在 `ResumeWorkflowEngineRun` 事务内补记发起人，续跑被拒绝时保留原值；本进程已为这个运行登记过执行身份（上次续跑在提交前失败、状态留在内存里）时补记的就是那个身份，保证库里的发起人和续跑 Agent 开的会话是同一个人；那个身份已不是本租户有效成员时不再沿用，改用这次算出的发起人，那份状态还有没收尾的派发或没确认结束的会话时按 `run_settling` 拒绝、稍后再试。运行记下的发起人已不是本租户有效成员（被移出团队或账号停用）时，预览和续跑都按 `starter_unavailable` 拒绝，不换人：续跑的 Agent 都以发起人的身份启动，只会被执行服务拒绝。页面让用户「再跑一次」以自己的身份重新运行；发起人回到团队或账号恢复后，同一运行又能续跑。确认表单的「重试任务创建」（`POST …/form/retry`）同样会让失败的运行回到 running，所以按同一规则拒绝：重试事务在运行锁下、改动任何状态之前用 `tenantAccess` 检查记下的发起人，失效时返回 409 `starter_unavailable`，运行、表单和节点行都不变（没有记下发起人的运行不检查）。执行服务或执行器目录拒绝启动 Agent、而这时运行的执行身份已不是有效成员（运行进行中发起人被移出、重启后恢复这类运行）时，节点按 `starter_unavailable` 失败，不自动重试，节点详情说明发起人已不可用。补记会扩大可见范围：这些早先的节点会话原先只有所有者自己能读，补记后同租户成员都能读。权限与取消相同（本租户成员）。
- 与 desktop 相同的边界：同波次失败后仍保留 `running` 的根作用域 swift 行（见下文“与 desktop 的差异”）会让空闲门一直关着；v2 用 `node_stuck` 说明这种情况，让用户「再跑一次」。
- 只有续跑能帮上忙的失败才在节点详情里提示「从失败处继续」：失败详情 `resumable` 为假的配置类失败（例如 `missing_agent_ref`，同一冻结快照续跑必然再失败）不提示，除非下次尝试会被告知这次失败。

Follows desktop's `resume_from_failure` (`crates/application/src/workflow_run/engine/engine.rs:169-211`, `crates/db/src/repository/workflow_run_engine/resume.rs:12-146` at `ebd5b725`) with only desktop's keep mode: workspace files are left as they are. Both `GET …/resume-preview` and `POST …/resume` take their plan from `planResume` in `internal/workflow`; the resume replans from the locked rows, so a stale preview cannot bypass a rule. Only failed or cancelled runs resume (`run_not_ended`); a node still executing refuses it (`node_running`, the idle gate; Loop and Iteration rows parked by the ended run do not count, `region.rs:12-24`), as does a run with no failed or cancelled node (`no_failed_node`; the page then hides the action). Owner decision D25 (2026-11-15): a task's handling run (a link in `v2_task_workflow_runs`) is refused as `task_handling`. Its link settles once, so a resume would execute for the task again with the link settled, leave the task's status behind and admit a second handling run on the same task; the page sends the user back to the task and hides the action and the node detail's resume hint. Owner decision D30 (2026-11-16) extends this to the review form's “Retry task creation” (`POST …/form/retry`), the other path that returns a failed run to running: the retry transaction reads the task link under the run lock with the same check as resume (`runTaskLink`) and refuses with 409 `task_handling`, leaving the run, the task and its link unchanged; the page hides that button and points back to the task, and the node error recorded for its failed task creation, on the first dispatch and on a replay alike, points back to the task instead of suggesting a retry (the same text is the failure the task page's workflow history shows), while unowned runs keep it. Owner decision D32 (2026-11-16) extends both refusals to a task decomposition's run (`v2_task_decompositions.workflow_run_id`): its decomposition settles once, when the run ends, so a resumed run would carry on detached from it, its subtasks never written, and, no longer tied to a running decomposition, would be absorbed by the boot sweep across a restart. The preview and `POST …/resume` refuse it as `task_decomposition` in the resume transaction under the run lock, the form retry answers 409 `task_decomposition`, the node error of a failed task creation sends the user back to the task to decompose again, and the page hides “Resume from failure”, the node detail's hint and “Retry task creation” for it. The check runs in the resume transaction under the run lock (the link is created with the run), and inspection and review-source runs, which no task owns (no `run.taskLink`), stay resumable; a decomposition link is inserted after its run is created pending and before it starts, so a run that can resume already has it; `run.taskLink` (`handle` / `decomposition`) tells the page which link owns the run. A frozen review form in the clear set is refused as `review_form` when it was never submitted, as `review_form_retry` when it was submitted, sits behind a restarting container and the failed run's task creation failed (the form's `…/form/retry` precondition, so the page points to its “Retry task creation”), and as `review_form_submitted` otherwise (the form cannot be filled again: “Run again”). The cancel/resume lock exists only while held or awaited, after the run was found in the caller's tenant, and a cancellation reads the run status before it sets the retry fence and cancels the retry timers, so one whose status read fails leaves the run as it was: unfenced, with its timers still armed (nothing would re-arm a cancelled one). v2 adds three more refusals: `node_stuck` when only swift rows a same-wave failure left `running` block the gate (desktop leaves them too, `engine.rs:406-439`; they never end, so the page points to “Run again”); the form refusals above, while a run whose task creation failed after submission, with no container restarting, keeps its form node and resumes normally, rerunning only the task creation; and `run_settling`, in the preview and the resume alike, while the bridge still starts or ends an Agent turn of the run (a stopped session that has not finished ending). Each resume advances the run's process generation and every Agent dispatch carries its generation; once a dispatch holds the start slot it checks again that its generation is current and its row still runs, and otherwise releases the slot without starting a session, so an attempt queued before a cancellation never starts for the row a later resume soft-deleted, and one whose run a sibling failed while it resolved its executor does not start either. Cancellation and resume are serialized per run: a cancellation that begins during a resume waits for it, then re-reads the status together with the retry fence under the run gate (`FenceRetriesForCancel`) and stops the resumed Agent, and a resume during a cancellation is refused as `run_settling`. The node detail suggests resuming only where it can help: not for a failure whose detail says `resumable: false` (a configuration failure such as `missing_agent_ref` that the frozen snapshot cannot change) unless the next attempt is told about it. The clear set is `resume_clear_node_ids` (`region.rs:56-120`): each failed or cancelled node with all its root-graph descendants; anything in a Loop body or Iteration region, and the container itself, resumes through the container, which is cleared with all its members and descendants and restarts from round 1. As in desktop, an Iteration that absorbed member failures under `continue` and succeeded restarts too. One transaction soft-deletes the cleared nodes' live rows in every scope, cancels the round scopes of cleared Loop rows and soft-deletes their rows, removes pool values written by cleared nodes (the catalog stays, `revision` increments), their Condition decisions, Iteration ledgers and per-round decisions, and returns the run to `running` without error, finish instant or current nodes; the normal scheduling pass then recomputes the ready set under the same run gate. Succeeded nodes and unrelated branches stay. For root nodes and Iteration members, attempt numbers continue per `(run, node, iteration)`: a resume after three failed attempts runs attempt 4, without `auto_retry`, so its retry budget is fresh. Loop members are the exception: the resume replaces the Loop row, so its new rounds are new scopes and, under the per-round counting of Loop members (`workflowNodeAttempt`, see “Agent automatic retry”), start again at 1. Replaced attempts stay in `failedAttempts`; the run view labels those of root nodes and Iteration members "resumed manually", each against the live row of the same `(node, iteration)`, and leaves the old Loop rounds' unlabelled. Waits are already settled in PostgreSQL when a run leaves running; after a resume the engine also cancels the run's leftover timers and drops a cancellation fence left by `CancelRetries`, which would otherwise swallow the resumed run's next retryable failure. Resumed Agents execute under the run's persisted starter, so the run's sessions keep one owner (when none is recorded yet, `WorkflowResumeStarter` takes the earliest bound node session whose owner is still an active member of the tenant, falls back to the caller when none qualifies, and persists that starter inside the `ResumeWorkflowEngineRun` transaction only once the resume commits, leaving `started_by` untouched if refused; when this process already admitted a resume of the run — one that failed before committing — that admission's owner is persisted instead, so the recorded starter and the sessions the resumed Agents open stay one user, unless that owner is no longer an active member of the tenant: then this resume's owner replaces it, or, while that state still has a dispatch in flight or a session End unconfirmed, the resume is refused as `run_settling` to retry shortly). A recorded starter who is no longer an active member of the tenant (removed, or the account disabled) refuses the preview and the resume as `starter_unavailable`; the run is not moved to someone else, since every resumed Agent would start under that identity and be refused. The page points to “Run again” under the user's own identity, and the same run resumes again once the starter is back. The review form's “Retry task creation” (`POST …/form/retry`) also returns a failed run to running, so it follows the same rule: the retry transaction checks the recorded starter with `tenantAccess` under the run lock before changing anything, and answers 409 `starter_unavailable` when they can no longer act, leaving the run, the form and its rows unchanged (a run without a recorded starter is not checked). When the execution service or the executor catalog refuses an Agent while the run's execution identity is no longer an active member (the starter removed mid-run, or a run recovered after a restart), the node fails as `starter_unavailable`, which never retries automatically, and the node detail says the starter is unavailable. Backfilling a starter widens who can read the run's earlier node sessions: before the resume only their owner could read them, afterwards every member of the tenant can, as for the sessions a resume opens. The permission is the cancellation's. Desktop's boundary remains: a root swift row left `running` after a same-wave failure keeps the idle gate closed, reported as `node_stuck`. A root sibling that finishes after another node failed the run records its outcome without changing the run, so the gate can open and the resume reuses that answer; for a late failure v2 deliberately keeps the original failure anchor (see “A late sibling outcome is recorded”).

容器的普通业务失败（Loop 上限、反馈/结束判断、LoopExit 结算失败、Iteration 的 `fail` 策略）只写错误消息，沿用 desktop 的轮次失败语义；初始化、计划或持久状态无效才标记 `invalid_run_payload`。Loop 子节点的 `error_detail` 归属于子节点，父节点只继承错误消息并保留自己的 payload；同轮活动兄弟节点取消时保留 `sibling_failed` 原因。

#### 八节点行为与验收入口

对照基线为 desktop [`df694c8836fc70b1c789cc7e8e523d4d5b1b6c66`](https://github.com/ora-space/desktop/tree/df694c8836fc70b1c789cc7e8e523d4d5b1b6c66)。下表描述可执行语义；所有节点共用下文的并发派发、取消和重启边界。

| 节点 | desktop 来源 | v2 行为 | 自动化入口 |
| --- | --- | --- | --- |
| Start | [control.rs][desktop-control]、[variable_pool.rs][desktop-pool] | 按声明校验并写入运行输入、初始变量与全局值；无效输入在启动边界拒绝 | [engine_test.go](internal/workflow/engine_test.go) 的输入与启动拒绝用例；[host_test.go](internal/workflowhost/host_test.go) 的双 Agent 链 |
| Agent | [executor.rs][desktop-executor]、[structured_output.rs][desktop-structured] | 使用公共执行服务；按变量池渲染任务，传递上游结果；结构化契约验证成功才写 `structured_output`，失败保留原回答 | [host_test.go](internal/workflowhost/host_test.go) 的结果映射、结构化分流、取消与重启；[interactive_test.go](internal/workflowhost/interactive_test.go) 的交互节点停住、追问、完成、竞争与取消；[workflow-runs.spec.ts](../frontend-v2/e2e/workflow-runs.spec.ts) |
| Output | [control.rs][desktop-control] | 从当前变量池解析命名选择器，按 JSON 对象输出；缺少所需值时失败 | [engine_test.go](internal/workflow/engine_test.go) 的完整链路；[workflow-runs.spec.ts](../frontend-v2/e2e/workflow-runs.spec.ts) 的结果页与刷新 |
| Condition | [condition.rs][desktop-condition] | 首个成立的 case 胜出，否则走 else；未激活分支不建行；决定按根、Loop scope 或 Iteration 下标隔离 | [condition_test.go](internal/workflow/condition_test.go)、[branch_test.go](internal/workflow/branch_test.go)、[server-run-adapter.test.ts](../frontend-v2/src/features/chat/workflow-run/server-run-adapter.test.ts) |
| Aggregator | [aggregator.rs][desktop-aggregator] | 取声明顺序中首个已赋值候选，保留 null 与 falsy 值；无候选或选择器无效时失败 | [branch_test.go](internal/workflow/branch_test.go) 的优先级与 falsy 表；[workflow-runs.spec.ts](../frontend-v2/e2e/workflow-runs.spec.ts) 的分支汇合 |
| Loop | [loop_config.rs][desktop-loop-config]、[loop_round.rs][desktop-loop-round] | 1 基 round、每轮新池、同快照反馈、until/上限和显式导出；轮次转换原子提交，命名输出之外不隐式写变量 | [loop_runtime_test.go](internal/workflow/loop_runtime_test.go)、[containers_test.go](internal/workflowhost/containers_test.go)、[workflow-containers.spec.ts](../frontend-v2/e2e/workflow-containers.spec.ts) |
| LoopExit | [loop_exit.rs][desktop-loop-exit] | 同波次优先，不创建其他待派发兄弟行；冻结结果、取消活动兄弟并等待 session End，未确认停止不推进 | [containers_test.go](internal/workflowhost/containers_test.go) 的同波次、活动会话与迟到回调，以及 [loop_exit_restart_test.go](internal/workflowhost/loop_exit_restart_test.go) 的待确认清理恢复、冻结结果和重复恢复；[workflow-containers.spec.ts](../frontend-v2/e2e/workflow-containers.spec.ts) 的退出原因与结果 |
| Iteration | [node_runtime/iteration.rs][desktop-iteration] | 0 基数组下标、串行 collect、空集合、默认 50 上限、fail/continue；账本与后继原子提交，未执行 collect 不复用旧值 | [containers_test.go](internal/workflowhost/containers_test.go) 的集合边界、子节点失败与缺失 collect；[workflow-containers.spec.ts](../frontend-v2/e2e/workflow-containers.spec.ts) 的逐轮会话 |

[desktop-control]: https://github.com/ora-space/desktop/blob/df694c8836fc70b1c789cc7e8e523d4d5b1b6c66/crates/application/src/workflow_run/engine/node_runtime/control.rs
[desktop-pool]: https://github.com/ora-space/desktop/blob/df694c8836fc70b1c789cc7e8e523d4d5b1b6c66/crates/application/src/workflow_run/engine/variable_pool.rs
[desktop-executor]: https://github.com/ora-space/desktop/blob/df694c8836fc70b1c789cc7e8e523d4d5b1b6c66/crates/backend/src/workflow/run/executor.rs
[desktop-structured]: https://github.com/ora-space/desktop/blob/df694c8836fc70b1c789cc7e8e523d4d5b1b6c66/crates/application/src/workflow_run/engine/structured_output.rs
[desktop-condition]: https://github.com/ora-space/desktop/blob/df694c8836fc70b1c789cc7e8e523d4d5b1b6c66/crates/application/src/workflow_run/engine/condition.rs
[desktop-aggregator]: https://github.com/ora-space/desktop/blob/df694c8836fc70b1c789cc7e8e523d4d5b1b6c66/crates/application/src/workflow_run/engine/aggregator.rs
[desktop-loop-config]: https://github.com/ora-space/desktop/blob/df694c8836fc70b1c789cc7e8e523d4d5b1b6c66/crates/application/src/workflow_run/engine/loop_config.rs
[desktop-loop-round]: https://github.com/ora-space/desktop/blob/df694c8836fc70b1c789cc7e8e523d4d5b1b6c66/crates/db/src/repository/workflow_run_engine/loop_round.rs
[desktop-loop-exit]: https://github.com/ora-space/desktop/blob/df694c8836fc70b1c789cc7e8e523d4d5b1b6c66/crates/backend/src/workflow/run/executor/loop_exit.rs
[desktop-iteration]: https://github.com/ora-space/desktop/blob/df694c8836fc70b1c789cc7e8e523d4d5b1b6c66/crates/application/src/workflow_run/engine/node_runtime/iteration.rs

领域测试验证纯规则；`workflowhost` 用真实 PostgreSQL 与可控执行替身验证事务、取消和回调次序。
浏览器验收运行真实前后端；Playwright 启动 v2-server 时传 `-test-fixtures`。设置
`V2_NODE_ASSETS_DIR` 并准备 Node 资产后，以下组合通过真实 Node + fixture 执行全部八类节点，
并检查轮次选择、会话归属和失败提示：

```sh
cd frontend-v2
npm run e2e -- e2e/workflow-runs.spec.ts e2e/workflow-containers.spec.ts e2e/schedule-workflows.spec.ts
```

命令从仓库根目录运行；Node 数据目录用独立的 `V2_DATA_DIR`，并与测试数据库配对。没有 Node 资产时，真实 Agent 容器用例按条件跳过，分支用例只验收缺少执行器的失败路径，不能据此认定 Agent 和 Aggregator 的成功链路已经验证。fixture 的回答是确定性的，外部模型服务的输出稳定性不在这些测试的证据范围内。

`schedule-workflows.spec.ts` 还覆盖定时任务的“立即运行”入口：固定发布版本与初始输入，等待 occurrence 结算，在任务历史读取结果，再从对话页项目下打开同一运行，刷新后核对根变量、Loop 反馈、Iteration 逐项任务及各次尝试的会话流。这里验证的是入口合流，不代替真实时钟到点触发、进程重启或调度故障的消费侧验收。

与 desktop 的差异：

- **同一运行的 Agent 并发执行**（与 desktop 相同，列在这里说明做法；Loop 轮次失败时的处理除外，见本条末尾）：同一波就绪的 Agent 各自启动（受下面的会话上限约束），每个尝试是一个独立的执行会话，共用运行的目录，轮次同时执行；交互节点的后续消息同样不等其他节点。同一目录最多 8 个未结束的会话（Node 的 `shared_checkout_sessions` 上限，等待输入的交互节点会话也算在内），执行桥按这个上限限流：运行的未结束会话已满时，新尝试在启动会话前等待，有会话结束（普通尝试结束会话、交互节点完成、停止或随运行关闭）再开始；取消运行或关闭它所在的 Loop 轮次时，等待中的尝试不再启动。会话等 End 得到确认（成功，或报会话不存在、已结束）才让出名额：运行还在执行时，没确认的会话继续占名额，后台从 1 秒起、翻倍到 30 秒重试 End；运行已失败、取消（包括停止未确认、运行还开着的取消）或结束时不再重试，名额直接让出，会话留给项目里的下一次 End——每次没得到停止证明的 End 都会让执行服务重建项目的 Node，连带结束同项目的其他会话，不该为一个用不上的名额提早触发。End 一直确认不了的会话在运行执行期间一直占着名额：8 个名额都被这样的会话占着时，新尝试一直显示「等待会话名额」，不报错；取消运行或重启进程可以解开（重启时先证明旧 Node 进程已停）。重启只保留合法的交互等待（见「交互等待跨重启」）：它们重启后继续占名额，新 Agent 要到下一轮才起，这样计数偏保守；重启前会话已结束、End 没确认的停住节点，恢复时再 End 一次，确认后让出名额。Node 仍拒绝某个会话时只有它自己失败，普通 Agent 节点以 `session` 失败并照常自动重试，交互节点首轮被拒以 `interactive_session` 失败（可从失败处继续）。每个尝试启动前、以及解析完执行器即将 `Start` 前各确认一次运行仍在执行：运行已经结束时它还没有启动任何东西，直接收为 `cancelled`。已经在执行的兄弟不受另一个节点让运行失败的影响，迟到结果照常记录（desktop 同样如此）；Loop 轮次因子节点失败关闭时，已在执行的同轮兄弟收为 `cancelled`，执行桥随即停掉它们的 turn（不等停止确认），迟到结果不再写入；desktop 让它们执行完本轮，这里有意偏离（见 `internal/workflow/README.md`）。代价是被停的 turn 可能在运行目录里留下写了一半的文件：续跑保留文件、从第 1 轮重开 Loop，就从这些文件开始。
- **prompt**：按 desktop 的 `assemble_workflow_prompt` 组装 `workspace_root`、`current_workflow_step`、`workflow_context`，按运行冻结的 locale 选文案；选择角色且角色正文非空白时，按 desktop 的模板在 `workspace_root` 之后、`current_workflow_step` 之前插入 `<system_instructions>`（角色说明、引用名、正文），选择 Skill 时附加 `required_skills`；启用 `injectLastFailure`（默认开启）且同一节点、同一 Iteration 项存在可注入的上一次失败尝试（不区分 Loop 轮次：续跑从第 1 轮重放时，第 1 轮会拿到后面轮次的失败，和 desktop 一致）（`structured_output`、`agent_refusal`、`unknown_stop_reason`、`multiple_outputs`）时，在 `required_skills` 之后追加上次失败的文字说明（`structured_output` 另附截断的原输出），写入 `agentPrompt`（`workflowhost/prompt.go`），同一段文字也记在尝试的 `payload.injected_failure_context` 里、显示在节点详情中，同租户成员亦可在重跑或续跑尝试的 `turn_ended.prompt` 里看到。节点 prompt 的 `{{#…#}}` 先按变量池渲染，渲染失败的节点以 `prompt_template` 失败，不启动 Agent。公共执行服务在接受 prompt 之后才在 Node 里克隆运行目录，也不暴露它的路径，所以 `workspace_root` 写 `.`（Agent 进程的工作目录就是运行目录），不写 desktop 的绝对路径。
- **会话绑定时机**：`Start` 返回 turn 引用即视为 prompt 已被接受，随后把 session 绑定到节点尝试（desktop 同样在 prompt 被接受后才发布绑定）；绑定被拒（尝试已不在运行）时结束该会话。turn 结束后执行桥结束会话，对话历史保留；节点的 Agent 在后台起的作业（开发服务器、Gradle、sccache 这类守护进程）随会话被杀，不会留给后继节点，见「会话结束时杀掉的进程」。
- **结果映射**：只有 `completed` 且答案完整（`AnswerComplete`）才可能成功，再按 desktop 的停止原因映射：`end_turn`、`max_tokens`、`max_turn_requests` 成功；`refusal` 为 `agent_refusal`；缺少停止原因为 `session_ended_without_stop_reason`；其他为 `unknown_stop_reason`。turn 失败、取消、中断或答案不完整一律以 `session` 失败，失败节点保留已有的回答。
- **取消**：先用发起运行的用户身份同时取消所有活动 turn，全部确认后再关闭运行；还在 Start 的尝试不拖住别的停止，它的 Start 一返回就取消它。任一停止未确认（包括等 Start 超时）时返回 `503 workflow_run_stop_unconfirmed`，运行保持打开但不再推进；已确认停止的兄弟不会提前关闭运行，最后一个 turn 结束后才由执行桥关闭为已取消；超时后才返回的 Start 由它的尝试自己取消。
- **运行发起者持久化**：启动门禁内保存 started_by，表单默认确认人为实际发起者。执行中的尝试仍只在进程内记录；取消等 Start 超时后补发的停止在后台进行，进程关闭时最多再等它们约 `StopTimeout` + `execution.TurnStopSettleLimit`（默认 30 + 60 = 90 秒）：补发的停止先等 Node 结算只停这一轮的停止（最多 60 秒），等不到才结束会话并等停止确认；普通 Agent 执行重启后中断，冻结表单及提交后的确定性任务步骤可以恢复。拆分运行同样恢复已提交的审核，但停在未提交表单的拆分运行会在重启时取消（见上文拆分的“重启”）；任务的处理运行在重启时一律取消。
- **失效事件**：`workflowhost.Hub` 在每次提交的状态转移、会话绑定和取消后按运行发布一个不带数据的失效通知，供运行事件流使用；读者收到后重新读取运行，未消费的通知会合并。
- **失败后的操作**：失败或已取消的运行，顶栏有两个入口：「从失败处继续」在同一次运行里重跑失败节点及其下游（见下文），“再跑一次”创建新的运行再从 Start 启动。失败提示同时指向两者。
- **迟到的兄弟结果照常记录**：另一个节点让运行失败后，仍在执行的根作用域兄弟（例如 Condition 失败时正在跑的 Agent）结束时照 desktop 记下自己的成功或失败（`scoped_node.rs:54-112、160-240`@`ebd5b725`）：成功折进变量池，失败写 `error_detail`，都不改运行状态、不再调度。否则这一行会一直是 running，续跑的空闲门永远打不开；续跑时复用这个迟到的回答。迟到的失败有一处是有意偏离 desktop：desktop 还会再结算一次等待中的重试（`scoped_node.rs:182`），并把 `current_nodes` 改成迟到节点的锚点（`scoped_node.rs:225-230`）；v2 两样都不做——运行离开 running 时等待已经结算过，失败锚点要一直指向真正让运行失败的节点（运行页和运行的错误都按它显示）。
- **A late sibling outcome is recorded (English)**: after another node failed the run, a root sibling that was still executing records its success (folded into the pool) or failure (`error_detail`) like desktop, without changing the run or scheduling, so the resume idle gate can open. For a late failure v2 deliberately deviates from desktop, which also re-settles retry waits (`scoped_node.rs:182`) and moves `current_nodes` to the late node (`scoped_node.rs:225-230`): the waits were settled when the run left running, and the anchor keeps naming the node that actually failed the run.
- **根作用域的并行 swift 行**：同波次一个 swift 节点失败后，另一个节点行可能仍保留 `running`，desktop 基线也有这一表现。运行本身已经失败，历史展示应以运行终态判断能否继续，不能将该行解释为仍有执行活动。Loop scope 的兄弟节点取消不受这一边界影响。
- **同轮兄弟取消的错误展示**：`sibling_failed` 以 `{"reason":"sibling_failed"}` 写入节点错误，详情会显示这段 JSON；与 desktop 相同，尚未转成面向用户的说明。
- **失败节点的执行元数据**：`executor`、`model` 和渲染后的 `task` 随成功结果写入；包括结构化契约失败在内的失败结果不承诺这些字段。原回答与错误仍保留，界面不能用配置值冒充这次实际使用的执行元数据。

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

- 登录：`email` 只凭邮箱、没有密码，只认 `users.json` 预设或被邀请过的邮箱，仅用于开发和演示；`huawei-idaas` 是机密客户端的授权码流程，不用 PKCE。进行中的登录只存在进程内存里（10 分钟、单次），重启后要重新登录发起；同一个人用两种方式登录是两个账号，不做账号关联。
- 会话存在同一个库里，只存 token 的 SHA-256；没有 Gateway 现签的内部 JWT。desktop 令牌和网页会话同表，没有轮换、绝对上限、设备列表或远程吊销（A6 D7）；过期的行不清理，只是不再被认出。
- 对话与插件市场写请求检查 `Idempotency-Key`；插件市场同时保存成功响应以重放同一选择。其余写请求不检查该头，也没有执行代次隔离。定时任务配置与小队更新使用乐观版本检查，对话的创建和发消息把 `Idempotency-Key` 用作执行的 attempt ID。
- 小队权限把 workspace `owner` 角色当作工作区管理员；把成员移出工作区（`DELETE /api/v2/tenants/{tid}/members/{uid}`）会同时结束他在本工作区小队里的编组。
- 空间与插件：每个租户固定一个空间（slug `demo`）。演示团队的叫「演示空间」，开发启动预置账号时创建；`admin-invite` 建出的工作区在第一个人加入时创建，叫「协作空间」，不带演示字样。里面登记两个真实安装、不可从设置页移除的 Agent 插件——`official/ora-space.opencode@0.6.11` 和 `official/ora-space.claude@0.4.5`，即 [`scripts/prepare-node.sh`](scripts/prepare-node.sh) 校验 SHA 后准备、离线包打进去、chat、工作流与定时任务真正加载的那两个，所以登记的版本就是已安装的版本，不是演示数据。目录来自配置的 marketplace Git 仓库，不读取仓库内硬编码条目；解析 `orax.toml`、README 和 logo，校验后事务替换 PostgreSQL 快照，同步失败或没有可见条目时保留上一版。MCP 通过租户安装和配置接口管理。Plugins 页对市场 Agent、Hook、合集等其他类型只浏览，不提供安装或移除（服务端拒绝新建这些选择，返回 `plugin_kind_not_installable`）；系统预装的 Agent 也只读展示「运行时预装」。Skill 就是空间选择本身，写入即可用、删除即移除，项目 Node 只按选择接收安装包，不参与状态确认。工作流插件由 v2-server 下载并校验 `.orax`，在安装事务里导入 `assets/workflows/*.json`、发布 `v1` 并记录来源幂等表；租户已有同名工作流时给新导入的定义加「（插件）」或序号，不改动已有定义；移除只清除选择，不删除工作流、快照或运行历史。安装写入口要求 owner、`Idempotency-Key` 和当前 version。
- 成员列表：`memberships` 没有成员 id、版本和加入来源，所以 `id` 直接用 user id、`version` 恒为 1、`status` 取用户状态，`joinMethod` / `joinedAt` / `actorDisplayName` 恒为空；分页参数被忽略，一页返回全部。切到正式 Cloud 的成员接口时替换。
- 数据库迁移只有前进方向、启动时自动执行，没有单独的 migrate 命令和回滚脚本；引入迁移之前的数据库直接重建，不做衔接（见「数据库迁移」）。
- 本地 Node 的传输、监督和配套服务位于 [`internal/node`](internal/node/README.md) 与
  [`internal/nodeservices`](internal/nodeservices/README.md)，通过项目 Agent driver 接入服务入口。
  配套服务只支持演示仓库的只读 Git、固定 OpenCode / Claude Code 包、各协议的 fixture，以及配置的 OpenAI / Anthropic 兼容 provider；
  Claude Code 的冻结配置没有个人 Settings（`settings_json` 为 `{}`、revision 为 0）；
  本地证书随实例创建、有效期 10 年，连续运行不会因证书到期而失效；关闭后撤销凭据，无正式 Cloud 的身份、Git 交付或证书轮换机制。
- [`internal/nodehistory`](internal/nodehistory/README.md) 将固定 Node 的历史转换为替换快照，
  只读其本地 JSONL 恢复完整结果；历史缺失、损坏或超限明确失败。提供真实 Node + PostgreSQL
  的超长回答验证；公共执行服务在 PostgreSQL 保存历史快照与终态后才 ACK Node。

定时运行的内部状态与只读核对查询见 [状态说明](internal/store/occurrence-states.md)。采用同步接收后的原身份核对：reconciling 不重派，execution_busy 不表示跨任务争抢目录，只有 End 确认后释放同任务占位。schema 只接受当前状态枚举；本层不启动执行或定时器。

### 空间插件市场

设置 → 插件保留共享官方市场，允许 owner 添加空间私有的公共 HTTPS Git 仓库。来源管理接口见 `market-sources.openapi.json`；目录和安装选择继续使用原 Cloud 客户端。自定义来源按仓库分配命名空间。同仓库的不同 URL 写法共享身份；改到另一仓库会更换命名空间并清空旧目录，改到已配置的仓库返回冲突。官方市场在后台启动首次同步，手动同步每分钟最多触发一次（超出返回 429）。每次同步只提交该来源的完整快照，失败保留上次成功内容。停用或移除来源不会删除空间选择。

目录条目的 `packMembers` 在正式 Cloud 契约中表示合集的成员插件；v2 在空间已安装的 Skill 条目上把它改填为该包运行时物化的技能名（`assets/<name>/SKILL.md`，安装时从校验过的 `.orax` 读出），供设置页做同名提示。这是对 Cloud 契约语义的偏离，也是 Cloud 的候选：Cloud 若要提供同样的提示，需要在目录条目上给出 Skill 包的运行时技能名（复用该字段或新增字段）；在那之前，`frontend-v2` 改连 Cloud 后同名提示会退回按 README 标题匹配。

自定义来源的 Git 连接和 Skill 包下载只允许访问本次解析并固定的公网地址；禁止二次请求连接其他主机、私网、保留地址和 Git 重定向。本地透明代理常用 `198.18.0.0/15` 作为 fake-IP 应答，此时只对 DNS 域名固定该代理地址，字面量同一网段地址仍拒绝。来源启用代理时，可选 `V2_MARKET_SOCKS_PROXY` 为部署信任的 SOCKS5 代理；客户端先解析并校验公网 IP，再把数字地址交给代理，避免代理重新解析到其他端点。空间不能指定代理地址。S3 凭据以 AES-256-GCM 加密，S3 来源需要部署持久提供 `V2_MARKET_CREDENTIAL_KEY`（Base64 格式的 32 字节随机密钥），备份数据库时单独保管密钥。凭据绑定到空间、来源、端点和桶，不通过读取接口返回；删除来源时会在同一事务中清空对应安装计划保存的加密凭据。自定义来源还支持 HTTPS Git 认证（用户名加令牌或密码），同样加密存库、绑定到空间和来源、读取接口只返回是否已配置（`gitConfigured`）而不返回凭据。来源的命名空间由仓库地址决定，凭据绑定在命名空间上，所以更改已保存 Git 认证的来源的仓库地址时，必须同时填写新凭据或清除旧凭据，否则返回 `market_credentials_unavailable`。同步时凭据以 `http.extraHeader` 的形式通过 git 环境注入（经 `GIT_CONFIG_COUNT` 环境变量配置），不进入仓库地址、命令行参数、日志或工作目录的 git 配置；认证失败会与网络失败区分，在来源上显示"同步失败：仓库认证失败，请检查 Git 用户名和令牌。"。

### 租户 MCP 安装和配置

独立候选契约为 [mcp.openapi.json](mcp.openapi.json)。MCP 按租户安装，空间的 owner 从该空间可见的插件目录选择版本，租户成员共享设置。服务端使用已有的市场下载与凭据规则，验证 SHA-256、归档身份和配置声明后，将原始包缓存到 PostgreSQL。界面的「已安装」表示服务端缓存就绪。GET `/api/v2/tenants/{tid}/workflow-runs/{rid}/nodes/{nodeRunId}/mcp` 返回该次尝试的选中成员、目标配置及实时 ACP 和宿主状态。发起人可 POST 同一路径下的 `/probe`，按当前 generation 对单个已配置成员重新探测。

| 路由 | 行为 |
|---|---|
| GET /api/v1/tenants/{tid}/mcp | 成员读取安装、版本、设置声明、配置完整性与缺失标识，秘密仅返回已配置标志 |
| POST /api/v1/tenants/{tid}/spaces/{sid}/mcp | owner 安装明确版本，同版本同摘要可重复调用，升级带当前 revision |
| PUT /api/v1/tenants/{tid}/spaces/{sid}/mcp/settings | owner 以 revision 合并 values，clear 仅清除指定覆盖值，省略秘密保留原值 |
| DELETE /api/v1/tenants/{tid}/spaces/{sid}/mcp | owner 按 revision 删除租户安装，后续节点无法再选择它 |

设置复用 V2_MARKET_CREDENTIAL_KEY，以 AES-256-GCM 在写入 SQL 前密封，附加数据绑定租户与插件。密钥缺失或不可解密时返回 mcp_credentials_unavailable。读取接口隐藏 secret 设置的值及默认值；HTTP Authorization 或 Proxy-Authorization 引用的设置也视为秘密，兼容未声明 secret 的旧包。升级包会清空旧覆盖值。正在执行的节点拥有独立冻结快照，配置和卸载影响后续会话。

每个租户与插件的 revision 独立递增，卸载后保留版本计数。重新安装不会复用旧 revision，旧页面提交的设置或删除请求返回版本冲突；升级已有数据库时先从当前安装记录回填版本计数。

工作流只解析启用且明确选择的 MCP。配置缺失会标出 plugin_id、setting_id 或 transport。部署密钥缺失或轮换导致解密失败时，自动节点沿用会话重试策略，交互节点保留人工恢复。插件页会提示检查部署密钥，owner 仍可移除受影响的安装。驱动检查 Node 的 `session_mcp_lifecycle` 能力。首轮以 generation 0 和显式选择构造，追加轮携带各自的 generation 下限。设置值仅供内存派发，执行记录保留无值身份。使用 `TEST_DATABASE_URL`、`V2_NODE_ASSETS_DIR` 运行 `go test -race -run '^TestRealWorkflowMCPLifecycle$' ./internal/workflowhost` 可验证真实 Node 的生命周期行为。

### 项目 Skill 安装

空间选择冻结 Skill 的来源、版本、目标平台和 SHA-256，已上线的项目 Node 通过 `install_plugins` / `remove_plugins` 接收并应用。状态不经过 Node：选择写入即为已安装，删除选择行即为移除，所以没有绑定项目、Node 离线或项目还没应用时，设置页和工作流节点的技能候选照常反映选择结果。历史项目绑定到同一租户的默认演示空间；创建项目可传 `spaceId`，省略则沿用默认演示空间。目录升级不会改变现有选择。成员可读项目回执；项目回执只记录交付情况，不再决定空间的已安装状态。

下载限制为公网 HTTPS、归属来源的 S3 SigV4 或直接 HTTPS；DNS 域名经本地透明代理的 fake-IP（`198.18.0.0/15`）应答时仍固定该地址，字面量地址不放宽。官方市场的包在每次下发时按当前部署配置选择下载方式：设了合法的 `V2_MARKET_SOCKS_PROXY` 时走该代理，否则直连，与安装时一致，所以改动代理设置对已有安装计划同样生效；自定义来源沿用其计划冻结的下载方式和凭据。校验摘要和归档中的 Skill 标识、版本后，才通过节点已有的包服务交付。网络、临时 HTTP 状态、Node 中途退出等可恢复错误会按 5 秒起步、最长 5 分钟的指数退避自动重试；摘要不符、包校验失败等确定错误立即失败，并把可定位的安全摘要写入项目回执和服务端日志，不影响空间选择本身。技能在项目工作副本的 `.opencode/skills`、`.claude/skills` 中发现；已有项目文件不被覆盖。同名时市场优先：仅由 Ora 物化出来的旧用户技能目录（登记在服务自己的 exclude 标题段下）被市场链接替换，项目自带的技能保留并向服务端返回带技能名的冲突。Agent 的固定运行时版本保持独立；市场 Agent、Hook 和其他未验证类型仅供浏览，MCP 使用租户安装接口。

与 Desktop 的差异：

- Desktop 从 Agent 插件声明中发现 Skill 目录，复制到工作区并写 ownership marker；v2 固定使用 `.opencode/skills`、`.claude/skills`，以 symlink 指向该项目的 Node 内部安装目录，因此依赖当前 Node 的目录布局。
- Desktop 会先让 Agent 停下，再在会话进行中应用变化；v2 只在项目 Node 已在线且项目空闲时安装或移除。新选择要等该项目有一个会话结束后才生效，服务启动后的第一个会话也拿不到尚未应用的 Skill。

测试真实 Node 时设置 `V2_NODE_ASSETS_DIR`、`V2_TEST_SKILL_ARCHIVE`（官方 Matt Pocock Skills 1.3.1）和可选的 `V2_TEST_SKILL_UPDATE_ARCHIVE`（1.3.2），在 Linux 原生、路径足够短的用户目录下运行 `go test -run TestRealMarketSkillNode ./internal/agentruntime`。该测试用真实 OpenCode 读取插件 Skill 和工作流所选个人 Skill，并验证下一节点清除两者；模型响应仍为 fixture，不验证真实模型的指令遵循能力。

同时提供 `TEST_DATABASE_URL` 后，`go test -race -run TestRealWorkflowSelectedSkills ./internal/workflowhost` 覆盖发布后的完整两节点工作流：真实插件安装回执、节点选择、执行会话的数据库快照、Agent 读取产物与下一节点清理。模型请求仍由 fixture 提供，MCP 不在该测试范围内。

## 动态表单与巡检提单

`form` 负责冻结证据并等待确认，`createTasks` 根据确认结果创建或关联任务；二者使用 `workflows.openapi.json` 中的 v2 契约。表单的 GET/PUT/POST `/api/v2/tenants/{tid}/workflow-runs/{rid}/form` 与 POST `…/form/retry` 属于正式 Cloud 尚未提供的候选接口。行为、权限、幂等、去重与恢复边界见 [动态表单说明](internal/workflowforms/README.md)。等待确认的轮次不占用下一晚的自动执行资格，但仍保留未完成状态和终态跟踪。DTS 只有前端演示回执，不代表外部工单已提交。

任务时间轴中的 `TaskRun.demoSeed` 明确区分未执行的预置历史样例。它依据 seed 专用会话身份与启动键的一致关系投影，既有样例也会标记，不需要数据迁移；实际运行即使使用 fixture 模型也为 false。团队概览排除这类样例的运行数和 Agent 产出，会话分析仍保留样例以供检查。

## 内网部署

首次部署构建和长期安装/升级步骤见 [部署说明](docs/deployment.md)（[English](docs/deployment.en.md)）。
开发启动设 `V2_LOGIN=email`，demo 与开发 task 已设置。部署设 `V2_LOGIN=huawei-idaas` 和 IDaaS 配置，第一个管理员用 `v2-server admin-invite` 生成链接，见部署说明。`V2_FRONTEND_DIR` 指向前端构建目录，空值保留 API-only。
`V2_PLUGIN_MARKETPLACE_URL` 未设置保留开发默认，显式空字符串关闭官方同步和目录，保留自定义源；`V2_MARKET_CREDENTIAL_KEY` 同步保护自定义市场 S3/Git 凭据、租户 MCP 设置与私有项目仓库令牌。

### 日志有界保留

Node 子进程的诊断日志（`<Node 可执行文件>.log`）和每个项目的事件日志 `node-events.jsonl` 有界保留：单文件分别 10 MiB、64 MiB，各保留 3 份（含当前文件）；超过预算即轮转为 `.1`/`.2` 并继续写当前文件，不丢 envelope、不改变“写入后 `Sync`”的顺序，失败会从写入或 `Sync` 返回而不是静默丢事件（#1191 方案 A 的预算）。工作目录回收和 Node 会话历史 JSONL 不在本项范围内。

部署边界依据：[Cloud 单进程（A1）](https://github.com/ora-space/specs/blob/main/decisions/cloud/lifecycle/0-single-process-cloud-on-enterprise-intranet.md) 和[登录与会话（A6）](https://github.com/ora-space/specs/blob/main/decisions/cloud/login/0-idaas-or-email-login-with-shared-web-and-desktop-sessions.md)。

来源列表候选契约 `market-sources.openapi.json` 的 `officialEnabled` 返回部署是否启用官方来源，
不依赖同步结果或目录条目；`items` 仍只列空间自定义来源，不将官方来源作为可编辑条目。
