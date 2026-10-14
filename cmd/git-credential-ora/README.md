# cmd/git-credential-ora: 部署固定的 Git credential helper

[中文](README.md) | [English](README.en.md)

`cmd/git-credential-ora` 是**部署固定**的 Git credential helper:把执行器为一次 clone 物化好的 Token 交给
Git。

它在协议意义上就是 Git credential helper——Git 用 `get`/`store`/`erase` 调用它,把请求写在标准输入——但它
**不是**凭据仓库。它读一个由执行器选定位置的文件,拒绝读别的任何东西,退出那一刻忘掉一切。

## 用法

```
git-credential-ora <material-root> <execution-id> <get|store|erase>
```

由执行器经 `-c credential.helper` 配置。程序路径由**部署**固定,参数由**执行器**固定,clone 请求的任何部分都
到不了它这里。这个分工就是要点:helper 在 Git 会执行的路径上,它唯一必须被信任去做的事,就是读一个被告知要读的
文件并按协议形态打印出来。而"这个 Token 到底能不能用"的判断早已做完——除非判断通过,否则盘上根本没有这个 Token。

## 职责与不变量

- **只读一个文件**:位置来自执行器的参数,内容由 `internal/credentialmaterial` 保证(0700 目录下的 0600
  普通文件)。缺失、权限被放宽、非普通文件一律拒绝。
- **仓库范围在交出去的这一处强制**:`get` 先读 `scope` 文件(规范化仓库身份,`host`+`path`),再把请求里的
  `host` 与 `path` 用**同一个**归约(`credentialmaterial.NormalizeScope`)化到同一拼写后逐字比较;主机大小写不
  敏感,路径大小写敏感,任何不等——另一个仓库、同一主机上的另一个仓库、子/父路径、大小写不同的路径、百分号编码
  或带 query/fragment 的路径、请求未带 `path`——都拒绝。`http.followRedirects=false` 与协议约束已经说过 Git 不
  会跟随重定向,这一条让拒绝发生在秘密真正离开的那一点,于是该保证不依赖"Git 的行为与文档一致"。`path` 之所以
  在请求里,是执行器固定了 `credential.useHttpPath=true`;没有它 Git 只报主机,同一主机上的另一个仓库就能借到
  这份 scope。
- **`store`/`erase` 什么也不做**:Git 在认证成功后调用 `store`、被拒后调用 `erase`,两者都是 helper 持久化或
  丢弃凭据的机会。本 helper 两者都不做——材料属于一次 clone,生命周期由执行器管。
- **先消费请求再判断**:任何操作都先把 Git 的请求读完。Git 写完就等,不读完会让它那侧收到 broken pipe;而且先读
  意味着畸形请求改变不了这个程序做什么,因为请求里没有任何字段被使用(除被比对的两个字段 `host` 与 `path` 外,
  未知键忽略而不是拒绝——那是 Git 自己的输出而非调用方输入,新版 Git 加一个键不该让认证停摆)。
- **错误信息不含材料**:Git 会把 helper 的 stderr 显给用户并放进自己的错误输出,进而进入 clone 日志。因此错误只
  说失败,绝不说内容;`token` 只出现在标准输出的 `password=` 一行里。
- **用户名是固定标签**:`x-access-token`(GitHub 文档化的 Token 认证形式)。它**不**从材料里读——用户名不是秘密,
  让文件携带第二个值只会让 helper 的输出依赖超过那一件事。

## 依赖与调用方

- 依赖:`internal/credentialmaterial`(共享布局)与标准库。**刻意不**依赖 `internal/cloneexec`:它不该链接
  执行器的作业逻辑。
- 调用方:只有 Git。执行器通过 `-c credential.helper` 把它固定为本次 clone 的**唯一** helper。

参见 [cmd 入口总览](../README.md)、[物化布局](../../internal/credentialmaterial/README.md) 与
[执行器包](../../internal/cloneexec/README.md)。
