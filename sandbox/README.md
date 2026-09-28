# Agent Substrate 沙盒适配器

该目录提供 Ora Cloud `docs/execution-contract.md` 中 Substrate 最小接口的 Agent Substrate 实现。它包含生命周期 effect 服务、受控 Ora Node WebSocket 路由、沙盒内运行时、部署清单和协议演示程序。

当前实现用于单集群 POC 和集成验证。服务默认只监听 loopback，并通过 `kubectl-ate` 调用 Agent Substrate。生产部署需要在入口增加服务身份认证、TLS、Project/Workspace 授权，并把单机 journal 替换为事务型持久化。

## 目录

| 路径 | 用途 |
| --- | --- |
| `service/` | `GET/PUT /effects/{effectId}` 和 `/ora-node/v1` WebSocket 路由 |
| `runtime/` | Actor 中的命令执行入口与 Ora Node v1 协议 |
| `deploy/` | ActorTemplate 与 systemd 示例 |
| `examples/controller-demo/` | 最小二进制 WebSocket Controller |
| `docs/effects-api.md` | effect、幂等、终止与恢复语义 |
| `docs/deployment.md` | 构建、部署、运行和日志 |
| `docs/verification.md` | 已完成的真实 Actor 验证证据 |

## 架构

```text
Ora Controller
  ├─ HTTP effects ───────▶ lifecycle :18002 ─┐
  └─ WebSocket binary ──▶ router :18001 ─────┤ sandbox/service
                                              └─▶ atenet ingress :18000
                                                    └─▶ Actor / Ora Node :80
```

Controller 只能连接受控 router。router 根据 `Ate-Target-Actor: <atespace>/<sandboxInstanceId>` 查询本地 effect journal 和 Substrate Actor，在接受下游 WebSocket Upgrade 前先建立上游连接。router 不解析 ORA 应用帧，只保留消息类型与内容。

## 关键不变量

- `effectId` 是 Cloud 分配的 UUID，也是幂等键。
- intent 必须在任何外部动作前原子持久化。
- 同 ID、同请求可以重试；同 ID、不同请求返回 `409`。
- 同一 Workspace 最多有一个未终止 sandbox。
- terminate 先持久化 tombstone，再暂停、建立 DATA Tag 和删除 Actor。
- tombstone 生效后，旧 sandbox 不能重新创建，也不能通过 router 访问 Workspace。
- `/workspace` 数据跨 sandbox 代次保留，直到 `workspace_data_delete`。
- 一个 Workspace 的 `nodeId` 稳定；每个新 Node 进程生成新的 `incarnation_id`。
- Node WebSocket 使用 binary message，单条消息最大 16 MiB。
- lifecycle 接口不接受 Git 密钥；Cloud 只管理基础设施 credential reference。

## 构建与测试

从仓库根目录执行：

```bash
go test ./sandbox/...
go test -race ./sandbox/...
go vet ./sandbox/...

go build -o ./bin/ora-sandbox-service ./sandbox/service
go build -o ./bin/ora-sandbox-runtime ./sandbox/runtime
go build -o ./bin/ora-controller-demo ./sandbox/examples/controller-demo
```

构建运行时镜像：

```bash
docker build -f sandbox/runtime/Containerfile -t ora-sandbox-runtime:dev .
```

## 接口

```text
GET /effects/{effectId}
PUT /effects/{effectId}
GET /ora-node/v1   # WebSocket Upgrade
```

首版支持 `sandbox_ensure`、`sandbox_terminate` 和 `workspace_data_delete`。Controller 生命周期 API 不包含 suspend/resume。Substrate 内部可以在 terminate 流程中使用 suspend 生成一致的 DATA snapshot。

详细请求、响应和恢复规则见 [effects-api.md](docs/effects-api.md)，部署步骤见 [deployment.md](docs/deployment.md)。

## 安全边界

默认配置中的 `127.0.0.1` 是 POC 的网络边界，不能作为生产鉴权。生产入口必须验证调用者身份与 Project、Workspace、effect 作用域，并将内部 atenet ingress 与 Controller 网络隔离。日志不得记录 token、Git 凭据或 kubeconfig 内容。
