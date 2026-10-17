# internal: 权威云端子系统

[中文](README.md) | [English](README.en.md)

`internal` 存放 Ora Cloud 的私有内部实现包。遵循 `AGENTS.md` 中记录的架构边界规范，所有核心业务状态、策略逻辑、协议转换与基础设施适配器均保持在 `internal/` 作用域内私有化。

## 模块概览

- [core](core/README.md)：权威领域核心，拥有业务状态机、事务边界、数据库级咨询锁以及密码学身份认证。
  - [migrations](core/migrations/README.md)：包含按顺序执行、仅向前的 PostgreSQL Schema 迁移脚本和校验和验证。
- [api](api/README.md)：HTTP 表现层与协议接入层。
  - [router](api/router/README.md)：绑定 HTTP 路由、验证两层 JWT 凭据、在大小限制下解析 JSON 请求体，并将领域错误转换为稳定契约。
- [gateway](gateway/README.md)：公开认证边界：Login Attempt 与 Browser Session 的 PostgreSQL 存储、provider-neutral 登录编排、内部 JWT 签发、Cookie/CSRF/redirect 防护与 `/api/v1` 代理。
  - [idaas](gateway/idaas/README.md)：华为 IDaaS 2.0（`client_secret_post`）适配器，产生 `source=huawei-corp` 的 `VerifiedIdentity`。
  - [github](gateway/github/README.md)：GitHub OAuth App 适配器，产生 `VerifiedIdentity`。
  - [devlogin](gateway/devlogin/README.md)：仅限本地开发的 provider：一个输入任意身份即可登录的本地表单，只在 loopback 开发 origin 上注册。
- [contract](contract/README.md)：定义 OpenAPI 3.0 Schema 模型、DTO 结构体与契约覆盖率测试。
- [repository](repository/README.md)：基于 GORM 管理 PostgreSQL 连接池与启动时快速探活。
- [objectstore](objectstore/README.md)：Cloud 自己的 S3 兼容对象存储客户端（SigV4 预签上传授权、HEAD 校验对象的大小与 SHA-256），只服务 Revision 链路。
- [gitpreset](gitpreset/README.md)：仅用于验收的 Git Server 契约预置与断言，直接写仓库卷与数据库，不被服务进程引用。
- [gitserver](gitserver/README.md)：Project 仓库的 Git 访问：沙盒与 Repo Executor 两个仓库监听、按 Core 逐请求授权的 ref 规则集、`git http-backend`、pre-receive 按 namespace 执行规则与 Cloud 的 ref 查询。
- [cloneauth](cloneauth/README.md)：外部 Git 线的一次性授权：`sync`/`publish`/`probe` 执行授权与浏览器 `submit` 提交授权的 Ed25519 签发与严格验证、`NormalizeRepository`、密钥加载。
- [credentialmaterial](credentialmaterial/README.md)：Repo Executor 一次任务的凭据材料目录（0700 目录、0600 独占文件、fsync、确认删除、启动清扫），与 `git-credential-ora` 共用的布局。
- [processgroup](processgroup/README.md)：让 Git 子进程领头新进程组，取消时 `SIGKILL` 整组并设 `WaitDelay`。
- [capture](capture/README.md)：子进程输出的有界缓冲：保留前 N 字节、计数其余、写入永不失败。
- [credentialstore](credentialstore/README.md)：独立的 Credential Store：KEK keyring 与信封加密、原子落盘的凭据卷、一次性兑换与提交认领、1 小时版本保留与撤销销毁密文；兑换、管理、密钥、浏览器入口四个监听，以及 Cloud 与执行器用的客户端。
- [config](config/README.md)：加载并校验应用程序配置文件及环境变量覆盖。
- [logger](logger/README.md)：基于 Zap 和 Lumberjack 提供结构化、非阻塞的 JSON 日志记录。
- [simulator](simulator/README.md)：实现 Substrate 执行引擎、Controller 与 Workspace Node 的进程内替身。
- [controlpb](controlpb/README.md)：由 [`proto/`](../proto/README.md) 生成的 Controller 内部控制契约 gRPC Go 代码（服务端桩与消息），只读。
- [controlgrpc](controlgrpc/README.md)：该契约的 gRPC 服务端：调用方身份拦截器、`Fault` → 状态码映射、租约服务；只翻译，不含业务。

## 分层与架构规则

1. **严格单向依赖**：
   - `cmd/*` $\rightarrow$ `internal/api/router`, `internal/gateway`, `internal/core`, `internal/config`, `internal/logger`, `internal/repository`, `internal/objectstore`。
   - `internal/gateway` $\rightarrow$ `internal/core`（仅 `Claims` 类型）、`internal/config`、`internal/logger`；`internal/gateway/idaas`、`internal/gateway/github` 与 `internal/gateway/devlogin` $\rightarrow$ `internal/gateway`。Gateway 不查询 Cloud 业务表。
   - `internal/api/router` $\rightarrow$ `internal/core`, `internal/contract`；`internal/controlgrpc` $\rightarrow$ `internal/core`, `internal/controlpb`。
   - `internal/core` $\rightarrow$ 标准库、`gorm.io/gorm`、`internal/core/migrations`。
   - `internal/repository` $\rightarrow$ `internal/config`, `gorm.io/gorm`。
   - `internal/gitpreset` $\rightarrow$ `internal/core`（ref 词汇）、`internal/gitserver`（建仓）；只有 `cmd/git-server-preset` 导入它。
   - `internal/credentialmaterial` $\rightarrow$ `internal/cloneauth`（仅 `NormalizeRepository`）；`cmd/git-credential-ora` 只导入这两个包。`cloneauth`、`capture`、`processgroup` 只依赖标准库（`cloneauth` 另用 `github.com/google/uuid`）。
   - `internal/credentialstore` $\rightarrow$ `internal/cloneauth`；只有 `cmd/credential-store`、Cloud（`ManagementClient`）与执行器（`RedemptionClient`）导入它。
   - `internal/objectstore` $\rightarrow$ `internal/core`（仅 `UploadGrant` 值类型）；`internal/core` 通过自己声明的 `RevisionObjects` 接口调用它，**不**反向导入本包。
   - 底层包（`core`、`repository`、`objectstore`）严禁反向导入上层表现层包（`api`、`router`）。
2. **PostgreSQL 为唯一权威持久化**：
   - 所有共享业务状态必须持久化在 PostgreSQL 中。严禁跨请求在内存中缓存权威领域状态。
3. **事务边界约束**：
   - 数据库事务与咨询锁严格限制在 `core.Store.transact` 内的 PostgreSQL 操作中。**绝对禁止**跨外部 HTTP 请求、Git CLI 命令或文件系统 I/O 持有事务。
4. **错误处理**：
   - 面向客户端的公开错误统一使用稳定的 `Fault` 结构体（包含 `Code`、`Params`、`Status`）。内部 SQL 细节、错误堆栈与数据库异常仅记录于内部日志中，绝不对外暴露。

参见 [AGENTS.md](../AGENTS.md)、[核心不变量与契约](../docs/core-contract.md) 与 [认证配置与凭据](../docs/authentication.md)。
