# Ora Cloud

[中文](README.md) | [English](README.en.md)

阶段一实现：Go/Gin cloud 核心、PostgreSQL 权威持久化、内部认证和有限控制契约，以及使用真实 HTTP、PG、磁盘和 Git 的模拟执行组件。此仓库尚未完成 Rust Controller/Workspace Node 拆分、Desktop 重构或 Kubernetes 部署。

需要 Go 1.27.1、Git、PostgreSQL 17、可选的 Task，以及 `buf`（≥ 1.73；`task check` 会 lint 契约并重新生成 `internal/controlpb` 检查漂移，见 [`proto/`](proto/README.md)）。数据库通过 GORM 初始化并注入，事务层执行参数化 PostgreSQL SQL；没有全局 DB、SQLite/MySQL 示例用户 CRUD，也没有生产启动 AutoMigrate。

## 本地验证

Windows 可在项目 `.local/` 隔离安装并启动 PostgreSQL 17.11，不创建系统服务：

```powershell
./scripts/postgres.ps1 start
$env:TEST_DATABASE_URL='host=127.0.0.1 port=55432 user=postgres dbname=ora_test sslmode=disable'
task check
```

脚本仅监听 `127.0.0.1:55432`，使用本地测试 trust 认证。二进制来自 [EDB PostgreSQL Windows 分发](https://www.enterprisedb.com/download-postgresql-binaries)，固定版本和 SHA256。停止用 `./scripts/postgres.ps1 stop`，数据保留。

也可使用 Docker：

```sh
docker compose up -d --wait
export TEST_DATABASE_URL='host=127.0.0.1 port=55432 user=ora password=ora-local dbname=ora sslmode=disable'
task check
task test:race
```

配置过 `config.toml` 并执行 `task setup` 后不必再手工 export：`.local/dev.env` 会为每个 task 提供 `TEST_DATABASE_URL`（shell 中已有的值优先）。测试为每个用例建立独立 PG schema 并自动清理，测试账号需要 CREATE SCHEMA 权限。`task check/test/test:integration/test:race` 会设置 `REQUIRE_POSTGRES=1`；缺少真实 PG 配置会失败，不能静默跳过。直接 `go test ./...` 未配置 PG 时会显式跳过 integration，用 `task test:unit` 可单独运行非 PG 测试。

Windows race 需要可用 C 编译器：

```powershell
$env:CC='D:\tmp\ora-cloud-test-tools\llvm-mingw-20260908-ucrt-x86_64\bin\x86_64-w64-mingw32-gcc.exe'
$env:PATH=(Split-Path $env:CC)+';'+$env:PATH
task test:race
```

该路径是本次验证使用的隔离工具目录，其他机器设置自己的 MinGW/LLVM-MinGW `CC` 即可。Linux CI 使用系统 C 编译器。

## 运行

配置文件默认 `configs/config.yaml`，所有已有配置项可由 `CLOUD_` 环境变量覆盖，如 `CLOUD_DATABASE_DSN`。生产数据库使用 TLS、独立 DML 账号和部署迁移账号；不要使用示例本地 trust 配置。

```powershell
# 默认示例指向上面的本地 ora_test；真实部署请指定 -config。
go run ./cmd/cloudctl -command migrate
go run ./cmd/cloudctl -command bootstrap -name '研发组织' -source 'huawei-corp' -subject 'stable-account-id' -display-name '首位管理员'
go run ./cmd/cloudctl -command credential-ref -tenant '<tenant UUID>' -owner '<user UUID>' -secret-ref 'infra-secret://git/team/account'
```

`bootstrap` 原子创建租户、首位管理员与 `default` 空间，是部署操作；重复执行会新建租户。当 `-source` 为 `huawei-corp` 时，`-subject` 必须传入 IDaaS 返回的稳定 `uuid`（形如 `uuid~...`），勿填工号或 W3 账号——否则该员工首次登录会被当成新用户，并在前端被引导去创建自己的新租户。`credential-ref` 只保存基础设施引用，不接收 Git 密钥值；引用受 tenant+owner 外键约束。已登录用户也可以通过 `POST /api/v1/tenants` 为自己创建租户：请求体只有第一个协作空间的 `name` 与 `slug`，租户借用该名称，调用者成为租户管理员和空间 owner；租户是产品不展示的隐式容器。加入已有租户仍须先经有效 gateway 身份访问 `/api/v1/me` 建立 user，再由管理员通过成员 API 显式添加；没有外部组自动授权。

生产启动前在配置中设置内部验证公钥，见 [认证配置与凭据](docs/authentication.md)。空 trust 配置会启动失败：

```sh
go run ./cmd/server -config /path/to/config.yaml
```

server 只检查已执行迁移及 checksum，不执行 DDL；数据库、迁移、trust 或监听失败会非零退出。`GET /healthz` 检查 PG 可达性。

本地联调（浏览器 → Gateway → Cloud，见 [认证 Gateway](docs/gateway.md)）不需要 GitHub 账号：Gateway 自带仅限本地的开发者登录，输入任意身份即可进入。要测真实 GitHub 登录时再新建一个 OAuth App（callback 登记为 `http://localhost:5173/auth/callback/github`）并填进 `config.toml` 的 `[github]`。

```sh
cp config.toml.template config.toml   # 默认值即可用；[github] 可选。config.toml 被 Git 忽略
task setup                            # 生成密钥、写入 secret 文件与 .local/dev.env、应用迁移，可重复执行
task dev                              # Cloud :8080 + Gateway :8081 + 前端 :5173
```

`task setup`（[cmd/devsetup](cmd/devsetup/README.md)）把 `config.toml` 变成服务本来就接受的 `CLOUD_*`/`GATEWAY_*` 环境变量覆盖（`.local/dev.env`，`Taskfile.yml` 为每个 task 自动加载，也提供 `TEST_DATABASE_URL`）与 Gateway 读取的 client secret 文件；`configs/*.yaml` 仍是权威配置。首次登录的用户没有租户，前端会引导其创建第一个工作区。

可直接运行完整创建演示（独立生成短期模拟签名密钥，仅限进程内测试）：

```sh
go run ./cmd/cloudctl -command migrate
go run ./cmd/simulator
```

演示启动独立 loopback HTTP cloud/Substrate，创建测试租户、模拟 sandbox 和 Node，并把仓库真实 clone 到 main Workspace 自己的数据中，再输出 Ready Workspace。磁盘在 `.local/demo/`，PG 记录保留；再次运行创建新的演示租户。模拟器没有生产基础设施凭据，不部署 Kubernetes，不启动真实 Agent/Deno。

## 前端

正式 Web 前端在 [`frontend/`](frontend/README.md)：React 19 + TypeScript + Vite + Tailwind CSS 4 +
TanStack Query，API 层由 [orval](https://orval.dev) 从 [`api/openapi.json`](api/openapi.json) 编译
生成，独立安装、开发、构建：

```sh
cd frontend
npm ci                 # Node >= 24，见 frontend/.node-version
npm run dev            # Vite dev server（:5173），/api、/internal、/healthz 代理到 :8080
npm run build          # tsc -b && vite build
```

鉴权复用 Cloud 的双 JWT：浏览器只持有会话 cookie + 用户 profile，令牌由边缘服务 `cmd/ora-web`
在服务端签名（浏览器不可信）。dev 联调需真实 PostgreSQL，按上文「本地验证」起库并 `task run`
（:8080），再 `npm run dev`。

与 demo 的区分：`cmd/demo-issue-board-web` 是单文件 HTML 的看板演示（无前端依赖、无 CORS、
服务端签 JWT），只用于手工验证 Issue 接口；`frontend/` 才是正式产品前端。二者并存、互不替代。

## 契约与边界

- [OpenAPI 3.0](api/openapi.json)：所有 19 个公开接口、15 个内部接口和 health。`task openapi` 重新生成，测试校验文档合法性、生成结果和实际 HTTP 响应结构。
- [Web 前端](frontend/README.md)：`frontend/src/api` 由 orval 从同一份 `api/openapi.json` 生成带类型的 TanStack Query hooks，`task frontend:generate` 一次完成 Go 契约 → JSON → TypeScript；CI 检测生成物漂移。
- [核心不变量与状态机](docs/core-contract.md)：身份、归属、幂等、准入、租约、恢复和清理。
- [Substrate/Node 与阶段二边界](docs/execution-contract.md)：Workspace 数据、Node clone、Substrate 接口与迁移责任。
- [需求—实现—验证清单](docs/acceptance.md)：本次实际证据与未完成的阶段二验证。

## 模块架构与分层文档

每个子系统、服务命令与工具均遵循与 Ora 桌面端同等严谨的架构设计，并配备独立的模块级规范文档：

- **命令与运维入口 (`cmd/`)**：[入口总览 (`cmd/`)](cmd/README.md)
  - [服务守护进程 (`cmd/server`)](cmd/server/README.md)：生产环境 HTTP Daemon 核心。
  - [认证 Gateway (`cmd/gateway`)](cmd/gateway/README.md)：华为 IDaaS 或 GitHub OAuth 登录、PostgreSQL 浏览器会话与 `/api/v1` 反向代理。
  - [运维管理工具 (`cmd/cloudctl`)](cmd/cloudctl/README.md)：迁移执行、初始租户引导与凭据引用配置。
  - [本地执行模拟器 (`cmd/simulator`)](cmd/simulator/README.md)：内存与磁盘执行双工演示。
  - [OpenAPI 同步工具 (`cmd/openapi`)](cmd/openapi/README.md)：从 Go 契约自动编译导出 `api/openapi.json`。
  - [代码格式严检门禁 (`cmd/checkformat`)](cmd/checkformat/README.md)：CI 格式静态门禁。
- **内部核心子系统 (`internal/`)**：[子系统总览 (`internal/`)](internal/README.md)
  - [领域状态机引擎 (`internal/core`)](internal/core/README.md)：聚合根、事务与全局锁、乐观版本控制、租约与幂等。
  - [PostgreSQL 迁移目录 (`internal/core/migrations`)](internal/core/migrations/README.md)：0001~0005 线性 SQL 迁移与校验和防篡改校验。
  - [HTTP 路由网关 (`internal/api/router`)](internal/api/router/README.md)：Gin 路由分流、双重 JWT 校验、白名单与 Fault 映射。
  - [认证边界 (`internal/gateway`)](internal/gateway/README.md)：登录编排、Login Attempt/Session 存储、内部凭据签发、Cookie/CSRF 与代理；[华为 IDaaS 适配器 (`internal/gateway/idaas`)](internal/gateway/idaas/README.md)；[GitHub 适配器 (`internal/gateway/github`)](internal/gateway/github/README.md)。
  - [API 契约定义 (`internal/contract`)](internal/contract/README.md)：OpenAPI 3.0 数据模型与测试。
  - [数据库连接池管理 (`internal/repository`)](internal/repository/README.md)：GORM 连接池、快速探活与安全约束。
  - [配置解析与加载 (`internal/config`)](internal/config/README.md)：Viper 强类型配置与环境变量映射。
  - [结构化日志 (`internal/logger`)](internal/logger/README.md)：Zap + Lumberjack 轮转与平台适配。
  - [执行替身 (`internal/simulator`)](internal/simulator/README.md)：Substrate、Node 与 Controller 开发期替身。
- **公共库与集成测试**：
  - [公共导出边界 (`pkg/`)](pkg/README.md)：公共库导出策略与约束。
  - [集成测试套件 (`integration/`)](integration/README.md)：基于独立真实 PG Schema 的全链路集成测试。

阶段一采用数据库事务级全局 advisory lock 串行核心事务，并限制每 Project 一个未完成 operation。HTTP/Git/Substrate 调用从不持有数据库事务。此选择适用于首版单集群单活，牺牲写吞吐以降低并发不变量复杂度；后续可按租户/Project 细分锁，但必须保持现有并发测试。

容器打包 server/gateway/cloudctl，运行身份为非 root。构建用 `docker build -f scripts/Dockerfile -t ora-cloud:phase-one .`，挂载自有配置和公钥；镜像还带有前端构建产物 `/app/web`，设置 `GATEWAY_WEB_DIST_DIR=/app/web` 后由 Gateway 与 `/auth`、`/api` 同源提供（见 `docs/gateway.md`）；另有 `devsetup` target（`cmd/devsetup`，只用于本地生成密钥与执行迁移，不进入服务镜像），供 `ora-space/cluster` 的 Compose 整套环境使用；迁移使用同镜像 `--entrypoint /app/cloudctl` 独立执行，认证 Gateway 使用 `--entrypoint /app/gateway`（见 `docs/gateway.md`）。仓库 CI 分为两个 workflow：`Backend` 使用 PG service 跑格式/静态检查和 race 集成测试；`Frontend` 只在 `frontend/`、`api/` 或 `internal/contract/` 变化时触发，校验生成客户端与契约一致，并执行格式、lint、类型、测试覆盖率、模块文档/测试、死代码、重复代码与构建门禁（见 `frontend/AGENTS.md`）。Docker 镜像和真实部署不属于本地已验证结果。
