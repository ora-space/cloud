# Revision：交付、校验登记与续接

[English](revisions.en.md) | 中文

Revision 是一次 Agent 运行保存下来的成果：最终提交（Agent 自己的提交之后，Node 可能为剩余未提交的改动追加一个内部快照提交）、
相对本次 clone 基础提交的增量 bundle，以及会话 JSONL。没有任何改动时也登记 Revision，只是没有 bundle。

Revision 只在 Cloud 亲自核对过对象存储里的对象之后才登记；运行 Workspace 只在 Revision 登记之后（或交付被明确放弃之后）才释放。
运行生命周期见 [Agent IssueRun](issue-runs.md)。

## 对象存储

Cloud 自己持有对象存储的配置，签发限定的上传和下载授权，并在登记前核验对象；Node 只拿到一次性的授权，从不持有存储凭据。

| 配置（`object_store.*`） | 说明 |
|---|---|
| `enabled` | 关闭时不签发任何授权；交付在声明时就以 `skipped` 结算，运行直接进入释放 |
| `endpoint` | Cloud 访问存储的私有地址，用于核验和读取 |
| `public_endpoint` | 可选；Node 与 Cloud 访问存储的地址不同时，签进上传和下载授权的地址 |
| `region`、`bucket`、`path_style` | S3 兼容存储的参数，MinIO、RustFS 一类需要 `path_style: true` |
| `access_key_id_file`、`secret_access_key_file` | 凭据**文件**路径；启动时读入进程内存，不出现在配置、环境变量或日志里 |
| `upload_grant_ttl` | 授权有效期，默认 15 分钟，必须在 1 秒到 7 天之间 |

启动时用两个地址各签一次授权做配置自检，不发网络请求；配置有误时 server 拒绝启动。`GET /healthz` 报告对象存储是 `configured`
还是 `unconfigured`，不代表存储此刻在线，也不暴露地址或凭据。签名使用自带的 AWS SigV4 实现（`internal/objectstore`），不引入
额外依赖。

## 交付流程

```text
会话终态 ──▶ Cloud 声明交付工作项（固定输入与对象键）
              │
Controller 登记派发 ──▶ Node 生成内部快照、bundle 与会话 JSONL
              │
Controller ──GrantRevisionUpload──▶ Cloud 签发上传授权（只在内存中流转）
              │
Node 上传对象 ──▶ 报告终态：revision_delivered / revision_unchanged / revision_failed
              │
Cloud：事务外核验对象 ──▶ 事务内复核、记录核验结论、登记 Revision、推进运行
```

**1. 声明。** 会话执行有了终态，运行进入 `delivering`，Cloud 在同一事务里声明一个 `deliver_revision` 工作项。输入是固定的：基础
提交（等于运行 Workspace 的 `base_commit_id`）、会话与 checkout 执行的身份、前序 Revision（从会话输入复制），以及三个由**服务端**
确定的位置：

- bundle 对象键 `revisions/{tenant}/{run}/{work}/revision.bundle`；
- 会话 JSONL 对象键 `revisions/{tenant}/{run}/{work}/session.jsonl`；
- Revision ref `refs/ora/revisions/{run}`。

对象键包含工作项 ID，所以每次重试都是一个新的工作项、一组新的对象键，没有调用方能够指定另一次尝试已经写过的对象。

**2. 上传授权。** Controller 调用 `GrantRevisionUpload` 为交付执行取得授权，可以同时提交每个对象的 SHA-256。每个授权只覆盖一个对象键、
一个方法、一段时限：

- 每个 `PUT` 都签入 `If-None-Match: *`，存储侧只允许首次创建，仍在有效期内的授权也不能覆盖已经验证过的对象；
- 提交了摘要时，摘要以 `x-amz-checksum-sha256` 签进请求，内容不符时存储拒绝上传；
- 授权是短期的持有者凭据，**不是**执行输入：它只在内存和进程间流转，不写数据库、文件、环境变量、命令参数或日志。刷新授权不会改变
  固定的输入或对象键。

**3. 结果。** Node 报告的终态是三者之一：`revision_delivered`（有改动，带 bundle 与 JSONL）、`revision_unchanged`（没有改动，只有 JSONL）、
`revision_failed`（失败原因属于封闭集合：`session_not_settled`、`checkout_unavailable`、`snapshot_failed`、`bundle_failed`、
`history_unavailable`、`upload_failed`）。

