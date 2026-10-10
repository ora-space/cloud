import type { IssueRun } from '@/features/issues/types'

/** Renders a byte count the way the delivery line needs it: B, KB or MB with one decimal. */
function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`
}

/**
 * The one-line summary of what an agent run saved, or null while there is
 * nothing to say. A registered Revision wins: its short final commit and
 * whether the run changed files, or reused the Revision it resumed. Without one, `result.deliveryState` explains
 * why (skipped without an object store, failed after retries), and a run whose
 * session ended but has not settled yet is still saving.
 */
export function deliverySummary(run: IssueRun, threadEnded: boolean): string | null {
  const revision = run.revision
  if (revision) {
    const commit = revision.finalCommit.slice(0, 7)
    if (revision.changed) {
      return `已保存 Revision ${commit}（含改动，bundle ${formatBytes(revision.bundleSize ?? 0)}）`
    }
    // A resumed run that added nothing reuses the resumed Revision's bundle instead of storing one.
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
