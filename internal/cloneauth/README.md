# internal/cloneauth: 单次授权的签发与验签

[中文](README.md) | [English](README.en.md)

`internal/cloneauth` 定义**单次授权**这一载体的形状,以及 Cloud 签发、Credential Service 验签的两个
函数。它落实 `specs/decisions/cloud/identity-access/20261010-user-managed-git-secret-store.md` 的 D3 与 D10:
Store 必须能**自己**判定一份授权是否真实,而 D10 又禁止签发方在签发阶段往 Store 里预登记任何东西
("Cloud 只做绑定与签发,不做任何'已用'标记,也不预留消费记录")。

载体目前承载两种用途:`OperationClone`(一次 clone 执行取回自己的检出)与 `OperationMirrorPrepare`
(为一次 Project Operation 取回上游仓库的一个分支,用于发布内部镜像)。两者是**同一个载体上的两种
operation**,不是同一个 operation 上的一个开关:它们授权的对象不同,因此绑定字段也按 operation 分成
互斥的两组。

## 为什么是这个形态

两份 ADR 规定了授权绑定什么、活多久、在哪里被消费,但**没有**规定它的密码学形态:没有 HMAC、签名、公钥、
JWT 或句柄的表述;Store 侧要做的校验只列了三条(mTLS 对端是 `broker-resolver`、绑定与实际请求一致、未过期
且尚未被消费),其中**没有**"验真伪"。

这个留白必须补上,否则任何持有 `broker-resolver` 证书的进程都能自造一份授权,去兑换**任意租户**的凭据,
`20261010-controlled-node-git-authentication-isolation` D14-B 第 11 项要求的多租户隔离就只剩调用方纪律。
而"签发时在 Store 里登记一条待消费记录"被 D10 明确禁止。两条夹起来只剩一个形态:**Cloud 用 Ed25519 签名,
Store 只持有公钥验签**。Store 的卷与进程同时被攻破也造不出授权,这正是 D10 那条消费记录还有意义的前提。
密钥格式与部署已有的做法一致:`openssl genpkey -algorithm ED25519` 写出的 PKCS#8 私钥与 PKIX 公钥。

## 文件

- `authorization.go`:`OperationClone`、`OperationMirrorPrepare`、`MaxLifetime`(15 分钟)、六个哨兵错误、
  `Binding`、`validate` 与按 operation 拆分的 `validateClone`/`validateMirrorPrepare`、`Issue`、
  `Verify`、`split`、`LoadSigningKey`、`LoadVerificationKeys`。
- `capability.go`:capability 词表(`clone`)、`CanonicalCapabilities`/`HasCapability`,以及
  `permittedCapabilities`——它把 operation 映射到**唯一**允许的 capability 集合。
- `ref.go`:`ValidateRef`——镜像用途钉住的分支名,必须且只能是 `refs/heads/...`。
- `repository.go`:`CanonicalRepository`——规范化仓库身份。
- `authorization_test.go`:签发/验签往返、20 个不完整绑定的表驱动用例、上限边界、8 种畸形 token、
  篡改载荷、他人密钥、时钟由验签方决定、不可用的密钥、密钥文件往返与 9 种不可用密钥文件。
- `ref_test.go`:22 个 ref 形状、镜像绑定往返、ref 确实在签名载荷之内。
- `purpose_test.go`:两种用途互斥字段的双向负向矩阵、有效签名下的混用载荷仍被拒、未知 operation、
  以及两种用途各自携带的身份字段。

## 接口

- `Binding`:授权绑定的一切——授权标识、`operation`、租户与所有者、Project、workspace、凭据 id 与版本、
  规范化仓库身份、签发与到期时刻。**其余字段由 operation 决定,并且是双向强制的**:
  - `OperationClone` **要求** `executionId` 与整组 Node 运行时 fencing 值:`nodeId`、
    `nodeInstanceId`(`node_instances.id`)、`nodeIncarnationId`(每个 Node 进程一个);
    **禁止** `ref`、`operationId`、`preparationGeneration`、`executorIdentity`。
  - `OperationMirrorPrepare` **要求** `operationId`(Project Operation)、`preparationGeneration`(≥1)、
    `executorIdentity`(SPIFFE)、`ref`(`refs/heads/...`);**禁止** `executionId` 与整组 Node 运行时值。
  - 两种 operation **共用** `runtimeGeneration`(`sandbox_instances.generation`)与 `controlEpoch`
    (`runtime_controls.control_epoch`):镜像准备同样被 Workspace 的运行时代次栅栏,迟到的准备不得在已经
    前移的运行时下发布。

  两组字段**不允许同时出现**。"忽略另一个用途的字段"是不够的:一份同时带着 execution 与 Node 运行时、
  又带着 operation id 与准备代次的载荷,读起来两种解释都成立,operation 就退化成一个标签。这不是措辞
  问题,而是"把 Operation ID 填进 execution 字段"这类兼容捷径必须被结构性排除的原因。同样是这个理由,
  clone 用途刻意**不签**分支名(它放在 job 自己的字段里),镜像用途则必须签——发布的镜像以 ref 解析出的
  commit 为键,读者拿它和 Cloud 的行比对,ref 因此必须是签发方钉住的那一个。

  fencing 值刻意**不用** Controller 租约的 `controller_leases.epoch`:它标识的是租约持有者,与 Node 实例无关,
  混用会让 fencing 指向错误的代次。

