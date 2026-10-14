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

C 今天承担**三项能力**,走**三条路由**:`POST /v1/clone-executions` 把授权换成一次 `git clone`,
`POST /v1/mirror-preparations` 把授权换成一份内部镜像,`POST /v1/push-executions` 把授权换成**对某条 ref 的
一次写入**。三者**共享**部署、验签、兑换与 Git 调用方式,**不共享**路由、请求类型、结果类型与消费方——不在
同一个 Job 上加 `kind` 开关,理由见下文"三条路由,不是一个开关"。三项的相对代价并不相同:前两项是读,第三项
是**写**,因此它的次序、超时与失败语义都比前两项更窄(见"推送的顺序")。

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

## 三条路由,不是一个开关

三项能力各有各的路由、请求类型与结果类型,而且是三个导出方法,不是同一个方法上的开关:

| | Clone | 镜像准备 | 推送 |
| --- | --- | --- | --- |
| 路由 | `POST /v1/clone-executions` | `POST /v1/mirror-preparations` | `POST /v1/push-executions` |
| 请求 / 结果 | `Job` / `Result` | `MirrorJob` / `MirrorResult` | `PushJob` / `PushResult` |
| 授权 operation | `clone` | `mirror_prepare` | `push` |
| 主体 | 某台 Node 上的某次 execution 的 checkout | 某个 Project Operation 的某一次 attempt | 某一次人工批准的写入(某 Project 的哪条 ref、从哪个提交到哪个提交) |
| 身份字段 | `executionId`、`nodeId`/`nodeInstanceId`/`nodeIncarnationId` | `operationId`/`preparationGeneration`/`executorIdentity` | `operationId`/`executorIdentity`,以及 ref 与两个提交 |
| Cloud 一侧的方法 | `Client.Dispatch` | `Client.PrepareMirror` | `Client.Push` |

理由是主体不同:clone 绑定"某台 Node 上的某次执行",镜像准备绑定"某个 Project Operation 的第几次尝试",推送
绑定"某一次被批准的写入",三者的身份字段**没有交集**——`Binding.validate` 对 clone 要求三个 node id 且
**禁止** operation/generation/executorIdentity,对镜像与推送要求 operation 且**禁止**三个 node id,而推送还
要求 ref 与两个提交、镜像则要求 ref 与 generation。若合成一个带 `kind` 的 Job,解码器就要自己判断哪些
身份字段有意义,operation 边界会退化成 C 内部的一个分支,而不是一道 Cloud 无法伪造的签名(2026-10-13 裁决)。

**用途混淆在三个方向都必须被拒**,而且拒法是比对**签名里的 operation**,不是比对请求落在哪条路由:

- 镜像路由遇到 `clone` 授权 → 400 `binding_mismatch`(`matchMirrorBinding` 的第一项),发生在**向 Store 要
  任何东西之前**;
- Clone 路由遇到 `mirror_prepare` 授权 → 400 `binding_mismatch`(`matchBinding` 的同类检查);
- 推送路由遇到 `clone`/`mirror_prepare` 授权 → 400 `binding_mismatch`(`matchPushBinding` 的第一项),同样
  发生在**向 Store 要任何东西之前**;反向也成立:写向的授权在另外两条路由上被拒,而 Store 侧再独立判一次用途
  (`Resolve`/`ResolveMirror`/`ResolvePush` 各自先比 operation)。

三边的"没被消费"都不靠 C 的承诺作证,而是靠事后经**该授权本该走的那条路**兑换成功来证明(见"测试")。

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
   `rev-parse --verify --quiet refs/heads/ora-mirror^{commit}` 把 commit 读回来;`for-each-ref` 必须**恰好**只有
   `refs/heads/ora-mirror` 一条(`mirrorpublish.MirrorRef`),并 `symbolic-ref HEAD refs/heads/ora-mirror`
   ——它必须是 `refs/heads` 下的分支,否则 Node 的 `git clone --branch` 找不到它(理由见
   [`mirrorpublish`](../mirrorpublish/README.md))。
