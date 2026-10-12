# internal/mirrorpublish：镜像写入方

[English](README.en.md) | [中文](README.md)

`internal/mirrorpublish` 是内部 Git Server 镜像的写入方与回收方。[`internal/mirror`](../mirror/)
存储回答"某个镜像此刻是否可读"，本包回答"应当存在哪些镜像"——后者只有 Cloud 自己的数据行能回答。

## 职责

- **授权**（[`references.go`](references.go)）：`References.Binding` 只读一次 PostgreSQL，返回全部存活
  Project 及其当前指向的上游仓库，以及 Cloud 的数据行此刻引用的全部镜像键——Workspace 未删除、带有已钉住
  的提交、属于 active Project，且在 `clone_executions` 中有 `clone_ready` 执行。键由这些行推导，绝不来自
  调用方。
- **一轮对账**（[`mirrorpublish.go`](mirrorpublish.go)）：`Reconciler.Reconcile` 依次执行撤销、发布、清扫。
  `materialize` 在触碰存储之前再次用 Cloud 的答复校验该键，因此"不发布 Cloud 未引用的镜像"对进入它的**每一条**
  路径都成立——能写镜像卷的进程，仍无法发布一个 Cloud 未绑定租户、仓库与克隆执行的镜像（D2）。
- **撤销与回收**：某个已存储的键所属 Project 现在指向了别的上游仓库，则**持久撤销**（D6 的第一条触发条件）；
  未被引用、但 Project 仍指向同一仓库的键留给 `mirror.Store.Sweep`，在宽限期后回收。完全读不到 Cloud 的一轮
  不做任何改动并返回错误。
- **抓取**（[`fetch.go`](fetch.go)）：`GitFetcher` 是本包唯一触网的组件。它执行一次加固后的 `git fetch`——与
  带凭据 clone 相同的封闭协议集合、拒绝重定向与固定凭据 helper——只抓取一个分支到唯一一个 ref
  （`refs/ora/mirror`），并从仓库里**读回**提交，使存储的核验比较的是两个独立取得的值。

## 不变量

- **发布的授权来自 Cloud，而不是文件系统的写权限。** 入口只有 `Reconcile` 一个，它的第一步就是取 Cloud 的
  答复。Cloud 数据行未提及的键得到 `ErrNotAuthorized`。
- **一次失败的尝试不等于一个损坏的镜像。** 抓取失败会让记录停在 failed 状态（读取方拒绝而不是服务），暂存内容
  被删除；下一轮会重试该键。
- **撤销是持久的。** Project 若回到此前离开的上游，需要运维动作——因为撤销刻意不是普通一轮能撤销的事情。

## 本包未实现

写入方目前**没有生产调用方**。`Fetcher` 与 `MaterialSource` 是消费侧边界：部署把它们接到执行身份 C 所执行的
带凭据抓取上，而那次抓取要兑换一枚 Cloud 为单个仓库签发的单次授权。Cloud 今天不签发镜像授权，因此写入方无法
部署、也无法端到端测量；它所依赖的带凭据 clone 本身按设计 fail-closed。Cloud 的"就绪确认"在读取侧已有调用方：
Git Server 服务前会先问同一个存储；缺的是 D7 的"Cloud 只在镜像就绪后写克隆输入"。
