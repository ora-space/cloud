# settings：设置页

## 职责

承载设置导航、空间基本信息页与当前用户的「Git 身份」页。管理员可按版本修改空间名称，同步更新租户名称；slug 保持不变。Git 身份属于登录用户而非空间：Agent 提交代码时以它作为 Git 作者与提交者，修改只对之后启动的会话生效。成员与计费页面由各自功能模块负责。

不负责成员权限实现或空间归档；个人模型连接由 `features/model-connections` 的专用模块负责。

## 文件

| 文件 | 说明 |
| --- | --- |
| `settings-layout.tsx` | 设置页导航和子路由容器，含当前用户的「模型连接」入口 |
| `general-settings-page.tsx` | 空间名称编辑及只读 slug |
| `git-identity-api.ts` | `/api/v1/me/git-identity` 的读取、按版本保存（PUT）、恢复默认（DELETE，带稳定幂等键）hooks 与故障文案 |
| `git-identity-page.tsx` | 「Git 身份」卡片：名称/邮箱、「默认身份」徽章、「保存」「恢复默认」 |
| `*.test.tsx` | 导航、管理员编辑、普通成员只读与 Git 身份的保存/冲突/恢复默认测试 |

## 依赖与不变量

依赖 `features/spaces`（含 `mutationHeaders`、`useIdempotencyKeys`）、生成客户端、`lib/api-client`（`faultCode`）、路由和 UI 组件；由应用路由消费（`settings`、`settings/git-identity`）。服务端仍校验角色与版本，界面的禁用状态不构成授权。Git 身份表单按 `version` 重新挂载：保存或恢复默认后，草稿取自服务端的返回值，下一次保存携带新版本；版本冲突只提示刷新，不自动覆盖用户草稿。

## 测试

MSW 模拟改名接口并验证普通成员无法提交编辑；Git 身份测试校验 PUT/DELETE 请求体中的版本号、各故障码文案与默认身份时禁用「恢复默认」。
