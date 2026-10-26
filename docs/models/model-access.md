# 个人模型连接、运行绑定与临时授权

[English](model-access.en.md) | 中文

用户可以配置自己的模型服务（OpenAI 兼容或 Anthropic 接口）和 API Key，让自己触发的 Agent 运行使用这个模型。Key 由平台保管，
每次运行通过 [model-gateway](model-gateway.md) 代理模型请求：Agent 所在的沙盒从头到尾拿不到原始 Key，只拿到一个只对这次运行有效的
临时令牌。

本文说明 Cloud 一侧的四件事：连接与默认模型、密钥的保管、运行时固定的绑定，以及模型请求的临时授权。

声明式 Agent 运行默认关闭；部署完成后以 `plugins.personal_agent_target` 指定精确 Linux target。Cloud 冻结包、声明、adapter 版本、配置 revision、模型元数据和数值原文；Controller 与 Node 必须证明同一 target、版本及协议才可开始。Settings 以经过校验的 JSON 文本跨控制边界，避免大整数和高精度小数被中间层舍入；运行 token 只在 mTLS grant 与插件启动内存通道出现。当前 Node 不支持时按能力缺失结算，未连接或过期的 Node 不构成确定性拒绝。

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

企业模式下，创建和修改连接（不含 Key 的写入路径）还要过[部署出网准入](../operations/deployment-egress.md#更新撤销与健康信息)：短事务里先检查身份、归属、版本和已完成的幂等记录，事务外做 DNS 与地址准入，写事务再核对当前权威状态。DNS 拒绝返回 `400 invalid_model_url`，不写新版本。已完成的合法重放、纯停用、删除和清 Key 不做这次准入。准入通过也不让之后的转发跳过当次出网检查，DNS 与外部 HTTP 都不放进持锁事务。

## 密钥的保管

原始 API Key 只进入 model-gateway：

- 浏览器经 [Gateway](../identity/gateway.md#代理) 写入或清除 Key 时，Gateway 只把这两条路径转发到固定的 HTTPS model-gateway；Cloud 的 HTTP
  路由在读取请求体之前就拒绝这两条路径（`404 credential_gateway_required`），即使有人绕过 Gateway 直接请求 Cloud，明文也进不了 Cloud 进程。
- model-gateway 用 AES-256-GCM 加密 Key，每次写入随机生成 nonce，把**不可变的密文记录**交给 Cloud 保存。Cloud core 只接收密文，从不接收、
  解密或返回原始 Key。
- 替换 Key 会生成新的密文记录并保留旧的引用，已经创建的运行不受影响。带幂等键的重试会关联到同一份密文；Cloud 只比较不透明标识、版本和
  服务密钥 ID，不对原始 Key 或随机密文计算请求指纹。
- 写入和清除都要求用户作用域的 `Idempotency-Key` 和当前资源版本。清除 Key、停用或删除连接，都在同一个事务里撤销这个连接已签发的全部临时授权。

## 个人 Agent 配置

`/api/v1/me/agent-configurations/:spaceId/:pluginNamespace/:pluginIdentifier` 读取、保存和清除当前用户的插件配置；`POST .../repair` 显式确认变更后的声明。空间只提供选定发行的上下文，配置按用户与 canonical Plugin ID 单份保存，管理员没有他人的读写权。

读取同时返回当前声明和原配置，指纹不匹配时保留旧值并标记需修复。保存检查配置 revision、空间选择版本和声明指纹；修复必须按当前声明完整提交。所有写操作要求用户作用域的幂等键，同一幂等键的重放返回首次保存的响应，Settings 的数值字面量原样返回。清除保留递增 revision 的空记录，不删除连接，也不撤销旧运行。OpenCode 配置仅保存选项，其模型选择由原全局默认资源按自身 version 修改。

Settings 只接受声明中的标量字段；普通配置请求不承载连接凭据。连接和模型引用必须属于当前用户且协议与声明匹配，Claude 只接受 Messages。保存配置不证明模型可连接，也不证明部署已有 adapter。

声明式 Agent 的任务就绪提示与最终运行准入使用同一组公开缺口码：连接停用、删除或不可读取为 `model_connection_unavailable`，缺 Key 为
`model_credential_required`，所选模型已撤下为 `model_not_found`，未配置模型选择为 `agent_configuration_required`。页面将连接、凭据和模型缺口引导到个人模型连接，将未配置的插件模型选择引导到插件“我的配置”；领域内部的工作流缺口原因不直接作为这些公开错误返回。

## 运行绑定

`official/ora-space.opencode` 的 Agent 运行在**创建运行的同一个事务里**生成一份运行绑定（`run_model_bindings`），固定：发起者、连接及其版本、
协议、上游地址、认证方式、模型和密文引用；同时固定发起者的 Git 作者身份。

- 发起者没有默认模型、没有配置 Key 或连接不可用时，整条评论、交互和运行一起回滚，分别返回 `409 model_default_required`、
  `model_credential_required`、`model_connection_unavailable`。无新声明的旧 OpenCode 保持这一行为。
- 之后修改连接或默认模型，不会改变已经创建的运行。
- 一次运行可以有多份绑定：工作流运行在创建事务里为图引用的**每个槽位**各冻结一份，每个节点的会话只取用它引用的那个槽位的绑定（见
  [工作流](../workflows/workflows.md#执行器槽位)）。
- 会话执行的输入只带一个不透明的 `modelBindingId`；临时令牌和上游 Key 都不进入执行输入或 Controller 的控制消息。
- 登记会话派发时，Cloud 检查当前 Node 握手时声明了模型代理能力，没有这项能力的旧 Node 不能执行使用个人模型的会话
  （`409 model_proxy_required`）；签发模型授权时同样要求这项能力。启动期间确认 Node 缺少能力时的结算见
  [有界启动](../agent-runs/startup-settlement.md)。

使用个人模型的运行，Thread 只能由发起者追加，发起者或当前管理员可以结束，其他成员仍可读取（见 [Thread](../agent-runs/threads.md)）。

带 Agent 声明的运行还校验个人配置、声明指纹及部署提供的精确 target/adapter 契约能力，并在同一事务写入私有 `run_agent_configurations`。没有该能力时返回 `agent_adapter_unavailable`；Claude 没有声明时返回 `agent_declaration_required`。部署组装必须在实际 Node/Controller adapter 路径可消费冻结输入后才提供能力。

私有快照保存包身份、声明身份、配置 revision 和解析后的选项；公开运行详情只显示插件、版本与配置 revision。延迟创建 Workspace 的安装计划继续取原冻结发行，空间升级或卸载不改变这次执行。后续普通配置编辑不改写快照，连接撤销仍由原运行绑定统一管理。

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

- [个人 Agent 配置按插件解析并在创建运行时冻结](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261017-personal-agent-configurations.md)（部分实现，adapter 与端到端义务仍由该 ADR 约束）

- [个人模型连接由用户配置，平台保管密钥并为每次运行代理模型请求](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/0-personal-model-connections-and-run-scoped-proxy.md)
- [个人模型 Thread 只由发起者追加，成员可读且管理员可结束](https://github.com/ora-space/specs/blob/main/decisions/cloud/thread/20261010-personal-model-initiator-controls-input.md)
- [沙盒注入专用模型客户端证书，个人模型 Key 保留在平台代理](https://github.com/ora-space/specs/blob/main/decisions/cluster/sandbox-server/20261010-model-client-identity-replaces-shared-model-key-injection.md)
- [个人槽位映射为工作流解析执行器，每次运行按槽位锁定多组模型绑定](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261015-executor-slot-mappings-and-per-slot-run-bindings.md)（approved）
- [模型转发与 Cloud 资产下载共用部署方出网白名单和显式解析](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261017-deployment-owned-egress-and-explicit-resolution.md)（approved）
