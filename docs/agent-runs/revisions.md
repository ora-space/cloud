# Revision：交付、校验登记与续接

[English](revisions.en.md) | 中文

本文的单会话交付与续接规则适用于 Agent IssueRun。WorkflowRun 的 Revision 使用互斥的 `workflow_run_id` 归属和显式 `workflow_manifest_v1` 历史类型，完整规则见[工作流成果保存](../workflows/workflows.md#成果保存与历史归档)；不能把清单作为旧 JSONL 读取，也不启用跨任务前序恢复。

Revision 是一次 Agent 运行保存下来的成果：最终提交（Agent 自己的提交之后，Node 可能为剩余未提交的改动追加一个内部快照提交）、
Node 把最终提交推送到 Project 仓库后得到的**交付 ref**，以及会话 JSONL。代码只经 [Git Server](../repositories/git-server.md)
交付，会话 JSONL 存在对象存储里。没有任何改动时也登记 Revision，同样推送并登记自己的交付 ref。

Revision 只在 Cloud 亲自核对过交付 ref 和会话 JSONL 之后才登记；运行 Workspace 只在 Revision 登记之后（或交付被明确放弃之后）
才释放。运行生命周期见 [Agent IssueRun](issue-runs.md)。

本文描述的 Git 交付与续接实现的是已 `approved` 的决策（见文末相关决策）。

## 依赖的部署组件

交付同时需要对象存储（会话 JSONL）和 Git Server（代码）。任一个没有配置时，交付在声明时就以 `skipped` 结算，运行直接进入释放：
原因先看对象存储（`object_store_unconfigured`），再看 Git Server（`git_server_unconfigured`）。`GET /healthz` 分别报告二者是
`configured` 还是 `unconfigured`，不代表它们此刻在线，也不暴露地址或凭据。

### 对象存储

Cloud 自己持有对象存储的配置，签发限定的上传授权，并在登记前核验对象；Node 只拿到一次性的授权，从不持有存储凭据。

| 配置（`object_store.*`） | 说明 |
|---|---|
| `enabled` | 关闭时不签发任何授权 |
| `endpoint` | Cloud 访问存储的私有地址，用于核验和读取 |
| `public_endpoint` | 可选；Node 与 Cloud 访问存储的地址不同时，签进上传授权的地址 |
| `region`、`bucket`、`path_style` | S3 兼容存储的参数，MinIO、RustFS 一类需要 `path_style: true` |
| `access_key_id_file`、`secret_access_key_file` | 凭据**文件**路径；启动时读入进程内存，不出现在配置、环境变量或日志里 |
| `upload_grant_ttl` | 授权有效期，默认 15 分钟，必须在 1 秒到 7 天之间 |

启动时用两个地址各签一次授权做配置自检，不发网络请求；配置有误时 server 拒绝启动。签名使用自带的 AWS SigV4 实现
（`internal/objectstore`），不引入额外依赖。

### Git Server

Cloud 用 `git_server.*` 配置得到沙盒访问的仓库地址（写进 clone、会话和交付输入），并用自己的客户端证书经 Git Server 的 ref 查询
监听核验交付 ref（配置见 [配置参考](../operations/configuration.md#cloud-server)，ref 查询见
[Git Server](../repositories/git-server.md#ref-查询)）。

## 交付流程

```text
会话终态 ──▶ Cloud 声明交付工作项（固定输入：JSONL 对象键、仓库地址、交付 ref、Workspace 分支）
              │
Controller 登记派发 ──▶ Node 生成内部快照提交与会话 JSONL
              │
Node 推送最终提交到交付 ref ──▶ Git Server 按当前状态授权这次推送
              │
Controller ──GrantRevisionUpload──▶ Cloud 签发 JSONL 的上传授权（只在内存中流转）
              │
Node 上传 JSONL ──▶ 报告终态：revision_delivered / revision_unchanged / revision_failed
              │
Cloud：事务外经 ref 查询核验交付 ref、HEAD 核验 JSONL ──▶ 事务内复核、记录核验结论、登记 Revision、推进运行
```

**1. 声明。** 会话执行有了终态，运行进入 `delivering`，Cloud 在同一事务里声明一个 `deliver_revision` 工作项。输入是固定的：基础
提交（等于运行 Workspace 的 `base_commit_id`）、会话与 checkout 执行的身份、前序 Revision（从会话输入复制，只带 ID 与最终提交），
以及由**服务端**确定的位置：

- 会话 JSONL 对象键 `revisions/{tenant}/{run}/{work}/session.jsonl`；
- Git Server 上的仓库地址 `gitRepositoryUrl`（`git_server.repository_base_url` 加 `/{tenant}/{project}.git`）；
- 本次尝试专属的交付 ref `refs/ora/deliveries/{work}`；
- 运行 Workspace 在 Git Server 上的分支 `workspaceRef`，取自做出这个 checkout 的 clone 执行的输入；
- Node 在 checkout 里写的本地 ref `refs/ora/revisions/{run}`，它不出现在 Git Server 上。

对象键和交付 ref 都包含工作项 ID，所以每次重试都是一个新的工作项、一个新的对象键和一个新的交付 ref，没有调用方能够指定另一次
尝试已经写过的对象或 ref。

**2. 推送。** Node 把最终提交推送到交付 ref，可以顺带把它快进到 `workspaceRef`。Git Server 只在运行处于 `delivering`、交付执行
仍然有效时放行，交付 ref 只能新建，`workspaceRef` 只能新建或快进，而且必须是证书所属 Workspace 的分支（见
[Git Server](../repositories/git-server.md#请求与授权)）。Cloud 只核验交付 ref，不核验 `workspaceRef`。

**3. 上传授权。** Controller 调用 `GrantRevisionUpload` 为交付执行取得会话 JSONL 的授权，可以同时提交它的 SHA-256；提交的摘要
只能针对输入固定的 JSONL 对象键，其他键返回 `400 invalid_upload_checksum`。授权只覆盖一个对象键、一个方法、一段时限：

- 每个 `PUT` 都签入 `If-None-Match: *`，存储侧只允许首次创建，仍在有效期内的授权也不能覆盖已经验证过的对象；
- 提交了摘要时，摘要以 `x-amz-checksum-sha256` 签进请求，内容不符时存储拒绝上传；
- 授权是短期的持有者凭据，**不是**执行输入：它只在内存和进程间流转，不写数据库、文件、环境变量、命令参数或日志。刷新授权不会改变
  固定的输入或对象键。

**4. 结果。** Node 报告的终态是三者之一：

- `revision_delivered`：最终提交既不等于基础提交，也不等于前序 Revision 的最终提交；
- `revision_unchanged`：最终提交等于基础提交，或者等于续接的前序 Revision 的最终提交；
- `revision_failed`：失败原因属于封闭集合 `session_not_settled`、`checkout_unavailable`、`snapshot_failed`、`history_unavailable`、
  `upload_failed`、`push_rejected`、`push_failed`。原来的 `bundle_failed`（值 4）在线上枚举里是 `reserved`，带这个值的结果按封闭集合之外的原因拒绝。

声明的结果与改动情况不符，或者还带着 bundle，返回 `409 result_conflict`。

**5. 核验与登记。** 成功的声明分两步处理：

- **事务外**：先在短事务里核对服务身份、租约、执行身份、JSONL 的形状，以及 JSONL 对象键、基础提交、本地 Revision ref 与冻结输入
  一致；然后经 Git Server 的 ref 查询读出交付 ref 指向的提交，它必须等于声明的最终提交；再对 JSONL 向私有地址发 `HEAD`，比对大小
  和存储保存的 SHA-256。交付 ref 不存在、指向其他提交、JSONL 不符或存储没有返回摘要（它从没收到过校验和），都算核验失败。
  Git Server 或对象存储不可达返回可重试的 `503`（`git_server_unavailable`、`object_store_unavailable`），不接管、不确认 Node 事件，
  之后可以重放。交付 ref 只能新建、不能移动或删除，所以核验时看到的提交就是登记之后它一直指向的提交。
- **事务内**：再次检查运行没有结束、运行 Workspace 仍就绪、运行时控制仍属于这个运行、当前 Node 与沙盒仍是交付的目标；然后写入核验
  结论（`revision_verifications`），核验通过时登记 `revisions` 行并记下 `delivery_ref`，再调用业务钩子把运行推进到 `releasing`。
  原始的 Node 结果、收据、核验结论、Revision 和业务转换在同一个事务里提交。

核验不通过时，Cloud 记录自己的结论 `verification_failed`，保留 Node 的原始声明，不登记 Revision，交付按退避重试；这个原因只能由 Cloud
写出，Node 无法声称它。已经结算过的执行再重放时，不会重新访问 Git Server 或存储。交付的重试与放弃上限见
[Agent IssueRun](issue-runs.md#上限)。

每个交付 ref 至多属于一个 Revision（部分唯一索引 `revisions_delivery_ref`）。改走 Git 之前登记的 bundle 形状的 Revision 原样保留，
数据库约束让两种形状互斥（迁移 `0054`）。

### 放弃交付后的原事实

根 D5 已将运行以 `deliveryState=failed`、`revisionId=null` 移到 `releasing`，或随后根 D8 将其移到 `done` 时，
原已登记交付仍可按原 execution、work、Workspace、Node 和 sandbox/generation 身份提交终态，无需残留账本或运维接管意图。
这条入口不放宽普通 `liveDelivery` 或新派发资格，也不改变运行、原失败 delete、admission 或回收责任。
原声明、事件 receipt、submission 和必要证据原子提交后，现有 takeover 成功才允许 ACK；ACK 只表示事实已接管。
迟到失败不重试、不结算业务；成功声明仍按上述证据规则核验。

根 D5/D8 和已有残留救援均只在同租户同 Issue 没有后创建 Run 时首次登记迟到 Revision。判断使用每个 Run 创建时同事务写入的
`run.enqueued` 活动 seq，在最终登记事务内与 Run 创建互斥；不按时间戳、UUID、执行者、状态或软删除筛选。
顺序证据缺失、有歧义或已有后创建 Run 时，只保存原声明与 receipt，不登记 Revision 或 verified delivery。
同事实重放保留第一次结论，不重新读取外部证据、登记或调用业务 hook；真实冲突仍按原规则拒绝，已有 ledger 的冲突审计不变。
正常释放在独立事务中继续，D8 的历史失败删除不会因迟到接收重启。依据
[迟到事实接管 ADR D1–D3](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/20261022-late-delivery-fact-takeover.md)（approved，落地运行负责实施状态与联合验收）。

## 续接前序 Revision

同一个 Issue 上再次 @Agent 时，新运行仍然得到全新的运行 Workspace，但在 Agent 启动之前，Node 先从 Git Server 取回该 Issue 最近
一个 Revision 的交付 ref，恢复进 checkout，Agent 因此接着上一次的成果工作。

- **选择在会话启动时一次完成并固定**：候选是同一租户、同一 Issue 的**其他**运行登记的、带交付 ref 且最终提交不同于基础提交的
  Revision，按登记时间取最新的一个。只停在基础提交上的 Revision 会把新 checkout 带回旧代码，改走 Git 之前的 bundle Revision 没有
  可取的 ref，二者都不是候选。候选的 Project 或外部仓库地址与本次运行不同时不续接（`project_changed`）；候选曾被标记为不可续接时
  不续接（`resume_refused`）；本次 checkout 不是从候选所在的 Git Server 仓库 clone 的、或者没有配置 Git Server 时不续接
  （`repository_changed`）；不回退到更早的候选。选中的 Revision 写进会话输入的 `priorRevision`
  （`revisionId`、`finalCommit`、`baseCommit`、`gitRepositoryUrl`、`deliveryRef`）和运行的 `resumeRevisionId`，之后不再改变。
- **读取经 Git Server 授权**：存活的 Agent 会话可以读取整个 Project 仓库，取回交付 ref 不需要额外的授权；对象存储不再为续接签发
  任何下载授权。
- **恢复失败就失败**：恢复不了时会话以 `agent_failed` 结束，运行失败、不启动 Agent，但照常经过交付。只有交付 ref 取不回来
  （`prior_revision_base_unavailable`）这种永久失败会把该 Revision 标为不可续接，下一次运行从头开始；运行已经请求取消时例外，
  因为 Git Server 对已取消运行的读取给出的拒绝与 ref 不存在相同，这次失败不能说明 Revision 本身有问题。
- **只需要最近一个**：续接后的提交建立在前序最终提交之上，新的交付 ref 自然包含此前所有的提交。续接运行的最终提交与前序相同
  （没有新改动）时记为 `unchanged`，它同样有自己的交付 ref，不沿用前序的任何东西。
- **同一 Issue 的运行假定是串行的**，Cloud 不强制：两个重叠的运行可能选中同一个前序 Revision，下一次运行只会续接其中登记较晚的那个。

会话输入另带一份 `workspaceDirectory`：会话开始时该 Project 最多 100 个 Workspace（最新在前，运行 Workspace 删除后仍列出，因为它们
的分支还在 Git Server 上），每项带 Workspace ID、类型（`main`、`isolated`、`run`），运行 Workspace 另带 Issue、运行与运行状态。
WorkflowRun 的 Workspace 不列入：它的会话没有 Git Server 访问，目录里也没有它的字段。目录只供会话里的 Agent 参考，不刷新，也从不用于
授权。

## 公开读取

运行资源的 `revision` 字段投影已登记 Revision 的元数据（字段以 [`api/openapi.json`](../../api/openapi.json) 为准）。`changed` 是 Cloud
自己的核验结论：本次运行交付了新的提交时为 `true`，停在基础提交或前序最终提交上时为 `false`；对改走 Git 之前登记的 Revision，它表示
"本次存了自己的 bundle"。经 Git 交付的 Revision 的 `bundleSize` 为 `null`。没有登记 Revision 的运行该字段为 `null`，原因看
`result.deliveryState`。对象键、交付 ref、Revision ref 和摘要都不公开，公开 API 也不提供代码下载。会话 JSONL 归档可以按
[会话读取](session-reads.md) 读取。

## 相关决策

- [Cloud 持有对象存储配置并签发限定上传授权，Revision 在 Cloud 校验对象后才登记](https://github.com/ora-space/specs/blob/main/decisions/cloud/revision/0-cloud-owned-object-store-and-verified-revisions.md)
- [同一 Issue 的新运行先续接最近一次 Revision，再启动 Agent](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/20261010-resume-run-from-latest-issue-revision.md)
- [Revision 的代码改动只经 Git Server 交付：Cloud 经 ref 查询核验后登记交付 ref，续作只从前序自己的交付 ref 恢复](https://github.com/ora-space/specs/blob/main/decisions/cloud/revision/20261017-git-native-delivery-refs-and-resume.md)（approved；它取代 20261012 的 Git Server 交付决策）
- [clone 输入替换为 Git Server 快照输入；Git Server 按执行类型给出写入规则；交付与会话输入去掉 bundle，删除下载授权](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20261016-snapshot-clone-input-and-execution-ref-grants.md)（approved）
