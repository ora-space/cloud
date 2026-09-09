# Substrate、Node 和阶段二契约

阶段一交付 cloud 核心及模拟组件。`internal/simulator` 的 Controller 没有数据库 handle，通过真实 HTTP 领取/推进；Substrate 在磁盘保存 effect journal，Git 用真实命令；Node 通过 scoped 签名凭据模拟注册、初始化、idle 和结束票据。没有真实 Pod、Agent、Deno 插件、PTY、跨宿主卷或 Rust 进程。

## 存储与挂载

每个 Project 一个共享卷，layoutVersion=1：

```text
repository.git/
workspaces/{workspace UUID}/checkout/
workspaces/{workspace UUID}/runtime/
```

repository.git 是 bare repository；main 与 isolated 都是 linked worktree。branch 固定由服务生成 `ora/{workspace UUID}`，cloud 保存 requestedRef 与最终 commitId，客户端不能指定宿主路径、Node 地址、sandbox ID。runtime 持久保存 Node 所需工作数据，stop 保留；只有 Workspace delete 清理自己的 checkout/runtime，Project delete 在全部 sandbox/维护 Job 结束后删除卷。

普通 Node 容器只挂自身 checkout/runtime 及共享 Git metadata，容器路径约定 `/workspace/checkout`、`/workspace/runtime`、`/project/repository.git`。生产 Substrate 必须修正 Git worktree metadata 内的路径，使 linked worktree 在该固定容器路径可用；本地模拟器使用完整本地绝对路径，未证明部分挂载在真实容器内可用。禁止挂载其他 Workspace 的未提交目录。共享 refs/objects/Git metadata 不提供同 Project 内 Git 内容保密；跨用户跨 Project 必须通过独立卷、服务 scope 与基础设施挂载权限隔离。

维护 Job 使用 Node 镜像的有限维护入口，可见完整 Project 卷；Controller 按 Project 串行调度 clone/init、解析 ref、worktree add/remove 和维护。普通部分挂载 Node 不执行全仓库 prune，也不增加常驻维护服务。任务标识以 cloud effect ID 幂等；已运行旧 Job 在数据库 lease 失效时不会自动停止。

## Substrate 最小接口

下面是模拟器实际实现的 HTTP 形式，生产适配器可以映射到其控制 API，但必须保持语义：

| 方法 | 请求 | 返回与不变量 |
|---|---|---|
| GET `/effects/{effectId}` | cloud 事先分配的 UUID | 404=确知尚无该 intent；否则返回原 request、externalId、state、result，查询不会创建 |
| PUT `/effects/{effectId}` | kind、projectId、workspaceId?、repositoryUrl、requestedRef、sandboxInstanceId? | 首次先落 journal 再执行；同 ID 同 payload 幂等，同 ID 不同 payload 409；成功后原结果保留 |

kind 支持 storage_ensure、worktree_ensure、sandbox_ensure、sandbox_terminate、worktree_delete、storage_delete。模拟器只接受显式 repository URL→本地 fixture 映射，不接入真实私有 Git 凭据。cloud 内部的 snapshot 才向受控 Controller 提供本 Project 的 credential reference；基础设施负责解析引用、注入 Git 凭据、审计和轮换，Cloud 从不保存密钥值。

生产 Substrate 应用独立服务身份与受控网络保护这些接口，并验证 Project/Workspace/effect scope；模拟 HTTP handler 仅供 loopback 测试，不能作为生产公共端点发布。幂等 ensure 必须能查询“执行成功但响应丢失”的实际对象，不能以调用方超时判定不存在。terminate 返回确认旧进程不会再访问存储的证据；不确定就 blocked，不分配新 generation。

模拟器是同步有限 Job，持久状态为 running/succeeded/failed；全局 mutex 使重入同任务串行，重新构建 handler 后按 journal 与磁盘/Git 状态协调。真实异步 Job 必须提供查询、终止请求与终止确认，Controller 接管先协调存量 Job，不能把“旧 controller 失租”等同于 Job 已终止。

## 阶段二必须实际验证

- 单集群跨宿主共享存储需 RWX 与 Git 依赖的锁、原子 rename/文件操作语义；RWO 不能冒充跨节点共享卷。
- Node 镜像、bare/linked-worktree 的部分挂载路径、Git common-dir/worktree metadata 的容器可移植性。
- 旧 sandbox/维护 Job 的终止或 storage fence，在网络分区与 Controller 接管下仍阻止旧文件写入。
- 真实 Node 本地准入/idle 原子性、全部执行类型覆盖、Node 重启和 token 刷新、Agent/Deno/PTY 子进程归属。
- 长 Job 期间每 10 秒续租和失租立即停止后续调度；未知 prompt 结果不自动重放。
- Gateway 的华为登录集成、内部凭据签发和生产证书/密钥分发。

## 后续业务数据迁移

Session 仍属于 Workspace，不将 sandboxId/nodeId 作为持久业务身份。完整 Session/Workflow Run、Effect、插件配置/安装状态、历史持久化和 desktop 迁移不在本阶段实现。未来表需继承 tenant+owner+Project+Workspace 约束，并由 cloud 保存权威状态；Controller 不得自行建立 PG 旁路。

desktop bootstrap 当前耦合 SQLite、Session JSONL、插件与 Workflow，阶段二拆出接口适配。历史 JSONL 追加顺序不等同展示 position；重复 position 表示修正，迁移必须保留最后修正、Gap、受损尾行和 pending tool 语义，不能直接按 append 顺序赋展示 seq。先定义快照/增量边界、幂等导入键与所有权映射，再导入历史；本次未导入任何桌面数据。

Effect Scope、Desired State/Generation、插件 canonical identity、Workflow 状态与运行结果必须各自有明确 cloud 持久化归属与版本契约；不要把这些塞进阶段一 operations.result 的任意 JSON 中。当前 Task 仅为 isolated Workspace 一对一展示身份，execution_tickets 是并发准入证据，不是完整 Session/Workflow 领域替代品。
