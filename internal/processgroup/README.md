# internal/processgroup：让子进程的结束带上它启动的一切

[中文](README.md) | [English](README.en.md)

`internal/processgroup` 只做**一件事**：把调用方启动的那个子进程放到一个**它自己领头的新进程组**里，并让
「上下文结束」结束**这个组**，而不是只结束那个进程。它服务于
`specs/decisions/node/repository/20261010-controlled-node-git-authentication-isolation.md` 的 D13（崩溃/超时/
重试清理）与推送执行权限里「一次执行结束后不留下持有凭据的进程」这条义务。

## 为什么需要它

`exec.CommandContext` 的默认取消只杀**它启动的那一个**进程。本仓库启动的 Git 从来不是一个进程：`git fetch`
会起传输 helper 与索引器，`git push` 会起 send-pack 并拉起接收侧，`git upload-pack` 会起打包器——它们**继承父
进程的管道**，并在父进程被杀之后继续运行。

后果分两层，第二层是安全性的：

- 可靠性与资源：`Wait` 可能等到那个继承管道的后代（写向 `strings.Builder` 时 exec 会为管道开一个复制
  goroutine，`Wait` 等它），于是「超时」变成建议性的；`reapDelay` 只是给这种等待加一个上界，**并不结束那个
  进程**。后代会一直跑到它自己愿意结束为止。
- 安全：在凭据执行器里，后代**自己内存里**持有物化的 Git Token。helper 只把 Token 交给 Git 正在运行的那个
  进程；一次执行结束了而它的后代还在跑，就是**凭据活得比执行更久**——这正是本包存在的理由。

进程组是按**负 pid** 寻址的，向它发信号会到达组内每一个成员，这就是机制的全部。组是子进程自己的组，不是调用方
的组，因此这里发不出任何能送到执行器自身或兄弟作业的信号。

## 接口与不变量

- `Isolate(command *exec.Cmd)`：把 `command` 设为其新进程组的头（`Setpgid`），并把它的 `Cancel` 换成「杀整个
  组」。必须在 `Start` **之前**调用：进程组与取消动作都是在进程创建时落定的。
- **无上下文的命令原样返回**：`Isolate` 用 `command.Cancel == nil` 区分 `exec.Command` 与
  `exec.CommandContext`。没有上下文的命令不会有人调用取消，把子进程移出调用方（shell、服务管理器）的组只会
  让它收不到本该收到的整树停止信号，却换来一个谁也不会发的信号。
- **覆盖调用方已有的 `Cancel`**：两者回答的是同一个事件，而「调用方想让这个进程以别的方式结束」在凭据这条
  论证下没有读法——被请求停止而没停的子进程，正是本包要处理的形态。
- **用 `SIGKILL` 而不是 `SIGTERM`**：本包只在调用方**已经决定这次工作结束**的路径上运行（超时、请求取消、
  关闭）。被礼貌请求的 Git 可以用没有上界的时间去完成一次它已被要求放弃的传输；要保护的是后代内存里的
  凭据，而 Git 没有任何先清掉它的关闭路径。
- **组已经不在不算错误**：上下文可能与命令自身退出发生在同一瞬间，报 `ESRCH` 会把一次正常完成的命令变成失败。
  包装 `os.ErrProcessDone` 正是告诉 `exec.Cmd.Wait` 保留命令自己的退出状态。
- 包是**按平台分文件**的：Unix 上实现，其他平台上是**有文档的空操作**（`processgroup_other.go`）。这样非 Unix
  构建不会拿一个它并不拥有的保证去编译。

## 本包不声称的事

- **不设 `Pdeathsig`**。它保证的是「父进程死则子进程死」，而部署里这已经由容器给出：执行器是它自己容器的
  PID 1，PID 1 一死，内核会杀掉该 PID 命名空间内的全部进程。本包要解决的是**父进程还活着、只是放弃了这次
  工作**时后代仍在运行；在这一点上 `Pdeathsig` 什么也不做。何况它在 Linux 上的语义是「创建该进程的**线程**
  退出时投递」而不是「进程退出时」，在一个 Go 程序里它会读起来像一个它给不出的保证。非容器化部署（例如
  systemd 单元）由执行器自己的取消路径界定，见下面「调用方」。
- **不管 Windows**：Windows 没有可这样寻址的进程组（对应能力是 Job Object），而启动 Git 的三个组件都以
  Linux 容器部署。

## 调用方

四个启动 Git 的点都需要它，缺一个就有一条「超时后仍持有 Token 的进程」的路：

- `internal/cloneexec`：`runGit` 与 `headCommit`（clone、镜像准备、推送三条路都经过 `runGit`）。
- `internal/mirrorpublish`：`run`。
- `internal/gitserver`：`startGit`（upload-pack / receive-pack；这条路上没有凭据，但一次被放弃的请求同样不该
  留下仍在跑的打包进程）。

部署侧的第二个保证在 `ora-space/cluster`：`clone-executor` 容器加 `init: true`，让 PID 1 的职责（回收僵尸）
有真正的 init 承担。

## 依赖与测试

- 依赖：仅标准库（`errors`、`os`、`os/exec`、`syscall`）。
- `processgroup_unix_test.go`：用**真实进程树**（一个 `sh` 起后台进程后 `wait`）测两件事——被 `Isolate` 的命令
  在上下文结束后**后代确实消失**（轮询 `kill(pid, 0)` 直到 `ESRCH`，因为被杀的子进程在被回收前是僵尸，而僵尸
  仍然应答信号 0）；以及**对照**：同样的程序不被 `Isolate` 时，上下文杀掉了调用方启动的那个进程，而后代仍在
  运行。可证伪：把 `Isolate` 改成空操作，第一条用例失败并报出存活的后代 pid。
