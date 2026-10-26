# internal/egress：部署出网策略

[中文](README.md) | [English](README.en.md)

为模型请求、工作流资产下载和外部 Git 访问提供共享的不可变部署策略；各调用方负责接线，不在这里决定业务权限。

| 文件 | 职责 |
|---|---|
| `config.go`、`config_errors.go` | 有界读取与严格解析 schemaVersion 1 JSON，拒绝重复/未知字段和尾随数据；规范化主机、端口、网段与静态地址 |
| `policy.go` | `Policy`、目标准入、地址检查与规范化配置摘要 |
| `dns.go`、`public_dns.go` | 受控解析与拨号：检查所有解析地址后只连接通过校验的数字地址 |
| `health.go` | 就绪响应所需的 schema/digest 元数据 |

public 模式只接受公网目标；enterprise 模式只接受部署列出的目标，并按静态地址或部署 TCP DNS、允许网段与内部拓扑约束检查。空配置路径采用 public 策略；读取错误、未知版本和非法配置均返回错误。摘要可以公开，原始配置和端点细节不进入就绪响应。

包测试覆盖配置拒绝矩阵、DNS、地址范围、连接固定与摘要。配置格式和部署步骤见 [部署方共享出网配置](../../docs/operations/deployment-egress.md)。
