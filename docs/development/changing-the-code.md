# 修改与扩展

[English](changing-the-code.en.md) | 中文

本文是修改 Cloud 代码时的操作指南：代码放在哪里、新增接口或表要改哪些地方、几种常见扩展的固定做法，以及容易踩的坑。通用的编码规则
（错误处理、并发、依赖、文件大小）以仓库根目录的 `AGENTS.md` 为准；涉及状态转换、归属、持久化、外部副作用、恢复、安全或兼容性的修改，
先读相关的 specs ADR。

## 代码放在哪里

| 位置 | 放什么 |
|---|---|
| `cmd/*` | 只做配置加载、依赖装配、生命周期和退出码；没有业务逻辑、没有 SQL |
| `internal/core` | 全部权威状态和策略：聚合、状态转换、事务、授权判定 |
| `internal/api/router` | HTTP 路由白名单、双凭据校验、严格解码、`Fault` 到 HTTP 的映射 |
| `internal/controlgrpc` | gRPC 到 `Store.Control` 的翻译，不含业务 |
| `internal/contract` | OpenAPI 文档的 Go 模型 |
| `internal/gateway`、`internal/modelgateway`、`internal/gitserver` | 各自独立进程的实现；只通过 core 暴露的方法读写业务状态 |
| `internal/repository`、`internal/config`、`internal/logger` | 连接池、配置加载、日志 |
| `internal/simulator`、`internal/collab` | 只用于开发和测试的替身与夹具 |

依赖单向：`cmd` 依赖其余包；router 与 controlgrpc 依赖 core；core 只依赖标准库、GORM 和迁移目录，不反向导入任何表现层包。core 需要外部
能力时在自己这边定义接口（例如对象存储、工作流资产下载、协作端口），由 `cmd` 注入实现。新的实现包放在 `internal/` 下；只有确实要作为公开
API 给外部模块复用的代码才放进 `pkg/`。

## 新增或修改一个公开接口

1. **路由**：在 `router.Routes()` 里加一条 `{Method, Path, "", Fields}`，`Fields` 是请求体允许的字段。新的路径参数在路由处理里绑定到
   `core.PublicRequest` 的字段上，必要时给 `PublicRequest` 加字段。
2. **字段类型**：不是字符串的请求字段，在 `validField` 里加一个分支，固定它的类型或元素形状。类型系统是封闭的，默认一切都是字符串。
3. **分派**：写操作在 `Store.Public`、读操作在 `readPublic` 里加分支（见下方"分派顺序"）。业务逻辑写在 core 的事务里：限定租户、校验成员与
   角色、需要时检查版本；`POST` 和 `DELETE` 的幂等由 `Public` 统一处理。
4. **OpenAPI**：在 `internal/contract/openapi.go` 里补 schema 和响应、请求、可选字段的定义，列表接口要被识别为列表。响应里的每个字段都必须在
   schema 里声明。
5. **生成**：运行 `task frontend:generate`（会先运行 `task openapi`），把 `api/openapi.json` 和 `frontend/src/api` 与源码放在同一个提交里。
   生成物不手改。
6. **测试**：在 `integration/` 里经路由驱动新接口；测试的 HTTP 传输会用 OpenAPI 校验每个真实响应。修改 Space 内的业务状态时，记得在事务里收集
   对应的 SSE 失效事件。

## 新增一张表或修改表结构

1. 新建 `internal/core/migrations/NNNN_描述.sql`，编号接在合并时 main 上的最新编号之后；已应用的迁移文件永远不改。改表用新的迁移 `ALTER`。
2. 遵循已有约定：`id uuid` 主键，可变资源带 `version`（`CHECK(version>0)`）和 `created_at`、`updated_at`，业务行用 `deleted_at` 软删除；每张表
   按 `tenant_id` 限定作用域，引用用户的列通过 `(tenant_id, user_id)` 复合外键指向 `tenant_memberships`。能由数据库保证的跨行规则写成约束。
3. 每个租户都要有的数据（例如 Issue 的系统状态列）不能在迁移里预置，因为租户只在运行时产生；在第一次读写时用 `ON CONFLICT DO NOTHING` 懒惰补齐。
4. 迁移要能安全重试，并同时测试新库和从上一版 schema 升级。`integration/cloud_test.go` 的升级测试有一份固定的"升级前基线"列表，只有在测试本来就
   要经过你的迁移升级时才把它加进去。