**4. 核验与登记。** 成功的声明分两步处理：

- **事务外**：先在短事务里核对服务身份、租约、执行身份、对象形状，以及对象键、基础提交、Revision ref 与冻结输入一致；然后对每个对象
  向私有地址发 `HEAD`，比对大小和存储保存的 SHA-256。存储没有返回摘要（它从没收到过校验和）同样算核验失败。网络故障返回可重试的
  错误，不确认 Node 事件，之后可以重放。
- **事务内**：再次检查运行没有结束、运行 Workspace 仍就绪、运行时控制仍属于这个运行、当前 Node 与沙盒仍是交付的目标；然后写入核验
  结论（`revision_verifications`），核验通过时登记 `revisions` 行，再调用业务钩子把运行推进到 `releasing`。原始的 Node 结果、收据、
  核验结论、Revision 和业务转换在同一个事务里提交。

核验不通过时，Cloud 记录自己的结论 `verification_failed`，保留 Node 的原始声明，不登记 Revision，交付按退避重试；这个原因只能由 Cloud
写出，Node 无法声称它。已经结算过的执行再重放时，不会重新访问存储。交付的重试与放弃上限见 [Agent IssueRun](issue-runs.md#上限)。

## 续接前序 Revision

同一个 Issue 上再次 @Agent 时，新运行仍然得到全新的运行 Workspace，但在 Agent 启动之前，Node 先把该 Issue 最近一个 Revision 的成果
恢复进 checkout，Agent 因此接着上一次的成果工作。

- **选择在会话启动时一次完成并固定**：候选是同一租户、同一 Issue 的**其他**运行登记的、带 bundle（自己存了或沿用了前序的）的 Revision，
  按登记时间取最新的一个。候选的 Project 或仓库地址与本次运行不同时不续接（`project_changed`）；候选曾被标记为不可续接时也不续接
  （`resume_refused`）；不回退到更早的候选。选中的 Revision 写进会话输入的 `priorRevision` 和运行的 `resumeRevisionId`，之后不再改变。
- **下载授权**：会话执行用 `GrantRevisionDownload` 取得前序 bundle 的只读 `GET` 授权，对象键就是会话输入固定的那个。
- **恢复失败就失败**：恢复不了时会话以 `agent_failed` 结束，运行失败、不启动 Agent，但照常经过交付。只有"远端历史被改写、基础提交再也
  取不到"（`prior_revision_base_unavailable`）这种永久失败会把该 Revision 标为不可续接，下一次运行从头开始。
- **只需要最近一个**：交付的 bundle 始终相对本次 clone 的基础提交生成，续接后的提交建立在前序最终提交之上，所以新 bundle 自然包含此前
  所有尚未进入远端的提交。续接运行的最终提交与前序相同（没有新改动）时记为 `unchanged`，并沿用前序的 bundle。
- **同一 Issue 的运行假定是串行的**，Cloud 不强制：两个重叠的运行可能选中同一个前序 Revision，下一次运行只会续接其中登记较晚的那个。

## 公开读取

运行资源的 `revision` 字段投影已登记 Revision 的元数据：`id`、`baseCommit`、`finalCommit`、`changed`（本次是否存了自己的 bundle）、
`bundleSize`、`historySize`、`priorRevisionId`、`createdAt`。没有登记 Revision 的运行该字段为 `null`，原因看 `result.deliveryState`。
对象键、Revision ref 和摘要都不公开，公开 API 也不提供 bundle 下载。会话 JSONL 归档可以按 [会话读取](session-reads.md) 读取。

由 Git Server 接收交付、以交付 ref 作为登记依据的方案仍在决策中，见 [Git Server](../repositories/git-server.md#ref-查询)。

## 相关决策

- [Cloud 持有对象存储配置并签发限定上传授权，Revision 在 Cloud 校验对象后才登记](https://github.com/ora-space/specs/blob/main/decisions/cloud/revision/0-cloud-owned-object-store-and-verified-revisions.md)
- [同一 Issue 的新运行先续接最近一次 Revision，再启动 Agent](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/20261010-resume-run-from-latest-issue-revision.md)
- [会话与交付执行输入增加前序 Revision，新增 `GrantRevisionDownload`](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20261010-revision-restore-contract.md)
- [Revision 的代码改动只经 Git Server 交付](https://github.com/ora-space/specs/blob/main/decisions/cloud/revision/20261012-git-server-delivery-and-verified-delivery-refs.md)（proposed）
