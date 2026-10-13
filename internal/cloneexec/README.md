# internal/cloneexec: 凭据执行器(credentialed-clone executor)

[中文](README.md) | [English](README.en.md)

`internal/cloneexec` 是**当前正式产品的独立部署单元 C**:把 Cloud 签发的一份单次授权换成一次私有仓库
`git clone`,然后让这次 clone 用过的凭据无法被找回。它落实
`specs/decisions/node/repository/20261010-controlled-node-git-authentication-isolation.md` 的 D1/D2/D5/D6/
D7/D11/D13,以及把 C 的实现归属改判给当前产品的后续 ADR
`specs/decisions/node/repository/20261012-credential-executor-belongs-to-the-current-product.md`。

已批准决策在跑 clone 的机器上固定了三个身份——M(管理/TCB)、W(Agent 工作负载)、C(凭据执行),并要求 Token
只存在于 C 之下、放在 W 不可遍历的目录里、生命周期不长于一次 clone。本包就是 C,**以独立进程、独立身份、
独立卷、独立监听端口**落地,于是那条边界是两个进程之间的操作系统边界,而不是一个进程内部的纪律。

C 今天承担**两项能力**,走**两条路由**:`POST /v1/clone-executions` 把授权换成一次 `git clone`,
`POST /v1/mirror-preparations` 把授权换成一份内部镜像。两者**共享**部署、验签、兑换与 Git 调用方式,
**不共享**路由、请求类型、结果类型与消费方——不在同一个 Job 上加 `kind` 开关,理由见下文"两条路由,不是一个开关"。

它不运行任何 Agent 代码,不持有任何 Agent 状态;两次作业之间它什么都没有。它与 Node 内的工作负载今天分处
不同容器,因此同一文件系统内的 W/C 互斥性来自部署(见
`specs/test-cases/node/repository/git-credential-execution-isolation.md` 的如实标注),不是本包能自证的。

## 顺序就是契约

`Executor.Execute`(`executor.go:317`)是五步,顺序本身是不变量:

1. **本地先验、先不花钱**:用部署的验签公钥验授权,再逐项比对今天不花任何代价就能比对的绑定——execution、
   仓库、运行时 fencing 值(`matchBinding`,`executor.go:439`)。在这一步失败的作业**什么都没消耗**。
2. **以 `broker-resolver` 身份到 Credential Service 兑换**:授权在这里被花掉,而且由 Store **持久地**只花
   一次(`storeclient.go` 的 `Resolve`)。
3. **物化**:在物化根下建一个以 execution 命名的目录(0700),里面一个 0600 的文件(见
   `internal/credentialmaterial`)。
4. **一次受约束的 `git clone`**:唯一的 credential helper 是部署固定的那个程序,它只读那一个文件
   (`git.go` 的 `cloneArguments`)。
5. **销毁并证明已销毁**:第 3、4 步的**每一条**出口——成功、Git 失败、超时、取消、panic——都要走
   (`Execute` 的 `defer`)。

3 到 5 的次序让物化的存活期落在作业内部:Store 决定交付之后才创建,返回结果之前已销毁。崩溃是这条次序唯一
覆盖不到的情形,那是启动清扫(`Sweep`,`executor.go:304`)存在的理由——它在开始监听之前先跑。

## 两条路由,不是一个开关

两项能力各有各的路由、请求类型与结果类型,而且是两个导出方法,不是同一个方法上的开关:

| | Clone | 镜像准备 |
| --- | --- | --- |
| 路由 | `POST /v1/clone-executions` | `POST /v1/mirror-preparations` |
| 请求 / 结果 | `Job` / `Result` | `MirrorJob` / `MirrorResult` |
| 授权 operation | `clone` | `mirror_prepare` |
| 主体 | 某台 Node 上的某次 execution 的 checkout | 某个 Project Operation 的某一次 attempt |
| 身份字段 | `executionId`、`nodeId`/`nodeInstanceId`/`nodeIncarnationId` | `operationId`/`preparationGeneration`/`executorIdentity` |
| Cloud 一侧的方法 | `Client.Dispatch` | `Client.PrepareMirror` |

