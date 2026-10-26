# 外部 Git 线：Repo Executor、Credential Store 与同步发布

[English](external-git.en.md) | 中文

外部 Git 线是 Ora 与外部 Git 仓库之间唯一的通道：Repo Executor 把外部仓库同步进 Project 仓库，把 Project 仓库显式发布回
外部；凭据由独立的 Credential Store 保管；Cloud 只记录仓库 Binding、仓库 Operation 和凭据元数据。本文说明这条线的参与方、
同步与发布的过程、凭据的生命周期，以及贯穿其中的授权与失败收敛规则。Git Server 自身的监听与 ref 规则见
[Git Server 与 Project 仓库](git-server.md)。

## 参与方

| 参与方 | 入口 | 职责 |
|---|---|---|
| Cloud server | `cmd/server`，`internal/repoworker` | 记录 Binding 与 Operation；签发一次性授权；租约、续租和结算；同步成功前经 ref 查询核对；不接触凭据明文 |
| Repo Executor | `cmd/repo-executor`，`internal/repoexec` | 唯一访问外部 Git 仓库的进程；无状态、不连数据库；每个任务在私有临时目录里执行，结束后销毁目录与凭据材料 |
| Credential Store | `cmd/credential-store`，`internal/credentialstore` | 在独立卷上以信封加密保管 HTTPS token；四个监听分别服务浏览器提交、执行器兑换、Cloud 管理和 KEK 运维 |
| `git-credential-ora` | `cmd/git-credential-ora` | Repo Executor 为外部远端配置的唯一 Git credential helper；只在请求的仓库等于暂存作用域时交付凭据 |
| Git Server | `cmd/git-server`，`internal/gitserver` | Project 仓库的 smart HTTP 与只读 ref 查询；执行器经服务仓库监听写入快照与跟踪 ref |

外部仓库只经 https 访问。Repo Executor 同时接入内部服务网络与一条出网网络；Credential Store 只接入内部网络；两者都不加入
沙盒网络，沙盒也永远拿不到执行器证书或凭据明文。

## 同步

同步（`repository_sync`）把外部仓库的一个分支带进 Project 仓库。它在以下时刻创建：Project 创建事务里（触发原因
`project_created`）、成员手动发起、成员为 Binding 提交候选凭据，以及一次发布改变了目标分支之后（`after_publish`）。
同一个 Binding 同时至多有一个未结束的同步；重复的手动发起返回已有的那个 Operation。

1. 后台工作进程按空闲槽位领取 `pending` 的 Operation，把它移入 `running` 并在同一事务里签发这一次执行的一次性授权。
   进入 `running` 之前，它先经 ref 查询读取 Binding 的跟踪 ref；读不到就把租约还回去，不启动任务。授权记下这个期望旧值
   `expectedTrackingOid`，跟踪 ref 只能从它出发移动。
2. Repo Executor 验证授权与任务逐字段一致，检查出口策略，在 `tasks/` 下新建裸仓库，必要时以跟踪 ref 为基础 fetch，
   然后从外部远端 fetch。推送是一次 `--atomic` 的推送：在 `refs/ora/upstreams/<bindingId>/snapshots/<oid>` 新建快照，
   同时把 `refs/ora/upstreams/<bindingId>/heads/<branch>` 从期望旧值推到新提交。快照是 create-only 且名称必须等于目标提交，
   跟踪 ref 是 tracking-cas 且允许非快进，因此外部 force-push 只会让跟踪 ref 前进到新的历史。
3. 执行器报告 `applied` 之后，Cloud 还要经 ref 查询确认快照 ref 与跟踪 ref 都指向上报的提交，才在事务里记录 `succeeded`，
   并把该提交记为 Binding 的当前快照。查询失败在租约内退避重试；租约一旦结束就不做记录，交给清扫按幂等规则重放。
4. 同步成功会为所用凭据的当前版本记下 `read` 能力。候选凭据触发的同步成功、且 Binding 期间未被改动时，候选凭据同时被采纳。

未知结果的收敛按"任务是否可能已经执行"区分：执行器拒绝（`429 executor_busy`、身份不匹配）和请求未送达时任务一定没有开始，
记为 `failed`；连接在请求发出后丢失时同步可能已经写入，记为 `failed` 并由 Cloud 创建至多三次自动重放；推送因
`stale info` 被取代时记 `superseded`，跟踪 ref 已等于本提交且快照存在的特例记为 `applied`。

## 发布

