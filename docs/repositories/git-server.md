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

执行仓库 Operation 的 Repo Executor 是本仓库的 `cmd/repo-executor`，它按 Operation 状态获得 Git 访问授权；同步只写入本
Binding 的快照前缀和跟踪 ref，发布只读取源。整条线的同步、发布、凭据与收敛规则见
[外部 Git 线](external-git.md)。Workspace 的 `clone` 步骤只从 Binding 的当前快照 clone（见
[Workspace 生命周期](../runtime/workspaces.md#clone-步骤从-binding-快照-clone)），Node 不接触外部仓库，仓库凭据也不参与任何
Node 工作。

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

**Agent 会话与交付执行的授权。** 执行必须属于这个运行时、未结束、未被强停，运行时处于当前代次且就绪，Project 处于 `active`；触发运行的
用户仍然能使用这个运行时；当前 Node 就是执行的目标 Node，运行时控制处于该运行的 `maintenance` 且已确认绑定到这个沙盒。在此
基础上：

| 执行 | 运行阶段 | 可写入的 ref |
|---|---|---|
| `agent_session` | `starting` 或 `running`，未请求取消，Thread 未在结束 | `refs/heads/workspaces/<证书里的 workspaceId>/` 下的分支（快进规则）；前缀取自证书，从不取自请求 |
| `deliver_revision` | `delivering` | 新建输入固定的交付 ref `refs/ora/deliveries/<交付工作项 ID>`；另可把输入的 `workspaceRef` 新建或快进，前提是它属于证书里的 Workspace |

插件执行一律拒绝，不能访问仓库。WorkflowRun 会话按当前运行、图节点和尝试逐请求复核：仅运行中的当前尝试可读本 Project 仓库、快进本 Workspace 分支；结束请求、取消、换尝试或换代次立即撤销。整次交付使用独立资格，只新建自己固定的交付 ref，参见[工作流成果保存](../workflows/workflows.md#成果保存与历史归档)。

**clone 执行的授权。** Workspace 操作 `clone` 步骤的 clone 执行只读，并且只在操作仍处于 `running` 的 `clone` 步骤时有效：执行
没有结果、未被强停；Workspace 处于证书里的代次、未删除；Project 处于 `provisioning` 或 `active`（main Workspace 在 Project 创建中
clone）；执行输入的仓库地址指向这个租户和 Project；操作的发起者仍能使用这个运行时；没有强停；当前 Node 就是执行的目标 Node。它
不要求 Workspace 已就绪或已开放准入，因为 clone 发生在准入打开之前。操作离开 `clone` 步骤或执行有了结果，授权随即失效。

**Repo Executor 的授权。** 请求指定的 Operation 必须在这个租户和 Project 下、属于该 Project 的 Binding、处于 `running`，Project
未删除（`provisioning`、`active`、`deleting` 都可以，因为首次同步发生在创建中，删除开始时正在运行的同步也要能收尾）：

| Operation | 授权 |
|---|---|
| `repository_sync` | 在本 Binding 的快照前缀下新建快照 ref，并把本 Binding 的跟踪 ref 从授权记录的期望旧值推到新提交 |
| `repository_publish` | 只读，发布只需要从 Project 仓库读取源 |

**结果。** 任何拒绝都是同一个 `403`，仓库不存在也一样，沙盒无法借此探测仓库、执行或 Node 是否存在。数据库故障返回 `503`，
绝不放行。授权在只读的一致性快照里完成，不取 core 的全局锁，Git 流量不与控制面写入串行。

结束执行、clone 操作离开 `clone` 步骤、替换沙盒、取消运行、停用触发者账号、取消或结束 Operation，都会在**下一个请求**生效，包括已经进入 pre-receive 的推送。
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
- 沙盒执行产生 Workspace 分支与交付两种规则；外部线的同步 Operation 产生快照前缀和本 Binding 的跟踪 ref。
- 仓库策略和 hook 路径通过每个 git 进程的 `GIT_CONFIG_*` 环境变量生效，不写进仓库，所以重启或旧仓库都无法削弱它们。两个仓库
  监听都禁止删除 ref；仓库监听同时禁止非快进，服务仓库监听放开非快进，让跟踪 ref 能跟随外部的 force-push，其他 namespace 的
  非快进仍由 hook 拒绝。
- 仓库在第一次获授权的写入时创建（先在暂存位置建好再重命名）。对象传输和 hook 都不持有数据库事务。

## ref 查询

ref 查询监听只对 Cloud 的固定身份开放，只读：

- `GET ...?ref=<ref>`：返回任意 namespace 里一个 ref 指向的提交；
- `GET ...?prefix=<前缀>`：列出一个 namespace 实例内的 ref 与提交（一个交付、一个 Binding 的全部、快照或跟踪 ref、一个 Workspace）。

Cloud server 用 `git_server.*` 配置里的地址和自己的客户端证书调用 ref 查询（`internal/gitrefquery`，只用 `?ref=`），在任何数据库
事务之外核验 Revision 的交付 ref 指向 Node 声明的最终提交，再登记 Revision（见 [Revision](../agent-runs/revisions.md#交付流程)）。
只有 `404 ref_not_found` 算作 ref 确实不存在；其他响应、传输失败或超时都让核验暂不可用、之后重试，不作结论。查询不跟随重定向。
没有配置 Git Server 时，clone 步骤停在 `blocked`（`git_server_unconfigured`），交付以 `skipped` 结算，Cloud 也不经这个客户端调用 ref 查询。

这条交付路径实现的是已 `approved` 的 Revision 决策（见文末）。

外部 Git 线用 `repository.git_refs_*` 配置的另一个客户端（`internal/gitrefs`）在三个地方调用 ref 查询：同步报告 `applied`
后核对快照与跟踪 ref，创建发布前确认源 ref，以及列出 Workspace 的协作分支供选择发布源（见 [外部 Git 线](external-git.md)）。

## 运维

- bare 仓库存放在 `GIT_SERVER_REPOSITORY_ROOT` 指向的持久卷上；镜像需要带 `git http-backend` 的 `git`，缺少时启动失败。
- 启动先校验迁移；停止时先排空三个监听，再关闭 pre-receive hook 通道。
- 监听地址、证书和 CA 文件的配置见 [`cmd/git-server`](../../cmd/git-server/README.md) 与 [配置参考](../operations/configuration.md#git-server)。
- `cmd/git-server-preset` 是只用于验收的契约预置与断言工具，直接写仓库卷和数据库，只随开发镜像发布，不能用于生产。

## 相关决策

- [Git Server 是 Project 的代码中心：独立 HTTPS 服务，证书只认证身份，授权按 Cloud 当前状态逐请求判定](https://github.com/ora-space/specs/blob/main/decisions/cloud/git-server/0-project-code-hub-git-server-over-https.md)（approved）
- [Git Server 成为每个 Project 的代码中心：ref namespace、规则集授权与两类写入者的契约](https://github.com/ora-space/specs/blob/main/decisions/cloud/git-server/20261016-project-repository-as-code-hub-and-ref-contract.md)（approved）
- [Workspace 的 clone 步骤改为消费 Binding 当前快照](https://github.com/ora-space/specs/blob/main/decisions/cloud/operation/20261016-clone-step-consumes-binding-snapshot.md)（approved）
- [clone 输入替换为 Git Server 快照输入；Git Server 按执行类型给出写入规则](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20261016-snapshot-clone-input-and-execution-ref-grants.md)（approved）
- [Revision 的代码改动只经 Git Server 交付，Cloud 经 ref 查询核验后登记交付 ref](https://github.com/ora-space/specs/blob/main/decisions/cloud/revision/20261017-git-native-delivery-refs-and-resume.md)（approved；取代 20261012 的交付决策）