- **"谁可以花掉它"由 operation 决定,而且两个用途的答案来源不同。** clone 用途绑定一个 Node 运行时,那几个值
  Cloud 已经通过 `runtimeBinding` 下发给 Node,所以出示方能如实自述,Store 比对的是**请求自述**的值。镜像用途
  没有这个前提:C 跑在 Node 之外,没有运行时可自述,所以它绑定的是 `executorIdentity`——Store 从**验过的客户端
  证书**里解析对端身份,能花掉这份授权的因此是部署签发了证书的那一方,而不是请求体里写着的名字。
- `Issue(key, Binding) (string, error)`:载体是 `base64url(payload) + "." + base64url(Ed25519(payload))`。
  签名覆盖**实际传输的字节**,因此验签不依赖任何 JSON 规范化,字段顺序或转义差异既不会让一份真实授权失败,
  更不会让两份不同的载荷验成同一份。
- `Verify(keys, token, now) (Binding, error)`:接受一组公钥,任一把验过即通过——这正是轮换期间"用旧钥签发的
  授权不被拒绝"的前提。顺序固定为"验签 → 解析 → 校验绑定 → 判过期"。签名失败与格式错误是**两个**错误:
  格式正确但验签不过,是一次伪造的特征,而不是 bug 的特征。空密钥集报 `ErrKey`,而不是等到第一份 token 才
  拒绝,于是"配成空"的错误在犯错的地方就暴露。
- `LoadSigningKey`/`LoadVerificationKeys`:只接受 Ed25519。给错算法(例如 ECDSA)、给错半边(公钥当私钥用)、
  非 PEM、文件不存在,一律报 `ErrKey`,而不是带着一把什么都验不过的密钥启动。`LoadSigningKey` 要求文件里
  **恰好一个** PEM 块:一把"若干把之一"的签名私钥不是调用方可能想要的东西。`LoadVerificationKeys` 读取**每一**
  块,只要有一块不可用就拒绝整个文件——半完成的轮换因此是启动失败,而不是"比运维以为的少信一把钥"的部署。

## 不变量

- **签发方自己决定时钟**:`Issue` 不接受"现在",而是由调用方在 `Binding.IssuedAt`/`ExpiresAt` 里给出,
  因此签发在测试里是确定性的,部署的时间源也只有一处。
- **有效期有硬上限**:D5 要求授权不长于一次 clone 的命令超时。本包看不到那个超时,于是强制一个上限——
  真实值由调用方选,而一个 bug 或一次配错不能把单次授权变成长期 bearer token。
- **过期由验签方的时钟判定**:`Verify` 用调用方传入的 `now`,不采信载荷里的自述。签发方与 Store 的时钟若有
  漂移,结论落在 Store 一侧——决定是否交付秘密的是 Store,这个方向是 fail-closed 的方向。
- **用途混淆是结构性的,不是靠一次检查。** 一份授权不可能同时是 clone 和镜像准备:两组身份字段互斥,缺字段
  与混字段都在签发与验签两侧被拒,未知 operation 也被拒而不是回落到 `clone`。因此消费方必须**比对 operation**
  才能决定要不要花掉它,而不能靠"请求是从哪条路由进来的"来推断用途。
- **不完整即拒签**:`validate` 在签发与验签两侧都跑。签发侧挡住"造一份畸形授权",验签侧挡住"一份签过名但
  畸形的载荷"进入 Store 的校验流程;因此 Store 永远不必判断空的租户或零代次意味着什么。两侧都跑是必需的:
  到达 Store 的是一串别人产生的字节,一份由规则更宽的构建签出的 token 只能在这一侧被挡住。
- **授权是 bearer,是秘密**:本包不打印、不记录、不缓存它;调用方必须保证它不进日志、审计与任何持久介质
  (Store 侧的消费记录只保存授权标识,不保存载体)。

## 公私钥归属、轮换与失效

- **私钥归 Cloud,且只归 Cloud。** 它以文件引用交付,绝不作为配置值(`cloud/AGENTS.md` 禁止把部署秘密写进
  配置文件),也绝不挂载进 Credential Service。Store 只持有公钥,所以"卷与进程同时被攻破"的 Store 能验签却
  造不出授权——这正是 D10 那条消费记录还有意义的前提。不存在带内密钥分发:两个文件都由部署放置,token 里
  也不带密钥标识,因此调用方无法选择用哪把钥。
- **轮换是部署的事,而它之所以可行,是因为验签文件可以同时携带多把公钥。** 重叠期是:把新公钥追加进验签
  文件 → 重启 Credential Service → 把 `Issue` 切到新私钥 → 等过 `MaxLifetime` → 移除旧公钥 → 再重启。
  整个窗口内两把钥都能验过,因此没有一份"在旧钥下签发、且仍然有效"的 clone 授权会被拒绝。
