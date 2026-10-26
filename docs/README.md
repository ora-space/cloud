# Ora Cloud 文档

[English](README.en.md) | 中文

这里是 Ora Cloud 跨模块的架构、契约与运维文档。每个包内部的实现细节在所在目录的 `README.md` 里；业务决策的权威来源是
[specs](https://github.com/ora-space/specs/tree/main/decisions/cloud) 的 ADR；接口字段以 [`api/openapi.json`](../api/openapi.json) 为准。

## 从哪里开始

| 我想…… | 先读 |
|---|---|
| 了解系统全貌 | [架构总览](architecture.md) |
| 在本机跑起来 | [本地开发](development/local-development.md) |
| 修改代码 | [修改与扩展](development/changing-the-code.md)，再读要改的领域对应的文档 |
| 部署或运维 | [部署与运维](operations/deployment.md)、[配置参考](operations/configuration.md) |

## 全部文档

**核心**

- [权威持久化与事务模型](core/persistence.md)：Store、全局锁、错误、提交后发布、业务钩子、一致性手段、迁移
- [HTTP API 约定](core/http-api.md)：路由白名单、凭据、请求解码、错误、分页、SSE、契约生成链

**身份与租户**

- [内部凭据与服务信任](identity/authentication.md)：双 JWT、Controller 的 mTLS 身份、从凭据到用户
- [Gateway](identity/gateway.md)：外部登录、浏览器会话、代理、登录 provider
- [用户、租户与成员](identity/tenancy.md)：租户与 Space、两种加入方式、成员管理

**运行时**

- [Project、Workspace 与生命周期操作](runtime/workspaces.md)：聚合、持久操作与步骤、effect
- [运行时使用权限、独占控制与强停](runtime/runtime-control.md)
- [Controller 内部控制契约](runtime/controller-integration.md)：租约、提交身份、信号流、各个服务

**仓库**

- [Git Server 与 Project 仓库](repositories/git-server.md)：仓库 Binding 与 Operation、监听、授权、ref 规则
- [外部 Git 线](repositories/external-git.md)：Repo Executor、Credential Store、同步与显式发布、凭据生命周期、授权与失败收敛

**协作**

- [Issue 看板](collaboration/issues.md)
- [Issue 协作：@ 目标、交互与执行意图](collaboration/interactions.md)

**Agent 运行**

- [Agent IssueRun：一次 Agent 运行的全过程](agent-runs/issue-runs.md)
- [Thread：Agent 运行的对话](agent-runs/threads.md)
- [有界启动：等待上限与未知结果的收尾](agent-runs/startup-settlement.md)
- [Agent Run 残留回收：Cloud 责任与证据](agent-runs/residual-reclamation.md)
- [Revision：交付、校验登记与续接](agent-runs/revisions.md)
- [会话读取：会话列表、归档与模型请求统计](agent-runs/session-reads.md)

**模型**

- [个人模型连接、运行绑定与临时授权](models/model-access.md)
- [model-gateway：个人密钥与模型代理服务](models/model-gateway.md)

**插件与工作流**

- [插件市场与空间插件](plugins/marketplace.md)
- [工作流](workflows/workflows.md)

**运维**

- [配置参考](operations/configuration.md)
- [部署与运维](operations/deployment.md)
- [部署方共享出网配置](operations/deployment-egress.md)

**开发**

- [本地开发](development/local-development.md)
- [测试与门禁](development/testing.md)
- [修改与扩展](development/changing-the-code.md)

## 维护规则

- `docs/` 只放随代码长期维护的文档。计划、进度、实现日志、调研笔记和验收记录不进入仓库，写在跟踪这项工作的 issue 里。
- 每篇文档都有中文（`x.md`）和英文（`x.en.md`）两个版本，标题下互相链接；修改时两个版本在同一个改动里一起更新。
- 文档描述当前代码能核验的事实，链接相应的 ADR 而不复述决策过程；`proposed` 或 `approved` 但尚未实现的决策不写成已实现。
