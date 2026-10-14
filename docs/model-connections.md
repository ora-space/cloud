# 个人模型连接与运行授权

[中文](model-connections.md) | [English](model-connections.en.md)

`/api/v1/me/model-connections` 管理当前用户私有的连接和模型列表，`/api/v1/me/model-default`
选择默认模型。两者独立于租户：用户加入空间前也可以配置。更新使用当前 `version`，
创建和删除使用按用户隔离的 `Idempotency-Key`。模型 ID 是不透明字符串，包括 `/` 的 ID 原样保留。

支持 `openai-completions`、`anthropic-messages`，上游地址只能是公网 HTTPS；认证方式为
`bearer`，Anthropic 也可用 `x-api-key`。元数据接口不支持密钥或自定义认证头。读取只返回
`credentialConfigured`；密钥写入和清除由 Gateway 直接交给独立 model-gateway。Cloud HTTP 在读取
请求体前拒绝 credential 路由。Cloud 核心的专用服务方法仅接收密文，不接收、解密或返回原始 API Key。
凭据 PUT 写入和 DELETE 清除都要求用户作用域的 `Idempotency-Key`，并携带当前资源版本。

0033 追加迁移保存连接、用户默认值、不可变密文凭据、运行绑定与临时授权摘要，不改变旧运行。
密文凭据 ID 由 model-gateway 创建，替换当前凭据保留旧引用，避免改变已创建运行的配置。
有幂等键时，凭据 ID 将重试与同一密钥关联，Cloud 只比较不透明标识、版本和服务密钥 ID，
不对原始密钥或随机密文计算请求指纹。

创建 `official/ora-space.opencode` 运行的同一事务固定发起者、连接版本、协议、地址、认证方式、
模型与密文引用，也固定用户 Git 作者身份。缺默认模型、凭据或可用连接时，整个评论、交互和运行
事务回滚。其他 agent 的既有行为保持兼容。Controller 的 `AgentSessionSpec.model_binding_id`
只携带不透明引用，临时授权与上游密钥不进入控制消息。

会话可以同时固定个人模型绑定与前序 Revision；前序 Revision 使用已发布的通信字段 6，
模型绑定使用独立字段 7。Controller 分别检查模型代理与 Revision 恢复能力，不互相替代。
两个 `0033` 迁移按完整文件名和校验和独立追踪，保留已应用迁移的身份及 SQL。

model-gateway 将专用 mTLS 证书的租户、Workspace、运行代次交给 Core。Core 同时检查已登记的
会话执行、当前 Node、运行占用、运行与 Thread 状态、租户成员、用户和连接状态。授权只保存
SHA-256 摘要，有效期为 PostgreSQL 时钟的 15 分钟；续期延长同一授权，不换令牌。
每次转发及持续转发期间重新校验，停用账号、退出空间、清除密钥、停用/删除连接、结束会话或
强停运行都会拒绝后续请求；凭据清除和连接停用/删除也在同一事务持久撤销既有授权。

只有在证书作用域、执行、账号、成员、连接及当前 Node/运行占用全部校验通过后，结束中或取消
待处理的会话才返回 `403 model_session_ending`，让 Node 等待已持久化的 EndSession 命令并保留
用户结束原因。错误作用域、账号/凭据撤销仍返回一般拒绝，不能伪装为正常结束。模型访问使用
已经 clone 的 checkout，不重新要求远程 Git 凭据。

Thread 读取增加 `initiatorUserId`、可空 `model{connectionName,modelId,modelName}` 和
`canAppend`/`canEnd`。使用个人模型的运行仅发起者能追加请求；当前管理员也能结束运行。
其他有效成员仍可读取 Thread。旧运行与 Echo 不新增个人模型权限限制。

模型转发只使用 model-gateway 部署指定的 DNS-over-HTTPS（RFC 8484）。
`MODEL_GATEWAY_DNS_HTTPS_URL` 默认 `https://cloudflare-dns.com/dns-query`，
`MODEL_GATEWAY_DNS_BOOTSTRAP_IPS` 默认 `1.1.1.1,1.0.0.1`；bootstrap 必须是公网数字 IP。
部署可选择其他可信 HTTPS 解析器。模型用户不能选择解析器；解析失败时不回退系统 DNS。
解析器连接和模型连接均保留原域名进行 TLS 验证，模型 A/AAAA 全部地址检查后才向数字 IP 拨号。
`198.18.0.0/15`、`2001:2::/48` 等保留地址、混合公网/私网答案及重定向仍拒绝。
开发 fixture 只对明确配置的精确主机名使用 Docker DNS，不成为生产回退。

拒绝码区分策略拒绝、解析失败、TLS 验证失败、超时、重定向和上游不可达；日志只写安全分类，
不记录原始 URL、解析地址、请求体、认证头或供应商错误。ACP 失败终止 Node 会话并撤销授权；
已知的 `Authentication required` / `Not logged in` 拒绝归类为
`agent_authentication_required`，其他供应商原始文本不公开。
Thread GET 的 `failureCode` 只来自白名单。取消的 `TurnEnded` 不触发空闲；正常多轮完成仍进入 idle。
运行 GET/List 的 `preparation` 显示环境、clone、Agent 准备、启动阶段、持久 clone 次数和等待重试。
同一 Workspace 操作最多三次 clone，首次/第二次失败后等待 5/10 秒，第三次终止；未知结果继续阻塞。
运行 Workspace 失败后按已有路径释放，项目主 Workspace 保留且不开准入。数据库无需变更。

验证：`internal/core/model_connections_db_test.go` 用真实隔离 PostgreSQL 验证归属、资源版本、
幂等性、账号停用、缺配置原子回滚、冻结配置、运行代次、授权续期与撤销；契约和 HTTP 边界
测试验证严格模型结构及写入密钥接口的所有权。完整真实 OpenCode 验收由 cluster 的 M4 接线负责。
