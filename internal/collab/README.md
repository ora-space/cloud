# internal/collab：协作开发夹具

[中文](README.md) | [English](README.en.md)

实现 `core` 声明的协作端口的开发替身，不访问数据库，也不执行真实 Agent、Team、模型调用或工作流任务。

| 文件 / 类型 | 职责 |
|---|---|
| `FixtureAgentTeamDirectory` | 固定的两个 Agent 与一个 Team；按名称过滤、按类型和 ID 解析 |
| `FallbackDirectory` | 解析时先查真实目录；列表组合夹具与真实目录，Workflow 始终来自 Store |
| `DeterministicContextBuilder` | 整理 Store 提供的任务、Issue、近期评论、确认输入与上下文引用 |
| `MockExecutionDispatcher` | 接受后用 observer 同步报告模拟开始、消息和完成；Workflow 消息成为系统活动 |
| `MockInputAssistProvider` | 只为当前表单声明且尚未填写的已知字段建议确定值，不调用 LLM |
| `workflow-run-simulator.go` | 从 start 节点按排序后的广度优先顺序遍历边；生成带模拟标识的节点状态，不可达节点不生成状态 |

`WireDevelopmentFixtures` 是统一装配点：组合现有 Directory、设置 Context/Dispatcher/Assist/Simulator，保留真实 Forms。server 仅在显式 `collaboration.development_fixtures` 开启时调用；`cmd/ora-web` 和测试主动接入。外部登录配置不会自动打开夹具。

夹具 UUID 固定，便于重复演示；轨迹 ID 和时间不是固定值。端口组合与生产默认关闭由 `wire_test.go` 覆盖。协作语义见 [Issue 协作](../../docs/collaboration/interactions.md)，工作流语义见 [工作流](../../docs/workflows/workflows.md)。