5. **发布并销毁**:`Preparation.Begin` →若该 commit 已发布则返回既有记录(按 commit 幂等)→ 否则 `Adopt`
   移入 store 的 staging 后再 `Publish`;物化的销毁与 fetch 目录的移除在**每一条**出口上执行,销毁失败照样
   报错——凭据可能还在盘上的准备不是一次成功的准备。

准备超时取"部署配置值"与"授权剩余有效期"的**较小者**,于是一份即将过期的授权不会让一次 fetch 跑过它的
有效期。

## 推送的顺序

`PushExecutor.Push`(`push.go:300`)与另外两条路径同一套"顺序即契约",但它在次序上**刻意**有一处不同:写向的
两个本地判定只能在兑换**之后**做。

1. **本地先验、先不花钱**:规范化仓库 → 校验 operation/project/workspace/凭据 id → 用签发方自己的
   `ValidateRef`/`ValidateCommit` 校验 ref 与两个提交 → 验签 → `matchPushBinding`(第一项就是 operation)。
   这一步失败的作业**什么都没消耗**。
2. **以 `broker-resolver` 身份兑换**:`Store.ResolvePush` 在 Store 侧先判用途与出示身份——出示方必须是授权里
   `executorIdentity` 那一个,且请求**不得**自称任何运行时(写向授权不绑定 Node),**再**持久地只消费一次。
3. **物化**:目录名取**授权 id**(`pushMaterialIdentity`),理由与镜像准备相同——授权只花一次,所以同一次批准
   的两次尝试不可能撞进同一个目录。
4. **一次受约束的 fetch**:在 scratch 裸仓库里 fetch 签名仓库的 `refs/heads/*`(`--no-tags`,且不写
   `FETCH_HEAD`,于是一条 fetch 不会留下影响后续命令的文件)。
5. **两次本地判定**:签名的 new commit 必须**已经在**签名的 upstream 里,且签名的 old 必须是它的**祖先**
   (`verifyWriteIsExpressible`)。任一不成立即 `credential_executor_push_not_applicable`,而且**发生在任何
   网络写入之前**。代价是这一步在第 2 步之后,因此一次指向不存在提交的作业会**花掉一次授权**——这是写向唯一
   与另外两条路径不同的次序,理由写在 `Push` 的注释里:把判定挪到兑换之前,就得给对象再开一条内容通道,而那是
   一条新的、需要被单独授权的东西。
6. **一次 compare-and-swap 写入**:见下节;写入之后把 ref 的当前值读回来,作为结果的证据而不是结论的替代。
7. **销毁**:成功、Git 失败、超时、取消、panic 都在 `defer` 上;销毁失败即使远端已经写入也照样报
   `credential_executor_cleanup_failed`——凭据可能还在盘上的推送不是一次完成的推送。

推送超时取"部署配置值"与"授权剩余有效期"的**较小者**,理由与镜像准备相同。

### 写入只有一条命令

`pushUpdateArguments`(`pushgit.go`)是**唯一**会改变执行器之外任何东西的那一行,决策要求的每一条限制都落在
它上面:

- refspec 是 `<本地 ref>:<签名 ref>`——一个源、一个目标,都写全名,没有 pattern、没有多 ref 列表、没有 `+`
  前缀、源不为空,于是"删除"与"全推"**不可表达**。本地 ref 在写入前一行按签名的 new commit 建好,所以远端
  收到的值与签名覆盖的值**按构造是同一个提交**,而不是靠读的人去比对两种拼写。
- `--force-with-lease=<ref>:<old>` 是 compare-and-swap,期望值**写在命令行上**而不是取自本地引用:拿本地引用
  去比,比的是这个进程自己刚写下的值,等于没比。
- `--no-verify` 与 `-c push.followTags=false` 是纵深:钩子已由 `core.hooksPath=/dev/null` 关掉,显式 refspec
  也带不了 tag,但写是唯一一种"第二个没被审阅的程序或 ref 会跟着一起上"的操作。
- `--porcelain` 让结果可读:默认输出是给人看的散文,读它就得匹配英文。

## 文件

- `executor.go`:`Config`、`Executor`、`New`、`Sweep`、`Execute`、`Job`、`Result`、`Runtime`、`Failure` 与
  七个 reason code。
