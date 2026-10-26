# 会话读取：会话列表、归档与模型请求统计

[English](session-reads.en.md) | 中文

除了实时的 [Thread](threads.md)，Cloud 还提供几个只读入口，用来浏览和分析已经发生的 Agent 会话：租户的会话列表、某次运行登记的
会话归档，以及经过模型网关的 HTTP 请求统计。它们只读取 Cloud 已经持久化的授权记录和登记的归档，不创建会话、不修改运行状态，也不
扫描用户个人的文件系统。

## 共同规则

- **只读的身份解析。** 这几个入口在通用的公开请求事务之前分派：只认已有的身份和华为别名，校验身份冲突、账号启用和当前租户成员身份，
  不即时创建用户、不补绑别名、不刷新运行时状态。
- **严格的查询参数。** 只接受文档列出的单值参数，未知、重复或空的参数返回 `400 invalid_session_filter`；归档和统计接口不接受任何查询
  参数，客户端不能传入 URL、路径或对象键。Gateway 在转发前就拒绝无法解析的查询串。
- 响应带 `Cache-Control: private, no-store`。
- **只展示已有的证据。** 会话记录不保证包含 token、费用、父子会话关系或每个工具的输入、输出和耗时；缺失的信息应显示为不可用，
  不能用默认值补造。

## 会话列表

`GET /api/v1/tenants/:tid/agent-sessions` 列出租户里已持久化的 Agent IssueRun，排除已删除的运行和 Issue。Chat 或 WorkflowRun 不算作这里的
会话。

- 每项包含运行与 Issue 的标识、Issue 标题和编号、可空的 Project 与 Agent 插件、运行状态和 Thread 状态、创建/开始/完成时间、可空的冻结
  模型摘要，以及 `historyAvailable`。
- 筛选：`projectId`（Project UUID）、`status`（单个运行状态）、`agent`（精确的插件标识）、`from`（含）与 `to`（不含）按创建时间，RFC 3339。
- 顺序为 `(createdAt, id)` 倒序，默认每页 50 条、最多 100 条；`after` 是上一页的 `nextCursor`，必须是当前租户里可见的 Agent 运行；不支持
  `before`。
- `historyAvailable` 只表示存在一个未过期的 Revision 登记，不保证对象存储此刻可达，也不保证文件在读取上限之内。

## 会话归档

`GET /api/v1/tenants/:tid/issues/:iid/runs/:rid/history` 读取这次运行登记的完整会话 JSONL：

1. 在短事务里核验成员身份、Issue、Agent 运行和未过期的 Revision 登记。对象键只能来自登记，并且必须精确匹配
   `revisions/{tenant}/{run}/{work}/session.jsonl`。
2. 事务提交后，用私有端点签名 `GET` 读取对象：不跟随重定向，传播取消，存储读取超时 30 秒；实际字节的大小和 SHA-256 必须与登记一致，
   至多读取 32 MiB、100000 条合法 UTF-8 JSONL 对象。
3. 返回之前在新事务里**再次**核验当前权限和登记一致性：读取期间成员被撤销、登记被替换，都不能让这个请求继续返回会话内容。

成功响应为 `{"source":"archive","records":[...],"sizeBytes":…,"sha256":"…"}`。`records` 保留文件中的顺序、原始字段、JSON 数字精度和同一个
`seq` 的重复更新，由消费者按会话记录的语义重放。记录里的 `seq` 是会话记录自己的序号，与 Cloud Thread 分页用的 `seq` 是两个序列，不能
互相替代。

| 情况 | 响应 |
|---|---|
| 没有登记或登记已过期 | `404 history_unavailable` |
| 实际大小或记录数超限 | `413 history_too_large` |
| 完整性校验失败或 JSONL 损坏 | `502 history_invalid` |
| 存储故障 | `502 history_unavailable` |

公开响应、错误和日志里都不出现私有存储地址、签名 URL、对象键或凭据。归档不是实时流，实时显示继续使用 Thread 和 Space 的 SSE 失效提示。

## 模型请求统计

