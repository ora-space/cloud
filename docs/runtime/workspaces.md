# Project、Workspace 与生命周期操作

[English](workspaces.en.md) | 中文

Project 是一个 Git 仓库在 Cloud 里的开发环境；Workspace 是 Project 下的一个**运行时**：自己的一份持久数据、
自己克隆的代码，以及挂载这份数据运行的沙盒和 Node。本文说明这些聚合的形状，以及创建、启动、停止、重启、
删除这些生命周期动作如何以持久操作的形式，由 Cloud 和 Controller 协作推进。

谁可以使用一个运行时、页面如何独占操作、强停如何工作，见 [运行时控制](runtime-control.md)；Controller 一侧的
RPC 契约见 [Controller 内部控制契约](controller-integration.md)。

## 参与方

```text
Cloud（权威状态、决定下一步）
  ▲  gRPC：领取操作、登记 effect 结果、推进
  │
Controller（只拨出；执行外部动作）
  ├──▶ Substrate（cluster 的 Sandbox Server）：创建、终止沙盒，删除 Workspace 数据
  └──▶ Node（沙盒里的 desktop ora-node）：clone、安装插件、空闲证明、Agent 会话
```

Cloud 不直接调用 Substrate 或 Node。它只保存意图和证据，并按证据决定操作能否进入下一步；外部动作由持有租约
的 Controller 执行，结果报回 Cloud 后才算数。Node 不持有 Cloud 凭据，也不直接与 Cloud 通信，它的连接、心跳、
结束和空闲状态都由 Controller 代为报告。

## 聚合

**Project**（`projects`，读取时与 `repository_bindings` 合并成一份文档）

- 属于租户和它唯一的 Space，`owner_user_id` 是创建者，作为持久归属和凭据外键保留；它**不**是额外的访问授权，
  租户的所有当前成员都能看到并使用 Project。
- 外部仓库记录在它唯一的仓库 Binding 上：地址、默认分支、可选的凭据引用。仓库地址只接受 `https` 或 `ssh`，
  不允许带查询、片段或内嵌密码；默认分支必须是一个具体的分支名，不接受 `HEAD`，因为 Cloud 不读取远端仓库，
  Node 也只 clone 字面分支。
- 生命周期 `lifecycle`：`provisioning`（创建中）→ `active` → `deleting` → `deleted`。创建操作进入终态时，无论
  成功还是失败，Project 都进入 `active`：失败的是它的 main Workspace，不是 Project。删除在 Node 拒绝停止时回到
  `active`。

**Workspace**（`workspaces`）

| kind | 说明 |
|---|---|
| `main` | 每个 Project 恰有一个，与 Project 同时创建，不能单独删除 |
| `isolated` | 用户为一个任务另建的运行时，与 `tasks` 一一对应（任务标题） |

- 每个 Workspace 有自己的持久数据，被它的每一代沙盒挂载为 Node 的 home；停止保留数据，只有删除才删数据。
  Workspace 之间不共享卷，也没有共享的 Project 仓库或 linked worktree。
- `requested_ref` 是要 clone 的分支，`base_commit_id` 是 clone 后得到的基础提交。创建 isolated Workspace 时
  `baseRef` 写 `HEAD` 表示 Project 的默认分支。
- `creator_user_id` 是真正发起创建的用户。运行时内容的使用权限按它判定，而不是按 Project 的 owner（见
  [运行时控制](runtime-control.md)）。
- 状态由三组字段描述：
  - `desired_state`：`running`、`stopped`、`deleted`，即用户想要的状态；
  - `observed_state`：`provisioning`、`starting`、`ready`、`stopping`、`stopped`、`deleting`、`deleted`、`unavailable`，即 Cloud 依据证据确认的状态；
  - `admission_open` 与 `admission_epoch`：是否接受新的执行；每次关闭准入都递增 epoch，旧的空闲证明随之失效。
- `runtime_generation` 是运行代次。每计划一次沙盒创建就递增，Node 的所有报告都必须属于当前代次。

**沙盒与 Node 实例**

