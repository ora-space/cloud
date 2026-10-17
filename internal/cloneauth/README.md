# cloneauth：外部 Git 线的一次性授权

[中文](README.md) | [English](README.en.md)

Cloud 签发、执行器与 Credential Store 验证的两类授权（ADR `repo-executor/0` D2；identity-access 的 Credential Store 决策）。
格式沿用旧实现：`base64url(payload) "." base64url(Ed25519 签名)`（RawURLEncoding），签名覆盖原始 payload 字节，验证不依赖
JSON 规范化。验证方持有公钥集合以便轮换；私钥只在 Cloud。

| 文件 | 职责 |
|---|---|
| `token.go` | 编码、签名与验证、严格解码（拒绝未知字段与多余值）、有效期与时钟窗口、哨兵错误 |
| `authorization.go` | `Purpose`（`sync`、`publish`、`probe`）、`Authorization`、`Sign` / `Verify` 与按用途的字段规则 |
| `submission.go` | `Submission`（用途 `submit`）、`SignSubmission` / `VerifySubmission` |
| `repository.go` | `NormalizeRepository` |
| `fields.go` | 规范 UUID、完整 OID、ref 与分支名校验 |
| `keys.go` | `LoadPrivateKey`（单个 PKCS#8 块）、`LoadPublicKeys`（一个或多个 PKIX 块） |

按用途的字段（其余用途的字段必须为空）：

| 用途 | 必填 | 允许 |
|---|---|---|
| `sync` | `operationId`、`tenantId`、`projectId`、`bindingId`、`repository`、`branch`、`expectedTrackingOid` | 凭据（公开仓库可匿名） |
| `publish` | 同上的 ID、`repository`、`sourceRef`、`sourceCommit`、`targetRef`（`refs/heads/<branch>`）、`expectedOldOid`、凭据 | — |
| `probe` | `operationId` 与 `verificationId` 恰好其一、`tenantId`、`repository`、`service`（`upload-pack`/`receive-pack`）、凭据 | `projectId`、`bindingId`、`targetRef` |

公共字段：`authorizationId`、`executorIdentity`（必须是 `spiffe://ora.local/repo-executor`）、`issuedAt`、`expiresAt`（UTC，
寿命在 `(0, MaxLifetime=15m]`）。凭据是 `credentialId` + `credentialVersion > 0`，或两者皆空。旧用途 `clone`、`mirror_prepare`、
`push` 不再定义，按未知用途拒绝。

不变量：

- `Sign` 与 `Verify` 执行同一套字段规则；`Sign` 拒绝无效输入（缺字段、禁止字段、寿命超限），`Verify` 不信任签发方。
- `Verify` 先验签名再解析 JSON；`now < expiresAt`，且 `issuedAt` 最多超前 `MaxClockSkew`（1 分钟），超前更多返回 `ErrInvalid`。
- 执行授权与提交授权用不同密钥对；即使误用同一密钥，严格解码与用途字段也让两者互不通过。
- `NormalizeRepository`：只接受 https；拒绝 userinfo、query、fragment、任何百分号编码、空 / `.` / `..` 段与非 ASCII；host 小写、
  去掉 `:443`；路径保留大小写，去掉一个尾部 `/` 与一个 `.git`。结果幂等，签名中的 `repository` 必须等于其规范化结果。
- 错误是哨兵（`ErrMalformed`、`ErrSignature`、`ErrExpired`、`ErrInvalid`、`ErrKey`、`ErrRepository`），不携带 payload 或密钥内容。

测试覆盖各用途往返、每个必填与禁止字段、错误密钥、篡改 payload 与签名、过期、超前签发、寿命超限、执行与提交授权互用、
未知字段、非 UTC 时间、密钥文件，以及 `NormalizeRepository` 的接受、相等与拒绝矩阵。依赖：标准库、`github.com/google/uuid`。
