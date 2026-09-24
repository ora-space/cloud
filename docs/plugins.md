# 插件市场:cloud 权威状态与 catalog 快照

> 对应 `plan.md`(功能设计)与 desktop 仓的插件契约(orax.toml / 目录条目 / 命名空间)。cloud 是插件
> **选择状态**与**市场目录快照**的唯一权威;Node 执行平面只下载与运行 cloud fan-out 出来的内容,
> 从不自行同步市场。desktop 仓本期零改动,其本地同步与安装运行时保持原样。

## 数据归属

- `plugin_sources`(0015):部署全局的市场源。默认源保留 `official` 命名空间,与 desktop 身份模型一致;
  `synced_at`/`sync_error` 记录新鲜度与最近一次失败,`enabled` 为将来多源预留。
- `plugin_catalog_entries`(0015):同步产物,主键 `(source_namespace, identifier)`。**目录读取永远不出网**
  —— API 只读这张表,等价于 desktop 的 cache-only 语义。一次成功同步在单事务内 DELETE+INSERT 整源
  替换,读者看不到中间态;失败保留上一版快照。
- `space_plugins`(0015):工作区插件选择状态(cloud 权威,与技能、智能体同级)。canonical identity =
  `(source_namespace, identifier)` 显式列,满足 execution-contract 的"显式版本化契约"要求。
  `desired_*` 是用户意图(安装时固定版本,D2),`observed_*` 是 fan-out 聚合事实,乐观 `version` 防并发。
- `workspace_plugin_instances`(0015):fan-out 执行事实,每个 live 运行时 workspace 一行,复合外键继承
  `(workspace_id, tenant_id, owner_user_id)` 与 `(workspace_id, project_id)`,防止跨租户引用。

## 同步(`internal/pluginmarket` + `cmd/server` 接线)

- go-git 纯 Go 传输(D3 选 ①):首次 clone 到临时目录再原子 rename;之后 fetch + checkout +
  `pull --ff-only`(go-git 只支持 fast-forward merge,非 ff 即报错,与 desktop gitlancer 同语义)。
- 单飞准入:并发 `Sync` 直接返回 `ErrSyncInFlight`(不排队,desktop "turn away" 语义)。
- 扫描镜像 desktop 校验:清单 ≤ 1 MiB、resolver=1、8 种 kind、标识符 slug 语法、title/description/
  license 文本策略、sha256 64 位 hex、url(S3 对象键或 https)/targets(agent/hook 专属、canonical
  三元组白名单、去重)互斥、pack 无 release、`marketplace_visible=false` 跳过;损坏条目单独跳过,
  同 canonical id 按路径序首个胜出;README 截断存储、logo 变体(universal/light/dark)记录组合而非字节。
- `RunSyncLoop`:启动立即同步一次,此后每 `plugins.sync_interval`(默认 5m)一次;ctx 取消退出
  (进程 WaitGroup 等待);失败只写 `sync_error` 与结构化日志,下个 tick 重试。
- 目录替换提交后向所有活跃 SSE 订阅广播 `plugins.catalog_updated`(MVP 内存 hub 的 `PublishAll`,
  多实例换 broker,边界不变)。

## 公开 API(router 白名单 + contract 生成物)

| 路由 | 语义 |
|---|---|
| `GET …/spaces/:spaceId/plugins/catalog` | 全量目录 + `syncedAt` 新鲜度(v1 全量下发,过滤在前端) |
| `GET …/spaces/:spaceId/plugins` | 该 space 的选择状态(desired/observed 徽章数据) |
| `POST …/spaces/:spaceId/plugins` | `{identifier, pluginVersion?}`;缺省固定目录当前版本;幂等(重复安装不重复 fan-out);pack v1 禁装 |
| `DELETE …/spaces/:spaceId/plugins` | `{identifier, version}`;乐观版本 428/409;移除 fan-out |

均为公开路由:双 JWT + space 成员门控 + 严格解码(未知字段 400 `unknown_field`)。`version` 字段名留给
乐观并发,插件版本用 `pluginVersion` 字符串。安装/移除是成员级操作(与 space 内创建 project 门控一致)。

## 安装链路(R4:先持久化 plan,再外部副作用)

1. POST 事务:upsert `space_plugins`(desired=installed、版本固定)→ 对每个 live 运行时 workspace
   upsert 实例行(pending)并创建 `install_plugin` operation(workspace 绑定)→ 提交后广播
   `space.plugins_updated`。目标 project 有在途 operation 时整个安装 409 `operation_in_progress`
   (`one_project_operation` 兜底);同一 project 多 workspace 时每次安装只放行一个 operation,
   其余实例保持 pending,由下次安装收敛。
2. Controller claim → plan `plugin_ensure`:payload 从**目录快照**自包含拼装
   `{kind, projectId, workspaceId, pluginId, version, universal{url,sha256} | targets[{target,url,sha256}]}`,
   与 desktop `DownloadRequest` 能力一一对应;准入与 node 步骤同门控(workspace ready 才派发)。
   计划即回写实例 `installing`。
3. Node(本期 simulator)下载 → **sha256 必校验** → 原子安装到 `plugins/installed/<ns>/<name>/<version>`;
   `plugin_delete` 幂等卸载。
4. effect_result 成功证据校验(`installed=true` 且 `version` 与计划一致,缺失 → 400
   `invalid_plugin_evidence`);失败回写实例 failed + `install_error`。
5. advance:实例 → installed/removed,同事务重算 space 聚合并广播 `space.plugins_updated`。

聚合规则(纯函数,单测覆盖):任一实例 failed → failed;否则任一非终态 → installing/removing;
全部终态或没有 live workspace → installed/removed。

## SSE 事件(只带失效信息,R5)

- `space.plugins_updated`(spaceId/version):选择状态或执行回写提交后广播。
- `plugins.catalog_updated`(无 space 上下文):同步提交后 `PublishAll` 到所有订阅者。

前端 `use-space-events.ts` 按类型失效对应查询;目录查询 staleTime 对齐 5 分钟同步节奏。

## v1 边界与后续 phase

- fan-out 只覆盖安装时刻已存在的 live workspace;之后新建 workspace 的自动补装属于
  desired-state 收敛(后续 phase)。Q1a/Q1b 见 plan.md。
- pack v1 禁装(目录可列出);激活/停止/配置/日志属于 Node 运行时平面(执行契约同口径),不在本期。
- 真实 Node 侧的 `plugin_ensure`/`plugin_delete` 执行器接线为后续工作(desktop 仓另立项);
  本期 simulator 保证 effect 载荷对现有 desktop `plugin-manager` 能力自闭合。
