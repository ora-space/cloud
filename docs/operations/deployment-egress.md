# 部署方共享出网配置

[English](deployment-egress.en.md) | 中文

Cloud 的个人模型连接保存门禁、工作流资产 HTTPS 下载、model-gateway 转发和 Repo Executor 的外部 Git 访问读取同一份包外
`deployment-egress.json`。Cloud server 与 model-gateway 使用相同的
`-deployment-egress /absolute/path/deployment-egress.json` 参数，或相同的 `ORA_DEPLOYMENT_EGRESS_FILE`
环境变量；参数优先。Repo Executor 只读环境变量：`REPO_EXECUTOR_DEPLOYMENT_EGRESS_FILE` 优先，否则用
`ORA_DEPLOYMENT_EGRESS_FILE`，路径必须是绝对路径。文件只在启动时读取，必须只读挂载，不可由用户、插件或个人连接 API 修改。不配置路径时保持公网模式。
策略形状与拒绝规则见
[模型转发与 Cloud 资产下载共用部署方出网白名单和显式解析](https://github.com/ora-space/specs/blob/main/decisions/cloud/model-access/20261017-deployment-owned-egress-and-explicit-resolution.md)（approved）。

## 企业模式

```json
{
  "schemaVersion": 1,
  "mode": "enterprise",
  "resolver": {"transport": "tcp", "address": "192.0.2.53:53"},
  "targets": [
    {"host": "model.example.invalid", "port": 443, "allowPrivate": true, "allowedCidrs": ["10.60.0.0/16"]},
    {"host": "assets.example.invalid", "port": 443, "allowPrivate": true, "staticAddresses": ["10.61.0.10"]}
  ],
  "topology": {
    "clusterCidrs": ["172.30.0.0/16"],
    "hostAddresses": ["192.0.2.10"]
  }
}
```

示例只说明配置形状，其中的地址和域名不能当作现场参数。`topology` 由部署入口采集后写入文件：Docker 默认桥、Compose 网络、动态
Node/Workspace 网络池，以及宿主接口、网桥和网关的双栈地址，再合并管理员补充项。Cloud 只消费该文件，不访问 Docker socket。

- `targets` 是模型转发和资产下载共同的精确 host:port 集合，两类调用方不能分开设置。大小写、一个末尾根点、默认 443 与显式 443
  等价；其他端口、后缀、别名和数字 IP URL 均不获得许可。模型 baseUrl 禁止 query；资产 URL 保留原路径及合法签名 query。许可不携带个人模型
  Key，也不替代资产安装权限或摘要验证。
- 未列出的公网和私网目标均在 DNS 前拒绝。`allowPrivate` 默认 false，仅允许 RFC1918/ULA 例外；`allowedCidrs` 可省略，但填写时不能是空数组，
  且不能借另一目标的 CIDR 获准。
- cluster 网段、宿主地址、回环、链路本地以及其他既有保留地址始终拒绝，优先于白名单与私网许可。指向这些地址的静态地址使配置加载失败。
  `topology` 的两张表必须存在且非空。
- `resolver` 只接受显式数字 IP:port 的 DNS-over-TCP。查询 A/AAAA 共用 5 秒预算，每族最多 8 次 CNAME 跳转；每条 TCP DNS 消息最多 65535
  字节、256 条资源记录。任何一族出错或任一答案越界即拒绝；一族明确无数据而另一族有效可以通过。authority/additional 记录不作为连接地址。
- 非空 `staticAddresses` 完全替代该目标的动态解析，失败不会回退。全部目标都是静态地址时可以省略 resolver。企业解析不读取系统 DNS、搜索域或
  默认 Docker DNS，也不回退公网 DoH。resolver 自己的地址不能当作模型或资产 HTTP 目标。
- 单文件上限 1 MiB。未知版本或字段、重复键、null、畸形地址、重复目标、空 CIDR 或静态数组、缺少必需拓扑，都使启动失败。企业模式与
  `MODEL_GATEWAY_DEVELOPMENT_ALLOW_HOSTS` 同时设置时拒绝启动。

## 公网模式与解析一致性

显式公网文件为 `{"schemaVersion":1,"mode":"public"}`，不接受 enterprise 专用字段。Cloud、model-gateway 和 Repo Executor 都读取
`MODEL_GATEWAY_DNS_HTTPS_URL` 和 `MODEL_GATEWAY_DNS_BOOTSTRAP_IPS`（逗号分隔的公网数字地址）。三个进程必须拿到相同的值。未设置时三者采用
Cloudflare DoH 默认值 `https://cloudflare-dns.com/dns-query` 与 `1.1.1.1,1.0.0.1`。企业模式忽略这两项。

所有新业务连接都重新解析并验证全部地址，再拨数字地址，并按规范化后的原域名校验 TLS。禁用环境 HTTP/SOCKS 代理和重定向。企业模式的业务传输不尝试
HTTP/2。仅企业模式设置 30 分钟连接存活上限，到期会关闭活跃连接，不自动重发模型生成；公网模式不设硬性存活上限，保持原行为。两种模式的空闲超时均为 60 秒。

受检地址仍按 DNS 返回顺序在同一个拨号总预算内逐个拨号。每次拨号拿到「剩余总预算 ÷ 剩余地址数」，约 2 秒下限由总 deadline 收口，一个不响应的边缘地址不会耗尽留给后续地址的时间。全部地址失败时，有界错误和结构化日志的 detail 按数字地址列出各自原因（timeout、connection refused、network unreachable 或未尝试），不包含 URL、query、认证头或凭据。

## 更新、撤销与健康信息

启动配置错误指出字段路径及原因，例如 `targets[1].allowedCidrs: empty`，不输出字段值、地址或配置原文；未知字段名也不回显，避免把任意输入当作诊断内容。

更换配置时先停止 Cloud server、全部 model-gateway 和全部 Repo Executor，确认旧进程退出，再替换共同文件并启动。仅编辑文件不改变已经加载的快照；加载失败不会自动恢复旧文件或切回公网模式，Repo Executor 加载失败会拒绝启动。回滚也是显式选择旧文件并执行同样的停启流程。进程退出会终止进行中的模型流、资产请求和外部 Git 任务。

Cloud server 的 `GET /healthz`、model-gateway **凭据监听**上的 `GET /healthz`，以及 Repo Executor **健康监听**（`REPO_EXECUTOR_HEALTH_ADDR`，默认 `:8084`，明文，只在部署网络内）上的 `GET /healthz`，响应头为 `X-Ora-Egress-Schema: 1` 与
`X-Ora-Egress-Digest`（SHA-256）。摘要涵盖规范化策略；公网模式另包含实际 DoH 配置，不包含 Key，也不返回策略正文。`model-gateway -healthcheck`
不报告这两项。部署入口应同时检查这些健康状态及摘要是否一致；摘要一致不替代每次实际出网的独立校验。

企业模式下，创建或修改个人模型连接会在短事务中检查身份、归属、版本和已完成的幂等记录，事务外完成 DNS 与地址准入，再在写事务中重新检查当前权威状态。
DNS 拒绝返回 `400 invalid_model_url`，不写新连接版本。已完成的合法幂等重放、纯停用、删除和清 Key 不依赖这次准入。保存通过也不让之后的转发跳过当次出网检查。

固定内部对象存储的 object-key 取包继续走部署客户端，不能当作被拒 HTTPS 的回退。此策略不覆盖 Node 插件安装、marketplace Git 同步或身份认证。

部署工具可先运行 `/app/model-gateway -check-egress -deployment-egress <file>` 或
`/app/repo-executor -check-egress -deployment-egress <file>`。该入口使用实际服务的解析器和公网 DNS 环境配置，只向标准输出打印
`schemaVersion` 与 `digest` 的 JSON，不查询 DNS、不连接数据库、不加载凭据、不监听端口。无效配置以非零状态退出。应用后再与各 healthz 的摘要比较。
