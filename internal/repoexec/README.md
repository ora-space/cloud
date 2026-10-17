# repoexec：Repo Executor

[中文](README.md) | [English](README.en.md)

Ora 中唯一访问外部 Git 仓库的程序（`proposed` 的 ADR `cloud/repo-executor/0`，实现 Git Server 代码中心契约中执行器一方的义务）。
无状态：不连数据库，每个任务在私有临时目录里新建裸仓库，结束后删除任务目录与凭据材料。只报告观察到的事实，Operation 状态由 Cloud 决定。

| 文件 | 职责 |
|---|---|
| `task.go` | 任务请求 `Task`、结果 `Result`（JSON 与设计文档一致）、分类与错误码 |
| `authorize.go` | URL 检查、`cloneauth.Verify`、任务与授权逐字段比对（含用途，probe 授权不能驱动 publish）、按用途的超时上限 |
| `server.go` | mTLS 任务监听 `POST /v1/tasks`：只接受唯一 URI SAN 为 `spiffe://ora.local/cloud/repo-dispatcher` 的客户端；请求体 ≤ 64 KiB 严格解码；槽位满时 `429 executor_busy`，任务不开始 |
| `executor.go` | 一个任务的生命周期：验证 → 出口检查 → 任务目录与大小监控 → 兑换凭据（仅当有 `credentialId`）→ 写入材料 → 执行 → 销毁材料与目录（任何路径，含 panic；失败置 `cleanupFailed`） |
| `git.go` | 从空环境构造的 Git 环境、加固配置（ADR D4 单一列表）、只作用于 Git Server URL 的证书 / CA / `Ora-Git-Operation-Id`、进程组隔离、有界捕获、先脱敏再截断到 2 KiB |
| `sync.go` | ADR D5：可选的跟踪 ref 基础 fetch、加固外部 fetch、`forced` 计算、一次不带 `+` 的 `--atomic --force-with-lease` 推送；`stale info` 与 hook 拒绝分开处理 |
| `publish.go` | ADR D6：从 Git Server 取源、只快进、前置 `ls-remote`、lease 推送、在独立窗口里做后置 `ls-remote` |
| `probe.go` | ADR D7：`upload-pack` 用 `ls-remote`；`receive-pack` 用 Go HTTP 读取第一个 pkt-line 即断开 |
| `classify.go` | porcelain 与 `ls-remote` 解析、stderr → 错误码、ADR D8 发布组合表 |
| `egress.go` | 出口策略：仅 https、端口 443、拒绝回环 / 私有 / CGNAT / 链路本地 / ULA / 组播 / 元数据地址、内部主机名和无点主机名；允许列表（fixture）整体放行；拨号时再校验 |
| `workdir.go` | 工作根目录布局（`tasks/`、`credentials/`）、启动清扫、每 250ms 的大小监控 |
| `client.go` | Cloud 侧 `Client.Run`：`NotDeliveredError`（请求未写完，任务肯定没开始）、`LostError`（已发送但没有可读结果）、`RefusedError`（429/403/400，任务没开始）；不设短于任务自身的超时，不重试 |

Git Server 访问：仓库 `<GIT_SERVER_URL>/<tenantId>/<projectId>.git`；`http.<GIT_SERVER_URL>/.sslCert|sslKey|sslCAInfo|extraHeader`
按 URL 前缀生效，外部远端（不同的 scheme/host/port）拿不到执行器证书和 Operation 头；出口策略还总是拒绝 Git Server 与
Credential Store 的主机名。外部凭据只经 `git-credential-ora <材料目录>`，作用域是规范化后的仓库 URL，任务中的仓库地址按用户输入原样使用。

分类：同步 `applied`（含重放）、`superseded`（lease `stale info`，附 `observedOid`；若跟踪 ref 已等于本提交且快照存在则 `applied`）、
`failed(git_server_rejected)`（Git Server 的 403 或 pre-receive 拒绝）；发布按 ADR D8 表；probe 读到广告为 `applied`。
`uncertain` 之后执行器从不重推。

测试使用真实 git：真实 `gitserver` 服务仓库监听（策略替身 + 测试二进制作为 pre-receive hook）和带 TLS 的外部 smart HTTP
fixture（`git http-backend`，可要求 basic auth，模式 `moved`、`drop`、`hang`、`hang-read`、`redirect`，以及 pre-receive 拒绝）。
每个任务后断言工作根为空、结果与日志中不含秘密。运行 `go test -race ./internal/repoexec/...`。
