# gitpreset：Git Server 契约的验收预置

[中文](README.md) | [English](README.en.md)

`cmd/git-server-preset` 的实现，只用于开发与验收。它替代外部线（快照、Operation）或内部线（Workspace 分支），
直接写仓库卷与 Cloud 数据库，刻意绕过 Git Server 的授权，因此不进生产镜像，也不被任何服务进程引用。

| 文件 | 职责 |
|---|---|
| `preset.go` | `Snapshot`（确定性的合成历史或已有提交 → 快照 ref、跟踪 ref、`succeeded` 同步与当前快照，一个事务登记）、`Ref`、`Operation` |
| `check.go` | `Check`：ref 都在契约 namespace 内并指向提交；上游 ref 属于本 Project 的 Binding；快照 ref 名称等于目标；跟踪 ref 指向已有快照；至多一个未结束同步；当前快照来自本 Binding 的成功同步，且 Git Server 上的快照 ref 指向记录的提交 |

用法错误返回 `UsageError`，契约问题以 `violations` 返回，二者都与命令无法执行（数据库、git 故障）区分开。
测试用真实 PostgreSQL（`TEST_DATABASE_URL`）与真实 git。
