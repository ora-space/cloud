# cmd: 命令入口

[中文](README.md) | [English](README.en.md)

`cmd` 包含 Ora Cloud 的命令行工具和守护进程入口。该目录下的包严格限于加载运行时配置、注入依赖、编排进程生命周期、处理操作系统信号和设置进程退出码。

## 模块概览

- [server](server/README.md)：核心的权威 HTTP API 服务守护进程。
- [gateway](gateway/README.md)：浏览器可访问的认证与反向代理边界，装配 IDaaS/GitHub 与开发 provider、PostgreSQL 会话与内部凭据签发。
- [cloudctl](cloudctl/README.md)：受限的运维与部署管理 CLI，用于数据库迁移与租户初始化引导。
- [devsetup](devsetup/README.md)：仅限本地开发的一键配置（`task setup`），按 `config.toml` 生成 Gateway 私钥、Cloud 信任公钥与 PKCE 密钥，写出选定外部 provider 的 client secret 文件与 `.local/dev.env`，并应用迁移。
- [git-server](git-server/README.md)：以 smart HTTP 向沙盒和 Repo Executor 提供 Project 仓库，每个请求按 Cloud 当前状态授权，并为 Cloud 提供只读的 ref 查询。
- [git-server-preset](git-server-preset/README.md)：仅用于验收的 Git Server 契约预置与断言工具，只随开发镜像发布。
- [repo-executor](repo-executor/README.md)：唯一访问外部 Git 仓库的进程，执行同步、显式发布与只读探测；无状态、不连数据库，只接受 Cloud 的 mTLS 任务。
- [git-credential-ora](git-credential-ora/README.md)：Repo Executor 的固定 Git credential helper，只在请求的 https 仓库等于暂存作用域时交付凭据。
- [credential-store](credential-store/README.md)：独立的 Credential Store 进程：兑换、管理、密钥三个 mTLS 监听与浏览器入口，keyring 缺失时拒绝启动，定期清扫过期版本。
- [simulator](simulator/README.md)：基于进程内 Substrate 和 Git 执行替身提供一体化的本地演示环境。
- [openapi](openapi/README.md)：根据 Go 契约定义生成并同步权威 OpenAPI 3.0 规范（`api/openapi.json`）。
- [checkformat](checkformat/README.md)：作为严格的 CI 校验门禁，强制执行仓库 Go 代码格式化规范。
- [model-gateway](model-gateway/README.md)：个人密钥保管与授权模型代理服务。
- [demo-issue-board](demo-issue-board/README.md)：在独立 PostgreSQL schema 中打印单次看板演示。
- [demo-issue-board-web](demo-issue-board-web/README.md)：提供嵌入的浏览器看板和临时演示数据。
- [ora-web](ora-web/README.md)：使用持久本地 schema 的前端开发入口。

## 装配边界

服务命令加载配置、装配依赖并拥有进程停机，领域策略归 [core](../internal/core/README.md)。运维工具和开发演示的数据库访问与迁移行为各有边界，见对应模块 README。参见 [架构总览](../docs/architecture.md)、[部署](../docs/operations/deployment.md) 和 [AGENTS.md](../AGENTS.md)。
