# clones：「仓库」页

[中文](README.md) | [English](README.en.md)

## 职责

侧边栏「仓库」对应的页面：经公开 clones API（`/api/v1/tenants/{tid}/clones`）提交 clone，由 Controller 领取、Node 执行 `git clone` 落盘，页面只展示 Cloud 记录的事实。它负责：

- 列出当前成员在当前空间所属租户下提交的 clone（Cloud 只让提交者本人看到自己的记录，同一租户的不同空间看到的是同一份列表），按 `createdAt` 倒序；
- 有 pending 或上次查询失败时每 2 秒轮询一次，全部到终态后停止；
- 提交前把请求身份（`requestId`、仓库、分支）写进当前标签页的 sessionStorage，确认被接受或被拒绝后才清除，回复丢失或刷新后只能「重试原请求」。

不负责：项目（`features/projects`，那是另一套 operation 模型）、会话与登录（`features/auth`）、租户与空间解析（`features/spaces`）、私有仓库凭据。

## 文件

| 文件 | 说明 |
|---|---|
| `api.ts` | `useCloneList`（带轮询的列表查询）、`useSubmitClone`（提交，经 `features/spaces` 的 `mutationHeaders` 以 `requestId` 作 `Idempotency-Key`；故障码经 `lib/api-client` 的 `faultCode` 读取），以及纯函数 `newestFirst`、`awaitsResult`、`classifySubmitFailure` |
| `pending.ts` | sessionStorage 边界：`pendingSubmissionKey`（按租户和用户分槽）、`readPendingSubmission`、`writePendingSubmission` |
| `use-clone-submission.ts` | 提交状态机：先存身份再发送，按失败类型保留或清除，列表里出现该 `requestId` 即视为已接受 |
| `status.ts` | 由事实推导的阶段（排队中／已派发／已完成／失败）、徽章样式与失败原因文案 |
| `messages.ts` | 提交提示与按故障码解释拒绝原因的 `rejectionMessage` |
| `clone-dialog.tsx` | 「Clone 仓库」对话框：新提交的表单，或待确认提交的只读摘要加「重试原请求」 |
| `clone-list.tsx` | 结果表格：仓库与分支、阶段徽章、commit／Node 路径或失败原因／残留路径、提交时间 |
| `repositories-page.tsx` | 页面组合：空间和会话就绪前不发任何请求；按存储键重挂载，切换用户或租户时读取各自的待确认提交 |
| `*.test.ts(x)` | 见下文「测试」 |

## 依赖与使用

依赖：`src/api`（生成的 clones 客户端）、`lib/api-client`（`faultCode`）、`features/auth/session`（用户 id）、`features/spaces/current-space`（租户 id）、`features/spaces/api`（`mutationHeaders`）、`components/ui`、`components/layout/page-header`、`components/common/dialog-form-field`、TanStack Query。

可被依赖：`routes.tsx`（`/w/:workspaceSlug/repositories`）。

## 不变量

- pending 只表示「还没有终态事实」，不能显示成失败、超时或进度。排队中与已派发的区别只看 `executionId` 是否已记录。
- 同一个待确认提交只能原样重发。只有 Cloud 明确拒绝（400/403/404/409/422）或已接受时才清除身份；断网、5xx、401、429 都保留。
- 存储键必须同时包含租户 id 和用户 id，否则同一标签页里换账号登录后，可能用新账号重发旧账号的请求。
- 存储读不出来时（抛异常或内容损坏）拒绝生成新的身份，不能当作「没有待确认提交」。
- 分支和仓库地址在发送前 trim。Cloud 会拒绝任何含空白或控制字符的输入，前端校验只用于尽早提示。

## 测试

`pending.test.ts` 覆盖存储槽隔离、读写往返和各种不可读情况。`api.test.ts` 覆盖排序、轮询判断、失败分类和拒绝文案。`repositories-page.test.tsx` 用 MSW 覆盖：列表事实与排序（含中断失败的文案与残留路径）、trim 后提交且幂等键等于 `requestId`、回复丢失后原样重发、刷新恢复而不自动重发、列表出现即确认、拒绝后清除并解释、不暴露其他用户的待确认提交、轮询在失败后自动恢复并在终态后停止、存储不可读或不可写时不发请求。轮询测试只伪造 `setInterval`，其余计时保持真实，避免与 MSW 和 findBy 互相等待。
