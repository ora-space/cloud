# cmd/gateway: 认证 Gateway 守护进程

[中文](README.md) | [English](README.en.md)

`cmd/gateway` 是浏览器可访问的公开认证与反向代理入口，实现 `specs/decisions/cloud/identity-access/0-gateway-mediated-external-identity.md`。它只做配置、依赖装配、生命周期与退出行为；认证编排、会话策略、代理与 provider 适配都在 `internal/gateway`。

## 职责

- **配置与日志加载**：通过 `gateway.LoadConfig` 读取 `configs/gateway.yaml`（`GATEWAY_*` 环境变量可覆盖），校验公开 origin 必须为 HTTPS（开发 loopback 除外）、session 期限不超过 90 天、内部凭据期限不超过 5 分钟等边界，并初始化 Zap 日志。
- **PostgreSQL 与迁移门禁**：通过 `internal/repository.InitDB` 连接数据库，执行 `core.Store.CheckSchema` 校验 `internal/core/migrations` 全部已应用且校验和匹配；从不执行 DDL。
- **密钥装载**：从文件读取两把 PKCS#8 Ed25519 私钥（service 与 user 用途分离）、PKCE 派生密钥与当前所选 provider 的 client secret，只保存在进程内存；未选择的 provider 不要求其 secret 存在。
- **边界装配**：按 `login.provider` 构造华为 IDaaS 或 GitHub 适配器，再装配 `gateway.Store`、`gateway.Login`、`gateway.Issuer`、限流器与 `gateway.NewHandler`，监听配置端口。
- **有界清理**：启动由进程生命周期拥有的清理 goroutine，按批删除超过审计窗口的过期/吊销 session 与过期 attempt；停机时取消并等待其退出。
- **优雅停机**：捕获 `SIGINT`/`SIGTERM`，10 秒内完成处理中的请求，然后关闭连接池并刷新日志。

## 边界与不变量

- 只向固定配置的 Cloud upstream 转发 `/api/v1/*`；`/internal/v1/*` 无路由。
- 浏览器提交的 `Authorization`、`X-Ora-User-Token`、`Cookie` 与 `X-Forwarded-*` 不会到达 Cloud；每个请求使用本副本私钥重新签发分钟级 service/user JWT。
- 运行时只访问 `gateway_login_attempts`、`gateway_sessions` 与 `schema_migrations`。
- 华为内网部署使用 Authorization Code + PKCE S256；W3 登录只建立 Cloud 本地会话，退出不影响 W3/IDaaS SSO。

参见 [cmd 入口总览](../README.md)、[认证边界 (`internal/gateway`)](../../internal/gateway/README.md) 与 [Gateway 文档](../../docs/gateway.md)。
