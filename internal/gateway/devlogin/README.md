# internal/gateway/devlogin：本地开发登录 provider

[中文](README.md) | [English](README.en.md)

`devlogin.Authenticator` 实现 `gateway.Authenticator`，同时自己扮演外部 provider：浏览器不去 GitHub，而是被送到本包提供的一个小表单（`GET /auth/dev/authorize`），开发者输入想要成为的身份，表单把一个带签名的 code 交回普通的 `/auth/callback/dev` 路由。Login Attempt、`state`、PKCE、Cookie、PostgreSQL 会话与凭据签发全是真实的，只有"你是谁"这一步是假的。它的存在是为了让前端在没有 GitHub OAuth App 时也能开发与演示。

## 行为

- `AuthorizationURL`：指向 `<public origin>/auth/dev/authorize?state=…&code_challenge=…`，拒绝任何不是自己的 callback URL。
- `GET /auth/dev/authorize`：渲染表单，`state` 与 `code_challenge` 为隐藏字段，可填 `source` / `subject` / `display_name`（默认 `dev` / `developer` / `Developer`）。`Cache-Control: no-store`。
- `POST /auth/dev/authorize`：仅限同源（`gateway.SameOrigin`）；用 `gateway.Normalize` 规范化身份（source 与 subject 必填，遵守 Cloud 的字节上限），用进程内随机密钥的 HMAC-SHA256 封装 `{source, subject, displayName, challenge, expires}`，`303` 到 `<callback>?state=…&code=…`。code 有效期 5 分钟。
- 表单路由自身的错误响应（缺失 `state`/`code_challenge`、跨站提交、身份非法时重新渲染表单）是面向人的纯文本/HTML，不走 JSON fault 形状；`/auth/callback/dev` 的失败仍统一为 fault，编排层不因 provider 而异。
- `Exchange`：校验 callback URL、签名、有效期，以及 `S256(code_verifier)` 等于封装的 challenge，然后返回输入的身份。任何失败都是 `gateway.ErrProviderRejected`，编排层统一回答 `401 login_failed`。

## 不变量

- 只有设置 `login.development_provider` 时才注册，而 `gateway.Config.Validate` 只在同时开启 `public.development`（loopback HTTP）时接受它；`New` 还会拒绝任何非 loopback `http://` 的 origin。
- 签名密钥只存在于进程内存，每次启动重新生成；来自其他进程的 code、被篡改的 payload 或不匹配的 verifier 一律拒绝。
- provider 不授予任何权限：Cloud 仍按 `(source, subject)` 创建或解析用户，membership 与授权与 GitHub 身份完全相同。默认 source 为 `dev`，避免与 `github.com` 身份混淆，但可以输入任意 source，例如以 `cloudctl bootstrap` 创建的管理员身份登录。
- 开启该 provider 时 Gateway 在启动日志中给出 warning。

参见 [认证边界（`internal/gateway`）](../README.md) 与 [Gateway 文档](../../../docs/identity/gateway.md)。
