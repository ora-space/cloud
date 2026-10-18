# Ora Cloud

[English](README.en.md) | 中文

Ora Cloud 是 Ora 多人协作的权威服务端：它保存租户、成员、Project、运行时 Workspace、Issue、Agent 运行、插件和工作流的业务状态，
通过 Gateway 向浏览器提供 HTTP API，通过 mTLS gRPC 向 Controller 提供内部控制契约。PostgreSQL 是唯一的权威存储，所有决定在短事务里做出，
外部副作用先持久化意图再执行。系统全貌见 [架构总览](docs/architecture.md)。

## 进程

| 进程 | 入口 | 职责 |
|---|---|---|
| Cloud server | [`cmd/server`](cmd/server/README.md) | 公开 API、SSE 失效事件、Controller 的 gRPC 控制监听、状态机与后台循环 |
| Gateway | [`cmd/gateway`](cmd/gateway/README.md) | 浏览器唯一的入口：外部登录、会话、向 Cloud 转发并现签内部凭据 |
| model-gateway | [`cmd/model-gateway`](cmd/model-gateway/README.md) | 个人模型 Key 的保管与模型请求转发 |
| git-server | [`cmd/git-server`](cmd/git-server/README.md) | Project 仓库的 Git 服务，逐请求按 Cloud 状态授权 |
| cloudctl | [`cmd/cloudctl`](cmd/cloudctl/README.md) | 迁移、首个租户引导、凭据引用登记 |

正式前端在 [`frontend/`](frontend/README.md)，部署时可由 Gateway 同源提供。Controller、Node 在 [ora-space/desktop](https://github.com/ora-space/desktop)，
Sandbox Server 与完整的 Compose 环境在 [ora-space/cluster](https://github.com/ora-space/cluster)。

## 快速开始

需要 Go（版本以 `go.mod` 为准）、Git、PostgreSQL 17、[Task](https://taskfile.dev) 和 `buf`；前端另需 Node 24。

```sh
docker compose up -d --wait           # 本地 PostgreSQL 17，监听 127.0.0.1:55432
cp config.toml.template config.toml   # 默认值即可用；config.toml 被 Git 忽略
task setup                            # 生成密钥与 secret 文件、写 .local/dev.env、执行迁移
task dev                              # Cloud :8080 + Gateway :8081 + 前端 :5173
task check                            # 格式、lint 与完整的 PostgreSQL 测试
```

Windows 的数据库、控制监听证书等细节见 [本地开发](docs/development/local-development.md)，测试分层与门禁见 [测试与门禁](docs/development/testing.md)。

## 契约

- HTTP：[`api/openapi.json`](api/openapi.json) 由 `router.Routes` 与 `internal/contract` 生成，`frontend/src/api` 再由它生成，
  `task frontend:generate` 一次完成；接口字段以它为准，约定见 [HTTP API 约定](docs/core/http-api.md)。
- Controller：[`proto/`](proto/README.md) 定义 gRPC 控制契约，`internal/controlpb` 由它生成，见 [Controller 内部控制契约](docs/runtime/controller-integration.md)。
- 业务决策：[specs](https://github.com/ora-space/specs/tree/main/decisions/cloud) 的 ADR 与核心用例。

## 文档

- [文档地图](docs/README.md)：架构、各领域、运维与开发文档。
- 每个包的实现细节在所在目录的 `README.md`：[`cmd/`](cmd/README.md)、[`internal/`](internal/README.md)、[`integration/`](integration/README.md)、
  [`pkg/`](pkg/README.md)。
- 代码规范与修改流程：[AGENTS.md](AGENTS.md)、[修改与扩展](docs/development/changing-the-code.md)。
