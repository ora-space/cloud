# internal/gateway/github: GitHub OAuth 适配器

[中文](README.md) | [English](README.en.md)

`github.Authenticator` 实现 `gateway.Authenticator`，把 GitHub OAuth App 的 Authorization Code Flow 归一为 `VerifiedIdentity`。它是唯一理解 GitHub 端点与用户文档的代码，不能授予任何 Ora 权限。

## 行为

- `AuthorizationURL`：携带 `client_id`、固定 `redirect_uri`、编排层生成的 `state` 与 `code_challenge`（`S256`）、`allow_signup=false` 与 `prompt=select_account`（强制 GitHub 弹出账号选择器，否则 GitHub 会静默复用浏览器当前的 github.com 登录态，无法换账号登录）；不请求任何 scope。
- `Exchange`：以 `code`、`code_verifier`、`client_secret` 与 `redirect_uri` 向 token 端点交换 access token（GitHub 收到 challenge 后强制校验 verifier），随后调用 `GET /user`，读取后立即丢弃 token。
- 身份归一：`source` 默认 `github.com`（GitHub Enterprise Server 配置为该 host），`subject` 为数字 `id` 的十进制字符串，`displayName` 取非空 `name` 否则 `login`。缺失、非数字或非正的 `id` 使登录失败。
- 失败分类：GitHub 的错误对象（即使 HTTP 200）、非 `bearer` token、用户端点非 200、超过 64 KiB 或非法 JSON 的响应都是 `gateway.ErrProviderRejected`；网络、超时与取消是基础设施错误，编排层据此区分"登录失败"与"登录暂不可用"。
- HTTP 客户端必须带总超时，`NewHTTPClient` 额外禁止重定向。

## 不变量

- GitHub access token、完整用户文档与 email 不写入数据库、日志、错误、JWT 或浏览器。
- `login`、`name`、email 永不作为身份键；改名后仍映射到同一 `(github.com, id)`。

参见 [认证边界 (`internal/gateway`)](../README.md) 与 [Gateway 文档](../../../docs/identity/gateway.md)。