- `git.go`:受限 Git 调用的全部构造,以及**两条路径共用的**那份 Git 环境——`gitDeployment`(两条路径不能
  在"用哪个 helper、信哪个 CA"上分叉)、`gitEnvironment`、`repositoryScope`、`validateExecutablePath`、
  `runGit`、`redactFailure`;clone 一侧的 `cloneArguments`、`helperArgument`、`validateBranch`、
  `headCommit`。
- `egress.go`:部署可选的、默认关闭的 Git 出网代理——`GitEgress`、`NewGitEgress`、`validate`、
  `proxyEndpoint`、`validateEgressHost` 与 `arguments`。它是本包唯一会写出代理的地方,而且写在**命令行**
  上而不是环境里,并且只对部署列出的主机生效:名单之外的地址一个参数都不产生,所以 Cloud、内部 Git Server
  与 Credential Store 永远不会被代理,也就不需要维护一份排除名单。`gitEnvironment` 的白名单不因它变动,
  "能改道凭据的变量"因此没有增加。它偏离的两条已批准决策记录在 `GitEgress` 的文档注释里,那两条尚未修订。
- `mirror.go`:第二项能力——`MirrorJob`、`MirrorResult`、两个 reason code
  (`credential_executor_fetch_failed`/`credential_executor_publish_failed`)、`MirrorConfig`、
  `MirrorExecutor`、`NewMirrorExecutor`、`Sweep`、`Prepare`、`matchMirrorBinding`、
  `mirrorMaterialIdentity`。
- `mirrorcontent.go`:镜像准备里跑 Git 的那一半——`mirrorArguments`(与 `cloneArguments` 同类,固定
  helper、`credential.useHttpPath`、`protocol.*`、hooks/symlinks/LFS,并做一次 `--bare` fetch)、
  `validateMirrorRef`、`fetch`(init/fetch/rev-parse/for-each-ref/symbolic-ref)、`gitFact`、`publish`。
- `push.go`:第三项能力——`PushJob`、`PushState`(五态)、`PushResult`、两个写向 reason code、
  `PushConfig`、`PushExecutor`、`NewPushExecutor`、`Sweep`、`Push`、`matchPushBinding`、
  `pushMaterialIdentity`、`materialize`、`verifyWriteIsExpressible`、`update`、`reconcile`、`stage`。
- `pushgit.go`:推送里跑 Git 的那一半——`pushFetchArguments`、`pushUpdateArguments`(唯一一条写命令)、
  `pushFactArguments`、`validatePushTarget`,以及两个读取器:Git 的 `--porcelain` 报告与 `ls-remote` 的远端值。
- `server.go`:双向认证 HTTP 监听(`Server`、`ServerConfig`、`NewServer`),三条路由,只接受部署固定的
  派发身份(`presentedIdentity`/`callerIdentity`),请求体上限 16 KiB(`maxJobBody`)。三条路由**共用同一份
  派发身份**:同一个出示方在三条路由上得到不同答案,正是"判据是签名里的 operation,不是路由"的可观测形式。
- `client.go`:调用方一侧的客户端(`Client`、`ClientConfig`、`NewClient`、`Dispatch`、`PrepareMirror`、
  `DispatcherRefusal`)。
- `storeclient.go`:`RedemptionClient` 接口与 `StoreClient`——`broker-resolver` 的 mTLS 客户端、十个 Store
  侧 reason code、`Credential`。
- `executor_test.go`:18 项部署变异、argv 与环境的逐位固定、拒绝路径、失败仍销毁、清扫。
- `mirror_test.go`:13 项镜像部署变异、20 项"花钱前先拒"、argv 固定、发布内容、按 commit 幂等、fetch 失败
  仍销毁、fetch 目录清扫。
- `acceptance_test.go`:真实私有仓库的 clone 端到端验收(见下文"测试")。
- `mirror_acceptance_test.go`:真实私有仓库的镜像准备端到端验收,以及两个方向的用途混淆矩阵(见下文"测试")。
- `push_acceptance_test.go`:真实私有仓库的推送端到端验收,含只读对账与一次批准只能写一次(见下文"测试")。
- `pushgit_test.go`:写命令与两个读取器的离线固定(见下文"测试")。

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
- `PushConfig` / `NewPushExecutor`:第三项能力的部署描述与构造。它同样**复用** clone 的 Git 与物化输入
  (`gitDeployment` 校验的是同一组边界),并**额外**要求 scratch 根、验签公钥集与 `PushRedemptionClient`,缺一
  即拒绝启动;scratch 根必须是**物化根之外的另一个目录**,因为一次推送会把一整个私有 upstream 写进盘里,把它
  指向放 Token 的那个目录等于让"清扫被遗弃的仓库"和"清扫被遗弃的 Token"变成同一个动作。
