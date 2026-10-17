# git-server-preset：Git Server 契约的预置与断言工具

[中文](README.md) | [English](README.en.md)

只用于验收，随开发镜像（`devsetup`）进入 cluster 开发栈，不进生产镜像。它直接写 Git Server 的仓库卷和 Cloud
数据库，绕过 Git Server 的授权，让内部线和外部线各自用它替代对方，独立做端到端验收（`proposed` 的后续决策
“Git Server 成为每个 Project 的代码中心”，落地顺序第 1 步）。逻辑在 [`internal/gitpreset`](../../internal/gitpreset/README.md)，
本命令只负责参数、依赖装配和退出码。

| 子命令 | 作用 |
|---|---|
| `snapshot --project <id> (--commit <oid> \| --synthetic <n>)` | 写入 `snapshots/<oid>` 与跟踪 ref，把该 Binding 未结束的同步（例如建 Project 时的那一行）推进为 `succeeded`，没有时登记一行新的 `succeeded` 同步，并推进当前快照；`--synthetic` 先生成有 n 个提交的确定性历史 |
| `ref --project <id> --ref <name> --commit <oid>` | 写入任意 namespace 的一个 ref（例如作为发布源的 Workspace 分支）；提交必须已在仓库中 |
| `operation --project <id> --type sync\|publish --state <state> [--error-code <code>]` | 写入一行给定状态的 Operation，用于等待、失败、删除等场景；终态写 `finished_at`。同步改写该 Binding 未结束的同步（例如建 Project 时的那一行）为给定状态，没有时才新写一行，因此任何同步状态都能预置；发布总是新写一行 |
| `check --project <id>` | 契约断言：Git Server 上的 ref 与 Cloud 记录是否符合契约 |

每次输出一行 JSON：成功为 `{"ok": true, ...}`；`check` 发现问题为 `{"ok": false, "violations": [...]}`；命令失败为
`{"ok": false, "error": "..."}`。退出码：成功 0，断言失败或命令失败 1，用法错误 2。

数据库来自 Cloud 配置（`-config`，或 `CLOUD_DATABASE_DSN`），并要求迁移已应用；仓库卷与 git 程序沿用 Git Server 的
`GIT_SERVER_REPOSITORY_ROOT` 与 `GIT_SERVER_GIT_PROGRAM`。仓库不存在时按 Git Server 的原子方式建仓。