发布（`repository_publish`）是 Ora 写外部仓库的唯一方式，只能由用户显式创建。它的字段在创建后不可变：源 ref 与提交
（工作区分支、交付 ref，或一个已有 Revision：取它登记时核验过的交付 ref 与最终提交，没有交付 ref 的 bundle 形态
Revision 以 `revision_not_publishable` 拒绝）、目标分支、期望旧值 `expectedOldOid` 和凭据引用；`expectedOldOid` 必须与
源提交不同。

1. Cloud 先经 ref 查询确认源 ref 存在（不存在时以 `409 source_ref_not_found` 拒绝），再创建 Operation；它不比对源 ref
   的当前提交。源提交是否可从源 ref 到达由执行器检查，因为 Cloud 不读取 Git 对象。
2. 凭据的当前版本必须已具备 `publish` 能力。个人凭据由所有者批准；租户管理员用别人的个人凭据发布时需要所有者在
   `awaiting_approval` 上批准，管理员不能代批。团队凭据由租户管理员直接发布。批准把字段与凭据版本绑定，有效期 24 小时；
   期间凭据出现新版本时，这次发布以 `credential_changed` 取消或拒绝。
3. 执行器从 Git Server 服务仓库监听取源，先对目标做一次 `ls-remote`，再以 `expectedOldOid` 为比较值做只快进的 lease 推送。
   发布从不 force，非快进以 `not_fast_forward` 拒绝。
4. 成功之后 Cloud 为 Binding 请求一次同步，让当前快照追上被发布的分支；这次同步吸收已有的补同步请求。

发布收到不确定结果时进入 `uncertain`，不会盲目重发。第一次核对等到任务期限之后（此时执行器已不可能再推送），之后由
`probe` 授权读取目标 ref，间隔从 2 分钟指数增长到 1 小时，至多 20 次（约 16 小时）：目标等于源提交记 `succeeded`，仍等于
期望旧值记 `failed(not_applied)`，其他值记 `conflict(moved_after_uncertain)`；核对本身读不到时计入失败次数，用尽后记
`failed(outcome_unknown)`。成员可以请求立即核对一次。

## 凭据

Cloud 只保存凭据的元数据：归属（`personal` 或 `team`）、名称、仓库作用域、Store 当前版本、已证明的能力和可用状态。
token 从浏览器直接进入 Credential Store，任何 Cloud 路径都不会看到它。

1. **创建。** 个人凭据由成员创建，团队凭据只能由租户管理员创建。仓库作用域必须是规范化后的 https 仓库地址；Cloud 返回一份
   提交授权（`submit` 用途，至多 5 分钟）和 Credential Store 的入口 URL。
2. **提交。** 浏览器把 token 直接 `PUT` 到 Credential Store 的浏览器入口，凭据是提交授权。Store 以自己的时钟和 Cloud 的
   提交公钥集合验证授权，要求授权的 `credentialId` 等于路径、版本恰为当前版本加一、提交尚未使用，然后独占认领并写入新版本。
3. **确认。** 浏览器随后通知 Cloud。Cloud 不信任这个通知，而是经管理接口询问 Store 当前版本与提交 ID，只有与待定提交完全
   一致才把凭据记为 `stored`。提交授权过期后 Cloud 再等 2 分钟，询问一次 Store，采纳已写入的版本或清除待定记录。
4. **轮换。** 轮换为下一个版本签发新的提交授权，旧版本在保留期内仍可兑换；新版本在确认前不改变凭据的可用状态。
5. **撤销。** Cloud 先把自己的引用标为 `revoked` 并停止签发新的授权，再调用 Store 撤销并重试直到成功。Store 撤销会删除所有
   版本的密文；未知 ID 也会留下撤销标记。
6. **能力。** 新版本开始时没有任何能力。同步成功证明 `read`；发布能力 `publish` 由一次 `receive-pack` 广告探测证明
   （`probe` 授权，`verified` / `push_denied` / `verification_failed`）。外部平台拒绝 token 时能力被移除，凭据可能被标为
   `invalid`；个人凭据的所有者离开租户后，凭据被冻结。

