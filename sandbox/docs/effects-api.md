# 生命周期 Effects 与 WebSocket 路由

## Effects API

Controller 配置 `effects_url`，并对 Cloud 分配的 UUID 调用：

| 方法 | 路径 | 语义 |
| --- | --- | --- |
| `GET` | `/effects/{effectId}` | 查询已经持久化的 intent；不会启动新动作 |
| `PUT` | `/effects/{effectId}` | 首次持久化 intent 后执行；相同请求可重试 |

请求：

```json
{"kind":"sandbox_ensure","projectId":"<project-uuid>","workspaceId":"<workspace-uuid>"}
{"kind":"sandbox_terminate","projectId":"<project-uuid>","workspaceId":"<workspace-uuid>","sandboxInstanceId":"<ensure-effect-id>"}
{"kind":"workspace_data_delete","projectId":"<project-uuid>","workspaceId":"<workspace-uuid>"}
```

ensure 成功：

```json
{
  "id": "<ensure-effect-id>",
  "externalId": "<ensure-effect-id>",
  "state": "succeeded",
  "request": {
    "kind": "sandbox_ensure",
    "projectId": "<project-uuid>",
    "workspaceId": "<workspace-uuid>"
  },
  "result": {
    "sandboxInstanceId": "<ensure-effect-id>",
    "nodeId": "workspace-<workspace-uuid>"
  }
}
```

terminate 与数据删除成功结果分别是 `{"terminated":true}` 和 `{"removed":true}`。

`state` 只能是 `running`、`succeeded`、`failed`。已经受理但执行失败时，服务返回 `503` 和完整 effect 条目；`GET` 仍以 `200` 返回这个条目。成功条目不会重新执行外部副作用。

## 状态码

| HTTP | 含义 |
| --- | --- |
| `200` | 查询或执行成功 |
| `400` | effect ID、JSON、字段、scope 格式或 kind 错误 |
| `404` | 确定从未收到该 effect，或 terminate 目标不是已知 ensure |
| `409` | effect payload 改变，或 Project/Workspace 作用域冲突 |
| `503` | intent 已受理，但平台动作失败；响应包含完整 effect |

## Journal 与恢复

journal 默认位于 `/opt/substrate-poc/ora-sandbox-service/state/journal.json`。写入顺序是临时文件、文件 `fsync`、原子 rename、目录 `fsync`。

`GET` 只允许通过读取 Substrate 状态对 `running` effect 对账，不创建、暂停或删除 Actor。失败的同 ID、同请求 `PUT` 可以再次执行。ensure 重试会重新检查 Workspace 占用；terminate 重试沿用 tombstone 和 pending Tag。

Workspace 数据删除后，历史 effect 仍保留 Project 归属，防止同一个 Workspace ID 被另一个 Project 重新绑定。

## Terminate 顺序

```text
persist terminate intent + tombstone
  → deny old router target
  → suspend Actor
  → wait for suspended
  → create generation-specific DATA Tag
  → wait for Tag snapshot
  → delete Actor
  → publish current Workspace Tag
  → mark effect succeeded
```

无法确认终止或 Tag 时返回失败，不能安全分配下一代 sandbox。

## WebSocket Router

Controller 连接：

```http
GET /ora-node/v1 HTTP/1.1
Connection: Upgrade
Upgrade: websocket
Ate-Target-Actor: <atespace>/<sandboxInstanceId>
```

router 验证 effect、当前活动 sandbox、tombstone 与 Actor 状态，再建立上游 WebSocket。

| HTTP | 含义 |
| --- | --- |
| `400` | `Ate-Target-Actor` 缺失或格式错误 |
| `404` | sandbox 不存在、已终止或不是当前活动实例 |
| `503` | sandbox 已知但暂时不可路由 |
| `502` / `504` | Actor 发现或 Node 连接失败、超时 |
| `426` | 请求没有执行 WebSocket Upgrade |

每个方向使用独立连接并施加 16 MiB read limit。gorilla/websocket 在每段处理 ping/pong，阻塞写入提供背压，close code 与 reason 会转发到另一侧。router 不解析或修改 ORA frame。

## Ora Node 握手

每条 ORA frame 放在一条 binary WebSocket message 中：

```text
4-byte big-endian length | 0x01 | UTF-8 JSON envelope
```

第一条消息必须是协议 v1 `hello`。`controller_id` 必须与 service 配置一致。`hello_accepted` 返回稳定 `node_id`、当前 `incarnation_id` 和 capability。Node 的持久账本位于 `/workspace/.ora-node/state.json`。
