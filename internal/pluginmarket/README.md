# internal/pluginmarket: 插件市场同步

[中文](README.md) | [English](README.en.md)

`internal/pluginmarket` 把配置的插件市场 git 仓库同步进 cloud 的 PostgreSQL 目录快照。它只负责
**网络、git 与扫描**这半条链路:校验并镜像 desktop 的市场契约(orax.toml 布局、标识符命名空间、
release 三元组),然后经 `CatalogSink` 接口把结果交给 `internal/core` 落库。它不持有数据库句柄,
不产生 goroutine(`RunSyncLoop` 由进程生命周期驱动),也不修改 desktop 仓。

## 文件

- `manifest.go`：orax.toml 解析与校验,逐字段镜像 desktop `crates/plugin-manifest` 的
  resolver-1 规则(大小上限、8 种 kind、semver、slug 标识符、文本策略、sha256、URL/对象键、
  目标三元组白名单、pack 互斥)。
- `catalog.go`：扫描 `registry/**/orax.toml` 构建目录条目;`marketplace_visible=false` 与损坏
  清单跳过;同 canonical id 按路径序首个胜出;logo 变体(universal/light/dark + 扩展名优先级)
  与 README(截断存储)解析。
- `sync.go`：`Syncer.Sync` —— seed → go-git clone/pull --ff-only → 扫描 → 事务替换目录;
  单飞准入(并发调用方直接返回 `ErrSyncInFlight`);失败记录 `sync_error` 且保留上一版快照。
- `loop.go`：`RunSyncLoop` —— 启动即同步一次,此后每 interval 一次,ctx 取消退出,失败只记日志
  不中断循环;注入 `SleepFunc` 使 tick 可确定性测试。

## 依赖与调用方

- 依赖:`pelletier/go-toml/v2`(清单解析)、`go-git/go-git/v5`(纯 Go git 传输)、`zap`(日志)。
- 调用方:`cmd/server`(接线同步循环);`internal/core` 实现 `CatalogSink`(权威落库)。
- 不得依赖:任何 `internal/core` 类型(防止循环);本包是叶子包。

## 不变量

- 网络与扫描全部发生在数据库事务之外;落库只是 DELETE + INSERT + 时间戳的单事务替换。
- 一次失败的同步绝不覆盖上一版目录快照(失败路径只写 `plugin_sources.sync_error`)。
- 目录读取永远不出网:API 只读 PG 快照,与 desktop 的 cache-only 语义一致。
- `ErrNonFastForwardMerge` 即 `pull --ff-only` 契约;desktop 的 `gitlancer` 同语义。

## 测试

- 单测全部离线:go-git 本地裸仓 fixture 不发真实网络;`fakeSink` 断言事务边界行为;并发单飞
  用阻塞 seed 确定性同步;loop 用注入 sleep 驱动 tick。
- 真实 PG 约束与 HTTP 链路由 `integration/plugins_test.go` 覆盖。