- **失效 = 移除 + 重启。** Store 在启动时一次性读取验签文件,这样一次兑换就不会依赖"文件此刻仍可读":
  运维卸载或损坏它,并不会因此把能力关掉。这个选择的代价是失效要到下次重启才生效,而让这个延迟可以接受的
  正是 `MaxLifetime`——攻击者用偷来的私钥签出的任何授权,都会在十五分钟内失效,无论部署有没有察觉。这个
  上界也就是上面那套轮换流程不需要协同割接、不需要密钥版本协商的原因。

## 依赖与调用方

- 依赖:仅标准库(`crypto/ed25519`、`crypto/x509`、`encoding/base64`、`encoding/json`、`encoding/pem`、
  `errors`、`fmt`、`os`、`slices`、`strings`、`time`)。不依赖 `internal/core`,也不依赖任何传输层。
- 调用方:`internal/credentialstore` 用 `Verify` 与 `LoadVerificationKeys` 实现兑换;
  `internal/cloneexec`(凭据执行器 C)用同一组函数在花掉任何东西之前**本地**拒掉一份它不认的授权,并在
  `matchBinding` 里比对 `binding.Operation != OperationClone`——这是 clone 路由拒绝镜像授权的落点,发生在
  Store 被询问之前。
- 签发侧(`Issue`/`LoadSigningKey`)的生产调用点有两个,都由 `cmd/server` 从同一段配置
  (`clone_dispatch`,Go 类型 `ExecutorDispatchConfig`)装配,因为两者共用同一个 executor 端点、
  同一张 dispatcher 证书和同一把签名私钥:clone 走 `internal/core` 的 `CloneDispatcher.Execute`,
  mirror 走 `MirrorDispatcher.Prepare`。
- **两个能力各自独立开关、默认都关**:`CLOUD_CLONE_DISPATCH_ENABLED` 为 `false` 时
  `requireRepositoryAccess` 仍在派发之前无条件拒绝,因此一次也不会签发;私钥文件也因此不是必需配置,
  缺它时 Cloud 照常启动并保持 fail-closed。`CLOUD_CLONE_DISPATCH_MIRROR_ENABLED` 只装配镜像准备,
  不改变 clone 路径的行为——这样镜像链路可以先行接通,而每一次带凭据的 clone 依旧被拒。
  **配了就必须可用**:`ExecutorDispatch.Open()` 在"启用但不可用"(缺端点、缺私钥、缺证书、缺执行器身份)
  时让进程启动失败,而不是运行期降级。
- 镜像用途多一项部署事实:`CLOUD_CLONE_DISPATCH_EXECUTOR_IDENTITY` 必须与
  `CREDENTIAL_STORE_BROKER_RESOLVER_IDENTITY` 逐字一致,因为 Store 比对的不是载荷里的自述,而是它从
  **已验证客户端证书**解析出的身份。写错不会静默通过:兑换被拒,凭据一次也不会被交付。
  镜像授权寿命另受 `core.MirrorPreparationLease` 约束,因为授权不该比它服务的尝试租约活得更久。

## 测试

`authorization_test.go` 全部离线且确定性,覆盖:签发-验签往返后绑定逐字段不变(这是 Store 能校验绑定而非
只校验签名的前提)、20 个不完整绑定各自被拒(空租户、零版本、零代次、零 epoch、到期不晚于签发、超上限
等)、恰好等于上限的寿命被接受、8 种畸形 token、篡改载荷后签名失败、用他人密钥签发的 token 被拒、
过期由传入时钟判定(到期时刻本身即已过期)、零值密钥被拒,以及 PKCS#8 私钥与 PKIX 公钥的 PEM 往返与
9 种不可用密钥文件(含另一种算法的 ECDSA 密钥)。轮换行为有直接覆盖:两把钥的集合里任一把签的 token 都能
验过、从集合中撤下的钥不再能验过它签过的任何东西、挂载文件的每一块都被读取、只要有一块不可用就拒绝整个
文件、签名私钥文件含多于一块时被拒。

用途分离由 `purpose_test.go` 覆盖:14 个"改一个字段"的用例证明两种用途双向互斥(含把 Operation ID 填进
execution 字段这一种),另有三个用例手工签名——不经 `Issue`、只保留真实签名——证明**有效签名也不能让一份
混用或未知 operation 的载荷通过 `Verify`;`OperationClone` 与 `OperationMirrorPrepare` 各自携带本用途的
身份字段而不携带对方的,则是消费方能靠 operation 分辨用途的前提。`cloneexec` 侧另有
`TestExecuteRefusesAMirrorAuthorizationOnTheCloneRoute`:一份合法签发、且仓库/凭据/版本/代次都与 job 一致的
镜像授权,在 clone 路由上必须以 `binding_mismatch` 被拒,且 Store 一次都没有被询问。
