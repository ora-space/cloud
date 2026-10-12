# internal/credentialstore: 独立凭据服务(Secret Store)

[中文](README.md) | [English](README.en.md)

`internal/credentialstore` 实现**独立的** Credential Service:保存用户自管的个人 HTTPS Git Token,以及
Cloud 侧把 Token 交接给它所需的 `ingress-writer` 客户端。它落实
`specs/decisions/cloud/identity-access/20261010-user-managed-git-secret-store.md` 的安全边界:服务是
独立进程,有自己的持久卷、自己的密钥材料、自己的信任根,与 Cloud 业务库不共享数据库实例、连接、凭据或卷(D1)。
Cloud 在同一次请求内交出 Token,此后只保留 `credential_refs` 元数据与不透明的 `secret_ref`;Cloud 进程
不持有解密能力(D2)。

本包刻意只服务凭据存储这一件事:凭据归谁、能否绑定到 Project、Clone 是否被允许,全部属于 `internal/core`;
本包只把"密封、轮换、销毁、按单次授权兑换、以及只对三种服务身份开放"做对。

## 文件

- `store.go`:`Config`(`Volume`、`KeyFile`、`Retention`、`AuthorizationPublicKeyFile`、`Now`)与
  `Store`(`Write`/`Rotate`/`Revoke`,以及只用于取回 `*Resolver` 的 `Resolver()`)。
  `Open` 是 D6/D8 要求的启动闸门——卷不可读或可被其他本机账户读取、KEK 缺失或不可用、卷未配置,都在此拒绝,
  绝不返回半初始化的 Store;`AuthorizationPublicKeyFile` 可以同时携带**多把**公钥,这正是"用旧钥签发的授权
  仍然有效时轮换 Cloud 签名钥"的前提,该集合在 `Open` 时一次性读入,因此一次兑换不依赖"文件此刻仍可读";
  `newSecretRef` 在服务内生成 24 字节(约 192 位)不透明引用(前缀 `cs_`),
  浏览器、终端用户与部署者都不能选择或预测;`prune` 只在写路径上清理保留窗口已过的旧版本,没有后台清扫任务。
- `keystore.go`:`Keyring` 与 `LoadKeyring`——部署挂载的 KEK 集(`primary` 加 `keys[]`,每个 32 字节
  base64),新封装一律用 `primary`,而封装记录自身携带 `keyId`,因此 KEK 轮换是纯部署变更、不需要数据迁移。
  每个密钥一份 DEK:`sealToken`/`openToken` 用 AES-256-GCM,`credentialID` 作为 AAD(密封块不能被挪到另一个
  凭据记录上);DEK 在 `defer clear` 中清零。`fingerprint` 是用 KEK 键控的 HMAC-SHA256,让重放被识别为同一份
  材料,同时使持有卷的攻击者无法离线校验候选 Token。任何不可用 KEK 一律报 `ErrKeyMissing`。
- `record.go`:`record`/`material`/`retained` 与 `fileStore`。卷内 `credentials/<id>.json`,一个凭据一条记录;
  `validCredentialID` 只接受规范小写 UUID,`fileStore.path` 再校验一次不让 id 逃出记录目录;写入走同目录临时
  文件加 `Sync` 后 `Rename` 原子替换。记录里的 `revokedAt` 是墓碑,`retained.retainUntil` 是旧版本的保留期限。
  `record.version` 按请求的版本号取出 `current` 或保留窗口内的 `previous`,取不到即"该版本不可用"——
  兑换时的版本绑定校验的是 Store 自己的状态,而不是授权里的自述。`authorizationDirectory`
  (`authorizations/`)是与 `credentials/` 并列的第二个目录,因为一次授权即使对应的凭据后来被销毁,它也已经被消费过了。
- `identity.go`:`Capability`(`write`/`resolve`/`keys`)、`IdentityConfig`(三个固定身份)与 `Identities`。
  三个身份必须存在、互不相同且形如 `spiffe://<信任域>/<kind>/<name>`;`PresentedIdentity` 从已被客户端 CA
  验证过的叶子证书 URI SAN 中取出身份,验证通过但不在表内的证书被拒绝,而不是当作匿名。
- `server.go`:`Server`——mTLS 加能力门控的 HTTP 面。`POST /v1/credentials`、`.../{id}/rotate`、`.../{id}/revoke`
  需要 `write` 能力,`.../{id}/resolve` 需要 `resolve` 能力;严格 JSON 解码(未知字段、非单一 JSON 对象、
  尾随内容都拒绝);`failure` 把 Store 错误映射为固定原因码,错误文本只进本服务日志;`audit` 每次操作记录
  operation/credentialId/serviceIdentity/result。`resolve` 在部署配置了验签公钥时执行真实兑换,未配置时
  仍以 409 `authorization_verification_unavailable` 拒绝,且拒绝发生在读取请求体之前。
