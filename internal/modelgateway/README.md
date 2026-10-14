# modelgateway：个人密钥与运行模型代理

[中文](README.md) | [English](README.en.md)

本模块独占个人上游 API Key 的加解密和模型转发。业务归属、配置冻结、运行权限及授权摘要由
`internal/core` / PostgreSQL 所有；Controller 与 Node 只取得绑定引用和临时运行凭据。

| 文件 | 职责 |
|---|---|
| `crypto.go`、`key_verification.go` | AES-256-GCM 密文、不可变记录绑定、重试指纹及旧加密身份检查 |
| `credentials.go` | Gateway 独占的写入/清除边界，双重签名身份和当前个人归属 |
| `grants.go` | 专用 mTLS 模型身份的授权、同令牌续期与撤销 |
| `forward.go`、`events.go`、`envelopes.go`、`response_io.go` | 限定模型与路径、校验协议封装、HTTP/SSE 错误过滤及撤销时打断阻塞写入 |
| `transport.go`、`dns.go` | `NewUpstreamClient(UpstreamConfig)` 固定部署所有的 `DNSConfig`；通过认证 HTTPS DNS 与公网数值 IP 建立连接，禁用环境代理和重定向 |
| `diagnostics.go` | 只按类型生成有限错误类别，不读取上游错误文本；`Options.Logger` 仅记录类别 |
| `config.go`、`health.go` | 部署引用、监听用途和正常验证证书的就绪检查 |

凭据 HTTP 监听仅受浏览器 Gateway 的服务与用户凭据信任；授权监听需要 model-access 客户端证书；
模型数据监听需要临时令牌。三个入口不可互换。所有响应和错误禁止出现真实 Key。
成功的 SSE 逐事件刷新，HTTP 200 内的错误事件也归一为安全错误；撤销同时打断上游和阻塞的下游写入。
每个授权观察任务归属于一个请求，取消后必须等待它退出。

生产 DNS 不回退系统解析器。HTTPS 解析器由公网数值 IP 引导并正常验证 TLS；模型域名的 A/AAAA
答案全部通过地址策略后才允许数值拨号，TLS 仍验证原模型域名。仅部署配置中精确指定的开发
fixture 主机使用系统 DNS；该例外不能由用户连接配置开启。`2001:2::/48` 和 `198.18.0.0/15` 均拒绝。

原始 Key 仅在本服务内存中存在，密文写入前随机产生 nonce；Node 的临时令牌只保存摘要。
备份必须同时包含数据库和加密主密钥卷。启动时不匹配已有密文就失败，不重置历史记录。

`*_test.go` 用生成的临时密钥、真实 TLS/HTTP 和故障策略替身验证加密、证书用途、流式边界、
阻塞取消、诊断过滤及地址策略。`transport_network_test.go` 的可选无凭据探测只证明可信解析与
真实 TLS 连接，HTTP 401 不能作为模型调用验收。真实 PostgreSQL 归属和授权验证见 Core 测试；实际 OpenCode 与
模型服务证据由 cluster 独立 M4 验收提供。详见 [个人模型连接](../../docs/model-connections.md)。
