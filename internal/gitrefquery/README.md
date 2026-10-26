# gitrefquery：Cloud 的 Git Server ref 查询客户端

[中文](README.md) | [English](README.en.md)

Cloud 访问 Git Server 只读 ref 查询的客户端。Cloud 在任何数据库事务之外用它核对一个即将登记的 ref
（例如 Revision 的交付 ref）存在且指向被告知的提交。查询监听只接受 Cloud 固定的客户端证书；TLS 配置
（固定 Git Server CA、出示 Cloud 身份、不跟随重定向）由 `internal/config` 的 `git_server` 段构造后注入。

| 文件 | 职责 |
|---|---|
| `client.go` | `New`（base 必须是 `https://host[:port]`，需要 HTTP client 与正的超时）；`ResolveGitRef` 以 `GET /internal/v1/refs?tenantId=&projectId=&ref=` 查询，`200` 带合法提交即返回该提交 |

只有 `404 ref_not_found` 是确定的 `core.ErrGitRefNotFound`；其他响应、传输失败或超时都是不能证明任何事的
错误，调用方应重试而不是下结论。每次查询受超时约束，响应体大小有上限。本包实现 `core.GitRefResolver`，
`internal/core` 不导入它。
