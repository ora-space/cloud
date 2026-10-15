# projects：空间项目

## 职责

展示当前租户空间的项目列表、详情和项目操作。项目由空间成员共享；管理员可按现有生命周期删除项目，普通成员可查看并修改业务信息。创建项目时可从自己的 Git 凭据中挑选一个绑定，或选择公开仓库而不绑定。

不负责选择租户、管理成员或直接操纵执行资源。

## 文件

| 文件 | 说明 |
| --- | --- |
| `api.ts` | 项目查询、创建、修改与删除的 Cloud hooks（创建支持可选 `credentialRefId`） |
| `provisioning.ts` | 项目背后的开通 Operation，收敛为开通阶段、终态失败及其各原因码文案 |
| `deletion.ts` | 删除项目可能被拒绝的各原因码，以及每个码对应的可操作文案 |
| `projects-page.tsx`、`project-detail-page.tsx` | 列表与详情页面 |
| `create-project-dialog.tsx` | 创建项目表单，含可选凭据选择器与凭据拒绝文案 |
| `status.ts` | 项目生命周期状态到徽标的映射 |
| `components/` | 展示组件子模块（图标、开通失败提示），见 [`components/README.md`](components/README.md) |
| `*.test.tsx`、`*.test.ts` | 页面、hook、操作与文案测试 |

## 依赖与不变量

依赖 `features/spaces/current-space`、`features/settings/git-credentials-api`（读取自己的凭据）、生成客户端和 UI 组件；应用路由消费页面。租户 ID 必须来自所选空间；敏感删除操作只对 `admin` 展示，并由后端再次授权。创建项目时凭据是可选项，`credentialRefId` 只在实际选择后发送；服务端对不可用凭据返回 `repository_credential_frozen`、对范围不匹配返回 `repository_credential_scope_mismatch`，界面把这两个码转成可操作提示。

删除请求被拒绝与开通失败是两件不同的事实，前者的文案由 `deletion.ts` 拥有。它经 `lib/api-client` 的 `faultCode` 读取 Cloud 的 `Fault` 码（HTTP 只在那一个边界适配器里解析），在删除对话框里把句子和原因码并排渲染：运行环境正被占用、同一项目已有一次删除在进行、账号角色已变更，都说明原因并说明怎样才能结束它，而不是只显示一个原因码；拒绝里读不出码时什么都不渲染，因为替 Cloud 断言一个它没说过的原因是把成员引向错误的方向。被拒绝的删除停留在当前页面（只有 Cloud 接受的删除才跳转），成员据此处理后可以再次提交。

项目创建是异步的，结果落在 Operation 上而不是项目上。公开契约没有「某项目的 Operation 列表」路由，因此 `provisioning.ts` 经由项目的 Workspace 列表取到 Operation，再读它的 `errorCode`：项目的创建由**主** Workspace 承载（`kind === 'main'`），isolated Workspace 有自己的 Operation、其结果项目不共享——列表按随机 id 排序，取「第一个带 `creatorOperationId` 的 Workspace」会跟着列表顺序摇摆，报出项目没有的失败，或漏掉真实的那次。hook 把 Operation 状态收敛为四个开通阶段（`provisioning`、`retrying`、`failed`、`succeeded`），列表和详情的状态徽标优先显示阶段：`lifecycle` 没有失败态，创建终态失败的项目会永远停在 `provisioning`，只看 `lifecycle` 就会一直显示「规划中」。阶段未知（还没有 Workspace、或遇到本版本未教过的状态）时回退显示 `lifecycle` 状态，而不是替服务端断言一个前端证明不了的状态。只有 `state === 'failed'` 算失败：`running` 是 `provisioning`，`retry_wait`、`blocked` 是 `retrying`，把它们当失败展示等于让成员放弃一件还在跑的事。列表页只对仍在创建（`status === 'planned'`）的项目查询开通 Operation：已结算的项目不会有可变的答案，多问一次就是每张卡片两个多余请求。原因码原样显示在文案旁；没有对应文案的码回退为原因码本身，而不是一句让人安心的空话。带凭据 Clone 的确定性拒绝——部署未安装执行器（`repository_credential_executor_unavailable`）、引用未声明本次所需能力（`repository_credential_capability_unsupported`），以及成员已删除的凭据（`repository_credential_revoked`，其 Token 已销毁、拒绝永远不会自行结束）——都有可操作文案，因为三者都是成员或管理员能据以行动的事实。提示不提供重试按钮——公开重试路由本来就拒绝 `failed` 的 Operation。

## 测试

MSW 模拟 Cloud 项目生命周期、版本冲突和角色控件；重命名通过粘贴完整名称验证乐观版本请求。创建必须指定具体分支，运行环境异步启动并克隆仓库；提交反馈复用 `CreateFormSubmit`，等待时禁止重复提交。测试覆盖不选凭据、选择凭据、以及两种凭据拒绝码的可操作文案，也覆盖已结算的开通失败在列表与详情页取代「规划中」出现、重试中与终态失败的区分，以及已结算的项目不再发起开通查询。`provisioning.test.tsx` 用两个 Workspace 固定取 Operation 的依据：isolated 的 Operation 失败时项目仍读作成功，主 Workspace 失败时即使 isolated 成功也照样报出失败——只按「第一个带 `creatorOperationId` 的 Workspace」选，这两个方向都会读错。删除方面覆盖被接受的删除路径、被拒时显示可操作文案与原因码、以及重复删除读作「已有一次删除正在进行」；`deletion.test.ts` 固定删除可能被拒的每个码都有自己的文案、重复与拒绝两者不会读起来一样、以及未教过的码回退为码本身。
