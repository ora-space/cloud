# internal/credentialmaterial: 凭据物化的磁盘布局

[中文](README.md) | [English](README.en.md)

`internal/credentialmaterial` 只拥有**一件事**:凭据执行身份为一次 clone 把一份 Git Token 物化到磁盘时所用的
布局,以及"在哪里、以什么权限"这个问题的答案。它落实
`specs/decisions/node/repository/20261010-controlled-node-git-authentication-isolation.md` 的 D11(C 自行解析并
物化)与 D13(崩溃/超时/重试清理)。

## 为什么它是一个独立包

必须有**两个**程序对这套布局达成一致,而它们被刻意分开:执行器写和销毁材料,部署固定的 Git credential helper
读它。helper 是一个**独立二进制**——它就在 Git 会去执行的路径上,所以它能做的事越少越好,绝不能链接执行器的
作业逻辑;但两者一旦对"Token 在哪"有分歧,隔离就会**fail open**。只共享这么多、不共享更多,是"两边一致"与
"helper 不变成第二个执行器"同时成立的原因。

布局是能成立的最小形态:

```
<root>/                    部署创建,工作负载身份不可写
└── <execution-id>/         凭据执行身份创建,0700
    └── token               0600,明文 HTTPS Git Token
    └── scope               0600,该 Token 唯一可被出示的主机
```

每个路径都由**规范小写 UUID** 推导,因此调用方无法让一次 clone 的材料出现在另一个名字下,也无法用路径片段逃出
根目录。本包不读部署配置、不做策略判断:材料**是否允许存在**是执行器的决定,本包只让"在哪"和"什么权限"在两边
成为同一个答案。

## 文件

- `material.go`:`TokenFile`、`ScopeFile`、`DirectoryMode`/`FileMode`、三个哨兵错误、`ValidExecutionID`、
  `ExecutionDirectory`、`TokenPath`、`Stage`、`Read`、`ReadScope`、`Destroy`、`Sweep`,以及内部的
  `writeFile`、`readMaterialFile`、`validateRoot`、`syncDirectory`。
- `material_test.go`:布局与权限的逐位断言、拒绝路径、销毁证明与清扫。

## 接口与不变量

- `TokenFile = "token"`:文件内容**就是**秘密本身——没有用户名、没有 header、没有 JSON 信封。整个内容都是秘密
  的文件是最容易销毁、也最容易证明已销毁的东西,而且它无法在"只期望一个值"的读者面前夹带第二个值。
- `ScopeFile = "scope"`:Token 唯一可被出示的主机。它**不是**秘密,也**不是** Token 的授权依据——仓库范围由
  执行器在物化之前就按授权定好。它存在的意义是让 helper 不依赖"Git 的重定向与子模块行为如文档所述",就能拒绝
  对别的主机的请求:`http.followRedirects=false` 表达的是意图,而这个文件在 Token 真正被交出去的那一处**强制**
  它。
- `DirectoryMode`/`FileMode` 是常量而不是配置:**隔离论证恰好依赖这两个值**,一个能放宽它们的部署就能静默地
  取掉那条边界。
- `ValidExecutionID`:只接受规范小写 UUID(连字符位置固定、无大写、无花括号、无多余形式)。`ExecutionDirectory`
  与 `TokenPath` 各自再校验一次,并且 `validateRoot` 要求根是**绝对**路径(`ErrUnsafeRoot`)——相对根会随进程
  工作目录解释,于是"根在哪"变成调用方的运行时属性而不是部署属性。
- `Stage` 拒绝空材料与**已存在**的目录(不覆盖别人已经物化的东西),`Read` 拒绝缺失、被放宽权限、以及**非普通
  文件**(符号链接、设备、FIFO 一律不读:那正是"把 Token 读成别的进程提供的管道"的做法),空内容按
  `ErrAbsent` 处理而不是当空 Token 用。
- `Destroy` 删掉文件后**同步目录项**:报告成功之后再崩溃,不能把材料带回来(`syncDirectory(filepath.Clean(root))`)。
  `Destroy` 成功即已证明消失,而不是"发出了 unlink"。
- `Sweep`:移除根下**每一个**条目,返回数量;执行器在开始监听前调用它,用于清理崩溃残留。它不做"这个目录是不
  是本次运行创建的"这类判断——按定义,本次运行还没有创建过任何东西。
- 三个哨兵错误(`ErrInvalidExecutionID`、`ErrUnsafeRoot`、`ErrAbsent`)都不携带材料的任何部分:调用方把它们
  映射成自己的 reason code。

## 依赖与调用方

- 依赖:仅标准库(`errors`、`fmt`、`os`、`path/filepath`、`strings`)。
- 调用方:`internal/cloneexec`(执行器,写与销毁)与 `cmd/git-credential-ora`(helper,读)。两者都不依赖对方,
  这正是本包存在的理由。

## 测试

`material_test.go` 覆盖:execution id 只接受规范小写 UUID;execution 目录严格位于根之下;`Stage` 造出文档化的
布局与文档化的权限(0700/0600)且拒绝空材料与已存在目录;`Read` 拒绝缺失、被放宽权限与非普通文件;空内容按
`ErrAbsent` 处理;`Destroy` 移除材料并证明其消失;`Destroy` 之后在根下**任何位置**都不残留 Token 字节;
`Sweep` 移除根下每一个条目。
