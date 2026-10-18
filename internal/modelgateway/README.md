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
| `attempt.go`、`usage.go` | 每次上游 HTTP attempt 的持久化登记、单调时钟计时、协议 usage 快照与有界幂等收尾 |
| `config.go`、`health.go` | 部署引用、监听用途和正常验证证书的就绪检查 |

凭据 HTTP 监听仅受浏览器 Gateway 的服务与用户凭据信任；授权监听需要 model-access 客户端证书；
模型数据监听需要临时令牌。三个入口不可互换。所有响应和错误禁止出现真实 Key。
成功的 SSE 逐事件刷新，HTTP 200 内的错误事件也归一为安全错误；撤销同时打断上游和阻塞的下游写入。
每个授权观察任务归属于一个请求，取消后必须等待它退出。

模型转发在 `Upstream.Do` 前调用 Core `BeginModelAttempt`，失败时返回安全的
`model_metrics_unavailable`，不会请求上游。一次登记表示一次网关发送意图；它不能证明远端已经
接收，更不是用户轮次或自动推断的逻辑请求数。网关不重试模型调用。结束时使用独立的 3 秒
上下文，最多三次提交同一 attempt ID 和不可变结果；仍失败则保留 `started`，只记录安全 ID。
进程崩溃或登记后未能发送也可能留下 `started`，不得解释为成功或零用量。

成功 JSON 和 SSE 字节仍原样转发。采集只保留允许的整数计数、状态与耗时，禁止正文、工具参数、
上游响应头、Key 和临时令牌。usage 按累计快照覆盖，不累加重复最终帧；缺失、负数、非整数及超过
浏览器精确整数范围的值保持未知。OpenAI 采 `prompt_tokens`、`completion_tokens`、`total_tokens`、
`prompt_tokens_details.cached_tokens/cache_write_tokens` 和 `completion_tokens_details.reasoning_tokens`；
输入含缓存。Anthropic 采 `input_tokens`、`output_tokens`、`cache_read_input_tokens`、
`cache_creation_input_tokens` 和已报告的 `output_tokens_details.thinking_tokens`；输入不含缓存，
总量保持未知。推理 token 是 output 细项，禁止再次相加；本模块不估价或保存供应商费用扩展。

OpenAI 流使用最终用量块：接受标准的空 `choices`，以及 OpenRouter 官方约定的无内容
`delta` 加已结束 `finish_reason` 的用量帧；未结束或错误 choice 不代表最终用量。
Anthropic 合并 `message_start` 与累计 `message_delta`。
终止标记独立保存：OpenAI `[DONE]`、Anthropic `message_stop`；正常 EOF 缺标记时保持原转发行为，
但 attempt 记为 `incomplete`。usage `complete` 还要求最终快照中 input/output 可用；否则是
`partial` 或 `unavailable`。供应商错误、取消、撤销、上下游中断分别保存安全结果，已观察到的部分
用量不会丢弃。客户端已有 `stream_options` 完整保留，不覆盖 `include_usage:false`，也不对未知
兼容供应商默认注入 `include_usage`。

耗时从登记成功后的实际发送点开始，以 Go 单调时钟计算。首有效输出忽略 keepalive、空 role、空
文本和 usage-only chunk，识别文本、拒绝、工具内容、Anthropic thinking，以及兼容响应中明确的
`reasoning/reasoning_content` 字符串（这些文本不用于推算 token）；非流式 JSON
只能记录完整响应到达后的可观察输出时刻，不是逐 token TTFT。该指标不包含 Agent 工具耗时，
也不能还原 compaction、权限等待、子 Agent 或 SDK 内部逻辑重试。

计数口径依据 [OpenAI Chat Completions](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)、
[OpenRouter 最终用量块](https://github.com/OpenRouterTeam/docs/blob/main/api_reference/streaming.mdx)、
[Anthropic streaming](https://platform.claude.com/docs/en/build-with-claude/streaming)、
[prompt caching](https://platform.claude.com/docs/en/build-with-claude/prompt-caching) 和
[thinking usage](https://platform.claude.com/docs/en/build-with-claude/thinking-steering-and-cost)。

生产 DNS 不回退系统解析器。HTTPS 解析器由公网数值 IP 引导并正常验证 TLS；模型域名的 A/AAAA
答案全部通过地址策略后才允许数值拨号，TLS 仍验证原模型域名。仅部署配置中精确指定的开发
fixture 主机使用系统 DNS；该例外不能由用户连接配置开启。`2001:2::/48` 和 `198.18.0.0/15` 均拒绝。

原始 Key 仅在本服务内存中存在，密文写入前随机产生 nonce；Node 的临时令牌只保存摘要。
备份必须同时包含数据库和加密主密钥卷。启动时不匹配已有密文就失败，不重置历史记录。

`*_test.go` 用生成的临时密钥、真实 TLS/HTTP 和故障策略替身验证加密、证书用途、流式边界、
阻塞取消、诊断过滤及地址策略。`transport_network_test.go` 的可选无凭据探测只证明可信解析与
真实 TLS 连接，HTTP 401 不能作为模型调用验收。真实 PostgreSQL 归属和授权验证见 Core 测试。详见
[model-gateway](../../docs/models/model-gateway.md) 与 [个人模型连接](../../docs/models/model-access.md)。
`attempt*_test.go` 与 `usage_test.go` 另外验证发送前登记、重复快照、未知/零、单调计时、取消后收尾、
失败不重发及原始转发字节；这些受控上游均为合成数据，不代表任何真实模型的计费结果。