理由是主体不同:clone 绑定"某台 Node 上的某次执行",镜像准备绑定"某个 Project Operation 的第几次尝试",两者
的身份字段**没有交集**——`Binding.validate` 对 clone 要求三个 node id 且**禁止** operation/generation/
executorIdentity,对镜像要求这三项且**禁止**三个 node id。若合成一个带 `kind` 的 Job,解码器就要自己判断哪些
身份字段有意义,operation 边界会退化成 C 内部的一个分支,而不是一道 Cloud 无法伪造的签名(2026-10-13 裁决)。

**用途混淆在两个方向都必须被拒**,而且拒法是比对**签名里的 operation**,不是比对请求落在哪条路由:

- 镜像路由遇到 `clone` 授权 → 400 `binding_mismatch`(`matchMirrorBinding` 的第一项),发生在**向 Store 要
  任何东西之前**;
- Clone 路由遇到 `mirror_prepare` 授权 → 400 `binding_mismatch`(`matchBinding` 的同类检查)。

两边的"没被消费"都不靠 C 的承诺作证,而是靠事后经**该授权本该走的那条路**兑换成功来证明(见"测试")。

## 镜像准备的顺序

`MirrorExecutor.Prepare`(`mirror.go:264`)是同样的"顺序即契约",只是终点从 checkout 换成 publication:

1. **本地先验、先不花钱**:规范化仓库 → 校验 `operationId`/`preparationGeneration`/`projectId`/凭据 id → 用
   签发方自己的 `cloneauth.ValidateRef` 校验 ref → 验签 → `matchMirrorBinding`(第一项就是 operation)。
   在这一步失败的作业**什么都没消耗**。
2. **以 `broker-resolver` 身份兑换**:`Store.ResolveMirror` 在 Store 侧**先**判用途(不符 → 403
   `authorization_purpose_mismatch`)与出示身份,**再**持久地只消费一次。
3. **物化**:目录名取**授权 id**(`mirrorMaterialIdentity`),不是 operation id——授权只花一次,所以同一
   operation 的两次尝试(包括 Cloud 已经重试的那次)不可能撞进同一个目录。
4. **一次受约束的 fetch**:`init --bare` 后只 fetch 那一个签名 ref,再
   `rev-parse --verify --quiet refs/ora/mirror^{commit}` 把 commit 读回来;`for-each-ref` 必须**恰好**只有
   `refs/ora/mirror` 一条(`mirrorpublish.MirrorRef`),并 `symbolic-ref HEAD refs/ora/mirror`。
5. **发布并销毁**:`Preparation.Begin` →若该 commit 已发布则返回既有记录(按 commit 幂等)→ 否则 `Adopt`
   移入 store 的 staging 后再 `Publish`;物化的销毁与 fetch 目录的移除在**每一条**出口上执行,销毁失败照样
   报错——凭据可能还在盘上的准备不是一次成功的准备。

准备超时取"部署配置值"与"授权剩余有效期"的**较小者**,于是一份即将过期的授权不会让一次 fetch 跑过它的
有效期。

## 文件

- `executor.go`:`Config`、`Executor`、`New`、`Sweep`、`Execute`、`Job`、`Result`、`Runtime`、`Failure` 与
  七个 reason code。
- `git.go`:受限 Git 调用的全部构造,以及**两条路径共用的**那份 Git 环境——`gitDeployment`(两条路径不能
  在"用哪个 helper、信哪个 CA"上分叉)、`gitEnvironment`、`repositoryScope`、`validateExecutablePath`、
  `runGit`、`redactFailure`;clone 一侧的 `cloneArguments`、`helperArgument`、`validateBranch`、
  `headCommit`。
- `mirror.go`:第二项能力——`MirrorJob`、`MirrorResult`、两个 reason code
  (`credential_executor_fetch_failed`/`credential_executor_publish_failed`)、`MirrorConfig`、
  `MirrorExecutor`、`NewMirrorExecutor`、`Sweep`、`Prepare`、`matchMirrorBinding`、
  `mirrorMaterialIdentity`。
