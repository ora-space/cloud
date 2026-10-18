# gitserver：Project 仓库的 Git 访问

[中文](README.md) | [English](README.en.md)

只以 smart HTTP 提供 Project 仓库（`/{tenantId}/{projectId}.git`），见根决策 `cloud/git-server/0` 与
`proposed` 的后续决策“Git Server 成为每个 Project 的代码中心”。每项权限都由 `internal/core` 与 PostgreSQL 判定，
本包不保存授权状态。

| 文件 | 职责 |
|---|---|
| `config.go` | 部署引用与监听 TLS：仓库监听只信沙盒 CA；服务仓库监听只信 Repo Executor CA；ref 监听固定 Cloud 的叶证书 |
| `scope.go` | `RuntimeScope` 只接受 `git-access` 用途的 SPIFFE 身份；`RequireExecutor` 只接受 `spiffe://ora.local/repo-executor` |
| `repository.go` | smart HTTP 的四个端点、规范 ID、首次获授权写入时建仓（先暂存再 rename）、按精确名称或前缀读取 ref |
| `service.go` | 两个仓库监听的逐请求授权（各自只认一个上下文头）、按监听注入仓库配置、经 CGI 调用 `git http-backend`、ref 查询与健康检查 |
| `rules.go` | 按 namespace 执行写入规则：create-only、fast-forward、tracking-cas |
| `hook.go` | 一次性推送令牌、私有 Unix socket hook 通道与 `RunPreReceive` hook 主体 |

两个仓库监听：

- **仓库监听**（沙盒网络）：沙盒 git-access 证书，请求必须且只能带 `Ora-Execution-Id`；
- **服务仓库监听**（内部服务网络）：Repo Executor 服务证书，请求必须且只能带 `Ora-Git-Operation-Id`。

带错头、缺头或两个都带都返回 `400`。Core 把请求解析成一份 `GitGrant`：可读整个 Project 仓库，以及可写的 ref
或前缀，每项带上所在 namespace 固定的规则。读侧不隐藏 ref。任何拒绝都是同一个 `403`，仓库不存在也一样；
数据库故障返回 `503`，绝不放行。`pre-receive` 在写入任何 ref 之前重新解析同一份授权，逐个 ref 核对：

| namespace | 规则 |
|---|---|
| `refs/ora/upstreams/<bindingId>/snapshots/<oid>` | create-only，名称中的 OID 必须等于目标 |
| `refs/ora/upstreams/<bindingId>/heads/<branch>` | tracking-cas：旧值等于授权记录的 expected old；新值必须已有、或在同一次推送中新建对应快照；允许非快进 |
| `refs/heads/workspaces/<workspaceId>/<branch>` | fast-forward：新建或快进（hook 在隔离区内判定祖先关系） |
| `refs/ora/deliveries/<deliveryWorkId>` | create-only |

任何 ref 都不能删除，也不能写表外的 ref；只要有一个更新不被规则接受，整次推送拒绝。目前沙盒执行只产生交付规则，
同步 Operation 只产生本 Binding 的快照前缀；工作区分支与跟踪 ref 的规则来源由内部线、外部线各自添加。

仓库策略与 hook 路径通过 `GIT_CONFIG_*` 环境变量作用于每个 git 进程，而不写进仓库，因此重启（hook 目录变化）
或旧仓库都无法削弱它们。仓库监听保持 `receive.denyNonFastForwards=true`；服务仓库监听关闭它，使跟踪 ref 能跟随外部
force-push，其余 namespace 的非快进仍由 hook 拒绝。两个监听都保持 `receive.denyDeletes=true`。对象传输与 hook 都不持有
数据库事务。

ref 查询只对 Cloud 的固定身份开放，只读：`?ref=` 查询任意 namespace 中一个 ref 指向的提交；`?prefix=` 列出一个
namespace 实例内（交付、一个 Binding 的全部 / 快照 / 跟踪、一个 Workspace）的 ref 与提交。

测试用真实 git 客户端、`git http-backend` 与 TLS，配合策略替身覆盖规则与监听的边界矩阵；
`integration/git_server_test.go` 与 `integration/git_operation_access_test.go` 用真实 Cloud 状态和发布的命令作为 hook
跑通交付与执行器同步。长时间的 fetch 不在传输中途重新授权；写入在 pre-receive 中重新授权。
