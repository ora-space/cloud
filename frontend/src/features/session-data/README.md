# 会话观测数据

[中文](README.md) | [English](README.en.md)

本模块是无框架边界，将 ora-history 归档或公开 Thread 条目投影为保守的会话观测数据。不负责请求、鉴权、计价、推断模型调用或解析 Agent 原生日志。

| 文件 | 职责 |
| --- | --- |
| `index.ts`、`types.ts` | 公开适配入口与视图契约。 |
| `adapter.ts` | 从显式输入组装快照。 |
| `records.ts` | 校验外层结构，按来源序号重放最终版本。 |
| `values.ts` | 校验时间、冻结模型元数据与文本块。 |
| `events.ts` | 分类观测记录并建立显式用户标记分组。 |
| `tool-data.ts` | 保留工具证据，按已知耗时聚合。 |
| `projection.ts` | 构造三泳道记录及已加载数据统计。 |
| `adapter.test.ts` | 合成、确定性的边界与保真测试。 |

依赖仅限标准 JavaScript；会话页面、时间轴组件及查询适配模块可通过 `@/features/session-data` 依赖本模块。不得跨模块引用内部辅助文件。

不变量：`events` 是按来源 seq 排序的最终版本重放结果；`rawRecords` 保留每个原始输入及被替代版本。非法身份的记录以 unknown 行保留在有序记录之后。归档 seq 可为零；Thread 用顶层正整数 seq 标识，独立于 `record.seq`。缺少 `record.at` 时保留未知，不用 `createdAt` 替代事件时间。用户标记编号表示观测分组，不是模型调用次数；Thread 回声不新建分组。思考、生命周期及未知记录留在完整列表。三泳道列表保留未知时间，条带定位要求 `startMs`；工具结束时间扩展观测范围。工具 kind/title 不代表原生工具名称。工具数是记录数，平均耗时仅使用已知值并包含真实零。提供商指标保持 null；冻结模型注明 `frozen_run_binding` 来源。公开 `turnId` 仅在实际存在时保留，不读取内部数据库字段。

运行 `npx vitest run src/features/session-data/adapter.test.ts`。夹具不含真实日志或会话身份；测试无网络、时钟或文件读写。JSON 展示由使用者负责，必须按纯文本渲染。

时间范围和工具最大耗时采用逐项计算，不把随记录数量增长的数组展开为函数参数。合成的 100000 条记录回归测试验证后端记录上限下的数据投影不会超过参数栈；这不代表浏览器渲染性能基准已验收。
