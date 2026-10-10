# internal/credentialstore: 独立凭据服务(Secret Store)

[中文](README.md) | [English](README.en.md)

`internal/credentialstore` 实现**独立的** Credential Service:保存用户自管的个人 HTTPS Git Token,以及
Cloud 侧把 Token 交接给它所需的 `ingress-writer` 客户端。它落实
`specs/decisions/cloud/identity-access/20261010-user-managed-git-secret-store.md` 的安全边界:服务是
独立进程,有自己的持久卷、自己的密钥材料、自己的信任根,与 Cloud 业务库不共享数据库实例、连接、凭据或卷(D1)。
Cloud 在同一次请求内交出 Token,此后只保留 `credential_refs` 元数据与不透明的 `secret_ref`;Cloud 进程
不持有解密能力(D2)。

本包刻意只服务凭据存储这一件事:凭据归谁、能否绑定到 Project、Clone 是否被允许,全部属于 `internal/core`;
本包只把"密封、轮换、销毁、以及只对三种服务身份开放"做对。

## 文件

- `store.go`:`Config`(`Volume`、`KeyFile`、`Retention`、`Now`)与 `Store`(`Write`/`Rotate`/`Revoke`)。
  `Open` 是 D6/D8 要求的启动闸门——卷不可读或可被其他本机账户读取、KEK 缺失或不可用、卷未配置,都在此拒绝,
  绝不返回半初始化的 Store;`newSecretRef` 在服务内生成 24 字节(约 192 位)不透明引用(前缀 `cs_`),
  浏览器、终端用户与部署者都不能选择或预测;`prune` 只在写路径上清理保留窗口已过的旧版本,没有后台清扫任务。
- `keystore.go`:`Keyring` 与 `LoadKeyring`——部署挂载的 KEK 集(`primary` 加 `keys[]`,每个 32 字节
  base64),新封装一律用 `primary`,而封装记录自身携带 `keyId`,因此 KEK 轮换是纯部署变更、不需要数据迁移。
  每个密钥一份 DEK:`sealToken`/`openToken` 用 AES-256-GCM,`credentialID` 作为 AAD(密封块不能被挪到另一个
  凭据记录上);DEK 在 `defer clear` 中清零。`fingerprint` 是用 KEK 键控的 HMAC-SHA256,让重放被识别为同一份
  材料,同时使持有卷的攻击者无法离线校验候选 Token。任何不可用 KEK 一律报 `ErrKeyMissing`。
- `record.go`:`record`/`material`/`retained` 与 `fileStore`。卷内 `credentials/<id>.json`,一个凭据一条记录;
  `validCredentialID` 只接受规范小写 UUID,`fileStore.path` 再校验一次不让 id 逃出记录目录;写入走同目录临时
  文件加 `Sync` 后 `Rename` 原子替换。记录里的 `revokedAt` 是墓碑,`retained.retainUntil` 是旧版本的保留期限。
- `identity.go`:`Capability`(`write`/`resolve`/`keys`)、`IdentityConfig`(三个固定身份)与 `Identities`。
  三个身份必须存在、互不相同且形如 `spiffe://<信任域>/<kind>/<name>`;`PresentedIdentity` 从已被客户端 CA
  验证过的叶子证书 URI SAN 中取出身份,验证通过但不在表内的证书被拒绝,而不是当作匿名。
- `server.go`:`Server`——mTLS 加能力门控的 HTTP 面。`POST /v1/credentials`、`.../{id}/rotate`、`.../{id}/revoke`
  需要 `write` 能力,`.../{id}/resolve` 需要 `resolve` 能力;严格 JSON 解码(未知字段、非单一 JSON 对象、
  尾随内容都拒绝);`failure` 把 Store 错误映射为固定原因码,错误文本只进本服务日志;`audit` 每次操作记录
  operation/credentialId/serviceIdentity/result。`resolve` 目前无条件 409
  `authorization_verification_unavailable`。
- `client.go`:Cloud 侧 `ingress-writer` 客户端。`ClientConfig`(Endpoint 必须 `https`、不带 query 与
  fragment;TLS 必须带客户端证书与信任池)、`Failure`(带 `ReasonCode()`)、`Write`/`Rotate`/`Revoke`。
  刻意不重试:超时的写可能已经成功,恢复方式是调用方用同一个幂等键重发整个请求(D4)。
- 测试:`store_test.go`、`keystore_test.go`、`server_test.go`、`client_test.go`。

## 依赖与调用方

- 依赖:标准库(`crypto/aes`、`crypto/cipher`、`crypto/hmac`、`crypto/sha256`、`crypto/rand`、
  `crypto/tls`、`encoding/json`、`net/http`、`net/url`)与 `go.uber.org/zap`。本包**不**导入
  `internal/core`。
- 调用方:`internal/config`(`CredentialStoreServiceConfig.Store`/`ServerTLS`/`Identities` 装配服务端,
  `CredentialIngressConfig.Open` 装配 Cloud 侧 `*Client`);`cmd/credential-store`(独立进程装配与生命周期);
  `cmd/server`(把 `*Client` 交给 `internal/core` 作为 `store.Secrets`)。
