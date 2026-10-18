# Git Server 与 Project 仓库

[English](git-server.en.md) | 中文

Git Server（`cmd/git-server`，实现在 `internal/gitserver`）为每个 Project 托管一个 bare 仓库，作为这个 Project 的代码
中心：沙盒里的 Agent 执行通过它读取和提交代码，Repo Executor 通过它把外部仓库的快照带进来。它是一个独立进程，与 Cloud
使用同一个镜像，自己不保存任何授权状态：每个请求能做什么，都由 `internal/core` 根据 PostgreSQL 的当前状态现场判定。

本文先说明 Cloud 一侧记录的仓库 Binding 与仓库 Operation，再说明 Git Server 的监听、身份和授权规则。

## Cloud 一侧：仓库 Binding 与仓库 Operation

- **Binding（`repository_bindings`）。** 每个 Project 恰有一个，记录外部仓库的地址、要同步的分支和可选的凭据引用；Project
  文档里的 `repositoryUrl`、`defaultBranch` 就是从这里读出的。
- **仓库 Operation（`repository_operations`）。** 两种类型：`repository_sync` 把外部仓库的内容同步进 Project 仓库，
  `repository_publish` 把 Project 仓库的内容发布回外部。状态和终态由 `CHECK` 约束固定，每个 Binding 同时至多一个未结束的
  同步。创建 Project 的事务同时建立 Binding 和一个 `pending`、触发原因为 `project_created` 的同步。
- **与 Project 删除的关系。** Project 进入 `deleting` 的事务取消所有还没有开始运行的 Operation，清除补同步请求；正在运行
  或结果待确认（`uncertain`）的 Operation 照常收尾，`delete_project` 等它们全部结束才开始删除。删除因 Node 拒绝停止而中止、
  Project 回到 `active` 时，同一事务把被取消的同步补回来；被取消的发布不恢复，需要用户重新发起。

执行仓库 Operation 的 Repo Executor 不在本仓库里。Cloud 负责记录 Operation、按 Operation 状态授权 Git 访问；目前 Workspace
的 `clone` 步骤仍然直接从外部仓库地址克隆。

## 监听

| 监听 | 默认地址 | 网络 | 客户端 |
|---|---|---|---|
| 仓库监听 | `:8445` | 只接入沙盒网络 | 沙盒里的执行，出示 git-access 客户端证书 |
| 服务仓库监听 | `:8447` | 只接入内部服务网络 | Repo Executor，出示服务证书 |
| ref 查询监听 | `:8446` | 默认网络 | 只有 Cloud 的固定客户端证书，另提供 `/healthz` |

三个监听各自只信任一个 CA：仓库监听只信沙盒 CA，服务仓库监听只信 Repo Executor CA，ref 查询监听固定接受 Cloud 的那一张
叶证书。证书只证明"调用方是谁"，不携带任何仓库权限。

## 请求与授权

仓库通过 smart HTTP 提供，路径是 `/{tenantId}/{projectId}.git`。每个请求必须**恰好**带一个上下文头，说明它服务于哪一项工作：

| 监听 | 必须带的头 | 证书身份 |
|---|---|---|
| 仓库监听 | `Ora-Execution-Id: <执行 ID>` | `spiffe://ora.local/git-access/tenant/<tenantId>/runtime/<workspaceId>/generation/<n>`，URI 必须唯一且用途段为 `git-access` |
| 服务仓库监听 | `Ora-Git-Operation-Id: <Operation ID>` | `spiffe://ora.local/repo-executor` |

缺头、带错头、两个都带都返回 `400`。core 把请求解析成一份授权：可以读取整个 Project 仓库，以及可以写入的 ref 或前缀，每一项
带上所在 namespace 固定的规则。仓库就是隔离边界，读取时不隐藏仓库内的任何 ref。

**沙盒执行的授权。** 执行必须属于这个运行时、未结束、未被强停，运行时处于当前代次且就绪，Project 处于 `active`；触发运行的
用户仍然能使用这个运行时；当前 Node 就是执行的目标 Node，运行时控制处于该运行的 `maintenance` 且已确认绑定到这个沙盒。在此
基础上：

| 执行 | 运行阶段 | 授权 |
|---|---|---|
| `deliver_revision` | `delivering` | 只能新建 `refs/ora/deliveries/<交付工作项 ID>` |
| `agent_session` | `starting` 或 `running`，未请求取消，Thread 未在结束 | 只读，而且只给需要恢复前序 Revision 的会话；其他会话从外部仓库克隆，没有理由访问 Git Server |

