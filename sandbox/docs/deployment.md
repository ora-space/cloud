# 构建与部署

## 前提

- Go 1.27.1
- 可用的 Agent Substrate 集群、Atespace、WorkerPool 和 `kubectl-ate`
- Actor 到 snapshot storage 的访问
- gVisor sandbox class
- 受控的 atenet ingress

## 构建

```bash
go build -o /opt/substrate-poc/bin/ora-sandbox-service ./sandbox/service
go build -o /opt/substrate-poc/bin/ora-controller-demo ./sandbox/examples/controller-demo
docker build -f sandbox/runtime/Containerfile -t ora-sandbox-runtime:dev .
```

将运行时镜像推送到 Worker 可访问的 registry，把 `deploy/ora-actor-template.json` 中的镜像引用改为不可变 digest，然后创建 ActorTemplate。

模板中的 `/workspace` 使用 `durableDir`，`onPause` 与 `onCommit` 必须保持 `SNAPSHOT_CONTENT_SCOPE_DATA`。这样 Tag 保留 Workspace 文件，不恢复旧进程内存。

## 安装服务

```bash
install -m 0755 ./bin/ora-sandbox-service /opt/substrate-poc/bin/ora-sandbox-service
install -m 0644 sandbox/deploy/ora-sandbox-service.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now ora-sandbox-service
```

| 参数 | 默认值 |
| --- | --- |
| `-atespace` | `ate-coding-poc` |
| `-template` | `ora-coding-v3` |
| `-controller-id` | `ora-cloud-controller` |
| `-lifecycle-addr` | `127.0.0.1:18002` |
| `-router-addr` | `127.0.0.1:18001` |
| `-internal-router` | `ws://127.0.0.1:18000` |
| `-state` | `/opt/substrate-poc/ora-sandbox-service/state/journal.json` |

## 健康与日志

```bash
systemctl status ora-sandbox-service
journalctl -u ora-sandbox-service -f
ss -lntp | grep -E '18000|18001|18002'
```

查询 Substrate：

```bash
kubectl-ate --kubeconfig <path> --context <context> \
  get actor <sandbox-id> -a <atespace> -o json

kubectl-ate --kubeconfig <path> --context <context> \
  get tag -a <atespace> -o json
```

## 最小演示

```bash
PROJECT_ID=11111111-1111-4111-8111-111111111111
WORKSPACE_ID=22222222-2222-4222-8222-222222222222
ENSURE_ID=33333333-3333-4333-8333-333333333333

curl --noproxy '*' -X PUT \
  -H 'Content-Type: application/json' \
  --data "{\"kind\":\"sandbox_ensure\",\"projectId\":\"$PROJECT_ID\",\"workspaceId\":\"$WORKSPACE_ID\"}" \
  "http://127.0.0.1:18002/effects/$ENSURE_ID"

./bin/ora-controller-demo "$ENSURE_ID" status
```

结束后使用新的 effect ID 调用 `sandbox_terminate`，再调用 `workspace_data_delete`。

## 生产差距

- 用 ate-api gRPC client 代替 `kubectl-ate` 子进程。
- 用 PostgreSQL 或同等级事务存储代替单机 JSON journal。
- 多副本部署按 Workspace 使用分布式锁和 fencing。
- 入口增加 mTLS 或工作负载身份、Project/Workspace 授权和审计。
- router 增加连接数、空闲时间、总时长和主动 ping 策略。
- 自动检测 Worker 重注册和 WorkerAssignment 地址漂移。
- 内部 atenet ingress 与 Controller 网络完全隔离。
