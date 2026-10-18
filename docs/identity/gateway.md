# Gateway：外部登录、浏览器会话与代理

[English](gateway.en.md) | 中文

Gateway（`cmd/gateway`，实现在 `internal/gateway`）是浏览器唯一直接访问的服务。它完成外部登录，在
PostgreSQL 里保存浏览器会话，为每个已认证请求现签两份内部 JWT，然后把 `/api/v1/*` 转发到固定的 Cloud
upstream。部署配置了前端构建产物时，它还在同一个 origin 上提供前端静态文件。

Gateway 不读写任何 Cloud 业务表：用户、身份映射、成员和资源授权都在 Cloud。它只拥有两张表
`gateway_login_attempts` 与 `gateway_sessions`，并且和其他业务进程一样，启动时只校验迁移，不执行 DDL。

## 路由

| 路由 | 说明 |
|---|---|
| `GET /auth/providers` | 列出本部署注册的登录方式和默认的外部 provider，前端据此决定显示哪些按钮、是否自动跳转 |
| `POST /auth/login` | 同源 JSON `{"provider":"github","returnTo":"/path"}`；`provider` 省略时用部署的外部 provider。创建一次登录尝试，返回 `{"authorizationUrl"}` 并设置 attempt Cookie |
| `GET /auth/callback/:provider` | provider 回跳。校验通过后创建会话，`303` 回到登录前的页面；失败统一为 `401 login_failed` |
| `POST /auth/logout` | 同源校验后吊销当前会话并清除 Cookie，幂等，返回 `204` |
| `ANY /api/v1/*` | 解析会话，现签内部凭据后转发 |
| `GET /healthz` | PostgreSQL 探活 |
| 其他 `GET`/`HEAD` | 配置了 `web.dist_dir` 时提供前端文件，未命中的客户端路由返回 `index.html` |

`/internal/*` 没有任何路由。错误外形与 Cloud 相同：`{"code","params","requestId"}`。

## 登录流程

```text
浏览器                 Gateway                         外部 provider            PostgreSQL
  │ POST /auth/login    │                                    │                      │
  │────────────────────▶│ 同源检查、限流                       │                      │
  │                     │ 生成 attempt secret、state，派生 PKCE verifier             │
  │                     │──────────────────────────────────────────────────────────▶│ 写 attempt（只存摘要）
  │◀── authorizationUrl + attempt Cookie                     │                      │
  │ 跳转到 provider ───────────────────────────────────────────▶│                      │
  │◀───────────────────────────────────────── 回跳 callback?code&state              │
  │ GET /auth/callback  │ 匹配 attempt Cookie、state、provider、未过期、未消费         │
  │────────────────────▶│ 在事务外用 code 换取身份 ───────────▶│                      │
  │                     │ 短事务：锁定 attempt，写 consumed_at，创建 session ──────────▶│
  │◀── 303 returnTo + session Cookie（清除 attempt Cookie）                          │
```

- **一次尝试最多一个会话。** callback 先查 attempt，再在事务外调用 provider，最后在短事务里
  `FOR UPDATE` 锁定 attempt、检查仍未消费、写入 `consumed_at` 并创建会话。并发的两个 callback 只有一个
  能成功。provider 成功但事务失败就是登录失败，不推断会话已创建。
- **PKCE verifier 不落库。** 它由 attempt secret 和 `login.pkce_key_file` 经带域分离的 HMAC-SHA256 派生。
  GitHub 会把 verifier 发给 provider，所以**所有 Gateway 副本必须挂载同一把 PKCE 密钥**，否则回跳到另一个
  副本时登录会失败。
- **`returnTo` 只接受站内相对路径**：以单个 `/` 开头，第二个字符不是 `/` 或 `\`，不含控制字符。数据库层
  也拒绝绝对路径和 `//`、`/\` 形式。
- **失败对外收敛。** provider 拒绝、OAuth 错误、超大或非法响应、缺少必要字段都返回 `401 login_failed`；
  网络、超时、429 和 5xx 返回 `503 login_unavailable`。原始错误不返回给浏览器。
- 登录开始和回调在写入任何 attempt 之前按客户端地址限流。

## 会话与 Cookie

| Cookie | 生产名称 | 属性 |
|---|---|---|
| 会话 | `__Host-ora_session` | `HttpOnly`、`Secure`、`SameSite=Lax`、`Path=/`、无 `Domain`；`Max-Age` 等于数据库里的剩余有效期 |
| 登录尝试 | `__Secure-ora_login` | `SameSite=Lax`（provider 回跳是跨站顶层导航，`Strict` 会让 callback 收不到 Cookie）、`Path=/auth/callback`，`Max-Age` 等于尝试有效期 |

