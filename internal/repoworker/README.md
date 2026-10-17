# repoworker：把仓库 Operation 推过 Repo Executor

[中文](README.md) | [English](README.en.md)

Cloud 侧驱动仓库 Operation 的工作进程（Cloud 同步与发布 ADR、执行器根 ADR D2）。它拥有所有跨进程边界的东西：
同步启动前读跟踪 ref、签发一次性授权、在续租的同时调用执行器、同步成功后先经 ref 查询核对再让 Cloud 记录成功。
**状态判定全部留在 `internal/core`**，每次写入都以租约世代为栅栏；等待网络时绝不持有事务。

| 文件 | 职责 |
|---|---|
| `worker.go` | `Config`/`New`（校验并填默认值：Slots 4、Lease 1m、Sync 30m、Publish 5m、Probe 5m、Interval 2s）、`Run`（按 Interval 循环，退出时取消在途任务并等它们收敛）、`Pass`（四个清扫 + 按空闲槽位领取 Operation / 核验 / 凭据验证）、`spawn`（先占槽位再起 goroutine，因此一轮不会超过 Slots）、`settleContext`（15s 后台超时，活得比 shutdown 久，用来记录被取消任务的去向） |
| `operation.go` | `runOperation`、`authorize`、`renew`、`settleSync`、`settleCallFailure`、`settleLost`、`syncVerdict`、`publishVerdict`、`runCheck`、`runVerification` |

## 关键规则

- **期望旧值读得尽可能晚**：同步在 `StartRepositoryOperation` 之前才读跟踪 ref（同步 ADR D2），绝不取可能滞后的快照记录；读不到就把租约还回去，不启动。
- **续租即所有权**：每 `Lease/3` 续一次；租约丢失（`core.IsLeaseLost`）立刻取消任务——清扫已经结束了那个 Operation，Git Server 也不会再认这个 ID。
- **成功必须先核对**：同步报 `applied` 之后，快照 ref 与跟踪 ref 都必须指向上报的提交，Cloud 才记录 `succeeded`；查询失败在租约内退避重试（1s 起、上限 15s），租约结束则不做任何记录，交给清扫重放。
- **按「任务是否可能已经执行」收敛**：执行器拒绝（429 `executor_busy`、403 身份）与未送达＝一定没跑，记为 `failed`；连接丢失＝可能跑过，同步记 `failed`（写入幂等，可重放），发布记 `uncertain`（永不盲目重发）。
- **不确定的发布用一次探测核对**：`runCheck` 用 `probe` 授权读一次目标 ref；`runVerification` 用 `receive-pack` 广告证明某个凭据版本具备推送能力，`verified` / `push_denied` / `verification_failed` 三选一。

## 测试

`harness_test.go` 提供 `Store`、`Executor` 与 `core.GitRefReader` 的假实现（含 UUID 夹具，因为签发的授权要过与执行器相同的校验），
其余测试直接驱动 `runOperation` / `runCheck` / `runVerification` 与 `Pass` / `Run`，确定性、不依赖 PostgreSQL。
等待只用事件通道，不用固定 sleep。并发与租约路径由 `task test:race` 覆盖。
