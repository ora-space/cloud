# Agent 控制面范围与验收（cloud#41 后续）

[English](agent-run-control-plane-review.en.md) | 中文

A 的 Cloud 控制闭环已实现，直接验收使用真实 PostgreSQL、Git 和 RustFS。按用户授权实现当前 ADR 草案，保留其 proposed 状态。Cloud #41 已合并；本次通过新的上游 PR 提交其后新增的 Revision 交付与恢复修复，并同步当前 main。A 完成不代表 cluster#5/#7 或 M1–M4 跨仓里程碑完成。

依据：[specs#58](https://github.com/ora-space/specs/pull/58)、[cluster#5](https://github.com/ora-space/cluster/issues/5)、[cluster#7](https://github.com/ora-space/cluster/issues/7)、[specs#66](https://github.com/ora-space/specs/pull/66)。

## A 的逐项范围

| 项目 | 实现与直接证据 |
|---|---|
| gRPC、执行登记与恢复 | plugin/session/delivery 固定输入、当前 Node/sandbox、租约和许可；控制面及身份边界测试，`registeredDelivery` 直接断言交付 pending |
| Thread 与命令 | 顺序接管、原子收据、水位和终态排序；持久命令次序、幂等投递及 echo 重启追加/结束；Thread hook 回滚测试 |
| 工作项与授权 | Cloud 按 work ID 固定键，失败重试新 work/key，刷新不改输入；可选 v1 `checksums` 将 SHA-256 绑定 PUT 签名。失败重试、checksum 错误、授权过期与刷新测试 |
| ObjectStore 与 Revision | 事务外私网 HEAD 检查存在、大小、S3 保存的 SHA-256；重查租约、输入和状态；同事务保存原 Node 证据、验证结论、Revision 和 hook。`TestRevisionS3VerificationAndAtomicTakeover` 及网络/状态围栏测试 |
| plugin 与运行 Workspace | 固定计划、逐项校验、整体失败新执行、未知结果 blocked；公开 start/stop/delete 返回 404。`TestStartPluginPlanSurvivesSelectionChangesAndExecutionFailures`、`TestRunWorkspaceAndPluginStep` |
| 五钩子与四 helper | 使用调用方事务；ready/failed、Thread、sessionEnded、delivery、deleted 的成功、失败、重复、回滚直接测试 |
| Go 替身 | 真实 Git 推送交付 ref、封存 JSONL、真实上传；过期授权刷新、上传后双替身重启、原终态重放、Workspace 删除后保留 Revision。`TestSimulatorDeliversRevisionToS3AcrossRestart`、Git 快照测试 |
| 前向迁移 | 仅新增 0027；新库及 0023/0024/0026 重复升级保留历史 effect、输入、命令、原 Node 结果和收据，不补造 Revision |
| cluster 联调 | RustFS 持久卷、凭据文件、幂等建桶、私网/公网 endpoint；独立 sandbox 网络直接上传。`task agent:acceptance` 强制真实 PostgreSQL、S3、sandbox 证据 |

## A/B 接缝与实际实现

`enqueueExecutionWork`、`enqueueThreadCommand`、`createRunWorkspace`、`deleteRunWorkspace` 使用调用方 transaction。五个命名钩子 `runWorkspaceSettled`、`threadEventsTakenOver`、`sessionEnded`、`deliverySettled`、`runWorkspaceDeleted` 使用同一 sql.Tx，不得开启新 Store 事务或做外部 I/O。

成功交付 hook 为 `saved`/`unchanged`，含 `revisionId` 和元数据；失败为 `failed` 加安全 reason；未配置存储一次性 `skipped/object_store_unconfigured`，不派发工作。hook 失败回滚业务写和控制证据。对象不匹配保留原 Node 成功声明，另记 Cloud `verification_failed`，无 Revision；网络失败不 ACK，可重放。已接管重放不重新访问 S3。临时 `revision_verification_required` 拒绝已替换。

代码经 Git Server 交付，提交与 JSONL 内容语义交给 Node；Cloud 只经 Git Server ref 查询核对交付 ref 指向声明的提交、经 HEAD 核对 JSONL，不下载解释它们。真实 Git 替身证明累计恢复，不代替 D 真实 Agent。当前 A schema 没有 B 的 delivering/releasing phase：A 重查已登记交付、已结束会话、运行非终态、maintenance binding、Workspace 准入及当前 Node，B 通过钩子维护 phase。

授权只在内存流转，不进入数据库、日志、文件、环境或命令参数。checksum map 仅影响当前签名，不改输入；上传发送所有返回签名头。健康信息报告数据库可达和对象存储、Git Server 的配置状态，configured 不代表 S3 或 Git Server 在线，不泄漏 endpoint/凭据。

## 验证与跨仓边界

2026-09-30 复核修复：所有 PUT 签入 `If-None-Match: *`，防止未过期授权覆盖已验证对象；412 的声明仍由 Cloud 校验。Controller 取消不保存交付失败，重启继续原执行。单项插件失败释放维护绑定，旧 revision 的迟到失败恢复最新选择为 pending 并清除旧错误。恢复读取对整个固定输入做 protobuf 等值检查，覆盖 pending 与 GetDispatch。直接回归为 `TestRevisionStoredObjectsRejectLiveGrantOverwrite`、`TestPluginFailedResultReleasesMaintenanceForNewSelection`、`TestRevisionUploadReconcilesAnAmbiguousCreatedObject` 与扩展的双重启交付测试。

首次 PUT 前持久固定对象字节和声明，部分上传后重启复用原快照；原子保存终态后清理对象副本。扩展测试在首对象已实际写入 RustFS 后取消并替换双替身、改变 checkout 内容，验证交付仍使用原快照。旧 pending 日志的 `Result: null` 保持可重放，授权从不落盘。

2026-09-30 直接验收使用 PostgreSQL 17 和 `rustfs/rustfs:latest`，digest `sha256:8cc9801755448b71a786705ce76692c77e14936cccd87cf2fc31842e58f4d1ff`。最终格式/lint、全量 PostgreSQL+S3、race、构建、proto/OpenAPI/前端漂移结果记录在 PR。前端全检查覆盖 53 文件、300 测试。

B 仍负责 IssueRun phase、Thread 投影/API/SSE、Git 身份及界面；C 仍需生产 Rust Controller/Node relay、授权消费、日志/ACK 和恢复验收；D 仍需真实 Agent/插件运行和交付。desktop 配套仅固定 Cloud 来源、生成协议和兼容测试。specs 保留这些 Partial，ADR 状态不变。