本地开发（`public.development: true`，loopback HTTP）下名称为 `ora_session`、`ora_login`，去掉 `Secure`。

- 会话有效期是**绝对期限**，读取不续期。默认 30 天；`login.provider` 为 `huawei-idaas` 时默认 12 小时，
  使企业账号状态的变化在有限时间内由 IDaaS 重新核验；上限 90 天，数据库约束同样限制。
- 有效性只由数据库状态和 `clock_timestamp()` 判定。未知、过期、已吊销的会话一律返回 `401 unauthenticated`
  并清除 Cookie。
- 退出写入 `revoked_reason=logout`。数据库只允许 `logout`、`identity_revoked`、`administrative` 三种吊销原因，
  HTTP 只开放本人退出。
- 生命周期内的清理 goroutine 每 `session.cleanup_interval` 运行一次，每批至多 `session.cleanup_batch` 行，
  只删除超过 `session.retention` 的过期或已吊销会话和过期尝试，期间保留审计窗口。
- 有效的 Gateway 会话不能绕过 Cloud：用户被停用、成员被移除时，Cloud 照样返回 401/403，Gateway 原样透传。

数据库只保存 attempt secret、`state` 和会话 token 的 SHA-256 摘要。原始 token、OAuth code、provider
access token 和 PKCE verifier 只存在于单次请求的内存里，不持久化、不写日志、不进入 Cloud。

## 代理

`ANY /api/v1/*` 的处理：

1. 修改类方法要求同源证明：`Origin` 与公开 origin 精确一致，或 `Sec-Fetch-Site: same-origin`，否则
   `403 origin_forbidden`。
2. 解析会话 Cookie；失败返回 `401 unauthenticated`。
3. 删除浏览器带来的 `Authorization`、`X-Ora-User-Token`、`Cookie`、`Forwarded`、`X-Forwarded-*`、
   `X-Real-Ip`，换上本副本现签的 service JWT 与 user JWT（见 [内部凭据与服务信任](authentication.md)）。
4. 转发到固定的 `cloud.upstream`，请求不能选择其他目标。请求体上限 64 KiB，响应体上限 8 MiB；Cloud 不可达
   或在 `cloud.timeout` 内没有完成响应时返回 `502 upstream_unavailable`，会话不受影响。

两类请求有特殊处理：

- **SSE 事件流。** Cloud 以 `text/event-stream` 应答后，这一条已授权的连接不再受 `cloud.timeout`、
  `server.write_timeout` 和响应体上限约束，逐帧转发直到任一方关闭。
- **个人模型 Key。** `.../me/model-connections/:id/credential` 的写入和清除只转发到
  `model_credentials.upstream` 指定的固定 HTTPS model-gateway，用部署提供的 CA 校验对方证书；Cloud 永远
  不作为这条路径的回退，没有配置时返回 `503 model_service_unavailable`。Gateway 只持有 model-gateway 的
  公开 CA，不持有它的私钥或加密主密钥。
- 会话分析的只读接口在转发前拒绝无法被 `url.ParseQuery` 解析的查询串（`400 invalid_session_filter`），
  避免代理清洗查询后改变请求含义。

## 登录 provider

**华为 IDaaS（`internal/gateway/idaas`）。** 使用 IDaaS 2.0 Authorization Code，以机密客户端方式
（`client_secret_post`）换取令牌，不启用 PKCE。

- authorize 默认地址为 `<idaas.base_url>/saaslogin1/oauth2/v1/authorize`，只带 `client_id`、
  `response_type=code`、`scope=base.profile`、`state` 和固定 callback。callback 的协议、域名、端口必须与
  IDaaS 应用的登记值逐字一致。
- 以表单 `POST` 调用 `/saaslogin1/oauth2/v1/token`，`client_id`、`client_secret`、`code` 放在请求体里，
  不放进 URL，避免进入访问日志；再以同样方式调用 `/saaslogin1/oauth2/v1/userinfo`。
- 规范化出的身份为 `source=huawei-corp`、`subject=<去首尾空白的 uuid>`，另带经验证的 `globalUserId`（兼容
  `globalUserID` 拼写）和 `idaas.display_name_field` 指定的显示名，缺失时回退为 uuid。`globalUserId` 缺失时
  仍可登录，只是不能自动关联管理员按员工目录预先添加的人员。
