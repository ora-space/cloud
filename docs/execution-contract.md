# Substrate、Node 和阶段二契约

阶段一交付 cloud 核心及模拟组件。`internal/simulator` 的 Controller 没有数据库 handle，通过真实 HTTP 领取/推进；Substrate 在磁盘保存 effect journal，Git 用真实命令；clone 步骤通过 loopback gRPC 登记 execution；Node 通过 scoped 签名凭据模拟注册、初始化、idle 和结束票据。没有真实 Pod、Agent、Deno 插件、PTY、跨宿主卷或 Rust 进程。生产 Controller 通过 gRPC `NodeReportService` 报告 Node（NodeId + NodeIncarnationId），模拟器仍使用 Node 凭据 JSON 路径。

## Workspace 数据与 Node clone

Workspace 运行时跟随 desktop Node（specs `decisions/cloud/operation/0-workspace-runtime-follows-desktop-node.md`）：每个 Workspace 有自己的持久数据，被该 Workspace 的每一代 sandbox 挂载为 Node home；stop 保留，只有 `workspace_data_delete` 删除。Cloud 不再维护 Project 共享卷、bare repository、linked worktree 或维护 Job，也不生成 `ora/{workspace UUID}` 分支；`project_storage` 与 `workspace_worktrees` 只作为历史保留，数据库拒绝新行。

create_project / create_workspace 依次经过 sandbox → node → clone → plugin：sandbox_ensure 返回 `nodeId`，Controller 报告的 Node 必须带相同 NodeId；clone 的输入由 Cloud 在推进到 clone 步骤（以及领取）的事务里，从 Project 的 Binding 当前快照冻结进 `operations.request.clone`：`{kind:"clone", repositoryUrl, snapshotRef, baseCommit, workspaceRef}`，其中 `repositoryUrl` 为 `git_server.repository_base_url` + `/<tenantId>/<projectId>.git`，`snapshotRef` 为 `refs/ora/upstreams/<bindingId>/snapshots/<commit>`，`workspaceRef` 为 `refs/heads/workspaces/<workspaceId>/<requestedRef>`；每个 operation 只冻结一次，重试沿用。Controller 领取到的 snapshot 以 `cloneInput`（gRPC `OperationSnapshot.clone_input`）带出这份输入，先通过 gRPC `ExecutionService.RecordDispatch` 原样登记一次 execution（目标为当前 Node，输入不一致为 ABORTED），再交给 Node 执行，最后用 `RecordQueriedResult` 登记 Node 报告的结果。成功结果的 commit 必须等于输入的 `baseCommit`，推进时再核对一次后写入 Workspace `baseCommitId`，固定插件集合为空或 Node 全部逐项结果已接管后才开放准入。失败的 execution 保留，operation 进入 retry_wait，重试登记新的 execution；结果未知的 execution 阻止再次登记，operation 只能 blocked，不会自动重试。start 经过 node 与 plugin，不重新 clone。插件安装由 Node execution 执行，单项失败保存在实例，不阻止 Workspace ready。

没有可用快照时不派发 clone，也不消耗重试次数：快照与 Binding 最近一次同步在同一条语句里读取；最近一次同步尚未结束时 operation 进入 `retry_wait`（`snapshot_not_ready`，10 秒后重试）；最近一次同步以失败、被取代或取消结束时 operation `failed`（`repository_sync_failed`，`operations.result` 为 `{syncOperationId, syncErrorCode}`，公开的 operation 只返回 `syncErrorCode`），失败的 create_project 保留 `active` 的 Project；未配置 Git Server 时 `blocked`（`git_server_unconfigured`）。领取时遇到这类 operation 先停放，同一次领取继续找下一个可领取的。clone 从不回退到外部仓库，Cloud 也不在这里触发同步；创建 run Workspace 不同步。Controller 从推进得到非 running 的 operation（已停放或失败）时停止驱动它。

Node 不能自选路径、分支或仓库，只与 Git Server 通信；仓库凭据不控制任何 Node 工作（clone、session、交付）。

## Substrate 最小接口

下面是模拟器实际实现的 HTTP 形式，生产适配器可以映射到其控制 API，但必须保持语义：

