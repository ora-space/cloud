# layout：已认证应用外壳

[中文](README.md) | [English](README.en.md)

本模块组合侧边栏、页面页眉和路由出口。认证不在这里做：路由把 `DashboardLayout` 包在 `features/auth` 的 `RequireSession` 里，401 转到登录页并保留当前站内路径，403 就地提示账号停用，临时故障就地提示重试。本模块只在会话已确认后把 `:workspaceSlug` 解析为成员真实加入的空间（`CurrentSpaceProvider`），订阅该空间的事件流；没有任何空间时转到 `/onboarding`，未知或已归档的 slug 回落到第一个空间。

`AppSidebar` 通过 `useSession` 读取当前用户，列出 `/me/spaces` 返回的所有租户空间。切换后以所选空间的 `tenantId` 访问资源；“新建工作区”创建新租户。它还提供退出登录，以及 Gateway 列出 `github` 时的 GitHub 注销。工作导航只有任务、项目、工作流三项（没有独立的「仓库」页：仓库由项目绑定，Workspace 从 Git Server 快照获取）。业务导航仍可使用模拟数据，但不得在本模块保存 token 或复制认证状态。

测试覆盖工作导航条目、当前用户展示、空间解析与回落、事件流订阅、退出与 GitHub 注销。

会话分析入口位于侧栏“工作”分组的“工作流”正下方，路径为 `/w/:workspaceSlug/sessions`，Agent 详情为 `/sessions/agent/:issueId/:runId`。路径由 `workspacePaths` 统一生成；侧栏入口顺序与真实租户路由分别由布局测试、`session-routes.test.tsx` 覆盖。详情功能由 `features/sessions`、`session-data`、`session-timeline` 提供。