- `authorization.go`:`Resolver` 与兑换路径。`ResolveRequest`(授权载体加出示方自述的 Node 实例与运行代次)、
  `ResolvedCredential`(Token 与版本)与六个可区分的原因错误(`ErrAuthorizationUnverifiable`/
  `ErrAuthorizationInvalid`/`ErrAuthorizationExpired`/`ErrAuthorizationConsumed`/`ErrAuthorizationBinding`/
  `ErrVersionUnavailable`)。`Store.Resolver()` 返回 `(*Resolver, bool)`:没有验签公钥就没有兑换能力。
  `Resolver` 持有整个**密钥集合**并交给 `cloneauth.Verify`,因此任何一把挂载的钥签发的授权都能被交付。
  `claimAuthorization` 以 `O_CREATE|O_EXCL` 创建消费记录,创建成功即原子单次消费判定;`markAuthorizationDelivered`
  在秘密交付前把结果从"已消费未交付"改成"已交付"。
- `client.go`:Cloud 侧 `ingress-writer` 客户端。`ClientConfig`(Endpoint 必须 `https`、不带 query 与
  fragment;TLS 必须带客户端证书与信任池)、`Failure`(带 `ReasonCode()`)、`Write`/`Rotate`/`Revoke`。
  刻意不重试:超时的写可能已经成功,恢复方式是调用方用同一个幂等键重发整个请求(D4)。
- 测试:`store_test.go`、`keystore_test.go`、`server_test.go`、`client_test.go`、`authorization_test.go`,
  以及 `process_test.go`(跨进程验收,见下)。

## 依赖与调用方

- 依赖:标准库(`crypto/aes`、`crypto/cipher`、`crypto/ed25519`、`crypto/hmac`、`crypto/sha256`、
  `crypto/rand`、`crypto/tls`、`encoding/json`、`net/http`、`net/url`)与 `go.uber.org/zap`;
  以及 `internal/cloneauth`——授权载体的形状与验签由签发方和兑换方共用的那一个包定义,Store 只持有公钥,
  因此它验的是"Cloud 签过名的绑定",而不是一份自述。本包**不**导入 `internal/core`。
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
- **能力不重叠**:`Store` 类型上没有读回或列举 secret 的方法,兑换能力在一个**独立的 `Resolver` 类型**上,
  只能通过 `Store.Resolver()` 取得——`write` 身份读不回任何 secret,`resolve` 身份不能写、列举或删除;
  同一证书持有两种能力在启动时即被拒绝,因为那种失败的运行时表现是"每次调用都认证成功"。把兑换放在
  `Store` 上也能通过测试,但那会把这条边界降级成调用方纪律。
- **没有匿名路径**:`NewServer` 要求配置了客户端 CA 的 mTLS;`route` 的身份来自已验证的 TLS 状态、能力来自
  部署固定表,URL、请求头、请求体都不能影响这两者。兑换需要**两个因子同时成立**,且都不取自调用方的断言:
  `broker-resolver` 服务身份由传输层与能力表强制,单次授权则由 Store 自己用部署挂载的 Cloud 公钥验签
  (D3)。未配置公钥的部署没有 `Resolver`,每一次兑换都以
  `authorization_verification_unavailable` 拒绝,而不是降级(D6)。
- **授权是单次的,且先记录后交付**:兑换顺序固定为"验签与绑定 → 校验凭据状态 → 写入消费记录 → 才交付秘密"
  (D10)。消费判定是消费记录文件的 `O_CREATE|O_EXCL` 创建,因此它是原子的、无需锁,也不依赖进程内存——
  同一授权并发兑换至多一次成功,Store 重启后仍然如此。交付前失败只留下"已消费未交付";反向的
  "已交付但消费记录缺失"不可能出现,因为交付前必然已经落盘。授权本身是 bearer 秘密:请求体里的授权在进入
  审计与失败日志之前被清零,消费记录只保存授权标识、消费时刻、出示它的 Node 实例与运行代次和结果。
- **过期的判定权在 Store**:`Resolve` 用 Store 自己的时钟判断授权是否过期,不采信载荷里的自述;两处时钟若有
  漂移,实际生效的是 Store 的时钟。这一点对 Store 有利是刻意的——决定是否交付秘密的是 Store。
- **运行时字段是一致性校验,不是身份校验**:`nodeId`、`nodeInstanceId`、`nodeIncarnationId`、
  `runtimeGeneration`、`controlEpoch` 来自出示方自己的请求,因此只是与授权绑定值比对,仅此而已。它们把一份
  授权钉在它被签发时的那一代运行时上,并不证明出示者是谁。Store 能独立核实的因子只有 mTLS 的
  `broker-resolver` 身份,而部署固定的三身份表让每个 Node 拿到的是同一张证书——所以 Store 分不出不同的 Node
  实例。要让它成为密码学结论,需要 per-Node 身份,而身份表目前没有这一项。尽管如此,一份授权仍然无法跨租户或
  跨凭据转移,因为绑定里的 credential id、版本与租户来自 Cloud 的签名,而不是来自调用方。