经过 [model-gateway](../models/model-gateway.md) 的每一次上游 HTTP 尝试都会单独登记在 `model_request_attempts` 里：网关在真正发送请求之前
登记，之后写入完成观察。客户端重试是一次新的尝试；SSE 分块和 Thread 记录都不是请求次数。台账不含请求正文、密钥、请求头、供应商的原始
错误或用户的文件路径。服务崩溃留下的 `started` 观察保持未知，不推断成功、失败或用量为零。

两个读取接口在 IssueRun 和 WorkflowRun 下各有一组路径，都只读，与归档是否可用无关：IssueRun 路径校验当前成员、Issue 与
Agent 运行；WorkflowRun 路径校验当前成员，且运行属于路径里的 workflow、未被删除。租户成员只要能读该运行就能读统计，不限运行
发起者；归档的 workflow 连同它的运行历史一起不可读。

- `GET .../runs/:rid/statistics` 聚合这次运行的全部已登记尝试，不接受查询参数。
- `GET .../runs/:rid/model-calls` 按 `(startedAt, id)` 升序分页列出尝试，只接受 `limit`（默认 50、最大 100）和同一运行内的 UUID
  `after`；游标绑定该运行，不能跨运行使用。

统计的来源固定为 `model_gateway`、范围为 `recorded_http_attempts`：它不是历史会话的完整请求数，也不是账单。

- 六类 token（输入、输出、总量、缓存读、缓存写、推理）各自保留供应商给出的值，并带覆盖率：每个指标给出 `value`、`knownCount`、
  `totalCount` 与 `availability`，未知为 `null`，测得的 0 仍是 0。
- SSE 的累计用量快照是替换而不是相加；没有终止事件的快照只能算部分数据。OpenAI 的输入包含缓存读，Anthropic 的输入不含缓存读写；推理是
  输出的子集，不与输出重复相加。供应商没有报总量时不猜测总量，也不推算费用。
- 耗时来自网关的单调时钟；首个有效输出的耗时排除 ping、空 role 和只含用量的事件，它不是模型的 TTFT。汇总不随分页改变。
- `started` 可能是仍在进行的请求，也可能是进程退出留下的未完成观察。

Agent 运行时在最后的 `turnEnded` 里也可能带着 ACP 的 `token_usage`，那是另一个来源，与网关统计不相加。

## 相关决策

- [会话分析从 Cloud 授权记录与已登记归档建立只读视图](https://github.com/ora-space/specs/blob/main/decisions/cloud/thread/20261016-session-analysis-read-model.md)（approved）
- [模型用量按网关 HTTP 尝试独立登记，保留部分观测与未知值](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261016-recorded-http-attempt-statistics.md)（approved）

## 定时运行结论与产物

定时任务历史读取运行级 verdict（缺失即「未报告」）和已登记/被拒产物计数；执行完成不等于业务通过。
`GET .../issues/:iid/runs/:rid/artifacts` 返回当前登记元数据，不含对象键或下载 URL。
`GET .../issues/:iid/runs/:rid/artifacts/:aidx` 在短事务内核验当前成员、Issue、Run 与登记，事务外通过私有端点
读取对象并校验登记的大小/SHA-256（上限 10 MiB），返回前再次检查权限与登记。调用者只能给运行与序号，不能指定对象键。
对象缺失、登记被清理或校验失败返回 `404 artifact_unavailable`；未配置对象存储返回 `503 object_store_unavailable`。

成功响应为原始字节，强制 `Content-Disposition: attachment`（安全编码展示名称的 basename）、
`Content-Type: application/octet-stream`、`X-Content-Type-Options: nosniff` 与 `Cache-Control: private, no-store`。
浏览器只访问 Cloud 同源路径，不获得对象存储地址或凭据；HTML 也以附件下载。前端按需展开元数据，通过普通同源下载链接
每次重新鉴权，不展示链接有效期、不在应用内渲染文件。

`scheduled_task.artifacts_updated` 只触发历史/对应运行索引刷新，不携带产物事实。历史裁剪保留运行事实，产物按独立保留边界清理。
见 [verdict/产物 ADR](https://github.com/ora-space/specs/blob/main/decisions/cloud/scheduled-task/20261022-run-verdict-and-artifacts.md)。
非成员与资源不存在统一返回 `404 not_found`。仅成功/部分交付后索引被清空的产物标为已过期，无产物与上传失败不标过期。