- `sandbox_instances`：一代沙盒，`generation` 与 Workspace 的运行代次对应；同一 Workspace 至多一个未终止的沙盒。
- `node_instances`：一个 Node 进程实例（`node_id` 加 `node_incarnation_id`），挂在某个沙盒上；同一沙盒至多一个
  未结束的实例。

**运行 Workspace。** Agent IssueRun 也使用 isolated Workspace（`issue_run_id` 非空），由 Cloud 以触发运行的用户
身份建删，走同一套操作，但不出现在任何公开列表或接口里（见 [Agent IssueRun](../agent-runs/issue-runs.md)）。
运行 Workspace 恰好属于一次运行：`issue_run_id` 与 `workflow_run_id` 两列互斥，后者属于一次
[WorkflowRun](../workflows/workflows.md#运行)。

## 持久操作

所有生命周期动作都写成一行 `operations`，公开接口立即返回 `202` 和这行操作，之后由 Controller 推进：

| 状态 `state` | 含义 |
|---|---|
| `queued` | 等待 Controller 领取 |
| `running` | 已被领取，正在某个步骤上 |
| `retry_wait` | 暂停，`retry_at` 到期后可再次领取 |
| `blocked` | 暂停，只有用户显式重试（`POST /operations/:oid/retry`）才继续，Cloud 不自动重试；运行 Workspace 的创建例外，超过上限由 Cloud 放弃（见 [有界启动](../agent-runs/startup-settlement.md#创建-blocked-的有界放弃)） |
| `succeeded`、`failed` | 终态 |

**一个 Project 同一时间至多一个未结束的操作**（`queued`、`running`、`retry_wait`、`blocked`）。新的生命周期请求
在此期间返回 `409 operation_in_progress`。这个限制让同一 Project 的生命周期串行执行；它不提供多人同时编辑
同一目录的冲突合并。

### 操作领取与确定性拒绝

领取在 `queued`、已到期的 `retry_wait` 和 `running` 中按
`COALESCE(last_claimed_at, created_at), created_at, id` 升序轮转。`last_claimed_at` 只在实际返回操作时以
数据库时间写入，属于内部调度簿记，不进入公开操作和 gRPC 快照；历史操作初始为 NULL，首次按创建时间参与轮转。
每笔领取事务最多检查 16 个候选，结算拒绝后继续检查；预算耗尽返回空，下一轮继续领取。

处于 `clone` 步的 `create_project` / `create_workspace`，若没有未结算或成功的 clone、未决外部 effect
或执行，仓库凭据不存在、冻结、范围不匹配或执行器不可用会在领取事务内结算为 `failed`，保留对应
`repository_credential_*` 错误码、清除 `retry_at` 并递增版本。同一事务将 Workspace 标为 `unavailable`、
关闭执行准入、归还运行时维护控制；运行 Workspace 同时执行 `runWorkspaceSettled(failed)`。
步骤、请求和外部资源身份保留；钩子失败则整笔事务回滚。已登记的未决执行和成功 clone 继续原恢复路径，
不会因为后来冻结凭据而被改写；网络超时与提供方故障不在此确定性拒绝分类内。

每种操作经过的步骤 `step`：

| 操作 `kind` | 步骤 |
|---|---|
| `create_project`、`create_workspace` | `sandbox` → `node` → `clone` → `plugin` → `done` |
| `start` | `sandbox` → `node` → `plugin` → `done` |
| `restart` | `quiesce` → `terminate` → `sandbox` → `node` → `plugin` → `done` |
| `stop` | `quiesce` → `terminate` → `done` |
| `delete_workspace`、`delete_project` | `quiesce` → `terminate` → `cleanup` → `done` |
| `install_plugin`、`remove_plugin` | `plugin` → `done` |

没有插件需要处理时，`plugin` 步骤直接跳过。各步骤推进的条件：

| 步骤 | Controller 做什么 | Cloud 推进的条件 |
|---|---|---|
| `sandbox` | 计划并执行 `sandbox_ensure` effect，创建这一代沙盒 | effect 成功，结果里带回沙盒 ID 和 Node 必须出示的 `nodeId` |
| `node` | 与沙盒里的 Node 握手，报告它已注册 | 当前代次、未终止的沙盒上有一个已初始化、已连接、30 秒内有心跳的 Node，且 `nodeId` 与 ensure 结果一致 |
| `clone` | 登记并派发一次 clone 执行，接管 Node 的结果 | 最近一次 clone 在当前 Node 上成功，带回真实的 40 或 64 位提交；提交写入 `base_commit_id` |
| `plugin` | 登记并派发插件安装或移除执行 | 冻结的插件集合里每一项都有结果。单项失败记在实例上，不阻止 Workspace 就绪 |
| `quiesce` | 请 Node 原子地检查自身活动并报告空闲 | 没有未结束的执行；有存活沙盒时，Node 已针对当前 `admission_epoch` 给出空闲证明 |
| `terminate` | 计划并执行 `sandbox_terminate` | effect 成功且 `terminated=true`，即旧进程确认不会再访问存储 |
| `cleanup` | 为每个待删 Workspace 计划并执行 `workspace_data_delete` | 每个 effect 成功且 `removed=true`；计划之前该 Workspace 不能还有未终止的沙盒 |

"Workspace 就绪"（`observed_state=ready`、`admission_open=true`）和操作成功在同一个事务里提交，提交前再检查一次
Node，保证准入不会在 Node 已经离开时打开。

**几条关键规则。**

- **clone 有界重试。** Node 报告 clone 失败后，Controller 以 `clone_failed` 延期，Cloud 按第 1、2 次失败后等待
  5、10 秒安排重试，每次重试都是一次新的 clone 执行；第 3 次失败时操作失败，Workspace 进入 `unavailable`。
  结果未知（`clone_result_unknown`）的 clone 不会被自动重试，也不能登记第二次。
- **停止要先证明空闲。** `stop`、`restart`、删除先在同一事务里检查没有未结束的执行，关闭准入并递增
  `admission_epoch`，再请 Node 报告空闲。Node 报告"不空闲"时，操作以 `resource_in_use` 失败，所有 Workspace
  恢复到操作之前的状态；Project 删除被拒绝时 Project 也回到 `active`，不取消任何工作。没有 Node、心跳过期或
  状态不明都不能推进。
- **代次隔离旧实例。** 计划新的沙盒之前，旧沙盒必须已经确认终止；Node 的注册和报告都绑定当前代次和存活沙盒，
  迟到的旧代次报告返回 `409 stale_node` 或 `stale_sandbox`。数据库能拒绝迟到的写入，但不能终止已经在运行的进程，
  所以"确认终止"是真实的外部证据，无法确认时操作停在 `blocked`，不分配新代次。
- **Node 断连关闭准入。** Controller 报告 Node 断开时，就绪的 Workspace 进入 `unavailable` 并关闭准入；同一个
  已初始化的 Node 重新连上后恢复可用，但被撤回的页面操作资格不会因此恢复。
- **删除 Project 一次关闭所有 Workspace。** 只有管理员能删除；Project 下任何一个 Workspace 有活动或被页面占用，
  整个删除就被拒绝，不做部分删除。删除需要等 Project 仓库上已经在运行的同步或发布结束后才开始（见
  [Git Server](../repositories/git-server.md)）。

## effect：外部副作用的持久意图

Controller 对 Substrate 的每个动作都先在 Cloud 计划成一行 `external_effects`，再去执行。effect 的 ID 就是外部
幂等键（`sandbox_ensure` 的 effect ID 同时是预分配的沙盒实例 ID）。

| kind | 作用 | 成功证据 |
|---|---|---|
| `sandbox_ensure` | 为 Workspace 创建这一代沙盒 | 沙盒实例 ID 与 effect ID 一致，带回 `nodeId` |
| `sandbox_terminate` | 终止当前沙盒 | `terminated=true` |
| `workspace_data_delete` | 删除 Workspace 的持久数据 | `removed=true` |

- **结果不可改写。** 外部 ID 一经登记不能改变，成功的结果不能被另一份结果覆盖（`409 external_state_conflict`、
  `external_binding_conflict`）。
- **不凭超时推断。** 失败或超时只让 Controller 以有限的错误码把操作延期为 `retry_wait` 或 `blocked`，effect 的
  计划、外部引用和当前步骤都保留；"调用超时"从不被当作"外部没有执行"。
- **换了 Controller 先对账。** 每个 effect 记录对账它的租约 epoch。Controller 租约易主后，旧 epoch 的 effect 必须
  先按稳定 ID 向 Substrate 查询并重新登记观察结果，计划和推进才会继续（否则 `409 reconcile_required`）。
- Substrate 必须支持"按 ID 查询而不创建"，以便在"执行成功但响应丢失"后找回实际对象；终止必须给出旧进程不会
  再访问存储的证据，不确定就不报告成功。

## Workspace 的公开接口

| 接口 | 说明 |
|---|---|
| `POST /tenants/:tid/projects`（或 `/spaces/:spaceId/projects`） | 创建 Project 和它的 main Workspace，返回 `create_project` 操作 |
| `PATCH`、`DELETE /tenants/:tid/projects/:pid` | 改名；删除（仅管理员） |
| `POST /tenants/:tid/projects/:pid/workspaces` | 创建 isolated Workspace，带任务标题和 `baseRef` |
| `GET /tenants/:tid/projects/:pid/workspaces` | 列出运行时：能使用的人看到完整内容，其他成员只看到安全概况（状态、代次、创建者等） |
| `POST .../workspaces/:wid/start`、`/stop`、`/restart`，`DELETE .../workspaces/:wid` | 生命周期动作，需要当前页面操作资格；只能删除 isolated Workspace |
| `GET`、`POST .../operations/:oid`、`.../retry` | 读取操作；重试 `blocked` 或 `retry_wait` 的操作 |
| `GET /tenants/:tid/resource-status` | 管理员读取全租户运行时的安全概况，不含仓库地址、凭据或请求结果 |

启动要求 Workspace 已经完成过初始化（有 `base_commit_id`）并处于已停止状态；启动和重启都不重新 clone，而是挂载
原有数据。

**仓库凭据。** 创建 Project 时可以引用一条属于自己的 Git 凭据引用（`credential_refs`，只保存基础设施引用，不保存
密钥值）。但生产环境还没有能按引用解析凭据的执行端，所以任何需要凭据的新远程 Git 操作都明确拒绝为
`409 repository_credential_executor_unavailable`，不会降级成匿名访问或改用全局凭据；匿名的公开仓库照常工作。

租户级的 `/tenants/:tid/clones` 是早期的独立 clone 请求接口，生产环境里新建请求返回 `410 runtime_scope_required`，
只保留历史读取；Workspace 的 clone 一律走上面的 `clone` 步骤。

## 相关决策

- [运行时 Workspace 生命周期改为适配 desktop Node](https://github.com/ora-space/specs/blob/main/decisions/cloud/operation/0-workspace-runtime-follows-desktop-node.md)
- [Project 必须给出具体默认分支，Cloud 不读取远端仓库解析 HEAD](https://github.com/ora-space/specs/blob/main/decisions/cloud/operation/20260926-require-concrete-default-branch.md)
- [普通停止保护活动，管理员强停登记独立意图并等待目标真实终止](https://github.com/ora-space/specs/blob/main/decisions/cloud/operation/20260927-safe-stop-and-administrative-force-stop.md)
- [Workspace 创建与启动增加 plugin 步骤并由 Node 执行](https://github.com/ora-space/specs/blob/main/decisions/cloud/operation/20260928-plugin-step-and-run-workspace-release.md)
- [Internal Control gRPC 增加 Workspace 操作与 Controller 代报 Node](https://github.com/ora-space/specs/blob/main/decisions/cloud/controller-integration/20260926-workspace-operations-and-controller-reported-nodes.md)
- [Project 归属 Space：以成员身份取代个人所有作为可见与授权边界](https://github.com/ora-space/specs/blob/main/decisions/cloud/project/0-project-space-scoping.md)
- [仓库访问凭据按团队、个人或未知归属管理](https://github.com/ora-space/specs/blob/main/decisions/cloud/project/20260927-repository-credential-continuity.md)（approved）
