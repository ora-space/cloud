# repositories：外部 Git 线

## 职责

管理项目与外部 Git 仓库的绑定：展示当前代码来源（仓库、分支、快照、最近同步）、按成员意图触发同步、更换读取凭据，以及发起、批准、取消和核验一次显式发布。凭据本身只提交到 Credential Store，这里只处理元数据。

不负责克隆仓库、执行推送或判断快进（由 Cloud 与 repo-executor 决定），也不负责选择租户或空间。

## 文件

| 文件 | 说明 |
| --- | --- |
| `api.ts` | 代码来源、操作记录、凭据与发布动作的 Cloud hooks |
| `repository-panel.tsx` | 代码来源面板与发布记录 |
| `publish-dialog.tsx`、`publish-form.ts` | 显式发布表单；`publish-form.ts` 持有字段状态与两条派生规则 |
| `credential-page.tsx`、`credential-select.tsx`、`submission.ts` | 凭据列表、绑定选择与直接提交 |
| `credential-authority.ts` | 「谁能操作这条凭据」的唯一判定 |
| `*.test.ts(x)` | 模块测试 |

## 依赖与不变量

依赖 `features/spaces/current-space`（租户 ID 与角色）、生成客户端和 UI 组件；项目页面消费本模块。

- 期望的外部提交（`expectedOldOid`）必须由目标分支派生：只有目标仍是绑定的分支时才用快照提交，其他分支按「新建」处理（全零）。
- 发布按钮的文案与批准入口只表达权限，不构成授权；后端会重新判定。
- 凭据列表列出 Cloud 返回的每一条凭据，只有操作按钮按 `credential-authority.ts` 判定收窄（撤销额外允许租户管理员，因为撤销只会收紧访问）；token 永不进入前端或查询缓存。
- 一次请求只描述一个来源：填了 Revision 就不再发送分支。
- 列表（凭据、操作记录、可发布 Workspace）跟随游标读到最后一页：后端一页最多 100 行，更大的 `limit` 会被 400 `invalid_pagination` 拒绝，而不是截断。

## 测试

MSW 提供代码来源、操作记录、凭据、Workspace 与分支；`src/test/cloud-handlers.ts` 的 `repositoryFixture`、`repositoryOperationFixture`、`installRepositoryHandlers` 等夹具供本模块与后续模块共用。测试断言发出的请求体（完整比较）和可见的角色差异，不 mock 内部模块。
