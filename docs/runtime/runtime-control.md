# 运行时使用权限、独占控制与强停

[English](runtime-control.en.md) | 中文

同一租户的成员共享 Project，但一个运行时 Workspace 里的内容（代码、文件、正在运行的任务）只属于少数人，而且同一
时刻只能有一个页面在操作它。本文说明三件事：谁能使用一个运行时，页面如何取得并交还独占的操作资格，以及管理员如何
强行停止一个运行时。

所有决定都在 Cloud 的 PostgreSQL 事务里做出；Controller 只负责把 Cloud 已经裁决的绑定送到 Node，Node 在真正的执行
入口处再次核验，拒绝旧的资格。

## 谁能使用运行时

| 人 | 能看到 | 能使用（读内容、操作） |
|---|---|---|
| 运行时的创建者（`creator_user_id`） | 完整内容 | 可以，仍需取得操作资格 |
| 当前管理员 | 完整内容 | 可以，仍需取得操作资格 |
| 其他当前成员 | 安全概况：状态、代次、创建者等，`canUse=false` | 不可以，`403 runtime_use_forbidden` |

- 判定只看**当前**成员状态和角色。Project 的 owner、某次操作的发起者都不因此获得运行时内容的使用权；管理员被降级后，
  连重放他从前成功请求的幂等响应也会先被重新授权拒绝。
- 创建者来自创建请求本身（`creator_evidence=verified_request`），历史数据按唯一匹配的创建操作回填
  （`creation_operation`）；无法确定创建者的历史运行时（`unknown`）只有管理员能使用。
- 没有成员级的共享授权表，也没有多人实时共同编辑。

## 独占操作资格

每个 Workspace 有一行 `runtime_controls`，同一时刻至多一个页面会话持有它。页面在执行写操作（启动、停止、重启、删除，
以及之后的执行）之前必须先取得资格：

| 接口 | 作用 |
|---|---|
| `GET .../workspaces/:wid/control` | 查看占用情况：状态、控制代次、持有者、到期时间。不返回会话 ID |
| `POST .../control/acquire` | 取得资格。Cloud 分配一个新的 `sessionId` 只返回给这个页面，`control_epoch` 加一 |
| `POST .../control/renew` | 续期，需要原 `sessionId` |
| `POST .../control/release` | 主动交还 |

**状态。**

```text
          acquire                     Node 确认绑定
idle ───────────────▶ acquiring ───────────────────▶ held
 ▲                        │                            │  release / 到期 / 成员失效 / Node 关闭输入
 │                        └──────────────┬─────────────┘
 │                                       ▼
 │      所有责任结清                  draining ──（强停）──▶ reconciling
 └──────────────────────────────────────┘
```

- **取得**：调用者必须能使用这个运行时；资格必须空闲；运行时处于 `ready` 或 `stopped` 且没有未结束的活动。运行时有存活的
  沙盒时进入 `acquiring`，等 Node 经 Controller 确认绑定后才变成 `held`；已停止且没有沙盒时直接 `held`。同一用户在第二个
  页面上也必须重新争取，不能共用。
- **有效期**：60 秒，由 PostgreSQL 时钟判定；契约约定调用方每 20 秒续期一次。续期只延长 `held` 且仍有效的资格。Web 前端的
  运行时控制交互由单独的 ADR 承接（approved），目前还没有接入。
- **交还**：释放、到期（页面不再续期）、持有者不再是当前成员或失去使用权、绑定的 Node 已离开时，资格进入 `draining`，关闭新的
  写入。Node 报告输入已关闭
  时同样进入 `draining`，即使数据库里的期限还没到，迟到的续期也不能让它复活。
- **结清才回到空闲**：`draining` 只有在旧入口已关闭（Node 确认输入关闭，或运行时已停止且没有沙盒），并且没有未结束的执行、
  clone、普通操作、强停或残留责任时，才回到 `idle`。仅仅"连接断了""时间过了"都不算证据。
- 幂等重放返回历史响应，但不会让已撤回的资格复活。每次状态变化都写入 `runtime_control_events`，保留持有者和原会话。

**生命周期操作与资格共用同一个写边界。** 生命周期操作一旦登记，就把资格置为 `maintenance`（记录操作 ID，控制代次加一），
页面会话在此期间不能操作；操作结束后资格进入 `draining`，按上面的规则结清。运行 Workspace 的创建操作成功后，资格转为属于创建它的那次运行的
`maintenance`（预留记录运行 ID，IssueRun 与 WorkflowRun 都认），之后的会话和交付都以它为边界。

管理员的普通操作不能抢占别人持有的资格；需要立即打断时使用强停。

## 送达 Node：绑定与许可

Cloud 记录的是"现在谁有资格"，真正的写入发生在 Node 上，所以资格必须送到 Node，并在每次执行前重新确认。这部分是
Controller 的 RPC（见 [Controller 内部控制契约](controller-integration.md)）：