- 反向依赖是**接口而不是包**:`internal/core` 只声明结构性接口 `CredentialSecretWriter`
  (`Write`/`Rotate`/`Revoke`)与 `credentialRefusal`(`ReasonCode()`),`credentialstore.Client` 无需
  被 core 导入即可满足,`core` 因此不持有传输层。

## 不变量

- **卷里没有 KEK 就没有明文**:记录中除 `fingerprint`(KEK 键控 HMAC,离线不可校验)全是密文或封装元数据,
  因此卷可以按部署自己的保留策略备份而不带 KEK(D8)。换一把 KEK 打开同一卷必须失败,不能回退到别的密钥,
  也不能表现为"空存储"。
- **每个密钥一份 DEK**:Token 由一次性 DEK 的 AES-256-GCM 封装,DEK 再由 KEK 封装;`credentialID` 作为 AAD,
  密封块不可迁移。KEK 轮换由封装记录里的 `keyId` 选择,旧 KEK 在仍能解开既有封装期间必须留在 keyring 里。
- **`secret_ref` 由本服务生成**:写请求里没有这个字段,提供未知字段会被严格解码拒绝;引用不透明且每个凭据唯一。
- **写是幂等的,冲突不是**:同一 id 加同一材料的重发返回原 `secret_ref` 与原版本;同一 id 加不同材料是
  `ErrConflict`,不是静默覆盖。
- **rotate 先写新版本**:旧版本保留到 `retainUntil` 而不是删除,使在旧版本下已被放行的执行仍能结算;
  重发当前 Token 是无操作(不推进版本,也不消耗在飞执行可能仍需要的保留窗口)。
- **revoke 是墓碑且幂等**:销毁 `current` 与全部 `previous` 的密文并写 `revokedAt`;再次 revoke、或 revoke
  一个从未存在的 id 都返回成功;墓碑使 create 重放无法复活 Cloud 已宣布失效的凭据。D4 把撤销权放在 Cloud
  的元数据里,这里只是纵深防御。
- **能力不重叠**:`Store` 类型上根本没有 resolve 或列举方法——`write` 身份无法读回任何 secret,`resolve`
  身份不能写、列举或删除;同一证书持有两种能力在启动时即被拒绝,因为那种失败的运行时表现是"每次调用都认证成功"。
- **没有匿名路径**:`NewServer` 要求配置了客户端 CA 的 mTLS;`route` 的身份来自已验证的 TLS 状态、能力来自
  部署固定表,URL、请求头、请求体都不能影响这两者;`resolve` 在第二因子(绑定了租户、所有者、Project、
  workspace、clone execution、凭据 id 与版本、仓库身份、Node 实例与运行时代次的单次授权)就绪之前显式失败,
  而不是降级(D6)。
- **失败是显式的**:客户端把"不可达"与"被拒绝"分成两种错误;`ErrRevoked`、`ErrConflict`、`ErrNotFound`、
  `ErrInvalidRequest` 各自映射到固定原因码;响应文本从不包含错误详情,日志从不包含 Token、密文、被封装 DEK
  或 KEK(D7)。

## 测试

- 单测全部离线且确定性。`store_test.go`:`secret_ref` 不透明且唯一、按 credential id 幂等、非法输入被拒、
  rotate 推进版本并保留旧版本、同 Token rotate 无操作、保留窗口到期后清理、revoke 形成终态墓碑、
  "已撤销"与"从未存在"可区分。`keystore_test.go`:启动闸门、keyring 形状非法即拒、可被其他账户读取的卷即拒、
  导出整个卷得不到 Token(含各种 base64/hex 编码形式)、`keyId` 持久化且旧 KEK 仍可解、换一把 KEK 必须失败。
  `server_test.go`:真实 mTLS 连接下的身份表校验、能力分离、无证书与未知身份被拒、跨传输幂等、调用方不得自带
  `secret_ref`、审计行不含任何秘密。`client_test.go`:真实 `Client` 对真实 `Server` 的完整生命周期往返
  (写入返回的引用就是记录里的引用、密封的材料就是交出的 Token、重放不产生第二份密文、rotate 保留旧版本、
  revoke 后重放创建被墓碑拒绝)、拒绝映射为稳定原因码且不回显 Token、错误身份在能力层被拒、
  存储不可达是显式失败而不是"被拒绝"、以及 `NewClient` 拒绝无法完成双向认证的部署。
- 端到端:`integration/git_credentials_test.go`(凭据生命周期与交接)、
  `integration/git_credential_ingress_test.go`(入口边界与跨用户/跨租户隔离)、
  `integration/git_credential_binding_test.go`(Project 绑定与 credentialed Clone 的 fail-closed)。
  它们使用 `core.CredentialSecretWriter` 的测试替身,因此不依赖本服务进程;本包与 Cloud 的真实一致性由
  上面 `client_test.go` 的往返覆盖。
- `internal/config/credential_store_test.go` 覆盖这一层的部署闸门:服务端的卷、KEK、服务证书、客户端 CA 与
  三个身份缺一即拒绝启动,`ServerTLS` 始终要求并校验客户端证书,以及 `CREDENTIAL_STORE_*` 与 Cloud 的
  `CLOUD_*` 前缀互不串用。