凭据只有在 `stored`、可用、作用域与 Project 的仓库一致且具备所需能力时才能支撑新的远端工作，这个判断在创建工作和签发
授权时各做一次。Workspace 的 clone 不访问外部仓库，也不使用凭据：它只从 Binding 的当前快照 clone（见
[Workspace 生命周期](../runtime/workspaces.md#clone-步骤从-binding-快照-clone)），所以带凭据的 Project 与公开仓库的 Project
clone 方式相同。

## 授权与最小权限

| 授权 | 签发方 | 验证方 | 用途 |
|---|---|---|---|
| 执行授权（`sync` / `publish` / `probe`） | Cloud 的执行签名密钥 | Repo Executor、Credential Store | 启动一个任务；兑换一次凭据明文 |
| 提交授权（`submit`） | Cloud 的提交签名密钥 | Credential Store 浏览器入口 | 从浏览器写入一个凭据版本 |

- 两类授权使用两把不同的 Ed25519 私钥，Cloud 启动时拒绝相同的两把键；即使误配成同一把，严格的用途字段也让两者不能互用。
- 授权是 `base64url(payload).base64url(signature)`，验证方持有公钥集合以便轮换。有效期至多 15 分钟；执行授权里的
  `executorIdentity` 必须是 `spiffe://ora.local/repo-executor`。
- 执行授权按用途带字段：`sync` 带 Operation、租户、Project、Binding、仓库、分支与期望旧值；`publish` 带源 ref、源提交、
  目标 ref 与期望旧值；`probe` 带一次核对的 ID 或 Operation ID、仓库与 `upload-pack` / `receive-pack` 服务。其他用途的字段
  必须为空，`probe` 授权不能驱动发布。
- 凭据兑换每份执行授权至多一次：Credential Store 先独占创建认领文件，再解密。Repo Executor 只为一次任务把明文写进
  `0700` 的私有材料目录，任务结束（包括失败和 panic）时删除材料与任务目录。
- Repo Executor 的出口规则来自部署方共享的 `deployment-egress.json`（与 Cloud、model-gateway 同一份）：公网模式经部署 DNS
  解析，只接受 https 且每个地址都必须是公网地址；企业模式只接受文件里精确列出的 host:port，未命中的目标在 DNS 前拒绝。共享策略
  的 topology 始终拒绝 cluster 网段与宿主地址，因此 Git Server、Credential Store 这类内部服务不会成为外部仓库目标。每个访问
  外部仓库的 Git 子进程启动前重新解析并检查全部地址，再用 `http.curloptResolve` 把检查过的数字地址交给 Git，TLS 仍按原主机名
  验证；域名在检查后改指别处也连不过去。探针的 `receive-pack` 用同一拨号器。健康监听报告 schema 与摘要，供部署方在更换配置后
  与 Cloud、model-gateway 比对。外部远端拿不到执行器证书或 `Ora-Git-Operation-Id` 头。`git-credential-ora` 的参数固定为材料
  目录，Git 调用它时只有规范化后完全等于作用域的仓库才拿到用户名和密码，重定向、子模块或同 host 的另一个仓库都拿不到。

## 失败与清扫

仓库工作进程每 2 秒做一轮清扫，再按空闲槽位领取工作：

| 清扫 | 作用 |
|---|---|
| 租约过期 | `leased` 回到 `pending`（还没有授权，可安全重试）；`running` 的同步记 `failed(lease_lost)` 并重放，发布记 `uncertain` |
| 批准过期 | 超过 24 小时未执行的发布记 `cancelled(approval_expired)` |
| 未确认提交 | 提交授权过期 2 分钟后采纳 Store 已写入的版本或清除待定记录 |
| Store 撤销 | 重试 Cloud 已记录但 Store 尚未确认的撤销 |

默认并发是 4 个任务，租约 1 分钟并按三分之一续租，同步上限 30 分钟、发布与探测上限 5 分钟。租约丢失时工作进程立刻取消
任务，随后的结算被租约世代栅栏拒绝；同一个 Operation ID 只进入 `running` 一次，失去租约的执行器不能借后来的租约继续写。

## 配置

整条线由 Cloud 配置的 `repository.*` 打开（环境变量前缀 `CLOUD_REPOSITORY_`），默认关闭：关闭时 Project 仍可创建，但没有
同步，凭据创建返回 `503`。Repo Executor 与 Credential Store 各自只读自己的 `REPO_EXECUTOR_*`、`CREDENTIAL_STORE_*` 环境变量。
所有键、证书与文件引用见 [配置参考](../operations/configuration.md#repo-executor-与-credential-store)；进程镜像、密钥、网络与
备份见 [部署与运维](../operations/deployment.md)。

## 相关决策

以下 ADR 都是 `approved`，本仓库实现其中对应的部分：

- [Repo Executor 是访问外部 Git 的唯一边界](https://github.com/ora-space/specs/blob/main/decisions/cloud/repo-executor/0-repo-executor-sole-external-git-boundary.md)（approved）
- [Git 凭据由独立的 Credential Store 保管](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/20261016-git-credentials-in-independent-credential-store.md)（approved）
- [仓库 Binding 与快照同步](https://github.com/ora-space/specs/blob/main/decisions/cloud/project/20261016-repository-binding-and-snapshot-sync.md)（approved）
- [显式的只快进发布](https://github.com/ora-space/specs/blob/main/decisions/cloud/project/20261017-explicit-fast-forward-publish.md)（approved）
