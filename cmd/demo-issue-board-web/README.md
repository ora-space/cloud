# cmd/demo-issue-board-web：浏览器看板演示

[中文](README.md) | [English](README.en.md)

仅供开发的单文件看板入口；嵌入本目录的 `index.html`，并在同一 origin 提供真实 Cloud HTTP API。

## 装配与生命周期

- `DEMO_DATABASE_URL` 指定专用 PostgreSQL；默认连接本地 `127.0.0.1:55432` 的 `ora` 数据库（参数见 `main.go`）。
- 每次创建独立 `demo_issues_<UUID>` schema、执行迁移并引导 Alice 的租户。正常退出时删除 schema。
- `/demo/config` 从 Store 读取租户、用户、状态和标签；其他非根路径交给 `router.New`，服务端为演示身份签发双 JWT。
- `DEMO_WEB_ADDR` 默认 `127.0.0.1:8899`；绑定失败时退到随机 loopback 端口。`DEMO_WEB_OPEN` 为空或 `1` 时尝试通过 Windows `cmd /c start` 打开浏览器。
- Ctrl+C 后最多等待 3 秒关闭 HTTP 服务。

从仓库根目录运行 `go run ./cmd/demo-issue-board-web`。数据库账号须有 schema 创建/删除权限。

## 边界

请求统一使用演示用户，没有真实登录、成员身份验证或生产会话机制，只应在受控本地环境运行。与保留数据的 [ora-web](../ora-web/README.md) 不同，每次正常退出均清除演示数据。业务说明见 [Issue 看板](../../docs/collaboration/issues.md)。