| 方法 | 请求 | 返回与不变量 |
|---|---|---|
| GET `/effects/{effectId}` | cloud 事先分配的 UUID | 404=确知尚无该 intent；否则返回原 request、externalId、state、result，查询不会创建 |
| PUT `/effects/{effectId}` | kind、projectId、workspaceId、sandboxInstanceId? | 首次先落 journal 再执行；同 ID 同 payload 幂等，同 ID 不同 payload 409；成功后原结果保留 |
| PUT/GET `/clones/{executionId}` | 仅模拟器：模拟 Node 执行 clone；workspaceId、repositoryUrl、snapshotRef、baseCommit、workspaceRef | 同 execution 只执行一次并落 journal；从 Git Server 替身取快照 ref，返回 clone_ready+commit 或 clone_failed+reason |

kind 支持 sandbox_ensure（返回 sandboxInstanceId 与 nodeId）、sandbox_terminate（返回 terminated）、workspace_data_delete（返回 removed）。storage/worktree 与插件 effect 已退役，Cloud 拒绝再计划或接管插件 effect 结果。模拟器的 Node 只从 Git Server 替身（按 Git Server 卷布局存放的磁盘仓库）取快照，不接入外部仓库或 Git 凭据。cloud 内部的 snapshot 中 Project 只有 `id`，不带仓库 URL、默认分支或 credential reference。Cloud 不读取远端仓库：创建 Project 必须给出可作为 ref 的具体分支名 `defaultBranch`（否则 `400 invalid_ref`），它就是 Binding 分支；Workspace 的 `requestedRef` 等于它，是 Workspace 在 Git Server 上分支的短名，也是 checkout 的本地分支。isolated Workspace 的 `baseRef` 只接受省略、空、`HEAD` 或该分支，其他值 `400 base_ref_not_synced` 且不登记任何东西。

生产 Substrate 应用独立服务身份与受控网络保护这些接口，并验证 Project/Workspace/effect scope；模拟 HTTP handler 仅供 loopback 测试，不能作为生产公共端点发布。幂等 ensure 必须能查询“执行成功但响应丢失”的实际对象，不能以调用方超时判定不存在。terminate 返回确认旧进程不会再访问存储的证据；不确定就 blocked，不分配新 generation。

模拟器是同步有限 Job，持久状态为 running/succeeded/failed；全局 mutex 使重入同任务串行，重新构建 handler 后按 journal 与磁盘/Git 状态协调。真实异步 Job 必须提供查询、终止请求与终止确认，Controller 接管先协调存量 Job，不能把“旧 controller 失租”等同于 Job 已终止。

## 阶段二必须实际验证

- 每个 Workspace 数据卷在 sandbox 替换时的挂载与跨宿主迁移。
- 旧 sandbox 的终止或 storage fence，在网络分区与 Controller 接管下仍阻止旧文件写入。
- 真实 Node 本地准入/idle 原子性、全部执行类型覆盖、Node 重启和 token 刷新、Agent/Deno/PTY 子进程归属。
- 长 Job 期间每 10 秒续租和失租立即停止后续调度；未知 prompt 结果不自动重放。
- Gateway 的华为登录集成、内部凭据签发和生产证书/密钥分发。

## 后续业务数据迁移

Session 仍属于 Workspace，不将 sandboxId/nodeId 作为持久业务身份。完整 Session/Workflow Run、Effect、插件配置/安装状态、历史持久化和 desktop 迁移不在本阶段实现。未来表需继承 tenant+owner+Project+Workspace 约束，并由 cloud 保存权威状态；Controller 不得自行建立 PG 旁路。

desktop bootstrap 当前耦合 SQLite、Session JSONL、插件与 Workflow，阶段二拆出接口适配。历史 JSONL 追加顺序不等同展示 position；重复 position 表示修正，迁移必须保留最后修正、Gap、受损尾行和 pending tool 语义，不能直接按 append 顺序赋展示 seq。先定义快照/增量边界、幂等导入键与所有权映射，再导入历史；本次未导入任何桌面数据。

Effect Scope、Desired State/Generation、插件 canonical identity、Workflow 状态与运行结果必须各自有明确 cloud 持久化归属与版本契约；不要把这些塞进阶段一 operations.result 的任意 JSON 中。当前 Task 仅为 isolated Workspace 一对一展示身份，execution_tickets 是并发准入证据，不是完整 Session/Workflow 领域替代品。

## 运行时执行围栏

[多人控制](runtime-control.md) 新增页面控制绑定、独立 Node operation ID、短期许可及独立强停。派发前重新核验许可；Node 在持久接受和真实首次变更前拒绝旧绑定，未知响应仍按稳定 execution ID 核对。Controller 租约到期不能由缓存许可延长。生产旧无 scope 新 clone 准入关闭；历史查询、结果及清理责任保留。
