# gitrefs：Git Server 只读 ref 查询的客户端

[中文](README.md) | [English](README.en.md)

Cloud 侧调用 Git Server 只读 ref 查询（Git Server 契约 D6）的客户端。Cloud 从不写 Git Server：ref 在事务外读、读到的值在事务内记录，
所以这个包只回答「这个 ref 指向哪个提交」和「这个前缀下有哪些 ref」，用 Cloud 那张被 Git Server 固定的客户端证书认证。

| 文件 | 职责 |
|---|---|
| `client.go` | `New`（TLS 1.3 双向认证、钉住的客户端身份、默认 10s 超时、不跟随重定向）、`ResolveGitRef`、`ListGitRefs`、`get` |

## 不变量

- 只有 200 会解码；404 且 `code=ref_not_found` 表示 ref 不存在；**其余任何状态码都是错误**，这样「服务器不可用」永远不会被当成「ref 不存在」。
- 应答有 4 MiB 上限（`maxResponse`），超限按失败处理而不是无界缓冲。
- 提交必须是完整的 40/64 位小写十六进制；缩写、大写或空值都拒绝——它们要与 Git 自己的输出做字符串比较。
- 前缀列表里出现前缀之外的 ref、或指向非法提交的 ref，整条应答作废。

`NewWithHTTP` 用于测试与自定义传输层；生产路径只有 `New`。

## 测试

`client_test.go` 用 `httptest` 驱动，不依赖真实 Git Server：TLS 材料（CA 与客户端对）在测试内生成到临时目录，
覆盖 URL/CA/证书的拒绝分支、请求形状与查询参数、每个状态码与应答形状、超限、非法 JSON、上下文取消与不可达服务器。
