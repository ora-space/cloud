# settings：设置页

## 职责

承载设置导航、空间基本信息页，以及当前用户的「Git 身份」与「Git 凭据」页。管理员可按版本修改空间名称，同步更新租户名称；slug 保持不变。Git 身份属于登录用户而非空间：Agent 提交代码时以它作为 Git 作者与提交者，修改只对之后启动的会话生效。Git 凭据也是登录用户个人的 HTTPS 仓库凭据，`repositoryScope` 是它唯一被授权的仓库，`availability` 是它能否使用的唯一依据；Token 只写不读，任何响应都不会返回它，被撤销的凭据仍留在列表中。成员与计费页面由各自功能模块负责。

不负责成员权限实现或空间归档。

## 文件

| 文件 | 说明 |
| --- | --- |
| `settings-layout.tsx` | 设置页导航和子路由容器 |
| `general-settings-page.tsx` | 空间名称编辑及只读 slug |
| `git-identity-api.ts` | `/api/v1/me/git-identity` 的读取、按版本保存（PUT）、恢复默认（DELETE，带稳定幂等键）hooks 与故障文案 |
| `git-identity-page.tsx` | 「Git 身份」卡片：名称/邮箱、「默认身份」徽章、「保存」「恢复默认」 |
| `git-credentials-api.ts` | `/api/v1/tenants/{tid}/git-credentials` 的分页列表、创建（POST）、轮换（PUT）、撤销（DELETE，带稳定幂等键）hooks 与故障文案 |
| `git-credentials-page.tsx` | 「Git 凭据」卡片：列表（名称、仓库范围、可用性徽章、创建时间）、创建表单、逐行轮换与撤销 |
| `*.test.tsx` | 导航、管理员编辑、普通成员只读、Git 身份与 Git 凭据的加载/新增/冲突/撤销/部署未配置测试 |

## 依赖与不变量

依赖 `features/spaces`（含 `mutationHeaders`、`useIdempotencyKeys`）、`lib/pagination`（`readAllPages` 遍历凭据分页）、生成客户端、`lib/api-client`（`faultCode`）、路由和 UI 组件；由应用路由消费（`settings`、`settings/git-identity`、`settings/git-credentials`），项目创建对话框复用 `git-credentials-api` 的列表 hook 选择凭据。服务端仍校验角色、可用性与版本，界面的禁用状态不构成授权。Git 身份表单按 `version` 重新挂载：保存或恢复默认后，草稿取自服务端的返回值，下一次保存携带新版本；版本冲突只提示刷新，不自动覆盖用户草稿。Git 凭据的 Token 只存在于表单本地状态，成功后清空，失败时保留以便重试，绝不写入 localStorage、URL、日志或查询缓存键。

## 测试

MSW 模拟改名接口并验证普通成员无法提交编辑；Git 身份测试校验 PUT/DELETE 请求体中的版本号、各故障码文案与默认身份时禁用「恢复默认」。Git 凭据测试校验加载态、空列表、列表徽章、创建请求体与 `Idempotency-Key`、轮换/撤销的版本冲突文案与草稿保留、`credential_store_unavailable` 文案，以及被撤销的凭据仍以「已撤销」呈现而非消失。
