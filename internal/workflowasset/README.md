# internal/workflowasset：工作流插件文档读取

[中文](README.md) | [English](README.en.md)

负责下载并核验插件包、提取工作流 JSON、解析名称与图对象。没有数据库句柄，不执行工作流，不把包写到磁盘。

| 文件 | 职责 |
|---|---|
| `fetch.go` | `Fetcher.Fetch` 接受无凭据 HTTPS URL 或 S3 对象键，校验包 SHA-256；HTTPS 总下载时限 60 秒，不跟随重定向，注入的 transport 负责部署出网策略；nil transport 拒绝联网 |
| `archive.go` | `Documents` 只读取 ZIP 中 `assets/workflows/*.json` 的直接子文件，忽略目录、符号链接、其他扩展名和嵌套文件；按路径排序，重复路径保留首个 |
| `document.go` | `Parse` 要求 JSON 对象及非空、最多 200 字节的 trim 后名称；图是否可执行由调用方的发布门禁判断 |
| `workflowasset.go` | 包/文档结果类型、稳定错误码与大小常量 |

包最大 16 MiB，展开总量最大 64 MiB，条目最多 512，候选文档 1–256。忽略的 ZIP 条目也计入展开预算；提取同时约束实际读入量。包级失败不产生文档列表；单份 JSON 的错误由调用方逐份处理，不阻断其他文档。

`Fetcher.Timeout` 可在首次调用前注入 HTTPS 总下载时限；非正值仍使用 60 秒。生产 wiring 保持默认值，边界测试缩短时限并继续经过真实 transport、TLS 和失败清理路径，不等待生产秒数。

`Failure.Detail` 仅用于结构化日志，持久化来源只使用有限代码；`DocumentError` 提供有限原因。下载失败时，detail 取自受检拨号器的逐地址归因（数字地址与超时、拒绝等有限原因），不取 `http.Client` 的错误正文，因为后者带请求 URL 和签名 query。测试分别覆盖下载摘要/超限/取消/重定向、ZIP 边界和逐文档解析。导入租约、幂等和数据库写入在 `core/workflow_asset_imports.go`，见 [插件市场](../../docs/plugins/marketplace.md)。
