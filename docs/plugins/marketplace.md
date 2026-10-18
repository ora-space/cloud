# 插件市场与空间插件

[English](marketplace.en.md) | 中文

插件是空间级别的资源：管理员为空间选择要安装的插件，Cloud 把这个选择分发到空间下每个存活的运行时，由运行时里的 Node 真正下载并安装。
Cloud 是两样东西的唯一权威：**市场目录快照**和**空间的插件选择状态**。Node 只下载和运行 Cloud 交给它的内容，从不自己同步市场。

插件的身份规则（清单、`namespace/identifier` 形式的 canonical Plugin ID）见 specs 的
[领域词汇表](https://github.com/ora-space/specs/blob/main/domain/glossary.md#plugin)。

## 数据

| 表 | 内容 |
|---|---|
| `plugin_sources` | 部署级的市场源。默认源使用 `official` 命名空间；`synced_at`、`sync_error` 记录新鲜度和最近一次失败 |
| `plugin_catalog_entries` | 同步得到的目录快照，主键是 `(source_namespace, identifier)` |
| `space_plugins` | 空间的插件选择：`desired_*` 是管理员的意图（版本在选择时固定），`observed_*` 是各运行时结果的聚合，带乐观版本 |
| `workspace_plugin_instances` | 分发到每个运行时的执行事实，一个运行时一行，复合外键防止跨租户引用 |
| `space_agents` | 空间的 Agent 名册，由 Agent 类插件的安装结果形成 |

## 目录同步

Cloud server 自己维护目录（`internal/pluginmarket`），启动时同步一次，之后每 `plugins.sync_interval`（默认 5 分钟）一次：

- 首次 clone 到临时目录再原子重命名，之后 fetch 并快进；源分支发生非快进时同步失败。同一时间只允许一个同步，并发的同步直接返回而不排队。
- 扫描 `registry/**/orax.toml`，按与 desktop 相同的规则校验每份清单（大小、resolver 版本、kind、标识符语法、文本策略、SHA-256 格式、下载
  地址与 targets 的互斥关系等）。损坏的条目单独跳过；同一个 Plugin ID 出现多次时按路径顺序取第一个；`marketplace_visible=false` 的条目不进
  目录。
- 一次成功的同步在单个事务里整源替换目录，读取方看不到中间状态；失败时保留上一版快照，只在源上记录失败原因，下一轮重试。
- **读取目录从不出网**：`GET .../spaces/:spaceId/plugins/catalog` 只读这张表，返回全量目录和 `syncedAt`，过滤和搜索在前端完成。
- 目录替换提交后，向所有 SSE 订阅者广播 `plugins.catalog_updated`。

`plugins.sync_enabled: false` 时不建市场源、不运行同步；开启时市场地址必须是不带查询和片段的 `https` Git 地址，分支名必须合法，否则 server
拒绝启动。

## 安装与移除

| 接口 | 说明 |
|---|---|
| `GET .../spaces/:spaceId/plugins` | 空间的插件选择与进度汇总，任何当前成员可读 |
| `POST .../spaces/:spaceId/plugins` | 安装：`{identifier, pluginVersion?}`，版本只能是目录当前的版本（否则 `400 plugin_version_unavailable`），省略时即取它，并在选择时固定 |
| `DELETE .../spaces/:spaceId/plugins` | 移除：`{identifier, version}` |

- 只有管理员可以修改选择（`403 space_role_required`），修改带乐观版本；`pack` 类插件可以出现在目录里，但不能安装。
- 重复安装同一版本是幂等的，不重复分发；它同时是管理员重新触发工作流资产导入的方式（见 [工作流](../workflows/workflows.md#插件资产导入)）。
- 选择被接受时，Cloud 为当时**已经存在**的每个存活运行时写一行待处理的实例；之后新建的运行时不会被追加到这次维护里，而是在自己创建或启动时
  补装（见下文）。空间里没有存活运行时时，选择立即视为完成。
- 汇总只给出受影响、已完成、等待启动等计数，不暴露别的运行时的失败输出。

## 运行时里的执行

插件的安装和移除都是 Node 上的执行，发生在运行时操作的 `plugin` 步骤里（见 [Workspace 生命周期](../runtime/workspaces.md)）：

- **进入 `plugin` 步骤时冻结集合**：创建和启动运行时取当时空间里所有期望安装的插件；`install_plugin`、`remove_plugin` 维护操作只含那一个插件。
  集合为空时直接跳过这个步骤。冻结之后新选择的插件不进入本次集合。
- 每一项都是从目录快照拼出的**自包含载荷**：Plugin ID、版本、下载地址、SHA-256 或 targets。Node 不需要查询市场，并在落盘前校验 SHA-256。
- Controller 用 `RecordDispatch` 登记一次 `install_plugins` 或 `remove_plugins` 执行，目标必须是该运行时当前代次的 Node，输入必须等于冻结的集合。
- **逐项结果**：安装成功的版本必须等于计划版本，否则整份结果被拒绝；失败的项以有界的错误码记在实例上，**不阻止**运行时就绪。整次执行失败时按
  clone 步骤的方式延期，用新的执行重试。迟到的结果如果不属于当前这次选择，实例回到待处理，不会覆盖更新的选择。

**维护等待空闲。** 对已经存在的运行时，维护操作只在它空闲时才开始，并且使用与用户操作相同的独占边界（见 [运行时控制](../runtime/runtime-control.md)）。
每个待处理的实例都带一个等待原因：

| 原因 | 含义 |
|---|---|
| `waiting_start` | 运行时已停止，等它下次启动时补装 |
| `waiting_control` | 有页面持有运行时的操作资格 |
| `waiting_lifecycle` | 运行时未就绪，或 Project 有其他操作 |
| `waiting_execution` | 运行时里还有未结束的执行 |
| `legacy_selection_unverified` | 历史选择缺少可核验的请求者，不会自动执行 |

Controller 每次领取操作时，Cloud 都会重新扫描待处理的实例，条件满足就为它创建维护操作，因此不需要额外的定时器。停止的运行时不会为了装插件而被启动。

**聚合规则**：任一实例失败则空间状态为 `failed`；否则有未结束的实例时为 `installing` 或 `removing`；全部结束（或没有存活运行时）时为
`installed` 或 `removed`。运行 Workspace 的实例只是那次运行的执行事实，不参与空间的聚合。

## Agent 名册

`kind = agent` 的插件在空间聚合变为 `installed` 时，Cloud 在同一事务里为它创建（或恢复）一行 `space_agents`，于是它可以在 Issue 上被 @；插件被移除时
这一行变为 `retired` 而不是被删除，历史运行仍能解析自己的执行者。某个运行时安装失败不会让已有的 Agent 从列表里消失。

Agent 运行使用自己的运行 Workspace：创建时的 `plugin` 步骤装上空间期望的全部插件，运行就绪前还会检查运行固定的 Agent 插件是否按固定版本装上
（见 [Agent IssueRun](../agent-runs/issue-runs.md#阶段)）。

## 相关决策

- [插件市场：cloud 权威选择状态、目录快照与自闭合 effect 载荷](https://github.com/ora-space/specs/blob/main/decisions/cloud/plugin-marketplace/0-plugin-marketplace.md)
- [管理员保存空间插件期望，逐运行时维护等待空闲并使用同一独占边界](https://github.com/ora-space/specs/blob/main/decisions/cloud/plugin-marketplace/20260927-admin-selection-and-idle-maintenance.md)
- [插件安装与移除由 Node 执行，新建 Workspace 自动补装 Space 插件](https://github.com/ora-space/specs/blob/main/decisions/cloud/plugin-marketplace/20260928-node-executes-plugin-installs.md)
- [Workflow 插件资产由 Cloud 自行取包、逐文档导入为可编辑副本](https://github.com/ora-space/specs/blob/main/decisions/cloud/plugin-marketplace/20261016-workflow-plugin-assets.md)