- `PushExecutor.Push(job)`:`(PushResult, error)`,顺序见上文。返回的 `PushResult` 说的是**这一次尝试对远端
  确立了什么**:五态之一、执行器读回来的远端值(读过才有)、以及这次写入实际用的凭据版本。错误只在两种情形
  返回——执行器自己的拒绝,或凭据路径的失败——因为两种情形都意味着**写入没有开始**,而结果类型专门用来承载
  "开始了但结果不确定"。
- `PushExecutor.Sweep`:`(移除的条目数, error)`,移除 scratch 根下上一次运行留下的目录。这里的清扫比另外
  两条路径更要紧:scratch 里是私有 upstream 的一份明文副本,而不是一份 checkout。
- `Client.Push(job)`:第三个方法,POST 到 `/v1/push-executions`,与另外两个共用 `maxJobBody` 与
  `DispatcherRefusal` 映射。它只被 Cloud 的 `core.PushDispatcher` 调用,而那个派发器只在部署声明了 push 能力
  时才被装配。
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
  Git 报出仓库路径、供 helper 与物化 scope 比对的那一项)、`http.followRedirects=false`、代理键
  (`http.proxy=` 恒在;部署启用了出网策略时,后面至多多一个按主机限定的 `http.<url>.proxy`,见
  `egress.go`,且永远不会是空值)、以及
  `core.hooksPath`/`core.symlinks`/子模块/LFS;`gitEnvironment` 是克隆进程的**全部**环境
  (`GIT_CONFIG_GLOBAL`、`GIT_CONFIG_NOSYSTEM`、`GIT_TERMINAL_PROMPT=0`、`GIT_ASKPASS` 等)。启用的代理
  之所以走 argv 而不是变量,正是为了让这份白名单保持七项;它同时被拒绝携带 userinfo,因此凭据无法经它进入
  命令行。Token 不进
  argv,只走 helper 与 Git 之间的 `credential` 协议;写命令与只读对账用的是同一份环境与同一个 helper,因此
  写入路径上也没有第二条取得秘密的途径。
- **不运行仓库给的东西**:没有 hook、没有 LFS filter、没有子模块、没有符号链接跟随——每一项都是授权没有覆盖
  的程序或取数动作。
- **失败信息不回显秘密**:`redactFailure` 在错误串里抹掉 Token;`Failure` 不带任何凭据片段。
- **checkout 与物化根权限不同是有意的**:checkout 是交接面,0700 的物化根与 0755 的 checkout 根不能共用一套
  权限叙事——clone 里没有秘密,而物化根里全是。
- **用途由签名决定,不由路由决定**:三条路由各自先比对**授权里的 operation**,再问 Store 要任何东西。因此
  "把 clone 的作业发到镜像路由"不会换来一份镜像,也不会替 Node 花掉那次 clone——它换来的是 400
  `binding_mismatch`,而那份授权仍然可以在它本该走的路由上兑换成功;写向同理,一次写向的作业带着读向的授权
  到达时被拒,并且**什么都没花**。
- **只 fetch 签名里的那一个 ref**:ref 由授权签名携带并经 `cloneauth.ValidateRef` 校验,发布的镜像又用
  `for-each-ref` 断言**恰好**只有 `refs/heads/ora-mirror` 一条,所以读镜像的人拿到的 commit 就是 Cloud 行里那个
  commit,而不是派发方想让它是的那个。
- **fetch 目录生活在 mirror store 之内、由构造推导而非配置**:`fetchRoot = <mirror root>/fetch`,于是它移入
  store staging 的动作按构造是同文件系统内的 rename;而部署若把镜像根与物化根互相嵌套,`NewMirrorExecutor`
  直接拒绝启动(见下节)——因为启动清扫会删掉物化根下的**每一个**条目。
