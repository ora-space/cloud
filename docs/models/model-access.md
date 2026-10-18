# 个人模型连接、运行绑定与临时授权

[English](model-access.en.md) | 中文

用户可以配置自己的模型服务（OpenAI 兼容或 Anthropic 接口）和 API Key，让自己触发的 Agent 运行使用这个模型。Key 由平台保管，
每次运行通过 [model-gateway](model-gateway.md) 代理模型请求：Agent 所在的沙盒从头到尾拿不到原始 Key，只拿到一个只对这次运行有效的
临时令牌。

本文说明 Cloud 一侧的四件事：连接与默认模型、密钥的保管、运行时固定的绑定，以及模型请求的临时授权。

## 连接与默认模型

个人模型连接属于**用户本人**，与租户无关：用户加入任何空间之前就可以配置。

| 接口 | 作用 |
|---|---|
| `GET`、`POST /api/v1/me/model-connections` | 列出、创建连接 |
| `GET`、`PUT`、`DELETE /api/v1/me/model-connections/:mcid` | 读取、修改、删除连接 |
| `PUT`、`DELETE .../model-connections/:mcid/credential` | 写入、清除 API Key，**只**由 model-gateway 处理（见下文） |
| `GET`、`PUT /api/v1/me/model-default` | 读取、设置默认模型 |

- **连接**包含名称、协议、上游地址、认证方式、模型列表和是否启用。协议为 `openai-completions` 或 `anthropic-messages`；上游地址只能是
  `https`，不带用户信息、查询或片段；认证方式为 `bearer`，Anthropic 协议还可以用 `x-api-key`。连接元数据里不支持自定义认证头或携带密钥。
- **模型列表**每项有 `id`、`name`、`contextWindow`、`maxTokens`。模型 ID 是不透明字符串，包括含 `/` 的 ID 都原样保留。
- 修改使用当前 `version`，创建和删除使用按用户隔离的 `Idempotency-Key`。读取连接只返回 `credentialConfigured`，不返回 Key。
- **默认模型**指向某个连接里的某个模型，是用户触发 OpenCode 运行时使用的模型。

## 密钥的保管

原始 API Key 只进入 model-gateway：

- 浏览器经 [Gateway](../identity/gateway.md#代理) 写入或清除 Key 时，Gateway 只把这两条路径转发到固定的 HTTPS model-gateway；Cloud 的 HTTP
  路由在读取请求体之前就拒绝这两条路径（`404 credential_gateway_required`），即使有人绕过 Gateway 直接请求 Cloud，明文也进不了 Cloud 进程。
- model-gateway 用 AES-256-GCM 加密 Key，每次写入随机生成 nonce，把**不可变的密文记录**交给 Cloud 保存。Cloud core 只接收密文，从不接收、
  解密或返回原始 Key。
- 替换 Key 会生成新的密文记录并保留旧的引用，已经创建的运行不受影响。带幂等键的重试会关联到同一份密文；Cloud 只比较不透明标识、版本和
  服务密钥 ID，不对原始 Key 或随机密文计算请求指纹。
- 写入和清除都要求用户作用域的 `Idempotency-Key` 和当前资源版本。清除 Key、停用或删除连接，都在同一个事务里撤销这个连接已签发的全部临时授权。

## 运行绑定

`official/ora-space.opencode` 的 Agent 运行在**创建运行的同一个事务里**生成一份运行绑定（`run_model_bindings`），固定：发起者、连接及其版本、
协议、上游地址、认证方式、模型和密文引用；同时固定发起者的 Git 作者身份。

- 发起者没有默认模型、没有配置 Key 或连接不可用时，整条评论、交互和运行一起回滚，分别返回 `409 model_default_required`、
  `model_credential_required`、`model_connection_unavailable`。其他 Agent 的行为不受影响。
- 之后修改连接或默认模型，不会改变已经创建的运行。
- 会话执行的输入只带一个不透明的 `modelBindingId`；临时令牌和上游 Key 都不进入执行输入或 Controller 的控制消息。
- 登记会话派发时，Cloud 检查当前 Node 握手时声明了模型代理能力，没有这项能力的旧 Node 不能执行使用个人模型的会话
  （`409 model_proxy_required`）；签发模型授权时同样要求这项能力。启动期间确认 Node 缺少能力时的结算见
  [有界启动](../agent-runs/startup-settlement.md)。

使用个人模型的运行，Thread 只能由发起者追加，发起者或当前管理员可以结束，其他成员仍可读取（见 [Thread](../agent-runs/threads.md)）。

## 临时授权

沙盒里的 Node 用注入的专用模型客户端证书（mTLS）向 model-gateway 申请临时授权，证书携带租户、Workspace 和运行代次。Cloud core 在签发、
续期和每一次转发时都重新核验：

- 已登记的会话执行属于这个运行，当前 Node 和运行时占用都对得上；
- 运行与 Thread 的状态仍允许模型调用；
- 发起者仍是当前成员、账号启用，连接仍启用、Key 仍在。

授权只在数据库里保存令牌的 SHA-256 摘要，有效期为数据库时钟的 15 分钟；续期延长同一个授权，不换令牌。停用账号、退出空间、清除 Key、停用或删除
连接、结束会话、强停运行，都会让之后的请求被拒绝；长时间的流式转发期间也会持续复核，撤销会同时打断上游请求和阻塞的下游写入。

结束中、或取消正在等待处理的会话，在其他校验全部通过后返回 `403 model_session_ending`：Node 据此等待已经持久化的 `EndSession` 命令，并保留
用户给出的结束原因。作用域错误、账号或 Key 被撤销仍返回一般的拒绝，不会伪装成正常结束。模型访问使用已经 clone 好的 checkout，不重新要求远程
Git 凭据。

## 相关决策

- [个人模型连接由用户配置，平台保管密钥并为每次运行代理模型请求](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/0-personal-model-connections-and-run-scoped-proxy.md)
- [个人模型 Thread 只由发起者追加，成员可读且管理员可结束](https://github.com/ora-space/specs/blob/main/decisions/cloud/thread/20261010-personal-model-initiator-controls-input.md)
- [沙盒注入专用模型客户端证书，个人模型 Key 保留在平台代理](https://github.com/ora-space/specs/blob/main/decisions/cluster/sandbox-server/20261010-model-client-identity-replaces-shared-model-key-injection.md)
- [个人槽位映射为工作流解析执行器，每次运行按槽位锁定多组模型绑定](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261015-executor-slot-mappings-and-per-slot-run-bindings.md)（approved）
