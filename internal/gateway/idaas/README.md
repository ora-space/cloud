# idaas：华为统一身份适配器

[中文](README.md) | [English](README.en.md)

本模块把华为 IDaaS 2.0 Authorization Code（`client_secret_post`）适配为 Gateway 的 `Authenticator` interface。
authorize、表单 token 交换、userinfo 解析、provider 错误分类和响应上限都封装在模块内；调用者只看到
`VerifiedIdentity{source: "huawei-corp", subject: uuid, displayName}`。

| 文件 | 职责 |
| --- | --- |
| `idaas.go` | 固定端点和 scope、生成 authorize URL、交换 code、读取并最小化 profile。 |
| `idaas_test.go` | 验证 client_secret_post 表单、身份映射、字段回退、错误分类、取消和秘密不泄漏。 |

`uuid` 是唯一身份键，读取时去掉首尾空白（文档成功示例含尾部空格）；去掉后为空则拒绝。可配置的顶层
显示字段仅用于首次 JIT 展示，缺失时回退 `uuid`；邮箱、工号、refresh token 和完整响应不会离开模块。
authorize 只发送 2.0 普通模式必填字段，不附加 PKCE 或 `display`。token 以
`application/x-www-form-urlencoded` 把 `grant_type`、`client_id`、`client_secret` 和 `code` 放在请求体
（不发送 Basic 头，不把凭据放进 query，也不发送 PKCE-NULL 的 `code_verifier`）；userinfo 只在请求体
提交 `access_token`。token/userinfo 调用必须有总超时、禁止重定向且不得位于数据库事务内。超大或非法
JSON 响应与明确拒绝一样归为 `ErrProviderRejected`。参见
[Gateway 认证模块](../README.md) 与 [部署说明](../../../docs/gateway.md)。