- **消费记录在每种失败模式下都 fail-closed**:记录存在却无法解析时算作已消费(`ErrAuthorizationConsumed`)
  而不是不存在,因此一个损坏的卷不可能把已消费的授权变回可用;记录根本写不下去时报错且不交付任何秘密;
  在消费判定与交付之间崩溃或断电,留下的是保守读数 `consumed_not_delivered`,且是终态。`syncDirectory`
  在 `openToken` 之前返回,所以"丢失了记录"的那次断电同时意味着秘密从未离开进程——"已交付却无记录"这个状态
  在两个方向上都不可达。
- **失败是显式的**:客户端把"不可达"与"被拒绝"分成两种错误;`ErrRevoked`、`ErrConflict`、`ErrNotFound`、
  `ErrInvalidRequest` 各自映射到固定原因码,兑换的六种拒绝也各有独立原因码——被拒绝的调用方要能区分"换一份
  新授权"和"去查问题",而合并成一个 `resolve_failed` 就做不到(D7)。响应文本从不包含错误详情,日志从不包含
  Token、密文、被封装 DEK 或 KEK。

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
- `authorization_test.go` 覆盖兑换路径,用的是真实文件卷、真实 KEK、真实 mTLS 与真实 `cloneauth` 签名:
  交付与绑定版本一致、同一授权第二次兑换被拒、**关闭并重开 Store 后已消费的授权仍被拒**(重启持久性)、
  伪造签名、篡改载荷、过期、绑定错配(credential id、Node 实例、代次、epoch 等六项)、运行时声明不完整、
  版本不可用、保留窗口内的旧版本仍可兑换、已撤销、未知与非法 credential id、取消、校验失败时授权不被消耗、
  未配置公钥时无兑换能力、**八个 goroutine 并发兑换恰好一次成功**,以及跨真实 TLS 的交付、重放拒绝码
  与"审计行不含秘密"。上面那些失败语义各有专门用例:消费记录无法解析时读作已消费、卷写不下消费记录时不交付
  任何秘密、秘密打不开时留下 `consumed_not_delivered` 且第二次尝试被拒。轮换则跨真实重开覆盖:两把钥都在时
  任一把签发的授权都能交付,从文件里撤下的钥不再能验过它签过的任何东西。
- `process_test.go` 是跨进程验收。它编译真实的 `cmd/credential-store`,把真实配置文件、真实 PKI、真实 KEK
  keyring 与真实 Ed25519 密钥对写到磁盘,以自带环境变量的子进程方式启动该二进制,并通过真实的双向认证监听
  驱动它:一份授权恰好换到一个 Token、重放是 `authorization_consumed`、**进程重启后同一重放仍被拒**、之后
  新授权照常可用、消费记录落在服务自己的卷里且不含 Token、Token 与任何授权都不出现在服务的审计日志里。
  另有独立用例覆盖:真实传输上的能力分离(有效授权对 `ingress-writer` 身份毫无价值,`resolve` 身份不能写)、
  wire 上的拒绝矩阵(不可信密钥、错的运行时、已过期、根本不是授权——之后一份有效授权仍能兑换,证明这些拒绝
  没有消耗任何东西)、匿名客户端在握手阶段就被拒、以及双钥部署能交付任一把钥签发的授权。它刻意**不**覆盖的
  是:真实 **Cloud 服务进程**签发授权。今天没有这样的 Cloud 进程——`Issue` 没有生产调用方,因为它的每个调用点
  都在 `requireRepositoryAccess` 的无条件拒绝之后——所以这里的签发跑的是 Cloud 的真实代码与真实挂载的签名
  私钥,调用方是测试而不是 Cloud 服务进程,这就是全部的差距。
- 端到端:`integration/git_credentials_test.go`(凭据生命周期与交接)、
  `integration/git_credential_ingress_test.go`(入口边界与跨用户/跨租户隔离)、
  `integration/git_credential_binding_test.go`(Project 绑定与 credentialed Clone 的 fail-closed)。
  它们使用 `core.CredentialSecretWriter` 的测试替身,因此不依赖本服务进程;本包与 Cloud 的真实一致性由
  上面 `client_test.go` 的往返覆盖。集成套件里没有真实 credential-store 进程,所以兑换路径的真实 Store
  覆盖在本包的进程内 mTLS 测试里完成,而不是在那里。
- `internal/config/credential_store_test.go` 覆盖这一层的部署闸门:服务端的卷、KEK、服务证书、客户端 CA 与
  三个身份缺一即拒绝启动,`ServerTLS` 始终要求并校验客户端证书,兑换能力跟随验签公钥(未配置即无能力、
  已配置即有、配置了却不可用则启动失败,三种状态互不混淆),以及 `CREDENTIAL_STORE_*` 与 Cloud 的
  `CLOUD_*` 前缀互不串用。
