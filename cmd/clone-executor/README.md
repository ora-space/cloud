# cmd/clone-executor: 凭据执行器进程

[中文](README.md) | [English](README.en.md)

`cmd/clone-executor` 运行凭据执行器,也就是已批准 Node 隔离决策里的**凭据执行身份 C**
(`20261010-controlled-node-git-authentication-isolation` D1/D7/D11),但它被做成**当前正式产品的进程**,
而不是历史 Desktop Node 的一部分。它以自己的操作系统身份、自己的卷、自己的信任根运行:以
`broker-resolver` 身份兑换一份 Cloud 签发的单次授权,把 Token 物化到只有自己能读的地方,经部署固定的
credential helper 跑一次受约束的 `git clone`,并在作答之前销毁材料。

## 职责

- **配置**:`config.LoadCloneExecutorConfig` 读取本服务自己的配置文件,只认 `CLONE_EXECUTOR_*` 环境前缀,
  因此 Cloud 的配置无法设置本服务的卷、helper 路径或监听地址。
- **启动闸门**:在绑定监听端口之前依次完成 `CloneExecutorConfig.Executor()`(物化根与 checkout 根必须是不同
  的绝对路径、helper 与 Git 必须是绝对路径且不含 shell 元字符、`HOME` 必须存在且恰为 0700、必须有验签公钥、
  必须能构造 `broker-resolver` 的 Store 客户端)、`ServerTLS()`(服务证书、私钥与客户端 CA)与
  `cloneexec.NewServer`(派发身份必须存在)。D17 要求"配置缺失"是启动拒绝,而不是第一个作业才暴露。
- **启动清扫**:`executor.Sweep()` 在**监听之前**跑。它修复运行中的进程修不了的那一种失败——上一次崩溃或被
  杀掉的容器在盘上留下了 Token 而没有进程去销毁它,只有一次启动能(D13)。
- **生命周期**:`signal.NotifyContext` 处理 `SIGINT`/`SIGTERM`,`http.Server.Shutdown` 在 10 秒内优雅退出,
  退出时 `logger.Sync`。写超时必须长于一次 clone(响应就是 clone 的产物),因此是 `Timeout + 1 分钟`。

## 不变量

- **不含领域逻辑**:进程只做配置、装配、生命周期与退出码。受约束的 Git 调用、绑定比对、兑换与清理的次序、以及
  启动闸门本身,都在 `internal/cloneexec` 与 `internal/config`。
- **监听即双向认证**:`tls.NewListener` 直接包住监听器(不走 `ServeTLS` 的空文件名形式,那会静默依赖证书已在
  配置里),并在处理器之上校验客户端证书里的派发身份;不存在匿名请求能到达处理器的路径。
- **只记录位置,不记录秘密**:启动日志只写监听地址、两个根与清扫掉的条目数。凭据与授权永不进入日志行。

## 配置

`configs/clone-executor.yaml` 或 `CLONE_EXECUTOR_*` 环境变量,字段见
`internal/config.CloneExecutorServiceConfig`:`listen`、`material_root`、`checkout_root`、`home_directory`、
`helper_path`、`git_path`、`ssl_ca_info`、`authorization_public_key_file`、`certificate_file`、
`private_key_file`、`client_ca_file`、`dispatcher_identity`、`store_endpoint`、`store_certificate_file`、
`store_private_key_file`、`store_ca_file`、`timeout` 与 `logger`。证书、私钥与公钥一律以**文件路径**给出,
绝不写进配置文件或命令行。

只持有**公钥**:Cloud 的签发私钥不在这个进程里,也不在它挂载的任何卷里。

参见 [cmd 入口总览](../README.md)、[执行器包](../../internal/cloneexec/README.md)、
[物化布局](../../internal/credentialmaterial/README.md) 与 [配置加载](../../internal/config/README.md)。
