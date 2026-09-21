# 认证 Gateway

Gateway（`cmd/gateway`）是浏览器可访问的公开认证与反向代理边界，实现 `specs/decisions/cloud/identity-access/0-gateway-mediated-external-identity.md`。它完成外部登录、在 PostgreSQL 中保存浏览器会话、为每个已认证请求签发短期内部 service/user JWT，并只把 `/api/v1/*` 转发到固定配置的 Cloud upstream。Cloud 继续拥有用户、identity 映射、JIT 创建、membership 和资源授权；Gateway 不读写任何 Cloud 业务表，也不在启动时执行 DDL。

## 路由

| 路由 | 说明 |
|---|---|
| `POST /auth/login` | 同源 JSON：`{"returnTo":"/path"}`；兼容显式 `provider`，省略时采用 `login.provider`。校验 `Origin`（或 `Sec-Fetch-Site: same-origin`）、限流后写入 Login Attempt，返回 `{"authorizationUrl"}` 并设置 attempt Cookie。`returnTo` 只接受以单个 `/` 开头且第二个字符不是 `/` 或 `\` 的相对路径；不存在 `GET` 形式。 |
| `GET /auth/callback/{provider}` | provider 回跳。同时匹配 attempt Cookie、`state`、provider、未过期、未消费；在事务外向 provider 交换 code（GitHub 同时提交 PKCE verifier；IDaaS 以 `client_secret_post` 提交 `grant_type`/`client_id`/`client_secret`/`code`）；再在一个短事务里锁定 attempt、写入 `consumed_at` 并创建 session。成功 `303` 到 attempt 中保存的 `returnTo`，失败统一 `401 login_failed`。无论成败都清除 attempt Cookie。 |
| `POST /auth/logout` | 同源校验后吊销当前 session（`revoked_reason=logout`）并清除 Cookie；幂等，返回 `204`。不会调用 IDaaS/W3 logout。 |
| `ANY /api/v1/*` | 解析 session Cookie，失败返回 `401 unauthenticated`。修改状态的方法要求同源证明（`403 origin_forbidden`）。丢弃浏览器提供的 `Authorization`、`X-Ora-User-Token`、`Cookie`、`Forwarded`/`X-Forwarded-*`，用本副本的两把私钥签发 service/user JWT 后转发。Cloud 不可达返回 `502 upstream_unavailable`，session 不受影响。 |
| `GET /healthz` | PostgreSQL 探活。 |

`/internal/v1/*` 没有路由，落到 `404 not_found`。所有错误使用 `{"code","params","requestId"}`，与 Cloud 一致。

## Cookie

- Session Cookie：生产为 `__Host-ora_session`（`HttpOnly`、`Secure`、`SameSite=Lax`、`Path=/`、无 `Domain`），`Max-Age` 等于数据库 `expires_at` 与当前时间之差，不会晚于数据库期限。开发 loopback HTTP 下为 `ora_session` 且无 `Secure`。
- Attempt Cookie：`__Secure-ora_login`（开发为 `ora_login`），`SameSite=Lax`（provider 回跳是跨站顶层导航，`Strict` 会让 callback 永远收不到），`Path=/auth/callback`，`Max-Age` 等于 attempt 有效期。

## 持久化

`internal/core/migrations/0005_gateway_auth.sql` 创建 `gateway_login_attempts` 与 `gateway_sessions`。数据库只保存 attempt secret、`state` 与 session token 的 SHA-256 digest（`bytea`，长度 32），约束保证 `expires_at > created_at`、session 不超过创建后 90 天、attempt 不超过 1 小时、`revoked_at` 与 `revoked_reason` 同时存在且 reason 只能是 `logout`/`identity_revoked`/`administrative`，`return_to` 在数据库层也拒绝绝对、`//` 和 `/\` 形式。

PKCE verifier 不落库：由 attempt secret 和 `login.pkce_key_file` 通过带域分离的 HMAC-SHA256 派生。编排层始终派生该值以满足 provider-neutral `Authenticator` 接口。GitHub 会把 verifier 发给 provider，因此所有副本必须配置同一把 PKCE 密钥，否则回跳到另一副本时 GitHub 会拒绝（集成测试覆盖了这一点）。IDaaS 适配器不把 verifier 放到线上请求里，换票保护依赖 client secret 与一次性授权码。

有效性只由数据库状态和 `clock_timestamp()` 决定；读取不续期。`Revoke`/`RevokeIdentity` 只作用于尚未过期的 session，过期行保留其过期事实作为审计。清理由 Gateway 生命周期拥有的 goroutine 按 `session.cleanup_interval` 运行，每批最多 `session.cleanup_batch` 行，只删除超过 `session.retention` 的过期/吊销 session 与过期 attempt。

## 配置与密钥

参见 `configs/gateway.yaml`。环境变量前缀为 `GATEWAY_`。启动时校验：

- `public.base_url` 必须是 `https` origin（不含路径/查询）；只有 `public.development: true` 且 host 为 `localhost`/`127.0.0.1`/`::1` 时允许 `http`。callback URL 固定为 `<base_url>/auth/callback/<provider>`，不从请求 header 推导。
- `login.provider` 只能是 `huawei-idaas` 或 `github`；只校验和读取所选 provider 的 secret。`huawei-idaas` 未显式配置 `session.ttl` 时默认为 12 小时，GitHub 缺省仍为 30 天；所有 session TTL 为绝对期限、不会滑动，且不超过 90 天。
- `login.attempt_ttl` 在 1 分钟到 1 小时之间；`tokens.lifetime` 不超过 5 分钟（Cloud 验证器上限）。
- `tokens.service_private_key_file` 与 `tokens.user_private_key_file` 是两把不同的 PKCS#8 Ed25519 私钥，分别对应 Cloud `auth.keys` 中 `kind: service, role: gateway` 与 `kind: user` 的公钥条目；一把私钥不能同时承担两个用途。
- `login.pkce_key_file` 至少 32 字节随机数据，两种 provider 都必须配置：编排层始终派生 verifier。GitHub 会把它发给 provider；IDaaS 不发送。`idaas.client_secret_file` 或 `github.client_secret_file` 只在对应 provider 被选择时读取；这些文件只进入进程内存，从不写日志或数据库。

生成密钥示例：

```bash
openssl genpkey -algorithm ed25519 -out gateway-service.key && openssl pkey -in gateway-service.key -pubout -out gateway-service.pem
```

```bash
openssl genpkey -algorithm ed25519 -out user-identity.key && openssl pkey -in user-identity.key -pubout -out user-identity.pem
```

```bash
head -c 32 /dev/urandom > gateway-pkce.key
```

`.key` 交给 Gateway，`.pem`（PKIX PUBLIC KEY）交给 Cloud 的 `auth.keys`。

## 华为 IDaaS 适配器

`internal/gateway/idaas` 使用 IDaaS 2.0 Authorization Code 的 `client_secret_post` 契约。PKCE 模式默认关闭，Gateway 作为机密客户端也不启用它；不发送已废弃的 `display`。authorize 地址固定为 `<idaas.base_url>/saaslogin1/oauth2/v1/authorize`，只携带 `client_id`、`response_type=code`、`scope=base.profile`、随机 `state` 和固定 callback `<public.base_url>/auth/callback/huawei-idaas`；callback 的协议、域名和端口必须与 IDaaS 应用登记值逐字一致。生产显式配置 `https://uniportal.huawei.com`，测试显式配置 `https://uniportal-beta.huawei.com`，Gateway 不自动猜测环境。

Gateway 以 `application/x-www-form-urlencoded` `POST` 调用 `/saaslogin1/oauth2/v1/token` 交换 code，请求体仅包含 `grant_type=authorization_code`、`client_id`、`client_secret` 和 `code`（不发送 `Authorization: Basic`，也不发送 PKCE-NULL 的 `code_verifier`/`redirect_uri`）。IDaaS 2.0 示例把同一组字段写在 query，但把 `Content-Type: application/x-www-form-urlencoded` 列为必填，该头只约束 entity-body。Gateway 按 RFC 6749 §2.3.1 `client_secret_post` 与 GitHub 适配器把字段放在请求体，避免 `client_secret`/`access_token` 进入 URL 或访问日志；不改用 `client_secret_basic`，因为认证方法不匹配时 IDaaS 返回 `error=invalid_request`。随后以同样的表单 `POST` 调用 `/saaslogin1/oauth2/v1/userinfo`，只携带 `access_token`。只保留 `uuid` 与 `idaas.display_name_field` 指定的顶层字符串：规范化身份固定为 `source=huawei-corp, subject=<trim 后的 uuid>`。`uuid` 只去掉首尾空白（文档成功示例含尾部空格，空白不承载身份区分度）；去掉后为空视为缺失。显示名缺失或无效时回退 uuid。access token、refresh token、code、verifier 和完整 userinfo 只存在于单次请求内存；不解析 `expires_in`，不持久化、不签入 JWT、不记录邮箱或工号。

token/userinfo 客户端有独立总超时、64 KiB 响应上限、传播请求取消且禁止跟随重定向。provider 拒绝、OAuth `error`（以及遗留 `errorCode`）、超大或非法 JSON 响应和缺少 uuid 对外收敛为 `login_failed`；网络、超时、429 和 5xx 为 `login_unavailable`。多个 Gateway 副本必须共享 PostgreSQL、PKCE key、IDaaS client 配置和 client secret。

IDaaS 应用登记与上线检查：

1. 为每个环境单独登记精确 callback 的 Authorization Code 应用，并申请 `base.profile`、`uuid` 及所需姓名字段；不要复用 beta 与生产 origin。IDaaS 2.0 §2.1 默认返回列表写的是 tenantid/UID/globalUserID，与 §2.4 把 `uuid` 标为必填互相矛盾。`uuid` 是唯一身份键：若默认 userinfo 不含该字段，必须先在控制台申请，不能改用 `globalUserID`/`UID`。IDaaS 2.0 的 PKCE 模式默认关闭；Gateway 使用 `client_secret_post`，不向 IDaaS 发送 PKCE 参数。
2. 将实际顶层姓名 JSON 键配置为 `idaas.display_name_field`；未获批或返回缺失时系统安全回退 uuid，不尝试邮箱/工号匹配。
3. 以只读 secret 文件挂载 client secret 与至少 32 字节 PKCE key；所有 Gateway 副本挂载相同内容，并使用同一 client ID/base URL。
4. 确认现有管理员 identity 已按 `huawei-corp + IDaaS uuid`（trim 后的值）预置；系统不会模糊合并旧账号，也不会从 IDaaS 群组自动授予 tenant membership。
5. 先以 beta IDaaS 验证：token/userinfo 接受请求体中的 `client_secret_post` 字段（query 为空）、脱敏 userinfo 含已申请的 `uuid`、callback、原路径恢复、停用用户和本地退出，再切换生产 base URL、client 配置与精确生产 callback。上线后不得把 code、token、userinfo、邮箱或工号加入日志采样。

## GitHub 适配器

`internal/gateway/github` 使用 OAuth App Authorization Code Flow，不请求任何 scope，authorize 请求携带 `state`、`code_challenge`（S256）与固定 `redirect_uri`；GitHub 在收到 challenge 后要求 token exchange 携带 `code_verifier`。适配器读取 `GET /user` 后立即丢弃 access token，只返回 `VerifiedIdentity{source: "github.com", subject: 数字 id 的十进制字符串, displayName: name 或 login}`。`displayName` 在离开编排层前按字节截断到 200，与 Cloud `identity()` 的上限一致。配置 `authorize_url`/`token_url`/`user_url`/`source` 可指向 GitHub Enterprise Server，此时 `source` 是该 host。

## 浏览器自动登录

前端以 HttpOnly session Cookie 和 `GET /api/v1/me` 为唯一认证事实。目标页面得到 401 后把当前站内相对路径放入 `returnTo` 并进入登录过渡页；该页自动 `POST /auth/login`，再用 `location.replace(authorizationUrl)` 进入 IDaaS/W3，成功后由 callback 根据 Login Attempt 中保存的 `returnTo` 以 303 返回原页面。callback 不接受新的跳转参数。

403 表示 Cloud 用户已停用，不再次登录；5xx 或登录启动失败会停止自动重试并展示显式重试入口。有效 session 访问登录页时直接返回目标页面。退出成功后清除当前用户查询缓存，再进入同一自动登录流程。前端与 Gateway 在开发和生产中必须对浏览器表现为同一 origin；Vite 仅把 `/auth`、`/api`、`/healthz` 代理至 `:8081`，从不暴露 `/internal`。

## 运行时数据库角色

生产部署应为 Gateway 配置只拥有 `gateway_login_attempts`、`gateway_sessions` 上 `SELECT/INSERT/UPDATE/DELETE` 以及 `schema_migrations` 上 `SELECT` 的角色。Gateway 启动只调用 `CheckSchema` 校验 migration 校验和，绝不执行 DDL；migration 仍由 `cloudctl migrate` 应用。

## 本地运行

```bash
task run:gateway
```

需要 Cloud（`task run`）已启动、数据库已迁移、`configs/gateway.yaml` 指向可用的密钥文件，且 Cloud `auth.keys` 登记了 Gateway 的两把公钥。完整本地联调可用 `task dev` 同时启动 Cloud、Gateway 与 Vite，并从 `http://localhost:5173` 访问。
