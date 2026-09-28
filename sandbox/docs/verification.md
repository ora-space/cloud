# 验证证据

2026-09-28 在真实 Agent Substrate、gVisor Actor 和 DATA snapshot 上完成以下验证。测试结束后，测试 Actor 与 Tag 已删除；effect journal 保留用于恢复与审计。

| 场景 | 结果 |
| --- | --- |
| ensure | 返回 `sandboxInstanceId=effectId` 和稳定 `nodeId` |
| 同 ID、不同 payload | `409 effect_conflict` |
| 未知 effect GET | `404 not_found` |
| 非 WebSocket 请求访问 router | `426 websocket_upgrade_required` |
| 首代写入 `/workspace` | 成功 |
| terminate | Actor 删除，旧 router 目标不可访问 |
| terminate Tag | `SNAPSHOT_CONTENT_SCOPE_DATA` |
| 二代 ensure | 从 Tag 恢复成功 |
| Node 身份 | `nodeId` 不变，`incarnation_id` 改变 |
| 二代读取首代文件 | 成功 |
| 活动状态删除 Workspace | `503 workspace_active` 和完整 effect |
| 终止后重试同一 delete effect | `200`、`removed=true` |
| 删除数据后的新一代 | 原测试文件不存在 |
| 不支持的 `sandbox_suspend` | `400 unsupported_kind` |
| 跨 Workspace terminate | `409 scope_conflict` |
| Workspace 换绑其他 Project | `409 scope_conflict` |
| 第二个 ensure 先 busy，旧实例终止后重试 | 同一 effect 从 `503` 变为 `200`，router 握手成功 |
| 服务重启后查询历史 effect | 返回持久化的 succeeded 条目 |

仓库验证命令：

```bash
go test ./sandbox/...
go test -race ./sandbox/...
go vet ./sandbox/...
go build ./sandbox/service ./sandbox/runtime ./sandbox/examples/controller-demo
git diff --check
```

服务和 runtime 的测试覆盖请求校验、GET 只读语义、effect 幂等冲突、tombstone 路由拒绝、跨 Workspace terminate、Workspace Project 归属、输出上限、退出码和超时进程组清理。
