# 阶段一验收记录

2026-09-09，工作区 `D:\project\cloud`。已完成阶段一 cloud 核心与模拟执行契约，不等同于真实 Controller/Node/Kubernetes 上线。源码未提交、未推送、未部署；desktop 只读参考，独立 `specs/` 仓库没有修改。

## 实际运行环境与结果

- Windows amd64，Go 1.27.1，Git，隔离 PostgreSQL 17.11（`.local/postgres`，`127.0.0.1:55432`，测试库 `ora_test`）。每个集成用例独立 schema，测试结束清理 schema。
- `task check`：通过，包含严格格式检查、原有 golangci-lint 规则和全部测试；PG 被强制要求，不存在 SQLite/mock repository 替代或静默跳过。19 个顶层集成测试，加凭据/约束子用例，全部通过；最后一次 integration 用时 23.328s，跨 internal 包语句覆盖 83.4%。覆盖率仅是辅助信息，不代替下列不变量证据。
- `task test:race`：通过，`CGO_ENABLED=1`，使用隔离 LLVM-MinGW 20260908 编译器；最后一次 integration 用时 33.860s，无 race 报告。
- `task build`：server/cloudctl/simulator 全部构建成功。
- `go run ./cmd/cloudctl -command migrate`：迁移 0001–0004 成功且可重入；集成测试既在新 schema 运行迁移两次，也从含数据的 0003 schema 升级并验证回填。server 检查全部 migration checksum，并拒绝来自更新二进制的未知 migration；启动时不执行迁移。
- `go run ./cmd/simulator`：真实 HTTP/PG/Git 演示完成，输出 `observedState=ready`、`admissionOpen=true`、generation=1；磁盘保留 bare repo 和 linked main worktree。此命令没有启动真正的 Rust Node/Agent。
- 空 trust 配置运行 server：实际非零退出（exit 1），没有退回匿名访问。PG/监听失败也由启动入口返回错误。
- `git diff --check`：通过。仅存在 Windows 行尾提示，不存在 patch 空白错误。

本机详细输出位于忽略目录 `.local/check-final.txt`、`.local/race-final.txt`、`.local/demo-result.json`，不会作为源码提交。测试入口和运行命令见 [README](../README.md)。CI 配置与这些门禁一致，但尚未推送运行远端 CI；本机没有 Docker，未构建/运行容器镜像。

## 需求—代码—直接验证

