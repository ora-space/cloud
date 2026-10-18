# 内部凭据与服务信任

[English](authentication.en.md) | 中文

Cloud 不接入任何外部登录协议，也不签发凭据。外部登录（华为 IDaaS、GitHub）只在
[Gateway](gateway.md) 里完成；Cloud 看到的永远是 Gateway 签发的内部凭据，以及服务之间的双向 TLS 身份。
本文说明这些内部信任关系，以及 Cloud 如何从一份已验证的凭据得到一个用户。

## 信任关系一览

| 调用方 → 被调用方 | 通道 | 证明身份的东西 |
|---|---|---|
| Gateway → Cloud server（HTTP） | `/api/v1/*` | 每个请求两份 Ed25519 JWT：service JWT 加 user JWT |
| Gateway → model-gateway（HTTPS） | 个人模型 Key 的写入与清除 | 同样的两份 JWT，外加对 model-gateway 证书的 CA 校验 |
| Controller → Cloud server（gRPC） | 内部控制契约 | TLS 1.3 双向认证的 Controller 证书（SPIFFE URI），加 `x-ora-controller-id` metadata |
| Node → model-gateway | 模型调用的临时授权 | 沙盒注入的专用模型客户端证书（见 [model-gateway](../models/model-gateway.md)） |
| 沙盒、Repo Executor、Cloud → git-server | Git 与 ref 查询 | 各自独立 CA 签发的客户端证书（见 [Git Server](../repositories/git-server.md)） |

所有身份都只回答"谁在调用"。调用方能做什么，由 core 根据当前 PostgreSQL 状态判定；证书和 JWT 里的
声明不会直接变成业务权限。

## HTTP 的双凭据

每个公开请求同时带两份独立签名的 JWT：

```text
Authorization: Bearer <service JWT>    证明调用方服务，例如 Gateway 的某个实例
X-Ora-User-Token: <user JWT>           证明最终用户，并绑定签发它的服务
```

两份凭据用两把**不同用途**的私钥签发（Gateway 启动时拒绝两把相同的密钥），所以拿到一类签名能力
不能伪造另一类。user JWT 的 `caller` 必须等于 service JWT 的 `sub`，从而把用户身份绑在当次调用的服务
实例上，被截获的 user JWT 不能换一个服务来用。

**声明。**

| 声明 | service JWT | user JWT |
|---|---|---|
| `iss`、`aud` | 必须匹配可信密钥的 issuer 和 Cloud 配置的 audience | 同左 |
| `sub` | 服务实例标识，例如 `gateway-a` | 用户在外部身份源里的稳定标识 |
| `kind` | `service` | `user` |
| `role` | `gateway`（公开接口只接受这个角色） | — |
| `caller` | — | 必填，等于 service JWT 的 `sub` |
| `source` | — | 必填，身份命名空间，例如 `huawei-corp`、`github.com` |
| `displayName` | — | 可选，只在首次创建用户时使用 |
| `globalUserId` | — | 可选，华为员工目录的关联键 |
| `iat`、`exp` | 必填 | 必填 |

**验证规则**（`core.Authenticator.Verify`）：

- 只接受 `EdDSA`（Ed25519）。JWT 头必须带 `kid`，并在配置的可信密钥里找到对应条目。
- 可信密钥条目固定了 `kind`、`issuer`，service 密钥还固定了 `role`；JWT 里的这几项必须与条目一致。
  于是一把 user 密钥签不出 service JWT，一把 Gateway 密钥签不出 Controller 角色。
- `aud` 必须包含配置的 audience；`exp` 与 `iat` 必须存在，`iat` 不能晚于当前时间，有效期
  （`exp − iat`）必须为正且不超过 **5 分钟**；带 `nbf` 时同样校验。
- `sub` 不能为空；user JWT 的 `source` 与 `caller` 不能为空。

有效期上限决定了凭据必须逐请求现签：Gateway 默认签 60 秒的凭据，浏览器会话本身的有效期由 Gateway
在数据库里管理，与 JWT 无关。

**密钥配置。** Cloud 只持有公钥：

