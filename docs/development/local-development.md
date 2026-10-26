# 本地开发

[English](local-development.en.md) | 中文

本文说明在一台开发机上把 Cloud、Gateway 和前端跑起来所需的工具、数据库和步骤。完整的多仓库环境（Controller、Sandbox Server、
真实 Node 与沙盒）由 [ora-space/cluster](https://github.com/ora-space/cluster) 的 Compose 提供。

## 工具

| 工具 | 用途 |
|---|---|
| Go（版本以 `go.mod` 为准） | 服务与测试 |
| Git | 测试与模拟器里的真实 clone |
| PostgreSQL 17 | 唯一支持的数据库，测试也必须用真实 PostgreSQL |
| [Task](https://taskfile.dev) | 仓库的任务入口，`task --list` 列出全部任务 |
| `buf` ≥ 1.73 | proto 的 lint、breaking 与生成物检查（`task check` 需要） |
| Node 24、npm ≥ 11.19 | 前端；只有 `task frontend:*` 需要 |

## 数据库

任选一种：

- **Docker**：`docker compose up -d --wait` 启动仓库自带的 PostgreSQL 17，监听 `127.0.0.1:55432`，账号 `ora`、密码 `ora-local`、数据库 `ora`，
  这也是 `config.toml.template` 的默认值。
- **Windows**：`./scripts/postgres.ps1 start` 在项目的 `.local/` 下安装并启动一个固定版本的 PostgreSQL 17，只监听 `127.0.0.1:55432`，使用本地
  trust 认证，不创建系统服务；`./scripts/postgres.ps1 stop` 停止，数据保留。
- **自己的服务器**：角色需要拥有数据库（迁移要建表，迁移测试要建 schema）；integration 与 core 普通 fixture 的独立模板副本还要求测试账号具备 `CREATEDB`。

## 配置与启动

```sh
cp config.toml.template config.toml   # config.toml 被 Git 忽略，可能含 secret，不要提交
task setup                            # 生成密钥与 secret 文件、写 .local/dev.env、执行迁移；可重复执行
task dev                              # Cloud :8080 + Gateway :8081 + Vite :5173
```

- `config.toml` 只填每台机器不同的值：数据库 DSN，以及可选的 `[github]` 或 `[idaas]`（至多一个）。两者都留空时，Gateway 只提供本地开发登录，
  输入任意身份即可进入。要测真实 GitHub 登录，新建一个 OAuth App，callback 登记为 `http://localhost:5173/auth/callback/github`。
- `task setup`（[`cmd/devsetup`](../../cmd/devsetup/README.md)）在 `.local/gateway/` 生成两对用途分离的 Ed25519 密钥和 PKCE 密钥（已存在的保留），
  写出 provider 的 client secret 文件和 `.local/dev.env`，并执行迁移。`.local/dev.env` 以 `CLOUD_*`、`GATEWAY_*` 环境变量覆盖 `configs/*.yaml`，
  还提供 `TEST_DATABASE_URL`；`Taskfile.yml` 为每个 task 自动加载它，shell 里已有的变量优先。`configs/*.yaml` 仍是权威配置。
- 浏览器只访问 `http://localhost:5173`：Vite 把 `/auth`、`/api`、`/healthz` 代理到 Gateway，所以 `public.base_url` 是这个地址，Cookie、同源检查和
  OAuth callback 都以它为准。首次登录的用户没有租户，前端会引导他创建第一个空间。

**控制监听的证书。** `task setup` 在 `.local/control/` 生成本地 CA（`ca.pem`/`ca.key`）、Cloud 服务证书和私钥（`server.pem`/`server.key`），
以及 Controller 客户端证书和私钥（`controller.pem`/`controller.key`）。服务证书支持 `localhost`、`cloud`、`127.0.0.1` 和 `::1`；
客户端 URI 为 `spiffe://ora.local/controller/local-controller`，与 cluster 使用相同的身份形状，但 CA 独立。
`.local/dev.env` 自动设置 `CLOUD_CONTROL_CERTIFICATE_FILE`、`CLOUD_CONTROL_PRIVATE_KEY_FILE`、`CLOUD_CONTROL_CLIENT_CA_FILE` 和
`CLOUD_CONTROL_CONTROLLER_IDENTITY`，无需手工配置。整套文件已存在时保留；缺少部分文件时拒绝继续，需要恢复缺失文件或删除整套后重新生成。
复用前会校验密钥匹配、CA 证书链、用途、SAN、Controller 身份及有效期；损坏或过期时明确报错并保留原文件。CA 有效期十年，叶证书一年，需显式修复或重新生成并更新对端材料。

本机 desktop Controller 连接 `https://localhost:8082`（`configs/config.yaml` 的 `control.grpc_addr`）时，使用 `controller.pem`、`controller.key`
和 `ca.pem`，Controller ID 为 `local-controller`；CA 私钥 `ca.key` 留在开发机，不交给 Controller。
Cloud 仍强制 TLS 1.3 双向认证，缺少配置仍拒绝启动。本地生成物只用于开发，不用于生产；cluster Compose 继续由 `management-setup` 生成和挂载材料。

单独启动各个部分：`task run`（Cloud）、`task run:gateway`（Gateway）、`task frontend:dev`（Vite）、`task migrate`（只执行迁移）。

## 不依赖 Controller 的演示

- **模拟器**：`task simulate`（`go run ./cmd/simulator`）在进程内启动 loopback 的 Cloud 和 Substrate 替身，创建测试租户、模拟沙盒和 Node，在 Git
  Server 替身（按 Git Server 卷布局存放的磁盘仓库，代替外部线登记演示 Project 的快照）里准备快照，再把该快照真实 clone 进 main Workspace
  自己的数据目录，最后输出一个就绪的 Workspace。Node 替身只从 Git Server 替身取快照，不访问外部仓库。磁盘数据在 `.local/demo/`，数据库记录保留，每次运行创建新的演示
  租户。它使用进程内生成的临时签名密钥，没有生产凭据，不启动真实的 Agent 或插件执行器，也不证明运行时独占、管理认证或工作负载隔离。
- **看板演示**：`cmd/demo-issue-board-web`（`scripts/demo-issue-board-web.sh`）是单文件 HTML 的 Issue 看板演示，服务端签 JWT，只用于手工验证 Issue
  接口；`frontend/` 才是正式的前端。`cmd/ora-web` 是早期的前端开发边缘服务，日常开发使用 Gateway。

这些入口都只用于开发，它们使用的 `router.NewDevelopment` 等开发构造函数不能出现在生产进程里。

## 前端

正式前端在 [`frontend/`](../../frontend/README.md)：React 19、TypeScript、Vite、Tailwind CSS 4 与 TanStack Query，API 层由 orval 从
`api/openapi.json` 生成。它有自己的 `AGENTS.md`，修改前端前先读它。

```sh
task frontend:install   # npm ci
task frontend:dev       # Vite :5173
task frontend:check     # 与 CI 相同的前端门禁
```

尚未接入后端的页面由 MSW 的 `/mock-api/*` 提供演示数据。

## 下一步

- 运行测试与门禁：[测试与门禁](testing.md)
- 修改代码：[修改与扩展](changing-the-code.md)
