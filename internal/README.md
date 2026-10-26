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
- [contract](contract/README.md)：定义 OpenAPI 3.0 Schema 模型与契约覆盖率测试。
- [repository](repository/README.md)：基于 GORM 管理 PostgreSQL 连接池与启动时快速探活。
- [objectstore](objectstore/README.md)：Cloud 自己的 S3 兼容对象存储客户端（SigV4 预签上传授权、HEAD 校验对象的大小与 SHA-256），并支持归档与工作流资产读取。
- [gitpreset](gitpreset/README.md)：仅用于验收的 Git Server 契约预置与断言，直接写仓库卷与数据库，不被服务进程引用。
- [gitserver](gitserver/README.md)：Project 仓库的 Git 访问：沙盒与 Repo Executor 两个仓库监听、按 Core 逐请求授权的 ref 规则集、`git http-backend`、pre-receive 按 namespace 执行规则与 Cloud 的 ref 查询。
- [gitrefquery](gitrefquery/README.md)：Cloud 访问 Git Server 只读 ref 查询（`GET /internal/v1/refs`）的客户端，在事务外核对要登记的 ref 存在且指向声明的提交。
- [repoexec](repoexec/README.md)：Ora 中唯一访问外部 Git 仓库的程序：按一次任务新建裸仓库、执行同步/发布/探测，只报告观察到的事实，Operation 状态由 Cloud 决定。
- [gitrefs](gitrefs/README.md)：Git Server 只读 ref 查询的客户端（契约 D6）：TLS 1.3 双向认证、有界应答，把「服务器不可用」与「ref 不存在」分开。
- [repoworker](repoworker/README.md)：驱动仓库 Operation 的工作进程：读跟踪 ref、签发一次性授权、续租与取消、按租约世代收敛同步与发布结果。
- [cloneauth](cloneauth/README.md)：外部 Git 线的一次性授权：`sync`/`publish`/`probe` 执行授权与浏览器 `submit` 提交授权的 Ed25519 签发与严格验证、`NormalizeRepository`、密钥加载。
- [credentialmaterial](credentialmaterial/README.md)：Repo Executor 一次任务的凭据材料目录（0700 目录、0600 独占文件、fsync、确认删除、启动清扫），与 `git-credential-ora` 共用的布局。
- [processgroup](processgroup/README.md)：让 Git 子进程领头新进程组，取消时 `SIGKILL` 整组并设 `WaitDelay`。
- [capture](capture/README.md)：子进程输出的有界缓冲：保留前 N 字节、计数其余、写入永不失败。
- [credentialstore](credentialstore/README.md)：独立的 Credential Store：KEK keyring 与信封加密、原子落盘的凭据卷、一次性兑换与提交认领、1 小时版本保留与撤销销毁密文；兑换、管理、密钥、浏览器入口四个监听，以及 Cloud 与执行器用的客户端。
- [config](config/README.md)：加载并校验应用程序配置文件及环境变量覆盖。
- [logger](logger/README.md)：基于 Zap 和 Lumberjack 提供结构化 JSON 日志记录。
- [simulator](simulator/README.md)：实现 Substrate 执行引擎、Controller 与 Workspace Node 的进程内替身。
- [controlpb](controlpb/README.md)：由 [`proto/`](../proto/README.md) 生成的 Controller 内部控制契约 gRPC Go 代码（服务端桩与消息），只读。
- [controlgrpc](controlgrpc/README.md)：该契约的 gRPC 服务端：调用方身份拦截器、`Fault` → 状态码映射、租约服务；只翻译，不含业务。
- [collab](collab/README.md)：显式启用的开发协作夹具。
- [workflowasset](workflowasset/README.md)：核验插件包并读取工作流文档，不访问数据库。
- [egress](egress/README.md)：共享部署出网配置、目标校验、DNS 与拨号。
- [modelgateway](modelgateway/README.md)：个人密钥加密与模型请求转发。

## 架构入口

依赖方向、职责归属与事务规则见 [架构总览](../docs/architecture.md)、[持久化](../docs/core/persistence.md) 和 [AGENTS.md](../AGENTS.md)。包内 README 说明本地实现；跨模块契约见 [文档地图](../docs/README.md)。