```yaml
auth:
  audience: ora-cloud
  keys:
    - id: gateway-service-2026          # JWT 头里的 kid
      issuer: ora-internal-issuer
      kind: service
      role: gateway
      public_key_file: /run/ora-keys/gateway-service.pem   # PKIX PUBLIC KEY，Ed25519
    - id: user-identity-2026
      issuer: ora-internal-issuer
      kind: user
      public_key_file: /run/ora-keys/user-identity.pem
```

- 没有任何可信密钥时 server 拒绝启动，不会退回匿名访问。
- 轮换公钥时同时配置新旧两个 `kid`，重启 server 生效，等旧凭据自然过期后再移除旧条目。
- 私钥只由 Gateway 持有（`tokens.service_private_key_file`、`tokens.user_private_key_file`，PKCS#8 PEM）。
  JWT 不负责保密，Gateway 与 Cloud 之间仍应使用 TLS 或受控的服务网络。

## Controller 的 gRPC 身份

Controller 只主动拨出到 Cloud 的控制监听（`control.grpc_addr`），这个监听只接受 TLS 1.3 双向认证：

- Cloud 用 `control.certificate_file`、`control.private_key_file` 提供服务端证书，用
  `control.client_ca_file` 校验客户端证书；四项配置缺一项 server 就拒绝启动，生产没有明文回退。
- `control.controller_identity` 是一个 SPIFFE URI，形如 `spiffe://<信任域>/controller/<id>`。Controller
  证书的 URI SAN 必须**精确**包含这个 URI，否则返回 `PERMISSION_DENIED controller_certificate_scope`。
- 每个调用还必须带一个 `x-ora-controller-id` metadata，值必须等于 URI 里的 `<id>`。它成为该调用的
  Controller 主体，用于租约持有者和提交身份的判定；metadata 不能选择另一个服务身份或角色。

服务身份只证明"这是可信的 Controller"，不代替运行时的使用权限、独占会话或各类代次校验（见
[运行时控制](../runtime/runtime-control.md)）。`controlgrpc.NewDevelopment` 只用于进程内测试，不校验证书，
不进入生产监听。

## 从凭据到用户

core 在每个公开请求的事务开头把 user JWT 解析成一个 Cloud 用户：

1. 按 `(source, sub)` 查找 `user_identities`。找不到时**即时创建**（JIT）一个用户和这条身份映射，
   `displayName` 只在此时作为初始显示名。
2. `source = huawei-corp` 且带 `globalUserId` 时，还会解析第二个身份键 `(huawei-global, <规范化的 globalUserId>)`：
   - 两个键都没有记录：创建用户并写入两条映射。
   - 只有一个键有记录：把另一个键补绑到同一用户上，这是管理员按员工目录预先添加的人员首次登录时的路径。
   - 两个键分别指向不同用户：返回 `409 identity_conflict`，**不自动合并**。
3. 用户必须是 `active` 且未删除，否则返回 `403 user_disabled`。

身份只按 `(source, subject)` 匹配：不按姓名、邮箱或工号合并账号，工号也不参与任何授权。华为员工的
`subject` 是 IDaaS 返回的 `uuid`（形如 `uuid~…`），不是工号或 W3 账号；用 `cloudctl bootstrap` 预置首位
管理员时，`-subject` 也必须填这个 `uuid`。

少数只读入口（会话列表、会话归档、模型请求统计）使用只读的身份解析：只认已有的身份和别名，不即时
创建用户、不补绑别名，然后照常校验成员身份。

用户解析出来之后，租户成员身份和角色的判定见 [用户、租户与成员](tenancy.md)。

## 不做的事

- 外部 provider 的 token、OAuth code、PKCE verifier 都不进入内部 JWT，也不进入 Cloud。
- Cloud 不持有任何签名私钥，没有自己的认证中心，也没有公开的用户增删改接口。
- 部署私钥、仓库凭据等密钥只以文件引用出现在配置里，进程只把内容读进内存，不写日志、不落库、不返回。

## 相关决策

- [外部身份由 Gateway 归一，Cloud 保持授权权威](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/0-gateway-mediated-external-identity.md)
- [IDaaS UUID 继续作为登录键，globalUserId 作为人员目录关联键](https://github.com/ora-space/specs/blob/main/decisions/cloud/identity-access/20260923-huawei-global-id-alias.md)
- [运行时控制由 Cloud 授权、Controller 派发、Node 在真实入口持久隔离旧资格](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260927-fenced-runtime-control-delivery.md)
