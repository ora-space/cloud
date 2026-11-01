# 工作流文档边界

本包对 React Flow 工作流 JSON 字符串做纯分析和发布校验，不访问数据库、HTTP、已安装能力清单或执行副作用。
两个接口都保留传入的完整编排文档，不修改或持久化投影。

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

```sh
go test -race ./internal/workflow
go test ./internal/workflow -run='^$' -fuzz=FuzzDocumentValidation -fuzztime=10s
```

夹具和变体表验证固定基线的接受 / 拒绝语义，不启动 desktop，也不替代运行时和界面端到端验收。
模糊测试检查无 panic、分析确定性、结果数组非 null，以及两个接口对结构失败的一致性。