**Repo Executor 的授权。** 请求指定的 Operation 必须在这个租户和 Project 下、属于该 Project 的 Binding、处于 `running`，Project
未删除（`provisioning`、`active`、`deleting` 都可以，因为首次同步发生在创建中，删除开始时正在运行的同步也要能收尾）：

| Operation | 授权 |
|---|---|
| `repository_sync` | 在本 Binding 的快照前缀下新建快照 ref |
| `repository_publish` | 只读，发布只需要从 Project 仓库读取源 |

**结果。** 任何拒绝都是同一个 `403`，仓库不存在也一样，沙盒无法借此探测仓库、执行或 Node 是否存在。数据库故障返回 `503`，
绝不放行。授权在只读的一致性快照里完成，不取 core 的全局锁，Git 流量不与控制面写入串行。

结束执行、替换沙盒、取消运行、停用触发者账号、取消或结束 Operation，都会在**下一个请求**生效，包括已经进入 pre-receive 的推送。
长时间的 fetch 不在传输中途重新授权。

## ref namespace 与写入规则

| namespace | 规则 |
|---|---|
| `refs/ora/upstreams/<bindingId>/snapshots/<oid>` | create-only，名称里的 OID 必须等于目标提交 |
| `refs/ora/upstreams/<bindingId>/heads/<branch>` | tracking-cas：旧值必须等于授权记录的预期旧值；新值必须已有、或在同一次推送中新建对应的快照；允许非快进 |
| `refs/heads/workspaces/<workspaceId>/<branch>` | fast-forward：新建或快进 |
| `refs/ora/deliveries/<deliveryWorkId>` | create-only |

- `pre-receive` 在写入任何 ref 之前**重新**解析同一份授权，逐个 ref 核对规则。任何 ref 都不能删除，也不能写表外的 ref；只要有
  一个更新不被接受，整次推送拒绝。
- 目前沙盒执行只产生交付规则，同步 Operation 只产生快照前缀；工作区分支和跟踪 ref 的授权来源尚未接入。
- 仓库策略和 hook 路径通过每个 git 进程的 `GIT_CONFIG_*` 环境变量生效，不写进仓库，所以重启或旧仓库都无法削弱它们。两个仓库
  监听都禁止删除 ref；仓库监听同时禁止非快进，服务仓库监听放开非快进，让跟踪 ref 能跟随外部的 force-push，其他 namespace 的
  非快进仍由 hook 拒绝。
- 仓库在第一次获授权的写入时创建（先在暂存位置建好再重命名）。对象传输和 hook 都不持有数据库事务。

## ref 查询

ref 查询监听只对 Cloud 的固定身份开放，只读：

- `GET ...?ref=<ref>`：返回任意 namespace 里一个 ref 指向的提交；
- `GET ...?prefix=<前缀>`：列出一个 namespace 实例内的 ref 与提交（一个交付、一个 Binding 的全部、快照或跟踪 ref、一个 Workspace）。

把交付 ref 作为 Revision 登记依据的方案还在决策中（见下方 proposed 的 ADR）；当前的 Revision 仍按
[对象存储里的 bundle](../agent-runs/revisions.md) 校验登记，Cloud server 还没有调用 ref 查询。

## 运维

- bare 仓库存放在 `GIT_SERVER_REPOSITORY_ROOT` 指向的持久卷上；镜像需要带 `git http-backend` 的 `git`，缺少时启动失败。
- 启动先校验迁移；停止时先排空三个监听，再关闭 pre-receive hook 通道。
- 监听地址、证书和 CA 文件的配置见 [`cmd/git-server`](../../cmd/git-server/README.md) 与 [配置参考](../operations/configuration.md#git-server)。
- `cmd/git-server-preset` 是只用于验收的契约预置与断言工具，直接写仓库卷和数据库，只随开发镜像发布，不能用于生产。

## 相关决策

- [Git Server 是 Project 的代码中心：独立 HTTPS 服务，证书只认证身份，授权按 Cloud 当前状态逐请求判定](https://github.com/ora-space/specs/blob/main/decisions/cloud/git-server/0-project-code-hub-git-server-over-https.md)（approved）
- [Git Server 成为每个 Project 的代码中心：ref namespace、规则集授权与两类写入者的契约](https://github.com/ora-space/specs/blob/main/decisions/cloud/git-server/20261016-project-repository-as-code-hub-and-ref-contract.md)（approved）
- [Revision 的代码改动只经 Git Server 交付](https://github.com/ora-space/specs/blob/main/decisions/cloud/revision/20261012-git-server-delivery-and-verified-delivery-refs.md)（proposed）