- `mirrorcontent.go`:镜像准备里跑 Git 的那一半——`mirrorArguments`(与 `cloneArguments` 同类,固定
  helper、`credential.useHttpPath`、`protocol.*`、hooks/symlinks/LFS,并做一次 `--bare` fetch)、
  `validateMirrorRef`、`fetch`(init/fetch/rev-parse/for-each-ref/symbolic-ref)、`gitFact`、`publish`。
- `server.go`:双向认证 HTTP 监听(`Server`、`ServerConfig`、`NewServer`),两条路由,只接受部署固定的
  派发身份(`presentedIdentity`/`callerIdentity`),请求体上限 16 KiB(`maxJobBody`)。两条路由**共用同一份
  派发身份**:同一个出示方在两条路由上得到不同答案,正是"判据是签名里的 operation,不是路由"的可观测形式。
- `client.go`:调用方一侧的客户端(`Client`、`ClientConfig`、`NewClient`、`Dispatch`、`PrepareMirror`、
  `DispatcherRefusal`)。
- `storeclient.go`:`RedemptionClient` 接口与 `StoreClient`——`broker-resolver` 的 mTLS 客户端、十个 Store
  侧 reason code、`Credential`。
- `executor_test.go`:18 项部署变异、argv 与环境的逐位固定、拒绝路径、失败仍销毁、清扫。
- `mirror_test.go`:13 项镜像部署变异、20 项"花钱前先拒"、argv 固定、发布内容、按 commit 幂等、fetch 失败
  仍销毁、fetch 目录清扫。
- `acceptance_test.go`:真实私有仓库的 clone 端到端验收(见下文"测试")。
- `mirror_acceptance_test.go`:真实私有仓库的镜像准备端到端验收,以及两个方向的用途混淆矩阵(见下文"测试")。

## 接口

- `Config`:物化根、checkout 根、Git 的 `HOME` 目录、helper 与 Git 的绝对路径、可选的 CA 文件、Cloud 的验签
  公钥集、`RedemptionClient`、超时、时钟、日志。**每一项都是部署固定的**:没有一项来自作业,因此一份作业
  不能把 clone 指向自己的信任锚或自己的程序。
- `New(cfg)`:不完整就拒绝启动,而不是运行期降级。物化根与 checkout 根必须是**不同**的目录、helper 与 Git
  必须是绝对路径且不含 shell 元字符、helper 不能被 C 自己改写、`HOME` 必须存在且恰为 0700——Git 把那个目录
  当 `HOME` 读,另一个账户写得进去的 home 就是别人能种下的 `.gitconfig`,而 `GIT_CONFIG_GLOBAL=/dev/null`
  只中和了"被指定去读的那个文件",没有中和 Git 可能回退到的目录。
- `Execute`:作业的授权是 **bearer 秘密**,本包不打印、不记录、不写进任何地方;日志每作业一行,只含
  execution id、仓库与 reason code。返回的 `*Failure` 带稳定 code 且 `Unwrap` 到 cause——拒绝发给调用方,
  cause 只进日志,因为告诉调用方"你伪造的授权是格式错、过期还是验签不过"等于帮它迭代伪造。
- `Sweep`:`(移除的条目数, error)`。启动时跑,移除上一次运行留下的每个物化目录。
- `MirrorConfig` / `NewMirrorExecutor`:第二项能力的部署描述与构造。它**复用** clone 的 Git 与物化输入
  (`gitDeployment` 校验的是同一组边界:helper、Git、`HOME`、CA、物化根),因为一个 C 身份只有一个物化根,
  第二个物化根就是第二个 Token 可以比作业活得更久的地方;它**额外**要求验签公钥集、`MirrorRedemptionClient`
  与 `*mirror.Store`,三者缺一即拒绝启动。
