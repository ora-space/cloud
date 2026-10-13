# projects：空间项目

## 职责

展示当前租户空间的项目列表、详情和项目操作。项目由空间成员共享；管理员可按现有生命周期删除项目，普通成员可查看并修改业务信息。创建项目时可从自己的 Git 凭据中挑选一个绑定，或选择公开仓库而不绑定。

不负责选择租户、管理成员或直接操纵执行资源。

## 文件

| 文件 | 说明 |
| --- | --- |
| `api.ts` | 项目查询、创建、修改与删除的 Cloud hooks（创建支持可选 `credentialRefId`） |
| `provisioning.ts` | 项目背后的开通 Operation，收敛为终态失败及其各原因码文案 |
| `projects-page.tsx`、`project-detail-page.tsx` | 列表与详情页面 |
| `create-project-dialog.tsx` | 创建项目表单，含可选凭据选择器与凭据拒绝文案 |
| `status.ts`、`components/` | 状态映射和展示组件，含开通失败提示 |
| `*.test.tsx` | 页面、hook 和操作测试 |

## 依赖与不变量

依赖 `features/spaces/current-space`、`features/settings/git-credentials-api`（读取自己的凭据）、生成客户端和 UI 组件；应用路由消费页面。租户 ID 必须来自所选空间；敏感删除操作只对 `admin` 展示，并由后端再次授权。创建项目时凭据是可选项，`credentialRefId` 只在实际选择后发送；服务端对不可用凭据返回 `repository_credential_frozen`、对范围不匹配返回 `repository_credential_scope_mismatch`，界面把这两个码转成可操作提示。

项目创建是异步的，结果落在 Operation 上而不是项目上。公开契约没有「某项目的 Operation 列表」路由，因此 `provisioning.ts` 经由项目的 Workspace 列表（`creatorOperationId`）取到 Operation，再读它的 `errorCode`。只有 `state === 'failed'` 算失败：`running`、`retry_wait`、`blocked` 都仍是进行中，把它们当失败展示等于让成员放弃一件还在跑的事。原因码原样显示在文案旁；没有对应文案的码回退为原因码本身，而不是一句让人安心的空话。带凭据 Clone 的两种确定性拒绝——部署未安装执行器（`repository_credential_executor_unavailable`）与引用未声明本次所需能力（`repository_credential_capability_unsupported`）——都有可操作文案，因为两者都是成员或管理员能据以行动的事实。提示不提供重试按钮——公开重试路由本来就拒绝 `failed` 的 Operation。

## 测试

MSW 模拟 Cloud 项目生命周期、版本冲突和角色控件。创建必须指定具体分支，运行环境异步启动并克隆仓库；提交反馈复用 `CreateFormSubmit`，等待时禁止重复提交。测试覆盖不选凭据、选择凭据、以及两种凭据拒绝码的可操作文案，也覆盖已结算的开通失败在详情页出现，以及那些不得被当成失败的状态。