- 令牌和 userinfo 客户端有独立的总超时、64 KiB 响应上限，传播取消，不跟随重定向。
- 部署可独立覆盖 authorize、token、userinfo 完整 HTTPS 地址；省略的端点仍使用上述默认路径。
  覆盖值不得包含 URL 用户凭据、query 或 fragment，也不从浏览器请求读取。配置项见
  [配置参考](../operations/configuration.md#gateway)。覆盖地址不改变表单协议、scope、`huawei-corp` 来源或 UUID 映射，
  不迁移身份、撤销已有会话或授予租户权限；目标必须维持同一企业身份权威。

**GitHub（`internal/gateway/github`）。** OAuth App Authorization Code 加 PKCE S256，不请求任何 scope，
authorize 请求带 `allow_signup=false` 和 `prompt=select_account`。读取 `GET /user` 后立即丢弃 access token，
身份为 `source=github.com`（GitHub Enterprise Server 时为配置的 host）、`subject=<数字 id>`、显示名取 name 或
login。

**本地开发（`internal/gateway/devlogin`）。** `login.development_provider: true` 注册 `dev` provider：一张由
Gateway 自己提供的表单，可以输入任意 `source`、`subject`、显示名登录，之后的 attempt、PKCE、会话流程与
GitHub 完全相同。因为它能以任何身份登录，它只能与 `public.development`（loopback HTTP）同时开启，启动日志
会给出警告；生产配置不得设置。

## 浏览器侧的自动登录

前端以会话 Cookie 和 `GET /api/v1/me` 作为唯一的认证事实。页面收到 `401` 后带着当前站内路径进入登录
过渡页；当 `GET /auth/providers` 只有一个外部 provider（没有 `dev`）时自动发起 `POST /auth/login` 并跳转，
否则为每个 provider 显示一个按钮。`403` 表示用户在 Cloud 已被停用，不再重新登录；`5xx` 或登录启动失败时
停止自动重试，显示手动重试入口。

前端与 Gateway 必须对浏览器表现为同一个 origin：部署时由 Gateway 通过 `web.dist_dir` 直接提供
`frontend/dist`；本地开发时 Vite 只把 `/auth`、`/api`、`/healthz` 代理到 Gateway，从不暴露 `/internal`。

## 部署要点

- 所有副本共享同一个 PostgreSQL、PKCE 密钥、provider 客户端配置和 client secret。
- 为 Gateway 配置专用的数据库角色：只对 `gateway_login_attempts`、`gateway_sessions` 拥有
  `SELECT/INSERT/UPDATE/DELETE`，对 `schema_migrations` 只有 `SELECT`。
- `public.base_url` 必须是 `https` origin（不含路径和查询），callback 固定为
  `<base_url>/auth/callback/<provider>`，不从请求头推导。只有开发模式允许 loopback 上的 `http`。
- 两把签名私钥（`tokens.*_private_key_file`）必须不同，对应 Cloud `auth.keys` 里 `kind: service, role: gateway`
  与 `kind: user` 两个条目；`tokens.lifetime` 不超过 5 分钟。
- 配置项的完整说明见 [配置参考](../operations/configuration.md#gateway)。

**IDaaS 上线检查。**

1. 每个环境单独登记 Authorization Code 应用和精确的 callback，申请 `base.profile`、`uuid`、`globalUserId`
   和所需的姓名字段；beta 与生产不复用 origin。默认 userinfo 不含 `uuid` 时必须先在控制台申请，不能改用
   `globalUserId` 或工号作为登录 subject。
2. 把实际返回的顶层姓名字段（例如 `userName` 或 `cn`）配置为 `idaas.display_name_field`。
3. 以只读文件挂载 client secret 和至少 32 字节的 PKCE 密钥，所有副本内容相同。
4. 预置首位管理员时，`cloudctl bootstrap -subject` 必须是该员工在 IDaaS 返回的 `uuid`。其他人员由管理员通过
   员工目录按 `globalUserId` 预先添加，首次登录后自动关联。
5. 先在 beta 环境验证换票、userinfo 字段、callback、原路径恢复、停用用户和本地退出，再切换生产的
   `idaas.base_url`、客户端配置和 callback。不要把 code、token、userinfo、邮箱或工号加入日志采样。

## 相关决策

- [外部身份由 Gateway 归一，Cloud 保持授权权威](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/0-gateway-mediated-external-identity.md)
- [IDaaS UUID 继续作为登录键，globalUserId 作为人员目录关联键](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/20260923-huawei-global-id-alias.md)
