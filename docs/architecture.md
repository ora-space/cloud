# Ora Cloud 架构总览

[English](architecture.en.md) | 中文

Ora Cloud 是 Ora 多人协作的权威服务端：它保存租户、成员、Project、运行时 Workspace、Issue、Agent 运行、插件和工作流的全部业务状态，
决定谁能做什么、下一步该做什么；真正的执行发生在沙盒里的 Node 上，由 Controller 在 Cloud 与 Node 之间传递。本文给出系统的全貌，每个部分
的细节在对应的文档里展开（见 [文档地图](README.md)）。

## 参与方

```text
Browser
  │  HTTPS, session cookie
  ▼
Gateway ─────────── personal key writes ──────────▶ model-gateway
  │  /api/v1/* + service JWT + user JWT                  ▲
  ▼                                                      │ model requests (short-lived token)
Cloud server ◀──── mTLS gRPC, dialed out ──── Controller │
  │                                              │       │
  ▼                                              │ Substrate effects / Node protocol
PostgreSQL                                       ▼       │
                                   Sandbox Server ──▶ sandbox: Node + Agent
                                                         ├──▶ git-server (Project repository)
                                                         └──▶ object storage (Revision upload grant)
```

- 浏览器只和 Gateway 通信；Gateway 为每个请求现签两份内部 JWT，再转发给 Cloud server。
- Controller 只主动拨出到 Cloud 的 gRPC 控制监听，Cloud 从不连接 Controller；Controller 驱动 Sandbox Server 建删沙盒，并与沙盒里的 Node 建立会话。
- 沙盒只能到达 model-gateway、git-server 的仓库监听和对象存储的公开地址，不能直接访问 Cloud、Gateway 或数据库。

| 参与方 | 仓库 | 职责 |
|---|---|---|
| Cloud server（`cmd/server`） | 本仓库 | 业务权威：公开 API、SSE 失效事件、Controller 的 gRPC 契约、所有状态机和后台循环 |
| Gateway（`cmd/gateway`） | 本仓库 | 浏览器唯一的入口：外部登录（华为 IDaaS、GitHub）、会话、代理 |
| model-gateway（`cmd/model-gateway`） | 本仓库 | 个人模型 Key 的保管与模型请求转发 |
| git-server（`cmd/git-server`） | 本仓库 | 每个 Project 的代码中心，逐请求按 Cloud 状态授权 |
| PostgreSQL | — | 唯一的业务权威存储 |
| 对象存储 | — | Revision 的 bundle 与会话 JSONL |
| Controller（`ora-controller`） | desktop | 只拨出到 Cloud；领取工作，执行 Substrate effect，与 Node 建会话并转发事件与命令 |
| Sandbox Server | cluster | Substrate 的 Docker 实现：按 Workspace 创建、终止沙盒，删除数据 |
| Node（`ora-node`） | desktop | 沙盒里的执行端：clone、插件安装、Agent 会话、Revision 交付 |

## 贯穿始终的原则

1. **PostgreSQL 是唯一的权威。** 其他进程都不持有另一份业务状态。所有决定在 `internal/core` 的短事务里做出，事务用一把全局 advisory lock
   串行（见 [权威持久化与事务模型](core/persistence.md)）。
2. **事务里只做数据库工作。** HTTP、Git、对象存储、Node、Substrate 的调用都在事务之外；需要时拆成"事务 → 外部调用 → 事务"，后一个事务重新
   检查前提。
3. **先持久化意图，再产生副作用。** 每个外部动作先在 Cloud 登记成持久的操作、effect 或执行，带稳定的幂等身份，Controller 执行后报回证据，
   Cloud 接管证据时才推进状态。结果不明时按稳定身份对账，从不凭超时推断"外部没有执行"。
4. **用代次隔离迟到者。** 资源版本、Controller 租约的 epoch、运行时代次、Node 实例、运行时控制代次分别记录；每次写入都校验对应的代次，旧的
   参与者即使还活着，它的写入也会被拒绝。
5. **提示不是状态。** SSE 事件和 Controller 信号只在提交之后发出、至多一次、不持久化；客户端靠重新读取 REST、Controller 靠周期领取保证不丢任何东西。
6. **身份只证明"谁"，授权看当前状态。** 内部 JWT、mTLS 证书只说明调用方是谁；能做什么由 core 按当前的成员、角色、归属和运行状态判定。
7. **契约只有一个来源。** HTTP 契约是 `router.Routes` 与 `internal/contract` 生成的 `api/openapi.json`，Controller 契约是 `proto/`；前端客户端和
   desktop 的客户端都由它们生成。

## 一个请求的路径

1. 浏览器带着会话 Cookie 请求 Gateway 的 `/api/v1/*`。Gateway 校验同源与会话，删掉浏览器带来的凭据头，现签一份 service JWT 和一份 user JWT，
   转发到固定的 Cloud upstream（见 [Gateway](identity/gateway.md)）。
