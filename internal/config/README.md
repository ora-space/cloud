# internal/config：配置加载与校验

[中文](README.md) | [English](README.en.md)

`Load` 用局部 Viper 实例加载服务配置，校验后返回 `Config`；没有全局可变配置。

`plugins.personal_agent_target` 为空时不启用声明式个人 Agent 运行；可显式指定当前 Node 支持的 Linux target。Controller/Node 与插件 adapter 部署完成后再配置；运行仍校验当前 incarnation 的 target、版本和协议。

| 文件 | 职责 |
|---|---|
| `config.go` | 主结构、默认值、显式环境变量绑定和通用校验 |
| `env.go` | 将配置键映射为 `CLOUD_*` 环境变量 |
| `control_tls.go` | 构造控制监听的 TLS 1.3 双向认证材料 |
| `object_store.go` | 检查对象存储配置、读取密钥文件并构造内存 `objectstore.Config` |
| `repository.go` | Git ref、执行器与凭据服务的部署引用和开关 |

未指定路径时在 `./configs`、`../configs`、`.` 查找 `config.yaml`；显式路径由命令的 `-config` 传入。`CLOUD_DATABASE_DSN` 等覆盖对应叶子键；完整默认值和取值范围以源码及 [配置参考](../../docs/operations/configuration.md) 为准。

配置文件保存部署引用，原始私钥和机密通过独立文件加载，不写进配置正文、日志或数据库。包负责启动期配置有效性；业务窗口的冻结、数据库时钟与恢复判定在 Core。对象存储开启不等于在线健康探测。

`issue_runs.residual_reconcile_interval` 控制 Cloud 残留账本恢复扫描的间隔，缺省为 `1m`；环境变量 `CLOUD_ISSUE_RUNS_RESIDUAL_RECONCILE_INTERVAL` 可覆盖它。显式零、负数和非法时长均拒绝启动，没有关闭开关。该间隔只安排扫描，不是资源清理成功的超时判据。Cloud 只接收既有入口提供的真实事实，不新增 Controller 发现契约；证据未到时账本保持 open。

测试覆盖默认值、环境覆盖、非法时长、TLS、对象存储与仓库配置。参见 [server](../../cmd/server/README.md)、[有界启动](../../docs/agent-runs/startup-settlement.md) 与 [部署出网配置](../../docs/operations/deployment-egress.md)。