- `MirrorExecutor.Prepare(job)`:`(MirrorResult, error)`,顺序见上一节。返回的 `MirrorResult` 四个字段都是
  Cloud 无法从自己发出的东西里推出来的事实:commit、单条 ref、规范化后的上游仓库、以及 mirror store 的
  发布代次——最后一项才是让 Cloud 能区分"它批准的那份镜像"与"同一 key 下的后来一次重新发布"的东西。
- `MirrorExecutor.Sweep`:`(移除的条目数, error)`,移除 fetch 根下上一次运行留下的目录。它**不碰** mirror
  store:那里被遗弃的 staging 由 store 自己的 sweep 回收,因为只有 store 能取到那把证明写者已死的 key 锁。
- `Client.PrepareMirror(job)`:与 `Dispatch` 并列的第二个方法,POST 到 `/v1/mirror-preparations`,共用
  `maxJobBody` 上界与 `DispatcherRefusal` 映射。它是**方法**而不是 `Dispatch` 的模式,理由与执行器有两个
  handler 相同:clone 指名一次 execution 与一个 Node,镜像准备指名一个 Operation 与一次 attempt,而"这次发的
  是哪种"是调用点的事实,不是某个字段的事实。
- `Server`/`Client`:HTTPS + JSON。载体是普通 HTTP 套 TLS,理由与 Credential Service 相同——平台的服务身份
  模型就是"固定 client CA 的 TLS + 客户端证书里的 SPIFFE URI",`net/http` 表达得完整;而这个端点**不属于**
  公开契约,决策明确要求授权不经业务协议传输。

## 不变量

- **拒绝而不是降级**:任一必需项缺失即启动失败;没有授权就拒绝作业;兑换失败就失败整次 clone,绝不退回匿名
  或静态 Git 身份。`ReasonCodeUnavailable`(`credential_executor_unavailable`)是"这个部署没有建立边界",
  与"这一次 clone 被拒"和"clone 本身失败"是三个不同的答案。
- **销毁失败不算成功**:`ReasonCodeCleanupFailed` 即使 clone 成功也照样报错——凭据可能还在盘上的 clone 不是
  一次成功的 clone(`TestExecuteReportsACleanupFailureRatherThanASuccess`)。
- **argv 与环境是固定的**:`cloneArguments` 逐位固定 helper 路径、`credential.useHttpPath=true`(它才是让
  Git 报出仓库路径、供 helper 与物化 scope 比对的那一项)、`http.followRedirects=false`、代理键、以及
  `core.hooksPath`/`core.symlinks`/子模块/LFS;`gitEnvironment` 是克隆进程的**全部**环境
  (`GIT_CONFIG_GLOBAL`、`GIT_CONFIG_NOSYSTEM`、`GIT_TERMINAL_PROMPT=0`、`GIT_ASKPASS` 等)。Token 不进
  argv,只走 helper 与 Git 之间的 `credential` 协议。
- **不运行仓库给的东西**:没有 hook、没有 LFS filter、没有子模块、没有符号链接跟随——每一项都是授权没有覆盖
  的程序或取数动作。
- **失败信息不回显秘密**:`redactFailure` 在错误串里抹掉 Token;`Failure` 不带任何凭据片段。
- **checkout 与物化根权限不同是有意的**:checkout 是交接面,0700 的物化根与 0755 的 checkout 根不能共用一套
  权限叙事——clone 里没有秘密,而物化根里全是。
- **用途由签名决定,不由路由决定**:两条路由各自先比对**授权里的 operation**,再问 Store 要任何东西。因此
  "把 clone 的作业发到镜像路由"不会换来一份镜像,也不会替 Node 花掉那次 clone——它换来的是 400
  `binding_mismatch`,而那份授权仍然可以在它本该走的路由上兑换成功。
- **只 fetch 签名里的那一个 ref**:ref 由授权签名携带并经 `cloneauth.ValidateRef` 校验,发布的镜像又用
  `for-each-ref` 断言**恰好**只有 `refs/ora/mirror` 一条,所以读镜像的人拿到的 commit 就是 Cloud 行里那个
  commit,而不是派发方想让它是的那个。
