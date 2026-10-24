# Agent Run 残留回收：Cloud 责任与证据

[English](residual-reclamation.en.md) | 中文

[有界启动](startup-settlement.md)的 D4/D5 收尾可以先结束业务运行，把尚未查明的资源留在
`agent_run_residuals`。Cloud 的残留回收在原账本上继续取证，证据足够后才取得系统清理责任，沿已有
owned delete 执行删除。它不重新结算 [IssueRun](issue-runs.md)，也不重新开放运行 Workspace。

本文描述 Cloud 自动回收和第 2 步受控回收的实现合同，不作为 Controller、Sandbox Server 的物理终止、部署或联合故障验收证据。
[残留回收 ADR](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/20261018-residual-agent-run-reclamation.md)
仍为 `approved`；本步不是整份 ADR 已实施完成的声明。

## 账本状态和单一责任

| `reclaim_state` | 含义 | 取得责任的字段 |
|---|---|---|
| `reconciling` | 收集原身份事实，尚未授予删除资格 | ID、mode、operation 为空，authorization 为 `{}` |
| `reclaiming` | 已取得一份持久责任，继续推进或等待证据 | 固定 `reclaim_id`、mode 和非空 authorization；`owned_delete` 必须绑定唯一 operation |
| `cleared` | 原责任下的处置已完成 | 保留原责任，同时有 `cleared_at` 和 disposition |

自动回收使用 `owned_delete`，完成处置写 `released`；显式运维命令使用 `controlled`，可先完成为
`terminated_data_retained`，或对已有未完成意图登记 `externally_confirmed`。两种模式竞争同一责任，
不能相互覆盖；自动层不会因为失败、超时或受控入口存在而自动升级授权。

自动责任始终绑定同一个 `delete_workspace` operation。重启、换 lease、重试和 quiesce 拒绝不会产生第二个责任或 operation，
也不会把失败的历史 operation 恢复为运行态。普通删除、派发和许可入口继续检查残留保护；内部系统责任不能被当成公开删除开关。
账本、Run、Workspace、Project、operation 的租户、原绑定和 generation 在同一事务里重新比对；UUID 外键本身不证明这些范围一致。

[迁移](../../internal/core/migrations/README.md)保留已有账本，并增加独立意图、命令回执及逐目标终止计划。存量 D4/D5 行通过默认值进入 `reconciling`，不推断完成、不回填 D8，
不修改旧 `reason`、`responsibility` 或业务结算。旧行已有非空 `cleared_at` 时明确报错回滚，等待显式兼容处理（A2）。

## 原身份取证与删除资格

`reclaim_targets` 是原计划和事实目标的并集，包括原 operation/effect、预先分配的 sandbox ensure ID、sandbox、Node、
execution、clone、work 和数据目标。迟到事实可以补充原目标，不能将旧目标替换成当前 generation，不能用“查不到”删去未知目标。

新事实追加到 `reclaim_evidence`，包含逻辑事件键、事实、来源和数据库接收时间，不再写入旧 `responsibility.lateFacts`。
相同事实重放复用原回执；新证据、目标补充或状态推进增加 `reclaim_version`。证据接管与授权、许可、完成都使用 Cloud 的既有
串行事务边界，因此完成必须重新看本事务内的完整目标和最新事实。

系统授权要求原身份可定位、admission 已关闭、无冲突、无阻挡中的 Project 操作或其他活动，并满足交付和执行证据条件。
执行侧需要同一 runtime control 身份的关闭输入确认及明确的未完成执行列表，并检查原执行终态；逐目标的终止证据也可证明其
物理执行已封闭。仅在原计划和执行关系确实不存在时才能采用无目标证明，缺行、NULL result 或等待时间均不构成这种证明。

自动层只接受原 `deliver_revision` 执行真实登记、核验完成的 Revision 作为交付资格；其他实际交付执行也必须各自核验完成。
当前没有可统一引用的既有“明确放弃交付”记录来源。`failed`、`skipped`、业务超时、没有对象存储或无结果都不自动等于明确放弃（A1），
证据不足的条目继续保持未完成。

