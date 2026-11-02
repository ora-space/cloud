# 工作流文档边界

本包包含 React Flow 工作流文档的纯分析、发布校验、变量与节点语义，以及通过 Repository / Dispatcher 端口驱动的调度引擎。
文档分析与校验不访问数据库、HTTP、已安装能力清单或执行副作用；两个接口都保留传入的完整编排文档，不修改或持久化投影。

```go
analysis, err := workflow.AnalyzeWorkflow(graph)
err = workflow.ValidateWorkflow(graph)
```

`AnalyzeWorkflow` 先检查节点身份、边与容器归属，再沿根 Start 和可达 Loop 的入口计算参与集合。
`Analysis.UnusedNodeIDs` 是排序后的已知类型备用节点 ID；`UnrecognizedNodes` 按原始节点顺序列出
所有未知类型，包括可达的未知节点。成功时两个列表都非 nil。根图缺少 Start 时保留全图参与校验。

`ValidateWorkflow` 继续检查可达图的节点配置、DAG、Start 和全局声明、Aggregator / Iteration 派生类型、
Loop 配置及作用域，以及对被排除节点的变量引用。备用节点配置不参与检查，但无效身份、边和归属不能被过滤隐藏。
已知的历史 `prompt` / `tool` 类型沿用 desktop 的可解析行为；解析通过不代表运行时支持这些节点。

发布边界对应 `WorkflowGraph::parse`，启动边界另外调用 `validate_executable_graph`。
例如缺少 Start 的平面图、未指定本机可用执行器的 Agent 仍可能通过发布校验。
执行侧必须在启动前另行检查图、运行输入和能力可用性。保存草稿不应调用 `ValidateWorkflow`。

装配层将 `Analysis` 映射成 HTTP 传输类型，`ValidateWorkflow` 可直接作为回调传入。
失败返回 `*ValidationError`，包含领域分类 `Code`、可选的 `NodeID`、`Field` 和 `Reason`。
HTTP 错误码由适配层映射；诊断措辞不承诺逐字匹配 desktop。

## 行为依据与测试

兼容基线固定为
[ora-space/desktop df694c8](https://github.com/ora-space/desktop/tree/df694c8836fc70b1c789cc7e8e523d4d5b1b6c66/crates/application/src/workflow_run/engine)。
对应 `execution_document.rs`、`graph.rs`、`loop_graph.rs`、`loop_bindings.rs`、
`unused_references.rs`、`variable_pool.rs` 和各节点配置模块。
文件值沿用 desktop 的跨平台相对路径准入规则，接受旧版路径字符串，但不会改写原始 JSON。

| 测试 | 行为边界 |
| --- | --- |
| `document_test.go` | 编排投影、无效结构、未知类型、备用节点重新接入、无效引用、发布与启动校验边界 |
| `config_test.go` | Start / 全局声明、Agent 策略和 schema、Condition / Output 历史格式、Aggregator 拓扑及类型 |
| `container_test.go` | Loop 轮次上限、携带值与可见性；Iteration 归属、输入 / 收集类型及边界 |
| `testdata/spare-chain.json`、`testdata/iteration.json` | 改编自 desktop `execution_document/tests.rs` |
| `testdata/loop.json` | 改编自 desktop `loop_graph/tests.rs` |
| `condition_test.go` | Condition 的 case 顺序、空 case、and/or、各运算符、比较值模板与失败（改编自 desktop `condition.rs`） |
| `branch_test.go` | 引擎层的条件分支、未走分支不建行、Aggregator 首个已赋值与 falsy 值、启动时的分支校验（改编自 desktop `aggregator_tests.rs`、`engine.rs`） |
| `structured_test.go` | 结构化输出的提取顺序、Schema 子集校验与示例（改编自 desktop `structured_output.rs`） |
| `loop_runtime_test.go` | Loop feedback 同快照读取、until 的顺序与退出绕过未使用反馈 |
| `../workflowhost/containers_test.go` | PostgreSQL 上的 Loop 隔离、轮数上限、退出排空和晚回调、重启中断、Iteration 空数组/超限/失败策略、缺失 collect 与原子推进回滚 |

Loop 的每一轮是独立 scope，调度使用该轮的变量池和 Condition 决策；Iteration 的成员共享根 scope，以数组下标隔离节点记录与分支决策。
`ContainerRepository.AdvanceContainer` 将轮次 ledger、旧 scope 关闭、下一轮入口或父节点终态一次提交。LoopExit 先提交冻结输出和取消事实，再通过 `LoopExitDispatcher` 异步排空执行中的 session；清理回调持有运行门禁核对同一个父节点和 scope 后结算。
持久化仍由 store 实现，host 仍负责每次运行内的 Agent 串行执行。服务重启只中断，不恢复调度或重放 Agent。

```sh
go test -race ./internal/workflow
go test ./internal/workflow -run='^$' -fuzz=FuzzDocumentValidation -fuzztime=10s
```

夹具和变体表验证固定基线的接受 / 拒绝语义，不启动 desktop，也不替代运行时和界面端到端验收。
模糊测试检查无 panic、分析确定性、结果数组非 null，以及两个接口对结构失败的一致性。
