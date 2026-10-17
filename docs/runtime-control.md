# 多人运行时控制

[English](runtime-control.en.md)

## 权威与范围

协作空间是可见租户；Workspace 是项目下独立克隆与数据卷的运行时。项目归属 owner 不变，creator_user_id 独立保存真实创建者，operation.actor_user_id 保存任务发起者。有效成员可看共享项目和安全概况，运行时内容及操作详情只供创建者或当前管理员。未知历史创建者只供管理员。没有第一阶段成员授权表或共同编辑。

PostgreSQL 是业务权威。所有决定、计划和版本变化只在短数据库事务中发生；Controller 和执行程序调用在事务之后进行。Node 的 SQLite 仅保存受保护的本地执行责任，Cloud 模式 Controller 不建立业务数据库。

## 操作资格与责任

Cloud 为页面分配 sessionId；同用户第二页面必须重新争取资格。获取先预留 acquiring，当前 Node 持久确认后变 held；停止且无沙盒的运行时可直接 held。60 秒有效期、每 20 秒续期，时间由 PostgreSQL 裁决。使用权限、页面资格及活动保护分别检查；管理员普通操作不能抢占。

release、到期、断线和成员失效关闭新资格，进入 draining；未知存量进入 reconciling。旧入口关闭且所有 ticket、clone 和普通生命周期责任结束才 idle。幂等重放返回历史响应，不复活资格。运行时控制代次、Controller 租约代次、运行时代次、Node/Host incarnation（一次进程实例的身份）和稳定 execution ID 分别记录。Node 同会话也只接受一个冲突活动，在接受及首次文件变更前检查资格；已开始的旧执行只按原范围收尾。

生命周期请求携带 version 与 sessionId。restart 是单个持久 operation，先确认终止再推进新运行代次，挂载原数据，不重新 clone。项目删除要求管理员，原子检查全部运行时占用及活动，不部分删。

## 强停

POST /api/v1/tenants/{tenantId}/workspaces/{workspaceId}/force-stop 是独立入口，要求 reason、version、impactConfirmed=true 和 Idempotency-Key。调用方必须明确提示目标全部工作会被打断及未保存数据可能丢失。强停可与普通 operation 在途并存，记录独立持久意图；登记不删除或伪造原 operation/effect/execution。

Controller 派发前重新获取短期 effect permit（执行许可），执行端持久化 runtime generation 围栏及 sandbox tombstone（已终止标记）再终止。Cloud 只接受 terminated=true 且 lateEnsureFenced=true 的确认证据；之前禁止重启、交接及删卷。许可最长 10 秒，且不能超过 Controller 租约期限；Node 绑定同样不能超过租约。旧结果仍按原稳定身份核对。

## 插件与仓库凭据

仅管理员修改空间插件期望，成员可读安全汇总。选择固定发布版本和请求者，现有目标保存 pending 与等待原因；恢复扫描自动重访。维护使用相同独占边界，停止实例不会为插件启动，不自动把以后新建实例加入旧选择。生产真实插件执行器尚未接线，保持 executor_capability_unavailable，不报告安装成功。模拟器仅为显式开发测试装配。

凭据只保存引用、类型、范围、能力、版本及可用性。成员离开原子冻结 personal/unknown，team 不因旧 owner 离开冻结；重新加入不自动解冻。保留原 owner 外键、旧引用与任务责任。项目凭据提供方及受控团队能力验证/原子重绑未交付。仓库凭据不控制任何 Node 工作：clone、Agent 会话与交付只与 Git Server 通信，Node 不接触外部仓库。

## 部署与升级

追加迁移 0018–0023；不得编辑已发布迁移。cloudctl migrate 使用部署迁移身份，server 启动只验证迁移。历史创建者只从唯一匹配创建记录回填，旧运行实例不根据空新表推断空闲。Cloud proto 是契约源；消费端应锁定提交并生成。旧无运行时 scope 的新 clone 准入关闭，旧责任查询保留；生产内部过渡 JSON 管理路径返回 410。

Cloud Controller gRPC 使用双向 TLS（两端均核验证书），配置 CLOUD_CONTROL_CERTIFICATE_FILE、CLOUD_CONTROL_PRIVATE_KEY_FILE、CLOUD_CONTROL_CLIENT_CA_FILE、CLOUD_CONTROL_CONTROLLER_IDENTITY。身份 URI 必须匹配可信 Controller，metadata 声明仅用于一致性检查。私钥只在管理卷，不能放入代码目录。Controller 只拨出，不恢复旧监听或 minicloud。真实部署见 cluster 的运行时验收说明。

## 证据边界

Cloud integration/runtime_* 覆盖真实 PostgreSQL/HTTP/gRPC 权限、竞争、历史重放、迁移与强停状态机；注入 Node 确认不构成物理终止证明。integration/plugin_pending_test.go 验证生产无执行能力时持久等待。integration/repository_credentials_test.go 验证冻结及关闭静态回退。真实 Node、Git、容器、进程和卷由 desktop/cluster 分别验证。文件、终端、Agent 云端产品入口、真实插件执行器、项目凭据解析与验证重绑没有安全配套，保持关闭。完整故障恢复与升级/回滚组合仍须独立验收；六份 ADR 不因这些局部实现自动变 implemented。
