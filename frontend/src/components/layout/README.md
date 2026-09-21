# layout：已认证应用外壳

[中文](README.md) | [English](README.en.md)

本模块组合侧边栏、页面页眉和路由出口。`DashboardLayout` 在渲染工作区前通过 `/api/v1/me` 确认真实 Cloud 会话：401 转到自动登录页并保留当前站内路径，403 停止重登并提示账号停用，临时故障只提供显式重试。

`AppSidebar` 接收 Cloud 返回的 `User`，展示首次 JIT 创建时记录的 `displayName`，并通过真实 `POST /auth/logout` 吊销 Gateway 会话。业务导航仍可使用模拟数据，但不得在本模块保存 token 或复制认证状态。

测试覆盖当前用户展示、未登录跳转、故障处理和本地退出行为。
