# features/plugins: 工作区插件页

[中文](README.md) | [English](README.en.md)

工作区侧栏"插件"栏的页面与数据层:上半是**已选插件**列表(desired/observed 状态徽章 + 移除),
中间是**工作流资产**导入结果(逐文档结果,与安装状态分开),下半是**插件市场**目录网格(搜索、
kind 筛选、安装)。它只表达 cloud 的权威选择状态;激活、停止、配置与日志属于 Node 运行时平面,
不在本模块。

## 文件

- `api.ts`：cloud-backed hooks —— `usePluginCatalog`(5 分钟 staleTime,与云端同步节奏对齐)、
  `useSpacePlugins`、`useWorkflowAssetImports`(独立读取;非终态时每 10 秒轮询)、
  `useInstallPlugin`(固定目录版本)、`useRemovePlugin`(乐观版本;409 后自动刷新)。
- `plugins-page.tsx`：页面与展示子组件(状态徽章文案、kind 中文标签、安装/移除交互、资产导入
  文档结果与有界失败码文案)。
- `*.test.tsx`：页面与 hooks 的功能测试(MSW 契约 fixtures)。

## 依赖与调用方

- 依赖:`src/api`(orval 生成客户端)、`features/spaces`(current-space、幂等键、SSE 失效)、
  `components/ui`。
- 调用方:`routes.tsx`(`/w/:slug/plugins`)、`components/layout/app-sidebar.tsx`(导航入口)。
- 测试依赖:`mocks/handlers/plugins.ts`、`test/cloud-handlers.ts`。

## 不变量

- 目录数据只来自 PostgreSQL 快照(staleTime 对齐 5 分钟同步节奏);页面永不直接访问市场网络。
- 状态徽章只展示 `observedState` 六态之一,`desiredState=removed` 的行不进入已选列表。
- 工作流资产导入是独立查询,不和插件安装徽章合并:安装完成而某文档导入失败是合法组合。
  文档失败码是稳定契约,前端只做文案映射,未知码原样展示。
- 导入行只在 `pending`/`importing` 时轮询;全部终态后停止,过期结果由 SSE 或下次挂载刷新。
- `pack` 条目 v1 禁装:按钮恒禁用并显示"暂不支持"。
- 所有异步更新都在查询/变更的 act 边界内(clean-stderr 门禁)。

## 测试

- 页面行为经真实 API 路径的 MSW fixtures 验证:安装 POST 后行进入待安装、按钮进入禁用态;
  移除需确认、携带乐观 version,409 时展示提示并刷新。
- 资产导入经独立的 `/plugins/assets` fixture 验证:逐文档已导入/失败结果与安装徽章分区展示;
  轮询测试用 fake timers 推进到终态后断开发起新请求。
- SSE 失效分支在 `features/spaces/spaces.test.tsx` 断言;导航入口在 app-sidebar 测试断言。

## 运行时控制

当前租户管理员修改协作空间插件选择，成员只读。请求携带资源版本（首次为 0）；期望保存后，各运行时等待空闲及维护独占权。界面显示受影响、完成、等待占用、等待启动、执行能力缺失与失败计数。生产插件执行器尚未接线，保存选择不表示真实安装完成，也不自动启动已停止的运行时。服务端重新校验权限，不依赖按钮状态。
