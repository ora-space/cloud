# pluginmarket

v2-server 的临时 marketplace 同步包。启动时从配置的 Git 仓库浅克隆或快进
`registry/**/orax.toml`，校验 resolver-1 清单、README 与 logo，随后在一个事务中替换
`v2_plugin_catalog` 快照。Agent 条目不进入目录，其版本由已安装的 Node 运行时管理；
扫描没有可见非 Agent 条目时拒绝替换上一版快照。读取 API 只访问 PostgreSQL，不在请求路径出网。

- `manifest.go`：清单字段、kind、semver、release、目标三元组和文本边界校验。
- `catalog.go`：扫描 registry、解析 logo 变体/README，返回排序后的目录条目。
- `sync.go`：Git 同步、原子更换 checkout、单次同步与定时刷新；每次同步有 2 分钟上限，失败时调用方保留上一版数据库快照。
- `download.go`：只连接解析并校验后的公网 IP，可按来源配置经 SOCKS5 代理下载；临时网络错误和 5xx/限流明确返回可重试错误，摘要或归档校验失败保持终态错误。

默认源和间隔由 `V2_PLUGIN_MARKETPLACE_URL`、`V2_PLUGIN_MARKETPLACE_BRANCH` 和
`V2_PLUGIN_MARKETPLACE_INTERVAL` 提供。配置入口只接受无凭据、无 query/fragment 的 HTTPS；
测试直接构造 Syncer 使用本地 `file://` Git 仓库。生产默认读取
`https://github.com/ora-space/marketplace`。