## 修改 Controller 契约

1. 修改 `proto/` 下的 `.proto`：`v1` 只做非破坏性修改，破坏性变更用并列的 `v2` package。运行 `task proto:lint`、`task proto:breaking`。
2. `task proto:generate` 重新生成 `internal/controlpb`，与改动一起提交。
3. 在 `internal/controlgrpc` 里把新 RPC 翻译成一个 `core.ControlRequest` 动作；业务写在 core 的 `Store.Control` 里，改变状态的动作用提交身份包裹，
   需要租约的动作校验 epoch。
4. 契约的语义由 specs 的 ADR 拥有；desktop 锁定合入 main 的提交并重新生成客户端（见 [Controller 内部控制契约](../runtime/controller-integration.md#修改契约)）。

## 新增一个后台循环

Cloud server 的周期性工作都是同一个形状，新增时照做：

- 循环由 `cmd/server` 拥有：用 `pluginmarket.RunSyncLoop` 启动，挂在同一个 `WaitGroup` 上，进程停止时取消并等待正在执行的一轮结束。
- 每一轮先在全局锁之外做一次有界、按确定顺序排序的只读扫描，再为每个对象开一个短事务，在事务里**重新读取**扫描时的条件，并以比较并交换的方式转换。
  重复的一轮必须什么都不做。
- 时间窗口一律按数据库时间比较；某个对象暂时卡住时回滚、留待下一轮，不刷日志，只有扫描本身失败才返回错误。

## 写在业务钩子里的东西

Agent 运行的业务转换写在控制面的五个钩子里（见 [同一事务里的业务钩子](../core/persistence.md#同一事务里的业务钩子)）：

- 钩子在调用方的事务里运行，通过 `businessTransaction` 找回外层事务；不开新事务、不起 goroutine、不做外部 I/O、不推迟任何提交后的副作用。
- 钩子只写业务表，执行证据只由控制面写；返回错误会以 `409 business_hook_failed` 回滚整个接管，所以不能"容忍"一个无法由权威数据证明的转换。

## 分派顺序与其他坑

1. **分派按路径子串匹配，顺序很重要。** `Public` 和 `readPublic` 里，路径更长、更具体的分支必须排在更宽泛的分支之前：Issue 下的子资源（评论、标签、
   订阅、交互、Thread）要在 Issue 的分支内部分派；`.../runs/:rid/thread` 要在 `.../runs` 之前；`.../workflows/:wfid/runs` 要在 Issue 的 `/runs`
   分支之前。OpenAPI 生成器里同样有按 `/issues` 子串匹配的分支，新资源的分支要放在它前面。
2. **Gin 的静态段优先于参数段**：`/issues/batch` 是安全的，字面量 `batch` 胜过 `:iid`，但这时 `c.Param("iid")` 为空。
3. **只有顶层列名转成 camelCase**，`jsonb` 里嵌套的键保持数据库原样；聚合列按 SQL 别名读取。
4. **`updated_at` 不是自动的**，每个 `UPDATE` 自己写 `updated_at=now()`；每个读改写都要 `version=version+1`。
5. **软删除要在每个查询里显式写** `deleted_at IS NULL`。
6. **事务里不做外部 I/O**：HTTP、Git、文件、对象存储、Node 都放到事务之外，需要时拆成"事务 → 外部调用 → 事务"，第二个事务重新检查前提。
7. **评论不是运行**：不要给评论表加 `run_id` 或 `interaction_id`，运行的来源记在运行自己的 `trigger_evidence_*` 上。
8. **对不可见资源返回 `404`**，不要用 `403` 泄露"存在但无权访问"。
9. **不要在公开文档里暴露内部控制字段**：运行 Workspace、阶段、执行身份、对象键这类字段在投影里显式剥离，新增列之后检查它会不会随 `SELECT *` 漏出去。

## 同步文档与 specs

- 行为、配置或接口变化时，同一个改动里更新 `docs/` 下对应的文档，两种语言一起改；包内实现细节更新所在模块的 `README.md` 与 `README.en.md`。
- 实现 specs ADR 的改动，在 PR 描述里写明 ADR 的状态；补齐了实现时同时在 specs 把 ADR 改为 `implemented`，并保持核心用例的证据状态准确。
