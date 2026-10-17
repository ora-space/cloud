import type { IssueRun } from '@/features/issues/types'

/**
 * The one-line summary of what an agent run saved, or null while there is
 * nothing to say. A registered Revision wins: its short final commit and
 * whether the run delivered new commits, or reused the Revision it resumed. Git-delivered Revisions
 * have no bundle (`bundleSize` is null), so the line never shows a size. Without one, `result.deliveryState` explains
 * why (skipped without an object store, failed after retries), and a run whose
 * session ended but has not settled yet is still saving.
 */
export function deliverySummary(run: IssueRun, threadEnded: boolean): string | null {
  const revision = run.revision
  if (revision) {
    const commit = revision.finalCommit.slice(0, 7)
    if (revision.changed) {
      return `已保存 Revision ${commit}（含改动）`
    }
    // A resumed run that added no commits reuses the resumed Revision instead of delivering one.
    return revision.priorRevisionId
      ? `已保存 Revision ${commit}（无新改动，沿用续接的成果）`
      : `已保存 Revision ${commit}（无文件改动）`
  }
  const state = run.result?.deliveryState
  if (state === 'skipped') return '未保存 Revision（未配置对象存储）'
  if (state === 'failed') return 'Revision 保存失败'
  return threadEnded && run.status === 'running' ? '正在保存 Revision…' : null
}

/** The delivery line under an agent run's Thread; renders nothing until there is a summary. */
export function RunDelivery({ run, threadEnded }: { run: IssueRun; threadEnded: boolean }) {
  const summary = deliverySummary(run, threadEnded)
  if (!summary) return null
  return (
    <p role="status" aria-label="Revision 交付" className="text-xs text-muted-foreground">
      {summary}
    </p>
  )
}