[迟到交付事实接管 ADR](https://github.com/ora-space/specs/blob/main/decisions/cloud/issue-run/20261022-late-delivery-fact-takeover.md)
的续作保护同样适用于残留救援，覆盖自动对账（`reconciling` / `reclaiming` / `cleared` 账本）和无账本 D8 的显式受控接管两类路径：
若同一 Issue 已有后创建的 Run（按 `run.enqueued` activity seq 比较），或原 Run 与同 Issue 其他 Run 的创建顺序证据缺失或有歧义，
原交付的迟到终态仅保存声明与回执，不登记 Revision，也不把它当作已验证交付或清理授权。只有顺序证据唯一且没有后创建 Run 时，
才按原交付的证据核验规则登记；事实接收本身不等于取得回收责任。

自动回收获授权后沿已有 quiesce → terminate → cleanup 推进。quiesce 拒绝使原 operation 留在 `retry_wait`，保留原责任及保护，稍后重新取证再试（A3）。
它不恢复新执行资格，不升级为强停，也不重新创建 operation。
既有 Controller 因终止未确认而请求 `blocked` 时，此系统责任也保存为可领取的 `retry_wait`；实际计划和许可仍重新检查证据。

该责任绑定的 `sandbox_terminate` / `workspace_data_delete` effect 失败是可重试观察，不是已处置结论。
重新取证后同一 effect 可推进到成功，失败与成功证据均保留；历史创建/执行失败、外部身份和已成功结论仍受冲突保护。
责任完成后，既有 effect 结果入口只接管原绑定事实或提交冲突审计，不推进 operation、改变其版本或重新授予执行资格。

每个原计划 sandbox/ensure ID 都需要对应成功终止结果同时包含 `terminated` 和 `lateEnsureFenced`。
仅有 `terminated_at`、ensure 失败、404 或经过一段时间都不够。当前安全目标可以复用已有 owned delete；额外历史 ID 缺少双确认时保持未完成，
不为它改写目标或另发终止 effect（A5）。数据删除 effect 的计划、许可和完成再次检查目标覆盖；同一事务确认数据删除成功、operation 成功、
Workspace 软删除并写 `cleared/released`，不改 Run 的 phase/status/result 或重复追加业务结算活动。

## 部署运维命令与两阶段处置

`cloudctl residual` 是使用部署数据库凭据的本地运维入口；认证主体来自数据库连接的 `session_user`。
可附加的 operator 标签仅为调用者自述，不能替代认证主体。凭据不进入输出或审计。
没有新增 HTTP 路由、tenant admin 旁路、Gateway JWT 权限或监听端口；隐藏 Run Workspace 的公开操作继续返回 404。

| 命令 | 范围与必要确认 |
|---|---|
| `list` / `get` | tenant 必填；查询可按 Project、Run、Workspace、operation 或 reclaim 相交过滤，详情只返回运维所需状态、目标、审计与证据 |
| `terminate` | 一个精确 Run/Workspace，reason、幂等键和显式影响确认；D8 需原 failed delete operation 身份或明确的无删除记录选择；默认保留数据 |
| `delete-data` | 一个已真实终止成功且 retained 的根意图，独立 reason、幂等键和数据删除确认；不能按裸 Workspace 发起 |
| `confirm-external` | 一个已存在且未完成的根或从属意图，reason、幂等键、说明、每个未完成目标的外部事实引用及承担剩余责任的确认 |

`list` 条目、`get` 的实际 residual 账本行以及存在时的资源输出通过 `stopEvidence` 展示持久化的 `stop_evidence`；资源仅在未选择意图时返回。没有证据时返回 `{}`，没有账本的历史 D8 列表条目同样返回 `{}`，但 `get` 中合成的历史 D8 residual 可能省略该字段。停止未确认记录保留 `kind=stop_unconfirmed`、`source=cloud`、`observedAt` 以及原身份的 `executionId`。这只是已有观察的展示，不证明物理终止，不授予终止、删除或清账权限，也不改变回收责任。

写命令不接受 tenant/Project 批量处置。所有命令键共用 tenant 内的回执空间；同键同规范输入返回原回执和当前进度，
同键不同动作、资源、父意图、reason 或确认返回 `idempotency_conflict`。重放不创建新责任或重复外部取证。

每个资源只有一个 `terminate` 根意图，最多一个依赖它的 `delete_data` 意图；两者各自按
`registered → terminating → succeeded` 推进。根意图持有唯一回收责任，从属意图不重新仲裁。
登记、原身份核对、关闭 admission/后续执行资格和计划写入在同一数据库事务提交；不在事务内调用 Controller。

每个原 sandbox/ensure 目标保留固定 effect ID 和 request。Cloud 通过既有 `ListForceStops`、`ConfirmForceStop`、
`GetEffectPermit` 字段投影这些受控计划，不新增 RPC/proto，也不往普通 force-stop 历史写伪记录。
当前 Controller lease、原 sandbox/generation 和 control epoch 仍约束许可与确认；每个目标必须同时确认
`terminated=true`、`lateEnsureFenced=true`，无目标时也须证明入口已关闭且没有计划或在途责任。
普通强停批量结束 operation、改业务状态的收尾逻辑不适用于受控回收。

终止完成后，根意图固定为 succeeded，账本（如有）写 `cleared/terminated_data_retained`，保留数据与 Workspace。
之后的显式 delete-data 沿同一责任绑定唯一 `delete_workspace` operation，从 cleanup 推进已有 operation/effect/permit 路径。
首次根完成记录、retained 历史和 `cleared_at` 不回退、不重置；只有真实 `removed=true` 齐全后，才完成从属意图、
软删除 Workspace、追加 released 处置并更新当前 disposition。失败沿同一 operation/effect 恢复，不能复活历史 failed operation。

外部确认必须覆盖每个未完成目标及其数据责任，引用可为工单或外部事实标识。它完成的是已有意图的责任转移，
停止该责任的新许可，不伪造 `terminated`、`lateEnsureFenced` 或 `removed`，也不据此开放同卷复用或授权删卷。
没有意图、意图已 succeeded、说明为空或漏目标均拒绝；迟到物理事实继续留存，不能静默把外部处置改成系统证明。

D8 有原删除操作时，按原 done Run、保留的 Workspace 绑定及 failed/node_unavailable delete operation 接管，不回填 D4/D5 账本。
若 D8 在删除声明建立前结束，部署运维须以精确 Run/Workspace 和显式 `--allow-missing-delete-operation` 选择无删除操作变体。
仅原 `done/workspace_unavailable`、已有交付结论、没有任何删除历史、保留原绑定且 admission 关闭的资源可受理；意图与从属删除保留
`legacyDeleteOperationMissing` 来源标记。默认命令不扩展准入，不补造原 operation，原业务结论仍不变。
无账本时根意图持有目标、证据、冲突和删除 operation 绑定；原 failed operation、Run 终态与结算保持不变。
普通扫描不会自动为 D8 创建受控责任；无账本或已 retained 也不能绕过 admission、Project 操作和数据复用保护。

## 既有事实入口与冲突

Cloud 使用现有 `runtime_ack`、创建事实、执行结果和 Revision intake；没有新增 RPC、proto 或 Controller 取证机制。
`runtime_ack` 通过原 control epoch/version、Node、sandbox 和 generation 检查后，将关闭输入和 `unfinishedExecutionIds` 原样接入账本。
会话终态和交付结果按原 execution/work/资源身份接管；Run 已 done 或账本已 cleared 也不能成为拒收一致迟到事实的理由。
成果救援沿既有 Revision 核验路径，不开放 admission，不启动新会话或执行。

一致重放不改原结论。矛盾终态先追加冲突审计并设置 `reclaim_conflicted_at`，提交这些事实及阻挡后才返回稳定的
`409 residual_fact_conflict`（A4）。规范事实、已有 `cleared_at` 和 disposition 保留，后续删除或复用受阻；本步没有解除冲突的入口。

## 对账、恢复与尚未覆盖的范围

`ReconcileAgentRunResidualsOnce` 独立扫描账本的 `reconciling/reclaiming`，不依赖业务 Run 仍处于 `releasing`。
扫描按 `reclaim_checked_at NULLS FIRST, created_at, run_id` 有界轮转；检查时间只是调度记录，不是完成证据。
每个条目重新读取目标、资格和原绑定；已有责任恢复原 operation，继续服从当前 Controller lease 和 effect 许可。
查询失败、证据不足和暂时拒绝均不清账、不扩大授权。

failed create 的原 effects 如果没有现成可主动发现的路径，就继续等待可验证的原身份事实（A6）。Cloud 接收证据和调度责任不证明
Controller 已新增发现能力，更不证明真实容器、迟到 ensure 围栏或物理数据删除已经通过生产验证。

| 范围 | Cloud 边界 |
|---|---|
| 第 2 步：Cloud 部署 CLI、受控意图、保留数据、独立删除与外部确认 | 本文说明直接 Cloud 合同；测试及连续 Cloud 路径是否通过须看实际执行证据 |
| 第 3 步：Controller / Sandbox Server 物理契约与逐目标证据 | 本文仅描述 Cloud 的证据要求，不声明物理验收完成 |
| 第 4 步：跨仓联合故障与恢复验收 | 未完成，不以单仓实现或数据库测试代替 |
| D8 历史删除放弃、无账本残留的接管 | 仅显式运维接管；不补账、不自动重试、不改历史业务结果；真实部署恢复留第 4 步 |

核心代码入口见 [core 代码地图](../../internal/core/README.md)。测试结果应以对应执行产物为准，本文不声明迁移、并发或端到端测试已经通过。
