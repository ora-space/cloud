# cmd/server：Cloud 服务装配

[中文](README.md) | [English](README.en.md)

生产服务入口，把配置、数据库、HTTP/gRPC 适配器与生命周期循环装配到同一个 Store。业务状态和策略位于 Core。

## 启动与服务

- 加载 YAML / `CLOUD_*` 配置、日志和 PostgreSQL pool；`CheckSchema` 拒绝缺失、未知或 checksum 不符的迁移，不执行 DDL。
- 读取部署信任材料，构造 Authenticator、对象存储和外部服务客户端。
- `router.New` 提供公开 HTTP；`controlgrpc.NewSecure` 提供 Controller 双向 TLS gRPC。旧管理 JSON 路径保持退役。
- `configureCollaboration` 在生产组合真实 Space Agent 和 Workflow 目录；只有显式开启开发开关才装入 collab 夹具。
- `wireRepository` 按配置接入 Git ref 查询、Credential Store 和 Repo Executor worker；网络 I/O 不在 Core 事务中。

## 后台任务与停机

`main.go` 用同一个取消 context 与 WaitGroup 拥有循环：可选市场同步和工作流资产导入、Agent 派发/启动、blocked 窗口、Thread 结束/取消、交付/释放恢复，以及 Workflow 调度。具体状态判定仍由各 Core 方法负责；循环不把进程内状态当作权威。

残留账本恢复循环调用 `ReconcileAgentRunResidualsOnce`，启动即运行一次，之后按 `issue_runs.residual_reconcile_interval`（缺省 `1m`）重试，并参与同一 context 的取消与 WaitGroup 等待。它在业务运行结束后继续承担恢复责任，只消费既有事实入口，不新增 Controller 发现 RPC 或契约；证据收不到就保留 open，不按超时、重连或强停推定清理成功。

停机与失败路径的清理范围不同：

- 信号退出分支（SIGINT/SIGTERM）：取消后台任务，发送 Controller drain 信号；gRPC 先优雅停止，到达与 HTTP 共用的 10 秒停机 deadline 时强制停止，然后调用 HTTP Shutdown，等待后台循环结束，再关闭数据库和刷新日志。
- 服务失败分支：取消后台任务并等待循环结束后返回，再关闭数据库和刷新日志；该分支不调用 HTTP Shutdown 或 gRPC Stop/GracefulStop，不保证排空另一个服务的在途请求。
- 启动失败：后台循环启动后，`wireRepository`、`net.Listen`、`Control.TLS` 或 `NewSecure` 出错会直接返回，不等待后台循环。已注册的 defer 按逆序执行，数据库关闭先于 context 取消；不能据此保证已启动任务完成清理。

配置与运维见 [部署](../../docs/operations/deployment.md) 和 [配置参考](../../docs/operations/configuration.md)；执行语义见 [IssueRun](../../docs/agent-runs/issue-runs.md)、[工作流](../../docs/workflows/workflows.md) 和 [外部 Git 线](../../docs/repositories/external-git.md)。本目录测试验证装配边界。
