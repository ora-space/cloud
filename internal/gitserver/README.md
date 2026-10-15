# gitserver：沙盒 Git 访问与镜像读取

[中文](README.md) | [English](README.en.md)

一个进程，两个面（proposed 的 ADR `cloud/git-server/0`）：

- **仓库面**只以 smart HTTP 向沙盒提供 Project 仓库（`/{tenantId}/{projectId}.git`），并向 Cloud 回答只读的交付
  ref 查询。调用方出示由沙盒 CA 签发的 mTLS 客户端证书，证书只声明运行时目标，**每次请求**再按 Cloud 当前状态判定；
  这是唯一能写的面。
- **镜像面**在一个独立监听上，把已发布的私仓镜像（`/mirror/v1/<digest>.git`）提供给 Workspace 的 Node。调用方在传输层
  不出示任何证书，身份来自连接到达的地址，按请求解析；这一面只读。

两面共享同一份进程、同一份依赖与同一条生命周期，但**不共享信任边界**：各自持有监听、TLS 配置与 handler，任一面的
handler 都到不了另一面的路由，因此一个面的可达地址不会决定另一个面的授权方式。Node 无法出示客户端证书
（`desktop/apps/ora-node` 的 clone 策略只允许 `[http] sslCAInfo` 一项，部署里也不存在它的客户端身份），把它并入仓库监听
会让读路径不可达；而放宽仓库监听的 `RequireAndVerifyClientCert` 会同时拿走该监听上每条路由的传输层保证。两面的
理由与拒绝顺序写在 `mirror.go` 的文件头。

每项权限都由 `internal/core` 与 PostgreSQL 判定，本包不保存授权状态。

| 文件 | 职责 |
|---|---|
| `service.go` | 两个面的组装：一次性解析唯一 git 程序、运行时目录与健康检查；缺任一面时拒绝启动，未装配的面一律 404 |
| `config.go` | 部署引用与监听 TLS：仓库监听要求沙盒 CA 签发的客户端证书；ref 监听固定 Cloud 的叶证书；只校验将要启动的面 |
| `scope.go` | `RuntimeScope`：只接受 `git-access` 用途的 SPIFFE 身份，同一 CA 签发的 WSS、model-access 证书都不接受 |
| `repository.go` | smart HTTP 的四个端点、规范 ID、首次获授权写入时建仓（先暂存再 rename）、按精确名称查交付 ref |
| `hook.go` | 一次性推送令牌、私有 Unix socket hook 通道与 `RunPreReceive` hook 主体 |
| `mirror.go` | 镜像面的路由、身份解析、逐请求授权与拒绝顺序；只服务读协议，写端点不挂载 |
| `gitrun.go` | 镜像面唯一的子进程：`git upload-pack --stateless-rpc`，封闭环境、有界 stderr、有界超时 |
| `origin.go` | 原点解析客户端：以本服务身份 mTLS 调用解析器并固定其证书，把「不是工作区」与「问不到」分开 |
| `identity.go` | 镜像面自己的客户端身份及其校验（必须声明 git-server 的 service URI 且可用于客户端认证） |
| `state.go` | `MirrorStateReader`：把 Cloud 的绑定与镜像存储的代次/可读状态合成为一次判定所需的快照 |

仓库请求从证书得到认证目标，再请 Core 判定所声明的执行能否读（恢复前序 Revision 的会话）或写（交付）。任何拒绝都是
同一个 `403`，仓库不存在也一样；数据库故障返回 `503`，绝不放行。交付只能新建 `refs/ora/deliveries/<workId>`：
`pre-receive` 在写入任何 ref 之前重新做一次完整的 Core 判定，除非每个更新都是用提交新建这个 ref，否则整次推送拒绝。
同一尝试的重放推送显示 up to date，不会失败。

镜像读的判定顺序是身份的：先拒掉写与未知 service，再解析连接地址，再问 Cloud，再按镜像存储的可读状态与代次开卷。任何
拒绝都是同一个 `403` 与同一段响应体，调用方无法用它区分「不存在」与「无权」；解析器或数据库读不到时返回 `503`，绝不
变成放行。授权与开卷之间镜像被重新发布（代次变化）时拒绝，读由调用方重试并重新授权。

镜像面的协商体按 `Content-Encoding` 解码后才交给 `upload-pack`。git 客户端协商体超过 1 KiB 时会自行压缩并声明
`gzip`，而越线正是增量 fetch 的常态：首次 clone 只要一个 ref、随即 `done`，请求体很小；Workspace 提交过本地工作后
再取下一代镜像时，`have` 分批增长，第三批就会越线。把 deflate 字节当作 packet stream 交给 `upload-pack`，调用方只会
得到一个 500——它读起来像服务坏了，而不是请求有什么问题。仓库面没有这个缺陷，因为它交给 `git http-backend`，由后者
自己解压。本面不实现的编码（例如 `deflate`）与自称 gzip 而不是 gzip 的请求体都在跑 git 之前拒绝（`415` / `400`），
理由相同：交给子进程也只是那个 500，而 500 会让调用方重试一个永远不会成功的请求。线上大小与解开后的大小都由
`MaxRequestBytes` 兜住——前者是调用方「发」不出来的，后者是调用方「声称的编码」撑不出来的。

仓库策略与 hook 路径通过 `GIT_CONFIG_*` 环境变量作用于每个 git 进程，而不写进仓库，因此重启（hook 目录变化）
或旧仓库都无法削弱它们。镜像面的子进程环境是从零构造的封闭集合，且 `GIT_OPTIONAL_LOCKS=0`，所以一次读取不可能改动
已发布的树。两条路径都不持有数据库事务，也不持有上游凭据。

测试用真实 git 客户端、`git http-backend` 与 TLS，配合策略替身覆盖边界矩阵（`service_test.go`、`server_test.go`、
`fault_test.go`、`negotiation_test.go`）；`surface_test.go` 覆盖「只服务被装配的面、路由互不串门」；Core 的
PostgreSQL 测试覆盖判定本身，`integration/git_server_test.go` 用真实 Cloud 状态下的交付、以发布的命令作为 hook 跑通全链路。
`negotiation_test.go` 用真实 git 客户端复现越线的那次增量 fetch（v0/v2 各一次），并把压缩体与明文体当作同一个请求来比对
——后者证明的不只是「不再 500」，而是 git 读到的就是客户端本意的那个 packet stream。长时间的 fetch 不在传输中途重新授权；
写入在 pre-receive 中重新授权。
