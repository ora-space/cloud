# internal/logger: 结构化日志子系统

[中文](README.md) | [English](README.en.md)

`internal/logger` 为 Ora Cloud 提供全进程范围的结构化日志记录能力，底层封装了 Uber Zap 与 Lumberjack。

## 职责

- **双输出端组合**：
  - **控制台输出端**：向 `stdout` 输出带颜色、采用 ISO8601 时间格式的人类可读日志行，用于本地开发和容器控制台。
  - **文件输出端**：输出机器可读的 JSON 日志事件，并由 Lumberjack 管理日志轮转（可配置 `max_size`、`max_backups`、`max_age` 和 gzip 压缩）。
- **标准化事件字段**：日志事件包含 ISO8601 时间戳、日志级别、简短调用位置、错误堆栈跟踪以及请求链路关联 ID（`requestId`）。
- **跨平台的缓冲区刷新**：`Sync(log)` 在停机时刷新缓冲条目，并显式处理和忽略同步控制台输出时发生的 Windows 控制台句柄 `EINVAL` 错误。

## 边界与不变量

- **严防凭据泄漏**：调用方绝对禁止将明文凭据、密码、认证 token 或未脱敏的 SQL 语句传入日志打印接口。
- **显式所有权传递**：Logger 实例统一在命令入口处构造，并显式注入给 HTTP 路由与中间件；全局不存在任何隐式的包级 Logger 单例。

参见 [日志配置](../config/README.md) 与 [cmd/server 守护进程](../../cmd/server/README.md)。