| 需求 | 实现 | 直接证据与结论 |
|---|---|---|
| PG、UUID/timestamptz、显式 migration、依赖注入 | `internal/repository/db.go`、`internal/core/store.go`、`internal/core/migrations/0001_core.sql`–`0004_effect_intent_and_ticket_scope.sql` | 每个 integration setup 使用真实 PG 全新 schema，迁移连续执行两次；`TestMigrateUpgradesPreviousSchemaAndData` 验证从含数据的上一 schema 升级；无 AutoMigrate/全局 DB/旧用户 CRUD |
| 非对称内部签名、服务/最终用户分别验证 | `internal/core/auth.go`、`internal/api/router/router.go` | `TestCredentialVerificationAndDisabledAccounts`：伪造、none、过期、未来、超长、缺 exp、错 aud/issuer/caller、错角色均拒绝；`TestDatabaseEffectAndTicketScopes`：gateway key 不能伪造 controller role |
| 并发首次登录唯一，source 不依赖协议/name/email | `store.go:identity`、user_identities 联合唯一 | `TestIdentityConcurrencyMembershipAndIsolation` 20 并发请求只产生一个新 user，无孤儿；凭据测试验证相同 subject 不同 source 不合并 |
| 无成员不准入，停用保留归属 | `membership`、`access` | `TestIdentityConcurrencyMembershipAndIsolation`、`TestRemainingPublicContractsAndMembershipRevocation`：创建/读取/执行被拒绝，PG 资源 owner 仍保留 |
| tenant+owner 隔离，admin 不能读取内容 | `public.go` SQL过滤、`adminResource/adminOperation` | `TestIdentityConcurrencyMembershipAndIsolation`：跨用户 Project/Workspace/operation 404，跨租户拒绝，列表为空；admin status/stop/op没有 request/result/repo/secret |
| 最后有效管理员并发保护 | membership事务和 PG 延迟触发器 | `TestConcurrentIdempotencyAndLastAdminProtection`：并发自降级恰一成功一409；`TestPostgresAggregateConstraints`：直接 SQL停用最后成员/用户被拒绝 |
| Project/main 原子聚合、复合 FK、隔离 Task 身份 | migrations 0001–0004、createProject/insertWorkspace | `TestPostgresAggregateConstraints`：缺main、移main、删main、跨归属Workspace、main关联Task、跨租户operation与跨owner ticket均拒绝；HTTP创建验证storage/main/operation事务与Task |
| 运行实例唯一、绑定不覆盖 | sandbox/node unique/FK、planEffect/nodeCommand | `TestPostgresAggregateConstraints`：第二live sandbox被拒绝；`TestOperationPreconditionsAndScheduledRetry`：不能替换live Node；生命周期测试确认重启generation++、Workspace ID和数据不变 |
| credential_refs受控配置/归属 | `cloudctl -command credential-ref`、ConfigureCredential、复合 FK | `TestPostgresAggregateConstraints`：为成员配置引用后，另一owner使用返回404；OpenAPI/公开JSON无secretRef |
| 19公开+15内部接口的可执行契约 | `router.Routes`、`internal/contract/openapi.go`、`api/openapi.json` | `TestPublishedOpenAPIIsValidAndCurrent`验证合法性/同步；集成客户端对所有cloud响应运行OpenAPI验证，补测members/tenants/workspace list/rename/access等正向路径 |
| POST/DELETE 幂等，重试在版本校验前 | idempotency_records、Public事务 | `TestConcurrentIdempotencyAndLastAdminProtection`：16并发只创建一组资源/operation；`TestHTTPProjectLifecycleAndDurableRecovery`同键不同内容409；`TestTerminationUnknownRetryAndVersionedReplay`旧version原样重放原operation |
| 分页、稳定排序、仅改名和严格输入 | `page`、PATCH分支、router字段白名单/类型检查 | `TestListPaginationAndErrorShape`、`TestRemainingPublicContractsAndMembershipRevocation`：UUID游标、范围限制、错误结构、rename version；禁止repo修改、客户端宿主路径、URL密码 |
| 真实创建流程，Ready需要Node初始化 | 控制有限状态机、模拟磁盘Substrate/真实Git | `TestHTTPProjectLifecycleAndDurableRecovery`：真实commit、bare repo、main linked worktree、isolated/Task；提前advance/伪造success/错effect步骤被`TestOperationPreconditionsAndScheduledRetry`拒绝 |
| 创建与Project删除并发 | Project操作唯一+事务锁+lifecycle检查 | `TestProjectCreationDeletionSerializationAndStrictInputs`：恰一方接受，另一冲突，没有逃逸Workspace |
| 停止与执行并发，保护待交互 | execution_tickets、关闭准入、scoped Node idle | `TestStopAdmissionRaceAndIdleEvidence`：真实interaction阻止停止，admit/stop竞争恰一方成功；缺idle不能推进；Node拒绝恢复原准入；没有常量0绕过 |
| idle证据绑定正确操作和实例 | node_idle operationId+workspace集合+epoch/version | `TestWrongWorkspaceNodeCannotRefuseAnotherStop`：同Project旁支Node不能取消另一Workspace的stop；旧Node在生命周期测试中拒绝 |
| Project删除等待全部终止/维护清理 | quiesce→terminate→cleanup→storage_delete | `TestProjectDeleteClosesEveryWorkspaceAndWaitsForCleanup`：任一Workspace活动阻止整项删除，关闭后两者都不能准入；Git清理失败不调度storage_delete，所有引用保留；恢复后整项删除 |
| Lease数据库时间、epoch接管/fencing | lease/claim/operation/准入检查 | `TestControllerTakeoverReconcilesAndFences`：过期接管epoch++，旧推进/续租/释放/admit均409，存量sandbox只一份 |
| 外部成功但响应丢失，重建对象恢复 | external_effects、Substrate磁盘journal、Controller GET-before-PUT | `TestRestartRecreatesCloudSubstrateAndController`：关闭原HTTP和PG pool，重建Store/HTTP/Substrate/Controller，原ID恢复；Git和sandbox不重复创建；幂等响应仍在PG |
| 未确认终止/清理失败不误报成功 | defer/retry与受控推进、保留绑定 | `TestTerminationUnknownRetryAndVersionedReplay`：blocked仍保留live实例，start失败，显式retry后恢复；生命周期/Project删除测试：cleanup失败保留数据和operation |
| 定时重试、版本与迟到结果拒绝 | operation retry_at/version/epoch与效果绑定 | `TestOperationPreconditionsAndScheduledRetry`：未到期不领取，到期协调原effect；旧version/完成后迟到结果拒绝 |
| 跨HTTP权威写入不能错配关联 | PG复合FK、有限内部接口 | `TestDatabaseEffectAndTicketScopes`：直接SQL ticket指向其他Workspace Node、effect指向其他Project operation都失败；模拟Controller没有DB handle |

## 明确限制和后续验证

阶段一核心验收使用真实 PG/HTTP/磁盘/Git；模拟沙盒和 Node 没有真实进程隔离能力。以下没有通过本阶段验收，不能作为已完成能力宣传：

1. Rust Controller/Node、Deno/Agent/PTY执行、真实Node本地idle与启动执行锁，以及所有Session/Workflow活动接入。现在提供真实云端票据边界及模拟回归，不提供完整后续业务领域。
2. K8s/Substrate部署、真实异步维护Job终止、跨宿主RWX锁/原子操作、卷/容器部分挂载的Git metadata路径可移植性、storage fence在网络分区下阻止旧写入。
3. 华为登录实际SDK/协议、生产内部签发端与密钥分发/TLS、真实私有Git凭据注入。cloud内部验证与引用约束已实现，外部基础设施适配仍需落地。
4. Session历史、Workflow、Effect、插件归属与desktop历史导入。迁移约束已记录在 [execution-contract.md](execution-contract.md)，未改写/导入原桌面数据。
5. 性能压测与多租户Controller分片。当前全局短事务锁和每Project一个未完成operation是首版有意采用的限制。

Docker/远端CI未运行是本机环境验证边界；SQL迁移、HTTP/PG并发、恢复、race、本地模拟命令和Go构建没有遗留失败。