- **写向只改一条 ref,而且"写宽"不可表达**:ref 与两个提交由签名携带,经签发方自己的 `ValidateRef`
  (`refs/heads/` 之下、拒绝 `*`/`?`/`..`/`@{`/`:`/`\` 等 Git 自身的拼写限制)与 `ValidateCommit`(拒绝缩写、
  大写与全零 oid——全零就是"删除"与"从空创建"的拼法)校验;`old == new` 被拒;写命令的 refspec 只有一个源与
  一个目标。因此 force、删除、wildcard、tag、多 ref 在**准入处**就被拒,而不是等远端拒绝。
- **写入前必须先在本地证明这次写是"可表达的"**:签名的 new commit 必须在签名的 upstream 里存在,且签名的 old
  必须是它的祖先。两条都不成立即拒绝,并且**不发出任何写**。它们依赖第 4 步 fetch 下来的对象,所以只能在兑换
  之后做;这是唯一的次序让位(见"推送的顺序"第 5 步)。
- **远端值只作为证据,不改变结论**:写入之后(或在一次拒绝之后)执行器会**只读地**读一次 ref 的当前值,并把它
  原样上报。它不把 `target_satisfied` 说成 `confirmed`——"ref 已经是那个提交"与"是这次写把它变成那个提交的"
  是两件事(不变量 12 的可观测面);它也不构成重试:对账不产生新的写授权,也不再次执行写入。
- **五态而不是布尔**:`confirmed`/`target_satisfied`/`not_applied`/`target_conflict`/`unknown` 是互相排斥的
  五个成员,`unknown` 是其中之一而不是"没有答案"——无法确立结果的尝试必须能被如实记成"无法确立",而不是一个
  读者会理解成"什么都没发生"的空值。

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
- 推送的 scratch 根是**第三个**根:物化根放秘密,checkout 根是交接面,scratch 根放的是**私有 upstream 的明文
  副本**。因此部署层(`internal/config` 的 `PushExecutor`)在启动时拒绝把它配成与物化根、checkout 根或镜像根
  互相包含的目录:`NewPushExecutor` 要求它与物化根**不同**,而包含关系的检查在配置层完成——被包含的 scratch
  根要么会被 clone 的启动清扫(`credentialmaterial.Sweep` 移除物化根下的每一个条目)连带删掉,要么把一份明文
  upstream 放进放秘密的那棵树里。**未声明推送根 = 没有这项能力**,路由如实回
  `credential_executor_unavailable`,另外两项能力不受影响。
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
- **默认没有任何调用方被启用**:`CLOUD_CLONE_DISPATCH_ENABLED`、`CLOUD_CLONE_DISPATCH_MIRROR_ENABLED` 与
  `CLOUD_CLONE_DISPATCH_PUSH_ENABLED` 默认都是 `false`。三个都关时 Cloud 既不签发也不派发,执行器因此拒绝
  每一个作业——这是 fail-closed 的默认,不是待办。打开 `mirror_enabled` 后 Cloud 会为每一次已领取的镜像准备
  尝试签发并派发(见 `internal/core` 的 `MirrorDispatcher` 与 `PrepareMirrorsOnce`);但**镜像内容今天仍无
  读者**:Cloud 尚未被告知沙盒从哪里读镜像(`git_server.sandbox_base_url` 为空),因此没有任何 Workspace 会
  从镜像 clone,`internal/mirrorpublish` 也仍没有本包之外的生产调用方。
- 打开 `push_enabled` 需要**三处同时**成立,并且三处互相独立地拒绝:Cloud 侧
  `CLOUD_CLONE_DISPATCH_PUSH_ENABLED`(装配 `core.PushDispatcher`)、执行器侧
  `CLONE_EXECUTOR_PUSH_ENABLED`(装配 `PushExecutor`,且要求 `push_root`)、Credential Service 侧配置了
  Cloud 的授权验签公钥(`CREDENTIAL_STORE_AUTHORIZATION_PUBLIC_KEY_FILE`,且兑换身份 `broker-resolver` 的
  客户端证书已签发)。缺任一处,一次推送会在**不同的**位置以不同的稳定码被拒——Cloud 不装配执行器时是
  `push_executor_unavailable`,Store 没有验签公钥时是 `authorization_verification_unavailable`。今天没有任何
  部署文件打开它们:推送路径有实现、有验收,但**没有生产部署启用**,这一点在报告与核实用例里如实标注。

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
  `ls-remote` 在同一个 commit 上报出 `HEAD` 与 `refs/heads/ora-mirror`(`HEAD` 是那条 ref 经符号引用看到的同一件事,
  不是第二条 ref),且授权不能再用第二次、物化根下 Token 不出现、条目为 0;**镜像路由拒绝 clone 授权**(400
  `binding_mismatch`、零记录,随后那份 clone 授权仍能经 `Resolve` 兑换成功);**clone 路由拒绝镜像授权**
  (同样 400,随后那份镜像授权仍能经 `ResolveMirror` 兑换成功);服务端建为 `Mirror: nil` 时报的是
  `credential_executor_unavailable`(500)而不是路由错误,且什么都没花。
- `pushgit_test.go` 离线且确定性:写命令逐位固定(`<本地 ref>:<签名 ref>` 的单个 refspec、
  `--force-with-lease=<ref>:<old>` 的期望值、`--no-verify`、`push.followTags=false`、`--porcelain`),并拒绝
  不可拨打的地址与被 `ValidateRef`/`ValidateCommit` 拒绝的 ref/提交;fetch 与两个只读命令断言**仍然只是读**;
  `--porcelain` 报告的读取器只读**恰好一条** ref;`ls-remote` 的读取器把"没有这条 ref"与"这次读没成功"
  分成两个答案(前者是"ref 不在",后者是"不知道")。
- `push_acceptance_test.go` 是推送的**真实链路**验收,复用同一套真实件并**把 Cloud 的 `core.PushDispatcher`
  接在链路头上**:真实私有 HTTPS 仓库、真实 Credential Service 与卷与 KEK、三方真实双向 TLS、现场编译的真实
  `git-credential-ora`、真实的三个执行器装在同一台服务器上。远端侧的证据不取自执行器的自述:仓库带一个
  `pre-receive` 钩子,把 Git 自己的 `<old> <new> <ref>` 行写下再决定放行或拒绝,于是"写了什么"是**服务端**
  看到的;仓库的请求计数则把"拒绝之后又读了一次"与"又写了一次"区分开(写侧没有歧义:一次 receive-pack 交换
  就是一次尝试,所以被钉住的是这个计数,而不是读侧的往返次数——那是 Git 协议协商的属性)。十一个用例:一次受控
  推送把真实 upstream 的一条 ref 从 old 移到 new,且服务端只看到**恰好一次** receive-pack、钩子只记下**一行**
  授权里的那个更新;远端被别的写者移走时 compare-and-swap 拒绝并且**不覆盖**;ref 已经是目标提交时报
  `target_satisfied` 而不是 `confirmed`(**写入者不可归因**);远端明确拒绝的更新不被应用、也**不重试**;签名的
  new commit 不在 upstream 里、或 old 不是 new 的祖先时,在不发出任何写的前提下被拒;会写宽写入的 **14 项**
  作业逐条被拒且**什么都没花**——非 https 地址、tag、`refs/heads/*` 通配、`+` force 前缀、两个 ref、`../..`
  遍历拼法、以全零 oid 拼出的删除与创建、授权没指名的仓库(受害者是一份**该凭据够得着**的真实
  仓库,所以"它没被动过"是对一个写入本可能落到的位置的断言)、授权没指名的 ref、授权没指名的提交对、另一个
  凭据、另一个凭据版本、以及一份为 clone 签发的授权;**一次批准不能写两次**;一次兑换不能被花在另一个用途上;
  打不通的 upstream 在配置的超时内被杀掉并回收,物化与 scratch 都不留秘密,且同一个执行器紧接着还能完成一次
  真实推送;启动清扫移除被遗弃的 scratch 仓库;不完整的推送部署在启动时被拒。**本文件不能证明的是** C 与
  W 的操作系统级隔离——整条链路跑在一个测试进程、一个 uid 之下,那条边界是部署属性,由容器级验收取证(与
  clone 验收同一处如实标注)。