- **绑定（`ListBindings`、`AcknowledgeBinding`）。** Controller 读取所有非空闲的资格，送到对应的 Node，再把 Node 的确认报回
  Cloud：资格是否已绑定到这个沙盒、Node 的输入是否已关闭、还有哪些未结束的执行。绑定里带着租户、Workspace、沙盒、运行代次、
  Node 宿主代次、控制代次和 Controller 租约代次，执行端据此核验范围，不接受调用方自称的成员身份。
- **执行许可（`GetExecutionPermit`）。** Controller 派发 clone、插件安装或 Agent 会话执行之前，必须为这一个执行取得新鲜的许可：
  资格处于该操作（或运行）的 `maintenance` 且已确认绑定，执行属于该操作、目标就是当前 Node、尚无结果、未被强停。许可从不由
  幂等响应复活。
- **effect 许可（`GetEffectPermit`）。** 派发 Substrate effect 之前同样取得许可，有效期不超过 10 秒，也不超过 Controller 租约。

Node 在持久接受一个执行、以及真正开始第一次变更之前，都会检查绑定，拒绝旧资格；回复丢失时仍按稳定的执行 ID 核对。
Controller 租约到期不能由缓存的许可延长。

## 强停

`POST /api/v1/tenants/:tid/workspaces/:wid/force-stop` 让管理员立即终止一个运行时，即使有普通操作正在进行、有页面正在使用。

- 只有当前管理员可以调用；请求体必须带 `reason`、当前 `version` 和 `impactConfirmed: true`，并带 `Idempotency-Key`。调用方
  必须明确提示：目标上的全部工作都会被打断，未保存的数据可能丢失。
- 强停是一份**独立的持久意图**（`runtime_force_stops`），不复用、不删除普通操作，也不伪造它们的结果。登记时就选定全部未终止的
  沙盒作为目标（包括还在分配中的），关闭准入，资格进入 `reconciling`。同一 Workspace 同一时刻只能有一个未完成的强停
  （`409 force_stop_pending`），期间新的生命周期请求和 effect 计划都被拒绝。
- Controller 通过 `ListForceStops` 读取计划，对每个目标取得 effect 许可并终止，再用 `ConfirmForceStop` 报告。只有
  `terminated=true` 且 `lateEnsureFenced=true` 的证据才被接受：执行端必须先持久化运行代次围栏和沙盒墓碑，保证迟到的创建请求
  不会再起一个沙盒。
- 所有目标确认之后，在同一个事务里：未结束的执行被标记为由强停终止，Node 实例结束、沙盒终止，进行中的普通操作以
  `administrative_force_stop` 失败（结果里引用强停 ID，原输入、发起者、执行身份和已有的真实结果都保留），Workspace 变为已停止。
  **数据卷保留**，另一个运行时的内容不受影响。
- `GET .../force-stop` 返回最近一次强停；无法确定目标的历史运行时（没有存活沙盒又不知道创建者）返回 `409 force_stop_target_unknown`。

## 尚未开放的入口

云端的文件、终端和完整 Agent 产品入口没有开放；它们必须先接入同样的使用权限、独占资格和执行许可边界。插件的真实执行已经由
Node 承担（见 [插件市场](../plugins/marketplace.md)）。Node 只与 Git Server 通信，不使用仓库凭据；外部仓库由仓库外的 Repo Executor
同步（见 [Workspace 生命周期](workspaces.md#workspace-的公开接口) 与 [Git Server](../repositories/git-server.md)）。

## 相关决策

- [共享项目的运行时内容只允许创建者和当前管理员使用](https://github.com/ora-space/specs/blob/main/decisions/cloud/workspace/20260927-runtime-workspace-use-permissions.md)
- [运行时操作权绑定独占会话，旧入口关闭并结清执行责任后才交接](https://github.com/ora-space/specs/blob/main/decisions/cloud/execution-admission/20260927-exclusive-runtime-control.md)
- [普通停止保护活动，管理员强停登记独立意图并等待目标真实终止](https://github.com/ora-space/specs/blob/main/decisions/cloud/operation/20260927-safe-stop-and-administrative-force-stop.md)
- [运行时控制由 Cloud 授权、Controller 派发、Node 在真实入口持久隔离旧资格](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260927-fenced-runtime-control-delivery.md)
- [执行访问准入跟随 Space 成员身份](https://github.com/ora-space/specs/blob/main/decisions/cloud/execution-admission/0-access-follows-space-membership.md)
- [运行时控制的前端交互与联合故障验收独立交付](https://github.com/ora-space/specs/blob/main/decisions/cloud/execution-admission/20261013-runtime-control-ui-and-fault-acceptance.md)（approved）
