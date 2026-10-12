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

## 文件

- `executor.go`:`Config`、`Executor`、`New`、`Sweep`、`Execute`、`Job`、`Result`、`Runtime`、`Failure` 与
  七个 reason code。
- `git.go`:受限 Git 调用的全部构造——`cloneArguments`、`helperArgument`、`gitEnvironment`、`repositoryScope`、
  `validateBranch`、`validateExecutablePath`、`runGit`、`redactFailure`、`headCommit`。
- `server.go`:双向认证 HTTP 监听(`Server`、`ServerConfig`、`NewServer`),只接受部署固定的派发身份
  (`presentedIdentity`),请求体上限 16 KiB(`maxJobBody`)。
- `client.go`:调用方一侧的客户端(`Client`、`ClientConfig`、`NewClient`、`Dispatch`、`DispatcherRefusal`)。
- `storeclient.go`:`RedemptionClient` 接口与 `StoreClient`——`broker-resolver` 的 mTLS 客户端、十个 Store
  侧 reason code、`Credential`。
- `executor_test.go`:18 项部署变异、argv 与环境的逐位固定、拒绝路径、失败仍销毁、清扫。
- `acceptance_test.go`:真实私有仓库的端到端验收(见下文"测试")。

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

## 部署归属与故障清理

- 身份:C 是**非 root** 的部署单元自身进程身份(后续 ADR D3;正式部署为 uid 10002,与 Cloud 的 10001、Node
  内 W 的 1000 互异)。它不持有 Cloud 的签名私钥,也不持有派发身份;它只持有 `broker-resolver` 兑换身份。
- 卷:物化根、checkout 根与 `HOME` 由部署创建为 C 独占的 0700;helper 与执行器本体 root 所有且 C 不可写,
  路径由部署固定。`cluster/compose/clone-executor-setup.sh` 造这些目录,`cluster/scripts/
  clone-executor-boundary.sh` 在运行栈上复核这组边界。
- 数据卷必须跨重启保留(`clone-executor-data`):丢了它,崩溃后残留的物化目录就无法被清扫。
- 清理面:作业内 `defer` 覆盖成功/失败/超时/取消/panic;进程外由 `Sweep` 覆盖崩溃。**尚未有直接证据**的是
  真实强杀加重启的端到端清扫,以及"超时"与 Node clone 状态机的 `Interrupted` 判定——执行器今天不接入 Node
  的 clone 状态机(见 `20261012` D4),这一点在核实用例里如实标为 `Partial`,不当作已完成。

## 依赖与调用方

- 依赖:`internal/cloneauth`(验签)、`internal/credentialmaterial`(布局)与 `zap`,其余为标准库。
  **不依赖** `internal/core`,也**不依赖** `internal/credentialstore`:Store 的十个拒绝码在 `storeclient.go`
  里**刻意重复声明**而不是导入,因为执行器依赖的是那套线上词汇,不是 Store 的实现——两个组件各有各的二进制,
  Store 若被换掉,执行器上报的码必须仍然表示同一件事。
- 调用方:`cmd/clone-executor` 是它的部署入口;`cmd/git-credential-ora` 是它固定的 helper(只共享
  `credentialmaterial`,不共享作业逻辑)。Cloud 一侧经 `internal/core` 的 `CloneDispatcher` 调用
  `client.go` 的 `Dispatch`。
- **今天没有生产调用方被启用**:`CLOUD_CLONE_DISPATCH_ENABLED` 默认 `false`,Cloud 既不签发也不派发,执行器
  因此拒绝每一个作业。这是 fail-closed 的默认,不是待办。

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
