# 部署方共享出网配置

Cloud 的个人模型连接保存门禁、工作流资产 HTTPS 下载和 model-gateway 转发读取同一份包外
`deployment-egress.json`。两个进程使用相同的 `-deployment-egress /absolute/path/deployment-egress.json`
参数，或相同的 `ORA_DEPLOYMENT_EGRESS_FILE` 环境变量；参数优先。文件只在启动时读取，必须只读挂载，
不可由用户、插件或个人连接 API 修改。不配置路径时保持原公网 HTTPS 模式。

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

示例是配置形状，文档地址和域名不能直接作为现场参数。`topology` 应由部署入口采集 Docker 的默认桥、
Compose 网络、动态 Node/Workspace 网络池以及宿主接口/网桥/网关的双栈地址，再合并管理员补充项。
Cloud 只消费该结果，不访问 Docker socket；不能拿一个示例网段代替实际拓扑。采集、共同挂载、双服务
停止/启动与 `ora verify` 的协调由 cluster 部署入口负责。

- `targets` 是共同的精确 host:port 集合，两类调用方不能分开设置。大小写、一个末尾根点、默认 443
  与显式 443 等价；其他端口、后缀、别名和数字 IP URL 均不获得许可。模型 baseUrl 禁止 query；资产
  URL 保留原路径及合法签名 query。许可不携带个人模型 Key，也不替代资产安装权限或摘要验证。
- 未列出的公网和私网目标均在 DNS 前拒绝。`allowPrivate` 默认 false，仅允许 RFC1918/ULA 例外；
  `allowedCidrs` 可省略，但填写时不能是空数组，且不能借另一目标的 CIDR 获准。
- cluster 网段、宿主地址、回环、链路本地以及其他既有保留地址始终拒绝，优先于白名单与私网许可。
  指向基础设施的静态地址使配置加载失败。拓扑两张表必须存在且非空；管理员无需证明所有 NAT/VIP
  已穷尽，但已知别名应补充，网络变化后应重新采集并重启。
- `resolver` 只接受显式数字 IP:port 的 DNS-over-TCP。查询 A/AAAA 共用 5 秒预算，每族最多 8 次
  CNAME 跳转；每条 TCP DNS 消息最多 65535 字节、256 条资源记录。任何一族出错或任一答案越界即拒绝；
  一族明确无数据而另一族有效可以通过。authority/additional 记录不作为连接地址。
- 非空 `staticAddresses` 完全替代该目标动态解析，失败不会回退。全部目标静态时可以省略 resolver。
  企业解析不读取系统 DNS、搜索域或默认 Docker DNS，不回退公网 DoH。resolver 控制通路的许可不允许
  将该地址当作模型或资产 HTTP 目标。
- 单文件上限 1 MiB；未知版本/字段、重复键、null、畸形地址、重复目标、空 CIDR/静态数组或缺少必需
  拓扑均使启动失败。企业模式与开发 allow-hosts 冲突时拒绝启动。

## 公网模式与解析一致性

显式公网文件为 `{"schemaVersion":1,"mode":"public"}`，不接受 enterprise 专用字段。
Cloud 和 model-gateway 都读取 `MODEL_GATEWAY_DNS_HTTPS_URL` 和 `MODEL_GATEWAY_DNS_BOOTSTRAP_IPS`
（逗号分隔的公网数字地址）。保留这些既有变量名以兼容部署；务必将相同值交给两个进程。未设置时
两者采用原 Cloudflare DoH 默认值，资产下载器不再另行选择 DNS。企业模式忽略公网 DNS 配置。

所有新业务连接都重新解析并验证全部地址，再拨数字地址并按规范化后的原域名校验 TLS。禁用环境
HTTP/SOCKS 代理和重定向。企业模式业务传输使用 HTTP/1.1，避免跨 host 的 HTTP/2 连接合并；最大连接寿命
5 分钟、空闲超时 60 秒，寿命到期也会中断活跃长请求，不自动重发模型生成。资产仍保留 10 秒连接、
30 秒响应头、60 秒整包及原摘要/大小边界。

## 更新、撤销与健康信息

更换配置时先停止 Cloud 和全部 model-gateway，确认旧进程退出，再替换共同文件并启动。仅编辑文件
不改变运行快照；加载失败不会自动恢复旧文件或公网模式。回滚也应显式选择旧配置并执行同样流程。
服务退出终止旧模型流/资产请求和连接；不能只关闭空闲连接就声称新策略已生效。

两个服务的 `GET /healthz` 响应头提供 `X-Ora-Egress-Schema: 1` 与 `X-Ora-Egress-Digest`（SHA-256）。
摘要涵盖规范化策略，公网模式另包含实际 DoH 配置，不包含 Key。部署入口应同时检查健康状态及这两项，
缺失或不一致必须报错；摘要一致不替代每次实际出网的独立校验。

企业保存模型连接会在事务外解析，提交时重查身份/归属/资源版本/幂等。DNS 拒绝不写新连接版本。
已完成的合法幂等重放无需重新解析。纯停用必须原样携带其他配置；它与删除、清 Key 都不依赖出网
许可，继续永久撤销旧绑定。重新允许目标或启用连接不会复活已撤销绑定。

固定内部对象存储的 object-key 取包继续走部署客户端，不可作为被拒 HTTPS 的回退。此策略不覆盖
Node 插件安装、marketplace Git 同步或身份认证。真实现场连通性、供应商工具/流式能力和 cluster
协调更新仍需对应部署验收。

部署工具可先运行 `/app/model-gateway -check-egress -deployment-egress <file>`：该入口使用实际服务的
解析器和公网 DNS 环境配置，仅输出 `schemaVersion` 与 `digest` JSON，不查询 DNS、不连接数据库、
不加载凭据、不监听端口。无效配置以非零状态退出，可在停服务前预检；应用后再与两端 healthz 比较。
