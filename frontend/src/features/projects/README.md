# projects：空间项目

## 职责

展示当前租户空间的项目列表、详情和项目操作。项目由空间成员共享；管理员可按现有生命周期删除项目，普通成员可查看并修改业务信息。创建项目时可从自己的 Git 凭据中挑选一个绑定，或选择公开仓库而不绑定。

不负责选择租户、管理成员或直接操纵执行资源。

## 文件

| 文件 | 说明 |
| --- | --- |
| `api.ts` | 项目查询、创建、修改与删除的 Cloud hooks（创建支持可选 `credentialRefId`） |
| `projects-page.tsx`、`project-detail-page.tsx` | 列表与详情页面 |
| `create-project-dialog.tsx` | 创建项目表单，含可选凭据选择器与凭据拒绝文案 |
| `status.ts`、`components/` | 状态映射和展示组件 |
| `*.test.tsx` | 页面和操作测试 |

## 依赖与不变量

依赖 `features/spaces/current-space`、`features/settings/git-credentials-api`（读取自己的凭据）、生成客户端和 UI 组件；应用路由消费页面。租户 ID 必须来自所选空间；敏感删除操作只对 `admin` 展示，并由后端再次授权。创建项目时凭据是可选项，`credentialRefId` 只在实际选择后发送；服务端对不可用凭据返回 `repository_credential_frozen`、对范围不匹配返回 `repository_credential_scope_mismatch`，界面把这两个码转成可操作提示。

## 测试

MSW 模拟 Cloud 项目生命周期、版本冲突和角色控件。创建必须指定具体分支，运行环境异步启动并克隆仓库；提交反馈复用 `CreateFormSubmit`，等待时禁止重复提交。测试覆盖不选凭据、选择凭据、以及两种凭据拒绝码的可操作文案。