- **fetch 目录生活在 mirror store 之内、由构造推导而非配置**:`fetchRoot = <mirror root>/fetch`,于是它移入
  store staging 的动作按构造是同文件系统内的 rename;而部署若把镜像根与物化根互相嵌套,`NewMirrorExecutor`
  直接拒绝启动(见下节)——因为启动清扫会删掉物化根下的**每一个**条目。

## 部署归属与故障清理

- 身份:C 是**非 root** 的部署单元自身进程身份(后续 ADR D3;正式部署为 uid 10002,与 Cloud 的 10001、Node
  内 W 的 1000 互异)。它不持有 Cloud 的签名私钥,也不持有派发身份;它只持有 `broker-resolver` 兑换身份。
- 卷:物化根、checkout 根与 `HOME` 由部署创建为 C 独占的 0700;helper 与执行器本体 root 所有且 C 不可写,
  路径由部署固定。`cluster/compose/clone-executor-setup.sh` 造这些目录,`cluster/scripts/
  clone-executor-boundary.sh` 在运行栈上复核这组边界。
- 数据卷必须跨重启保留(`clone-executor-data`):丢了它,崩溃后残留的物化目录就无法被清扫。
- 镜像卷是**另一个**卷:镜像根里放的是内部 Git Server 要对外提供的已发布内容,物化根里只有秘密。因此部署层
  (`internal/config` 的 `MirrorExecutor`)在启动时拒绝把镜像根配成与物化根或 checkout 根互相包含的目录——镜像
  根嵌在物化根里会被启动清扫(`credentialmaterial.Sweep` 移除物化根下的**每一个**条目)连同已发布内容一起删掉,
  反向嵌套则把凭据材料放到 Git Server 供人读取的卷上。两项都不是一个部署应该能描述出来的状态,所以两项都不是
  它能启动的状态。**未声明镜像根 = 没有这项能力**,路由如实回 `credential_executor_unavailable`,而 clone 路径
  完全不受影响——这与设备上是否跑 Git Server 是可分的两件事。
- 清理面:作业内 `defer` 覆盖成功/失败/超时/取消/panic;进程外由 `Sweep` 覆盖崩溃。**尚未有直接证据**的是
  真实强杀加重启的端到端清扫,以及"超时"与 Node clone 状态机的 `Interrupted` 判定——执行器今天不接入 Node
  的 clone 状态机(见 `20261012` D4),这一点在核实用例里如实标为 `Partial`,不当作已完成。

## 依赖与调用方

- 依赖:`internal/cloneauth`(验签)、`internal/credentialmaterial`(布局)、镜像一侧另有 `internal/mirror`
  (发布容器)与 `internal/mirrorpublish`(那一条固定的内部 ref 名),以及 `zap`;其余为标准库。
  **不依赖** `internal/core`,也**不依赖** `internal/credentialstore`:Store 的十个拒绝码在 `storeclient.go`
  里**刻意重复声明**而不是导入,因为执行器依赖的是那套线上词汇,不是 Store 的实现——两个组件各有各的二进制,
  Store 若被换掉,执行器上报的码必须仍然表示同一件事。
- 调用方:`cmd/clone-executor` 是它的部署入口;`cmd/git-credential-ora` 是它固定的 helper(只共享
  `credentialmaterial`,不共享作业逻辑)。Cloud 一侧经 `internal/core` 的 `CloneDispatcher` 调用
  `client.go` 的 `Dispatch`。
- **今天没有生产调用方被启用**:`CLOUD_CLONE_DISPATCH_ENABLED` 默认 `false`,Cloud 既不签发也不派发,执行器
  因此拒绝每一个作业。这是 fail-closed 的默认,不是待办。**镜像这条路一样**:`Client.PrepareMirror` 今天只在
  本包的验收测试里被驱动,Cloud 侧的镜像派发器尚未接线,`internal/mirrorpublish` 也还没有本包之外的生产调用方。

## 测试

