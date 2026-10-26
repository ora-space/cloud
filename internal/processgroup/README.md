# processgroup：取消时结束整个进程树

[中文](README.md) | [English](README.en.md)

`Isolate(cmd)` 在 `Start` 之前调用：`Setpgid` 让子进程领头一个新进程组，`Cancel` 换成向整个组发 `SIGKILL`，
`WaitDelay` 设为 5 秒（ADR `repo-executor/0` D4）。

为什么需要：`exec.CommandContext` 默认只杀它启动的那一个进程，而 Git 从来不是一个进程（传输 helper、索引器、
send-pack）。后代继承管道，在 Repo Executor 里还在内存中持有兑换来的凭据；只杀父进程就会让凭据活得比任务更久。

| 文件 | 职责 |
|---|---|
| `processgroup_unix.go` | Unix 实现：进程组、`SIGKILL` 整组、`WaitDelay` |
| `processgroup_other.go` | 其他平台：有文档的空操作（执行器只以 Linux 容器部署） |

不变量：

- 组是子进程自己的组，发不出能到达调用方或兄弟任务的信号。
- 用 `SIGKILL`：只在调用方已决定结束工作时运行，Git 没有先清掉内存中凭据的关闭路径。
- 组已不存在（`ESRCH`）返回 `os.ErrProcessDone`，保留命令自己的退出状态。
- `WaitDelay` 给逃出进程组仍持有管道的后代一个上界，超时后关闭管道、`Wait` 返回。
- 无上下文的命令（`Cancel == nil`）原样返回；覆盖调用方已设的 `Cancel` 与 `WaitDelay`。
- 不设 `Pdeathsig`：父进程死亡由容器 PID 命名空间（`init: true`）收尾。

测试用真实进程树（`sh` 起后台 `sleep` 后 `wait`）：被 `Isolate` 的命令取消后后代消失；对照组未 `Isolate` 时
后代存活；以及配置项与已结束命令的退出状态。依赖：仅标准库。
