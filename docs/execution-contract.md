# Substrate、Node 和阶段二契约

阶段一交付 cloud 核心及模拟组件。`internal/simulator` 的 Controller 没有数据库 handle，通过真实 HTTP 领取/推进；Substrate 在磁盘保存 effect journal，Git 用真实命令；clone 步骤通过 loopback gRPC 登记 execution；Node 通过 scoped 签名凭据模拟注册、初始化、idle 和结束票据。没有真实 Pod、Agent、Deno 插件、PTY、跨宿主卷或 Rust 进程。生产 Controller 通过 gRPC `NodeReportService` 报告 Node（NodeId + NodeIncarnationId），模拟器仍使用 Node 凭据 JSON 路径。

## Workspace 数据与 Node clone

Workspace 运行时跟随 desktop Node（specs `decisions/cloud/operation/0-workspace-runtime-follows-desktop-node.md`）：每个 Workspace 有自己的持久数据，被该 Workspace 的每一代 sandbox 挂载为 Node home；stop 保留，只有 `workspace_data_delete` 删除。Cloud 不再维护 Project 共享卷、bare repository、linked worktree 或维护 Job，也不生成 `ora/{workspace UUID}` 分支；`project_storage` 与 `workspace_worktrees` 只作为历史保留，数据库拒绝新行。

create_project / create_workspace 依次经过 sandbox → node → clone：sandbox_ensure 返回 `nodeId`，Controller 报告的 Node 必须带相同 NodeId；clone 步骤由 Controller 先通过 gRPC `ExecutionService.RecordDispatch` 登记一次 execution（输入固定为 Project 仓库与 Workspace `requestedRef`，目标为当前 Node），再交给 Node 执行，最后用 `RecordQueriedResult` 登记 Node 报告的结果。成功结果中的 commit 写入 Workspace `baseCommitId` 后才开放准入。失败的 execution 保留，operation 进入 retry_wait，重试登记新的 execution；结果未知的 execution 阻止再次登记，operation 只能 blocked，不会自动重试。start 只到 node，不重新 clone。

Node 不能自选路径、分支或仓库；克隆的 Git 凭据仍是基础设施引用，Cloud 不保存密钥值。

## Substrate 最小接口

下面是模拟器实际实现的 HTTP 形式，生产适配器可以映射到其控制 API，但必须保持语义：

| 方法 | 请求 | 返回与不变量 |
|---|---|---|
| GET `/effects/{effectId}` | cloud 事先分配的 UUID | 404=确知尚无该 intent；否则返回原 request、externalId、state、result，查询不会创建 |
| PUT `/effects/{effectId}` | kind、projectId、workspaceId、sandboxInstanceId? | 首次先落 journal 再执行；同 ID 同 payload 幂等，同 ID 不同 payload 409；成功后原结果保留 |
| PUT/GET `/clones/{executionId}` | 仅模拟器：模拟 Node 执行 clone；workspaceId、repositoryUrl、branch | 同 execution 只执行一次并落 journal；返回 clone_ready+commitId 或 clone_failed+reason |

kind 支持 sandbox_ensure（返回 sandboxInstanceId 与 nodeId）、sandbox_terminate（返回 terminated）、workspace_data_delete（返回 removed）以及插件 effect。storage/worktree 系列 kind 已退役，Cloud 拒绝再计划。模拟器只接受显式 repository URL→本地 fixture 映射，不接入真实私有 Git 凭据。cloud 内部的 snapshot 才向受控 Controller 提供本 Project 的 credential reference；基础设施负责解析引用、注入 Git 凭据、审计和轮换，Cloud 从不保存密钥值。

生产 Substrate 应用独立服务身份与受控网络保护这些接口，并验证 Project/Workspace/effect scope；模拟 HTTP handler 仅供 loopback 测试，不能作为生产公共端点发布。幂等 ensure 必须能查询“执行成功但响应丢失”的实际对象，不能以调用方超时判定不存在。terminate 返回确认旧进程不会再访问存储的证据；不确定就 blocked，不分配新 generation。

模拟器是同步有限 Job，持久状态为 running/succeeded/failed；全局 mutex 使重入同任务串行，重新构建 handler 后按 journal 与磁盘/Git 状态协调。真实异步 Job 必须提供查询、终止请求与终止确认，Controller 接管先协调存量 Job，不能把“旧 controller 失租”等同于 Job 已终止。

## 阶段二必须实际验证

- 每个 Workspace 数据卷在 sandbox 替换时的挂载与跨宿主迁移；desktop Node 的 clone 策略（例如对 `HEAD` 的处理）与 Cloud `requestedRef` 的对齐。
- 旧 sandbox 的终止或 storage fence，在网络分区与 Controller 接管下仍阻止旧文件写入。
- 真实 Node 本地准入/idle 原子性、全部执行类型覆盖、Node 重启和 token 刷新、Agent/Deno/PTY 子进程归属。
- 长 Job 期间每 10 秒续租和失租立即停止后续调度；未知 prompt 结果不自动重放。
- Gateway 的华为登录集成、内部凭据签发和生产证书/密钥分发。

## 后续业务数据迁移

Session 仍属于 Workspace，不将 sandboxId/nodeId 作为持久业务身份。完整 Session/Workflow Run、Effect、插件配置/安装状态、历史持久化和 desktop 迁移不在本阶段实现。未来表需继承 tenant+owner+Project+Workspace 约束，并由 cloud 保存权威状态；Controller 不得自行建立 PG 旁路。

desktop bootstrap 当前耦合 SQLite、Session JSONL、插件与 Workflow，阶段二拆出接口适配。历史 JSONL 追加顺序不等同展示 position；重复 position 表示修正，迁移必须保留最后修正、Gap、受损尾行和 pending tool 语义，不能直接按 append 顺序赋展示 seq。先定义快照/增量边界、幂等导入键与所有权映射，再导入历史；本次未导入任何桌面数据。

Effect Scope、Desired State/Generation、插件 canonical identity、Workflow 状态与运行结果必须各自有明确 cloud 持久化归属与版本契约；不要把这些塞进阶段一 operations.result 的任意 JSON 中。当前 Task 仅为 isolated Workspace 一对一展示身份，execution_tickets 是并发准入证据，不是完整 Session/Workflow 领域替代品。