- `executor_test.go` 离线且确定性(Git 由替身脚本顶替):`New` 对 18 项部署变异逐条拒绝(缺物化根、物化根
  与 checkout 同目录、缺 helper、helper 含 shell 元字符、缺验签公钥、缺服务身份、缺 Store 端点、`HOME`
  缺失/非目录/非 0700 等);`cloneArguments` 对配置逐位固定、并拒绝任何会让 Git 去跑别的东西的作业;
  helper 参数拒绝可被拆分或执行的路径;`repositoryScope` 与 Git 报给 helper 的主机名和路径一致(两侧都经
  `credentialmaterial.NormalizeScope` 归约);拒绝路径在花掉任何东西之前就拒绝;他人密钥签的授权被拒;
  Store 的拒绝码被原样保留;clone 成功且证明凭据已消失;clone 失败仍销毁;销毁失败不报成功;清扫移除上一次
  运行留下的物化目录。
- `acceptance_test.go` 是**真实链路**验收,不用替身:由 `git http-backend` 在 HTTP Basic 之后提供服务的真实
  私有 HTTPS 仓库、带真实卷与真实 KEK 的真实 Credential Service、三方之间真实双向 TLS、真实的
  `git-credential-ora` 二进制(现场编译),以及 Cloud 生产用的 `core.CloneDispatcher`。九个用例:私有仓库
  Clone 成功且 checkout 内容正确、成功后物化根条目为 0 且 Token 在物化根与 checkout 下均不出现;授权只被
  消费一次、重放被拒;授权指向另一个凭据时跨凭据兑换被拒(跨租户);运行时 fencing 值不符被拒;撤销后兑换被拒;
  保留窗口内的旧版本仍可交付、不存在的版本被拒(版本冲突);**默认部署拒绝每一次兑换**;失败的 clone 仍然
  销毁凭据;启动清扫移除崩溃残留。
- `mirror_test.go` 同样离线且确定性(Git 由替身脚本顶替):13 项镜像部署变异逐条拒绝(缺 per-mirror 上限、
  缺卷上限、卷上限低于单镜像上限、相对镜像根、缺验签公钥、缺 Store 端点、缺 Git 等),并断言 `fetchRoot`
  就是 `<mirror root>/fetch`;20 项"花钱前先拒"逐条断言 reason code **且** `store.calls == 0` **且** store 里
  零条记录;`TestMirrorPrepareRefusesACloneAuthorizationWithoutSpendingIt` 是本包内的用途混淆;`mirrorArguments`
  逐位固定配置并拒绝不可用的目标;发布后 store 可读、fetch 根清空、Token 不在物化根下;同一 commit 再次准备
  返回既有记录而不重复发布;fetch 失败仍销毁物化;清扫移除被遗弃的 fetch 目录。
- `mirror_acceptance_test.go` 是镜像准备的**真实链路**验收,复用同一套真实件(真实私有 HTTPS 仓库、真实
  Credential Service 与卷与 KEK、真实双向 TLS、现场编译的真实 `git-credential-ora`、真实 mirror store),并把
  **两个执行器装在同一台服务器上**,于是一个派发身份出示同一张证书时,两条路由给出不同答案——这就是用途判据
  的可观测形式。四个用例:真实私有仓库准备成功,发布出来的裸仓库能被真实 `git clone` 读出 README 的字节,
  `ls-remote` 在同一个 commit 上报出 `HEAD` 与 `refs/ora/mirror`(`HEAD` 是那条 ref 经符号引用看到的同一件事,
  不是第二条 ref),且授权不能再用第二次、物化根下 Token 不出现、条目为 0;**镜像路由拒绝 clone 授权**(400
  `binding_mismatch`、零记录,随后那份 clone 授权仍能经 `Resolve` 兑换成功);**clone 路由拒绝镜像授权**
  (同样 400,随后那份镜像授权仍能经 `ResolveMirror` 兑换成功);服务端建为 `Mirror: nil` 时报的是
  `credential_executor_unavailable`(500)而不是路由错误,且什么都没花。
