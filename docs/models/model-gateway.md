# model-gateway：个人密钥与模型代理服务

[English](model-gateway.en.md) | 中文

model-gateway（`cmd/model-gateway`，实现在 `internal/modelgateway`）是独立于 Cloud server 运行的可信服务，独占两件事：个人上游 API Key
的加解密，以及 Agent 运行的模型请求转发。业务归属、配置冻结、运行权限和授权摘要仍由 Cloud core 与 PostgreSQL 决定（见
[个人模型连接](model-access.md)），model-gateway 每一步都向 core 求证；Controller 和 Node 只拿到绑定引用和临时令牌。

## 三个监听

| 监听 | 默认地址 | 谁来调用 | 认证 |
|---|---|---|---|
| 凭据监听 | `:8083` | 只有浏览器 Gateway | Gateway 现签的 service JWT 与 user JWT；只处理 Key 的写入和清除 |
| 授权监听 | `:8444` | 沙盒里的 Node | TLS 双向认证，客户端证书的 URI 必须是 `spiffe://ora.local/model-access/tenant/<tenantId>/runtime/<workspaceId>/generation/<n>` |
| 模型数据监听 | `:8443` | 沙盒里的 Agent | 临时令牌；只转发 `/runtime/openai/v1/chat/completions` 与 `/runtime/anthropic/v1/messages` |

三个入口不可互换：请求体或请求头里的运行时标识不能替代授权监听的证书，临时令牌也不能用在凭据监听上。任何响应和错误里都不出现真实 Key。

## 启动与停止

1. 读取 Cloud 的配置文件（数据库与 Gateway 的验证公钥），以及本服务的部署环境变量。
2. 校验迁移；加载加密主密钥，并用它核对数据库里已有的密文：主密钥与已有密文的加密身份不匹配时**启动失败**，不会重置历史记录。
3. 建立受检的出网客户端，开放三个 TLS 监听。
4. 停止时先取消进行中的模型流，再等待全部监听退出。

| 环境变量 | 默认值 / 用途 |
|---|---|
| `MODEL_GATEWAY_CREDENTIAL_ADDR`、`_RUNTIME_ADDR`、`_GRANT_ADDR` | `:8083`、`:8443`、`:8444` |
| `MODEL_GATEWAY_PUBLIC_ORIGIN` | `https://ora-model-gateway:8443`，授权响应里返回给 Agent 的代理地址（`proxyBaseUrl`） |
| `MODEL_GATEWAY_CERTIFICATE_FILE`、`_PRIVATE_KEY_FILE`、`_CA_FILE` | 服务证书、私钥，以及校验 Node 客户端证书的 CA |
| `MODEL_GATEWAY_MASTER_KEY_FILE`、`_MASTER_KEY_ID` | 加密主密钥文件与它的 ID（默认 `v1`） |
| `MODEL_GATEWAY_DNS_HTTPS_URL`、`_DNS_BOOTSTRAP_IPS` | 出网解析器，默认 `https://cloudflare-dns.com/dns-query` 与 `1.1.1.1,1.0.0.1` |
| `MODEL_GATEWAY_DEVELOPMENT`、`_DEVELOPMENT_ALLOW_HOSTS` | 仅开发验收：两项同时设置时，精确列出的主机可以使用系统 DNS 和私网地址 |

`-healthcheck` 正常校验 HTTPS 和公开 CA，只检查数据库就绪，不读取用户 Key 或主密钥。**备份必须同时包含数据库和主密钥卷**，少了任何一个
都无法恢复已保存的 Key。

## 出网策略

模型转发连接的是用户填写的地址，所以每一次出网都要防止被引向内网：

- 只使用部署配置的、经过 TLS 认证的 DNS-over-HTTPS（RFC 8484）解析器，解析器由公网数字 IP 引导；解析失败时**不回退**系统 DNS。用户不能选择
  解析器。
- 模型域名的全部 A/AAAA 答案都通过地址策略之后，才向数字 IP 拨号；TLS 仍校验原始域名。私网、链路本地、文档、地址转换和特殊用途地址段
  （包括 `198.18.0.0/15`、`2001:2::/48`）一律拒绝，公网与私网混合的答案同样拒绝。
- 不使用环境变量里的代理，不跟随重定向。
- 拒绝码区分策略拒绝、解析失败、TLS 校验失败、超时、重定向和上游不可达；日志只记录安全分类，不记录原始 URL、解析地址、请求体、认证头或
  供应商的错误文本。

Cloud server 下载工作流插件资产时使用同一个受检客户端和同一套地址策略（见 [工作流](../workflows/workflows.md#插件资产导入)）。

## 转发

- 只转发授权里固定的模型和固定的路径，并校验协议的请求封装；成功的 JSON 和 SSE 字节原样转发，SSE 逐个事件刷新。
- HTTP 200 里夹带的错误事件也归一为安全错误；ACP 的认证失败（`Authentication required`、`Not logged in`）归类为
  `agent_authentication_required`，其他供应商的原始文本不公开。
- 每一次转发和长时间的流式转发期间都重新核验授权（见 [临时授权](model-access.md#临时授权)），撤销会同时打断上游请求和阻塞的下游写入。
  每个授权观察任务归属于一个请求，请求取消后等待它退出。
- 网关**不重试**模型调用。每一次上游 HTTP 尝试在发送之前登记，结束后写入状态、耗时和用量；登记失败时直接返回
  `model_metrics_unavailable`，不请求上游。完成记录使用独立的 3 秒上下文，最多提交三次同一份不可变结果，仍失败就保留 `started`。统计的
  口径见 [模型请求统计](../agent-runs/session-reads.md#模型请求统计)。

## 相关决策

- [个人模型连接由用户配置，平台保管密钥并为每次运行代理模型请求](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/0-personal-model-connections-and-run-scoped-proxy.md)
- [模型用量按网关 HTTP 尝试独立登记，保留部分观测与未知值](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261016-recorded-http-attempt-statistics.md)（approved）
- [模型转发与 Cloud 资产下载共用部署方出网白名单和显式解析](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261017-deployment-owned-egress-and-explicit-resolution.md)（approved）
