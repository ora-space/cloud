# credentialstore：独立保管外部 Git 凭据

[中文](README.md) | [English](README.en.md)

Credential Store 是独立进程，保管用户托管的 HTTPS Git token（`proposed` 的 ADR“Git 凭据由独立的 Credential Store
保管”D2、D3，执行器根决策 D3）。浏览器凭 Cloud 签发的一次性提交授权直接提交 token；Cloud 只经管理接口读元数据、撤销；
Repo Executor 凭一次性执行授权兑换明文，每份授权至多一次；KEK 运维身份在密钥轮换后重新包裹 DEK。每个调用方是一个独立
身份、一个独立监听，没有身份同时持有两项能力。

| 文件 | 职责 |
|---|---|
| `keyring.go` | 部署挂载的只读 KEK keyring（`{"primary":"<kekId>","keys":{"<kekId>":"<base64 32 字节>"}}`），缺失或无效即 `ErrKeyringUnavailable` |
| `envelope.go` | 信封加密：每个版本一个随机 DEK，AES-256-GCM 加密 `{"username","token"}`，AAD = `credentialId + "\x00" + version`；DEK 由 KEK 以 AES-256-GCM 包裹，AAD = `kekId`；`rewrap` 只换包裹 |
| `files.go` | 卷布局与原子写入（临时文件、fsync、rename、目录 fsync）；`O_CREATE\|O_EXCL` 的独占认领文件 |
| `store.go` | 提交、兑换、元数据、撤销、rewrap 与保留期清扫；写操作串行，兑换持读锁跨越“检查、认领、解密” |
| `identity.go` | 固定的 SPIFFE 身份；`PeerIdentity` 只接受已验证叶证书上恰好一个 URI SAN；`Identities.Validate` 拒绝重叠 |
| `server.go` | `NewServer`、错误码、严格 JSON 解码（16 KiB 上限）、mTLS 身份门禁 |
| `service.go` | 兑换、管理、密钥三个 mTLS 监听的处理器 |
| `ingress.go`、`ratelimit.go` | 浏览器入口：唯一路由、CORS、按来源 IP 的 GCRA 限速、token 校验 |
| `config.go` | `CREDENTIAL_STORE_*` 环境变量与 TLS 1.3 监听配置 |
| `client.go` | Cloud 用的 `ManagementClient{Get, Revoke}`，执行器用的 `RedemptionClient{Redeem}`：mTLS、TLS 1.3、不跟随重定向、响应体有上限、类型化错误 |

卷布局（根目录 `0700`，组和其他用户可访问时拒绝启动）：

- `credentials/<credentialId>.json`：当前版本与保留期内的旧版本密文、当前版本号与对应的 `submissionId`、撤销标记；
- `redemptions/<authorizationId>`：每份已兑换的执行授权一个认领文件；
- `submissions/<submissionId>`：每份已使用的提交授权一个认领文件。

认领文件保留 24 小时（远长于授权的最长有效期），之后由清扫删除。

## 四个监听

| 监听 | 调用方 | 路由 |
|---|---|---|
| 兑换（mTLS，默认 `:8461`） | 只认 `spiffe://ora.local/repo-executor` | `POST /v1/redemptions`，体 `{"authorization":"<token>"}` → `{"username","token","version"}` |
| 管理（mTLS，`:8462`） | 只认 `spiffe://ora.local/cloud/credential-manager` | `GET /v1/credentials/{id}` → `{"credentialId","currentVersion","submissionId","revoked"}`（未知 id 为版本 0）；`POST /v1/credentials/{id}/revoke` → `{"revoked":true}`，幂等 |
| 密钥（mTLS，`:8463`） | 只认 `spiffe://ora.local/credential-store/kek-operator` | `POST /v1/keys/rewrap` → `{"rewrapped":n}` |
| 浏览器入口（HTTPS，无客户端证书，`:8460`） | 持有提交授权的浏览器 | `PUT /v1/credentials/{id}/versions`，头 `Authorization: Ora-Submission <grant>`，体 `{"token","username"?}` → `{"storeVersion":N}` |

兑换的顺序：以 Store 自己的时钟和 Cloud 的**执行**公钥集合验证授权（`cloneauth.Verify`，用途只能是
`sync`/`publish`/`probe`）；授权中的 `executorIdentity` 必须等于证书身份；必须带凭据；凭据存在、未撤销、版本是当前版本
或被取代未满 1 小时；**先**独占创建 `redemptions/<authorizationId>` 并落盘，**再**解密。解密失败时认领仍在，同一授权不能
再兑换。

提交的顺序：以**提交**公钥集合验证授权（`cloneauth.VerifySubmission`），授权的 `credentialId` 必须等于路径；凭据未撤销、
租户与首次提交一致、`submissionId` 未使用、`expectedStoreVersion` 恰为当前版本加一；独占认领 `submissions/<submissionId>`
后写入新版本，旧的当前版本记为被取代，1 小时后不能兑换并被清扫销毁。token 为 1..8192 字节 UTF-8，不含 CR、LF、NUL；
用户名缺省为 `x-access-token`，不含 `:`。CORS 只回显配置的 origin，只允许 `PUT` 与 `Authorization`、`Content-Type` 头，
从不设置 `Access-Control-Allow-Credentials`；默认每 IP 每分钟 10 次、突发 5 次。

撤销写入撤销标记并在同一次原子替换中删掉所有版本的密文；未知 id 也留下撤销标记，之后的提交以 `credential_revoked` 拒绝。

## 错误码

所有拒绝都是 `{"code":"..."}`，不含内部细节：

| 状态 | 码 |
|---|---|
| 400 | `invalid_request`、`invalid_authorization` |
| 401 | `client_identity_required`、`invalid_submission`、`submission_expired` |
| 403 | `client_identity_forbidden`、`executor_identity_mismatch`、`submission_mismatch`、`origin_forbidden` |
| 404 | `credential_not_found`、`not_found`（未知路由） |
| 409 | `already_redeemed`、`submission_replayed`、`version_conflict` |
| 410 | `credential_unavailable`（已撤销、版本未知或超过保留期）、`credential_revoked`（向已撤销凭据提交） |
| 413 | `request_too_large` |
| 429 | `rate_limited` |
| 500 | `store_unavailable` |

客户端把它们映射为 `ErrAlreadyRedeemed`、`ErrInvalidAuthorization`、`ErrCredentialNotFound`、`ErrCredentialUnavailable`、
`ErrIdentityRefused`、`ErrRejected`；无法连接、5xx 或响应无法解析为 `ErrUnavailable`。`*RefusalError` 带状态和码。

审计日志（zap 结构化字段）只有 `credentialId`、`version`、`authorizationId`/`submissionId`、`purpose`、`peer` 与 `outcome`；
token、用户名、提交授权和执行授权都不进入日志、错误字符串或管理响应。`Secret` 与 `Credential` 的格式化输出会脱敏。

测试全部在 `t.TempDir` 和进程内生成的密钥、CA 与证书上，经真实 TLS 覆盖：加解密与 AAD、KEK 轮换与 rewrap、提交与兑换的
拒绝矩阵、并发兑换只成功一次、解密失败前已有兑换记录、保留期、撤销销毁密文、身份边界，以及日志中找不到明文。
