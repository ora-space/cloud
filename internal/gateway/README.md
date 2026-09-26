# internal/gateway: 公开认证边界

[中文](README.md) | [English](README.en.md)

`internal/gateway` 实现 Gateway 的认证编排、PostgreSQL 会话存储、内部凭据签发、浏览器安全策略与 Cloud 代理。它是 provider-neutral 的：只有 `Authenticator` 适配器（[华为 IDaaS](idaas/README.md)、[GitHub](github/README.md) 与仅限本地开发的 [devlogin](devlogin/README.md)）理解外部协议，其余部分只消费 `VerifiedIdentity` 与已解析的 `Session`。

## 文件与职责

| 文件 | 职责 |
|---|---|
| `identity.go` | `VerifiedIdentity`、`Authenticator` 接口、`Normalize`（按 Cloud 的 128/512/200 字节上限校验 source/subject，按 rune 边界截断 displayName）。 |
| `store.go` | `Store`：`CreateAttempt`、`LookupAttempt`（无锁预检）、`ConsumeAttempt`（`FOR UPDATE` 锁定 + `consumed_at` + session 同事务提交）、`Resolve`、`Revoke`、`RevokeIdentity`、`Cleanup`。只保存 SHA-256 digest，有效性由 `clock_timestamp()` 判定。 |
| `login.go` | `Login`：`Start` 生成 attempt secret、`state`，派生 PKCE verifier（HMAC，密钥仅 Gateway 可读）；省略 provider 时采用部署的外部 provider（`DefaultProvider`），绝不落到开发 provider；`Callback` 先查 attempt，在事务外调用 provider，再消费 attempt 创建 session。所有失败收敛为 `ErrLoginFailed`。`Providers` 列出已注册的适配器；`CodeChallenge` 是导出的 S256 变换。 |
| `tokens.go` | `Issuer`：用两把用途分离的 Ed25519 私钥签发 `kind=service,role=gateway` 与 `kind=user`（`caller` 绑定 service `sub`）凭据，期限不超过 Cloud 的 5 分钟上限。`LoadPrivateKey` 读取 PKCS#8 PEM。 |
| `security.go` | `NormalizeReturnTo`（拒绝绝对、`//`、`/\`、反斜杠、控制字符）、`SameOrigin`（`Origin` 精确匹配或 `Sec-Fetch-Site: same-origin`）、`CookiePolicy`（`__Host-` session Cookie、限定 callback 路径的 `SameSite=Lax` attempt Cookie）。 |
| `handler.go` | Gin 路由：`GET /auth/providers`、`POST /auth/login`、`GET /auth/callback/:provider`、`POST /auth/logout`、`ANY /api/v1/*`、`GET /healthz`；稳定错误形状 `{code, params, requestId}`。 |
| `proxy.go` | 到固定 upstream 的 `httputil.ReverseProxy`：连接/响应头超时、响应体 8 MiB 上限、错误回调经 context 传递。upstream 以 `text/event-stream` 应答时跳过体积上限并触发流回调，由 handler 解除该连接的 upstream 超时与写超时，事件流才能长期存活。 |
| `ratelimit.go` | 按客户端地址的令牌桶，键表有界；start/callback 在写入任何 attempt 前限流。 |
| `cleanup.go` | `RunCleanup`：生命周期拥有的有界批量清理循环。 |
| `web.go` | `Web`/`OpenWeb`：可选地从 `web.dist_dir` 提供前端构建产物。启动时要求 `index.html` 存在；只处理无路由命中的 `GET`/`HEAD`，文件经 `os.Root` 打开（`..` 与符号链接无法逃出目录），未命中的客户端路由返回 `index.html`（`Cache-Control: no-cache`）；`/api/`、`/auth/`、`/internal/` 与 `/healthz` 仍是 JSON 404。 |
| `config.go` | `Config`/`LoadConfig`/`Validate`/`PublicOrigin`：默认值与所有安全边界的启动期校验，包括至少配置一个 provider，以及 `login.development_provider` 只能与 `public.development` 同时开启。 |

## 不变量

- 原始 session token、attempt secret、OAuth code、provider token 与 PKCE verifier 不持久化、不写日志、不进入 Cloud。
- 外部 HTTP 调用不在数据库事务或行锁内；provider 成功但事务失败即登录失败，不推断 session 已创建。
- 每个 attempt 最多创建一个 session；两个并发 callback 只有一个能通过 `FOR UPDATE` 后的未消费检查。
- `Resolve` 不续期；未知、过期、吊销统一为 `ErrNotFound`/`401 unauthenticated`。
- 有效 session 不能绕过 Cloud 对 disabled user、membership 与 owner 的判定；Cloud 的 401/403 原样透传。

## 测试

单元测试覆盖 `return_to`/origin/Cookie 策略、identity 规范化、凭据签发与 Cloud 验证器互认、配置边界、限流与前端静态文件（文件、SPA 回退、越界与保留路径）；`integration/gateway_test.go` 用真实 HTTP、PostgreSQL、真实 Cloud router 与会校验 PKCE 的假 provider 覆盖登录、代理、伪造 header、跨源 mutation、重放、并发消费、多副本、过期、吊销、清理、provider/Cloud 故障、数据库约束，以及前端与 `/auth`、`/api` 同源提供。

参见 [内部子系统总览](../README.md)、[华为 IDaaS 适配器](idaas/README.md)、[GitHub 适配器](github/README.md)、[本地开发 provider](devlogin/README.md) 与 [Gateway 文档](../../docs/gateway.md)。