2. Cloud 的路由白名单匹配路由，校验两份凭据，严格解码请求体（见 [HTTP API 约定](core/http-api.md)）。
3. `Store.Public` 开启事务：取全局锁，把 user JWT 解析成用户（必要时即时创建），校验租户成员与角色，处理幂等重放，执行业务逻辑。
4. 事务提交后，发出收集到的 Controller 信号和 SSE 失效事件，返回响应。

## 一个运行时的生命周期

用户创建 Project 时，Cloud 在同一事务里建 Project、它的仓库 Binding、main Workspace 和一个 `create_project` 操作，返回 `202`。之后：

```text
Controller 领取操作 ──▶ 计划 sandbox_ensure effect ──▶ Sandbox Server 建沙盒
      ──▶ 与 Node 握手并代报注册 ──▶ 登记并派发 clone 执行 ──▶ Node clone
      ──▶ 登记并派发插件执行 ──▶ Cloud 在同一事务里打开准入、标记 Workspace 就绪、操作成功
```

每一步都要求 Cloud 已经接管了上一步的证据；停止、重启、删除要先证明 Node 空闲，再确认旧沙盒真实终止（见 [Workspace 生命周期](runtime/workspaces.md)）。
同一个 Project 同时只有一个生命周期操作。谁能使用这个运行时、页面如何独占操作、管理员如何强停，见 [运行时控制](runtime/runtime-control.md)。

## 一次 Agent 运行

用户在 Issue 评论里 @ 一个 Agent 并给出任务：

```text
评论事务：创建 IssueRun（固定 Agent 插件版本、个人模型绑定）
  ──▶ 派发循环：建运行 Workspace（一次性的 isolated Workspace）
  ──▶ 就绪后：写 Thread 首条提问，放出会话工作项 ──▶ Node 启动 Agent 会话
  ──▶ Thread：会话记录逐条流回 Cloud，用户可以继续追加消息
  ──▶ 会话结束：放出交付工作项 ──▶ Node 上传 bundle 与会话 JSONL
  ──▶ Cloud 在事务外核对对象，事务内登记 Revision ──▶ 删除运行 Workspace
```

执行结果与成果保存分开记录，启动、交付、删除各有等待上限，未知结果只请求结束、保留残留责任（见 [Agent IssueRun](agent-runs/issue-runs.md)）。

## 领域一览

| 领域 | 核心对象 | 文档 |
|---|---|---|
| 身份与租户 | 用户、身份映射、租户、Space、成员 | [内部凭据](identity/authentication.md)、[Gateway](identity/gateway.md)、[用户、租户与成员](identity/tenancy.md) |
| 运行时 | Project、Workspace、操作、effect、沙盒、Node、运行时控制、强停 | [Workspace 生命周期](runtime/workspaces.md)、[运行时控制](runtime/runtime-control.md)、[Controller 契约](runtime/controller-integration.md) |
| 仓库 | 仓库 Binding、仓库 Operation、Project 仓库 | [Git Server](repositories/git-server.md) |
| 协作 | Issue、状态列、评论、活动、交互 | [Issue 看板](collaboration/issues.md)、[Issue 协作](collaboration/interactions.md) |
| Agent 运行 | IssueRun、运行 Workspace、Thread、Revision、残留账本 | [Agent IssueRun](agent-runs/issue-runs.md) 及同目录各篇 |
| 模型 | 个人模型连接、运行绑定、临时授权、请求台账 | [个人模型连接](models/model-access.md)、[model-gateway](models/model-gateway.md) |
| 插件 | 市场目录、空间插件选择、运行时实例、Agent 名册 | [插件市场](plugins/marketplace.md) |
| 工作流 | 工作流文档、已发布版本、WorkflowRun、资产导入 | [工作流](workflows/workflows.md) |

## 后台循环

Cloud server 进程里运行着一组周期循环，处理没有外部事件可以跟随的推进与收尾：

| 循环 | 周期 | 文档 |
|---|---|---|
| 插件市场目录同步 | `plugins.sync_interval`（默认 5 分钟） | [插件市场](plugins/marketplace.md#目录同步) |
| 工作流插件资产导入 | `plugins.asset_import_interval`（默认 30 秒） | [工作流](workflows/workflows.md#插件资产导入) |
| Agent 运行的派发、会话启动、Thread 结束、取消、交付放弃、删除重声明、删除放弃 | 10 秒 | [Agent IssueRun](agent-runs/issue-runs.md#后台循环) |
| WorkflowRun 调度 | 10 秒 | [工作流](workflows/workflows.md#调度) |

它们都在全局锁之外做有界的只读扫描，再为每个对象开一个短事务重新判断并以比较并交换的方式转换，重复执行不会产生第二次效果。

## 权威决策在 specs

本仓库的文档描述当前代码的行为和约定；业务规则、取舍和被否定的方案记录在
[specs 的 Cloud 决策目录](https://github.com/ora-space/specs/tree/main/decisions/cloud)（ADR）与对应的核心用例里。文档与 ADR 冲突时，先确认
ADR 的状态：`approved` 表示已批准但不一定已实现，`implemented` 表示实现与它一致。
