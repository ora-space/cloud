# cmd/demo-issue-board：命令行 Issue 看板演示

[中文](README.md) | [English](README.en.md)

仅供开发使用的单次演示。通过真实 HTTP、PostgreSQL 和 `router.New` 创建、移动并列出 Issue，最后从新 SQL 查询读取行数，输出看板与持久化摘要。

## 运行与数据

从仓库根目录运行 `go run ./cmd/demo-issue-board`；可用 `DEMO_DATABASE_URL` 指定专用 PostgreSQL。省略时使用本地 `127.0.0.1:55432` 的 `ora` 数据库（默认连接参数见 `main.go`）。账号需要创建和删除 schema 的权限。

每次新建 `demo_issues_<UUID>` schema，应用内嵌迁移，引导演示租户 Alice，再用内存生成的双 JWT 调用 API。正常返回时通过 defer 删除 schema；调用失败直接 `os.Exit` 的分支不会执行清理，需人工检查遗留 schema。

## 边界

没有外部登录或生产 Gateway；演示密钥不落盘。本命令不改变生产 server 的启动迁移规则。自动化行为覆盖见 [Issue 测试](../../integration/issues_test.go)，领域语义见 [Issue 看板](../../docs/collaboration/issues.md)。
