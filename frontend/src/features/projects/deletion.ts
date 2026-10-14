import { faultCode } from '@/lib/api-client'

/**
 * Why the cloud refused to delete a project, in the shape the delete dialog
 * renders: the cloud's stable reason code together with a sentence the member
 * can act on.
 *
 * The code is kept beside the sentence rather than replaced by it, for the same
 * reason `provisioning.ts` keeps both — reason codes are part of the public
 * contract and are what support asks for, while the sentence is presentation.
 *
 * This is a different concern from `provisioning.ts`. That module explains why
 * an Operation *ended*: an asynchronous outcome the member reads afterwards.
 * This one explains why a *request* was refused here and now, which is the
 * synchronous answer to something the member just did. The two share a shape
 * and not a lifetime, and the codes are not interchangeable: a refused delete
 * can answer `operation_in_progress`, which never names a provisioning failure.
 */

/** A refused deletion: the code the cloud answered with, and a sentence for it. */
export interface DeletionRefusal {
  code: string
  message: string
}

/**
 * Sentences for the refusal reasons this module has been taught.
 *
 * Each one names what the member, or their administrator, can do next, because
 * a deletion refused with nothing to act on is one the member can only retry
 * blindly. A code that is not listed falls back to the bare code: for an
 * untaught reason the code itself says more than an invented sentence would.
 */
const DELETION_REFUSAL_MESSAGES: Record<string, string> = {
  // The same project was submitted for deletion twice — by two members, or by
  // one member in two tabs. The first request is already running the lifecycle
  // to completion, so the honest answer is that nothing more is needed, rather
  // than that deleting failed.
  operation_in_progress: '该项目已有一次删除正在进行，无需重复提交。可在项目列表刷新查看结果。',
  // A Workspace of the project is claimed right now — a member's runtime
  // session, or an Agent run using it. Cloud refuses rather than pulling a
  // sandbox out from under live work, and the way through is for that work to
  // end first.
  runtime_control_held:
    '该项目的某个运行环境正在被使用（成员会话或 Agent 运行）。请等它结束，或先停止该运行时，再删除项目。',
  // `checkActivities` found an execution Cloud cannot account for: an active
  // ticket, or an execution with no result. Cloud refuses because it cannot
  // prove the write has stopped, so waiting is the whole remedy.
  resource_in_use: '该项目仍有正在执行或结果未知的运行时任务，暂时无法删除。请稍后重试。',
  // The public delete requires the admin role of the project's space. The UI
  // hides the control from members, so reaching this means the caller's role
  // changed after the page was rendered.
  space_role_required: '只有该空间的 admin 可以删除项目。当前账号没有该权限，请联系空间管理员。',
  // Someone edited the project after this page was rendered, so the version the
  // dialog holds is stale. Refreshing is the whole remedy, and the delete was
  // not performed.
  version_conflict: '项目已被其他成员修改，删除未执行。请刷新页面后重试。',
  // The project is already gone: deleted by another member, or by this member
  // in another tab.
  not_found: '该项目已不存在，可能已被删除。请刷新页面。',
  // The Workspace has no runtime control record to reconcile against, which is
  // an out-of-band change to the deployment. Nothing the member can do here, so
  // the sentence names who can.
  runtime_control_reconciliation_required:
    '该项目的运行环境状态需要先由运维核对（缺少运行时控制记录），暂时无法删除。请联系运维。',
}

/** The sentence to show for `code`, falling back to the code itself. */
export function deletionRefusalMessage(code: string): string {
  return DELETION_REFUSAL_MESSAGES[code] ?? `删除失败：${code}`
}

/**
 * The refusal a failed delete request carries, or `undefined` when it carries
 * none — a network failure, an abort, or a rejection this build cannot read.
 * The dialog renders nothing in that case rather than claiming Cloud named a
 * reason it did not.
 */
export function deletionRefusalOf(error: unknown): DeletionRefusal | undefined {
  const code = faultCode(error)
  if (!code) {
    return undefined
  }
  return { code, message: deletionRefusalMessage(code) }
}
